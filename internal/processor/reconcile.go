package processor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// reconcileOne brings the record for an already-named activity into line with
// what Strava holds.
//
// This is the athlete's rename reaching the title history. It runs here rather
// than at intake because everything it needs is here and nowhere else: the
// authenticated context, the Strava client, and the write mode. Intake holds
// none of the three and, more to the point, holds nothing it may believe — a
// POST to the callback URL carries no signature, so a title read out of a
// request body could be anybody's. What is recorded below is the title Strava
// answered with. A forged event costs one redundant read.
//
// Two things follow from re-reading rather than comparing against a claim.
// There is no stale echo to suppress: an event delivered twice, or out of
// order, or minutes late, resolves against the same current Strava state and
// therefore to the same answer. And a rename after a rename is recorded like
// the first one, because nothing here remembers how many there have been —
// the limit that used to exist was a property of trusting the event, not of
// the athlete's editing.
//
// The three endings:
//
//   - Strava holds the recorded title. Nothing to record.
//   - Strava holds something else, and it is a person's. The row becomes that
//     title under [store.SourceHuman], whatever it was before.
//   - Strava holds something else that is not a person's — a Strava default
//     the athlete reverted to, a tool's overwrite, one of this service's own
//     templates. Nothing is recorded, because [store.SourceHuman] is what the
//     few-shot examples are drawn from and a row saying the athlete named a
//     ride "Morning Ride" would teach exactly that.
//
// Either of the last two may then need the attribution line taken back out;
// see [Processor.removeAttribution].
//
// The activity is never renamed here and never can be. This service is the
// last writer and it writes a title once; a title the athlete has replaced is
// theirs, and the row exists so that no later sweep reconsiders it.
func (p *Processor) reconcileOne(
	ctx context.Context, pending store.Pending, recorded store.NamedTitle, logger *slog.Logger,
) (outcome, error) {
	logger = logger.With("recorded_title", logsafe.String(recorded.Title),
		"recorded_source", logsafe.String(recorded.Source))

	activity, err := p.deps.Activities.GetActivity(ctx, pending.ActivityID)
	if err != nil {
		if gone(err) {
			logger.Info("reconciled: strava has no such activity; nothing left to record")

			return outcomeReconciled, nil
		}

		return outcomeSkipped, fmt.Errorf("re-read the activity to reconcile it: %w", err)
	}

	source := recorded.Source

	switch {
	case activity.Name == recorded.Title:
		logger.Info("reconciled: strava holds the recorded title")
	case p.notTheAthletesTitle(activity.Name) != "":
		logger.Info("reconciled: the title on strava is not the athlete's own; leaving the record",
			"strava_title", logsafe.String(activity.Name),
			"reason", p.notTheAthletesTitle(activity.Name))
	default:
		source = renamedSource(recorded.Source)

		if err := p.recordRename(ctx, pending.AthleteID, activity, source, logger); err != nil {
			return outcomeSkipped, err
		}
	}

	return p.removeAttribution(ctx, activity, source, logger)
}

// renamedSource says what a row's source becomes when the athlete renames the
// activity.
//
// [store.SourceHuman] for almost everything: the source records how a title was
// produced, and this one was produced by the athlete typing it — as true of a
// ride this service named as of a commute it titled from a template, and as
// true of the second rename as of the first.
//
// [store.SourceImported] is the exception, and it is structural. An imported
// row is barred from the few-shot examples on purpose: a decade of the
// athlete's own shorthand is bare town names and private jokes, and an example
// set built from it teaches a model to answer with the name of a town. Tidying
// up a ten-year-old ride is still an act on a ten-year-old ride; it does not
// make that title current voice, and letting the rename promote the row would
// put exactly the material the import exists to keep out of EXAMPLES back into
// it. The title still follows Strava — the row would otherwise record a title
// that is not there — and RECENT, which takes every source, still sees it.
func renamedSource(recorded string) string {
	if recorded == store.SourceImported {
		return store.SourceImported
	}

	return store.SourceHuman
}

// recordRename replaces the row with the title Strava holds.
//
// Dated by the ride rather than the sweep, as an import and a skip-gate
// recording are: RECENT is ordered by this date, and a rename made days later
// must not jump the ride to the top of it.
//
// It records in dry run. Dry run withholds writes to Strava; the athlete's
// rename already happened and the record of it is not a write to Strava.
func (p *Processor) recordRename(
	ctx context.Context, athleteID int64, activity *strava.Activity,
	source string, logger *slog.Logger,
) error {
	at := activity.StartDate
	if at.IsZero() {
		at = p.deps.Now()
	}

	if err := p.deps.Store.MarkNamed(ctx, store.Naming{
		AthleteID:  athleteID,
		ActivityID: activity.ID,
		Title:      activity.Name,
		Language:   string(naming.GuessLanguage(activity.Name)),
		Source:     source,
		At:         at,
	}); err != nil {
		return fmt.Errorf("record the athlete's rename: %w", err)
	}

	logger.Info("reconciled: the athlete renamed the activity; the record is now theirs",
		"strava_title", logsafe.String(activity.Name), "source", source)

	return nil
}

// removeAttribution takes back the line that says this service named the
// activity, once the record says it did not.
//
// The line is a claim about authorship, and after a rename the claim is false.
// It is also the idempotency sentinel, so removing it is what lets the ride be
// named again should the athlete ever revert it to a Strava default — but that
// is not why it goes: it goes because it is no longer true.
//
// Driven by the row's source rather than by what this sweep did, which is what
// makes it convergent. A crash between the record and this write leaves the
// row human and the line in place; the next update event on the activity finds
// the title unchanged and still finishes the job. Dry run leans on the same
// property: the row is recorded, the write is withheld, the entry stays queued,
// and turning writes on completes it.
//
// The description is the one the re-read returned, seconds old at most. It is
// a read-modify-write like the attribution write itself: whatever else the
// description holds — Xert's, myWindsock's, mybiketraffic's output — is put
// back byte for byte, and only the exact line and the blank line after it are
// taken out. A description carrying some older wording of the line keeps it;
// [naming.RemoveAttribution] refuses to guess at which bytes were ours.
//
// Never fatal to the reconciliation that preceded it — the row is already
// right, and the line is cosmetic — but a failed write does return an error,
// so the entry stays queued and the next sweep tries again.
func (p *Processor) removeAttribution(
	ctx context.Context, activity *strava.Activity, source string, logger *slog.Logger,
) (outcome, error) {
	if source != store.SourceHuman {
		return outcomeReconciled, nil
	}

	description, removed := naming.RemoveAttribution(activity.Description)
	if !removed {
		return outcomeReconciled, nil
	}

	if !p.deps.WritesEnabled {
		logger.Info("dry run: not removing the attribution line")

		return outcomeReconcileDryRun, nil
	}

	if _, err := p.deps.Activities.UpdateActivityDescription(
		ctx, activity.ID, description); err != nil {
		return outcomeSkipped, fmt.Errorf("remove the attribution line: %w", err)
	}

	logger.Info("removed the attribution line; the title is no longer ours")

	return outcomeReconciled, nil
}

// notTheAthletesTitle says why a title Strava holds is not one the athlete
// typed, or nothing when it is.
//
// The same filter the import applies and the skip gate's recorder applies,
// stated once more here because a reconcile has no [classifier.Decision] to
// lean on: the gate that ruled out Strava's defaults and the configured
// machine titles never ran on this activity, so this checks them itself.
//
// What it deliberately does not check is the tier. A commute is titled from a
// template and its row says so; if the athlete renames it, the row has to
// follow or it records a title that is not on Strava. The tier decides what
// this service may *name*, and naming is over for this activity.
func (p *Processor) notTheAthletesTitle(title string) string {
	trimmed := strings.TrimSpace(title)

	if trimmed == "" {
		return "an empty title"
	}

	if classifier.IsDefaultTitle(trimmed) {
		return "one of Strava's default titles"
	}

	if p.deps.Classifier.MachineTitles.Matches(trimmed) {
		return "a recognized machine title"
	}

	return notAPersonsTitle(trimmed, p.deps.Classifier.TemplateTitles())
}
