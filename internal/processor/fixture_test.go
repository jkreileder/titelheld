package processor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/geo"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// staticToken is a token provider that needs no OAuth round trip.
type staticToken struct{}

func (staticToken) AccessToken(_ context.Context) (string, error) { return "test-access-token", nil }

// The whole pipeline, from Strava's wire format to the PUT it sends.
//
// The activity arrives as JSON over HTTP and is decoded by the real client,
// rather than being built as a Go value in the test. That is the point: a
// hand-built struct proves the pipeline works on data shaped the way the test
// author imagined, and cannot catch a wrong JSON tag, a field nested one level
// off, or the start_date_local trap — a local wall-clock time that carries a
// "Z" suffix, which reads as UTC and would move an evening ride into the next
// morning.
//
// The fixture is synthetic. This is a public repository for a service that
// handles one person's GPS data, so the coordinates are invented and the
// identifiers are fake.
func TestTheWholePipelineFromStravasWireFormat(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/gravel_ride.json")
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}

	var (
		mu        sync.Mutex
		puts      []map[string]string
		getCalls  int
		gearCalls int
	)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		if strings.HasPrefix(r.URL.Path, "/gear/") {
			gearCalls++

			_, _ = w.Write([]byte(`{"id":"b0000000","name":"Pink Panther"}`))

			return
		}

		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)

			values, err := parseForm(string(body))
			if err != nil {
				t.Errorf("the PUT body is not form-encoded: %v", err)
			}

			puts = append(puts, values)
		} else {
			getCalls++
		}

		_, _ = w.Write(fixture)
	}))
	defer api.Close()

	client, err := strava.NewClient(strava.ClientConfig{
		Tokens:    staticToken{},
		WriteMode: strava.WriteModeEnabled,
		BaseURL:   api.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	memory := store.NewMemory()
	provider := &fakeProvider{}

	proc, err := New(Deps{
		Store:      memory,
		Activities: client,
		Geo: fakeGeo{summary: geo.Summary{
			Region:  "Musterregion",
			Country: "Musterland",
		}},
		Provider:      provider,
		Classifier:    classifier.DefaultConfig(),
		Validator:     naming.NewValidator(naming.DefaultBannedWords()),
		WritesEnabled: true,
		Logger:        quiet(),
		Now:           func() time.Time { return time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := memory.Enqueue(t.Context(), store.Pending{
		AthleteID: 42424242, ActivityID: 90000000001, Aspect: "create",
		EnqueuedAt:   time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC),
		ProcessAfter: time.Date(2026, 8, 15, 15, 10, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	result, err := proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Named != 1 {
		t.Fatalf("result %+v, want one activity named", result)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(puts) != 1 {
		t.Fatalf("%d PUTs, want 1: %+v", len(puts), puts)
	}

	// The title the mocked model returned, validated and sent on.
	if got := puts[0]["name"]; got != "Musterrunde am Musterbach" {
		t.Errorf("name is %q, want the validated title", got)
	}

	// The attribution went in front of the third-party content, which came
	// back unchanged.
	description := puts[0]["description"]
	if !strings.HasPrefix(description, naming.Attribution+"\n\n") {
		t.Errorf("the description does not start with the attribution: %q", description)
	}

	if !strings.Contains(description, "Vehicles: 87") {
		t.Errorf("third-party content did not survive the prepend: %q", description)
	}

	// The local start time survived the "Z" suffix. 14:30 on a Saturday is
	// what the fixture says, and it is what the prompt must have been built
	// from — an evening in UTC would have been a different day and a
	// different part of the day.
	var decoded strava.Activity
	if err := json.Unmarshal(fixture, &decoded); err != nil {
		t.Fatalf("decode the fixture: %v", err)
	}

	if hour, weekday := decoded.StartDateLocal.Hour(), decoded.StartDateLocal.Weekday(); hour != 14 ||
		weekday != time.Saturday {
		t.Errorf("start_date_local reads as %s %02d:00, want Saturday 14:00", weekday, hour)
	}

	// The parsed description facts, not the raw text, are what the prompt got.
	facts := naming.ParseFacts(decoded.Description)
	if len(facts) == 0 {
		t.Fatal("no facts were parsed from the fixture's description")
	}

	// The labels are the parser's own, not the tools' wording, and the raw
	// description never reaches the prompt.
	labels := make(map[string]string, len(facts))
	for _, fact := range facts {
		labels[fact.Label] = fact.Value
	}

	for label, want := range map[string]string{
		"Difficulty":       "Difficult",
		"Focus":            "Breakaway Specialist",
		"Headwind":         "42%",
		"Vehicles passing": "87",
	} {
		if labels[label] != want {
			t.Errorf("fact %q = %q, want %q", label, labels[label], want)
		}
	}

	if provider.calls != 1 {
		t.Errorf("the model was called %d times, want 1", provider.calls)
	}

	// Two activity GETs for a named activity, and exactly two: the
	// classifier's fetch, and the re-read immediately before the write that
	// merges the description. Pinned because Strava allows 100 requests per
	// 15 minutes and an accidental extra fetch would not otherwise show up.
	if getCalls != 2 {
		t.Errorf("%d activity GETs for one named activity, want 2", getCalls)
	}

	// One gear lookup, for the franchise match. The name is cached for the
	// life of the process, so a second activity on the same bike costs
	// nothing — a bike's name changes about never.
	if gearCalls != 1 {
		t.Errorf("%d gear lookups, want 1", gearCalls)
	}

	// The first naming has no history to derive examples from, so nothing was
	// re-read for few-shots — which is exactly why no third activity GET
	// appears above. That is what the synthetic set is for.
}

// parseForm decodes an application/x-www-form-urlencoded body.
func parseForm(body string) (map[string]string, error) {
	values, err := url.ParseQuery(body)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(values))
	for key := range values {
		out[key] = values.Get(key)
	}

	return out, nil
}
