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
	ctx context.Context, activity *strava.Activity,
	decision classifier.Decision, logger *slog.Logger,
) (string, error) {
	switch decision.Action {
	case classifier.ActionCommuteTemplate:
		return commuteTitle(decision, p.deps.Classifier), nil

	case classifier.ActionErrandTemplate:
		return errandTitle(activity), nil

	case classifier.ActionLLM, classifier.ActionLLMIndoor:
		return p.llmTitle(ctx, activity, decision, logger)

	default:
		return "", fmt.Errorf("unhandled action %v", decision.Action)
	}
}

// llmTitle gathers, prompts, calls and validates.
func (p *Processor) llmTitle(
	ctx context.Context, activity *strava.Activity,
	decision classifier.Decision, logger *slog.Logger,
) (string, error) {
	if p.deps.Provider == nil {
		return "", errors.New("no LLM provider configured")
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

	// An indoor ride is named from effort and season only. Watopia is not in
	// the Solomon Islands, and geocoding a fictional coordinate would produce
	// a confidently wrong place name.
	if decision.Action == classifier.ActionLLM {
		summary, err := p.geography(ctx, activity, logger)
		if err != nil {
			return "", err
		}

		ride.Places = summary.Names()
		ride.Region = summary.Region
		ride.Country = summary.Country
	}

	promptContext := naming.Context{Examples: naming.SyntheticExamples()}

	prompt := naming.BuildPrompt(ride, promptContext)

	raw, err := p.deps.Provider.Complete(ctx, prompt)
	if err != nil {
		return "", fmt.Errorf("llm %s: %w", p.deps.Provider.Name(), err)
	}

	title, err := p.deps.Validator.ParseAndValidate(raw)
	if err != nil {
		return "", fmt.Errorf("llm %s returned an unusable title: %w", p.deps.Provider.Name(), err)
	}

	logger.Info("named",
		"title", logsafe.String(title.Text),
		"language", string(title.Language),
		"places", len(ride.Places),
		"facts", len(ride.Facts))

	return title.Text, nil
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
