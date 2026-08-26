package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/processor"
	"github.com/jkreileder/titelheld/internal/store"
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
