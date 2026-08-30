package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/store"
)

var testNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

const testVerifyToken = "test-verify-token"

// quietLogger keeps test output readable.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newHandler(t *testing.T, memory *store.Memory, athleteID int64) *Handler {
	t.Helper()

	handler, err := New(Config{
		VerifyToken: testVerifyToken,
		AthleteID:   athleteID,
		Delay:       10 * time.Minute,
		Queue:       memory,
		Named:       memory,
		Logger:      quietLogger(),
		Now:         func() time.Time { return testNow },
		AthleteTitle: func(title string) bool {
			// The production filter's shape, minimal: a Strava default is not
			// the athlete's, anything else is.
			return !classifier.IsDefaultTitle(title)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return handler
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()

	if _, err := New(Config{Queue: memory, Named: memory}); err == nil {
		t.Error("New without a verify token = nil error, want error")
	}
	if _, err := New(Config{VerifyToken: "t"}); err == nil {
		t.Error("New without a queue = nil error, want error")
	}

	handler, err := New(Config{VerifyToken: "t", Queue: memory, Named: memory})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if handler.logger == nil || handler.now == nil {
		t.Error("New left the logger or clock unset")
	}
}

func TestValidationHandshake(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, store.NewMemory(), 0)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/webhook/secret?hub.mode=subscribe&hub.verify_token="+testVerifyToken+
			"&hub.challenge=15f7d1a91c1f40f8a748fd134752feb3", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if got := body["hub.challenge"]; got != "15f7d1a91c1f40f8a748fd134752feb3" {
		t.Errorf("hub.challenge = %q, want the value echoed back exactly", got)
	}
}

func TestValidationRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		query    string
		wantCode int
	}{
		{
			name:     "wrong verify token",
			query:    "hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=abc",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "empty verify token",
			query:    "hub.mode=subscribe&hub.challenge=abc",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "wrong mode",
			query:    "hub.mode=unsubscribe&hub.verify_token=" + testVerifyToken + "&hub.challenge=abc",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "missing challenge",
			query:    "hub.mode=subscribe&hub.verify_token=" + testVerifyToken,
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := newHandler(t, store.NewMemory(), 0)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/webhook/secret?"+tt.query, nil))

			if recorder.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", recorder.Code, tt.wantCode)
			}
		})
	}
}

func TestUnsupportedMethod(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, store.NewMemory(), 0)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/webhook/secret", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", recorder.Code)
	}
	if got := recorder.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q", got)
	}
}

// post delivers an event and returns the recorder.
func post(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook/secret", strings.NewReader(body))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	return recorder
}

func TestEventQueuesTheActivity(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 4242)

	// Five minutes before the handler's clock: the upload happened, then the
	// event reached us.
	eventTime := testNow.Add(-5 * time.Minute)

	recorder := post(t, handler, `{
		"object_type": "activity",
		"object_id": 19755622151,
		"aspect_type": "create",
		"owner_id": 4242,
		"subscription_id": 999,
		"event_time": `+strconv.FormatInt(eventTime.Unix(), 10)+`
	}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	due, err := memory.Due(t.Context(), testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	if len(due) != 1 {
		t.Fatalf("queued %d entries, want 1", len(due))
	}

	pending := due[0]
	if pending.ActivityID != 19755622151 || pending.AthleteID != 4242 {
		t.Errorf("pending = %+v", pending)
	}
	if pending.Aspect != "create" {
		t.Errorf("Aspect = %q", pending.Aspect)
	}

	// The delay is anchored on Strava's event_time, not on arrival, so a
	// redelivery lands on the same deadline instead of pushing it back.
	want := eventTime.Add(10 * time.Minute)
	if !pending.ProcessAfter.Equal(want) {
		t.Errorf("ProcessAfter = %v, want %v", pending.ProcessAfter, want)
	}
}

func TestEventWithoutEventTimeUsesArrival(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	post(t, handler, `{"object_type":"activity","object_id":1,"aspect_type":"create","owner_id":7}`)

	due, _ := memory.Due(t.Context(), testNow.Add(time.Hour))
	if len(due) != 1 {
		t.Fatalf("queued %d entries, want 1", len(due))
	}

	if want := testNow.Add(10 * time.Minute); !due[0].ProcessAfter.Equal(want) {
		t.Errorf("ProcessAfter = %v, want %v", due[0].ProcessAfter, want)
	}
}

// A future event_time must not push the deadline out beyond the configured
// delay from now.
func TestFutureEventTimeIsIgnored(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	future := testNow.Add(24 * time.Hour).Unix()
	post(t, handler, `{"object_type":"activity","object_id":1,"aspect_type":"create",
		"owner_id":7,"event_time":`+strconv.FormatInt(future, 10)+`}`)

	due, _ := memory.Due(t.Context(), testNow.Add(time.Hour))
	if len(due) != 1 {
		t.Fatalf("queued %d entries, want 1", len(due))
	}

	if want := testNow.Add(10 * time.Minute); !due[0].ProcessAfter.Equal(want) {
		t.Errorf("ProcessAfter = %v, want %v", due[0].ProcessAfter, want)
	}
}

func TestRepeatedDeliveryIsIdempotent(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	body := `{"object_type":"activity","object_id":5,"aspect_type":"create","owner_id":7}`

	for range 3 {
		if code := post(t, handler, body).Code; code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}

	// The create plus the update another tool causes must be one unit of work.
	post(t, handler, `{"object_type":"activity","object_id":5,"aspect_type":"update","owner_id":7}`)

	count, _ := memory.Len(t.Context())
	if count != 1 {
		t.Errorf("queue holds %d entries, want 1", count)
	}
}

func TestAlreadyNamedActivityIsDropped(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	// This is the self-caused update: the rename we performed comes back as an
	// event, and the named log makes it a no-op.
	if err := memory.MarkNamed(t.Context(), store.Naming{
		AthleteID: 7, ActivityID: 5,
		Title: "The Pink Panther Strikes Again", At: time.Now(),
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	recorder := post(t, handler, `{"object_type":"activity","object_id":5,
		"aspect_type":"update","owner_id":7,"updates":{"title":"The Pink Panther Strikes Again"}}`)

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", recorder.Code)
	}

	count, _ := memory.Len(t.Context())
	if count != 0 {
		t.Errorf("queue holds %d entries, want 0", count)
	}
}

func TestIgnoredEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "athlete object",
			body: `{"object_type":"athlete","object_id":4242,"aspect_type":"update","owner_id":4242}`,
		},
		{
			name: "deauthorization",
			body: `{"object_type":"athlete","object_id":4242,"aspect_type":"update",
				"owner_id":4242,"updates":{"authorized":"false"}}`,
		},
		{
			name: "delete aspect",
			body: `{"object_type":"activity","object_id":5,"aspect_type":"delete","owner_id":4242}`,
		},
		{
			name: "another athlete",
			body: `{"object_type":"activity","object_id":5,"aspect_type":"create","owner_id":99}`,
		},
		{
			name: "missing activity id",
			body: `{"object_type":"activity","aspect_type":"create","owner_id":4242}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			memory := store.NewMemory()
			handler := newHandler(t, memory, 4242)

			// Every one of these is still acknowledged: Strava must not retry
			// an event we have deliberately ignored.
			if code := post(t, handler, tt.body).Code; code != http.StatusOK {
				t.Errorf("status = %d, want 200", code)
			}

			count, _ := memory.Len(t.Context())
			if count != 0 {
				t.Errorf("queue holds %d entries, want 0", count)
			}
		})
	}
}

func TestUndecodableBodyIsRejected(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	if code := post(t, handler, `{"object_type":`).Code; code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}

	count, _ := memory.Len(t.Context())
	if count != 0 {
		t.Errorf("queue holds %d entries, want 0", count)
	}
}

// failingStore lets the store errors be exercised.
type failingStore struct {
	*store.Memory

	namedErr   error
	enqueueErr error
}

func (f *failingStore) Named(ctx context.Context, athleteID, activityID int64) (store.NamedTitle, bool, error) {
	if f.namedErr != nil {
		return store.NamedTitle{}, false, f.namedErr
	}

	return f.Memory.Named(ctx, athleteID, activityID)
}

func (f *failingStore) Enqueue(ctx context.Context, pending store.Pending) (bool, error) {
	if f.enqueueErr != nil {
		return false, f.enqueueErr
	}

	return f.Memory.Enqueue(ctx, pending)
}

func TestStoreFailuresStillAcknowledge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		failing *failingStore
	}{
		{
			name:    "named log unavailable",
			failing: &failingStore{Memory: store.NewMemory(), namedErr: errors.New("unavailable")},
		},
		{
			name:    "queue unavailable",
			failing: &failingStore{Memory: store.NewMemory(), enqueueErr: errors.New("unavailable")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler, err := New(Config{
				VerifyToken: testVerifyToken,
				Queue:       tt.failing,
				Named:       tt.failing,
				Logger:      quietLogger(),
				Now:         func() time.Time { return testNow },
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			// The acknowledgement goes out before the store is touched, so a
			// store outage cannot turn into a non-200.
			body := `{"object_type":"activity","object_id":5,"aspect_type":"create","owner_id":7}`
			if code := post(t, handler, body).Code; code != http.StatusOK {
				t.Errorf("status = %d, want 200", code)
			}
		})
	}
}

func TestOversizedBodyIsBounded(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	// A body far past the cap is truncated mid-JSON and rejected rather than
	// read into memory in full.
	huge := `{"object_type":"activity","object_id":5,"aspect_type":"create","owner_id":7,"pad":"` +
		strings.Repeat("x", maxBodyBytes+1024) + `"}`

	if code := post(t, handler, huge).Code; code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestEqualSecret(t *testing.T) {
	t.Parallel()

	if !equalSecret("abc", "abc") {
		t.Error("equalSecret on identical values = false")
	}
	if equalSecret("abc", "abd") {
		t.Error("equalSecret on differing values = true")
	}
	// Different lengths must not short-circuit into a match.
	if equalSecret("abc", "abcdef") {
		t.Error("equalSecret across lengths = true")
	}
	if equalSecret("", "abc") {
		t.Error("equalSecret with an empty candidate = true")
	}
}

func TestEventDeauthorized(t *testing.T) {
	t.Parallel()

	deauth := Event{ObjectType: "athlete", Updates: map[string]string{"authorized": "false"}}
	if !deauth.Deauthorized() {
		t.Error("Deauthorized() = false for an athlete authorized:false event")
	}

	// The same update on an activity event is not a deauthorization. Treating
	// it as one would silently drop a nameable activity.
	onActivity := Event{ObjectType: "activity", Updates: map[string]string{"authorized": "false"}}
	if onActivity.Deauthorized() {
		t.Error("Deauthorized() = true for an activity event")
	}

	if (Event{ObjectType: "athlete", Updates: map[string]string{"title": "x"}}).Deauthorized() {
		t.Error("Deauthorized() = true for a title update")
	}
	if (Event{}).Deauthorized() {
		t.Error("Deauthorized() = true for an event with no updates")
	}
}

// brokenWriter fails on write so the response-encoding path is reachable.
type brokenWriter struct{ header http.Header }

func (b *brokenWriter) Header() http.Header {
	if b.header == nil {
		b.header = make(http.Header)
	}

	return b.header
}

func (b *brokenWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }
func (b *brokenWriter) WriteHeader(int)           {}

func TestValidationResponseWriteFailureIsLogged(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, store.NewMemory(), 0)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/webhook/secret?hub.mode=subscribe&hub.verify_token="+testVerifyToken+"&hub.challenge=abc", nil)

	// Must not panic when the client has gone away mid-handshake.
	handler.ServeHTTP(&brokenWriter{}, request)
}

// A writer that cannot flush must not stop the event being queued.
func TestIntakeWithoutFlushSupportStillQueues(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook/secret",
		strings.NewReader(`{"object_type":"activity","object_id":5,"aspect_type":"create","owner_id":7}`))

	handler.ServeHTTP(&brokenWriter{}, request)

	count, _ := memory.Len(t.Context())
	if count != 1 {
		t.Errorf("queue holds %d entries, want 1", count)
	}
}

// The acknowledgement is flushed before the work starts, which lets the client
// return and close the body — cancelling the request context. The queue write
// must not ride on that context: Strava has already been told 200, so a
// canceled write would drop the activity with no retry.
func TestIntakeSurvivesACancelledRequestContext(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/webhook/secret",
		strings.NewReader(`{"object_type":"activity","object_id":5,"aspect_type":"create","owner_id":7}`))

	handler.ServeHTTP(httptest.NewRecorder(), request)

	count, _ := memory.Len(t.Context())
	if count != 1 {
		t.Errorf("queue holds %d entries, want 1 — the work must outlive the request", count)
	}
}

// A bogus or ancient event_time must not collapse the delay. The wait is the
// reason this service runs last in the chain.
func TestAncientEventTimeCannotBypassTheDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		eventTime int64
	}{
		{name: "epoch", eventTime: 1},
		{name: "a year ago", eventTime: testNow.Add(-365 * 24 * time.Hour).Unix()},
		{name: "just past the trusted window", eventTime: testNow.Add(-25 * time.Hour).Unix()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			memory := store.NewMemory()
			handler := newHandler(t, memory, 0)

			post(t, handler, `{"object_type":"activity","object_id":5,"aspect_type":"create",
				"owner_id":7,"event_time":`+strconv.FormatInt(tt.eventTime, 10)+`}`)

			due, _ := memory.Due(t.Context(), testNow)
			if len(due) != 0 {
				t.Fatalf("%d entries are already due; the delay was bypassed", len(due))
			}

			all, _ := memory.Due(t.Context(), testNow.Add(time.Hour))
			if want := testNow.Add(10 * time.Minute); !all[0].ProcessAfter.Equal(want) {
				t.Errorf("ProcessAfter = %v, want %v", all[0].ProcessAfter, want)
			}
		})
	}
}

// A recent event_time is still honoured, so a redelivery keeps its deadline.
func TestRecentEventTimeIsStillHonoured(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 0)

	eventTime := testNow.Add(-3 * time.Hour)

	post(t, handler, `{"object_type":"activity","object_id":5,"aspect_type":"create",
		"owner_id":7,"event_time":`+strconv.FormatInt(eventTime.Unix(), 10)+`}`)

	due, _ := memory.Due(t.Context(), testNow.Add(time.Hour))
	if want := eventTime.Add(10 * time.Minute); !due[0].ProcessAfter.Equal(want) {
		t.Errorf("ProcessAfter = %v, want %v", due[0].ProcessAfter, want)
	}
}

// renamedActivity is the one activity the rename cases work on.
const renamedActivity = 777

// renameEvent is the update Strava sends when the athlete retitles an activity.
func renameEvent(title string) string {
	return fmt.Sprintf(
		`{"object_type":"activity","object_id":%d,"aspect_type":"update",`+
			`"owner_id":4242,"updates":{"title":%q}}`, renamedActivity, title)
}

// named seeds a row the way the sweep would have written one: source
// service, because that is the only kind a rename can arrive on.
func named(t *testing.T, memory *store.Memory, title string) time.Time {
	t.Helper()

	at := testNow.Add(-72 * time.Hour)

	if err := memory.MarkNamed(t.Context(), store.Naming{
		AthleteID: 4242, ActivityID: renamedActivity,
		Title: title, Language: "de", Source: store.SourceService, At: at,
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	return at
}

func row(t *testing.T, memory *store.Memory) store.NamedTitle {
	t.Helper()

	entry, ok, err := memory.Named(t.Context(), 4242, renamedActivity)
	if err != nil || !ok {
		t.Fatalf("Named: %v, %v", ok, err)
	}

	return entry
}

// The athlete's rename becomes the row, under their name.
//
// This is the systematic form of the one-off edit that repaired the row after
// a title was written that should never have been: a correction the athlete
// makes on their feed reaches the store by itself, joins RECENT so the model
// cannot invent it again, and becomes eligible to teach the examples.
func TestARenameBecomesTheAthletesRow(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 4242)
	at := named(t, memory, "Neun auf einen Streich")

	if code := post(t, handler, renameEvent("Windschief")).Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	entry := row(t, memory)

	if entry.Title != "Windschief" {
		t.Errorf("title = %q, want the athlete's", entry.Title)
	}

	if entry.Source != store.SourceHuman {
		t.Errorf("source = %q, want %q", entry.Source, store.SourceHuman)
	}

	// RECENT is ordered by this, so a rename must not move the ride.
	if !entry.NamedAt.Equal(at) {
		t.Errorf("named_at = %v, want the row's own %v", entry.NamedAt, at)
	}

	// Still named: the activity is final either way, and a rename must not
	// put it back in the queue.
	if n, _ := memory.Len(t.Context()); n != 0 {
		t.Errorf("%d queued; a renamed activity is still named", n)
	}
}

// An update carrying the title this service wrote changes nothing.
//
// This is the shape of our own write coming back round, which is the event the
// named log exists to absorb.
func TestOurOwnTitleComingBackChangesNothing(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 4242)
	named(t, memory, "Windschief")

	post(t, handler, renameEvent("Windschief"))

	if entry := row(t, memory); entry.Source != store.SourceService {
		t.Errorf("source = %q; a re-delivery rewrote the row", entry.Source)
	}
}

// An update that says nothing about the title changes nothing.
func TestAnUpdateWithoutATitleChangesNothing(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 4242)
	named(t, memory, "Windschief")

	post(t, handler, fmt.Sprintf(`{"object_type":"activity","object_id":%d,"aspect_type":"update",`+
		`"owner_id":4242,"updates":{"type":"Ride"}}`, renamedActivity))

	if entry := row(t, memory); entry.Source != store.SourceService {
		t.Errorf("source = %q; an unrelated update rewrote the row", entry.Source)
	}
}

// The override is idempotent per title: it fires once for a rename, and a
// re-delivery of the same event does nothing further.
func TestTheOverrideFiresOncePerRename(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 4242)
	named(t, memory, "Neun auf einen Streich")

	for range 3 {
		post(t, handler, renameEvent("Windschief"))
	}

	entry := row(t, memory)
	if entry.Title != "Windschief" || entry.Source != store.SourceHuman {
		t.Fatalf("row = %q/%q", entry.Title, entry.Source)
	}

	// A second, different rename is a second override — "once per rename",
	// not once ever.
	post(t, handler, renameEvent("Gegenwind bis Musterstadt"))

	if got := row(t, memory).Title; got != "Gegenwind bis Musterstadt" {
		t.Errorf("title = %q, want the second rename", got)
	}
}

// Resetting a title to a Strava default is not authorship.
//
// The row would otherwise teach the examples a title the athlete never wrote,
// and RECENT would forbid a name that is not a name.
func TestAResetToADefaultDoesNotBecomeTheAthletesRow(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	handler := newHandler(t, memory, 4242)
	named(t, memory, "Windschief")

	post(t, handler, renameEvent("Morning Ride"))

	entry := row(t, memory)
	if entry.Title != "Windschief" || entry.Source != store.SourceService {
		t.Errorf("row = %q/%q; a reset to a Strava default was recorded as the athlete's",
			entry.Title, entry.Source)
	}
}
