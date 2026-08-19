package strava

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore is a minimal TokenStore whose behavior tests can steer.
type fakeStore struct {
	token    Token
	loadErr  error
	saveErr  error
	saves    atomic.Int64
	lastSave atomic.Value
}

func (f *fakeStore) Load(context.Context, int64) (Token, error) {
	if f.loadErr != nil {
		return Token{}, f.loadErr
	}

	return f.token, nil
}

func (f *fakeStore) Save(_ context.Context, token Token) error {
	if f.saveErr != nil {
		return f.saveErr
	}

	f.saves.Add(1)
	f.lastSave.Store(token)
	f.token = token

	return nil
}

func refreshServer(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		_, _ = w.Write([]byte(`{
			"access_token": "access-` + string(rune('0'+n)) + `",
			"refresh_token": "refresh-` + string(rune('0'+n)) + `",
			"expires_in": 21600
		}`))
	}))
}

func TestTokenSourceReturnsAValidTokenWithoutRefreshing(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := refreshServer(t, &calls)
	defer server.Close()

	store := &fakeStore{token: Token{
		AthleteID:    4242,
		AccessToken:  "still-good",
		RefreshToken: "refresh-one",
		ExpiresAt:    fixedNow.Add(time.Hour),
	}}

	source := NewStoredTokenSource(testOAuth(server), store, 4242)
	source.now = func() time.Time { return fixedNow }

	token, err := source.AccessToken(t.Context())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	if token != "still-good" {
		t.Errorf("AccessToken = %q", token)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("refresh calls = %d, want 0", got)
	}
}

func TestTokenSourceRefreshesAndPersistsBeforeReturning(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := refreshServer(t, &calls)
	defer server.Close()

	store := &fakeStore{token: Token{
		AthleteID:    4242,
		AccessToken:  "expired",
		RefreshToken: "refresh-one",
		ExpiresAt:    fixedNow.Add(-time.Minute),
		Scopes:       []string{ScopeActivityReadAll, ScopeActivityWrite},
	}}

	source := NewStoredTokenSource(testOAuth(server), store, 4242)
	source.now = func() time.Time { return fixedNow }

	token, err := source.Token(t.Context())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if token.AccessToken != "access-1" || token.RefreshToken != "refresh-1" {
		t.Errorf("token = %+v", token)
	}

	// The rotated pair must be persisted, or the old refresh token — already
	// invalidated by Strava — would be all that survives a restart.
	if got := store.saves.Load(); got != 1 {
		t.Fatalf("saves = %d, want 1", got)
	}

	saved, _ := store.lastSave.Load().(Token)
	if saved.RefreshToken != "refresh-1" {
		t.Errorf("persisted refresh token = %q, want the rotated one", saved.RefreshToken)
	}

	// Athlete ID and scopes are absent from a refresh response and must be
	// carried across rather than lost.
	if saved.AthleteID != 4242 {
		t.Errorf("persisted AthleteID = %d, want 4242", saved.AthleteID)
	}
	if !saved.HasScope(ScopeActivityWrite) {
		t.Errorf("persisted scopes = %v, want the previous grant carried across", saved.Scopes)
	}
}

func TestTokenSourceCachesAcrossCalls(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := refreshServer(t, &calls)
	defer server.Close()

	store := &fakeStore{token: Token{
		AthleteID:    4242,
		RefreshToken: "refresh-one",
		ExpiresAt:    fixedNow.Add(-time.Minute),
	}}

	source := NewStoredTokenSource(testOAuth(server), store, 4242)
	source.now = func() time.Time { return fixedNow }

	for range 3 {
		if _, err := source.AccessToken(t.Context()); err != nil {
			t.Fatalf("AccessToken: %v", err)
		}
	}

	// Each refreshed token lands six hours out, so only the first call
	// refreshes.
	if got := calls.Load(); got != 1 {
		t.Errorf("refresh calls = %d, want 1", got)
	}
}

func TestTokenSourceReportsMissingToken(t *testing.T) {
	t.Parallel()

	store := &fakeStore{loadErr: ErrTokenNotFound}

	source := NewStoredTokenSource(testOAuth(nil), store, 4242)

	if _, err := source.AccessToken(t.Context()); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("error = %v, want ErrTokenNotFound", err)
	}
}

func TestTokenSourceWithoutRefreshTokenCannotRefresh(t *testing.T) {
	t.Parallel()

	store := &fakeStore{token: Token{AthleteID: 4242, ExpiresAt: fixedNow.Add(-time.Hour)}}

	source := NewStoredTokenSource(testOAuth(nil), store, 4242)
	source.now = func() time.Time { return fixedNow }

	if _, err := source.Token(t.Context()); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("error = %v, want ErrTokenNotFound", err)
	}
}

func TestTokenSourceFailsWhenPersistFails(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := refreshServer(t, &calls)
	defer server.Close()

	saveErr := errors.New("firestore unavailable")
	store := &fakeStore{
		token: Token{
			AthleteID:    4242,
			RefreshToken: "refresh-one",
			ExpiresAt:    fixedNow.Add(-time.Minute),
		},
		saveErr: saveErr,
	}

	source := NewStoredTokenSource(testOAuth(server), store, 4242)
	source.now = func() time.Time { return fixedNow }

	// A refreshed pair that cannot be persisted must surface as an error: the
	// previous refresh token is already dead, so silently carrying on would
	// hide the fact that the service can no longer recover.
	if _, err := source.Token(t.Context()); !errors.Is(err, saveErr) {
		t.Fatalf("error = %v, want %v", err, saveErr)
	}
}

func TestTokenSourceReportsRefreshFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	store := &fakeStore{token: Token{
		AthleteID:    4242,
		RefreshToken: "refresh-one",
		ExpiresAt:    fixedNow.Add(-time.Minute),
	}}

	source := NewStoredTokenSource(testOAuth(server), store, 4242)
	source.now = func() time.Time { return fixedNow }

	if _, err := source.Token(t.Context()); err == nil {
		t.Fatal("Token = nil error, want the refresh failure")
	}
}

func TestTokenSourceFallsBackToConfiguredAthleteID(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := refreshServer(t, &calls)
	defer server.Close()

	store := &fakeStore{token: Token{
		RefreshToken: "refresh-one",
		ExpiresAt:    fixedNow.Add(-time.Minute),
	}}

	source := NewStoredTokenSource(testOAuth(server), store, 99)
	source.now = func() time.Time { return fixedNow }

	token, err := source.Token(t.Context())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if token.AthleteID != 99 {
		t.Errorf("AthleteID = %d, want the configured 99", token.AthleteID)
	}
}

func TestNewStoredTokenSourceUsesRealClock(t *testing.T) {
	t.Parallel()

	source := NewStoredTokenSource(testOAuth(nil), &fakeStore{}, 1)
	if source.now().IsZero() {
		t.Error("now() returned the zero time")
	}
}

// A refresh whose result cannot be persisted must not leave the source holding
// the previous refresh token: Strava invalidated that one the instant it issued
// the new pair, so keeping it would strand every later refresh.
func TestTokenSourceKeepsTheNewPairWhenPersistFails(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := refreshServer(t, &calls)
	defer server.Close()

	store := &fakeStore{
		token: Token{
			AthleteID:    4242,
			RefreshToken: "refresh-one",
			ExpiresAt:    fixedNow.Add(-time.Minute),
		},
		saveErr: errors.New("firestore unavailable"),
	}

	source := NewStoredTokenSource(testOAuth(server), store, 4242)
	source.now = func() time.Time { return fixedNow }

	// The write failure is reported...
	if _, err := source.Token(t.Context()); err == nil {
		t.Fatal("Token = nil error, want the persistence failure")
	}

	// ...but the live pair is retained, so the service can keep serving.
	source.mu.Lock()
	cached := source.cached
	source.mu.Unlock()

	if cached.RefreshToken != "refresh-1" {
		t.Fatalf("cached refresh token = %q, want the rotated one", cached.RefreshToken)
	}

	// The store recovers; the next call must persist the pair it is holding
	// rather than refreshing again against a token Strava already rejected.
	store.saveErr = nil

	token, err := source.Token(t.Context())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if token.RefreshToken != "refresh-1" {
		t.Errorf("RefreshToken = %q", token.RefreshToken)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("refresh calls = %d, want 1 — the second call must not re-refresh", got)
	}

	saved, _ := store.lastSave.Load().(Token)
	if saved.RefreshToken != "refresh-1" {
		t.Errorf("persisted refresh token = %q, want the retry to have written it", saved.RefreshToken)
	}
}
