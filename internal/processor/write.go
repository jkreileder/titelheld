package processor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// write records the title and sends it.
//
// The order is deliberate and is the spec's: the named log is written first,
// then the title goes to Strava. A crash between the two leaves an activity
// marked as named that never got its title — it keeps the Strava default
// forever. The other order would risk renaming an activity twice, and would
// let the webhook event caused by our own rename arrive before anything
// recorded that we caused it.
//
// In dry run nothing is sent and the log line says exactly what would have
// been: the title, and whether the attribution line would have been added.
// The pipeline above this point runs in full either way — the spec asks for
// the would-be title, which means the LLM really is called.
// It reports whether anything was actually sent. Dry run returns false, and
// the caller leaves the activity queued on the strength of it.
func (p *Processor) write(
	ctx context.Context, athleteID int64, activity *strava.Activity,
	title titled, logger *slog.Logger,
) (bool, error) {
	if !p.deps.WritesEnabled {
		// No re-read here. Dry run leaves the activity queued, so the pipeline
		// runs again on every sweep for as long as the review window is open,
		// and a second GET per activity per sweep would spend the 100-per-15-
		// minutes budget on a line in a log. The copy the classifier fetched
		// answers the only question the log asks.
		//
		// would_advance_franchise is empty unless the title actually used the
		// entry it was offered, which is the same test a real write applies.
		logger.Info("dry run: not writing",
			"would_title", logsafe.String(title.Text),
			"would_attribute", p.wouldAttribute(activity),
			"would_advance_franchise", logsafe.String(title.Franchise))

		return false, nil
	}

	description, attribute := p.description(ctx, activity, logger)

	// Recorded before the write. See the note above.
	//
	// Keyed on the athlete the queue entry names rather than the one the
	// activity reports, because that is the key the dedup read uses. Strava
	// omitting athlete.id would otherwise file this under athlete 0, where the
	// check that stops a second rename would never find it.
	if err := p.deps.Store.MarkNamed(ctx, store.Naming{
		AthleteID:  athleteID,
		ActivityID: activity.ID,
		Title:      title.Text,
		Language:   string(title.Language),
		Source:     title.Source,
		At:         p.deps.Now(),
	}); err != nil {
		return false, fmt.Errorf("record the title before writing: %w", err)
	}

	// Recorded before the write, for the same reason the named log is. A
	// crash between here and the PUT skips a franchise entry; the other order
	// would issue one entry twice, which is the failure the series exists to
	// prevent.
	p.recordFranchise(ctx, athleteID, title, logger)

	var (
		written *strava.Activity
		err     error
	)

	if attribute {
		written, err = p.deps.Activities.UpdateActivityNameAndDescription(
			ctx, activity.ID, title.Text, description)
	} else {
		written, err = p.deps.Activities.UpdateActivityName(ctx, activity.ID, title.Text)
	}

	if err != nil {
		return false, fmt.Errorf("write the title: %w", err)
	}

	logger.Info("wrote the title",
		"title", logsafe.String(title.Text), "attributed", attribute)

	p.reconcileTitle(ctx, athleteID, activity.ID, title, written, logger)

	return true, nil
}

// reconcileTitle records what Strava kept, when that is not what was sent.
//
// Strava rewrites a title it thinks contains a link: a hand-written
// "Über Ruhstorf a.d.Rott nach Pocking" came back as "Über Ruhstorf  nach
// Pocking" on 2026-08-24, the token excised and both spaces left behind.
// Place names are normalized before they reach the prompt so this should not
// arise, but "should not" is not a guarantee about somebody else's server.
//
// The named log is written before the PUT, deliberately, so that a crash
// cannot rename an activity twice. The cost is that the row holds what was
// sent. Left uncorrected, RECENT would forbid repeating a title that does not
// exist and a few-shot example could teach a form that never survives a write.
//
// Never fatal. The title is already on Strava; a failure to correct the record
// is worth a log line and nothing more.
func (p *Processor) reconcileTitle(
	ctx context.Context, athleteID, activityID int64,
	title titled, written *strava.Activity, logger *slog.Logger,
) {
	if written == nil || written.Name == "" || written.Name == title.Text {
		return
	}

	logger.Warn("strava stored a different title than the one sent; correcting the record",
		"sent", logsafe.String(title.Text),
		"stored", logsafe.String(written.Name))

	if err := p.deps.Store.MarkNamed(ctx, store.Naming{
		AthleteID:  athleteID,
		ActivityID: activityID,
		Title:      written.Name,
		Language:   string(title.Language),
		Source:     title.Source,
		At:         p.deps.Now(),
	}); err != nil {
		logger.Error("could not correct the named log; it holds the title that was sent",
			"error", err)
	}
}

// wouldAttribute reports what a real write would have done about the
// description, from the copy already in hand.
//
// It can differ from what the write would actually find — another tool may add
// the line in the seconds a naming takes — but this is a dry-run log line, and
// being right about it is not worth a Strava request on every sweep.
func (p *Processor) wouldAttribute(activity *strava.Activity) bool {
	return !p.deps.DisableAttribution && !naming.HasAttribution(activity.Description)
}

// recordFranchise advances the series past the entry this title used.
//
// Only a title that used the entry gets here: the naming layer leaves
// [titled.Franchise] empty when the offer was declined, so an entry that was
// merely offered is still there for the next ride.
//
// Never fatal. A position that could not be advanced means the next ride is
// offered the same entry again — a repeat, which is worse than a gap but not
// worth abandoning a title that is about to be written and already recorded.
func (p *Processor) recordFranchise(
	ctx context.Context, athleteID int64, title titled, logger *slog.Logger,
) {
	if title.Franchise == "" {
		return
	}

	position, err := p.deps.Store.AdvanceFranchisePast(
		ctx, athleteID, title.Franchise, title.FranchiseIndex)
	if err != nil {
		logger.Error("could not advance the franchise; the next ride may repeat this entry",
			"franchise", logsafe.String(title.Franchise), "error", err)

		return
	}

	logger.Info("advanced the franchise",
		"franchise", logsafe.String(title.Franchise), "position", position)
}

// description decides what description to send, if any.
//
// Attribution must never block a naming. Every failure here — the fetch, the
// sentinel check, anything — ends the same way: no description is sent, the
// title goes out on its own, and the reason is logged with the API's own
// words rather than a summary of them.
func (p *Processor) description(
	ctx context.Context, activity *strava.Activity, logger *slog.Logger,
) (string, bool) {
	if p.deps.DisableAttribution {
		return "", false
	}

	// Re-fetched rather than reused from the copy the classifier saw. That
	// copy is already post-delay — the queue held the activity until then, and
	// the sweep fetched it afterwards — so the gap this closes is only the
	// naming itself: the geocoding and the model call, seconds in which
	// another tool may have written. The spec asks for a read at write time,
	// and prepending to a description that moved under us would delete
	// whatever arrived in between.
	//
	// It is a second GET per named activity. Strava allows 100 requests per
	// 15 minutes and this service names a handful of rides a day, so the
	// budget is not the constraint here; correctness of the merge is.
	fresh, err := p.deps.Activities.GetActivity(ctx, activity.ID)
	if err != nil {
		logger.Warn("could not re-read the description; writing the title without attribution",
			"error", err)

		return "", false
	}

	updated, changed := naming.Describe(fresh.Description, true)
	if !changed {
		logger.Info("description already carries the attribution; leaving it alone")

		return "", false
	}

	return updated, true
}
