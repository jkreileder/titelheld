package processor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// exampleCount is how many few-shot examples the prompt carries.
//
// The spec asks for about six. Each one costs a Strava read the first time it
// is derived, so the number is also a budget.
const exampleCount = 6

// promptContext gathers everything the prompt needs beyond the ride itself.
//
// The title history drives three of the four: the RECENT list, the few-shot
// examples derived from it, and nothing else uses it. A failure to read it is
// a failure of the activity, not something to work around — see history.
func (p *Processor) promptContext(
	ctx context.Context, athleteID int64, ride naming.Ride, logger *slog.Logger,
) (naming.Context, error) {
	history, err := p.history(ctx, athleteID)
	if err != nil {
		return naming.Context{}, err
	}

	promptContext := naming.Context{
		RecentTitles: titlesOf(history),
		Examples:     p.examplesFrom(ctx, history, logger),
	}

	promptContext.FranchiseNext = p.franchiseNext(ctx, athleteID, ride, logger)

	return promptContext, nil
}

// history reads the titles this service has written for an athlete.
//
// A failure here fails the activity and leaves it queued. The realistic cause
// is the composite index missing, which is a deployment error that fixes
// itself on the next apply — and naming without history in the meantime would
// produce exactly the repetition the history exists to prevent, with nothing
// in the log to say why.
func (p *Processor) history(ctx context.Context, athleteID int64) ([]store.NamedTitle, error) {
	history, err := p.deps.Store.RecentTitles(ctx, athleteID, naming.RecentTitleLimit)
	if err != nil {
		return nil, fmt.Errorf("read the title history: %w", err)
	}

	return history, nil
}

func titlesOf(history []store.NamedTitle) []string {
	titles := make([]string, 0, len(history))
	for _, entry := range history {
		titles = append(titles, entry.Title)
	}

	return titles
}

// franchiseNext offers the next entry of a series the ride qualifies for.
//
// Never blocking. A gear lookup that fails, a franchise that has run out, an
// athlete with no franchises: all of them mean this ride is named normally,
// which is the same outcome as the feature being off.
func (p *Processor) franchiseNext(
	ctx context.Context, athleteID int64, ride naming.Ride, logger *slog.Logger,
) string {
	franchise, ok := naming.FranchiseFor(p.deps.Franchises, ride.SportType, ride.GearName)
	if !ok {
		return ""
	}

	position, err := p.deps.Store.FranchisePosition(ctx, athleteID, franchise.Name)
	if err != nil {
		logger.Warn("could not read the franchise position; naming without it",
			"franchise", logsafe.String(franchise.Name), "error", err)

		return ""
	}

	next, ok := franchise.Next(position)
	if !ok {
		logger.Info("franchise exhausted; naming normally",
			"franchise", logsafe.String(franchise.Name), "position", position)

		return ""
	}

	return next
}

// examplesFrom derives few-shot examples from the title history.
//
// The spec asks for examples in the athlete's own style, derived at runtime
// rather than committed. The named log holds the title and the language; the
// situation that produced it does not survive, so it is rebuilt by re-reading
// the activity from Strava.
//
// That costs a read per example, which is why the result is cached against
// the history it came from. The history changes only when something is named,
// so a dry-run sweep repeating every five minutes pays once, not every time.
//
// Failure is not fatal. Fewer examples, or the shipped synthetic set, is a
// worse prompt and not a wrong one.
func (p *Processor) examplesFrom(
	ctx context.Context, history []store.NamedTitle, logger *slog.Logger,
) []naming.Example {
	if len(history) == 0 {
		// Nothing written yet. The synthetic set is what it is for.
		return naming.SyntheticExamples()
	}

	key := historyKey(history)

	p.examplesMu.Lock()
	if key == p.examplesKey && p.examples != nil {
		cached := p.examples
		p.examplesMu.Unlock()

		return cached
	}
	p.examplesMu.Unlock()

	examples := make([]naming.Example, 0, exampleCount)

	for _, entry := range history {
		if len(examples) >= exampleCount {
			break
		}

		activity, err := p.deps.Activities.GetActivity(ctx, entry.ActivityID)
		if err != nil {
			logger.Warn("could not re-read an activity for a few-shot example",
				"activity_id", entry.ActivityID, "error", err)

			continue
		}

		examples = append(examples, naming.Example{
			Situation: situationOf(activity),
			Title:     entry.Title,
			Language:  naming.Language(entry.Language),
		})
	}

	if len(examples) == 0 {
		return naming.SyntheticExamples()
	}

	p.examplesMu.Lock()
	p.examplesKey = key
	p.examples = examples
	p.examplesMu.Unlock()

	return examples
}

// historyKey identifies a history, so a cached derivation can be recognized.
func historyKey(history []store.NamedTitle) string {
	var b strings.Builder

	for _, entry := range history {
		fmt.Fprintf(&b, "%d:%s\n", entry.ActivityID, entry.Title)
	}

	digest := sha256.Sum256([]byte(b.String()))

	return hex.EncodeToString(digest[:])
}

// situationOf describes a ride in one line, for a few-shot example.
//
// Shape and time only. No place names: they would need geocoding every
// example, and an example is there to show the style of a title, not to
// supply geography the model may use — the PLACES list is the only geography
// the prompt permits.
func situationOf(activity *strava.Activity) string {
	parts := make([]string, 0, 4)

	if activity.Distance > 0 {
		parts = append(parts, fmt.Sprintf("%.0f km", activity.Distance/1000))
	}

	sport := activity.SportType
	if sport == "" {
		sport = "ride"
	}

	parts = append(parts, sport)

	if activity.TotalElevGain > 0 {
		parts = append(parts, fmt.Sprintf("%.0f m climbing", activity.TotalElevGain))
	}

	if !activity.StartDateLocal.IsZero() {
		parts = append(parts, fmt.Sprintf("%s %02d:00",
			activity.StartDateLocal.Weekday(), activity.StartDateLocal.Hour()))
	}

	return strings.Join(parts, ", ")
}

// routeHistory reports how often this route has been ridden before.
//
// Read, never written, at this point: the count is recorded with the title, so
// a naming that fails does not inflate it. The count offered to the prompt is
// this ride's ordinal, which is one more than what is stored.
func (p *Processor) routeHistory(
	ctx context.Context, athleteID int64, fingerprint string, logger *slog.Logger,
) (string, int) {
	if fingerprint == "" {
		return "", 0
	}

	route, ok, err := p.deps.Store.Route(ctx, athleteID, fingerprint)
	if err != nil {
		logger.Warn("could not read the route history; naming without it", "error", err)

		return "", 0
	}

	if !ok {
		return "", 1
	}

	return route.FirstSeen.Format("2 January 2006"), route.Count + 1
}
