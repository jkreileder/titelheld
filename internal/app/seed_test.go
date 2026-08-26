package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/processor"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// The first document is the shipped profile with the named entries reserved,
// and the rotation then offers nothing when every remaining entry is reserved.
//
// The negative control is the same document without the reservation: the
// shipped profile offers its one unreserved entry, so a seed that dropped the
// -reserve flags on the floor would still read back as "offers something".
func TestSeedConfigReservesOnTopOfTheDefaultProfile(t *testing.T) {
	t.Parallel()

	profile := naming.DefaultProfile()
	if len(profile) != 1 {
		t.Fatalf("the shipped profile has %d series; this test assumes one", len(profile))
	}

	offered, _, ok := profile[0].Next(0)
	if !ok {
		t.Fatal("the shipped profile offers nothing; reserving cannot be told from doing nothing")
	}

	memory := boundMemory(t)

	if err := seedConfigWith(t.Context(), memory, 4242, []string{offered}, quietLogger()); err != nil {
		t.Fatalf("seedConfigWith: %v", err)
	}

	written, exists, err := memory.AthleteConfig(t.Context(), 4242)
	if err != nil || !exists {
		t.Fatalf("AthleteConfig: exists=%v, err=%v", exists, err)
	}

	stored := processor.FranchisesFromStored(written.Franchises)
	if len(stored) != 1 || stored[0].Name != profile[0].Name {
		t.Fatalf("stored %+v, want the shipped series", stored)
	}

	if len(stored[0].Titles) != len(profile[0].Titles) {
		t.Errorf("titles were %d, want the profile's %d: reserving is not deleting",
			len(stored[0].Titles), len(profile[0].Titles))
	}

	if len(stored[0].Reserved) != len(profile[0].Reserved)+1 {
		t.Errorf("reserved = %v, want the profile's plus %q", stored[0].Reserved, offered)
	}

	if next, _, ok := stored[0].Next(0); ok {
		t.Errorf("the seeded series still offers %q", next)
	}

	// The control: without the flag the same seed offers the entry.
	control := store.NewMemory()

	if err := seedConfigWith(t.Context(), control, 4242, nil, quietLogger()); err != nil {
		t.Fatalf("seedConfigWith (control): %v", err)
	}

	unreserved, _, _ := control.AthleteConfig(t.Context(), 4242)
	if next, _, ok := processor.FranchisesFromStored(unreserved.Franchises)[0].Next(0); !ok || next != offered {
		t.Errorf("the control offers %q, %v; want %q", next, ok, offered)
	}
}

// A second run refuses: the document is authoritative once it exists.
func TestSeedConfigRefusesAnExistingDocument(t *testing.T) {
	t.Parallel()

	memory := boundMemory(t)

	if err := seedConfigWith(t.Context(), memory, 4242, nil, quietLogger()); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	before, _, _ := memory.AthleteConfig(t.Context(), 4242)

	err := seedConfigWith(t.Context(), memory, 4242, []string{"Son of the Pink Panther"}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "authoritative") {
		t.Fatalf("second seed: %v, want a refusal naming the document as authoritative", err)
	}

	after, _, _ := memory.AthleteConfig(t.Context(), 4242)
	if len(after.Franchises[0].Reserved) != len(before.Franchises[0].Reserved) {
		t.Errorf("the refused seed changed the document: %v -> %v",
			before.Franchises[0].Reserved, after.Franchises[0].Reserved)
	}
}

// A reservation that names nothing is refused, not written inert.
func TestSeedConfigRefusesAnUnknownEntry(t *testing.T) {
	t.Parallel()

	memory := boundMemory(t)

	err := seedConfigWith(t.Context(), memory, 4242, []string{"The Pink Panther Goes Gravel"}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "0 entries") {
		t.Fatalf("err = %v, want a refusal counting zero matches", err)
	}

	if _, exists, _ := memory.AthleteConfig(t.Context(), 4242); exists {
		t.Error("a refused seed wrote a document")
	}
}

// Reserving an entry twice, or one the profile already reserves, is harmless.
func TestReserveEntriesIsIdempotent(t *testing.T) {
	t.Parallel()

	profile := naming.DefaultProfile()
	already := profile[0].Reserved[0]

	offered, _, _ := profile[0].Next(0)

	reserved, err := reserveEntries(profile, []string{already, offered, " " + offered + " "})
	if err != nil {
		t.Fatalf("reserveEntries: %v", err)
	}

	if got, want := len(reserved[0].Reserved), len(naming.DefaultProfile()[0].Reserved)+1; got != want {
		t.Errorf("%d reserved entries, want %d: %v", got, want, reserved[0].Reserved)
	}
}

// The store's failures are reported with their own words.
func TestSeedConfigReportsAStoreFailure(t *testing.T) {
	t.Parallel()

	err := seedConfigWith(t.Context(), failingConfigStore{Store: store.NewMemory()}, 4242, nil, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "firestore: unavailable") {
		t.Fatalf("err = %v, want the store's own error", err)
	}
}

type failingConfigStore struct {
	store.Store
}

func (failingConfigStore) AthleteConfig(context.Context, int64) (store.AthleteConfig, bool, error) {
	return store.AthleteConfig{}, false, errors.New("firestore: unavailable")
}

// Without Firestore there is nothing durable to seed.
func TestSeedConfigRefusesTheInMemoryStore(t *testing.T) {
	t.Parallel()

	err := SeedConfig(t.Context(), quietLogger(), func(string) string { return "" }, nil)
	if err == nil || !strings.Contains(err.Error(), "Firestore") {
		t.Fatalf("err = %v, want a refusal naming Firestore", err)
	}
}

// A database without a project fails closed, as it does for the service.
func TestSeedConfigReportsABadConfiguration(t *testing.T) {
	t.Parallel()

	err := SeedConfig(t.Context(), quietLogger(), func(name string) string {
		if name == "FIRESTORE_DATABASE" {
			return "titelheld"
		}

		return ""
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("err = %v, want a configuration error", err)
	}
}

// Against the emulator, the wiring above seedConfigWith is exercised: the
// configuration is read, Firestore is opened, and the athlete is resolved —
// and with no athlete bound the command says so rather than seeding nobody.
func TestSeedConfigAgainstTheEmulator(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is not set; start the Firestore emulator to run this")
	}

	getenv := func(name string) string {
		switch name {
		case "FIRESTORE_PROJECT":
			return "titelheld-seed-test"
		case "FIRESTORE_DATABASE":
			return "(default)"
		default:
			return ""
		}
	}

	err := SeedConfig(t.Context(), quietLogger(), getenv, []string{"Son of the Pink Panther"})
	if err == nil {
		t.Fatal("the seed ran with no athlete bound")
	}

	if !strings.Contains(err.Error(), "authorization flow") {
		t.Errorf("error %q does not say what to do about it", err)
	}
}

// Each store failure after the absence check is reported with the store's
// own words and stops the seed: the write, the read-back, and the position.
func TestSeedConfigReportsEveryStoreFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		fault func(*seedFaults)
		want  string
	}{
		{"write", func(f *seedFaults) { f.saveErr = errors.New("firestore: write refused") }, "write refused"},
		{"read-back", func(f *seedFaults) { f.readBackErr = errors.New("firestore: read refused") }, "read refused"},
		{"position", func(f *seedFaults) { f.positionErr = errors.New("firestore: position refused") }, "position refused"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			faults := &seedFaults{Store: store.NewMemory()}
			tc.fault(faults)

			err := seedConfigWith(t.Context(), faults, 4242, nil, quietLogger())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want the store's %q", err, tc.want)
			}
		})
	}
}

// seedFaults fails one store call the seed makes, and delegates the rest.
type seedFaults struct {
	store.Store

	saveErr     error
	readBackErr error
	positionErr error

	reads int
}

func (f *seedFaults) SaveAthleteConfig(ctx context.Context, athleteID int64, config store.AthleteConfig) error {
	if f.saveErr != nil {
		return f.saveErr
	}

	return f.Store.SaveAthleteConfig(ctx, athleteID, config)
}

func (f *seedFaults) AthleteConfig(ctx context.Context, athleteID int64) (store.AthleteConfig, bool, error) {
	f.reads++

	// The first read is the absence check, which must succeed for the seed
	// to reach the write; the second is the read-back.
	if f.readBackErr != nil && f.reads > 1 {
		return store.AthleteConfig{}, false, f.readBackErr
	}

	return f.Store.AthleteConfig(ctx, athleteID)
}

func (f *seedFaults) FranchisePosition(ctx context.Context, athleteID int64, franchise string) (int, error) {
	if f.positionErr != nil {
		return 0, f.positionErr
	}

	return f.Store.FranchisePosition(ctx, athleteID, franchise)
}

// An empty name is refused before anything is matched.
func TestReserveEntriesRefusesAnEmptyName(t *testing.T) {
	t.Parallel()

	if _, err := reserveEntries(naming.DefaultProfile(), []string{"  "}); err == nil {
		t.Fatal("an empty entry was accepted")
	}
}

// Against the emulator with an athlete bound, the whole command runs: the
// store is opened, the athlete resolved, the document written and read back —
// and a second run refuses, because the document is now authoritative.
func TestSeedConfigWritesTheDocumentAgainstTheEmulator(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is not set; start the Firestore emulator to run this")
	}

	// A project of this run's own: the emulator keeps documents for as long
	// as it runs, and a fixed name would make the second invocation meet the
	// document the first one wrote and refuse.
	project := fmt.Sprintf("titelheld-seed-bound-%d", time.Now().UnixNano())

	getenv := func(name string) string {
		switch name {
		case "FIRESTORE_PROJECT":
			return project
		case "FIRESTORE_DATABASE":
			return "(default)"
		default:
			return ""
		}
	}

	cfg, err := config.LoadStore(getenv)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}

	// Bind an athlete the way the OAuth callback does, so AnyToken resolves.
	dataStore, closeStore, err := openStore(t.Context(), cfg, quietLogger())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}

	defer closeStore()

	if err := dataStore.Save(t.Context(), strava.Token{
		AthleteID: 4242, AccessToken: "test-access-token", RefreshToken: "test-refresh-token",
		ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		Scopes:    []string{"activity:read_all", "activity:write"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	offered, _, _ := naming.DefaultProfile()[0].Next(0)

	if err := SeedConfig(t.Context(), quietLogger(), getenv, []string{offered}); err != nil {
		t.Fatalf("SeedConfig: %v", err)
	}

	written, exists, err := dataStore.AthleteConfig(t.Context(), 4242)
	if err != nil || !exists {
		t.Fatalf("AthleteConfig after the seed: exists=%v, err=%v", exists, err)
	}

	if next, _, ok := processor.FranchisesFromStored(written.Franchises)[0].Next(0); ok {
		t.Errorf("the seeded series still offers %q", next)
	}

	err = SeedConfig(t.Context(), quietLogger(), getenv, nil)
	if err == nil || !strings.Contains(err.Error(), "authoritative") {
		t.Errorf("second seed: %v, want a refusal", err)
	}
}
