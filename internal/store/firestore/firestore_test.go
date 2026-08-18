package firestore_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/firestore"

	"github.com/jkreileder/titelheld/internal/store"
	fsstore "github.com/jkreileder/titelheld/internal/store/firestore"
	"github.com/jkreileder/titelheld/internal/store/storetest"
	"github.com/jkreileder/titelheld/internal/strava"
)

// testProject is a fake project ID. The emulator accepts any value and never
// contacts Google, which is what keeps CI off a real project.
const testProject = "titelheld-emulator-test"

// emulatorEnv is the variable the Firestore client library reads to decide it
// is talking to an emulator rather than to Google.
const emulatorEnv = "FIRESTORE_EMULATOR_HOST"

// Collection prefixes must be unique per *run*, not merely per process. The
// emulator keeps its data until it is restarted, so a counter alone makes a
// second run of the same suite read the first run's documents — which passes
// once and then fails, and looks like flakiness rather than the isolation bug
// it is.
var (
	prefixCounter atomic.Int64
	runID         = newRunID()
)

func newRunID() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		panic("firestore test: no entropy for a run ID: " + err.Error())
	}

	return hex.EncodeToString(raw)
}

// prefix returns a collection prefix nothing else will use.
func prefix(kind string) string {
	return kind + runID + "-" + strconv.FormatInt(prefixCounter.Add(1), 10) + "_"
}

// requireEmulator skips when there is no emulator to talk to.
//
// The skip is deliberate so `go test ./...` works on a laptop without one, but
// it is the dangerous kind of skip: a CI job full of skips looks exactly like a
// CI job full of passes. The firestore job in go.yaml therefore asserts that
// these tests actually ran.
func requireEmulator(t *testing.T) {
	t.Helper()

	if os.Getenv(emulatorEnv) == "" {
		t.Skipf("%s is not set; start the Firestore emulator to run these tests", emulatorEnv)
	}
}

func newStore(t *testing.T) store.Store {
	t.Helper()

	firestoreStore, err := fsstore.New(t.Context(), fsstore.Config{
		ProjectID: testProject,
		Prefix:    prefix("suite"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() {
		if err := firestoreStore.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	return firestoreStore
}

// TestFirestoreConformance holds Firestore to the same assertions as the
// in-memory store. "Same semantics" is the whole claim of this phase.
func TestFirestoreConformance(t *testing.T) {
	requireEmulator(t)
	storetest.Suite(t, newStore)
}

func TestNewRequiresAProjectID(t *testing.T) {
	t.Parallel()

	if _, err := fsstore.New(t.Context(), fsstore.Config{}); err == nil {
		t.Error("New without a project ID = nil error, want error")
	}
}

// AnyToken is not part of the shared suite because only the one-time bootstrap
// uses it, but both implementations must agree on it: it is what lets the
// service refuse a second athlete when no athlete ID is configured.
func TestAnyToken(t *testing.T) {
	requireEmulator(t)

	firestoreStore, err := fsstore.New(t.Context(), fsstore.Config{
		ProjectID: testProject,
		Prefix:    prefix("anytoken"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = firestoreStore.Close() })

	if _, err := firestoreStore.AnyToken(t.Context()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AnyToken on an empty store = %v, want store.ErrNotFound", err)
	}

	if err := firestoreStore.Save(t.Context(), strava.Token{AthleteID: 7, RefreshToken: "r"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	only, err := firestoreStore.AnyToken(t.Context())
	if err != nil {
		t.Fatalf("AnyToken: %v", err)
	}
	if only.AthleteID != 7 {
		t.Errorf("AthleteID = %d, want 7", only.AthleteID)
	}

	// With two athletes there is no single answer.
	if err := firestoreStore.Save(t.Context(), strava.Token{AthleteID: 8, RefreshToken: "r"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := firestoreStore.AnyToken(t.Context()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AnyToken with two athletes = %v, want store.ErrNotFound", err)
	}
}

// The cache key comes from another package, so document IDs Firestore rejects
// must be neutralised rather than assumed away.
func TestGeocodeKeysThatAreNotValidDocumentIDs(t *testing.T) {
	requireEmulator(t)

	firestoreStore := newStore(t)

	// Every key gets a *distinct* place, and all of them are written before any
	// is read back. Writing the same place each time would hide a collision:
	// two keys mapping to one document would still read back correctly.
	keys := []string{
		"", ".", "..", "0.000,0.000",
		"48.123/12.456",
		// Differs from the previous key only in the character that a
		// substituting escape would flatten.
		"48.123_12.456",
		"a/b", "a_b",
		strings.Repeat("x", 2000),
	}

	for i, key := range keys {
		place := store.Place{Name: "Musterdorf" + strconv.Itoa(i), Kind: "village"}

		if err := firestoreStore.SavePlace(t.Context(), key, place); err != nil {
			t.Fatalf("SavePlace(%q): %v", key, err)
		}
	}

	for i, key := range keys {
		want := "Musterdorf" + strconv.Itoa(i)

		cached, ok, err := firestoreStore.Place(t.Context(), key)
		if err != nil || !ok {
			t.Fatalf("Place(%q) = %+v, %v, %v", key, cached, ok, err)
		}

		if cached.Name != want {
			t.Errorf("Place(%q).Name = %q, want %q — two keys share a document",
				key, cached.Name, want)
		}
	}
}

// Every method must surface a failure rather than panic or report a false
// "not found". Closing the client is the cheapest way to make every RPC fail.
func TestEveryMethodReportsAFailedClient(t *testing.T) {
	requireEmulator(t)

	firestoreStore, err := fsstore.New(t.Context(), fsstore.Config{
		ProjectID: testProject,
		Prefix:    prefix("closed"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	if err := firestoreStore.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := t.Context()

	checks := map[string]func() error{
		"Load":     func() error { _, err := firestoreStore.Load(ctx, 1); return err },
		"Save":     func() error { return firestoreStore.Save(ctx, strava.Token{AthleteID: 1}) },
		"AnyToken": func() error { _, err := firestoreStore.AnyToken(ctx); return err },
		"Enqueue": func() error {
			_, err := firestoreStore.Enqueue(ctx, store.Pending{AthleteID: 1, ActivityID: 2})
			return err
		},
		"Due":       func() error { _, err := firestoreStore.Due(ctx, storetest.Now); return err },
		"Remove":    func() error { return firestoreStore.Remove(ctx, 1, 2) },
		"Len":       func() error { _, err := firestoreStore.Len(ctx); return err },
		"MarkNamed": func() error { return firestoreStore.MarkNamed(ctx, 1, 2, "t") },
		"Named":     func() error { _, _, err := firestoreStore.Named(ctx, 1, 2); return err },
		"Place":     func() error { _, _, err := firestoreStore.Place(ctx, "k"); return err },
		"SavePlace": func() error { return firestoreStore.SavePlace(ctx, "k", store.Place{Name: "n"}) },
	}

	for name, call := range checks {
		if err := call(); err == nil {
			t.Errorf("%s on a closed client = nil error, want a failure", name)
		}
	}

	// Closing twice must not panic.
	if err := firestoreStore.Close(); err == nil {
		t.Log("second Close returned nil; acceptable")
	}
}

// A document whose fields do not match the expected shape must be reported, not
// silently read as a zero value — a half-decoded token would look like a valid
// one with an empty refresh token.
func TestCorruptDocumentsAreReported(t *testing.T) {
	requireEmulator(t)

	collectionPrefix := prefix("corrupt")

	raw, err := firestore.NewClient(t.Context(), testProject)
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}

	t.Cleanup(func() { _ = raw.Close() })

	// expires_at is a Timestamp in the schema; a string cannot decode into it.
	if _, err := raw.Collection(collectionPrefix+fsstore.CollectionTokens).
		Doc("1").Set(t.Context(), map[string]any{"expires_at": "not-a-timestamp"}); err != nil {
		t.Fatalf("seed corrupt token: %v", err)
	}

	if _, err := raw.Collection(collectionPrefix+fsstore.CollectionNamed).
		Doc("1-2").Set(t.Context(), map[string]any{"title": 42}); err != nil {
		t.Fatalf("seed corrupt named entry: %v", err)
	}

	firestoreStore, err := fsstore.New(t.Context(), fsstore.Config{
		ProjectID: testProject,
		Prefix:    collectionPrefix,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = firestoreStore.Close() })

	if _, err := firestoreStore.Load(t.Context(), 1); err == nil {
		t.Error("Load of a corrupt token = nil error, want a decode failure")
	}
	if _, _, err := firestoreStore.Named(t.Context(), 1, 2); err == nil {
		t.Error("Named on a corrupt entry = nil error, want a decode failure")
	}
	if _, err := firestoreStore.AnyToken(t.Context()); err == nil {
		t.Error("AnyToken over a corrupt token = nil error, want a decode failure")
	}
}

// Firestore rejects document IDs matching __.*__ outright, and the safe set
// accepts "_", so the reserved form has to be excluded explicitly.
func TestReservedDocumentIDFormIsEscaped(t *testing.T) {
	requireEmulator(t)

	firestoreStore := newStore(t)

	keys := []string{"__proto__", "__a__", "____", "__", "_x_", "a__b"}

	for i, key := range keys {
		place := store.Place{Name: "Musterdorf" + strconv.Itoa(i), Kind: "village"}

		if err := firestoreStore.SavePlace(t.Context(), key, place); err != nil {
			t.Fatalf("SavePlace(%q): %v", key, err)
		}
	}

	for i, key := range keys {
		cached, ok, err := firestoreStore.Place(t.Context(), key)
		if err != nil || !ok {
			t.Fatalf("Place(%q) = %+v, %v, %v", key, cached, ok, err)
		}

		if want := "Musterdorf" + strconv.Itoa(i); cached.Name != want {
			t.Errorf("Place(%q).Name = %q, want %q", key, cached.Name, want)
		}
	}
}
