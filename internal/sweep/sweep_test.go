package sweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/idtoken"

	"github.com/jkreileder/titelheld/internal/processor"
)

const (
	testAudience = "https://titelheld.example.invalid"
	testAccount  = "titelheld-scheduler@example.invalid"
)

// countingSweeper records that a sweep happened. Every rejection test asserts
// this stayed at zero: a 401 that still drained the queue would be a 401 in
// name only.
type countingSweeper struct {
	mu    sync.Mutex
	calls int

	result  processor.Result
	err     error
	release chan struct{}

	// entered is signalled once the sweep is inside Sweep, so a test waits on
	// a deadline rather than spinning until a counter moves. A regression that
	// returns before reaching Sweep then fails on the deadline instead of
	// hanging until the package timeout.
	entered chan struct{}
}

func (c *countingSweeper) Sweep(ctx context.Context) (processor.Result, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	if c.entered != nil {
		// Non-blocking: only the first sweep is waited on, and a second one
		// must not stall here if nothing is listening.
		select {
		case c.entered <- struct{}{}:
		default:
		}
	}

	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return processor.Result{}, ctx.Err()
		}
	}

	return c.result, c.err
}

func (c *countingSweeper) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

// goodPayload is what a token from this service's own scheduler looks like.
func goodPayload() *idtoken.Payload {
	return &idtoken.Payload{
		Issuer: googleIssuer,
		Claims: map[string]any{
			"email":          testAccount,
			"email_verified": true,
		},
	}
}

type fixture struct {
	handler *Handler
	sweeper *countingSweeper
	logs    *bytes.Buffer

	// audience records what the handler asked the validator to check against.
	audience string
}

func newFixture(t *testing.T, payload *idtoken.Payload, validateErr error) *fixture {
	t.Helper()

	sweeper := &countingSweeper{}
	logs := &bytes.Buffer{}
	fix := &fixture{sweeper: sweeper, logs: logs}

	handler, err := New(Deps{
		Processor:      sweeper,
		Audience:       testAudience,
		ServiceAccount: testAccount,
		Logger:         slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Validate: func(_ context.Context, _, audience string) (*idtoken.Payload, error) {
			fix.audience = audience

			if validateErr != nil {
				return nil, validateErr
			}

			return payload, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fix.handler = handler

	return fix
}

// post sends a request with the given Authorization header, verbatim.
func (f *fixture) post(t *testing.T, authorization string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/sweep/secret", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	return rec
}

// Each rejected claim gets its own case, and each says so in the log.
//
// The response is the same bare 401 in every case on purpose. The log is where
// the difference lives, because the person who needs to know which claim
// failed is the operator, not the caller.
func TestEveryFailedClaimIsRejectedAndNamed(t *testing.T) {
	t.Parallel()

	unverified := goodPayload()
	unverified.Claims["email_verified"] = false

	missingVerified := goodPayload()
	delete(missingVerified.Claims, "email_verified")

	verifiedWrongType := goodPayload()
	verifiedWrongType.Claims["email_verified"] = "true"

	wrongEmail := goodPayload()
	wrongEmail.Claims["email"] = "someone-else@example.invalid"

	missingEmail := goodPayload()
	delete(missingEmail.Claims, "email")

	wrongIssuer := goodPayload()
	wrongIssuer.Issuer = "https://accounts.example.invalid"

	for _, tc := range []struct {
		name        string
		header      string
		payload     *idtoken.Payload
		validateErr error

		// wantLog is the phrase that has to appear in the rejection line, so
		// the operator can tell these cases apart.
		wantLog string
	}{
		{
			name:    "no Authorization header",
			header:  "",
			payload: goodPayload(),
			wantLog: "no Authorization header",
		},
		{
			name:    "not a Bearer token",
			header:  "Basic dXNlcjpwYXNz",
			payload: goodPayload(),
			wantLog: "not a Bearer token",
		},
		{
			name:    "an empty Bearer token",
			header:  "Bearer    ",
			payload: goodPayload(),
			wantLog: "Bearer token is empty",
		},
		{
			name:        "a token that does not validate",
			header:      "Bearer forged",
			validateErr: errors.New("idtoken: invalid signature"),
			wantLog:     "did not validate",
		},
		{
			name:    "the wrong issuer",
			header:  "Bearer t",
			payload: wrongIssuer,
			wantLog: "issuer is",
		},
		{
			name:    "the wrong service account",
			header:  "Bearer t",
			payload: wrongEmail,
			wantLog: "email is",
		},
		{
			name:    "no email claim at all",
			header:  "Bearer t",
			payload: missingEmail,
			wantLog: "email is",
		},
		{
			name:    "an unverified email",
			header:  "Bearer t",
			payload: unverified,
			wantLog: "email_verified is not true",
		},
		{
			name:    "no email_verified claim",
			header:  "Bearer t",
			payload: missingVerified,
			wantLog: "email_verified is not true",
		},
		{
			name:    "an email_verified claim of the wrong type",
			header:  "Bearer t",
			payload: verifiedWrongType,
			wantLog: "email_verified is not true",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fix := newFixture(t, tc.payload, tc.validateErr)

			rec := fix.post(t, tc.header)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status %d, want %d", rec.Code, http.StatusUnauthorized)
			}

			if fix.sweeper.count() != 0 {
				t.Errorf("the queue was swept %d times despite a 401", fix.sweeper.count())
			}

			if !strings.Contains(fix.logs.String(), tc.wantLog) {
				t.Errorf("the rejection log does not name the failing claim %q:\n%s",
					tc.wantLog, fix.logs.String())
			}

			// The caller learns nothing beyond "no".
			if body := rec.Body.String(); strings.Contains(body, testAccount) ||
				strings.Contains(body, testAudience) {
				t.Errorf("the response leaks what the handler expected: %q", body)
			}
		})
	}
}

// A lowercase scheme is still a Bearer token.
func TestTheBearerSchemeIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	for _, header := range []string{"Bearer t", "bearer t", "BEARER t"} {
		fix := newFixture(t, goodPayload(), nil)

		if rec := fix.post(t, header); rec.Code != http.StatusOK {
			t.Errorf("%q gave status %d, want %d", header, rec.Code, http.StatusOK)
		}
	}
}

// The scheduler's own token gets through, and the sweep runs once.
func TestTheSchedulersTokenIsAccepted(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, goodPayload(), nil)
	fix.sweeper.result = processor.Result{Due: 5, Named: 2, Reconciled: 1, Skipped: 1, Failed: 1}

	rec := fix.post(t, "Bearer t")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if fix.sweeper.count() != 1 {
		t.Errorf("the queue was swept %d times, want 1", fix.sweeper.count())
	}

	// The audience the handler checked against is the configured one, not
	// whatever the token happened to carry.
	if fix.audience != testAudience {
		t.Errorf("validated against audience %q, want %q", fix.audience, testAudience)
	}

	var body struct {
		Due, Named, Reconciled, Skipped, Failed int
		Cancelled                               bool
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v (%q)", err, rec.Body.String())
	}

	if body.Due != 5 || body.Named != 2 || body.Reconciled != 1 ||
		body.Skipped != 1 || body.Failed != 1 {
		t.Errorf("body %+v does not report the sweep's counts", body)
	}

	if body.Cancelled {
		t.Error("a sweep that ran to the end reported itself cancelled")
	}
}

// A sweep stopped by shutdown says so, and still answers 200.
//
// Cancellation is a distinct reported state rather than a failure: what was
// named is named, and the rest is still queued. Nothing else pins the field,
// so a regression that drops or inverts it would otherwise stay green.
func TestACancelledSweepIsReportedInTheResponse(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, goodPayload(), nil)
	fix.sweeper.result = processor.Result{Due: 3, Named: 1, Cancelled: true}

	rec := fix.post(t, "Bearer t")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Due, Named, Reconciled, Skipped, Failed int
		Cancelled                               bool
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v (%q)", err, rec.Body.String())
	}

	if !body.Cancelled {
		t.Errorf("body %+v does not report that the sweep stopped early", body)
	}

	if body.Named != 1 {
		t.Errorf("body %+v lost the count of what was named before it stopped", body)
	}
}

// A sweep with failures is still a 200.
//
// Every failed activity is still queued and the next fire retries it. A
// non-2xx would make Cloud Scheduler retry at once, straight back into
// whatever rate limit caused the failure.
func TestFailuresInsideASweepAreStillASuccessfulFire(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, goodPayload(), nil)
	fix.sweeper.result = processor.Result{Due: 3, Failed: 3}

	if rec := fix.post(t, "Bearer t"); rec.Code != http.StatusOK {
		t.Errorf("status %d, want %d", rec.Code, http.StatusOK)
	}
}

// A sweep that could not read its queue is a 500, so the fire is retried.
func TestABrokenQueueIsReportedToTheScheduler(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, goodPayload(), nil)
	fix.sweeper.err = errors.New("firestore: unavailable")

	rec := fix.post(t, "Bearer t")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	if body := rec.Body.String(); strings.Contains(body, "firestore") {
		t.Errorf("the response repeats the internal error: %q", body)
	}
}

// Two fires that overlap run one sweep, not two.
//
// Cloud Scheduler's attempt deadline is longer than the interval between
// fires, so this is a scheduled event rather than a hypothetical. Two sweeps
// over the same queue can both read the named log for an activity before
// either writes it, and rename it twice.
func TestAnOverlappingFireDoesNotStartASecondSweep(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, goodPayload(), nil)
	fix.sweeper.release = make(chan struct{})
	fix.sweeper.entered = make(chan struct{}, 1)

	done := make(chan int, 1)

	go func() {
		rec := fix.post(t, "Bearer t")
		done <- rec.Code
	}()

	// Wait until the first sweep is genuinely inside Sweep, so the second
	// request meets a held lock rather than racing to it. On a deadline, so a
	// handler that stops reaching Sweep fails here instead of hanging until
	// the package timeout takes the whole run down with it.
	select {
	case <-fix.sweeper.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached Sweep")
	}

	second := fix.post(t, "Bearer t")

	if second.Code != http.StatusOK {
		t.Errorf("the overlapping fire got status %d, want %d", second.Code, http.StatusOK)
	}

	if !strings.Contains(second.Body.String(), "already running") {
		t.Errorf("the overlapping fire was not reported as skipped: %q", second.Body.String())
	}

	close(fix.sweeper.release)

	if code := <-done; code != http.StatusOK {
		t.Errorf("the first fire got status %d, want %d", code, http.StatusOK)
	}

	if fix.sweeper.count() != 1 {
		t.Errorf("%d sweeps ran concurrently, want 1", fix.sweeper.count())
	}
}

// New refuses a handler that cannot check anything.
//
// Each of these would otherwise produce an endpoint that renames activities
// for callers it never verified.
func TestNewRefusesAnUncheckableConfiguration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		deps Deps
		want string
	}{
		{
			name: "no processor",
			deps: Deps{Audience: testAudience, ServiceAccount: testAccount},
			want: "processor",
		},
		{
			name: "no audience",
			deps: Deps{Processor: &countingSweeper{}, ServiceAccount: testAccount},
			want: "audience",
		},
		{
			name: "no service account",
			deps: Deps{Processor: &countingSweeper{}, Audience: testAudience},
			want: "service account",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler, err := New(tc.deps)
			if err == nil {
				t.Fatalf("New succeeded with %s", tc.name)
			}

			if handler != nil {
				t.Error("New returned a handler alongside an error")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the missing %s", err, tc.want)
			}
		})
	}
}

// New supplies the real validator and a logger when none are given.
func TestNewDefaultsToTheRealValidator(t *testing.T) {
	t.Parallel()

	handler, err := New(Deps{
		Processor:      &countingSweeper{},
		Audience:       testAudience,
		ServiceAccount: testAccount,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if handler.deps.Validate == nil {
		t.Error("no validator was supplied")
	}

	if handler.deps.Logger == nil {
		t.Error("no logger was supplied")
	}
}
