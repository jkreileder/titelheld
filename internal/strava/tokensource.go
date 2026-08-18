package strava

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// refreshLeeway is how long before expiry a token is refreshed anyway, so a
// request never sets off with a token that expires mid-flight.
const refreshLeeway = 5 * time.Minute

// StoredTokenSource hands out access tokens for one athlete, refreshing and
// persisting them as needed.
//
// The mutex serialises refreshes within the process. That is sufficient because
// the service runs with max-instances=1; two instances refreshing concurrently
// would race for the rotating refresh token, and the loser would hold a token
// Strava has already invalidated.
type StoredTokenSource struct {
	oauth     *OAuth
	store     TokenStore
	athleteID int64
	now       func() time.Time

	mu     sync.Mutex
	cached Token
	loaded bool
}

// NewStoredTokenSource builds a token source backed by store.
func NewStoredTokenSource(oauth *OAuth, store TokenStore, athleteID int64) *StoredTokenSource {
	return &StoredTokenSource{
		oauth:     oauth,
		store:     store,
		athleteID: athleteID,
		now:       time.Now,
	}
}

// AccessToken returns a token that is valid now, refreshing first if needed.
func (s *StoredTokenSource) AccessToken(ctx context.Context) (string, error) {
	token, err := s.Token(ctx)
	if err != nil {
		return "", err
	}

	return token.AccessToken, nil
}

// Token returns the current token, refreshing and persisting it if it is at or
// near expiry.
func (s *StoredTokenSource) Token(ctx context.Context) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.loaded {
		token, err := s.store.Load(ctx, s.athleteID)
		if err != nil {
			return Token{}, fmt.Errorf("strava: load token: %w", err)
		}

		s.cached = token
		s.loaded = true
	}

	if !s.cached.Expired(s.now(), refreshLeeway) {
		return s.cached, nil
	}

	return s.refreshLocked(ctx)
}

// refreshLocked exchanges the refresh token and persists the result.
//
// The new pair is written to the store *before* it is returned: Strava
// invalidates the previous refresh token the moment it issues a new one, so a
// pair that is used but not persisted would strand the service with no way back
// short of re-running the authorization flow by hand.
func (s *StoredTokenSource) refreshLocked(ctx context.Context) (Token, error) {
	if s.cached.RefreshToken == "" {
		return Token{}, ErrTokenNotFound
	}

	refreshed, err := s.oauth.Refresh(ctx, s.cached.RefreshToken)
	if err != nil {
		return Token{}, fmt.Errorf("strava: refresh token: %w", err)
	}

	// Strava's refresh response carries no athlete object, and the granted
	// scopes do not change on refresh, so both are carried across.
	if refreshed.AthleteID == 0 {
		refreshed.AthleteID = s.cached.AthleteID
	}
	if len(refreshed.Scopes) == 0 {
		refreshed.Scopes = s.cached.Scopes
	}
	if refreshed.AthleteID == 0 {
		refreshed.AthleteID = s.athleteID
	}

	if err := s.store.Save(ctx, refreshed); err != nil {
		return Token{}, fmt.Errorf("strava: persist refreshed token: %w", err)
	}

	s.cached = refreshed

	return refreshed, nil
}
