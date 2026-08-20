//go:build smoke

package naming

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"
)

// A real call to Vertex, behind a build tag so it never runs in CI.
//
//	go test -tags smoke ./internal/naming/ -run TestLiveVertex -v
func TestLiveVertex(t *testing.T) {
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT unset")
	}

	location := os.Getenv("VERTEX_LOCATION")
	if location == "" {
		location = "europe-west3"
	}

	client, _, err := htransport.NewClient(context.Background(),
		option.WithScopes("https://www.googleapis.com/auth/cloud-platform"))
	if err != nil {
		t.Fatalf("credentialed client: %v", err)
	}

	provider := &Vertex{Client: client, ProjectID: project, Location: location}
	t.Logf("provider: %s @ %s", provider.Name(), location)

	prompt := BuildPrompt(
		Ride{SportType: "GravelRide", DistanceKm: 67.6, MovingTimeMinutes: 181,
			ElevationGainMeters: 540, StartLocal: time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC),
			GearName: "Pink Panther", Places: []string{"Musterdorf", "Musterbach"},
			Region: "Musterregion", Country: "Testland"},
		Context{RecentTitles: []string{"Musterrunde"}, Examples: SyntheticExamples()},
	)

	raw, err := provider.Complete(t.Context(), prompt)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	t.Logf("raw response: %q", raw)

	title, err := NewValidator(DefaultBannedWords()).ParseAndValidate(raw)
	if err != nil {
		t.Fatalf("the model's response did not survive validation: %v", err)
	}

	t.Logf("VALIDATED TITLE: %q (%s)", title.Text, title.Language)
}
