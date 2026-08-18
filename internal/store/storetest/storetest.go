// Package storetest holds the conformance suite every [store.Store]
// implementation must pass.
//
// It lives in its own package, rather than in a _test.go file, so the in-memory
// and Firestore implementations can be held to exactly the same assertions.
// "Same semantics as the memory store" is otherwise a claim nobody checks.
package storetest

import (
	"errors"
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
		"TokenRoundTrip":             tokenRoundTrip,
		"TokenRotationReplaces":      tokenRotationReplaces,
		"TokenMissingIsTyped":        tokenMissingIsTyped,
		"TokensKeyedByAthlete":       tokensKeyedByAthlete,
		"EnqueueIsIdempotent":        enqueueIsIdempotent,
		"DueRespectsTheDeadline":     dueRespectsTheDeadline,
		"DueIsOrderedOldestFirst":    dueIsOrderedOldestFirst,
		"DueBreaksTiesConsistently":  dueBreaksTiesConsistently,
		"RemoveIsForgiving":          removeIsForgiving,
		"QueueKeyedByAthlete":        queueKeyedByAthlete,
		"NamedLogRoundTrip":          namedLogRoundTrip,
		"NamedLogKeyedByAthlete":     namedLogKeyedByAthlete,
		"GeocodeCacheRoundTrip":      geocodeCacheRoundTrip,
		"GeocodeCacheMissIsNotAnErr": geocodeCacheMissIsNotAnErr,
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

	if err := s.MarkNamed(t.Context(), 1, 5, "The Pink Panther Strikes Again"); err != nil {
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
	if err := s.MarkNamed(t.Context(), 1, 5, "title"); err != nil {
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
