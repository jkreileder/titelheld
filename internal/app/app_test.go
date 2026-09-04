package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/config"
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
		"MAX_INSTANCES":        "1",
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

// A canceled context must bring the whole thing up and straight back down,
// which exercises the wiring end to end.
func TestRunStartsAndStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	// The port is picked on the test goroutine: a t.Fatal inside the goroutine
	// below would exit that goroutine alone and leave done unwritten.
	port := freePort(t)
	done := make(chan error, 1)

	go func() { done <- Run(ctx, quietLogger(), env(map[string]string{"PORT": port})) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the context was canceled")
	}
}

// Writes stay off unless DRY_RUN says otherwise; this exercises the branch that
// logs the loud warning.
func TestRunAcceptsWritesEnabled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	port := freePort(t)
	done := make(chan error, 1)

	go func() {
		done <- Run(ctx, quietLogger(), env(map[string]string{"DRY_RUN": "0", "PORT": port}))
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	if err := <-done; err != nil {
		t.Errorf("run = %v, want nil", err)
	}
}

// The Firestore path is the one that matters in production, so it is started
// end to end against the emulator rather than assumed to work.
func TestRunOnFirestore(t *testing.T) {
	t.Parallel()

	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is not set; start the Firestore emulator to run this test")
	}

	ctx, cancel := context.WithCancel(t.Context())

	port := freePort(t)
	done := make(chan error, 1)

	go func() {
		done <- Run(ctx, quietLogger(), env(map[string]string{
			"PORT":              port,
			"FIRESTORE_PROJECT": "titelheld-emulator-test",
		}))
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run on Firestore = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was canceled")
	}
}

func TestStoreKind(t *testing.T) {
	t.Parallel()

	if got := storeKind(config.Config{}); got != "memory" {
		t.Errorf("storeKind with no project = %q, want memory", got)
	}
	if got := storeKind(config.Config{FirestoreProject: "p"}); got != "firestore" {
		t.Errorf("storeKind with a project = %q, want firestore", got)
	}
}

// The path secret is the only thing in front of the webhook intake — Strava
// does not sign its POSTs — so it must never reach a log. Cloud Run logs are
// readable by a much wider set of principals than the environment is.
func TestRunNeverLogsThePathSecret(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-segment"

	// Allocated out here: freePort calls t.Fatalf on failure, which stops only
	// the calling goroutine, so from inside the one below it would leave this
	// test waiting out its timeout and reporting the wrong thing.
	port := freePort(t)

	var logged safeBuffer

	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() {
		done <- Run(ctx, logger, env(map[string]string{
			"PORT":                port,
			"WEBHOOK_PATH_SECRET": secret,
		}))
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	// The error matters here, not just the return: a run that failed before it
	// logged anything would satisfy the assertion below while proving nothing.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the context was canceled")
	}

	if output := logged.String(); strings.Contains(output, secret) {
		t.Errorf("the path secret reached the log:\n%s", output)
	}
}

// safeBuffer is a bytes.Buffer that tolerates the logger writing from the
// server's goroutines while the test reads it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
