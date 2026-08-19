package strava

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPIBaseURL is Strava's REST API root.
const DefaultAPIBaseURL = "https://www.strava.com/api/v3"

// WriteMode controls whether the client may modify anything on Strava.
//
// The zero value is [WriteModeDryRun]. That is deliberate: a Client built from
// a zero-valued config, or a caller that forgets to set the field, gets the
// safe behavior rather than the destructive one.
type WriteMode int

const (
	// WriteModeDryRun refuses every write with [ErrDryRun]. Default.
	WriteModeDryRun WriteMode = iota

	// WriteModeEnabled permits writes. Only ever set from an explicit,
	// unambiguous configuration value.
	WriteModeEnabled
)

// String renders the mode for structured logs.
func (m WriteMode) String() string {
	if m == WriteModeEnabled {
		return "enabled"
	}

	return "dry_run"
}

// RateLimit is the quota state Strava reports on every response. Strava sends
// each figure as "fifteen-minute,daily".
type RateLimit struct {
	ShortLimit int
	DailyLimit int
	ShortUsage int
	DailyUsage int

	ReadShortLimit int
	ReadDailyLimit int
	ReadShortUsage int
	ReadDailyUsage int
}

// ClientConfig configures a [Client].
type ClientConfig struct {
	// Tokens supplies access tokens. Required.
	Tokens TokenProvider

	// WriteMode defaults to the safe [WriteModeDryRun].
	WriteMode WriteMode

	// BaseURL defaults to [DefaultAPIBaseURL].
	BaseURL string

	// HTTPClient defaults to a client with a 30 second timeout.
	HTTPClient *http.Client

	// MaxRetries bounds retries of 429 and 5xx responses. Defaults to 3.
	MaxRetries int

	// Sleep waits or returns the context error. Injected for tests.
	Sleep func(ctx context.Context, d time.Duration) error

	// UserAgent identifies this service to Strava.
	UserAgent string
}

// Client talks to Strava's REST API.
type Client struct {
	tokens     TokenProvider
	writeMode  WriteMode
	baseURL    string
	httpClient *http.Client
	maxRetries int
	sleep      func(ctx context.Context, d time.Duration) error
	userAgent  string

	mu        sync.Mutex
	rateLimit RateLimit
}

// NewClient builds a client. Tokens is required.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Tokens == nil {
		return nil, errors.New("strava: ClientConfig.Tokens is required")
	}

	client := &Client{
		tokens:     cfg.Tokens,
		writeMode:  cfg.WriteMode,
		baseURL:    strings.TrimSuffix(cfg.BaseURL, "/"),
		httpClient: cfg.HTTPClient,
		maxRetries: cfg.MaxRetries,
		sleep:      cfg.Sleep,
		userAgent:  cfg.UserAgent,
	}

	if client.baseURL == "" {
		client.baseURL = DefaultAPIBaseURL
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if client.maxRetries == 0 {
		client.maxRetries = 3
	}
	if client.sleep == nil {
		client.sleep = sleepContext
	}
	if client.userAgent == "" {
		client.userAgent = "titelheld"
	}

	return client, nil
}

// WriteMode reports whether this client may write.
func (c *Client) WriteMode() WriteMode {
	return c.writeMode
}

// RateLimit returns the quota state from the most recent response.
func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.rateLimit
}

// do performs a request with authorization, retries and rate-limit accounting.
//
// It is unexported on purpose: [UpdateActivityName] is the only method that
// builds a mutating request, so keeping the transport private means there is no
// exported path to a PUT that skips the write guard. The check below is a
// second, structural line of defense for any future method.
func (c *Client) do(ctx context.Context, method, path string, body func() (*strings.Reader, string)) (*http.Response, error) {
	if method != http.MethodGet && c.writeMode != WriteModeEnabled {
		return nil, fmt.Errorf("strava: refusing %s %s: %w", method, path, ErrDryRun)
	}

	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}

		response, err := c.attempt(ctx, method, path, body)
		if err != nil {
			lastErr = err

			// A token that cannot be obtained will not become obtainable by
			// asking again, and each retry costs another /oauth/token round
			// trip against a refresh token Strava has already rejected.
			var tokenErr *tokenError
			if errors.As(err, &tokenErr) {
				return nil, tokenErr.err
			}

			// A transport error is worth one more try; a context that is done
			// is not.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			continue
		}

		if response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError {
			lastErr = statusError(response.StatusCode)

			drainAndClose(response)

			continue
		}

		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			defer drainAndClose(response)

			return nil, statusError(response.StatusCode)
		}

		return response, nil
	}

	return nil, fmt.Errorf("strava: %s %s failed after %d attempts: %w",
		method, path, c.maxRetries+1, lastErr)
}

func (c *Client) attempt(ctx context.Context, method, path string, body func() (*strings.Reader, string)) (*http.Response, error) {
	var (
		reader      *strings.Reader
		contentType string
	)

	if body != nil {
		reader, contentType = body()
	}

	var request *http.Request

	var err error

	if reader != nil {
		request, err = http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	} else {
		request, err = http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	}

	if err != nil {
		return nil, fmt.Errorf("strava: build request: %w", err)
	}

	token, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return nil, &tokenError{err: err}
	}

	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("strava: %s %s: %w", method, path, err)
	}

	c.recordRateLimit(response.Header)

	return response, nil
}

// recordRateLimit stores the quota figures Strava reports on every response.
func (c *Client) recordRateLimit(header http.Header) {
	limit := RateLimit{}
	limit.ShortLimit, limit.DailyLimit = parsePair(header.Get("X-RateLimit-Limit"))
	limit.ShortUsage, limit.DailyUsage = parsePair(header.Get("X-RateLimit-Usage"))
	limit.ReadShortLimit, limit.ReadDailyLimit = parsePair(header.Get("X-ReadRateLimit-Limit"))
	limit.ReadShortUsage, limit.ReadDailyUsage = parsePair(header.Get("X-ReadRateLimit-Usage"))

	if limit == (RateLimit{}) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.rateLimit = limit
}

// parsePair splits Strava's "fifteen-minute,daily" header values.
func parsePair(value string) (int, int) {
	short, daily, found := strings.Cut(value, ",")
	if !found {
		return 0, 0
	}

	shortValue, err := strconv.Atoi(strings.TrimSpace(short))
	if err != nil {
		return 0, 0
	}

	dailyValue, err := strconv.Atoi(strings.TrimSpace(daily))
	if err != nil {
		return shortValue, 0
	}

	return shortValue, dailyValue
}

// backoff is exponential with jitter. Strava documents no Retry-After on 429,
// and the quota window is 15 minutes, so retries exist to ride out a brief
// burst rather than to wait out a full window; the caller re-queues instead.
func backoff(attempt int) time.Duration {
	base := time.Duration(1<<uint(attempt-1)) * time.Second
	// Jitter only spreads retries out; it guards nothing, so a non-cryptographic
	// generator is the right tool.
	jitter := time.Duration(rand.Int64N(int64(base / 2))) //nolint:gosec // scheduling noise, not a secret

	return base + jitter
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
