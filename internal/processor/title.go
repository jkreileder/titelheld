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
			Text:     commuteTitle(decision, p.deps.Classifier),
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

	// Franchise is the series this title came from, if any.
	Franchise string

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

	prompt := naming.BuildPrompt(ride, gathered.Context)

	raw, err := p.deps.Provider.Complete(ctx, prompt)
	if err != nil {
		return titled{}, fmt.Errorf("llm %s: %w", p.deps.Provider.Name(), err)
	}

	title, err := p.deps.Validator.ParseAndValidate(raw)
	if err != nil {
		return titled{}, fmt.Errorf(
			"llm %s returned an unusable title: %w", p.deps.Provider.Name(), err)
	}

	logger.Info("named",
		"title", logsafe.String(title.Text),
		"language", string(title.Language),
		"places", len(ride.Places),
		"facts", len(ride.Facts),
		"recent_titles", len(gathered.Context.RecentTitles),
		"examples", len(gathered.Context.Examples),
		"franchise_offered", gathered.Franchise != "")

	// The series that gets advanced is the one the prompt was shown, carried
	// out of the single lookup that resolved it rather than resolved again
	// here.
	//
	// Recorded as used whenever an entry was offered, without checking that
	// the title resembles it. The model is invited to adapt the wording, so a
	// title that used the entry and one that ignored it are not reliably
	// distinguishable — and the spec's rule is that the order may not be
	// skipped, which makes advancing the safer error: a franchise that
	// advances on a title that ignored it loses one entry, where one that
	// does not advance offers the same entry until a model happens to use it
	// verbatim.
	return titled{
		Text:      title.Text,
		Language:  title.Language,
		Source:    store.SourceLLM,
		Franchise: gathered.Franchise,
	}, nil
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

// commuteTitle is the safety net for a commute ActivityFix did not title.
func commuteTitle(decision classifier.Decision, cfg classifier.Config) string {
	if decision.Direction == classifier.DirectionToHome {
		if cfg.ToHomeTitle != "" {
			return cfg.ToHomeTitle
		}

		return "Nach Hause"
	}

	if cfg.ToWorkTitle != "" {
		return cfg.ToWorkTitle
	}

	return "Zur Arbeit"
}

// errandTitle picks a deliberately boring German name.
//
// No LLM and no geocoding: an errand's destination is exactly the thing a
// title must not reveal, and the spec's privacy rule is that a title never
// comes from a reverse-geocoded point of interest. The pool is small on
// purpose — these titles are meant to be unremarkable.
func errandTitle(activity *strava.Activity) string {
	pool := []string{"Besorgungen", "In die Stadt", "Stadtrunde"}

	// Chosen from the activity ID rather than at random, so replaying the same
	// activity produces the same title and a test can assert on it.
	return pool[int(activity.ID%int64(len(pool)))]
}
