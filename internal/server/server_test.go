package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jkreileder/strava-namer/internal/config"
	"github.com/jkreileder/strava-namer/internal/store"
	"github.com/jkreileder/strava-namer/internal/strava"
)

var testNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

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
		Config:  config.Config{WebhookPath: "/webhook/x"},
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
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, config.AuthPath, nil))

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
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, config.AuthPath, nil))

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
	server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, config.AuthPath, nil))

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
