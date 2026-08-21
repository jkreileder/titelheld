// Package importer seeds the title history from the athlete's Strava past.
//
// The prompt asks a model not to repeat a title and shows it examples in the
// athlete's own style. Both read the named log, which starts empty — so until
// this has run, a fresh deployment names from a synthetic example set and an
// empty RECENT list, which is exactly the cold start the spec says to seed
// away.
//
// It is a one-shot run by a person, not a route on the service. The service is
// invokable by allUsers, so every endpoint added to it is another thing that
// has to authenticate correctly; a job that runs once, under the operator's
// own credentials, with no request timeout over it, has no business being one.
package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// Activities is the part of the Strava client this package needs.
type Activities interface {
	ListActivities(ctx context.Context, page, perPage int) ([]strava.Activity, error)
}

// History is the part of the store it writes to.
type History interface {
	MarkNamed(ctx context.Context, naming store.Naming) error
	Named(ctx context.Context, athleteID, activityID int64) (string, bool, error)
}

// Deps is what an import needs.
type Deps struct {
	Activities Activities
	Store      History

	// AthleteID is whose history is being imported.
	AthleteID int64

	// MachineTitles recognizes titles another tool wrote. Imported alongside
	// Strava's own defaults, they are the two kinds of title this skips.
	//
	// Empty means the shipped set, not "recognize nothing". The zero value
	// matches no title at all, so a caller that left this out would seed
	// Xert's titles into the history as the athlete's own style — the one
	// outcome the skip exists to prevent, arrived at by omission.
	MachineTitles classifier.MachineTitles

	// PerPage defaults to [strava.MaxActivitiesPerPage]. Lower it only to
	// exercise paging in a test.
	PerPage int

	// Pause is how long to wait between pages. Strava allows a hundred reads
	// per fifteen minutes and a full history is a handful of pages, so this is
	// politeness rather than necessity.
	Pause func(ctx context.Context, d time.Duration) error

	Logger *slog.Logger
}

// pageInterval is the default wait between pages.
const pageInterval = 2 * time.Second

// Result is what an import did.
type Result struct {
	// Seen is how many activities the listing returned.
	Seen int

	// Imported is how many titles were written.
	Imported int

	// Skipped counts activities whose title is a Strava default or a
	// recognized machine title.
	Skipped int

	// AlreadyKnown counts activities the named log already had, which is what
	// makes a second run cheap and a resumed one correct.
	AlreadyKnown int

	// Pages is how many requests the listing cost.
	Pages int
}

// Run walks the athlete's activities and seeds the named log.
//
// Idempotent and resumable with no state of its own: an activity already in
// the named log is left exactly as it is, so a re-run rewrites nothing and a
// run interrupted halfway continues from where the log ends. Re-listing costs
// a few requests, which is cheaper than remembering a cursor and having to
// keep it honest.
func Run(ctx context.Context, deps Deps) (Result, error) {
	switch {
	case deps.Activities == nil:
		return Result{}, errors.New("importer: a Strava client is required")
	case deps.Store == nil:
		return Result{}, errors.New("importer: a store is required")
	case deps.AthleteID == 0:
		return Result{}, errors.New("importer: an athlete is required")
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	perPage := deps.PerPage
	if perPage == 0 {
		perPage = strava.MaxActivitiesPerPage
	}

	pause := deps.Pause
	if pause == nil {
		pause = sleepContext
	}

	if deps.MachineTitles.IsEmpty() {
		deps.MachineTitles = classifier.DefaultMachineTitles()
	}

	var result Result

	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		if page > 1 {
			if err := pause(ctx, pageInterval); err != nil {
				return result, err
			}
		}

		activities, err := deps.Activities.ListActivities(ctx, page, perPage)
		if err != nil {
			// Reported with the page reached, so a resumed run is a decision
			// rather than a guess — though it needs no argument to resume.
			return result, fmt.Errorf("importer: list page %d: %w", page, err)
		}

		result.Pages++

		if len(activities) == 0 {
			break
		}

		for index := range activities {
			if err := deps.importOne(ctx, &activities[index], &result, logger); err != nil {
				return result, err
			}
		}

		logger.Info("imported a page",
			"page", page, "seen", result.Seen, "imported", result.Imported)

		// A short page is the last one. Asking for the next would spend a
		// request and a pause to be told what this page already said.
		if len(activities) < perPage {
			break
		}
	}

	logger.Info("import complete",
		"seen", result.Seen,
		"imported", result.Imported,
		"skipped", result.Skipped,
		"already_known", result.AlreadyKnown,
		"pages", result.Pages)

	return result, nil
}

// importOne records a single activity's title, if it is one worth having.
func (d Deps) importOne(
	ctx context.Context, activity *strava.Activity, result *Result, logger *slog.Logger,
) error {
	result.Seen++

	// A Strava default is not a title, and a machine title is one this service
	// exists to replace. Either would arrive in RECENT as something not to
	// repeat — which is wrong for a default, since they repeat by design — and
	// in the few-shot examples as the athlete's style, which neither is. Fifty
	// "Morning Ride"s would do to the history exactly what commute templates
	// did before they were filtered out.
	if classifier.IsDefaultTitle(activity.Name) || d.MachineTitles.Matches(activity.Name) {
		result.Skipped++

		return nil
	}

	if _, known, err := d.Store.Named(ctx, d.AthleteID, activity.ID); err != nil {
		return fmt.Errorf("importer: read the named log for %d: %w", activity.ID, err)
	} else if known {
		// Left exactly as it is. A title this service wrote must not be
		// relabelled as imported, and re-importing an imported one would
		// rewrite a row to say the same thing.
		result.AlreadyKnown++

		return nil
	}

	// Dated by the ride, not by the import. Every entry stamped with the run's
	// clock would tie, leaving the order to the activity-ID tiebreak; and
	// StartDateLocal is the wall-clock value Strava sends with a misleading Z,
	// which would jumble the ordering by whole hours. StartDate is the real
	// instant, so RecentTitles keeps meaning "the most recent rides" — and
	// anything this service writes later, stamped now, sorts after all of it.
	if err := d.Store.MarkNamed(ctx, store.Naming{
		AthleteID:  d.AthleteID,
		ActivityID: activity.ID,
		Title:      activity.Name,
		Language:   Language(activity.Name),
		Source:     store.SourceImported,
		At:         activity.StartDate,
	}); err != nil {
		return fmt.Errorf("importer: record %d: %w", activity.ID, err)
	}

	result.Imported++

	logger.Debug("imported a title",
		"activity_id", activity.ID, "title", logsafe.String(activity.Name))

	return nil
}

// sleepContext waits, or gives up when the context does.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
