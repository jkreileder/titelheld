package processor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/naming"
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
func (p *Processor) write(
	ctx context.Context, activity *strava.Activity, title string, logger *slog.Logger,
) error {
	description, attribute := p.description(ctx, activity, logger)

	if !p.deps.WritesEnabled {
		logger.Info("dry run: not writing",
			"would_title", logsafe.String(title),
			"would_attribute", attribute)

		return nil
	}

	// Recorded before the write. See the note above.
	if err := p.deps.Store.MarkNamed(ctx, activity.Owner(), activity.ID, title); err != nil {
		return fmt.Errorf("record the title before writing: %w", err)
	}

	var err error
	if attribute {
		_, err = p.deps.Activities.UpdateActivityNameAndDescription(ctx, activity.ID, title, description)
	} else {
		_, err = p.deps.Activities.UpdateActivityName(ctx, activity.ID, title)
	}

	if err != nil {
		return fmt.Errorf("write the title: %w", err)
	}

	logger.Info("wrote the title", "title", logsafe.String(title), "attributed", attribute)

	return nil
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

	// The description is re-fetched rather than reused from the activity the
	// classifier saw. That copy was read before the delay elapsed; by now Xert
	// and myWindsock have written theirs, and prepending to the stale copy
	// would delete their work.
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
