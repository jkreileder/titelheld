package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/processor"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// memoryStore is the in-memory store plus the bootstrap lookup, so it
// satisfies boundStore.
type memoryStore struct{ *store.Memory }

func (memoryStore) AnyToken(_ context.Context) (strava.Token, error) {
	return strava.Token{}, errors.New("no token")
}

// An unconfigured sweep builds nothing at all.
//
// Not just no route: no Vertex client either, which is what lets the service
// run locally with no Google credentials, and lets the first Terraform apply
// produce a service that starts before the URL its audience is built from
// exists.
func TestNoSweepConfigurationBuildsNothing(t *testing.T) {
	t.Parallel()

	handler, err := buildSweep(
		t.Context(), config.Config{}, memoryStore{store.NewMemory()},
		&strava.OAuth{}, quiet())
	if err != nil {
		t.Fatalf("buildSweep: %v", err)
	}

	if handler != nil {
		t.Error("a sweep handler was built with no sweep configured")
	}
}

// A configured Anthropic provider needs no cloud credentials, so the whole
// pipeline can be assembled in a test.
func TestAFullyConfiguredSweepBuildsAHandler(t *testing.T) {
	t.Parallel()

	handler, err := buildSweep(
		t.Context(), anthropicConfig(), memoryStore{store.NewMemory()},
		&strava.OAuth{}, quiet())
	if err != nil {
		t.Fatalf("buildSweep: %v", err)
	}

	if handler == nil {
		t.Error("no sweep handler was built from a complete configuration")
	}
}

// A bad machine-title pattern stops the service starting.
//
// The alternative is a service that starts, looks healthy, and fails on the
// first activity it is asked to classify — hours later, in a sweep, where the
// only symptom is that nothing gets named.
func TestABadMachineTitlePatternIsFatalAtStartup(t *testing.T) {
	t.Parallel()

	cfg := anthropicConfig()
	cfg.LLM.MachineTitlePatterns = []string{"("}

	handler, err := buildSweep(
		t.Context(), cfg, memoryStore{store.NewMemory()}, &strava.OAuth{}, quiet())
	if err == nil {
		t.Fatal("an uncompilable machine-title pattern was accepted")
	}

	if handler != nil {
		t.Error("a handler was built alongside the error")
	}

	if !strings.Contains(err.Error(), "machine titles") {
		t.Errorf("error %q does not say what failed to compile", err)
	}
}

// Configured machine titles replace the shipped ones; none means the defaults.
//
// Observed through Classify rather than through the table, because what
// matters is whether an activity carrying that title may be renamed — the
// table is just how the answer is stored.
func TestClassifierConfigTakesTheConfiguredMachineTitles(t *testing.T) {
	t.Parallel()

	// A sport ride, so the tier is never the thing that skips it.
	ride := func(name string) classifier.Activity {
		return classifier.Activity{
			Name:              name,
			SportType:         "Ride",
			DistanceMeters:    67638,
			MovingTimeSeconds: 10876,
		}
	}

	defaults, err := classifierConfig(config.Config{})
	if err != nil {
		t.Fatalf("classifierConfig: %v", err)
	}

	// A Strava default title is renamable under any configuration.
	if got := classifier.Classify(ride("Afternoon Ride"), defaults); got.Action == classifier.ActionSkip {
		t.Errorf("a Strava default title was skipped: %s", got.Reason)
	}

	cfg := config.Config{}
	cfg.LLM.MachineTitlePatterns = []string{"Xert Workout"}

	custom, err := classifierConfig(cfg)
	if err != nil {
		t.Fatalf("classifierConfig: %v", err)
	}

	if got := classifier.Classify(ride("Xert Workout"), custom); got.Action == classifier.ActionSkip {
		t.Errorf("the configured machine title was skipped: %s", got.Reason)
	}

	// A human's title is still off limits, whatever else is configured.
	if got := classifier.Classify(ride("Epic day in the Alps"), custom); got.Action != classifier.ActionSkip {
		t.Errorf("a human-written title was renamable: %s", got.Reason)
	}
}

// The dry-run flag reaches the Strava client, not just the processor.
//
// Both refuse a write independently, and this is the wiring that has to agree
// with itself for that to mean anything.
func TestWritesStayDisabledUnlessConfigOn(t *testing.T) {
	t.Parallel()

	if _, err := buildSweep(
		t.Context(), anthropicConfig(), memoryStore{store.NewMemory()},
		&strava.OAuth{}, quiet()); err != nil {
		t.Fatalf("buildSweep: %v", err)
	}

	// A config whose WritesEnabled is false — the zero value — must not
	// produce a client in write mode.
	client, err := strava.NewClient(strava.ClientConfig{
		Tokens: strava.NewStoredTokenSource(&strava.OAuth{}, store.NewMemory(), 1),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if client.WriteMode() != strava.WriteModeDryRun {
		t.Error("a zero-valued configuration produced a client that can write")
	}
}

// anthropicConfig is a complete sweep configuration that needs no cloud
// credentials to assemble.
func anthropicConfig() config.Config {
	cfg := config.Config{
		NominatimUserAgent: "titelheld-test/1.0 (+https://namer.example.invalid)",
		Sweep: config.SweepConfig{
			Path:           "/sweep/AbCdEf0123456789AbCdEf0123456789",
			Audience:       "https://namer.example.invalid",
			ServiceAccount: "titelheld-scheduler@example.invalid",
		},
	}

	cfg.LLM.Provider = config.ProviderAnthropic
	cfg.LLM.APIKey = "test-key-not-a-real-one"

	return cfg
}

// deployedEnv is the environment a Cloud Run revision actually has, minus
// everything optional.
//
// Required variables only, plus the two that pick a provider needing no cloud
// credentials — Vertex is the production default, but reaching it from a test
// would mean depending on ambient Google credentials, and the provider is not
// what these assertions are about.
//
// Nothing else is set. That is the point: every other test in this package
// hands buildSweep a config.Config it built itself, which agrees with itself
// by construction and cannot notice a default that never reaches production.
func deployedEnv(overrides map[string]string) func(string) string {
	values := map[string]string{
		"STRAVA_CLIENT_ID":     "12345",
		"STRAVA_CLIENT_SECRET": "test-client-secret",
		"STRAVA_VERIFY_TOKEN":  "test-verify-token",
		"BASE_URL":             "https://namer.example.invalid",
		"WEBHOOK_PATH_SECRET":  "s3cr3t-segment",
		"MAX_INSTANCES":        "1",
		"LLM_PROVIDER":         "anthropic",
		"LLM_API_KEY":          "test-key",
	}

	for key, value := range overrides {
		values[key] = value
	}

	return func(key string) string { return values[key] }
}

// depsFromEnv runs the real loader through the real wiring.
func depsFromEnv(t *testing.T, overrides map[string]string) processor.Deps {
	t.Helper()

	cfg, err := config.Load(deployedEnv(overrides))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	client, err := strava.NewClient(strava.ClientConfig{
		Tokens: strava.NewStoredTokenSource(&strava.OAuth{}, store.NewMemory(), cfg.AthleteID),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	deps, err := sweepDeps(t.Context(), cfg, memoryStore{store.NewMemory()}, client, quiet())
	if err != nil {
		t.Fatalf("sweepDeps: %v", err)
	}

	return deps
}

// title is a model response the validator would otherwise accept.
func title(text string) string {
	return `{"title":"` + text + `","language":"en"}`
}

// An environment that sets nothing optional still bans the shipped words.
//
// This is the regression that shipped: BANNED_WORDS unset produced an empty
// list, `naming.NewValidator` was handed it, and the deployed service rejected
// nothing — while the configuration field's own comment said "empty means the
// shipped list". Every existing test built its own config.Config, so every
// existing test agreed the words were banned.
func TestAnEmptyEnvironmentStillBansTheShippedWords(t *testing.T) {
	t.Parallel()

	deps := depsFromEnv(t, nil)

	for _, word := range naming.DefaultBannedWords() {
		if _, err := deps.Validator.ParseAndValidate(title(word + " ride")); !errors.Is(err, naming.ErrTitleBanned) {
			t.Errorf("a title containing %q was accepted with no BANNED_WORDS set: %v", word, err)
		}
	}
}

// A configured list replaces the shipped one rather than adding to it.
//
// The half of the contract a fallback is easy to get wrong: an append would
// pass the test above and quietly keep banning words the athlete removed.
func TestConfiguredBannedWordsReplaceTheShippedList(t *testing.T) {
	t.Parallel()

	deps := depsFromEnv(t, map[string]string{"BANNED_WORDS": "Musterwort"})

	if _, err := deps.Validator.ParseAndValidate(title("Musterwort am Bach")); !errors.Is(err, naming.ErrTitleBanned) {
		t.Errorf("the configured banned word was not rejected: %v", err)
	}

	if _, err := deps.Validator.ParseAndValidate(title("Epic ride")); err != nil {
		t.Errorf("a shipped word was still banned after configuring a list: %v", err)
	}
}

// The rest of the posture a deployment gets from an environment that sets
// nothing optional.
//
// One test rather than five, because the thing under test is the same thing:
// what a Cloud Run revision is actually wired with. Each assertion is a
// default that exists in code and has to survive the trip through
// config.Load — the class of regression the banned-word list belongs to.
func TestTheDefaultPostureOfADeployedRevision(t *testing.T) {
	t.Parallel()

	deps := depsFromEnv(t, nil)

	// Writes are off unless DRY_RUN says otherwise. The zero value is the safe
	// one, and an empty environment must land on it.
	if deps.WritesEnabled {
		t.Error("an environment with no DRY_RUN set produced a processor that can write")
	}

	// Attribution is on by default, so the field that disables it is false.
	if deps.DisableAttribution {
		t.Error("attribution was disabled by default")
	}

	// Prompt logging follows the dry run: the observation window is exactly
	// when the whole prompt is the evidence being judged, and nobody should
	// have to remember to ask for it.
	if !deps.LogPrompt {
		t.Error("prompts are not logged in a dry-run deployment")
	}

	// Franchises unset means "read the athlete's document, falling back to the
	// shipped profile" — an empty non-nil slice would mean "this athlete has
	// none" and silently turn the feature off.
	if deps.Franchises != nil {
		t.Errorf("franchises were pinned at startup: %v", deps.Franchises)
	}

	// A sport ride at a Strava default title is renamable; Xert's machine
	// title is renamable; a human's title is not. These are the shipped
	// classifier defaults, reached with no MACHINE_TITLE_PATTERNS set.
	ride := func(name string) classifier.Activity {
		return classifier.Activity{
			Name: name, SportType: "Ride",
			DistanceMeters: 67638, MovingTimeSeconds: 10876,
		}
	}

	for name, wantSkip := range map[string]bool{
		"Afternoon Ride": false,
		"Difficult Mixed Breakaway Specialist Ride": false,
		"Musterrunde am Musterbach":                 true,
	} {
		got := classifier.Classify(ride(name), deps.Classifier)
		if skipped := got.Action == classifier.ActionSkip; skipped != wantSkip {
			t.Errorf("Classify(%q) skip = %v, want %v (%s)", name, skipped, wantSkip, got.Reason)
		}
	}

	// A Zwift ride keeps its title: the shipped zwift_mode is `keep`, and
	// `llm_indoor` has no configuration path at all.
	virtual := classifier.Activity{
		Name: "Afternoon Ride", SportType: "VirtualRide", Trainer: true,
		DistanceMeters: 30000, MovingTimeSeconds: 3600,
	}

	if got := classifier.Classify(virtual, deps.Classifier); got.Action != classifier.ActionSkip {
		t.Errorf("a Zwift ride was named rather than kept: %v (%s)", got.Action, got.Reason)
	}

	// And the pieces that make a pipeline are all there, so the assertions
	// above are about a processor that could actually run.
	if deps.Store == nil || deps.Activities == nil || deps.Geo == nil || deps.Provider == nil {
		t.Errorf("an incomplete pipeline: store=%v activities=%v geo=%v provider=%v",
			deps.Store != nil, deps.Activities != nil, deps.Geo != nil, deps.Provider != nil)
	}
}

// Prompt logging follows the writes flag, and LOG_PROMPT overrides either way.
//
// Asserted through config.Load and sweepDeps rather than on a hand-built
// config: the question is what a deployment gets, and the setting is derived
// from another one, which is the kind of wiring that silently stops working.
func TestPromptLoggingFollowsTheDryRunAndItsOverride(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "dry run, the deployed default", env: nil, want: true},
		{name: "writes enabled", env: map[string]string{"DRY_RUN": "0"}, want: false},
		{
			name: "writes enabled, logging asked for",
			env:  map[string]string{"DRY_RUN": "0", "LOG_PROMPT": "1"},
			want: true,
		},
		{
			name: "dry run, logging refused",
			env:  map[string]string{"LOG_PROMPT": "0"},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := depsFromEnv(t, tc.env).LogPrompt; got != tc.want {
				t.Errorf("LogPrompt = %v, want %v", got, tc.want)
			}
		})
	}
}

// Provider dispatch, through the real loader and the real wiring.
//
// With LLM_PROVIDER unset the Vertex branch is taken. Reaching Vertex needs
// ambient Google credentials, which a test machine may or may not have, so
// two outcomes are accepted and both identify that branch uniquely: a
// *naming.Vertex, or the "build Vertex credentials" error — neither of which
// the Anthropic or OpenRouter constructors can produce. The control below is
// what keeps this from passing either way: the same wiring with openrouter
// named returns an *naming.OpenRouter carrying the configured key, model and
// base URL.
func TestProviderDispatchIsVertexUnlessAsked(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(deployedEnv(map[string]string{"LLM_PROVIDER": "", "LLM_API_KEY": ""}))
	if err != nil {
		t.Fatalf("config.Load with the provider unset: %v", err)
	}

	if cfg.LLM.Provider != config.ProviderVertex {
		t.Fatalf("provider = %q, want %q", cfg.LLM.Provider, config.ProviderVertex)
	}

	provider, err := buildProvider(t.Context(), cfg)

	switch {
	case err != nil && strings.Contains(err.Error(), "build Vertex credentials"):
		// No ambient credentials here; the Vertex branch was still the one
		// taken, which is what this asserts.
	case err != nil:
		t.Fatalf("buildProvider: %v", err)
	default:
		if _, ok := provider.(*naming.Vertex); !ok {
			t.Fatalf("provider = %T, want *naming.Vertex", provider)
		}
	}

	// The control.
	cfg, err = config.Load(deployedEnv(map[string]string{
		"LLM_PROVIDER": "openrouter", "LLM_API_KEY": "test-key",
		"LLM_MODEL": "google/gemini-3.7-flash", "LLM_BASE_URL": "https://gateway.example/v1",
	}))
	if err != nil {
		t.Fatalf("config.Load with openrouter: %v", err)
	}

	provider, err = buildProvider(t.Context(), cfg)
	if err != nil {
		t.Fatalf("buildProvider with openrouter: %v", err)
	}

	openrouter, ok := provider.(*naming.OpenRouter)
	if !ok {
		t.Fatalf("provider = %T, want *naming.OpenRouter", provider)
	}

	if openrouter.APIKey != "test-key" || openrouter.Model != "google/gemini-3.7-flash" ||
		openrouter.BaseURL != "https://gateway.example/v1" {
		t.Errorf("openrouter = %+v", *openrouter)
	}

	if name := openrouter.Name(); name != "openrouter/google/gemini-3.7-flash" {
		t.Errorf("Name() = %q", name)
	}
}
