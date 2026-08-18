package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort reserves a port and releases it, so the service can bind it.
func freePort(t *testing.T) string {
	t.Helper()

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	return strconv.Itoa(port)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// env returns a getenv over a map with every required variable set.
func env(overrides map[string]string) func(string) string {
	values := map[string]string{
		"STRAVA_CLIENT_ID":     "12345",
		"STRAVA_CLIENT_SECRET": "test-client-secret",
		"STRAVA_VERIFY_TOKEN":  "test-verify-token",
		"BASE_URL":             "https://namer.example.invalid",
		"WEBHOOK_PATH_SECRET":  "s3cr3t-segment",
	}

	for key, value := range overrides {
		values[key] = value
	}

	return func(key string) string { return values[key] }
}

func TestRunRejectsBadConfiguration(t *testing.T) {
	t.Parallel()

	err := Run(t.Context(), quietLogger(), env(map[string]string{"STRAVA_CLIENT_ID": ""}))
	if err == nil {
		t.Fatal("run without a client ID = nil error, want error")
	}

	if !strings.Contains(err.Error(), "load configuration") {
		t.Errorf("error = %v, want it to mention the configuration", err)
	}
}

// A cancelled context must bring the whole thing up and straight back down,
// which exercises the wiring end to end.
func TestRunStartsAndStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() { done <- Run(ctx, quietLogger(), env(map[string]string{"PORT": freePort(t)})) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the context was cancelled")
	}
}

// Writes stay off unless DRY_RUN says otherwise; this exercises the branch that
// logs the loud warning.
func TestRunAcceptsWritesEnabled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() {
		done <- Run(ctx, quietLogger(), env(map[string]string{"DRY_RUN": "0", "PORT": freePort(t)}))
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	if err := <-done; err != nil {
		t.Errorf("run = %v, want nil", err)
	}
}
