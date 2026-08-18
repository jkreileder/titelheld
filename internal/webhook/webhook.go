// Package webhook implements Strava's push-subscription endpoint: the
// validation handshake and the event intake that queues activities for naming.
package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/store"
)

// maxBodyBytes caps the request body. Strava's events are a few hundred bytes;
// this is generous and still bounds what an unauthenticated caller can push.
const maxBodyBytes = 64 << 10

// processTimeout bounds the work done after the acknowledgement has gone out.
const processTimeout = 15 * time.Second

// maxEventAge is how far back an event_time is still believed. Beyond it the
// value is treated as bogus and the delay is measured from arrival instead.
const maxEventAge = 24 * time.Hour

// Object types and aspect types Strava sends.
const (
	objectTypeActivity = "activity"
	objectTypeAthlete  = "athlete"

	// updateAuthorized is the key Strava uses to signal a revoked grant, and
	// deauthorizedValue the value that means revoked.
	updateAuthorized  = "authorized"
	deauthorizedValue = "false"
	aspectCreate      = "create"
	aspectUpdate      = "update"
)

// Event is the JSON body Strava POSTs to the callback URL.
type Event struct {
	ObjectType     string            `json:"object_type"`
	ObjectID       int64             `json:"object_id"`
	AspectType     string            `json:"aspect_type"`
	OwnerID        int64             `json:"owner_id"`
	SubscriptionID int64             `json:"subscription_id"`
	EventTime      int64             `json:"event_time"`
	Updates        map[string]string `json:"updates"`
}

// Deauthorized reports whether the event says the athlete revoked access.
//
// Strava signals this on an *athlete* event with updates.authorized == "false".
// The object type is part of the test: without it, an activity event carrying
// the same update would be silently dropped and logged as a deauthorization.
func (e Event) Deauthorized() bool {
	return e.ObjectType == objectTypeAthlete && e.Updates[updateAuthorized] == deauthorizedValue
}

// Handler serves the subscription validation handshake and the event intake.
//
// It is mounted at an unguessable path in addition to checking the verify
// token, so an attacker needs both to reach it.
type Handler struct {
	verifyToken string
	athleteID   int64
	delay       time.Duration
	queue       store.Queue
	named       store.NamedLog
	logger      *slog.Logger
	now         func() time.Time
}

// Config configures a [Handler].
type Config struct {
	// VerifyToken is the shared secret Strava echoes during validation.
	// Required.
	VerifyToken string

	// AthleteID, when non-zero, is the only owner whose events are accepted.
	AthleteID int64

	// Delay is how long after the event an activity becomes eligible.
	Delay time.Duration

	// Queue and Named are required.
	Queue store.Queue
	Named store.NamedLog

	// Logger defaults to slog.Default.
	Logger *slog.Logger

	// Now defaults to time.Now.
	Now func() time.Time
}

// New builds a handler.
func New(cfg Config) (*Handler, error) {
	if cfg.VerifyToken == "" {
		return nil, errors.New("webhook: VerifyToken is required")
	}
	if cfg.Queue == nil || cfg.Named == nil {
		return nil, errors.New("webhook: Queue and Named are required")
	}

	handler := &Handler{
		verifyToken: cfg.VerifyToken,
		athleteID:   cfg.AthleteID,
		delay:       cfg.Delay,
		queue:       cfg.Queue,
		named:       cfg.Named,
		logger:      cfg.Logger,
		now:         cfg.Now,
	}

	if handler.logger == nil {
		handler.logger = slog.Default()
	}
	if handler.now == nil {
		handler.now = time.Now
	}

	return handler, nil
}

// ServeHTTP routes the two verbs Strava uses.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.validate(w, r)
	case http.MethodPost:
		h.intake(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// validate answers the subscription handshake by echoing hub.challenge.
func (h *Handler) validate(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if query.Get("hub.mode") != "subscribe" {
		h.logger.Warn("webhook validation rejected", "reason", "unexpected hub.mode")
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	if !equalSecret(query.Get("hub.verify_token"), h.verifyToken) {
		h.logger.Warn("webhook validation rejected", "reason", "verify token mismatch")
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	challenge := query.Get("hub.challenge")
	if challenge == "" {
		http.Error(w, "missing hub.challenge", http.StatusBadRequest)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(map[string]string{"hub.challenge": challenge}); err != nil {
		h.logger.Error("webhook validation response failed", "error", err)
	}

	h.logger.Info("webhook subscription validated")
}

// intake accepts an event, acknowledges it, and then queues the activity.
//
// The 200 goes out before the queue write, which is the order Strava's two
// second budget assumes: a cold start plus a store round trip is exactly what
// would blow it. Strava retries a delivery it never sees acknowledged, and the
// queue is idempotent, so the ordering costs nothing that is not already
// handled.
func (h *Handler) intake(w http.ResponseWriter, r *http.Request) {
	var event Event

	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	if err := decoder.Decode(&event); err != nil {
		h.logger.Warn("webhook event rejected", "reason", "undecodable body", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	// Acknowledge first, then do the work.
	w.WriteHeader(http.StatusOK)

	// Push the acknowledgement out before touching the store. A writer that
	// cannot flush is not a reason to drop the event: the work below is what
	// matters, and Strava will retry a delivery it never saw acknowledged.
	if err := http.NewResponseController(w).Flush(); err != nil {
		h.logger.Debug("could not flush the acknowledgement", "error", err)
	}

	// The work below must not run on the request context. Flushing is exactly
	// what lets the client return from its call and close the body, which
	// cancels that context while this handler is still working — the store
	// operations would then fail with context.Canceled and, because the
	// acknowledgement has already gone out, Strava would never retry.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), processTimeout)
	defer cancel()

	h.process(ctx, event)
}

// process decides what to do with an acknowledged event.
func (h *Handler) process(ctx context.Context, event Event) {
	// object_type and aspect_type come straight off the wire, so they are
	// sanitised before they reach the log. The numeric fields cannot carry a
	// forged line.
	log := h.logger.With(
		"object_type", logsafe.String(event.ObjectType),
		"aspect_type", logsafe.String(event.AspectType),
		"activity_id", event.ObjectID,
		"owner_id", event.OwnerID,
	)

	if event.Deauthorized() {
		log.Warn("athlete deauthorized the application; the stored token is now dead")

		return
	}

	if event.ObjectType != objectTypeActivity {
		log.Debug("event ignored", "reason", "not an activity")

		return
	}

	// Only create and update can produce something worth naming. A delete has
	// nothing left to name.
	if event.AspectType != aspectCreate && event.AspectType != aspectUpdate {
		log.Debug("event ignored", "reason", "aspect type not handled")

		return
	}

	if h.athleteID != 0 && event.OwnerID != h.athleteID {
		log.Warn("event ignored", "reason", "owner is not the configured athlete")

		return
	}

	if event.ObjectID == 0 {
		log.Warn("event ignored", "reason", "missing activity id")

		return
	}

	// An activity is named at most once, ever. This is also what makes the
	// update event caused by our own rename a no-op rather than a second pass.
	title, named, err := h.named.Named(ctx, event.OwnerID, event.ObjectID)
	if err != nil {
		log.Error("could not check the named log", "error", err)

		return
	}

	if named {
		log.Info("event ignored", "reason", "activity already named",
			"title", logsafe.String(title))

		return
	}

	pending := store.Pending{
		AthleteID:    event.OwnerID,
		ActivityID:   event.ObjectID,
		Aspect:       event.AspectType,
		EnqueuedAt:   h.now(),
		ProcessAfter: h.processAfter(event),
	}

	added, err := h.queue.Enqueue(ctx, pending)
	if err != nil {
		// The acknowledgement has already gone out, so Strava will not retry.
		// Log loudly: the cost is one activity keeping its default title.
		log.Error("could not queue the activity", "error", err)

		return
	}

	if !added {
		log.Info("event collapsed into an existing queue entry")

		return
	}

	log.Info("activity queued", "process_after", pending.ProcessAfter)
}

// processAfter anchors the delay on Strava's event time, so a redelivery of the
// same event lands on the same deadline rather than pushing it back.
//
// The event time is only believed when it is in the past and recent. Without
// the floor, an event_time of 1 would put the deadline in 1970 and the activity
// would be due on the very next sweep — turning off the wait that is the whole
// reason this service runs last. A time in the future is ignored for the mirror
// image of that reason.
func (h *Handler) processAfter(event Event) time.Time {
	now := h.now()
	base := now

	if event.EventTime > 0 {
		eventTime := time.Unix(event.EventTime, 0).UTC()
		if eventTime.Before(base) && now.Sub(eventTime) <= maxEventAge {
			base = eventTime
		}
	}

	return base.Add(h.delay)
}

// equalSecret compares two secrets in constant time.
//
// Both sides are hashed first so the comparison is over fixed-size inputs:
// subtle.ConstantTimeCompare is constant time in the contents but returns early
// on a length mismatch, which would leak the token's length.
func equalSecret(got, want string) bool {
	gotSum := sha256.Sum256([]byte(got))
	wantSum := sha256.Sum256([]byte(want))

	return subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) == 1
}
