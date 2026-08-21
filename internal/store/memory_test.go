package store

import (
	"errors"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/strava"
)

var testNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

// The in-memory store must satisfy every interface the service depends on.
var (
	_ strava.TokenStore = (*Memory)(nil)
	_ Queue             = (*Memory)(nil)
	_ NamedLog          = (*Memory)(nil)
)

func TestTokenRoundTrip(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	if _, err := memory.Load(t.Context(), 4242); !errors.Is(err, strava.ErrTokenNotFound) {
		t.Fatalf("Load on an empty store = %v, want ErrTokenNotFound", err)
	}

	token := strava.Token{
		AthleteID:    4242,
		AccessToken:  "access-one",
		RefreshToken: "refresh-one",
		ExpiresAt:    testNow.Add(6 * time.Hour),
		Scopes:       []string{strava.ScopeActivityReadAll, strava.ScopeActivityWrite},
	}

	if err := memory.Save(t.Context(), token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := memory.Load(t.Context(), 4242)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.RefreshToken != "refresh-one" || loaded.AthleteID != 4242 {
		t.Errorf("loaded = %+v", loaded)
	}

	// A rotated pair replaces the previous one rather than accumulating.
	token.RefreshToken = "refresh-two"
	if err := memory.Save(t.Context(), token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, _ = memory.Load(t.Context(), 4242)
	if loaded.RefreshToken != "refresh-two" {
		t.Errorf("RefreshToken = %q, want the rotated value", loaded.RefreshToken)
	}
}

func TestTokensAreKeyedByAthlete(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	for _, id := range []int64{1, 2} {
		if err := memory.Save(t.Context(), strava.Token{AthleteID: id, AccessToken: "a"}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	if _, err := memory.Load(t.Context(), 1); err != nil {
		t.Errorf("Load(1): %v", err)
	}
	if _, err := memory.Load(t.Context(), 3); !errors.Is(err, strava.ErrTokenNotFound) {
		t.Errorf("Load(3) = %v, want ErrTokenNotFound", err)
	}
}

func TestAnyToken(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	if _, err := memory.AnyToken(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("AnyToken on an empty store = %v, want ErrNotFound", err)
	}

	if err := memory.Save(t.Context(), strava.Token{AthleteID: 7}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	token, err := memory.AnyToken(t.Context())
	if err != nil {
		t.Fatalf("AnyToken: %v", err)
	}
	if token.AthleteID != 7 {
		t.Errorf("AthleteID = %d", token.AthleteID)
	}

	// With more than one athlete there is no single answer.
	if err := memory.Save(t.Context(), strava.Token{AthleteID: 8}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := memory.AnyToken(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("AnyToken with two athletes = %v, want ErrNotFound", err)
	}
}

func TestEnqueueIsIdempotent(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	pending := Pending{
		AthleteID:    4242,
		ActivityID:   19755622151,
		Aspect:       "create",
		EnqueuedAt:   testNow,
		ProcessAfter: testNow.Add(10 * time.Minute),
	}

	added, err := memory.Enqueue(t.Context(), pending)
	if err != nil || !added {
		t.Fatalf("Enqueue = %v, %v; want true, nil", added, err)
	}

	// A repeated webhook delivery, or the update event caused by another tool,
	// must collapse into the one queued unit of work.
	pending.Aspect = "update"

	added, err = memory.Enqueue(t.Context(), pending)
	if err != nil || added {
		t.Fatalf("second Enqueue = %v, %v; want false, nil", added, err)
	}

	count, err := memory.Len(t.Context())
	if err != nil || count != 1 {
		t.Fatalf("Len = %d, %v; want 1, nil", count, err)
	}
}

func TestDueReturnsOnlyElapsedEntriesOldestFirst(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	entries := []Pending{
		{AthleteID: 1, ActivityID: 300, ProcessAfter: testNow.Add(5 * time.Minute)},
		{AthleteID: 1, ActivityID: 100, ProcessAfter: testNow.Add(-10 * time.Minute)},
		{AthleteID: 1, ActivityID: 200, ProcessAfter: testNow.Add(-time.Minute)},
	}

	for _, entry := range entries {
		if _, err := memory.Enqueue(t.Context(), entry); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	due, err := memory.Due(t.Context(), testNow)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	if len(due) != 2 {
		t.Fatalf("Due returned %d entries, want 2", len(due))
	}
	if due[0].ActivityID != 100 || due[1].ActivityID != 200 {
		t.Errorf("Due order = %d, %d; want 100, 200", due[0].ActivityID, due[1].ActivityID)
	}
}

func TestDueIsStableForIdenticalDeadlines(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	for _, id := range []int64{300, 100, 200} {
		if _, err := memory.Enqueue(t.Context(), Pending{
			AthleteID: 1, ActivityID: id, ProcessAfter: testNow,
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	due, err := memory.Due(t.Context(), testNow)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	for i, want := range []int64{100, 200, 300} {
		if due[i].ActivityID != want {
			t.Fatalf("Due = %v, want ascending activity IDs", due)
		}
	}
}

func TestPendingDueBoundary(t *testing.T) {
	t.Parallel()

	pending := Pending{ProcessAfter: testNow}

	if !pending.Due(testNow) {
		t.Error("an entry is due at exactly its deadline")
	}
	if pending.Due(testNow.Add(-time.Nanosecond)) {
		t.Error("an entry is not due before its deadline")
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	if _, err := memory.Enqueue(t.Context(), Pending{AthleteID: 1, ActivityID: 5}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := memory.Remove(t.Context(), 1, 5); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	count, _ := memory.Len(t.Context())
	if count != 0 {
		t.Errorf("Len = %d, want 0", count)
	}

	// Removing something that is not there is not an error.
	if err := memory.Remove(t.Context(), 1, 5); err != nil {
		t.Errorf("Remove of a missing entry = %v, want nil", err)
	}
}

func TestQueueIsKeyedByAthleteAndActivity(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	for _, athlete := range []int64{1, 2} {
		added, err := memory.Enqueue(t.Context(), Pending{AthleteID: athlete, ActivityID: 5})
		if err != nil || !added {
			t.Fatalf("Enqueue(%d) = %v, %v", athlete, added, err)
		}
	}

	count, _ := memory.Len(t.Context())
	if count != 2 {
		t.Errorf("Len = %d, want 2 — the same activity ID for two athletes is two entries", count)
	}
}

func TestNamedLog(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	title, named, err := memory.Named(t.Context(), 1, 5)
	if err != nil {
		t.Fatalf("Named: %v", err)
	}
	if named || title != "" {
		t.Errorf("Named on an empty log = %q, %v", title, named)
	}

	if err := memory.MarkNamed(t.Context(), Naming{
		AthleteID: 1, ActivityID: 5,
		Title: "The Pink Panther Strikes Again", At: time.Now(),
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	title, named, err = memory.Named(t.Context(), 1, 5)
	if err != nil {
		t.Fatalf("Named: %v", err)
	}
	if !named || title != "The Pink Panther Strikes Again" {
		t.Errorf("Named = %q, %v", title, named)
	}

	// Another athlete's activity with the same ID is a different record.
	if _, named, _ := memory.Named(t.Context(), 2, 5); named {
		t.Error("the named log leaked across athletes")
	}
}

func TestMemoryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	done := make(chan struct{})

	for worker := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()

			for i := range 50 {
				id := int64(worker*50 + i)

				_, _ = memory.Enqueue(t.Context(), Pending{AthleteID: 1, ActivityID: id})
				_ = memory.MarkNamed(t.Context(), Naming{
					AthleteID: 1, ActivityID: id, Title: "title", At: time.Now(),
				})
				_, _, _ = memory.Named(t.Context(), 1, id)
				_, _ = memory.Due(t.Context(), testNow)
				_ = memory.Save(t.Context(), strava.Token{AthleteID: 1})
				_, _ = memory.Load(t.Context(), 1)
			}
		}()
	}

	for range 8 {
		<-done
	}

	count, _ := memory.Len(t.Context())
	if count != 400 {
		t.Errorf("Len = %d, want 400", count)
	}
}

func TestByDeadline(t *testing.T) {
	t.Parallel()

	early := Pending{ActivityID: 1, ProcessAfter: testNow}
	late := Pending{ActivityID: 2, ProcessAfter: testNow.Add(time.Minute)}

	if got := byDeadline(early, late); got != -1 {
		t.Errorf("byDeadline(early, late) = %d, want -1", got)
	}
	if got := byDeadline(late, early); got != 1 {
		t.Errorf("byDeadline(late, early) = %d, want 1", got)
	}

	sameTimeLowID := Pending{ActivityID: 10, ProcessAfter: testNow}
	sameTimeHighID := Pending{ActivityID: 20, ProcessAfter: testNow}

	if got := byDeadline(sameTimeLowID, sameTimeHighID); got >= 0 {
		t.Errorf("byDeadline for equal deadlines = %d, want the lower ID first", got)
	}
	if got := byDeadline(sameTimeLowID, sameTimeLowID); got != 0 {
		t.Errorf("byDeadline of an entry with itself = %d, want 0", got)
	}
}

func TestMemoryGeocodeCache(t *testing.T) {
	t.Parallel()

	memory := NewMemory()

	if _, ok, err := memory.Place(t.Context(), "0.000,0.000"); ok || err != nil {
		t.Fatalf("Place on an empty cache = %v, %v", ok, err)
	}

	place := Place{Name: "Musterdorf", Kind: "village", Region: "Musterregion", Country: "Testland"}
	if err := memory.SavePlace(t.Context(), "0.000,0.000", place); err != nil {
		t.Fatalf("SavePlace: %v", err)
	}

	cached, ok, err := memory.Place(t.Context(), "0.000,0.000")
	if err != nil || !ok || cached != place {
		t.Errorf("Place = %+v, %v, %v", cached, ok, err)
	}
}

func TestPlaceEmpty(t *testing.T) {
	t.Parallel()

	if !(Place{}).Empty() {
		t.Error("a zero Place must report as empty")
	}
	if (Place{Country: "Testland"}).Empty() {
		t.Error("a Place with a country is not empty")
	}
}
