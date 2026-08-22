// Package storetest holds the conformance suite every [store.Store]
// implementation must pass.
//
// It lives in its own package, rather than in a _test.go file, so the in-memory
// and Firestore implementations can be held to exactly the same assertions.
// "Same semantics as the memory store" is otherwise a claim nobody checks.
package storetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// Now is the fixed clock the suite uses. Implementations must round-trip
// timestamps closely enough that comparisons at second granularity hold;
// Firestore stores microseconds and returns UTC.
var Now = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

// Factory returns a fresh, empty store. It is called once per sub-test, so
// implementations must isolate each one — a Firestore-backed factory should use
// a distinct collection prefix or athlete ID per call.
type Factory func(t *testing.T) store.Store

// Suite runs every conformance test against the store the factory produces.
func Suite(t *testing.T, newStore Factory) {
	t.Helper()

	tests := map[string]func(*testing.T, store.Store){
		"TokenRoundTrip":              tokenRoundTrip,
		"TokenRotationReplaces":       tokenRotationReplaces,
		"TokenMissingIsTyped":         tokenMissingIsTyped,
		"TokensKeyedByAthlete":        tokensKeyedByAthlete,
		"EnqueueIsIdempotent":         enqueueIsIdempotent,
		"DueRespectsTheDeadline":      dueRespectsTheDeadline,
		"DueIsOrderedOldestFirst":     dueIsOrderedOldestFirst,
		"DueBreaksTiesConsistently":   dueBreaksTiesConsistently,
		"RemoveIsForgiving":           removeIsForgiving,
		"QueueKeyedByAthlete":         queueKeyedByAthlete,
		"NamedLogRoundTrip":           namedLogRoundTrip,
		"NamedLogKeyedByAthlete":      namedLogKeyedByAthlete,
		"GeocodeCacheRoundTrip":       geocodeCacheRoundTrip,
		"GeocodeCacheMissIsNotAnErr":  geocodeCacheMissIsNotAnErr,
		"FranchiseStartsAtZero":       franchiseStartsAtZero,
		"FranchiseAdvances":           franchiseAdvances,
		"FranchisesAreIndependent":    franchisesAreIndependent,
		"FranchiseKeyedByAthlete":     franchiseKeyedByAthlete,
		"FranchiseNamesAreArbitrary":  franchiseNamesAreArbitrary,
		"RecentTitlesNewestFirst":     recentTitlesNewestFirst,
		"RecentTitlesKeyedByAthlete":  recentTitlesKeyedByAthlete,
		"RecentTitlesBounded":         recentTitlesBounded,
		"AthleteConfigRoundTrip":      athleteConfigRoundTrip,
		"AthleteConfigAbsent":         athleteConfigAbsent,
		"AthleteConfigKeyedByAthlete": athleteConfigKeyedByAthlete,
		"AthleteConfigReplaces":       athleteConfigReplaces,
		"AthleteConfigIsCopied":       athleteConfigIsCopied,
	}

	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			run(t, newStore(t))
		})
	}
}

func tokenRoundTrip(t *testing.T, s store.Store) {
	token := strava.Token{
		AthleteID:    4242,
		AccessToken:  "access-one",
		RefreshToken: "refresh-one",
		ExpiresAt:    Now.Add(6 * time.Hour),
		Scopes:       []string{strava.ScopeActivityReadAll, strava.ScopeActivityWrite},
	}

	if err := s.Save(t.Context(), token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := s.Load(t.Context(), 4242)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.AccessToken != token.AccessToken || loaded.RefreshToken != token.RefreshToken {
		t.Errorf("tokens = %q/%q, want %q/%q",
			loaded.AccessToken, loaded.RefreshToken, token.AccessToken, token.RefreshToken)
	}
	if loaded.AthleteID != token.AthleteID {
		t.Errorf("AthleteID = %d, want %d", loaded.AthleteID, token.AthleteID)
	}
	if !loaded.ExpiresAt.Equal(token.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", loaded.ExpiresAt, token.ExpiresAt)
	}
	if !loaded.HasScope(strava.ScopeActivityWrite) {
		t.Errorf("scopes = %v, want the write scope preserved", loaded.Scopes)
	}
}

// The refresh token rotates on every refresh, so a save must replace rather
// than accumulate. Losing this is how the service ends up with a dead token.
func tokenRotationReplaces(t *testing.T, s store.Store) {
	token := strava.Token{AthleteID: 4242, RefreshToken: "refresh-one", ExpiresAt: Now}

	if err := s.Save(t.Context(), token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	token.RefreshToken = "refresh-two"
	if err := s.Save(t.Context(), token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := s.Load(t.Context(), 4242)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.RefreshToken != "refresh-two" {
		t.Errorf("RefreshToken = %q, want the rotated value", loaded.RefreshToken)
	}
}

func tokenMissingIsTyped(t *testing.T, s store.Store) {
	if _, err := s.Load(t.Context(), 999); !errors.Is(err, strava.ErrTokenNotFound) {
		t.Errorf("Load on an empty store = %v, want strava.ErrTokenNotFound", err)
	}
}

func tokensKeyedByAthlete(t *testing.T, s store.Store) {
	for _, id := range []int64{1, 2} {
		if err := s.Save(t.Context(), strava.Token{AthleteID: id, RefreshToken: "r"}); err != nil {
			t.Fatalf("Save(%d): %v", id, err)
		}
	}

	if _, err := s.Load(t.Context(), 1); err != nil {
		t.Errorf("Load(1): %v", err)
	}
	if _, err := s.Load(t.Context(), 3); !errors.Is(err, strava.ErrTokenNotFound) {
		t.Errorf("Load(3) = %v, want ErrTokenNotFound", err)
	}
}

func enqueueIsIdempotent(t *testing.T, s store.Store) {
	pending := store.Pending{
		AthleteID:    4242,
		ActivityID:   19755622151,
		Aspect:       "create",
		EnqueuedAt:   Now,
		ProcessAfter: Now.Add(10 * time.Minute),
	}

	added, err := s.Enqueue(t.Context(), pending)
	if err != nil || !added {
		t.Fatalf("Enqueue = %v, %v; want true, nil", added, err)
	}

	pending.Aspect = "update"

	added, err = s.Enqueue(t.Context(), pending)
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if added {
		t.Error("second Enqueue = true, want false — a redelivery is one unit of work")
	}

	count, err := s.Len(t.Context())
	if err != nil || count != 1 {
		t.Errorf("Len = %d, %v; want 1, nil", count, err)
	}
}

func dueRespectsTheDeadline(t *testing.T, s store.Store) {
	entries := []store.Pending{
		{AthleteID: 1, ActivityID: 100, ProcessAfter: Now.Add(-10 * time.Minute)},
		{AthleteID: 1, ActivityID: 200, ProcessAfter: Now},
		{AthleteID: 1, ActivityID: 300, ProcessAfter: Now.Add(5 * time.Minute)},
	}

	for _, entry := range entries {
		if _, err := s.Enqueue(t.Context(), entry); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	due, err := s.Due(t.Context(), Now)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	// An entry is due at exactly its deadline, and not before.
	if len(due) != 2 {
		t.Fatalf("Due returned %d entries, want 2 (%v)", len(due), ids(due))
	}
	if due[0].ActivityID != 100 || due[1].ActivityID != 200 {
		t.Errorf("Due = %v, want [100 200]", ids(due))
	}
}

func dueIsOrderedOldestFirst(t *testing.T, s store.Store) {
	for i, id := range []int64{300, 100, 200} {
		if _, err := s.Enqueue(t.Context(), store.Pending{
			AthleteID:    1,
			ActivityID:   id,
			ProcessAfter: Now.Add(time.Duration(-10+i) * time.Minute),
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	due, err := s.Due(t.Context(), Now)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	// Assert the count first: the ordering loop below is vacuous for zero or
	// one entry, so an implementation that silently dropped entries would pass.
	if len(due) != 3 {
		t.Fatalf("Due returned %d entries, want 3 (%v)", len(due), ids(due))
	}

	for i := 1; i < len(due); i++ {
		if due[i].ProcessAfter.Before(due[i-1].ProcessAfter) {
			t.Fatalf("Due is not ordered oldest first: %v", due)
		}
	}
}

// Two webhook events in the same second share a deadline. Firestore's own
// tie-break compares document IDs as strings, which puts 1000 before 200, so
// this is the one dimension where the implementations could quietly disagree.
func dueBreaksTiesConsistently(t *testing.T, s store.Store) {
	for _, id := range []int64{1000, 200, 30} {
		if _, err := s.Enqueue(t.Context(), store.Pending{
			AthleteID: 1, ActivityID: id, ProcessAfter: Now,
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	due, err := s.Due(t.Context(), Now)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	if len(due) != 3 {
		t.Fatalf("Due returned %d entries, want 3 (%v)", len(due), ids(due))
	}

	want := []int64{30, 200, 1000}
	for i, expected := range want {
		if due[i].ActivityID != expected {
			t.Fatalf("Due = %v, want %v — ties order numerically, not as strings",
				ids(due), want)
		}
	}
}

func removeIsForgiving(t *testing.T, s store.Store) {
	if _, err := s.Enqueue(t.Context(), store.Pending{AthleteID: 1, ActivityID: 5}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := s.Remove(t.Context(), 1, 5); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	count, _ := s.Len(t.Context())
	if count != 0 {
		t.Errorf("Len = %d, want 0", count)
	}

	// Removing something already gone is not an error: the sweep may race with
	// itself after a retry.
	if err := s.Remove(t.Context(), 1, 5); err != nil {
		t.Errorf("Remove of a missing entry = %v, want nil", err)
	}
}

func queueKeyedByAthlete(t *testing.T, s store.Store) {
	for _, athlete := range []int64{1, 2} {
		added, err := s.Enqueue(t.Context(), store.Pending{AthleteID: athlete, ActivityID: 5})
		if err != nil || !added {
			t.Fatalf("Enqueue(%d) = %v, %v", athlete, added, err)
		}
	}

	count, _ := s.Len(t.Context())
	if count != 2 {
		t.Errorf("Len = %d, want 2 — one activity ID for two athletes is two entries", count)
	}
}

func namedLogRoundTrip(t *testing.T, s store.Store) {
	if _, named, err := s.Named(t.Context(), 1, 5); err != nil || named {
		t.Fatalf("Named on an empty log = %v, %v", named, err)
	}

	if err := s.MarkNamed(t.Context(), store.Naming{
		AthleteID: 1, ActivityID: 5,
		Title: "The Pink Panther Strikes Again", Language: "en",
		Source: store.SourceService, At: Now,
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	title, named, err := s.Named(t.Context(), 1, 5)
	if err != nil {
		t.Fatalf("Named: %v", err)
	}
	if !named || title != "The Pink Panther Strikes Again" {
		t.Errorf("Named = %q, %v", title, named)
	}
}

func namedLogKeyedByAthlete(t *testing.T, s store.Store) {
	if err := s.MarkNamed(t.Context(), store.Naming{
		AthleteID: 1, ActivityID: 5, Title: "title", At: Now,
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	if _, named, _ := s.Named(t.Context(), 2, 5); named {
		t.Error("the named log leaked across athletes")
	}
}

func geocodeCacheRoundTrip(t *testing.T, s store.Store) {
	place := store.Place{Name: "Musterdorf", Kind: "village", Region: "Musterregion", Country: "Testland"}

	if err := s.SavePlace(t.Context(), "0.000,0.000", place); err != nil {
		t.Fatalf("SavePlace: %v", err)
	}

	cached, ok, err := s.Place(t.Context(), "0.000,0.000")
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if !ok {
		t.Fatal("Place reported a miss for a key just written")
	}

	if cached != place {
		t.Errorf("cached = %+v, want %+v", cached, place)
	}
}

func geocodeCacheMissIsNotAnErr(t *testing.T, s store.Store) {
	place, ok, err := s.Place(t.Context(), "unseen")
	if err != nil {
		t.Fatalf("Place on a miss = %v, want nil error", err)
	}
	if ok || !place.Empty() {
		t.Errorf("Place on a miss = %+v, %v", place, ok)
	}
}

func ids(pending []store.Pending) []int64 {
	out := make([]int64, len(pending))
	for i, p := range pending {
		out[i] = p.ActivityID
	}

	return out
}

// An unused franchise, and one that no longer exists in configuration, both
// answer zero. Removing a franchise from config should stop it being
// consulted, not start producing errors.
func franchiseStartsAtZero(t *testing.T, s store.Store) {
	t.Helper()

	position, err := s.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 0 {
		t.Errorf("position = %d, want 0 for a franchise never used", position)
	}

	if _, err := s.FranchisePosition(t.Context(), 4242, "a-franchise-that-was-removed"); err != nil {
		t.Errorf("FranchisePosition for an unknown franchise = %v, want no error", err)
	}
}

// The store decides the next number, so two callers cannot land on the same
// position and reuse a title.
func franchiseAdvances(t *testing.T, s store.Store) {
	t.Helper()

	for want := 1; want <= 3; want++ {
		got, err := s.AdvanceFranchise(t.Context(), 4242, "pink-panther")
		if err != nil {
			t.Fatalf("AdvanceFranchise: %v", err)
		}

		if got != want {
			t.Fatalf("AdvanceFranchise returned %d, want %d", got, want)
		}
	}

	position, err := s.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 3 {
		t.Errorf("position after three advances = %d, want 3", position)
	}
}

// Two franchises for one athlete are separate series.
func franchisesAreIndependent(t *testing.T, s store.Store) {
	t.Helper()

	if _, err := s.AdvanceFranchise(t.Context(), 4242, "pink-panther"); err != nil {
		t.Fatalf("AdvanceFranchise: %v", err)
	}

	other, err := s.FranchisePosition(t.Context(), 4242, "bond")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if other != 0 {
		t.Errorf("advancing one franchise moved another: %d", other)
	}
}

// Everything here is keyed by athlete, so a second athlete walks the same
// series from the start.
func franchiseKeyedByAthlete(t *testing.T, s store.Store) {
	t.Helper()

	if _, err := s.AdvanceFranchise(t.Context(), 4242, "pink-panther"); err != nil {
		t.Fatalf("AdvanceFranchise: %v", err)
	}

	other, err := s.FranchisePosition(t.Context(), 9999, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if other != 0 {
		t.Errorf("another athlete inherited a franchise position: %d", other)
	}
}

// A franchise name is configuration, so it is whatever a person typed.
//
// The one this service ships with is "Pink Panther": a space, which is not a
// character every backing store can put in a key. Names must not need
// sanitizing by the caller, and two different names must never share a
// position — that would hand out the same title twice.
func franchiseNamesAreArbitrary(t *testing.T, s store.Store) {
	t.Helper()

	names := []string{
		"Pink Panther",
		"Herr der Ringe / LOTR",
		"__proto__",
		"..",
		"Ocean's Eleven",
		"Über-Runde",
	}

	for _, name := range names {
		position, err := s.AdvanceFranchise(t.Context(), 4242, name)
		if err != nil {
			t.Fatalf("AdvanceFranchise(%q): %v", name, err)
		}

		if position != 1 {
			t.Errorf("AdvanceFranchise(%q) = %d, want 1", name, position)
		}
	}

	// Advancing one must not move any of the others.
	if _, err := s.AdvanceFranchise(t.Context(), 4242, names[0]); err != nil {
		t.Fatalf("second AdvanceFranchise: %v", err)
	}

	for index, name := range names {
		want := 1
		if index == 0 {
			want = 2
		}

		got, err := s.FranchisePosition(t.Context(), 4242, name)
		if err != nil {
			t.Fatalf("FranchisePosition(%q): %v", name, err)
		}

		if got != want {
			t.Errorf("FranchisePosition(%q) = %d, want %d: two names share a position",
				name, got, want)
		}
	}
}

// The title history reads newest first, which is the order the prompt wants:
// the most recent titles are the ones a new one must not repeat.
func recentTitlesNewestFirst(t *testing.T, s store.Store) {
	t.Helper()

	for index, title := range []string{"oldest", "middle", "newest"} {
		if err := s.MarkNamed(t.Context(), store.Naming{
			AthleteID:  7,
			ActivityID: int64(100 + index),
			Title:      title,
			Language:   "de",
			Source:     store.SourceService,
			At:         Now.Add(time.Duration(index) * time.Hour),
		}); err != nil {
			t.Fatalf("MarkNamed(%q): %v", title, err)
		}
	}

	titles, err := s.RecentTitles(t.Context(), 7, 10)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	got := make([]string, 0, len(titles))
	for _, entry := range titles {
		got = append(got, entry.Title)
	}

	want := []string{"newest", "middle", "oldest"}
	if !slices.Equal(got, want) {
		t.Errorf("RecentTitles = %v, want %v", got, want)
	}

	// The language and the source round-trip. Neither can be recovered from
	// Strava later, so losing them here loses them for good.
	if len(titles) > 0 && titles[0].Language != "de" {
		t.Errorf("language = %q, want %q", titles[0].Language, "de")
	}

	if len(titles) > 0 && titles[0].Source != store.SourceService {
		t.Errorf("source = %q, want %q", titles[0].Source, store.SourceService)
	}
}

// One athlete's history is not another's.
func recentTitlesKeyedByAthlete(t *testing.T, s store.Store) {
	t.Helper()

	if err := s.MarkNamed(t.Context(), store.Naming{
		AthleteID: 8, ActivityID: 200, Title: "theirs", At: Now,
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	titles, err := s.RecentTitles(t.Context(), 9, 10)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	if len(titles) != 0 {
		t.Errorf("RecentTitles for another athlete returned %d entries", len(titles))
	}
}

// The limit is honored, and a limit of zero means nothing rather than
// everything — an unbounded read grows with the athlete's riding.
func recentTitlesBounded(t *testing.T, s store.Store) {
	t.Helper()

	for index := range 5 {
		if err := s.MarkNamed(t.Context(), store.Naming{
			AthleteID:  10,
			ActivityID: int64(300 + index),
			Title:      "title",
			At:         Now.Add(time.Duration(index) * time.Minute),
		}); err != nil {
			t.Fatalf("MarkNamed: %v", err)
		}
	}

	titles, err := s.RecentTitles(t.Context(), 10, 2)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	if len(titles) != 2 {
		t.Errorf("RecentTitles(limit 2) returned %d entries", len(titles))
	}

	for _, limit := range []int{0, -1} {
		titles, err := s.RecentTitles(t.Context(), 10, limit)
		if err != nil {
			t.Fatalf("RecentTitles(limit %d): %v", limit, err)
		}

		if len(titles) != 0 {
			t.Errorf("RecentTitles(limit %d) returned %d entries, want none", limit, len(titles))
		}
	}
}

// Values the configuration cases assert on in more than one place, named so
// the expectation and the fixture cannot drift apart.
const (
	testGravelRide   = "GravelRide"
	testFirstEntry   = "The Pink Panther"
	testFranchiseKey = "pink-panther"
)

// testFranchises is a configuration document with one ordered series.
func testFranchises() store.AthleteConfig {
	return store.AthleteConfig{
		Franchises: []store.Franchise{
			{
				Name:       testFranchiseKey,
				SportTypes: []string{testGravelRide, "Ride"},
				GearName:   "Pink Panther",
				Titles:     []string{testFirstEntry, "A Shot in the Dark"},
			},
		},
	}
}

// A configuration document survives the round trip with every field intact.
func athleteConfigRoundTrip(t *testing.T, s store.Store) {
	t.Helper()

	want := testFranchises()

	if err := s.SaveAthleteConfig(t.Context(), 20, want); err != nil {
		t.Fatalf("SaveAthleteConfig: %v", err)
	}

	got, ok, err := s.AthleteConfig(t.Context(), 20)
	if err != nil || !ok {
		t.Fatalf("AthleteConfig = %v, %v", ok, err)
	}

	if len(got.Franchises) != 1 {
		t.Fatalf("%d franchises, want 1", len(got.Franchises))
	}

	franchise := got.Franchises[0]
	wanted := want.Franchises[0]

	if franchise.Name != wanted.Name || franchise.GearName != wanted.GearName {
		t.Errorf("franchise = %+v, want %+v", franchise, wanted)
	}

	if !slices.Equal(franchise.SportTypes, wanted.SportTypes) {
		t.Errorf("sport types = %v, want %v", franchise.SportTypes, wanted.SportTypes)
	}

	// Order is the whole point of a series.
	if !slices.Equal(franchise.Titles, wanted.Titles) {
		t.Errorf("titles = %v, want %v", franchise.Titles, wanted.Titles)
	}
}

// An athlete with no document is not an error: it is every deployment on its
// first run, and the caller falls back to its own defaults.
func athleteConfigAbsent(t *testing.T, s store.Store) {
	t.Helper()

	config, ok, err := s.AthleteConfig(t.Context(), 21)
	if err != nil {
		t.Fatalf("AthleteConfig for an athlete with none = %v", err)
	}

	if ok {
		t.Errorf("reported a configuration that was never written: %+v", config)
	}

	if len(config.Franchises) != 0 {
		t.Errorf("returned %d franchises with no document", len(config.Franchises))
	}
}

// One athlete's configuration is not another's.
func athleteConfigKeyedByAthlete(t *testing.T, s store.Store) {
	t.Helper()

	if err := s.SaveAthleteConfig(t.Context(), 22, testFranchises()); err != nil {
		t.Fatalf("SaveAthleteConfig: %v", err)
	}

	if _, ok, _ := s.AthleteConfig(t.Context(), 23); ok {
		t.Error("configuration leaked across athletes")
	}
}

// Saving replaces rather than merges, so removing a franchise removes it.
func athleteConfigReplaces(t *testing.T, s store.Store) {
	t.Helper()

	if err := s.SaveAthleteConfig(t.Context(), 24, testFranchises()); err != nil {
		t.Fatalf("SaveAthleteConfig: %v", err)
	}

	if err := s.SaveAthleteConfig(t.Context(), 24, store.AthleteConfig{}); err != nil {
		t.Fatalf("SaveAthleteConfig: %v", err)
	}

	got, ok, err := s.AthleteConfig(t.Context(), 24)
	if err != nil || !ok {
		t.Fatalf("AthleteConfig = %v, %v", ok, err)
	}

	if len(got.Franchises) != 0 {
		t.Errorf("%d franchises after replacing with an empty document", len(got.Franchises))
	}
}

// What a caller keeps is not what the store keeps.
//
// The Firestore implementation decodes afresh every read, so the in-memory one
// has to copy too — otherwise a caller mutating the slice it saved would
// silently reorder a stored series, and only one of the two stores would do it.
func athleteConfigIsCopied(t *testing.T, s store.Store) {
	t.Helper()

	config := testFranchises()

	if err := s.SaveAthleteConfig(t.Context(), 25, config); err != nil {
		t.Fatalf("SaveAthleteConfig: %v", err)
	}

	// Reach into the value that was handed to the store. Every slice, not
	// just the one: a store that copies Titles and aliases SportTypes passes
	// a test that only checks Titles.
	config.Franchises[0].Titles[0] = "Mutated After Saving"
	config.Franchises[0].SportTypes[0] = "MutatedSport"
	config.Franchises[0].Name = "mutated"

	got, ok, err := s.AthleteConfig(t.Context(), 25)
	if err != nil || !ok {
		t.Fatalf("AthleteConfig = %v, %v", ok, err)
	}

	if got.Franchises[0].Titles[0] != testFirstEntry {
		t.Errorf("a caller's later edit reached the stored series: %q",
			got.Franchises[0].Titles[0])
	}

	if got.Franchises[0].SportTypes[0] != testGravelRide {
		t.Errorf("a caller's later edit reached the stored sport types: %q",
			got.Franchises[0].SportTypes[0])
	}

	if got.Franchises[0].Name != testFranchiseKey {
		t.Errorf("a caller's later edit reached the stored name: %q", got.Franchises[0].Name)
	}

	// And the other boundary: what a reader is given is theirs to change.
	// Firestore hands back a fresh decode every time, so aliasing on the way
	// out would make one implementation mutable through its readers and the
	// other not.
	got.Franchises[0].Titles[0] = "Mutated After Reading"
	got.Franchises[0].SportTypes[0] = "MutatedSport"

	again, ok, err := s.AthleteConfig(t.Context(), 25)
	if err != nil || !ok {
		t.Fatalf("AthleteConfig = %v, %v", ok, err)
	}

	if again.Franchises[0].Titles[0] != testFirstEntry {
		t.Errorf("a reader's edit reached the stored series: %q", again.Franchises[0].Titles[0])
	}

	if again.Franchises[0].SportTypes[0] != testGravelRide {
		t.Errorf("a reader's edit reached the stored sport types: %q",
			again.Franchises[0].SportTypes[0])
	}
}
