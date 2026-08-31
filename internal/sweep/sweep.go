// Package sweep serves the scheduled drain of the delay queue.
//
// This is the one endpoint that makes the service act on its own initiative.
// Everything else here reacts: the webhook answers Strava, the OAuth routes
// answer a browser. A POST to this route makes the service go and rename
// things, which is why it is the route with an identity check rather than only
// an unguessable path.
//
// It has to do that check itself. Cloud Run holds roles/run.invoker for
// allUsers — the Strava webhook has no way to present a Google credential, and
// the subscription and the sweep share one service — so the platform
// authenticates nobody, and a request arriving here has been let through by
// Cloud Run rather than vouched for by it. The unguessable path is
// obfuscation; the token verified below is the authentication.
//
// Rejection is as specific as it honestly can be in the log and silent to the
// caller. The issuer, the email and email_verified are named individually.
// Signature, expiry and audience are not separable — the validator reports
// them as one error — so the log carries its wording together with the
// audience that was required, which is enough to recognize a misconfigured
// audience without claiming to have identified it.
//
// The response is a bare 401 in every case, because telling an unauthenticated
// caller which half of the token was wrong is telling them how to fix it.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/api/idtoken"

	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/processor"
)

// googleIssuer is the only issuer whose tokens this service accepts.
//
// Google mints ID tokens under two issuer strings, and this one is what Cloud
// Scheduler uses. Accepting both would widen the check for no gain: the token
// this service must accept is the one its own scheduler sends.
const googleIssuer = "https://accounts.google.com"

// Sweeper is the part of the processor this package needs.
type Sweeper interface {
	Sweep(ctx context.Context) (processor.Result, error)
}

// Validator verifies an ID token and returns its payload.
//
// Injected so the whole rejection matrix is reachable in a test without a
// Google-signed token. In production this is [idtoken.Validate].
type Validator func(ctx context.Context, token, audience string) (*idtoken.Payload, error)

// Deps are what the handler needs.
type Deps struct {
	Processor Sweeper

	// Audience and ServiceAccount are the two claims a caller must match.
	Audience       string
	ServiceAccount string

	// Validate defaults to idtoken.Validate.
	Validate Validator

	Logger *slog.Logger
}

// Handler verifies the caller and runs a sweep.
type Handler struct {
	deps Deps

	// running admits one sweep at a time.
	//
	// Cloud Scheduler's attempt deadline is longer than the interval between
	// fires, so a slow sweep can still be running when the next one arrives.
	// Two sweeps over the same queue is not merely wasteful: both can read the
	// named log for one activity before either writes it, and the activity is
	// renamed twice. Refusing the overlapping fire costs nothing, because the
	// sweep already running is draining the same entries.
	running sync.Mutex
}

// New builds the handler.
func New(deps Deps) (*Handler, error) {
	switch {
	case deps.Processor == nil:
		return nil, errors.New("sweep: a processor is required")
	case deps.Audience == "":
		return nil, errors.New("sweep: an audience is required")
	case deps.ServiceAccount == "":
		return nil, errors.New("sweep: a service account is required")
	}

	if deps.Validate == nil {
		deps.Validate = idtoken.Validate
	}

	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	return &Handler{deps: deps}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.authenticate(r); err != nil {
		// The claim that failed, and nothing the caller did not already know.
		h.deps.Logger.Warn("sweep rejected", "reason", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	// TryLock rather than Lock: waiting would hold the overlapping request
	// open for as long as the running sweep takes, and Cloud Scheduler would
	// count that against its attempt deadline for no benefit.
	if !h.running.TryLock() {
		h.deps.Logger.Info("sweep already running; skipping this fire")
		writeJSON(w, http.StatusOK, `{"status":"already running"}`)

		return
	}
	defer h.running.Unlock()

	// The request's context, so a Cloud Run shutdown stops the sweep at an
	// activity boundary rather than mid-rename.
	result, err := h.deps.Processor.Sweep(r.Context())
	if err != nil {
		h.deps.Logger.Error("sweep failed", "error", err)
		http.Error(w, "sweep failed", http.StatusInternalServerError)

		return
	}

	// A sweep with failures is still a successful sweep. Every failed activity
	// is still queued, and the next fire retries it; answering non-2xx would
	// make Cloud Scheduler retry immediately instead, straight back into
	// whatever rate limit caused the failure.
	writeJSON(w, http.StatusOK, fmt.Sprintf(
		`{"due":%d,"named":%d,"reconciled":%d,"skipped":%d,"failed":%d,"cancelled":%t}`,
		result.Due, result.Named, result.Reconciled,
		result.Skipped, result.Failed, result.Cancelled))
}

// authenticate returns nil if the caller is the scheduler, and otherwise an
// error naming the claim that failed.
func (h *Handler) authenticate(r *http.Request) error {
	raw, err := bearerToken(r)
	if err != nil {
		return err
	}

	// Validate checks the signature, the expiry and the audience. Anything it
	// rejects is reported as a bad token rather than as a bad audience: it
	// does not say which, and guessing in a log message is worse than not
	// knowing.
	payload, err := h.deps.Validate(r.Context(), raw, h.deps.Audience)
	if err != nil {
		return fmt.Errorf("token did not validate for audience %q: %w", h.deps.Audience, err)
	}

	if payload.Issuer != googleIssuer {
		return fmt.Errorf("issuer is %q, want %q", logsafe.String(payload.Issuer), googleIssuer)
	}

	email, _ := payload.Claims["email"].(string)
	if email != h.deps.ServiceAccount {
		return fmt.Errorf("email is %q, want %q", logsafe.String(email), h.deps.ServiceAccount)
	}

	// A Google service account's email is always verified, so this claim being
	// absent or false means the token is not the kind of token it looks like.
	if verified, ok := payload.Claims["email_verified"].(bool); !ok || !verified {
		return errors.New("email_verified is not true")
	}

	return nil
}

// bearerToken extracts the credential from the Authorization header.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", errors.New("no Authorization header")
	}

	// Scheme names are case-insensitive per RFC 7235, and rejecting "bearer"
	// would be rejecting a valid token on a technicality.
	token, ok := cutPrefixFold(header, "Bearer ")
	if !ok {
		return "", errors.New("the Authorization header is not a Bearer token")
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("the Bearer token is empty")
	}

	return token, nil
}

// cutPrefixFold is strings.CutPrefix, case-insensitively.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}

	return s[len(prefix):], true
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}
