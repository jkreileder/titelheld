package processor

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"regexp"
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

	// Series is the franchise the ride matched. Zero when none did — and
	// kept whole, and kept even when it had nothing to offer, because the
	// guard that refuses a title claiming an entry needs every entry of the
	// series and not only the offerable ones.
	Series naming.Franchise

	// FranchiseIndex is where in that series the offered entry sits. The
	// position is advanced past this index and not by one step, because the
	// rotation walks over reserved entries: advancing by one from a position
	// that stepped over two would offer a reserved title next.
	FranchiseIndex int
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
		// "Never repeat a title listed under RECENT" is aimed at every title
		// that was a choice — this service's own and the athlete's imported
		// past alike, because repeating either is the thing to avoid. Only a
		// commute template is dropped: it is meant to repeat, so listing it
		// forbids the right answer and crowds the real titles out of a list of
		// twenty-five.
		RecentTitles: titlesOf(worthNotRepeating(history)),
		Examples:     p.examplesFrom(ctx, history, logger),
	}

	next, index, series := p.franchiseNext(ctx, athleteID, ride, logger)
	promptContext.FranchiseNext = next

	return gathered{
		Context:        promptContext,
		Series:         series,
		FranchiseIndex: index,
	}, nil
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
// names the series it came from and the index it sits at.
//
// All three together, so a caller cannot advance a different series than the
// one the prompt was shown, or a different place in it.
//
// The series comes back whenever one matched, including on every path that
// offers nothing. An offer and a guard are different questions: the offer asks
// what this ride may be handed, and the guard asks which entries it may not
// take for itself — and a series with nothing left to offer is exactly the
// case where every entry it holds is somebody else's to spend.
//
// Never blocking. A gear lookup that fails, a franchise that has run out, an
// athlete with no franchises: all of them mean this ride is named normally,
// which is the same outcome as the feature being off.
func (p *Processor) franchiseNext(
	ctx context.Context, athleteID int64, ride naming.Ride, logger *slog.Logger,
) (next string, index int, series naming.Franchise) {
	franchise, ok := naming.FranchiseFor(
		p.franchises(ctx, athleteID, logger), ride.SportType, ride.GearName)
	if !ok {
		return "", 0, naming.Franchise{}
	}

	position, err := p.deps.Store.FranchisePosition(ctx, athleteID, franchise.Name)
	if err != nil {
		logger.Warn("could not read the franchise position; naming without it",
			"franchise", logsafe.String(franchise.Name), "error", err)

		// Nothing is offered, so the guard forbids the whole series. Not
		// knowing how far along the athlete is means not knowing which
		// entries are still theirs, and the safe reading of that is all of
		// them.
		return "", 0, franchise
	}

	next, index, ok = franchise.Next(position)
	if !ok {
		// Exhausted, or nothing left that is not reserved for the athlete to
		// spend by hand. Both mean the same thing here: this ride is named
		// normally.
		logger.Info("no franchise entry to offer; naming normally",
			"franchise", logsafe.String(franchise.Name), "position", position)

		return "", 0, franchise
	}

	// An entry that cannot be a title is not offered, and the position stays
	// where it is. Offering it would print a truncated version of it, which no
	// title can contain: the entry would go unused on this ride and on every
	// ride after it, three model calls at a time, with nothing in the log
	// saying why. This says why, on every ride, until the document is fixed.
	if !naming.Offerable(next) {
		logger.Error("the next franchise entry cannot be a title; naming without it",
			"franchise", logsafe.String(franchise.Name),
			"position", index,
			"entry", logsafe.String(next),
			"limit_runes", naming.MaxTitleRunes)

		return "", 0, franchise
	}

	return next, index, franchise
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

	if cached, ok := p.franchiseCache[athleteID]; ok {
		return cached
	}

	config, ok, err := p.deps.Store.AthleteConfig(ctx, athleteID)
	if err != nil {
		// Not cached. Answering from the default profile is the right thing to
		// do for this ride, and the wrong thing to keep doing: if the athlete
		// removed or renamed a series, every later ride in the process would
		// still be offered it, and the advance would durably count a
		// position the configuration no longer names. A repeated read is
		// cheap; a wrong write is not.
		logger.Error("could not read the athlete configuration; naming this ride from the default profile",
			"error", err)

		return naming.DefaultProfile()
	}

	// A successful read is remembered, including "no document" — that is a
	// real answer, and re-reading it on every activity would be a request per
	// ride to learn the same thing.
	resolved := naming.DefaultProfile()

	if ok {
		resolved = FranchisesFromStored(config.Franchises)

		logger.Info("loaded the athlete configuration", "franchises", len(resolved))
	} else {
		logger.Info("no athlete configuration; using the default franchise profile")
	}

	p.franchiseCache[athleteID] = resolved

	return resolved
}

// FranchisesFromStored converts the persisted shape into the one with
// behavior.
//
// The two are deliberately separate types: a franchise has methods here and a
// schema on disk, and letting one type be both means a refactor in this
// package silently rewrites what Firestore expects.
//
// Exported for the one other reader of the document, the seeding command,
// which reads back what it wrote through the same conversion the sweep uses
// — so what it reports as the next offer is what a sweep would compute.
func FranchisesFromStored(stored []store.Franchise) []naming.Franchise {
	franchises := make([]naming.Franchise, 0, len(stored))

	for _, entry := range stored {
		// Typed by a person into a document now, not written as a Go literal.
		// A trailing space on the gear name would make the series silently
		// inapplicable forever, and an empty name is not a key: the position
		// would be stored under an empty document ID, which is an error on
		// every ride the series matches.
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}

		franchises = append(franchises, naming.Franchise{
			Name:       name,
			SportTypes: entry.SportTypes,
			GearName:   strings.TrimSpace(entry.GearName),
			Titles:     entry.Titles,
			Reserved:   entry.Reserved,
		})
	}

	return franchises
}

// FranchisesToStored is the inverse of [FranchisesFromStored], for writing a
// profile the code ships into the document the athlete curates from then on.
func FranchisesToStored(franchises []naming.Franchise) []store.Franchise {
	stored := make([]store.Franchise, 0, len(franchises))

	for _, franchise := range franchises {
		stored = append(stored, store.Franchise{
			Name:       franchise.Name,
			SportTypes: franchise.SportTypes,
			GearName:   franchise.GearName,
			Titles:     franchise.Titles,
			Reserved:   franchise.Reserved,
		})
	}

	return stored
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
	// Only what teaches: a title the athlete wrote by hand. Not a filter over
	// titles that might be unsuitable — an admitted source, which neither an
	// imported row nor the service's own rows carry, so a decade of shorthand
	// and the pipeline's own output are structurally unable to become examples
	// rather than pattern-matched out of them.
	history = teachesStyle(history)

	if len(history) == 0 {
		// Nothing written yet. The synthetic set is what it is for, and it is
		// what the prompt carries until this service has named something.
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

// worthNotRepeating keeps the titles the RECENT list is about.
//
// Everything except a template: a title this service invented and a title the
// athlete gave a ride years ago are both worth not repeating, and a commute
// name is not.
//
// Entries recorded before the source field existed have none, and are kept:
// there are none in production, and treating an unknown source as a template
// would silently empty the list.
func worthNotRepeating(history []store.NamedTitle) []store.NamedTitle {
	kept := make([]store.NamedTitle, 0, len(history))

	for _, entry := range history {
		if entry.Source != store.SourceTemplate {
			kept = append(kept, entry)
		}
	}

	return kept
}

// teachesStyle keeps the titles a few-shot example may be built from.
//
// One source and no other: [store.SourceHuman], a title the athlete wrote on
// a sport ride since the recorder existed. The athlete's hand-namings are the
// best style data there will ever be, and they are the only titles here that
// carry a judgment: writing one is a choice, while leaving a service title
// standing is not approval of it — it is the athlete not editing a feed.
//
// [store.SourceService] stays out because admitting it closes the style loop
// on itself: a title this pipeline wrote is the floor the model produced under
// this same prompt, and an example set carrying it teaches the model to
// imitate its own weakest habits. The human channel is what grows the corpus
// — the reconcile records every rename and every skip-gate decline — and it
// grows it at the quality an example needs.
//
// [store.SourceImported] stays out too. That rule's target was a decade of
// shorthand — bare town names, private jokes, whatever a tool left behind —
// and an example set built from it teaches a model to answer "Regensburg".
// Which rows those are is decided by the source they carry, not by what the
// title looks like: the same athlete wrote both kinds, and no pattern tells
// last week's title from one from years ago.
//
// The opposite default to [worthNotRepeating], and deliberately so: this one
// admits a source rather than excluding some, so a row whose source is
// unknown, misspelled, or added later is not an example. An example teaches
// the model what a title should sound like, and the cost of a wrong one is
// every title afterwards.
func teachesStyle(history []store.NamedTitle) []store.NamedTitle {
	kept := make([]store.NamedTitle, 0, len(history))

	for _, entry := range history {
		if entry.Source == store.SourceHuman {
			kept = append(kept, entry)
		}
	}

	return kept
}

// situationOf describes a ride in one line, for a few-shot example.
//
// Shape, time, and the difficulty another tool wrote into the description when
// there is one to parse. No tally of records or achievements: a figure may
// reach the model only if it is one the athlete could check, and there is no
// such figure for what a ride achieved — Strava's own web feed, mobile feed
// and API report three different numbers for the same ride, and a local legend
// appears in none of the segment-effort data this service reads. A situation
// stating a count would teach that counts are a thing to write about.
//
// Never a segment name either: a name is somebody else's text and often
// carries a place, and an example is not a route through the geography rule.
// No place names for the same reason — they would need geocoding every
// example, and the PLACES list is the only geography the prompt permits.
func situationOf(activity *strava.Activity) string {
	parts := make([]string, 0, 8)

	if activity.Distance > 0 {
		parts = append(parts, fmt.Sprintf("%.0f km", activity.Distance/1000))
	}

	sport := cmp.Or(activity.SportType, "ride")

	parts = append(parts, sport)

	if activity.TotalElevGain > 0 {
		parts = append(parts, fmt.Sprintf("%.0f m climbing", activity.TotalElevGain))
	}

	if difficulty := difficultyOf(activity.Description); difficulty != "" {
		parts = append(parts, "difficulty "+difficulty)
	}

	if !activity.StartDateLocal.IsZero() {
		parts = append(parts, fmt.Sprintf("%s %02d:00",
			activity.StartDateLocal.Weekday(), activity.StartDateLocal.Hour()))
	}

	return strings.Join(parts, ", ")
}

// difficultyOf reads the difficulty a tool wrote into the description.
//
// Through the same parser the prompt uses for the ride being named, so the
// example and the ride describe the fact the same way. Empty when no tool
// wrote one — and empty when what it wrote is not a difficulty. An example
// line is "situation -> title (language)", so a value carrying an arrow or a
// parenthesis would read as a second mapping inside the first; the value is
// admitted by shape rather than stripped of delimiters, because a difficulty
// is a word or two, or a number, and anything else is not one.
func difficultyOf(description string) string {
	for _, fact := range naming.ParseFacts(description) {
		if fact.Label == naming.LabelDifficulty && difficultyShape.MatchString(fact.Value) {
			return fact.Value
		}
	}

	return ""
}

// difficultyShape is what a difficulty looks like: up to three words of
// letters, or a number. "Tough", "Very Difficult", "112" and "3.5" pass;
// "x -> Sieg (de)" does not.
var difficultyShape = regexp.MustCompile(`^(\p{L}+( \p{L}+){0,2}|[0-9]+([.,][0-9]+)?)$`)
