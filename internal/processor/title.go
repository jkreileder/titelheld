package processor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/geo"
	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/naming"
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
		return titled{Text: commuteTitle(decision, p.deps.Classifier), Language: naming.German}, nil

	case classifier.ActionErrandTemplate:
		return titled{Text: errandTitle(activity), Language: naming.German}, nil

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

	// Fingerprint is the route, if it had one. Recorded with the write, so a
	// naming that fails does not count a ride that never got a title.
	Fingerprint string
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
	// a confidently wrong place name. It has no route to recognize either.
	var fingerprint string

	if decision.Action == classifier.ActionLLM {
		summary, err := p.geography(ctx, activity, logger)
		if err != nil {
			return titled{}, err
		}

		ride.Places = summary.Names()
		ride.Region = summary.Region
		ride.Country = summary.Country

		fingerprint = p.fingerprint(activity, logger)
		ride.RepeatOfDate, ride.RepeatCount = p.routeHistory(ctx, athleteID, fingerprint, logger)
	}

	promptContext, err := p.promptContext(ctx, athleteID, ride, logger)
	if err != nil {
		return titled{}, err
	}

	prompt := naming.BuildPrompt(ride, promptContext)

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
		"recent_titles", len(promptContext.RecentTitles),
		"examples", len(promptContext.Examples),
		"franchise_offered", promptContext.FranchiseNext != "",
		"route_repeat", ride.RepeatCount)

	result := titled{Text: title.Text, Language: title.Language, Fingerprint: fingerprint}

	// The franchise is recorded as used only if this title actually came from
	// it. The model may adapt the wording, so an exact match is too strict and
	// no check at all would advance the series on a title that ignored it.
	if promptContext.FranchiseNext != "" {
		if franchise, ok := naming.FranchiseFor(
			p.deps.Franchises, ride.SportType, ride.GearName); ok {
			result.Franchise = franchise.Name
		}
	}

	return result, nil
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

	p.gearMu.Lock()
	p.gear[gearID] = gear.Name
	p.gearMu.Unlock()

	return gear.Name
}

// fingerprint reduces the route to something the store can count.
//
// A malformed polyline is logged and dropped rather than failing the naming:
// the ride is still nameable, it just cannot be recognized as one ridden
// before.
func (p *Processor) fingerprint(activity *strava.Activity, logger *slog.Logger) string {
	value, err := geo.Fingerprint(activity.Map.SummaryPolyline)
	if err != nil {
		logger.Warn("could not fingerprint the route; naming without route history",
			"error", err)

		return ""
	}

	return value
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
