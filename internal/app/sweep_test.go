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
