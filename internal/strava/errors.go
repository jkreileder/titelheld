package strava

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

var (
	// ErrDryRun is returned instead of performing a write while the client is
	// not in [WriteModeEnabled]. It is the guard that keeps a development or
	// misconfigured deployment from touching the athlete's activities.
	ErrDryRun = errors.New("strava: writes disabled (dry run)")

	// ErrIncompleteToken means Strava answered 200 without both tokens.
	ErrIncompleteToken = errors.New("strava: token response missing access or refresh token")

	// ErrRateLimited means the request kept getting 429 after every retry.
	ErrRateLimited = errors.New("strava: rate limited")

	// ErrNotFound means Strava has no such activity, or it is not visible with
	// the granted scopes.
	ErrNotFound = errors.New("strava: not found")

	// ErrUnauthorized means the access token was rejected.
	ErrUnauthorized = errors.New("strava: unauthorized")
)

// StatusError carries an unexpected HTTP status.
//
// Response bodies are deliberately not included: Strava echoes request
// parameters in some error payloads, and the token endpoint's parameters
// include the client secret.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

// Is lets errors.Is map the common statuses onto the sentinels above.
func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	default:
		return false
	}
}

// tokenError marks a failure to obtain an access token, so the transport can
// tell it apart from a transient network problem and stop retrying.
type tokenError struct {
	err error
}

func (e *tokenError) Error() string { return e.err.Error() }
func (e *tokenError) Unwrap() error { return e.err }

func statusError(code int) error {
	return &StatusError{StatusCode: code}
}

// drainAndClose releases the connection back to the pool.
func drainAndClose(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	_ = response.Body.Close()
}
