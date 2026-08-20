package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

var testNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

// testAuthPath mirrors what config.Load derives from the path secret.
const testAuthPath = "/auth/s3cr3t-segment"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubWebhook stands in for the real handler so routing can be asserted alone.
type stubWebhook struct{ hits int }

func (s *stubWebhook) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.hits++

	w.WriteHeader(http.StatusOK)
}

type failingTokens struct{ *store.Memory }

func (failingTokens) Save(context.Context, strava.Token) error {
	return errors.New("firestore unavailable")
}

// newServer builds a server whose OAuth points at tokenServer, if given.
func newServer(t *testing.T, tokenServer *httptest.Server, tokens strava.TokenStore, athleteID int64) (*Server, *stubWebhook) {
	t.Helper()

	oauth := &strava.OAuth{
		ClientID:     "12345",
		ClientSecret: "test-client-secret",
		RedirectURL:  "https://namer.example.invalid/auth/callback",
	}

	if tokenServer != nil {
		oauth.BaseURL = tokenServer.URL
		oauth.HTTPClient = tokenServer.Client()
	}

	hook := &stubWebhook{}

	server, err := New(Deps{
		Config: config.Config{
			WebhookPath: "/webhook/s3cr3t-segment",
			AuthPath:    testAuthPath,
			BaseURL:     "https://namer.example.invalid",
			AthleteID:   athleteID,
		},
		OAuth:   oauth,
		Tokens:  tokens,
		Webhook: hook,
		Logger:  quietLogger(),
		Now:     func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, hook
}

func TestNewValidatesDeps(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	hook := &stubWebhook{}
	oauth := &strava.OAuth{}

	if _, err := New(Deps{Tokens: memory, Webhook: hook}); err == nil {
		t.Error("New without OAuth = nil error, want error")
	}
	if _, err := New(Deps{OAuth: oauth, Tokens: memory, Webhook: hook}); err == nil {
		t.Error("New without a webhook path = nil error, want error")
	}

	server, err := New(Deps{
		Config:  config.Config{WebhookPath: "/webhook/x", AuthPath: "/auth/x"},
		OAuth:   oauth,
		Tokens:  memory,
		Webhook: hook,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if server.logger == nil || server.now == nil {
		t.Error("New left the logger or clock unset")
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	server, _ := newServer(t, nil, store.NewMemory(), 0)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, config.HealthPath, nil))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", recorder.Code)
	}
}

// The webhook is reachable only at its full secret path.
func TestWebhookRouting(t *testing.T) {
	t.Parallel()

	server, hook := newServer(t, nil, store.NewMemory(), 0)
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook/s3cr3t-segment", nil))

	if recorder.Code != http.StatusOK || hook.hits != 1 {
		t.Errorf("status = %d, hits = %d; want 200, 1", recorder.Code, hook.hits)
	}

	for _, path := range []string{"/webhook", "/webhook/", "/webhook/guessed", "/"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil))

		if recorder.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, recorder.Code)
		}
	}

	if hook.hits != 1 {
		t.Errorf("webhook was reached %d times, want 1", hook.hits)
	}
}

func TestAuthStartRedirectsToStrava(t *testing.T) {
	t.Parallel()

	server, _ := newServer(t, nil, store.NewMemory(), 0)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, testAuthPath, nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}

	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}

	if location.Host != "www.strava.com" || location.Path != "/oauth/authorize" {
		t.Errorf("Location = %s", location)
	}

	query := location.Query()
	if query.Get("state") == "" {
		t.Error("no state in the authorize URL")
	}
	if got := query.Get("scope"); got != strava.Scopes {
		t.Errorf("scope = %q, want %q", got, strava.Scopes)
	}
}

// startAuth performs GET /auth and returns the issued state.
func startAuth(t *testing.T, server *Server) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, testAuthPath, nil))

	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}

	return location.Query().Get("state")
}

func tokenServer(t *testing.T, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return server
}

func TestAuthCallbackStoresTheToken(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	tokens := tokenServer(t, `{
		"access_token": "access-one",
		"refresh_token": "refresh-one",
		"expires_in": 21600,
		"scope": "read,activity:read_all,activity:write",
		"athlete": {"id": 4242}
	}`)

	server, _ := newServer(t, tokens, memory, 4242)
	state := startAuth(t, server)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		config.AuthCallbackPath+"?code=auth-code&state="+state+
			"&scope=read,activity:read_all,activity:write", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", recorder.Code, recorder.Body.String())
	}

	stored, err := memory.Load(t.Context(), 4242)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if stored.RefreshToken != "refresh-one" {
		t.Errorf("stored token = %+v", stored)
	}
}

// A state is single use, so replaying the callback fails.
func TestAuthCallbackStateIsSingleUse(t *testing.T) {
	t.Parallel()

	tokens := tokenServer(t, `{"access_token":"a","refresh_token":"r",
		"scope":"activity:read_all,activity:write","athlete":{"id":1}}`)

	server, _ := newServer(t, tokens, store.NewMemory(), 0)
	state := startAuth(t, server)

	target := config.AuthCallbackPath + "?code=c&state=" + state +
		"&scope=activity:read_all,activity:write"

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))

	if first.Code != http.StatusOK {
		t.Fatalf("first callback = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))

	if second.Code != http.StatusBadRequest {
		t.Errorf("replayed callback = %d, want 400", second.Code)
	}
}

func TestAuthCallbackRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		query    func(state string) string
		wantCode int
	}{
		{
			name:     "authorization declined",
			query:    func(string) string { return "error=access_denied" },
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "unknown state",
			query:    func(string) string { return "code=c&state=forged" },
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "missing state",
			query:    func(string) string { return "code=c" },
			wantCode: http.StatusBadRequest,
		},
		{
			name: "missing code",
			query: func(state string) string {
				return "state=" + state + "&scope=activity:read_all,activity:write"
			},
			wantCode: http.StatusBadRequest,
		},
		{
			// A partial grant must fail here rather than as a 401 on the first
			// write weeks later.
			name: "write scope not granted",
			query: func(state string) string {
				return "code=c&state=" + state + "&scope=read,activity:read_all"
			},
			wantCode: http.StatusBadRequest,
		},
		{
			name: "no scopes at all",
			query: func(state string) string {
				return "code=c&state=" + state
			},
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, _ := newServer(t, nil, store.NewMemory(), 0)
			state := startAuth(t, server)

			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
				config.AuthCallbackPath+"?"+tt.query(state), nil))

			if recorder.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body %q",
					recorder.Code, tt.wantCode, recorder.Body.String())
			}
		})
	}
}

func TestAuthCallbackRejectsTheWrongAthlete(t *testing.T) {
	t.Parallel()

	tokens := tokenServer(t, `{"access_token":"a","refresh_token":"r",
		"scope":"activity:read_all,activity:write","athlete":{"id":99}}`)

	server, _ := newServer(t, tokens, store.NewMemory(), 4242)
	state := startAuth(t, server)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		config.AuthCallbackPath+"?code=c&state="+state+
			"&scope=activity:read_all,activity:write", nil))

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", recorder.Code)
	}
}

func TestAuthCallbackReportsExchangeFailure(t *testing.T) {
	t.Parallel()

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer failing.Close()

	server, _ := newServer(t, failing, store.NewMemory(), 0)
	state := startAuth(t, server)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		config.AuthCallbackPath+"?code=c&state="+state+
			"&scope=activity:read_all,activity:write", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", recorder.Code)
	}
}

// A token whose own scope list is empty falls back to the redirect's scopes;
// one that is genuinely short is rejected.
func TestAuthCallbackChecksTheTokenScopes(t *testing.T) {
	t.Parallel()

	tokens := tokenServer(t, `{"access_token":"a","refresh_token":"r","athlete":{"id":1}}`)

	server, _ := newServer(t, tokens, store.NewMemory(), 0)
	state := startAuth(t, server)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		config.AuthCallbackPath+"?code=c&state="+state+
			"&scope=activity:read_all,activity:write", nil))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — the redirect's scopes stand in", recorder.Code)
	}
}

func TestAuthCallbackReportsStoreFailure(t *testing.T) {
	t.Parallel()

	tokens := tokenServer(t, `{"access_token":"a","refresh_token":"r",
		"scope":"activity:read_all,activity:write","athlete":{"id":1}}`)

	server, _ := newServer(t, tokens, failingTokens{store.NewMemory()}, 0)
	state := startAuth(t, server)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		config.AuthCallbackPath+"?code=c&state="+state+
			"&scope=activity:read_all,activity:write", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}
}

func TestStateExpires(t *testing.T) {
	t.Parallel()

	server, _ := newServer(t, nil, store.NewMemory(), 0)

	state, err := server.newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}

	// Move the clock past the TTL.
	server.now = func() time.Time { return testNow.Add(stateTTL + time.Minute) }

	if server.consumeState(state) {
		t.Error("an expired state was accepted")
	}
	if server.consumeState("") {
		t.Error("an empty state was accepted")
	}
}

func TestExpiredStatesAreEvicted(t *testing.T) {
	t.Parallel()

	server, _ := newServer(t, nil, store.NewMemory(), 0)

	if _, err := server.newState(); err != nil {
		t.Fatalf("newState: %v", err)
	}

	server.now = func() time.Time { return testNow.Add(stateTTL + time.Minute) }

	if _, err := server.newState(); err != nil {
		t.Fatalf("newState: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	if len(server.states) != 1 {
		t.Errorf("states holds %d entries, want 1 — the expired one must be evicted", len(server.states))
	}
}

func TestMissingScopes(t *testing.T) {
	t.Parallel()

	if got := missingScopes([]string{strava.ScopeActivityReadAll, strava.ScopeActivityWrite}); len(got) != 0 {
		t.Errorf("missingScopes = %v, want none", got)
	}

	got := missingScopes([]string{strava.ScopeActivityReadAll})
	if len(got) != 1 || got[0] != strava.ScopeActivityWrite {
		t.Errorf("missingScopes = %v, want [activity:write]", got)
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	t.Parallel()

	server, _ := newServer(t, nil, store.NewMemory(), 0)

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr := listener.Addr().String()
	_ = listener.Close()

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() { done <- server.Run(ctx, addr) }()

	// Wait for the listener to come up.
	var response *http.Response

	for range 50 {
		response, err = http.Get("http://" + addr + config.HealthPath) //nolint:noctx // short-lived test probe
		if err == nil {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if err != nil {
		cancel()
		t.Fatalf("health probe never succeeded: %v", err)
	}

	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()

	if !strings.Contains(string(body), "ok") {
		t.Errorf("health body = %q", body)
	}

	cancel()

	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil after a clean shutdown", err)
	}
}

func TestRunReportsAListenFailure(t *testing.T) {
	t.Parallel()

	server, _ := newServer(t, nil, store.NewMemory(), 0)

	// Port 1 is privileged, so binding it fails without root.
	if err := server.Run(t.Context(), "127.0.0.1:1"); err == nil {
		t.Error("Run on a privileged port = nil error, want error")
	}
}

// failingRand stands in for an exhausted entropy source.
func failingRand([]byte) (int, error) {
	return 0, errors.New("no entropy")
}

func TestAuthStartReportsAStateFailure(t *testing.T) {
	t.Parallel()

	server, _ := newServer(t, nil, store.NewMemory(), 0)
	server.randRead = failingRand

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, testAuthPath, nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}
}

// The redirect can claim a full grant while the token itself comes back short;
// the token's own scopes are checked too.
func TestAuthCallbackRejectsAShortToken(t *testing.T) {
	t.Parallel()

	tokens := tokenServer(t, `{"access_token":"a","refresh_token":"r",
		"scope":"read,activity:read_all","athlete":{"id":1}}`)

	server, _ := newServer(t, tokens, store.NewMemory(), 0)
	state := startAuth(t, server)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		config.AuthCallbackPath+"?code=c&state="+state+
			"&scope=activity:read_all,activity:write", nil))

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
}

// With no athlete configured the service binds to whoever authorizes first, and
// refuses anyone else afterwards: a stranger who reached the start route could
// otherwise add a second token and leave the store with no single answer to
// which athlete this service is for.
func TestAuthCallbackBindsToTheFirstAthlete(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()

	if err := memory.Save(t.Context(), strava.Token{AthleteID: 4242}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tokens := tokenServer(t, `{"access_token":"a","refresh_token":"r",
		"scope":"activity:read_all,activity:write","athlete":{"id":99}}`)

	oauth := &strava.OAuth{ClientID: "1", BaseURL: tokens.URL, HTTPClient: tokens.Client()}

	server, err := New(Deps{
		Config:  config.Config{WebhookPath: "/webhook/x", AuthPath: testAuthPath},
		OAuth:   oauth,
		Tokens:  memory,
		Webhook: &stubWebhook{},
		Logger:  quietLogger(),
		Now:     func() time.Time { return testNow },
		Bound: func(ctx context.Context) (int64, bool) {
			token, err := memory.AnyToken(ctx)

			return token.AthleteID, err == nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	state := startAuth(t, server)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		config.AuthCallbackPath+"?code=c&state="+state+
			"&scope=activity:read_all,activity:write", nil))

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a second athlete", recorder.Code)
	}
}

// Re-authorizing the athlete already bound is fine — tokens expire and scopes
// change.
func TestAuthCallbackAllowsTheBoundAthleteAgain(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()

	if err := memory.Save(t.Context(), strava.Token{AthleteID: 4242}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tokens := tokenServer(t, `{"access_token":"a","refresh_token":"r",
		"scope":"activity:read_all,activity:write","athlete":{"id":4242}}`)

	oauth := &strava.OAuth{ClientID: "1", BaseURL: tokens.URL, HTTPClient: tokens.Client()}

	server, err := New(Deps{
		Config:  config.Config{WebhookPath: "/webhook/x", AuthPath: testAuthPath},
		OAuth:   oauth,
		Tokens:  memory,
		Webhook: &stubWebhook{},
		Logger:  quietLogger(),
		Now:     func() time.Time { return testNow },
		Bound: func(ctx context.Context) (int64, bool) {
			token, err := memory.AnyToken(ctx)

			return token.AthleteID, err == nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	state := startAuth(t, server)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		config.AuthCallbackPath+"?code=c&state="+state+
			"&scope=activity:read_all,activity:write", nil))

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body %q", recorder.Code, recorder.Body.String())
	}
}

// The bare /auth path must not exist: it is what a stranger would find.
func TestBareAuthPathIsNotRouted(t *testing.T) {
	t.Parallel()

	server, _ := newServer(t, nil, store.NewMemory(), 0)

	for _, path := range []string{"/auth", "/auth/", "/auth/guessed"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder,
			httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, recorder.Code)
		}
	}
}

// Two athletes authorizing at the same moment must not both bind. checkAthlete
// asks whether anything is bound and Tokens.Save does the binding, so without
// a lock spanning the pair both callbacks can observe an empty store and both
// proceed — leaving the service with no single answer to which athlete it is
// for. Exactly one must win.
func TestAuthCallbackBindsOnlyOneAthleteUnderRace(t *testing.T) {
	t.Parallel()

	const (
		firstAthlete  = 8001
		secondAthlete = 8002
	)

	memory := store.NewMemory()

	// Waiting for the check-then-write window to be hit by chance tests
	// nothing: it is a few instructions wide, and twenty unlocked runs never
	// reproduced it. Hold the *write* until both callbacks have completed
	// their check, which is precisely the interleaving the lock prevents.
	//
	// Gating the check instead does not work — whichever goroutine releases
	// the barrier runs on while the other waits to be rescheduled, so the
	// releaser writes first and the loser is correctly rejected even with no
	// lock at all, and the test passes for the wrong reason.
	//
	// With the lock the second callback cannot reach the check until the
	// first has written, so the first write waits out this timeout once.
	const writeGate = 2 * time.Second

	var checked atomic.Int32

	gated := &gatedTokenStore{
		TokenStore: memory,
		release:    func() bool { return checked.Load() >= 2 },
		timeout:    writeGate,
	}

	// A distinct athlete per exchange, so the two callbacks race to bind two
	// different identities rather than the same one.
	var issued atomic.Int64

	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		athlete := firstAthlete
		if issued.Add(1) > 1 {
			athlete = secondAthlete
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"a","refresh_token":"r",
			"scope":"activity:read_all,activity:write","athlete":{"id":%d}}`, athlete)
	}))
	t.Cleanup(tokens.Close)

	server, err := New(Deps{
		Config:  config.Config{WebhookPath: "/webhook/x", AuthPath: testAuthPath},
		OAuth:   &strava.OAuth{ClientID: "1", BaseURL: tokens.URL, HTTPClient: tokens.Client()},
		Tokens:  gated,
		Webhook: &stubWebhook{},
		Logger:  quietLogger(),
		Now:     func() time.Time { return testNow },
		Bound: func(ctx context.Context) (int64, bool) {
			token, err := memory.AnyToken(ctx)
			checked.Add(1)

			return token.AthleteID, err == nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Both states are minted before either callback runs, so the two requests
	// differ only in when they arrive.
	states := []string{startAuth(t, server), startAuth(t, server)}

	var (
		start     sync.WaitGroup
		done      sync.WaitGroup
		codes     = make([]int, len(states))
		handler   = server.Handler()
		scopeArgs = "&scope=activity:read_all,activity:write"
	)

	start.Add(1)

	for i, state := range states {
		done.Add(1)

		go func() {
			defer done.Done()

			start.Wait()

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
				config.AuthCallbackPath+"?code=c&state="+state+scopeArgs, nil))
			codes[i] = recorder.Code
		}()
	}

	start.Done()
	done.Wait()

	var accepted, rejected int

	for _, code := range codes {
		switch code {
		case http.StatusOK:
			accepted++
		case http.StatusForbidden:
			rejected++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}

	if accepted != 1 || rejected != 1 {
		t.Errorf("accepted %d and rejected %d, want exactly one of each (codes %v)",
			accepted, rejected, codes)
	}

	var bound int

	for _, athlete := range []int64{firstAthlete, secondAthlete} {
		if _, err := memory.Load(t.Context(), athlete); err == nil {
			bound++
		} else if !errors.Is(err, strava.ErrTokenNotFound) {
			t.Fatalf("Load(%d): %v", athlete, err)
		}
	}

	if bound != 1 {
		t.Errorf("%d athletes bound, want exactly 1", bound)
	}
}

// gatedTokenStore delays Save until release reports true, or until timeout. It
// exists so a test can pin the interleaving of a check-then-write rather than
// hoping the scheduler produces it.
type gatedTokenStore struct {
	strava.TokenStore

	release func() bool
	timeout time.Duration
}

func (g *gatedTokenStore) Save(ctx context.Context, token strava.Token) error {
	deadline := time.Now().Add(g.timeout)

	for !g.release() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	return g.TokenStore.Save(ctx, token)
}
