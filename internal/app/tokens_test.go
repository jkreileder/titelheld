package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// realStore is a memory store that also answers AnyToken for real, unlike the
// stub used elsewhere in these tests.
type realStore struct{ *store.Memory }

func (r realStore) AnyToken(ctx context.Context) (strava.Token, error) {
	return r.Memory.AnyToken(ctx)
}

func boundToken(athleteID int64) strava.Token {
	return strava.Token{
		AthleteID:    athleteID,
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		Scopes:       []string{"activity:read_all", "activity:write"},
	}
}

// With no athlete configured, the sweep finds the one that authorized.
//
// This is the deployed configuration: STRAVA_ATHLETE_ID is unset and the
// service binds to whoever completes the OAuth flow. Without the fallback the
// sweep asks for athlete 0 — a document that never exists — and fails on every
// activity of every sweep.
func TestAnUnconfiguredAthleteResolvesToTheBoundOne(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	if err := memory.Save(t.Context(), boundToken(4242)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tokens := boundTokens{realStore{memory}}

	got, err := tokens.Load(t.Context(), 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.AthleteID != 4242 {
		t.Errorf("resolved athlete %d, want the bound one (4242)", got.AthleteID)
	}

	if got.AccessToken != "test-access-token" {
		t.Errorf("access token is %q, want the stored one", got.AccessToken)
	}
}

// A configured athlete is looked up directly, not through the fallback.
func TestAConfiguredAthleteIsLookedUpDirectly(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	if err := memory.Save(t.Context(), boundToken(4242)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tokens := boundTokens{realStore{memory}}

	if _, err := tokens.Load(t.Context(), 4242); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A different athlete is not silently served the bound one's token.
	if _, err := tokens.Load(t.Context(), 9999); !errors.Is(err, strava.ErrTokenNotFound) {
		t.Errorf("Load for an unknown athlete = %v, want ErrTokenNotFound", err)
	}
}

// With nobody bound yet, the sweep reports a missing token rather than
// guessing. The service starts before anyone has authorized, so this is the
// state it comes up in.
func TestNoBoundAthleteIsAMissingToken(t *testing.T) {
	t.Parallel()

	tokens := boundTokens{realStore{store.NewMemory()}}

	_, err := tokens.Load(t.Context(), 0)
	if !errors.Is(err, strava.ErrTokenNotFound) {
		t.Fatalf("Load = %v, want ErrTokenNotFound", err)
	}
}

// Two bound athletes is not a situation to pick a winner in.
func TestMoreThanOneBoundAthleteIsRefused(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	for _, id := range []int64{4242, 5353} {
		if err := memory.Save(t.Context(), boundToken(id)); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	tokens := boundTokens{realStore{memory}}

	if _, err := tokens.Load(t.Context(), 0); !errors.Is(err, strava.ErrTokenNotFound) {
		t.Errorf("Load with two bound athletes = %v, want ErrTokenNotFound", err)
	}
}

// Saving goes straight through, so a rotated refresh token is persisted under
// the athlete it belongs to rather than under zero.
func TestSavePassesThroughToTheStore(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	tokens := boundTokens{realStore{memory}}

	if err := tokens.Save(t.Context(), boundToken(4242)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := memory.Load(t.Context(), 4242)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.RefreshToken != "test-refresh-token" {
		t.Errorf("refresh token is %q, want the saved one", got.RefreshToken)
	}
}
