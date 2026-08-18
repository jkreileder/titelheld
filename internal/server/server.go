// Package server assembles the service's HTTP surface: a health check, the
// one-time OAuth bootstrap, and the Strava webhook.
//
// The OAuth handlers are deliberately the only interactive thing here. They are
// plumbing for a one-time authorization, not a user interface.
package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/strava"
)

// stateTTL bounds how long an authorization attempt may take.
const stateTTL = 10 * time.Minute

// Deps are the collaborators the HTTP surface needs.
type Deps struct {
	Config  config.Config
	OAuth   *strava.OAuth
	Tokens  strava.TokenStore
	Webhook http.Handler
	Logger  *slog.Logger

	// Bound reports the athlete this service is already bound to, and whether
	// there is one. Consulted only when no athlete ID is configured. Optional.
	Bound func(context.Context) (int64, bool)

	// Now defaults to time.Now.
	Now func() time.Time

	// RandRead defaults to crypto/rand.Read. Injected so the failure path is
	// reachable in tests.
	RandRead func([]byte) (int, error)
}

// Server is the assembled HTTP surface.
type Server struct {
	deps     Deps
	logger   *slog.Logger
	now      func() time.Time
	randRead func([]byte) (int, error)

	mu     sync.Mutex
	states map[string]time.Time
}

// New builds the server and its routes.
func New(deps Deps) (*Server, error) {
	if deps.OAuth == nil || deps.Tokens == nil || deps.Webhook == nil {
		return nil, errors.New("server: OAuth, Tokens and Webhook are required")
	}
	if deps.Config.WebhookPath == "" || deps.Config.AuthPath == "" {
		return nil, errors.New("server: Config.WebhookPath and Config.AuthPath are required")
	}

	server := &Server{
		deps:     deps,
		logger:   deps.Logger,
		now:      deps.Now,
		randRead: deps.RandRead,
		states:   make(map[string]time.Time),
	}

	if server.logger == nil {
		server.logger = slog.Default()
	}
	if server.now == nil {
		server.now = time.Now
	}
	if server.randRead == nil {
		server.randRead = rand.Read
	}

	return server, nil
}

// Handler returns the router.
//
// The webhook and the authorization start are both mounted at their full secret
// paths, so a request that guesses the prefix but not the segment is a 404 from
// the mux rather than something a handler has to reason about. Starting the
// flow is what needs protecting: without it, anyone who found a bare /auth
// could authorize their own Strava account and have this service store their
// token. The callback stays at a fixed, registered URL and is guarded by the
// single-use state that only the start route issues.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+config.HealthPath, s.health)
	mux.HandleFunc("GET "+s.deps.Config.AuthPath, s.authStart)
	mux.HandleFunc("GET "+config.AuthCallbackPath, s.authCallback)
	mux.Handle(s.deps.Config.WebhookPath, s.deps.Webhook)

	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// authStart sends the athlete to Strava to grant access.
func (s *Server) authStart(w http.ResponseWriter, r *http.Request) {
	state, err := s.newState()
	if err != nil {
		s.logger.Error("could not generate an OAuth state", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	http.Redirect(w, r, s.deps.OAuth.AuthorizeURL(state), http.StatusFound)
}

// authCallback completes the flow and stores the token pair.
func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if authErr := query.Get("error"); authErr != "" {
		s.logger.Warn("authorization declined", "error", logsafe.String(authErr))
		http.Error(w, "authorization declined", http.StatusBadRequest)

		return
	}

	if !s.consumeState(query.Get("state")) {
		s.logger.Warn("authorization rejected", "reason", "unknown or expired state")
		http.Error(w, "invalid state", http.StatusBadRequest)

		return
	}

	code := query.Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)

		return
	}

	// Strava reports the granted scopes on the redirect. Checking here means a
	// partial grant fails now, loudly, rather than as a 401 on the first write
	// weeks later.
	granted := strava.ParseScopes(query.Get("scope"))
	if missing := missingScopes(granted); len(missing) > 0 {
		s.logger.Error("authorization incomplete", "missing_scopes", logsafe.Strings(missing))
		http.Error(w,
			"authorization is missing required scopes: "+strings.Join(missing, ", "),
			http.StatusBadRequest)

		return
	}

	token, err := s.deps.OAuth.Exchange(r.Context(), code)
	if err != nil {
		s.logger.Error("token exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)

		return
	}

	if len(token.Scopes) == 0 {
		token.Scopes = granted
	}

	if missing := token.MissingScopes(); len(missing) > 0 {
		s.logger.Error("token is missing required scopes", "missing_scopes", logsafe.Strings(missing))
		http.Error(w, "token is missing required scopes", http.StatusBadRequest)

		return
	}

	if err := s.checkAthlete(r.Context(), token.AthleteID); err != nil {
		s.logger.Error("authorization rejected", "error", err, "authorized", token.AthleteID)
		http.Error(w, "unexpected athlete", http.StatusForbidden)

		return
	}

	if err := s.deps.Tokens.Save(r.Context(), token); err != nil {
		s.logger.Error("could not store the token", "error", err)
		http.Error(w, "could not store the token", http.StatusInternalServerError)

		return
	}

	s.logger.Info("authorization complete",
		"athlete_id", token.AthleteID, "scopes", logsafe.Strings(token.Scopes))

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "Authorized athlete %d. You can close this tab.\n", token.AthleteID)
}

// checkAthlete decides whether this athlete may be stored.
//
// A configured athlete ID is authoritative. With none configured the service
// binds to whoever authorizes first, and refuses anyone else afterwards: a
// second athlete would not overwrite the first — it would add an entry and
// leave the store with no single answer to "which athlete is this service for".
func (s *Server) checkAthlete(ctx context.Context, authorized int64) error {
	if want := s.deps.Config.AthleteID; want != 0 {
		if authorized != want {
			return fmt.Errorf("athlete %d authorized, but this service is configured for %d",
				authorized, want)
		}

		return nil
	}

	if s.deps.Bound == nil {
		return nil
	}

	bound, ok := s.deps.Bound(ctx)
	if !ok {
		return nil // nothing bound yet: this authorization does the binding.
	}

	if bound != authorized {
		return fmt.Errorf("athlete %d authorized, but this service is already bound to %d",
			authorized, bound)
	}

	return nil
}

// missingScopes reports which required scopes are absent from granted.
func missingScopes(granted []string) []string {
	return strava.Token{Scopes: granted}.MissingScopes()
}

// newState issues a single-use CSRF state.
//
// States live in memory, which is correct only because the service runs with
// max-instances=1: a second instance would not recognise a state issued by the
// first. The authorization flow is a one-time bootstrap, so this is the cheapest
// thing that works — but it is the one place a change to max-instances would
// silently break.
func (s *Server) newState() (string, error) {
	raw := make([]byte, 32)
	if _, err := s.randRead(raw); err != nil {
		return "", fmt.Errorf("server: generate state: %w", err)
	}

	state := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for existing, issued := range s.states {
		if now.Sub(issued) > stateTTL {
			delete(s.states, existing)
		}
	}

	s.states[state] = now

	return state, nil
}

// consumeState checks and retires a state, so a replay of the callback fails.
func (s *Server) consumeState(state string) bool {
	if state == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	issued, ok := s.states[state]
	if !ok {
		return false
	}

	delete(s.states, state)

	return s.now().Sub(issued) <= stateTTL
}

// Run serves until the context is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context, addr string) error {
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)

	go func() {
		s.logger.Info("listening", "addr", addr)

		if err := httpServer.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			errc <- err

			return
		}

		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()

		s.logger.Info("shutting down")

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server: shutdown: %w", err)
		}

		return nil
	}
}
