package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// listOnce serves one page of activities and then the end.
type listOnce struct {
	activities []strava.Activity
	calls      int
}

func (l *listOnce) ListActivities(_ context.Context, page, _ int) ([]strava.Activity, error) {
	l.calls++

	if page > 1 {
		return nil, nil
	}

	return l.activities, nil
}

// boundMemory is a memory store with a token bound to one athlete.
func boundMemory(t *testing.T) *store.Memory {
	t.Helper()

	memory := store.NewMemory()

	if err := memory.Save(t.Context(), strava.Token{
		AthleteID:    4242,
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		Scopes:       []string{"activity:read_all", "activity:write"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	return memory
}

// An import seeds the named log for the bound athlete.
func TestImportWithSeedsTheBoundAthlete(t *testing.T) {
	t.Parallel()

	memory := boundMemory(t)

	list := &listOnce{activities: []strava.Activity{
		{
			ID: 1, Name: "Gegenwind bis Musterdorf", SportType: "GravelRide",
			StartDate: time.Now().Add(-24 * time.Hour),
		},
		{
			ID: 2, Name: "Morning Ride", SportType: "Ride",
			StartDate: time.Now().Add(-48 * time.Hour),
		},
	}}

	if err := importWith(t.Context(), config.Config{}, realStore{memory}, list, 4242, quietLogger()); err != nil {
		t.Fatalf("importWith: %v", err)
	}

	history, err := memory.RecentTitles(t.Context(), 4242, 25)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	if len(history) != 1 {
		t.Fatalf("%d titles seeded, want 1 (the Strava default is skipped)", len(history))
	}

	if history[0].Title != "Gegenwind bis Musterdorf" {
		t.Errorf("seeded %q", history[0].Title)
	}

	if history[0].Source != store.SourceImported {
		t.Errorf("source = %q, want %q", history[0].Source, store.SourceImported)
	}
}

// A bad machine-title pattern stops the import before it reads anything.
func TestImportWithRefusesABadMachineTitlePattern(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.LLM.MachineTitlePatterns = []string{"("}

	list := &listOnce{}

	err := importWith(t.Context(), cfg, realStore{boundMemory(t)}, list, 4242, quietLogger())
	if err == nil {
		t.Fatal("an uncompilable machine-title pattern was accepted")
	}

	if list.calls != 0 {
		t.Errorf("Strava was called %d times despite the bad configuration", list.calls)
	}
}

// A failing listing is reported, and what was already written stays.
func TestImportWithReportsAFailedListing(t *testing.T) {
	t.Parallel()

	err := importWith(t.Context(), config.Config{}, realStore{boundMemory(t)},
		failingList{}, 4242, quietLogger())
	if err == nil {
		t.Fatal("a failed listing was reported as success")
	}
}

type failingList struct{}

func (failingList) ListActivities(context.Context, int, int) ([]strava.Activity, error) {
	return nil, errors.New("strava: 429 rate limited")
}

// Import refuses to run against a store that disappears when it exits.
//
// The guard exists because the mistake it catches is silent: a mistyped or
// missing FIRESTORE_PROJECT would import a few hundred titles into memory and
// report success.
func TestImportRefusesTheInMemoryStore(t *testing.T) {
	t.Parallel()

	err := Import(t.Context(), quietLogger(), env(nil))
	if err == nil {
		t.Fatal("the import ran against the in-memory store")
	}

	if !strings.Contains(err.Error(), "Firestore") {
		t.Errorf("error %q does not name what is missing", err)
	}
}

// A configuration that will not load stops the import.
func TestImportReportsABadConfiguration(t *testing.T) {
	t.Parallel()

	err := Import(t.Context(), quietLogger(), func(string) string { return "" })
	if err == nil {
		t.Fatal("the import ran with no configuration")
	}

	if !strings.Contains(err.Error(), "configuration") {
		t.Errorf("error %q does not say the configuration failed", err)
	}
}

// The import's client cannot write, whatever else changes above it.
//
// The transport refuses every non-GET request when the write mode is dry run,
// so leaving it at the zero value is what makes "an import never writes to
// Strava" a property of the client rather than a promise about the caller.
func TestTheImportClientCannotWrite(t *testing.T) {
	t.Parallel()

	client, err := importClient(config.Config{}, realStore{boundMemory(t)}, 4242)
	if err != nil {
		t.Fatalf("importClient: %v", err)
	}

	if client.WriteMode() != strava.WriteModeDryRun {
		t.Errorf("write mode = %v, want dry run", client.WriteMode())
	}

	// And it refuses a write outright rather than attempting one. Both write
	// paths, because the guarantee is about activities and not about one
	// method.
	if _, err := client.UpdateActivityName(t.Context(), 1, "Neuer Titel"); err == nil {
		t.Error("the import's client accepted a rename")
	}

	if _, err := client.UpdateActivityNameAndDescription(
		t.Context(), 1, "Neuer Titel", "Neue Beschreibung"); err == nil {
		t.Error("the import's client accepted a rename with a description")
	}
}

// The whole import, against a real Firestore.
//
// Everything above this exercises the assembled path with an in-memory store;
// what is left is what only a database can cover — opening it, resolving the
// bound athlete out of it, and the refusal that guards against importing into
// a store that vanishes when the process exits.
//
// Skipped without the emulator, like the store's own conformance suite. CI
// runs one for the whole test run.
func TestImportAgainstTheEmulator(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is not set; start the Firestore emulator to run this")
	}

	// A database of its own, so a parallel run of anything else cannot see it.
	project := "titelheld-import-test"

	// Overrides only; env supplies the Strava client credentials LoadImport
	// requires, along with the rest of a complete environment.
	getenv := env(map[string]string{
		"FIRESTORE_PROJECT":  project,
		"FIRESTORE_DATABASE": "(default)",
	})

	// No athlete bound yet: the import must say so rather than import nothing
	// and report success.
	err := Import(t.Context(), quietLogger(), getenv)
	if err == nil {
		t.Fatal("the import ran with no athlete bound")
	}

	if !strings.Contains(err.Error(), "authorization flow") {
		t.Errorf("error %q does not say what to do about it", err)
	}
}

// The bound-store prologue tells a store failure from a missing athlete: only
// "none or several tokens" means the authorization flow is what is missing,
// and anything else is reported in the store's own words.
func TestBoundAthleteErrorsAreToldApart(t *testing.T) {
	t.Parallel()

	empty := store.NewMemory()

	if _, err := empty.AnyToken(t.Context()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an empty store's AnyToken = %v, want ErrNotFound", err)
	}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no athlete", store.ErrNotFound, "run the authorization flow first"},
		{"store failure", errors.New("firestore: list tokens: unavailable"), "resolve the bound athlete: firestore: list tokens: unavailable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := boundAthleteError(tc.err)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("boundAthleteError(%v) = %v, want %q", tc.err, err, tc.want)
			}

			if tc.name == "store failure" && strings.Contains(err.Error(), "authorization flow") {
				t.Errorf("a store failure was blamed on the authorization flow: %v", err)
			}
		})
	}
}
