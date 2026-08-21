package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

const testSweepPath = "/sweep/AbCdEf0123456789AbCdEf0123456789"

// stubSweep records that the route reached it.
type stubSweep struct{ hits int }

func (s *stubSweep) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.hits++

	w.WriteHeader(http.StatusOK)
}

func sweepConfig() config.SweepConfig {
	return config.SweepConfig{
		Path:           testSweepPath,
		Audience:       "https://namer.example.invalid",
		ServiceAccount: "titelheld-scheduler@example.invalid",
	}
}

// newSweepServer builds a server with whatever sweep wiring the test wants.
func newSweepServer(t *testing.T, handler http.Handler, sweep config.SweepConfig) (*Server, *stubSweep) {
	t.Helper()

	stub, _ := handler.(*stubSweep)

	server, err := New(Deps{
		Config: config.Config{
			WebhookPath: "/webhook/s3cr3t-segment",
			AuthPath:    testAuthPath,
			BaseURL:     "https://namer.example.invalid",
			Sweep:       sweep,
		},
		OAuth:   &strava.OAuth{},
		Tokens:  store.NewMemory(),
		Webhook: &stubWebhook{},
		Sweep:   handler,
		Logger:  quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, stub
}

// The route exists only when there is a handler and a full configuration.
func TestTheSweepRouteIsMountedOnlyWhenItCanVerifyItsCaller(t *testing.T) {
	t.Parallel()

	partial := sweepConfig()
	partial.ServiceAccount = ""

	for _, tc := range []struct {
		name        string
		withHandler bool
		sweep       config.SweepConfig
		wantStatus  int
	}{
		{
			name:        "fully configured",
			withHandler: true,
			sweep:       sweepConfig(),
			wantStatus:  http.StatusOK,
		},
		{
			name:        "no handler wired up",
			withHandler: false,
			sweep:       sweepConfig(),
			wantStatus:  http.StatusNotFound,
		},
		{
			name:        "a handler but no configuration",
			withHandler: true,
			sweep:       config.SweepConfig{},
			wantStatus:  http.StatusNotFound,
		},
		{
			name:        "a handler and a partial configuration",
			withHandler: true,
			sweep:       partial,
			wantStatus:  http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var handler http.Handler
			if tc.withHandler {
				handler = &stubSweep{}
			}

			server, stub := newSweepServer(t, handler, tc.sweep)

			req := httptest.NewRequestWithContext(
				t.Context(), http.MethodPost, testSweepPath, nil)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status %d, want %d", rec.Code, tc.wantStatus)
			}

			if tc.wantStatus == http.StatusNotFound && stub != nil && stub.hits != 0 {
				t.Errorf("the handler was reached %d times through an unmounted route", stub.hits)
			}
		})
	}
}

// Only POST reaches the sweep.
//
// Unlike the webhook, which answers a GET handshake at the same path it later
// receives events on, Cloud Scheduler only ever POSTs. Narrowing the method
// means a GET that guessed the path never reaches the token handling at all.
func TestTheSweepRouteAcceptsOnlyPOST(t *testing.T) {
	t.Parallel()

	for _, method := range []string{
		http.MethodGet, http.MethodPut, http.MethodDelete,
		http.MethodPatch, http.MethodHead,
	} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			server, stub := newSweepServer(t, &stubSweep{}, sweepConfig())

			req := httptest.NewRequestWithContext(t.Context(), method, testSweepPath, nil)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code == http.StatusOK {
				t.Errorf("%s reached the sweep handler", method)
			}

			if stub.hits != 0 {
				t.Errorf("%s reached the sweep handler %d times", method, stub.hits)
			}
		})
	}
}

// The sweep does not answer at a path that merely looks like it.
func TestTheSweepRouteIsNotServedAtItsPrefix(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/sweep", "/sweep/", "/sweep/wrong-segment"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			server, stub := newSweepServer(t, &stubSweep{}, sweepConfig())

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("%s gave status %d, want %d", path, rec.Code, http.StatusNotFound)
			}

			if stub.hits != 0 {
				t.Errorf("%s reached the sweep handler", path)
			}
		})
	}
}
