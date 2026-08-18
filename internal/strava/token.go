// Package strava is the adapter for Strava's REST API: OAuth, the HTTP client,
// rate-limit handling, and the small set of calls this service makes.
//
// It is the only package allowed to talk to Strava. Core packages depend on the
// interfaces declared here, never on net/http.
package strava

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Scopes are the OAuth scopes this service needs: read every activity,
// including those marked "Only You", and write titles back.
const Scopes = "activity:read_all,activity:write"

// ScopeActivityReadAll and ScopeActivityWrite are the individual scopes, used to
// verify what the athlete actually granted.
const (
	ScopeActivityReadAll = "activity:read_all"
	ScopeActivityWrite   = "activity:write"
)

// ErrTokenNotFound is returned by a [TokenStore] that holds no token yet, which
// means the one-time authorization flow has not been completed.
var ErrTokenNotFound = errors.New("strava: no stored token")

// Token is an OAuth token pair for one athlete.
//
// Strava rotates the refresh token on every refresh and invalidates the
// previous one immediately, so RefreshToken is the single piece of state this
// service cannot afford to lose or to persist late.
type Token struct {
	AthleteID    int64
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scopes       []string
}

// Expired reports whether the access token is expired, or close enough to
// expiry that it should be refreshed before use.
func (t Token) Expired(now time.Time, leeway time.Duration) bool {
	if t.ExpiresAt.IsZero() {
		return true
	}

	return !now.Add(leeway).Before(t.ExpiresAt)
}

// HasScope reports whether the athlete granted the named scope.
func (t Token) HasScope(scope string) bool {
	return slices.Contains(t.Scopes, scope)
}

// MissingScopes returns the scopes this service needs that the token lacks.
func (t Token) MissingScopes() []string {
	var missing []string

	for _, scope := range []string{ScopeActivityReadAll, ScopeActivityWrite} {
		if !t.HasScope(scope) {
			missing = append(missing, scope)
		}
	}

	return missing
}

// TokenStore persists the OAuth token pair. It is declared here, in the package
// that consumes it, so implementations can live wherever the storage does.
type TokenStore interface {
	// Load returns the stored token for an athlete, or [ErrTokenNotFound].
	Load(ctx context.Context, athleteID int64) (Token, error)

	// Save writes the token, replacing any previous one for the same athlete.
	Save(ctx context.Context, token Token) error
}

// TokenProvider hands out a usable access token, refreshing as needed.
type TokenProvider interface {
	AccessToken(ctx context.Context) (string, error)
}
