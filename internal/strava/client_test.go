package strava

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// staticTokens is a TokenProvider that never refreshes.
type staticTokens struct {
	token string
	err   error
}

func (s staticTokens) AccessToken(context.Context) (string, error) {
	return s.token, s.err
}

// newTestClient builds a client pointed at server with instant retries.
func newTestClient(t *testing.T, server *httptest.Server, mode WriteMode) *Client {
	t.Helper()

	client, err := NewClient(ClientConfig{
		Tokens:     staticTokens{token: "test-access-token"},
		WriteMode:  mode,
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	return client
}

func TestNewClientRequiresTokens(t *testing.T) {
	t.Parallel()

	if _, err := NewClient(ClientConfig{}); err == nil {
		t.Fatal("NewClient without Tokens = nil error, want error")
	}
}

func TestNewClientDefaultsToDryRun(t *testing.T) {
	t.Parallel()

	client, err := NewClient(ClientConfig{Tokens: staticTokens{token: "t"}})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if got := client.WriteMode(); got != WriteModeDryRun {
		t.Errorf("WriteMode of a zero-valued config = %v, want %v", got, WriteModeDryRun)
	}
}

// TestUpdateActivityNameRefusesInDryRun is the guard with teeth: with the write
// mode at its zero value, no request may reach Strava at all.
func TestUpdateActivityNameRefusesInDryRun(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	_, err := client.UpdateActivityName(t.Context(), 12345, "Kellerwinter, Woche 3")
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("UpdateActivityName error = %v, want ErrDryRun", err)
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("server saw %d requests, want 0 — dry run must not reach Strava", got)
	}
}

// TestTransportRefusesMutatingMethodsInDryRun covers the second, structural
// check: even a future method that builds a mutating request cannot get out.
func TestTransportRefusesMutatingMethodsInDryRun(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		//nolint:bodyclose // the guard returns before any response exists
		response, err := client.do(t.Context(), method, "/activities/1", nil)
		if response != nil {
			t.Errorf("do(%s) returned a response, want none", method)
		}

		if !errors.Is(err, ErrDryRun) {
			t.Errorf("do(%s) error = %v, want ErrDryRun", method, err)
		}
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("server saw %d requests, want 0", got)
	}
}

func TestUpdateActivityNameWritesWhenEnabled(t *testing.T) {
	t.Parallel()

	var (
		gotMethod atomic.Value
		gotPath   atomic.Value
		gotName   atomic.Value
		gotAuth   atomic.Value
		gotFields atomic.Int64
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}

		gotMethod.Store(r.Method)
		gotPath.Store(r.URL.Path)
		gotName.Store(r.PostForm.Get("name"))
		gotAuth.Store(r.Header.Get("Authorization"))
		gotFields.Store(int64(len(r.PostForm)))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":12345,"name":"Kellerwinter, Woche 3","athlete":{"id":7}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeEnabled)

	activity, err := client.UpdateActivityName(t.Context(), 12345, "Kellerwinter, Woche 3")
	if err != nil {
		t.Fatalf("UpdateActivityName: %v", err)
	}

	if got := gotMethod.Load(); got != http.MethodPut {
		t.Errorf("method = %v, want PUT", got)
	}
	if got := gotPath.Load(); got != "/activities/12345" {
		t.Errorf("path = %v, want /activities/12345", got)
	}
	if got := gotName.Load(); got != "Kellerwinter, Woche 3" {
		t.Errorf("name = %v", got)
	}
	if got := gotAuth.Load(); got != "Bearer test-access-token" {
		t.Errorf("Authorization = %v", got)
	}
	// Rename only: sport type, gear and description are never sent.
	if got := gotFields.Load(); got != 1 {
		t.Errorf("form carried %d fields, want exactly 1 (name)", got)
	}
	if activity.Name != "Kellerwinter, Woche 3" || activity.Owner() != 7 {
		t.Errorf("decoded activity = %+v", activity)
	}
}

func TestUpdateActivityNameRejectsEmptyTitle(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeEnabled)

	if _, err := client.UpdateActivityName(t.Context(), 1, "   "); err == nil {
		t.Error("UpdateActivityName with a blank title = nil error, want error")
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("server saw %d requests, want 0", got)
	}
}

func TestGetActivity(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activities/19755622151" {
			t.Errorf("path = %s", r.URL.Path)
		}

		w.Header().Set("X-RateLimit-Limit", "200,2000")
		w.Header().Set("X-RateLimit-Usage", "17,142")
		w.Header().Set("X-ReadRateLimit-Limit", "100,1000")
		w.Header().Set("X-ReadRateLimit-Usage", "9,88")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 19755622151,
			"name": "Afternoon Ride",
			"sport_type": "GravelRide",
			"distance": 67638.5,
			"moving_time": 10876,
			"trainer": false,
			"commute": false,
			"athlete": {"id": 4242},
			"start_latlng": [0.0005, 0.0005],
			"end_latlng": [0.0503, 0.0002]
		}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	activity, err := client.GetActivity(t.Context(), 19755622151)
	if err != nil {
		t.Fatalf("GetActivity: %v", err)
	}

	if activity.SportType != "GravelRide" || activity.Distance != 67638.5 {
		t.Errorf("activity = %+v", activity)
	}
	if activity.Owner() != 4242 {
		t.Errorf("Owner() = %d, want 4242", activity.Owner())
	}

	limit := client.RateLimit()
	want := RateLimit{
		ShortLimit: 200, DailyLimit: 2000, ShortUsage: 17, DailyUsage: 142,
		ReadShortLimit: 100, ReadDailyLimit: 1000, ReadShortUsage: 9, ReadDailyUsage: 88,
	}

	if limit != want {
		t.Errorf("RateLimit() = %+v, want %+v", limit, want)
	}
}

// Reads are permitted in dry run: only writes are suppressed.
func TestGetActivityAllowedInDryRun(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"athlete":{"id":2}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.GetActivity(t.Context(), 1); err != nil {
		t.Fatalf("GetActivity in dry run: %v", err)
	}
}

func TestRetriesRateLimitThenSucceeds(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		_, _ = w.Write([]byte(`{"id":1,"athlete":{"id":2}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.GetActivity(t.Context(), 1); err != nil {
		t.Fatalf("GetActivity: %v", err)
	}

	if got := calls.Load(); got != 3 {
		t.Errorf("server calls = %d, want 3", got)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{
		Tokens:     staticTokens{token: "t"},
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		MaxRetries: 2,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.GetActivity(t.Context(), 1)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}

	if got := calls.Load(); got != 3 {
		t.Errorf("server calls = %d, want 3 (initial + 2 retries)", got)
	}
}

func TestRetriesServerErrors(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)

			return
		}

		_, _ = w.Write([]byte(`{"id":1,"athlete":{"id":2}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.GetActivity(t.Context(), 1); err != nil {
		t.Fatalf("GetActivity: %v", err)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	_, err := client.GetActivity(t.Context(), 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d, want 1 — a 404 must not be retried", got)
	}
}

func TestUnauthorizedIsRecognized(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	_, err := client.GetActivity(t.Context(), 1)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

func TestTokenProviderErrorPropagates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	wantErr := errors.New("no token")

	client, err := NewClient(ClientConfig{
		Tokens:     staticTokens{err: wantErr},
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		MaxRetries: 1,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.GetActivity(t.Context(), 1); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{
		Tokens:     staticTokens{token: "t"},
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Sleep:      func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := client.GetActivity(ctx, 1); err == nil {
		t.Fatal("GetActivity with a canceled context = nil error, want error")
	}
}

func TestGetActivityRejectsBadJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.GetActivity(t.Context(), 1); err == nil {
		t.Fatal("GetActivity with truncated JSON = nil error, want error")
	}
}

func TestUpdateActivityNameRejectsBadJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeEnabled)

	if _, err := client.UpdateActivityName(t.Context(), 1, "Title"); err == nil {
		t.Fatal("UpdateActivityName with truncated JSON = nil error, want error")
	}
}

func TestTransportErrorIsReported(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serverURL := server.URL
	server.Close() // nothing is listening any more

	client, err := NewClient(ClientConfig{
		Tokens:     staticTokens{token: "t"},
		BaseURL:    serverURL,
		MaxRetries: 1,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.GetActivity(t.Context(), 1); err == nil {
		t.Fatal("GetActivity against a closed server = nil error, want error")
	}
}

func TestParsePair(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in         string
		wantShort  int
		wantDaily  int
		wantNoPair bool
	}{
		{in: "200,2000", wantShort: 200, wantDaily: 2000},
		{in: " 100 , 1000 ", wantShort: 100, wantDaily: 1000},
		{in: "200", wantNoPair: true},
		{in: "", wantNoPair: true},
		{in: "abc,2000", wantNoPair: true},
		{in: "200,abc", wantShort: 200},
	}

	for _, tt := range tests {
		short, daily := parsePair(tt.in)
		if tt.wantNoPair {
			tt.wantShort, tt.wantDaily = 0, 0
		}

		if short != tt.wantShort || daily != tt.wantDaily {
			t.Errorf("parsePair(%q) = %d, %d; want %d, %d",
				tt.in, short, daily, tt.wantShort, tt.wantDaily)
		}
	}
}

func TestRateLimitIgnoresResponsesWithoutHeaders(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Limit", "200,2000")
			w.Header().Set("X-RateLimit-Usage", "5,50")
		}

		_, _ = w.Write([]byte(`{"id":1,"athlete":{"id":2}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.GetActivity(t.Context(), 1); err != nil {
		t.Fatalf("GetActivity: %v", err)
	}
	if _, err := client.GetActivity(t.Context(), 1); err != nil {
		t.Fatalf("GetActivity: %v", err)
	}

	if got := client.RateLimit().ShortUsage; got != 5 {
		t.Errorf("ShortUsage = %d, want the last reported value 5", got)
	}
}

func TestBackoffGrows(t *testing.T) {
	t.Parallel()

	first := backoff(1)
	if first < time.Second || first >= 2*time.Second {
		t.Errorf("backoff(1) = %v, want between 1s and 2s", first)
	}

	if got := backoff(3); got < 4*time.Second {
		t.Errorf("backoff(3) = %v, want at least 4s", got)
	}
}

func TestSleepContextHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := sleepContext(ctx, time.Hour); err == nil {
		t.Error("sleepContext with a canceled context = nil, want error")
	}

	if err := sleepContext(t.Context(), time.Millisecond); err != nil {
		t.Errorf("sleepContext = %v, want nil", err)
	}
}

func TestWriteModeString(t *testing.T) {
	t.Parallel()

	if got := WriteModeDryRun.String(); got != "dry_run" {
		t.Errorf("WriteModeDryRun.String() = %q", got)
	}
	if got := WriteModeEnabled.String(); got != "enabled" {
		t.Errorf("WriteModeEnabled.String() = %q", got)
	}
}

func TestStatusErrorMessage(t *testing.T) {
	t.Parallel()

	err := &StatusError{StatusCode: http.StatusTeapot}
	if !strings.Contains(err.Error(), "418") {
		t.Errorf("Error() = %q, want it to mention the status code", err.Error())
	}

	if errors.Is(err, ErrNotFound) {
		t.Error("a 418 must not match ErrNotFound")
	}
}

func TestActivityOwnerFallsBackToNestedAthlete(t *testing.T) {
	t.Parallel()

	nested := &Activity{}
	nested.Athlete.ID = 4242

	if got := nested.Owner(); got != 4242 {
		t.Errorf("Owner() = %d, want the nested athlete ID 4242", got)
	}

	explicit := &Activity{AthleteID: 7}
	explicit.Athlete.ID = 4242

	if got := explicit.Owner(); got != 7 {
		t.Errorf("Owner() = %d, want the explicit 7", got)
	}
}

func TestUpdateActivityNamePropagatesTransportErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeEnabled)

	if _, err := client.UpdateActivityName(t.Context(), 1, "Title"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestSleepFailureAbortsRetries(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	sleepErr := errors.New("shutting down")

	client, err := NewClient(ClientConfig{
		Tokens:     staticTokens{token: "t"},
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Sleep:      func(context.Context, time.Duration) error { return sleepErr },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.GetActivity(t.Context(), 1); !errors.Is(err, sleepErr) {
		t.Fatalf("error = %v, want %v", err, sleepErr)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d, want 1 — the backoff failure stops the loop", got)
	}
}

func TestUnbuildableRequestIsReported(t *testing.T) {
	t.Parallel()

	client, err := NewClient(ClientConfig{
		Tokens:     staticTokens{token: "t"},
		BaseURL:    "://not-a-url",
		MaxRetries: 1,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.GetActivity(t.Context(), 1); err == nil {
		t.Fatal("GetActivity with an unparseable base URL = nil error, want error")
	}
}

func TestStatusErrorDoesNotMatchUnrelatedSentinels(t *testing.T) {
	t.Parallel()

	err := &StatusError{StatusCode: http.StatusTooManyRequests}

	if !errors.Is(err, ErrRateLimited) {
		t.Error("a 429 must match ErrRateLimited")
	}
	if errors.Is(err, ErrDryRun) {
		t.Error("a status error must not match ErrDryRun")
	}

	forbidden := &StatusError{StatusCode: http.StatusForbidden}
	if !errors.Is(forbidden, ErrUnauthorized) {
		t.Error("a 403 must match ErrUnauthorized")
	}
}

// A token that cannot be obtained will not become obtainable by asking again,
// and every retry costs another /oauth/token round trip.
func TestTokenErrorsAreNotRetried(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":1,"athlete":{"id":2}}`))
	}))
	defer server.Close()

	var sleeps atomic.Int64

	wantErr := errors.New("refresh token rejected")

	client, err := NewClient(ClientConfig{
		Tokens:     staticTokens{err: wantErr},
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		MaxRetries: 3,
		Sleep: func(context.Context, time.Duration) error {
			sleeps.Add(1)

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.GetActivity(t.Context(), 1); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}

	if got := sleeps.Load(); got != 0 {
		t.Errorf("backed off %d times, want 0", got)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("reached Strava %d times, want 0", got)
	}
}

func TestTokenErrorWrapsItsCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("refresh rejected")
	wrapped := &tokenError{err: cause}

	if wrapped.Error() != cause.Error() {
		t.Errorf("Error() = %q, want %q", wrapped.Error(), cause.Error())
	}
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is could not see through tokenError")
	}
}

// A response that never ends must not be decoded into memory without limit.
// The body here is valid JSON for far longer than the ceiling allows, so the
// decode fails on truncation rather than on a syntax error the server sent.
func TestGetActivityStopsAtTheSizeLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// A name field that keeps going. Written in chunks so the test does
		// not hold the whole oversized body in memory at once.
		_, _ = io.WriteString(w, `{"id":1,"name":"`)

		chunk := strings.Repeat("a", 4096)
		for written := 0; written < maxActivityBytes*2; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}

		_, _ = io.WriteString(w, `"}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.GetActivity(t.Context(), 1); err == nil {
		t.Error("GetActivity on an oversized body = nil error, want a decode failure")
	}
}

func TestUpdateActivityDescriptionSendsNoName(t *testing.T) {
	t.Parallel()

	var (
		gotMethod      atomic.Value
		gotPath        atomic.Value
		gotDescription atomic.Value
		gotFields      atomic.Int64
		hasName        atomic.Bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}

		gotMethod.Store(r.Method)
		gotPath.Store(r.URL.Path)
		gotDescription.Store(r.PostForm.Get("description"))
		gotFields.Store(int64(len(r.PostForm)))
		_, present := r.PostForm["name"]
		hasName.Store(present)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":12345,"name":"Windschief","athlete":{"id":7}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeEnabled)

	activity, err := client.UpdateActivityDescription(t.Context(), 12345, "Xert: Difficult")
	if err != nil {
		t.Fatalf("UpdateActivityDescription: %v", err)
	}

	if got := gotMethod.Load(); got != http.MethodPut {
		t.Errorf("method = %v, want PUT", got)
	}
	if got := gotPath.Load(); got != "/activities/12345" {
		t.Errorf("path = %v, want /activities/12345", got)
	}
	if got := gotDescription.Load(); got != "Xert: Difficult" {
		t.Errorf("description = %v", got)
	}

	// The whole point of the method: the title is not in the form, so Strava
	// has nothing to rewrite. A name field here — even the value already
	// stored — would be this service touching a title that is the athlete's.
	if hasName.Load() {
		t.Error("the form carried a name field, want none")
	}
	if got := gotFields.Load(); got != 1 {
		t.Errorf("form carried %d fields, want exactly 1 (description)", got)
	}

	if activity.Name != "Windschief" || activity.Owner() != 7 {
		t.Errorf("decoded activity = %+v", activity)
	}
}

func TestUpdateActivityDescriptionRefusesInDryRun(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.UpdateActivityDescription(t.Context(), 1, "anything"); !errors.Is(err, ErrDryRun) {
		t.Errorf("UpdateActivityDescription in dry run = %v, want ErrDryRun", err)
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("server saw %d requests, want 0", got)
	}
}

// An empty description is a legitimate value: it is what removing the
// attribution line from a description that held nothing else produces.
func TestUpdateActivityDescriptionAcceptsAnEmptyDescription(t *testing.T) {
	t.Parallel()

	var (
		gotFields      atomic.Int64
		hasDescription atomic.Bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}

		gotFields.Store(int64(len(r.PostForm)))
		_, present := r.PostForm["description"]
		hasDescription.Store(present)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"Windschief","athlete":{"id":7}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeEnabled)

	if _, err := client.UpdateActivityDescription(t.Context(), 1, ""); err != nil {
		t.Fatalf("UpdateActivityDescription with an empty description: %v", err)
	}

	if !hasDescription.Load() {
		t.Error("the form carried no description field, want an empty one")
	}
	if got := gotFields.Load(); got != 1 {
		t.Errorf("form carried %d fields, want exactly 1 (description)", got)
	}
}

// update is reachable only through the exported methods, each of which
// supplies at least one field. The guard is what keeps that true of the next
// one.
func TestUpdateRefusesWithNoFields(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeEnabled)

	if _, err := client.update(t.Context(), 1, nil, nil); err == nil {
		t.Error("update with no fields = nil error, want error")
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("server saw %d requests, want 0", got)
	}
}
