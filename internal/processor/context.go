package processor

import (
	"context"
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

// maxExampleFailures stops the derivation walking the whole history when
// Strava is refusing reads. Each failed read has already been retried with
// backoff by the client.
const maxExampleFailures = 2

// gathered is the prompt context, plus what the caller has to remember about
// how it was built.
//
// The franchise name travels with the entry it produced, so the series that
// gets advanced is by construction the one the prompt was shown. Resolving it
// twice would work today — the lookup is pure and the inputs are the same —
// and would drift the moment either site's inputs or franchise list changed,
// leaving the prompt offering one series while the store advanced another.
type gathered struct {
	Context naming.Context

	// Franchise is the series the offered entry came from. Empty when none
	// applied, which is also when Context.FranchiseNext is empty.
	Franchise string
}

// promptContext gathers everything the prompt needs beyond the ride itself.
//
// The title history drives two of the three: the RECENT list and the few-shot
// examples derived from it, and nothing else uses it. A failure to read it is
// a failure of the activity, not something to work around — see history.
func (p *Processor) promptContext(
	ctx context.Context, athleteID int64, ride naming.Ride, logger *slog.Logger,
) (gathered, error) {
	history, err := p.history(ctx, athleteID)
	if err != nil {
		return gathered{}, err
	}

	promptContext := naming.Context{
		// "Never repeat a title listed under RECENT" is aimed at the ones a
		// model invented. A commute template is meant to repeat, and listing
		// it both forbids the right answer and crowds the real titles out of
		// a list of twenty-five.
		RecentTitles: titlesOf(llmTitlesOnly(history)),
		Examples:     p.examplesFrom(ctx, history, logger),
	}

	next, franchise := p.franchiseNext(ctx, athleteID, ride, logger)
	promptContext.FranchiseNext = next

	return gathered{Context: promptContext, Franchise: franchise}, nil
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

// franchiseNext offers the next entry of a series the ride qualifies for, and
// names the series it came from.
//
// Both together, so a caller cannot advance a different series than the one
// the prompt was shown.
//
// Never blocking. A gear lookup that fails, a franchise that has run out, an
// athlete with no franchises: all of them mean this ride is named normally,
// which is the same outcome as the feature being off.
func (p *Processor) franchiseNext(
	ctx context.Context, athleteID int64, ride naming.Ride, logger *slog.Logger,
) (next, franchiseName string) {
	franchise, ok := naming.FranchiseFor(
		p.franchises(ctx, athleteID, logger), ride.SportType, ride.GearName)
	if !ok {
		return "", ""
	}

	position, err := p.deps.Store.FranchisePosition(ctx, athleteID, franchise.Name)
	if err != nil {
		logger.Warn("could not read the franchise position; naming without it",
			"franchise", logsafe.String(franchise.Name), "error", err)

		return "", ""
	}

	next, ok = franchise.Next(position)
	if !ok {
		logger.Info("franchise exhausted; naming normally",
			"franchise", logsafe.String(franchise.Name), "position", position)

		return "", ""
	}

	return next, franchise.Name
}

// franchises are the athlete's configured series.
//
// Read from the configuration document, because a franchise is data: adding
// one is an edit to a document, not a release. An athlete with no document
// gets the shipped default profile, which is what every deployment starts
// with and what a first document is seeded from.
//
// A document that cannot be read degrades to the default profile rather than
// failing the naming. A franchise is garnish — the ride still gets a title —
// and the alternative is a ride left with its Strava default because a
// configuration read timed out. The failure is logged rather than swallowed.
//
// Cached for the life of the process. Configuration changes about as often as
// a person edits it, and a restart is what picks it up: the same trade the
// gear cache makes, for the same reason.
func (p *Processor) franchises(
	ctx context.Context, athleteID int64, logger *slog.Logger,
) []naming.Franchise {
	if p.deps.Franchises != nil {
		return p.deps.Franchises
	}

	p.franchiseMu.Lock()
	defer p.franchiseMu.Unlock()

	if p.franchiseLoaded {
		return p.franchiseCache
	}

	p.franchiseCache = naming.DefaultProfile()
	p.franchiseLoaded = true

	config, ok, err := p.deps.Store.AthleteConfig(ctx, athleteID)
	if err != nil {
		logger.Error("could not read the athlete configuration; using the default profile",
			"error", err)

		return p.franchiseCache
	}

	if !ok {
		logger.Info("no athlete configuration; using the default franchise profile")

		return p.franchiseCache
	}

	p.franchiseCache = fromStored(config.Franchises)

	logger.Info("loaded the athlete configuration", "franchises", len(p.franchiseCache))

	return p.franchiseCache
}

// fromStored converts the persisted shape into the one with behavior.
//
// The two are deliberately separate types: a franchise has methods here and a
// schema on disk, and letting one type be both means a refactor in this
// package silently rewrites what Firestore expects.
func fromStored(stored []store.Franchise) []naming.Franchise {
	franchises := make([]naming.Franchise, 0, len(stored))

	for _, entry := range stored {
		franchises = append(franchises, naming.Franchise{
			Name:       entry.Name,
			SportTypes: entry.SportTypes,
			GearName:   entry.GearName,
			Titles:     entry.Titles,
		})
	}

	return franchises
}

// examplesFrom derives few-shot examples from the title history.
//
// The spec asks for examples in the athlete's own style, derived at runtime
// rather than committed. The named log holds the title and the language; the
// situation that produced it does not survive, so it is rebuilt by re-reading
// the activity from Strava.
//
// That costs a read per example, which is why each one is cached against the
// activity it describes — see [Processor.example] for why the activity and
// not the history. A past ride's situation does not change, so a derivation
// is paid once however often the prompt is rebuilt.
//
// Failure is not fatal. Fewer examples, or the shipped synthetic set, is a
// worse prompt and not a wrong one.
func (p *Processor) examplesFrom(
	ctx context.Context, history []store.NamedTitle, logger *slog.Logger,
) []naming.Example {
	// Only titles a model wrote. A commute template is the same two strings
	// every working day, and six of them would teach the model to name a
	// gravel ride "Zur Arbeit".
	history = llmTitlesOnly(history)

	if len(history) == 0 {
		// Nothing written yet. The synthetic set is what it is for.
		return naming.SyntheticExamples()
	}

	examples := make([]naming.Example, 0, exampleCount)

	// Failures are budgeted, not retried through the whole history. Client.do
	// already retries a rate-limited read several times with backoff, so a
	// Strava that is refusing everything would otherwise cost a hundred
	// requests per activity, per sweep — amplifying the very condition that
	// caused it.
	failures := 0

	for _, entry := range history {
		if len(examples) >= exampleCount || failures >= maxExampleFailures {
			break
		}

		example, ok := p.example(ctx, entry, logger)
		if !ok {
			failures++

			continue
		}

		examples = append(examples, example)
	}

	if len(examples) == 0 {
		return naming.SyntheticExamples()
	}

	return examples
}

// example builds one few-shot example, from cache if it has been built before.
//
// Cached per activity rather than per history. The history changes every time
// something is named, so a cache keyed on the whole of it misses for every
// activity after the first in a sweep — six Strava reads each, against a
// hundred per fifteen minutes. A past activity's situation does not change,
// so the entry that was derived for it stays valid however the history moves.
func (p *Processor) example(
	ctx context.Context, entry store.NamedTitle, logger *slog.Logger,
) (naming.Example, bool) {
	p.examplesMu.Lock()
	cached, ok := p.examples[entry.ActivityID]
	p.examplesMu.Unlock()

	if ok {
		return cached, true
	}

	activity, err := p.deps.Activities.GetActivity(ctx, entry.ActivityID)
	if err != nil {
		logger.Warn("could not re-read an activity for a few-shot example",
			"activity_id", entry.ActivityID, "error", err)

		return naming.Example{}, false
	}

	example := naming.Example{
		Situation: situationOf(activity),
		Title:     entry.Title,
		Language:  exampleLanguage(entry.Language),
	}

	p.examplesMu.Lock()
	p.examples[entry.ActivityID] = example
	p.examplesMu.Unlock()

	return example, true
}

// exampleLanguage falls back rather than rendering an empty token.
//
// The prompt prints every example as "situation -> title (language)", so an
// entry with no language — one written before the field existed, or by a path
// that omits it — would put "()" in front of the model. German is this
// athlete's default and a better guess than nothing.
func exampleLanguage(language string) naming.Language {
	switch naming.Language(language) {
	case naming.German, naming.English:
		return naming.Language(language)
	default:
		return naming.German
	}
}

// llmTitlesOnly keeps the titles a model wrote.
//
// Entries recorded before the source field existed have none, and are kept:
// there are none in production, and treating an unknown source as a template
// would silently empty the history.
func llmTitlesOnly(history []store.NamedTitle) []store.NamedTitle {
	kept := make([]store.NamedTitle, 0, len(history))

	for _, entry := range history {
		if entry.Source != store.SourceTemplate {
			kept = append(kept, entry)
		}
	}

	return kept
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
