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
// must be neutralized rather than assumed away.
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
		"Due":    func() error { _, err := firestoreStore.Due(ctx, storetest.Now); return err },
		"Remove": func() error { return firestoreStore.Remove(ctx, 1, 2) },
		"Len":    func() error { _, err := firestoreStore.Len(ctx); return err },
		"MarkNamed": func() error {
			return firestoreStore.MarkNamed(ctx, store.Naming{
				AthleteID: 1, ActivityID: 2, Title: "t", At: storetest.Now,
			})
		},
		"RecentTitles": func() error {
			_, err := firestoreStore.RecentTitles(ctx, 1, 5)
			return err
		},
		"Named":     func() error { _, _, err := firestoreStore.Named(ctx, 1, 2); return err },
		"Place":     func() error { _, _, err := firestoreStore.Place(ctx, "k"); return err },
		"SavePlace": func() error { return firestoreStore.SavePlace(ctx, "k", store.Place{Name: "n"}) },
		"FranchisePosition": func() error {
			_, err := firestoreStore.FranchisePosition(ctx, 1, "pink-panther")
			return err
		},
		"AdvanceFranchise": func() error {
			_, err := firestoreStore.AdvanceFranchise(ctx, 1, "pink-panther")
			return err
		},
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

	// position is an int in the schema. A franchise position that cannot be
	// read must not decode as zero: zero means "not started", so the athlete
	// would be sent back to the first title of the series and every entry
	// already used would be handed out a second time.
	if _, err := raw.Collection(collectionPrefix+fsstore.CollectionFranchise).
		Doc("1-pink-panther").Set(t.Context(), map[string]any{"position": "not-a-number"}); err != nil {
		t.Fatalf("seed corrupt franchise position: %v", err)
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

	if _, err := firestoreStore.FranchisePosition(t.Context(), 1, "pink-panther"); err == nil {
		t.Error("FranchisePosition on a corrupt document = nil error, want a decode failure")
	}

	// The same document read inside the transaction, so the decode failure
	// there is reported rather than starting the series again.
	if _, err := firestoreStore.AdvanceFranchise(t.Context(), 1, "pink-panther"); err == nil {
		t.Error("AdvanceFranchise on a corrupt document = nil error, want a decode failure")
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

// A franchise name is configuration, so it may be anything a person types.
//
// The one this service ships with is "Pink Panther" — a space, which the safe
// document-ID set rejects. A name containing "/" would not be a document ID at
// all but a path with the wrong number of segments. Both have to work, and two
// different franchises must never land on one document: sharing a position
// would hand out the same title twice.
func TestFranchiseNamesThatAreNotValidDocumentIDs(t *testing.T) {
	requireEmulator(t)

	firestoreStore := newStore(t)

	names := []string{
		"Pink Panther",
		"Herr der Ringe / LOTR",
		"__proto__",
		"..",
		".",
		"Ocean's Eleven",
		"Über-Runde",
		strings.Repeat("long", 500),
	}

	positions := make(map[string]int, len(names))

	for _, name := range names {
		position, err := firestoreStore.AdvanceFranchise(t.Context(), 1, name)
		if err != nil {
			t.Fatalf("AdvanceFranchise(%q): %v", name, err)
		}

		if position != 1 {
			t.Errorf("AdvanceFranchise(%q) = %d, want 1", name, position)
		}

		positions[name] = position
	}

	// Advance one of them again. If any two names collided, another franchise
	// would see this increment.
	if _, err := firestoreStore.AdvanceFranchise(t.Context(), 1, names[0]); err != nil {
		t.Fatalf("second AdvanceFranchise: %v", err)
	}

	for _, name := range names {
		got, err := firestoreStore.FranchisePosition(t.Context(), 1, name)
		if err != nil {
			t.Fatalf("FranchisePosition(%q): %v", name, err)
		}

		want := positions[name]
		if name == names[0] {
			want = 2
		}

		if got != want {
			t.Errorf("FranchisePosition(%q) = %d, want %d: two names share a document",
				name, got, want)
		}
	}
}

// The declared index has to match the query, and nothing else checks that.
//
// The emulator serves any query without index definitions, so every test in
// this package passes whether or not the Terraform declaration is right. The
// mismatch shows up only against the real database, as an error on every
// naming. Reading the declaration here turns "remember to eyeball it in
// review" into something that fails a build.
//
// It asserts the shape the query needs — an equality on athlete_id with an
// ordering on named_at descending — not the file's formatting.
func TestTheDeclaredIndexMatchesTheRecentTitlesQuery(t *testing.T) {
	t.Parallel()

	const declaration = "../../../infra/firestore.tf"

	raw, err := os.ReadFile(declaration)
	if err != nil {
		t.Fatalf("read the index declaration: %v", err)
	}

	terraform := string(raw)

	block := strings.Index(terraform, `resource "google_firestore_index" "named_recent"`)
	if block < 0 {
		t.Fatal("no google_firestore_index.named_recent is declared; RecentTitles cannot run")
	}

	body := terraform[block:]
	if end := strings.Index(body, "\nresource "); end > 0 {
		body = body[:end]
	}

	// The collection the query runs against. Production sets no prefix, so the
	// constant is the collection ID verbatim.
	if want := `collection = "` + fsstore.CollectionNamed + `"`; !strings.Contains(body, want) {
		t.Errorf("the index does not declare %s", want)
	}

	// Each field paired with its own order. Checking the two independently
	// would pass a declaration of athlete_id descending and named_at
	// ascending, which contains all four strings and is the wrong index.
	blocks := fieldBlocks(body)

	want := [][2]string{
		{"athlete_id", "ASCENDING"},
		{"named_at", "DESCENDING"},
	}

	if len(blocks) != len(want) {
		t.Fatalf("the index declares %d fields, want %d: %v", len(blocks), len(want), blocks)
	}

	// Order matters too: a composite index is ordered, and athlete_id has to
	// come first for an equality followed by a sort.
	for index, pair := range want {
		if blocks[index] != pair {
			t.Errorf("field %d is %v, want %v", index, blocks[index], pair)
		}
	}
}

// fieldBlocks extracts each fields { field_path, order } pair, in order.
func fieldBlocks(body string) [][2]string {
	var pairs [][2]string

	for rest := body; ; {
		start := strings.Index(rest, "fields {")
		if start < 0 {
			return pairs
		}

		rest = rest[start+len("fields {"):]

		end := strings.Index(rest, "}")
		if end < 0 {
			return pairs
		}

		block := rest[:end]
		rest = rest[end:]

		pairs = append(pairs, [2]string{
			quoted(block, "field_path"),
			quoted(block, "order"),
		})
	}
}

// quoted reads the quoted value of `name = "..."` from a block.
func quoted(block, name string) string {
	at := strings.Index(block, name)
	if at < 0 {
		return ""
	}

	rest := block[at:]

	open := strings.Index(rest, `"`)
	if open < 0 {
		return ""
	}

	rest = rest[open+1:]

	close := strings.Index(rest, `"`)
	if close < 0 {
		return ""
	}

	return rest[:close]
}
