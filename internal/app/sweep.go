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
	//
	// Wrapped, because cfg.AthleteID is legitimately zero: see [boundTokens].
	tokens := strava.NewStoredTokenSource(oauth, boundTokens{dataStore}, cfg.AthleteID)

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

	deps, err := sweepDeps(ctx, cfg, dataStore, client, logger)
	if err != nil {
		return nil, err
	}

	proc, err := processor.New(deps)
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
		"llm", deps.Provider.Name(),
		"writes_enabled", cfg.WritesEnabled,
		"banned_words", len(bannedWords(cfg)),
		"log_prompt", cfg.LogPrompt)

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
// secret for it. The two keyed providers share LLM_API_KEY; which of them the
// key belongs to is whatever LLM_PROVIDER says.
func buildProvider(ctx context.Context, cfg config.Config) (naming.Provider, error) {
	switch cfg.LLM.Provider {
	case config.ProviderAnthropic:
		return &naming.Anthropic{APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model}, nil
	case config.ProviderOpenRouter:
		return &naming.OpenRouter{
			APIKey:  cfg.LLM.APIKey,
			Model:   cfg.LLM.Model,
			BaseURL: cfg.LLM.BaseURL,
		}, nil
	case config.ProviderVertex:
		// Below: the keyless default, and the only one that needs the
		// ambient credentials.
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
// sweepDeps resolves everything the processor is given.
//
// Separated from [buildSweep] so there is one place where configuration
// becomes the processor's dependencies, and so a test can drive the real
// loader through the real wiring and inspect what a deployment would actually
// get. Every test that hand-builds a config.Config agrees with itself by
// construction and would hide a default that never reaches production — which
// is exactly how the banned-word list stayed empty in the deployed service
// while the field comment said it defaulted.
func sweepDeps(
	ctx context.Context, cfg config.Config, dataStore boundStore,
	client *strava.Client, logger *slog.Logger,
) (processor.Deps, error) {
	geographer, err := buildGeographer(cfg, dataStore, logger)
	if err != nil {
		return processor.Deps{}, err
	}

	provider, err := buildProvider(ctx, cfg)
	if err != nil {
		return processor.Deps{}, err
	}

	rules, err := classifierConfig(cfg)
	if err != nil {
		return processor.Deps{}, err
	}

	return processor.Deps{
		Store:         dataStore,
		Activities:    client,
		Geo:           geographer,
		Provider:      provider,
		Classifier:    rules,
		Validator:     naming.NewValidator(bannedWords(cfg)),
		WritesEnabled: cfg.WritesEnabled,
		LogPrompt:     cfg.LogPrompt,
		Logger:        logger,
	}, nil
}

// bannedWords resolves the list the validator rejects titles against.
//
// The shipped list applies when the environment names none, the same way an
// unset machine-title pattern falls back to the shipped set in
// [classifierConfig]. Without this the production service ran with no banned
// words at all: the field's own comment said "empty means the shipped list",
// and nothing anywhere made that true.
//
// A configured list *replaces* the shipped one rather than adding to it, which
// is what makes it configuration. The consequence is that there is no way to
// spell "no banned words" in the environment — set-but-empty and unset are the
// same string — and that is the right trade: the failure mode of the missing
// default was silent, and the failure mode of an unwanted default is a title
// the athlete can see was refused.
func bannedWords(cfg config.Config) []string {
	if len(cfg.LLM.BannedWords) > 0 {
		return cfg.LLM.BannedWords
	}

	return naming.DefaultBannedWords()
}

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
