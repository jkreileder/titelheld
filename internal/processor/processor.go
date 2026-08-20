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
	UpdateActivityName(ctx context.Context, activityID int64, name string) (*strava.Activity, error)
	UpdateActivityNameAndDescription(ctx context.Context, activityID int64, name, description string) (*strava.Activity, error)
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

	// Attribution is on unless this says otherwise. The field is negative so
	// that the zero value means the spec's default rather than its opposite.
	DisableAttribution bool

	// WritesEnabled mirrors the Strava client's write mode. The client refuses
	// a write on its own, twice over; this exists so the processor can say
	// what it *would* have done instead of attempting it and logging a
	// refusal on every ride.
	WritesEnabled bool

	Logger *slog.Logger
	Now    func() time.Time
}

// Processor names activities that are due.
type Processor struct {
	deps Deps
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

	return &Processor{deps: deps}, nil
}

// Result is what one sweep did.
type Result struct {
	// Due is how many entries were eligible.
	Due int

	// Named is how many got a title (or would have, in dry run).
	Named int

	// Skipped is how many the classifier or the named log declined.
	Skipped int

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
				"finished", result.Named+result.Skipped+result.Failed,
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
		case outcome == outcomeNamed:
			result.Named++
		default:
			result.Skipped++
		}

		if err == nil {
			if err := p.deps.Store.Remove(ctx, pending.AthleteID, pending.ActivityID); err != nil {
				p.deps.Logger.Error("could not dequeue a finished activity",
					"activity_id", pending.ActivityID, "error", err)
			}
		}
	}

	p.deps.Logger.Info("sweep complete",
		"due", result.Due, "named", result.Named,
		"skipped", result.Skipped, "failed", result.Failed,
		"cancelled", result.Cancelled)

	// See the note on Result.Cancelled: a partial sweep is a success.
	return result, nil
}

// outcome distinguishes the two non-error endings.
type outcome int

const (
	outcomeSkipped outcome = iota
	outcomeNamed
)

// processOne runs the pipeline for a single activity.
func (p *Processor) processOne(ctx context.Context, pending store.Pending) (outcome, error) {
	logger := p.deps.Logger.With(
		"activity_id", pending.ActivityID, "athlete_id", pending.AthleteID)

	// Already named is the self-caused-event case: this service renamed the
	// activity, Strava emitted an update event for that rename, and the event
	// came back round. It is not an error and not work.
	if title, named, err := p.deps.Store.Named(ctx, pending.AthleteID, pending.ActivityID); err != nil {
		return outcomeSkipped, fmt.Errorf("read the named log: %w", err)
	} else if named {
		logger.Info("already named; dropping a self-caused event",
			"title", logsafe.String(title))

		return outcomeSkipped, nil
	}

	activity, err := p.deps.Activities.GetActivity(ctx, pending.ActivityID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("fetch the activity: %w", err)
	}

	decision := classifier.Classify(toClassifierActivity(activity), p.deps.Classifier)

	logger = logger.With("tier", decision.Tier.String(), "action", decision.Action.String())

	if decision.Action == classifier.ActionSkip {
		logger.Info("skipping", "reason", decision.Reason)

		return outcomeSkipped, nil
	}

	title, err := p.title(ctx, activity, decision, logger)
	if err != nil {
		return outcomeSkipped, err
	}

	if err := p.write(ctx, activity, title, logger); err != nil {
		return outcomeSkipped, err
	}

	return outcomeNamed, nil
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
