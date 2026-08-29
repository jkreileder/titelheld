package processor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/geo"
	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// title produces the title for an activity the classifier has cleared.
//
// The deterministic tiers do not consult an LLM at all: a commute has two
// possible names and an errand has a small pool, and asking a model to pick
// from a list of two is spending money to add variance.
func (p *Processor) title(
	ctx context.Context, athleteID int64, activity *strava.Activity,
	decision classifier.Decision, logger *slog.Logger,
) (titled, error) {
	switch decision.Action {
	case classifier.ActionCommuteTemplate:
		// German templates, and the language is stated rather than guessed:
		// the named log keeps it, and few-shot examples read it back.
		return titled{
			Text:     p.deps.Classifier.CommuteTitle(decision.Direction),
			Language: naming.German,
			Source:   store.SourceTemplate,
		}, nil

	case classifier.ActionErrandTemplate:
		return titled{
			Text:     errandTitle(activity),
			Language: naming.German,
			Source:   store.SourceTemplate,
		}, nil

	case classifier.ActionLLM, classifier.ActionLLMIndoor:
		return p.llmTitle(ctx, athleteID, activity, decision, logger)

	default:
		return titled{}, fmt.Errorf("unhandled action %v", decision.Action)
	}
}

// titled is a title and what is known about it.
//
// Franchise is the series entry this title came from, if any. It is carried
// this far because the position is advanced with the write and not before: a
// naming that fails must not consume an entry.
type titled struct {
	Text     string
	Language naming.Language

	// Franchise is the series this title demonstrably used, if any. Empty when
	// no series applied and equally when one was offered and the title did not
	// use it — an offer that was declined costs nothing, so there is nothing
	// for the writer to record.
	Franchise string

	// FranchiseEntry is the entry itself, carried so the writer can check that
	// what Strava stored still contains it. Only meaningful when Franchise is
	// set.
	FranchiseEntry string

	// FranchiseIndex is where in that series the used entry sat. Only
	// meaningful when Franchise is set.
	FranchiseIndex int

	// Source is how the title was produced, for the history. A template and a
	// model's title are not interchangeable to a later prompt.
	Source string
}

// llmTitle gathers, prompts, calls and validates.
func (p *Processor) llmTitle(
	ctx context.Context, athleteID int64, activity *strava.Activity,
	decision classifier.Decision, logger *slog.Logger,
) (titled, error) {
	if p.deps.Provider == nil {
		return titled{}, errors.New("no LLM provider configured")
	}

	ride := naming.Ride{
		SportType:           activity.SportType,
		DistanceKm:          activity.Distance / 1000,
		MovingTimeMinutes:   activity.MovingTime / 60,
		ElevationGainMeters: activity.TotalElevGain,
		AverageSpeedKmh:     activity.AverageSpeed * 3.6,
		StartLocal:          activity.StartDateLocal,
		Facts:               naming.ParseFacts(activity.Description),
	}

	ride.GearName = p.gearName(ctx, activity.GearID, logger)
	ride.Achievements = achievementsOf(activity)

	// The counts beside the names, from the same rule a derived example's
	// situation uses: a title that escalates a number must be escalating the
	// number the examples taught, and the list of names is capped.
	ride.Records, ride.OtherAchievements = countEfforts(activity.SegmentEfforts)

	// An indoor ride is named from effort and season only. Watopia is not in
	// the Solomon Islands, and geocoding a fictional coordinate would produce
	// a confidently wrong place name.
	if decision.Action == classifier.ActionLLM {
		summary, err := p.geography(ctx, activity, logger)
		if err != nil {
			return titled{}, err
		}

		ride.Places = summary.Names()
		ride.Region = summary.Region
		ride.Country = summary.Country
	}

	gathered, err := p.promptContext(ctx, athleteID, ride, logger)
	if err != nil {
		return titled{}, err
	}

	title, used, err := p.complete(ctx, ride, gathered, logger)
	if err != nil {
		return titled{}, err
	}

	logger.Info("named",
		"title", logsafe.String(title.Text),
		"language", string(title.Language),
		"places", len(ride.Places),
		"achievements", len(ride.Achievements),
		"facts", len(ride.Facts),
		"recent_titles", len(gathered.Context.RecentTitles),
		"examples", len(gathered.Context.Examples),
		"franchise_offered", gathered.Context.FranchiseNext != "",
		"franchise_used", used)

	named := titled{
		Text:     title.Text,
		Language: title.Language,
		Source:   store.SourceService,
	}

	// The series that gets advanced is the one the prompt was shown, carried
	// out of the single lookup that resolved it rather than resolved again
	// here — and only when the title that came back actually used the entry.
	if used {
		named.Franchise = gathered.Series.Name
		named.FranchiseEntry = gathered.Context.FranchiseNext
		named.FranchiseIndex = gathered.FranchiseIndex
	}

	return named, nil
}

// maxFranchiseOffers is how many times one activity may be shown the same
// franchise entry.
//
// Two: the offer, and one more attempt. A model that has declined an entry
// twice is not going to be argued into it, and every further attempt is a
// paid call and a longer sweep for a title that would be no better.
const maxFranchiseOffers = 2

// complete asks the model for a title, and insists on the franchise entry only
// as far as the spec allows.
//
// The order is: offer the entry, and if the title uses it, that is the title
// and the entry is spent. If it does not, offer it once more — the model is
// sampled at temperature, so a second draw is a real second chance and not a
// repeat of the first. If that title does not use it either, the ride is named
// without the series: a third call carrying no FRANCHISE block, so what gets
// written is an ordinary title rather than one that was reaching for a film it
// never named.
//
// It reports whether the entry was used, which is the only thing that may
// advance the position.
func (p *Processor) complete(
	ctx context.Context, ride naming.Ride, gathered gathered, logger *slog.Logger,
) (naming.Title, bool, error) {
	entry := gathered.Context.FranchiseNext

	// Every entry of the ride's series except the one being offered. The
	// prompt asks the motif to leave the athlete's films alone; this is what
	// makes the request binding, and it binds the very first call, before any
	// series has been offered at all.
	guard := gathered.Series.Guard(entry)

	title, err := p.ask(ctx, naming.BuildPrompt(ride, gathered.Context), guard, logger)
	if err != nil {
		return naming.Title{}, false, err
	}

	if entry == "" {
		return title, false, nil
	}

	for offer := 1; offer <= maxFranchiseOffers; offer++ {
		if naming.UsesEntry(title.Text, entry) {
			return title, true, nil
		}

		if offer == maxFranchiseOffers {
			break
		}

		logger.Info("the title did not use the franchise entry; offering it once more",
			"franchise", logsafe.String(gathered.Series.Name),
			"entry", logsafe.String(entry),
			"title", logsafe.String(title.Text))

		retry, err := p.ask(ctx, naming.BuildPrompt(ride, gathered.Context), guard, logger)
		if err != nil {
			// The title in hand is a good one that simply ignored the series.
			// Losing it because a second call failed would leave the ride at
			// its Strava default over a franchise, which is garnish.
			logger.Warn("the second franchise offer failed; naming without the series",
				"franchise", logsafe.String(gathered.Series.Name), "error", err)

			return title, false, nil
		}

		title = retry
	}

	logger.Info("the franchise entry went unused twice; naming without the series",
		"franchise", logsafe.String(gathered.Series.Name),
		"entry", logsafe.String(entry))

	// Named again with no FRANCHISE block, so the title that gets written is
	// not one that was steering towards an entry it never used.
	plain := gathered.Context
	plain.FranchiseNext = ""

	// Nothing is offered any more, so the entry that was offered joins the
	// guard. Without that, a title from this call could still claim it — and
	// this call's title is written with the position left where it was, so
	// the entry would be spent on the feed and offered again on the next ride.
	plainTitle, err := p.ask(ctx, naming.BuildPrompt(ride, plain), gathered.Series.Guard(""), logger)
	if err != nil {
		logger.Warn("naming without the series failed; keeping the title that ignored it",
			"error", err)

		return title, false, nil
	}

	return plainTitle, false, nil
}

// ask makes one provider call and validates what comes back.
//
// The guard is the second half of it. Reserving a franchise entry governs what
// the rotation offers and nothing about what a model writes, so a title that
// claims an entry is refused here — the same place a banned word or an
// over-long title is refused, and with the same consequence: the activity
// fails and stays queued, so the next sweep draws again.
func (p *Processor) ask(
	ctx context.Context, prompt naming.Prompt, guard naming.Guarded, logger *slog.Logger,
) (naming.Title, error) {
	p.logPrompt(prompt, logger)

	raw, err := p.deps.Provider.Complete(ctx, prompt)
	if err != nil {
		return naming.Title{}, fmt.Errorf("llm %s: %w", p.deps.Provider.Name(), err)
	}

	title, trailing, err := p.deps.Validator.ParseAndValidate(raw)
	if err != nil {
		return naming.Title{}, fmt.Errorf(
			"llm %s returned an unusable title: %w", p.deps.Provider.Name(), err)
	}

	// The response carried the title and then kept going. Accepted rather
	// than refused — a byte after the object says nothing about the title
	// inside it — but recorded, because a provider that does this drifts and
	// the log is where that shows.
	if trailing != "" {
		logger.Warn("the model response carried trailing text after the JSON",
			"provider", p.deps.Provider.Name(), "trailing", logsafe.String(trailing))
	}

	if entry, claimed := guard.Claimed(title.Text); claimed {
		return naming.Title{}, fmt.Errorf(
			"llm %s returned an unusable title: %w: %q claims %q",
			p.deps.Provider.Name(), naming.ErrTitleClaimsEntry,
			logsafe.String(title.Text), logsafe.String(entry))
	}

	return title, nil
}

// maxAchievements bounds what the ACHIEVEMENTS block carries.
//
// A long ride crosses a lot of segments. Six is the same order as the few-shot
// examples: enough for the model to notice one, few enough that the block does
// not become the loudest thing in the prompt.
const maxAchievements = 6

// achievementsOf picks the efforts worth telling a model about.
//
// Names only. An effort carries times, ranks and identifiers; a title has no
// business with any of them, so nothing but the name crosses into the prompt —
// the same discipline the geo layer applies, where verified place names cross
// and coordinates cannot.
//
// Notable means a personal top-three on the segment, or an achievement Strava
// awarded. Strava already returns only what it considers notable with a
// detailed activity, but which efforts those are is Strava's decision and can
// change without telling anyone, so the selection is made here as well: the
// prompt should not fill up with every segment a long ride happened to cross.
//
// Deduplicated by name, because a loop ridden twice produces two efforts on
// one segment and "the same climb, twice" is not what a repeated line says to
// a model.
func achievementsOf(activity *strava.Activity) []string {
	names := make([]string, 0, maxAchievements)
	seen := make(map[string]bool, maxAchievements)

	for _, effort := range activity.SegmentEfforts {
		if len(names) >= maxAchievements {
			break
		}

		if !notableEffort(effort) {
			continue
		}

		name := strings.TrimSpace(effort.Name)
		if name == "" {
			name = strings.TrimSpace(effort.Segment.Name)
		}

		// Folded for the duplicate check only; the name reaches the prompt as
		// the athlete's community spelled it.
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}

		seen[key] = true

		names = append(names, name)
	}

	return names
}

// notableEffort reports whether an effort is worth a line in the prompt.
func notableEffort(effort strava.SegmentEffort) bool {
	return (effort.PRRank >= 1 && effort.PRRank <= 3) || len(effort.Achievements) > 0
}

// logPrompt writes the whole prompt, when asked to.
//
// Every attempt, not just the first: a ride that declines a franchise entry is
// named from up to three different prompts, and "what did the model receive"
// has three answers.
//
// Logged as two fields rather than one blob so a log viewer can fold them, and
// through logsafe like every other value that did not originate here — the
// prompt contains a gear name the athlete typed, ride notes other tools wrote
// and segment names a stranger chose.
func (p *Processor) logPrompt(prompt naming.Prompt, logger *slog.Logger) {
	if !p.deps.LogPrompt {
		return
	}

	logger.Info("prompt",
		"system", logsafe.Block(prompt.System),
		"user", logsafe.Block(prompt.User))
}

// gearName resolves a gear ID to the name a franchise matches on.
//
// Cached for the life of the process, and never fatal: a lookup that fails
// means no franchise applies to this ride, which is the same outcome as the
// athlete not having one.
func (p *Processor) gearName(ctx context.Context, gearID string, logger *slog.Logger) string {
	if gearID == "" {
		return ""
	}

	p.gearMu.Lock()
	cached, ok := p.gear[gearID]
	p.gearMu.Unlock()

	if ok {
		return cached
	}

	gear, err := p.deps.Activities.GetGear(ctx, gearID)
	if err != nil {
		logger.Warn("could not read the gear; naming without it", "error", err)

		return ""
	}

	name := sanitizeGearName(gear.Name)

	p.gearMu.Lock()
	p.gear[gearID] = name
	p.gearMu.Unlock()

	return name
}

// maxGearNameRunes bounds a gear name in the prompt.
const maxGearNameRunes = 60

// sanitizeGearName reduces a gear name to one short line.
//
// It is free text the athlete typed into Strava, and it reaches the prompt as
// a field rather than through a parser or an allow-list, which is how every
// other third-party string here is handled. A name containing newlines would
// restructure the prompt's blocks. Self-inflicted at worst — nobody else can
// set it — but the whole point of the other two defenses is that untrusted
// text never reaches the prompt verbatim.
func sanitizeGearName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			return ' '
		}

		return r
	}, name)

	name = strings.Join(strings.Fields(name), " ")

	if runes := []rune(name); len(runes) > maxGearNameRunes {
		name = string(runes[:maxGearNameRunes])
	}

	return name
}

// geography resolves place names, or reports why it could not.
//
// A ride with no polyline is not a failure: an indoor session or a recording
// with GPS off simply has no geography, and the prompt says less about it.
func (p *Processor) geography(
	ctx context.Context, activity *strava.Activity, logger *slog.Logger,
) (geo.Summary, error) {
	if p.deps.Geo == nil || activity.Map.SummaryPolyline == "" {
		return geo.Summary{}, nil
	}

	summary, err := p.deps.Geo.Describe(ctx, activity.Map.SummaryPolyline)
	if err != nil {
		// Geography is worth retrying: Nominatim rate-limits, and a title
		// invented without place names would be worse than one produced a
		// sweep later.
		return geo.Summary{}, fmt.Errorf("resolve geography: %w", err)
	}

	if summary.Empty() {
		logger.Info("no geography resolved; naming without place names")
	}

	return summary, nil
}

// errandTitle picks a deliberately boring German name.
//
// No LLM and no geocoding: an errand's destination is exactly the thing a
// title must not reveal, and the spec's privacy rule is that a title never
// comes from a reverse-geocoded point of interest. The pool is small on
// purpose — these titles are meant to be unremarkable.
func errandTitle(activity *strava.Activity) string {
	pool := classifier.DefaultErrandTitles()

	// Chosen from the activity ID rather than at random, so replaying the same
	// activity produces the same title and a test can assert on it.
	return pool[int(activity.ID%int64(len(pool)))]
}
