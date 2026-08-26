//go:build smoke

package naming

import (
	"os"
	"testing"
)

// A real call through OpenRouter, behind a build tag so it never runs in CI.
//
//	LLM_API_KEY=… LLM_MODEL=… go test -tags smoke ./internal/naming/ -run TestLiveOpenRouter -v
//
// The key is read from the environment for this one process and stored
// nowhere; the model defaults to the shipped one.
func TestLiveOpenRouter(t *testing.T) {
	key := os.Getenv("LLM_API_KEY")
	if key == "" {
		t.Skip("LLM_API_KEY unset")
	}

	provider := &OpenRouter{APIKey: key, Model: os.Getenv("LLM_MODEL"), BaseURL: os.Getenv("LLM_BASE_URL")}

	raw, err := provider.Complete(t.Context(), BuildPrompt(
		Ride{SportType: "GravelRide", DistanceKm: 68, MovingTimeMinutes: 181, ElevationGainMeters: 540},
		Context{Examples: SyntheticExamples()},
	))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	title, err := NewValidator(DefaultBannedWords()).ParseAndValidate(raw)
	if err != nil {
		t.Fatalf("ParseAndValidate(%q): %v", raw, err)
	}

	t.Logf("%s -> %q (%s)", provider.Name(), title.Text, title.Language)
}
