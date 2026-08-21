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
	title string, logger *slog.Logger,
) (bool, error) {
	if !p.deps.WritesEnabled {
		// No re-read here. Dry run leaves the activity queued, so the pipeline
		// runs again on every sweep for as long as the review window is open,
		// and a second GET per activity per sweep would spend the 100-per-15-
		// minutes budget on a line in a log. The copy the classifier fetched
		// answers the only question the log asks.
		logger.Info("dry run: not writing",
			"would_title", logsafe.String(title),
			"would_attribute", p.wouldAttribute(activity))

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
		Title:      title,
		At:         p.deps.Now(),
	}); err != nil {
		return false, fmt.Errorf("record the title before writing: %w", err)
	}

	var err error
	if attribute {
		_, err = p.deps.Activities.UpdateActivityNameAndDescription(ctx, activity.ID, title, description)
	} else {
		_, err = p.deps.Activities.UpdateActivityName(ctx, activity.ID, title)
	}

	if err != nil {
		return false, fmt.Errorf("write the title: %w", err)
	}

	logger.Info("wrote the title", "title", logsafe.String(title), "attributed", attribute)

	return true, nil
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
