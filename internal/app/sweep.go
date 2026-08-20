package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/geo"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/processor"
	"github.com/jkreileder/titelheld/internal/strava"
	"github.com/jkreileder/titelheld/internal/sweep"
)

// vertexScope is the OAuth scope Vertex AI calls are made with.
const vertexScope = "https://www.googleapis.com/auth/cloud-platform"

// buildSweep assembles the naming pipeline and the route that triggers it.
//
// It returns (nil, nil) when the sweep is not configured, which is the local
// case and the first-apply case. Nothing downstream of this is built then
// either: no Vertex client, so a local run needs no Google credentials, and no
// Strava client bound to an athlete who may not have authorized yet.
//
// The athlete's tiers, geofences and franchises are configuration rather than
// code, and the store that holds them is not built yet. Until it is, this is
// the shipped default set, and the wiring below is where the per-athlete load
// will go.
func buildSweep(
	ctx context.Context, cfg config.Config, dataStore boundStore,
	oauth *strava.OAuth, logger *slog.Logger,
) (http.Handler, error) {
	if !cfg.Sweep.Enabled() {
		logger.Info("no sweep route: the sweep is not configured")

		return nil, nil
	}

	// The token source refreshes on expiry and persists the rotated refresh
	// token, so a sweep hours after the last webhook still has a live token.
	tokens := strava.NewStoredTokenSource(oauth, dataStore, cfg.AthleteID)

	// The write mode is derived from the same flag the processor reads, and
	// the client refuses a write on its own if it disagrees. Two independent
	// refusals for one decision is deliberate: this is the only place in the
	// service that can change somebody's data.
	writeMode := strava.WriteModeDryRun
	if cfg.WritesEnabled {
		writeMode = strava.WriteModeEnabled
	}

	client, err := strava.NewClient(strava.ClientConfig{
		Tokens:    tokens,
		WriteMode: writeMode,
	})
	if err != nil {
		return nil, fmt.Errorf("build the Strava client: %w", err)
	}

	geographer, err := buildGeographer(cfg, dataStore, logger)
	if err != nil {
		return nil, err
	}

	provider, err := buildProvider(ctx, cfg)
	if err != nil {
		return nil, err
	}

	rules, err := classifierConfig(cfg)
	if err != nil {
		return nil, err
	}

	proc, err := processor.New(processor.Deps{
		Store:         dataStore,
		Activities:    client,
		Geo:           geographer,
		Provider:      provider,
		Classifier:    rules,
		Validator:     naming.NewValidator(cfg.LLM.BannedWords),
		WritesEnabled: cfg.WritesEnabled,
		Logger:        logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build the processor: %w", err)
	}

	handler, err := sweep.New(sweep.Deps{
		Processor:      proc,
		Audience:       cfg.Sweep.Audience,
		ServiceAccount: cfg.Sweep.ServiceAccount,
		Logger:         logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build the sweep handler: %w", err)
	}

	// The path itself never reaches a log. Anyone who can read the logs would
	// then be able to trigger a sweep, or would if the OIDC check ever
	// regressed, and the two defenses are meant to be independent.
	logger.Info("sweep route mounted",
		"llm", provider.Name(),
		"writes_enabled", cfg.WritesEnabled)

	return handler, nil
}

// buildGeographer wires Nominatim behind the shared geocode cache.
func buildGeographer(
	cfg config.Config, dataStore boundStore, logger *slog.Logger,
) (*geo.Describer, error) {
	nominatim, err := geo.NewNominatim(geo.NominatimConfig{UserAgent: cfg.NominatimUserAgent})
	if err != nil {
		return nil, fmt.Errorf("build the geocoder: %w", err)
	}

	describer, err := geo.NewDescriber(nominatim, dataStore, logger)
	if err != nil {
		return nil, fmt.Errorf("build the describer: %w", err)
	}

	return describer, nil
}

// buildProvider selects the LLM backend.
//
// Vertex is the default and needs no key: the runtime service account's
// ambient credentials are the authentication, which is why there is no sixth
// secret for it.
func buildProvider(ctx context.Context, cfg config.Config) (naming.Provider, error) {
	if cfg.LLM.Provider == config.ProviderAnthropic {
		return &naming.Anthropic{APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model}, nil
	}

	client, _, err := htransport.NewClient(ctx, option.WithScopes(vertexScope))
	if err != nil {
		return nil, fmt.Errorf("build Vertex credentials: %w", err)
	}

	return &naming.Vertex{
		Client:    client,
		ProjectID: cfg.LLM.VertexProject,
		Location:  cfg.LLM.VertexLocation,
		Model:     cfg.LLM.Model,
	}, nil
}

// classifierConfig is the athlete's naming rules.
//
// The shipped defaults, plus the machine titles the deployment configured.
// When the per-athlete configuration store exists, this is the one function
// that changes.
func classifierConfig(cfg config.Config) (classifier.Config, error) {
	rules := classifier.DefaultConfig()

	if len(cfg.LLM.MachineTitlePatterns) == 0 {
		return rules, nil
	}

	// The patterns are anchored and compiled here rather than at first use, so
	// a bad one stops the service from starting instead of failing on the
	// first activity it is asked to classify.
	titles, err := classifier.NewMachineTitles(cfg.LLM.MachineTitlePatterns)
	if err != nil {
		return classifier.Config{}, fmt.Errorf("compile the machine titles: %w", err)
	}

	rules.MachineTitles = titles

	return rules, nil
}
