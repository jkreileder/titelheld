// Package processor drains the delay queue and names what is due.
//
// It is the only place the pieces meet: the queue says what is ready, the
// classifier says whether it may be touched, geo and the description say what
// there is to say about it, the naming layer produces a title, and the writer
// sends it. Everything it depends on arrives as an interface, so a test can
// run the whole pipeline with no network and no Firestore.
//
// Two properties matter more than anything else here.
//
// One activity must never stall the sweep. A ride whose geocoding times out,
// whose LLM returns nonsense, or whose Strava call fails, is logged and left
// in the queue for the next run; the sweep moves on. The alternative — a sweep
// that stops at the first failure — means one bad activity blocks every later
// one, forever, unnoticed.
//
// An activity is named at most once, ever. The named log is written before the
// title is sent, so a crash between the two leaves the activity marked but
// unnamed. That is the trade the spec asks for, and it is the right way round:
// the failure mode is a ride that keeps its default title, not a ride renamed
// twice or a rename fighting a webhook event it caused itself.
package processor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/geo"
	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// Activities is the part of the Strava client this package needs.
//
// Defined here rather than imported, so the processor states what it uses and
// a test supplies it without a transport.
type Activities interface {
	GetActivity(ctx context.Context, activityID int64) (*strava.Activity, error)
	GetGear(ctx context.Context, gearID string) (strava.Gear, error)
	UpdateActivityName(ctx context.Context, activityID int64, name string) (*strava.Activity, error)
	UpdateActivityNameAndDescription(ctx context.Context, activityID int64, name, description string) (*strava.Activity, error)
	UpdateActivityDescription(ctx context.Context, activityID int64, description string) (*strava.Activity, error)
}

// Geographer resolves a polyline into verified place names.
type Geographer interface {
	Describe(ctx context.Context, encodedPolyline string) (geo.Summary, error)
}

// Deps is everything the processor needs to run.
type Deps struct {
	Store      store.Store
	Activities Activities
	Geo        Geographer
	Provider   naming.Provider
	Classifier classifier.Config
	Validator  naming.Validator

	// Franchises overrides the athlete's configured series. Nil means read
	// them from the configuration document, falling back to the shipped
	// default profile; an empty non-nil slice means none, which is how a test
	// turns the feature off without a document.
	Franchises []naming.Franchise

	// Attribution is on unless this says otherwise. The field is negative so
	// that the zero value means the spec's default rather than its opposite.
	DisableAttribution bool

	// WritesEnabled mirrors the Strava client's write mode. The client refuses
	// a write on its own, twice over; this exists so the processor can say
	// what it *would* have done instead of attempting it and logging a
	// refusal on every ride.
	WritesEnabled bool

	// LogPrompt logs the complete prompt for every naming.
	//
	// The counters on the "named" line say how many places, achievements,
	// examples and recent titles the prompt carried; they cannot say what any
	// of them were. During the observation window the judgement being made is
	// about the material the model actually received, so an inference from
	// counts is the wrong evidence.
	//
	// Verbosity is what this gates. Everything in a prompt is the athlete's
	// own material, and the geo layer cannot contribute a coordinate: it
	// produces names and has nowhere to hold a position.
	//
	// The exception worth knowing is a NOTES fact, whose label is allow-listed
	// and whose value is free text from a description another tool wrote.
	LogPrompt bool

	Logger *slog.Logger
	Now    func() time.Time
}

// Processor names activities that are due.
type Processor struct {
	deps Deps

	// gear caches gear names for the life of the process. A bike's name
	// changes about never, and the alternative is a Strava request per named
	// activity for a string that was the same last time. A restart re-reads
	// them, which is the right cost for a cache that must not go stale
	// forever.
	gearMu sync.Mutex
	gear   map[string]string

	// examples caches one few-shot example per past activity, for the life of
	// the process. Deriving one re-reads the activity from Strava, and what a
	// past ride looked like does not change.
	//
	// Keyed by activity rather than by the history as a whole: the history
	// moves every time something is named, so a whole-history key would miss
	// for every activity after the first in a sweep and pay six reads again
	// each time, against a hundred per fifteen minutes.
	examplesMu sync.Mutex
	examples   map[int64]naming.Example

	// franchiseCache holds each athlete's configured series, read once per
	// athlete. A person edits configuration about as often as they name a
	// bike, and a restart is what picks up an edit — the same trade the gear
	// cache makes.
	//
	// Keyed by athlete because everything here is: one process serves one
	// athlete today, and a cache that is not keyed would hand the first
	// athlete's franchises to the second the day that stops being true. The
	// map holds the answer including "none configured", which is a real
	// answer and must not be re-read on every activity — so presence in the
	// map is the loaded flag, and a nil value is a legitimate entry.
	franchiseMu    sync.Mutex
	franchiseCache map[int64][]naming.Franchise
}

// New builds a processor.
func New(deps Deps) (*Processor, error) {
	switch {
	case deps.Store == nil:
		return nil, errors.New("processor: a store is required")
	case deps.Activities == nil:
		return nil, errors.New("processor: a Strava client is required")
	}

	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	if deps.Now == nil {
		deps.Now = time.Now
	}

	return &Processor{
		deps:           deps,
		gear:           make(map[string]string),
		examples:       make(map[int64]naming.Example),
		franchiseCache: make(map[int64][]naming.Franchise),
	}, nil
}

// Result is what one sweep did.
type Result struct {
	// Due is how many entries were eligible.
	Due int

	// Named is how many got a title (or would have, in dry run).
	Named int

	// Skipped is how many the classifier declined.
	Skipped int

	// Reconciled is how many were re-examined against Strava rather than
	// named: an activity already in the named log, queued by an update event.
	// Counted apart from Named and Skipped because it is neither — no title
	// was produced, and the entry was not declined.
	Reconciled int

	// Failed is how many errored and stayed in the queue for the next sweep.
	Failed int

	// Cancelled reports that the sweep stopped before reaching the end of the
	// queue, because its context was done. It is not a failure: what was named
	// is named and recorded, and the rest is still queued for the next run.
	// The caller reports it rather than inferring it from Due exceeding the
	// other three counts.
	Cancelled bool
}

// Sweep drains everything currently due.
//
// It returns an error only when the queue itself could not be read. A failure
// on one activity is counted, logged and left behind; the sweep continues, and
// the entry is retried on the next run.
func (p *Processor) Sweep(ctx context.Context) (Result, error) {
	due, err := p.deps.Store.Due(ctx, p.deps.Now())
	if err != nil {
		return Result{}, fmt.Errorf("processor: read the queue: %w", err)
	}

	result := Result{Due: len(due)}

	for _, pending := range due {
		// The sweep's own deadline is respected between activities, so a
		// shutdown does not abandon one halfway through.
		if err := ctx.Err(); err != nil {
			p.deps.Logger.Warn("sweep cancelled; the rest stays queued",
				"due", result.Due,
				"finished", result.Named+result.Reconciled+result.Skipped+result.Failed,
				"error", err)

			result.Cancelled = true

			break
		}

		outcome, err := p.processOne(ctx, pending)

		switch {
		case err != nil:
			result.Failed++

			// The entry stays in the queue. Reporting the underlying error
			// rather than an interpretation of it is the house rule: a
			// rate-limited Strava call and a malformed response must not read
			// the same in a log.
			p.deps.Logger.Error("activity failed; left queued for the next sweep",
				"activity_id", pending.ActivityID,
				"athlete_id", pending.AthleteID,
				"error", err)
		case outcome == outcomeNamed, outcome == outcomeDryRun:
			result.Named++
		case outcome == outcomeReconciled, outcome == outcomeReconcileDryRun:
			result.Reconciled++
		default:
			result.Skipped++
		}

		// Left queued after a failure, so the next sweep retries it, and after
		// a dry run, so turning writes on still does the work. See
		// outcomeDryRun and outcome.staysQueued.
		if err == nil && !outcome.staysQueued() {
			if err := p.deps.Store.Remove(ctx, pending.AthleteID, pending.ActivityID); err != nil {
				p.deps.Logger.Error("could not dequeue a finished activity",
					"activity_id", pending.ActivityID, "error", err)
			}
		}
	}

	p.deps.Logger.Info("sweep complete",
		"due", result.Due, "named", result.Named,
		"reconciled", result.Reconciled,
		"skipped", result.Skipped, "failed", result.Failed,
		"cancelled", result.Cancelled)

	// See the note on Result.Cancelled: a partial sweep is a success.
	return result, nil
}

// outcome distinguishes the non-error endings.
type outcome int

const (
	outcomeSkipped outcome = iota
	outcomeNamed

	// outcomeDryRun is a naming that was worked out in full and not sent.
	//
	// It counts as named in the Result — the pipeline ran, the model was
	// called, the title exists in the log — but the queue entry stays put.
	// Dry run has to be a no-op on state, or the observation window silently
	// eats the activities it observes: nothing records them as named, and
	// nothing is left to name them once writes are turned on, so they keep
	// their Strava default forever.
	//
	// The cost of that is the pipeline re-running for a queued activity on
	// every sweep, model call included, for as long as dry run lasts. That is
	// the deliberate trade: dry run is a review window someone is watching,
	// the scheduler is paused until they open it, and a budget alert bounds
	// the bill.
	//
	// One piece of state is written in dry run regardless: a human title the
	// skip gate declines is recorded, because that ride is final whatever the
	// write mode — see [Processor.recordHumanTitle].
	outcomeDryRun

	// outcomeReconciled is an activity already in the named log, re-examined
	// against what Strava holds and finished with. See
	// [Processor.reconcileOne].
	outcomeReconciled

	// outcomeReconcileDryRun is a reconciliation that wanted to write a
	// description and did not, because writes are off.
	//
	// It stays queued for the same reason a dry-run naming does: the work is
	// still owed, and turning writes on has to be enough to do it. The row
	// itself is already updated — that is not a write to Strava and the
	// athlete's rename is final whatever the write mode.
	outcomeReconcileDryRun
)

// staysQueued reports whether the entry is left for the next sweep.
//
// Only the dry-run endings, and only because dry run has to be a no-op on the
// queue. Everything else is finished with: named, skipped or reconciled.
func (o outcome) staysQueued() bool {
	return o == outcomeDryRun || o == outcomeReconcileDryRun
}

// processOne runs the pipeline for a single activity.
func (p *Processor) processOne(ctx context.Context, pending store.Pending) (outcome, error) {
	logger := p.deps.Logger.With(
		"activity_id", pending.ActivityID, "athlete_id", pending.AthleteID)

	// An activity already in the named log is never named again — this
	// service is the last writer and it writes once. What is left to do is
	// reconcile: read what Strava holds now and bring the record into line
	// with it.
	//
	// The queue entry says nothing about which of the two this is, and must
	// not: it came from an unauthenticated POST. The named log is the
	// authority, and it is read here on both paths.
	recorded, named, err := p.deps.Store.Named(ctx, pending.AthleteID, pending.ActivityID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("read the named log: %w", err)
	}

	if named {
		return p.reconcileOne(ctx, pending, recorded, logger)
	}

	activity, err := p.deps.Activities.GetActivity(ctx, pending.ActivityID)
	if err != nil {
		if gone(err) {
			logger.Info("dropping the event", "reason", "strava has no such activity")

			return outcomeSkipped, nil
		}

		return outcomeSkipped, fmt.Errorf("fetch the activity: %w", err)
	}

	decision := classifier.Classify(toClassifierActivity(activity), p.deps.Classifier)

	logger = logger.With("tier", decision.Tier.String(), "action", decision.Action.String())

	if decision.Action == classifier.ActionSkip {
		if err := p.recordHumanTitle(ctx, pending.AthleteID, activity, decision, logger); err != nil {
			return outcomeSkipped, err
		}

		logger.Info("skipping", "reason", decision.Reason)

		return outcomeSkipped, nil
	}

	title, err := p.title(ctx, pending.AthleteID, activity, decision, logger)
	if err != nil {
		return outcomeSkipped, err
	}

	// pending.AthleteID, not the activity's own owner: this is the key the
	// dedup read above used, and the two have to be the same one or a replay
	// would look unnamed and be renamed a second time.
	written, err := p.write(ctx, pending.AthleteID, activity, title, logger)
	if err != nil {
		return outcomeSkipped, err
	}

	if !written {
		return outcomeDryRun, nil
	}

	return outcomeNamed, nil
}

// gone reports that Strava has nothing to offer for this activity — it was
// deleted, or it is no longer visible with the granted scopes.
//
// A queue entry is retried on every sweep until it succeeds, which is right for
// a rate limit or a 500 and wrong for a 404: nothing will ever change, and the
// entry would spend one request every five minutes forever against a budget of
// a hundred per fifteen minutes. There is nothing left to name or to reconcile,
// so the entry is finished with rather than failed.
func gone(err error) bool {
	return errors.Is(err, strava.ErrNotFound)
}

// recordHumanTitle remembers a title the athlete wrote, when that is why a
// sport ride was skipped.
//
// The skip gate declines any title it does not recognize. A title the athlete
// typed onto a ride is one the model must not invent a second time, and the
// best style data there is; recorded under [store.SourceHuman] it joins the
// no-repeat list and the few-shot examples.
//
// Sport rides only — a decision, not a derivation. A commute titled by
// ActivityFix arrives commute-tagged and classifies as an errand, and a ride
// below the sport thresholds is one this service would never name; neither
// title belongs in a sport ride's RECENT, and a working week of "Zur Arbeit"
// would crowd the list. A hand-titled Zwift ride under the indoor naming mode
// is skipped the same way and not recorded; that mode has no configuration
// path yet, and the tier check is where to widen this if it gets one.
//
// Not every unrecognized title is a person's. A tool's title on an outdoor
// ride — Zwift's route name on a ride uploaded as a plain Ride, Xert's suffix
// on a pattern the machine-title list does not name — and this service's own
// commute template typed by hand on a long ride are left unrecorded: the same
// filter the import applies, because a row recorded here teaches style and is
// never revisited.
//
// The row is the dedup record, so a ride recorded here is final *for naming*:
// this service is the last writer and never overwrites a person, and a title
// later reverted to a Strava default is not reconsidered. The row itself still
// follows the athlete — an edit they make afterwards arrives as an update
// event, which is queued and reconciled against Strava; see
// [Processor.reconcileOne]. Nothing is sent to Strava from here: this runs on
// the skip path, which never reaches the writer, so neither the title nor the
// attribution line can follow.
//
// It records in dry run too. Dry run withholds writes to Strava and leaves
// activities queued so that turning writes on still names them; a human title
// is neither — the ride is final whatever the write mode — and the observation
// window is exactly when the athlete's own titles should start teaching.
//
// Dated by the ride rather than the sweep, as an import is: with the scheduler
// paused a sweep runs days late, and RECENT is ordered by this date.
//
// A store failure fails the activity, which leaves it queued for the next
// sweep — the same trade the named log makes on a write.
func (p *Processor) recordHumanTitle(
	ctx context.Context, athleteID int64, activity *strava.Activity,
	decision classifier.Decision, logger *slog.Logger,
) error {
	if !decision.HumanTitled || decision.Tier != classifier.TierSportRide {
		return nil
	}

	if reason := notAPersonsTitle(activity.Name, p.deps.Classifier.TemplateTitles()); reason != "" {
		logger.Info("not recording the title; it is not the athlete's own",
			"title", logsafe.String(activity.Name), "reason", reason)

		return nil
	}

	at := activity.StartDate
	if at.IsZero() {
		at = p.deps.Now()
	}

	if err := p.deps.Store.MarkNamed(ctx, store.Naming{
		AthleteID:  athleteID,
		ActivityID: activity.ID,
		Title:      activity.Name,
		Language:   string(naming.GuessLanguage(activity.Name)),
		Source:     store.SourceHuman,
		At:         at,
	}); err != nil {
		return fmt.Errorf("record the athlete's own title: %w", err)
	}

	logger.Info("recorded the athlete's own title; it joins the title history",
		"title", logsafe.String(activity.Name))

	return nil
}

// notAPersonsTitle says why an unrecognized title is still not the athlete's,
// or nothing when it is. The gate has already ruled out Strava's defaults and
// the configured machine titles; this is the rest of the import's filter.
func notAPersonsTitle(title string, templates []string) string {
	title = strings.TrimSpace(title)

	if classifier.IsToolTitle(title) {
		return "a tool's title"
	}

	if slices.ContainsFunc(templates, func(template string) bool {
		return strings.TrimSpace(template) == title
	}) {
		return "one of this service's own templates"
	}

	return ""
}

// toClassifierActivity converts what Strava returned into what the classifier
// takes. The classifier imports no Strava types, which is what keeps it
// testable without one.
func toClassifierActivity(a *strava.Activity) classifier.Activity {
	converted := classifier.Activity{
		Name:              a.Name,
		Description:       a.Description,
		SportType:         a.SportType,
		Trainer:           a.Trainer,
		Commute:           a.Commute,
		DistanceMeters:    a.Distance,
		MovingTimeSeconds: a.MovingTime,
	}

	if len(a.StartLatLng) == 2 {
		converted.Start = &classifier.Point{Lat: a.StartLatLng[0], Lon: a.StartLatLng[1]}
	}

	if len(a.EndLatLng) == 2 {
		converted.End = &classifier.Point{Lat: a.EndLatLng[0], Lon: a.EndLatLng[1]}
	}

	return converted
}
