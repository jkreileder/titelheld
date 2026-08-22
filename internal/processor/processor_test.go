package processor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/geo"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStrava stands in for the API. It records every write, which is what the
// idempotency criterion is asserted against.
type fakeStrava struct {
	mu sync.Mutex

	activity strava.Activity

	getErr    error
	updateErr error

	// getCalls counts fetches; puts records each write in order.
	getCalls int
	puts     []put

	// gearName is what GetGear answers with, and gearCalls counts how often
	// it was asked — the name is cached, so "once" is the assertion.
	gearName  string
	gearErr   error
	gearCalls int

	// getErrFor fails the fetch of specific activity IDs, so a test can make
	// re-reading history fail while the activity being named still loads.
	getErrFor map[int64]error

	// byID serves distinct activities per ID. Without it every ID answers
	// with the one shared activity, so naming any of them retitles all of
	// them and the classifier declines the rest.
	byID map[int64]strava.Activity
}

type put struct {
	name        string
	description string
	hadDesc     bool
}

func (f *fakeStrava) GetActivity(_ context.Context, id int64) (*strava.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.getCalls++

	if f.getErr != nil {
		return nil, f.getErr
	}

	if err, ok := f.getErrFor[id]; ok {
		return nil, err
	}

	if activity, ok := f.byID[id]; ok {
		return &activity, nil
	}

	copied := f.activity

	return &copied, nil
}

func (f *fakeStrava) UpdateActivityName(_ context.Context, id int64, name string) (*strava.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updateErr != nil {
		return nil, f.updateErr
	}

	f.puts = append(f.puts, put{name: name})

	if activity, ok := f.byID[id]; ok {
		activity.Name = name
		f.byID[id] = activity
	} else {
		f.activity.Name = name
	}

	return &f.activity, nil
}

func (f *fakeStrava) UpdateActivityNameAndDescription(
	_ context.Context, _ int64, name, description string,
) (*strava.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updateErr != nil {
		return nil, f.updateErr
	}

	f.puts = append(f.puts, put{name: name, description: description, hadDesc: true})
	f.activity.Name = name
	f.activity.Description = description

	return &f.activity, nil
}

// GetGear answers the gear lookup a franchise needs.
func (f *fakeStrava) GetGear(_ context.Context, gearID string) (strava.Gear, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gearCalls++

	if f.gearErr != nil {
		return strava.Gear{}, f.gearErr
	}

	return strava.Gear{ID: gearID, Name: f.gearName}, nil
}

func (f *fakeStrava) writes() []put {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]put(nil), f.puts...)
}

// fakeProvider is the LLM. No live call ever happens in a test.
//
// Left to itself it is a model that does what it is told: when the prompt
// offers a franchise entry it returns that entry as the title, so a test about
// something else does not spend three calls and end up asserting against the
// prompt of the attempt that gave up on the series. A test about a model that
// ignores the entry sets ignoreFranchise, and one about a particular title
// sets response.
type fakeProvider struct {
	response string
	err      error
	calls    int

	// ignoreFranchise makes the model return its ordinary title even when an
	// entry was offered — which is the case the re-offer exists for.
	ignoreFranchise bool
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(_ context.Context, prompt naming.Prompt) (string, error) {
	f.calls++

	if f.err != nil {
		return "", f.err
	}

	if f.response != "" {
		return f.response, nil
	}

	if entry := franchiseEntryOf(prompt); entry != "" && !f.ignoreFranchise {
		return `{"title":"` + entry + `","language":"en"}`, nil
	}

	return `{"title":"Musterrunde am Musterbach","language":"de"}`, nil
}

// franchiseEntryOf reads back the entry a prompt offers, if it offers one.
//
// Parsed out of the prompt rather than passed in beside it, so what a fake
// model can comply with is exactly what the real one is shown.
func franchiseEntryOf(prompt naming.Prompt) string {
	const marker = "- This ride continues a series. The next entry is: "

	for line := range strings.Lines(prompt.User) {
		if entry, found := strings.CutPrefix(strings.TrimRight(line, "\n"), marker); found {
			return entry
		}
	}

	return ""
}

type fakeGeo struct {
	summary geo.Summary
	err     error

	// calls is a pointer because Deps takes the Geographer by value, and a
	// test that asserts an errand was never geocoded needs the count to
	// survive the copy.
	calls *int
	mu    *sync.Mutex
}

func (f fakeGeo) Describe(_ context.Context, _ string) (geo.Summary, error) {
	if f.calls != nil {
		f.mu.Lock()
		*f.calls++
		f.mu.Unlock()
	}

	return f.summary, f.err
}

// sportRide is a tier-5 activity at its Strava default title.
func sportRide() strava.Activity {
	return strava.Activity{
		ID:             777,
		Name:           "Afternoon Gravel Ride",
		SportType:      "GravelRide",
		Distance:       67638.5,
		MovingTime:     10876,
		TotalElevGain:  540,
		AverageSpeed:   6.2,
		StartDateLocal: time.Date(2026, 8, 15, 14, 30, 0, 0, time.UTC),
		Athlete: struct {
			ID int64 `json:"id"`
		}{ID: 4242},
	}
}

type harness struct {
	proc     *Processor
	strava   *fakeStrava
	provider *fakeProvider
	store    store.Store

	// now is the same instant the processor is given, so a test can ask the
	// store what is still due after a sweep.
	now time.Time

	geo fakeGeo
}

// geoCalls is how many times the geocoder was asked.
func (h *harness) geoCalls() int {
	h.geo.mu.Lock()
	defer h.geo.mu.Unlock()

	return *h.geo.calls
}

func newHarness(t *testing.T, writes bool, mutate func(*Deps)) *harness {
	t.Helper()

	memory := store.NewMemory()
	api := &fakeStrava{activity: sportRide()}
	provider := &fakeProvider{}
	now := time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)
	geocoder := fakeGeo{
		summary: geo.Summary{Region: "Musterregion"},
		calls:   new(int),
		mu:      &sync.Mutex{},
	}

	deps := Deps{
		Store:         memory,
		Activities:    api,
		Geo:           geocoder,
		Provider:      provider,
		Classifier:    classifier.DefaultConfig(),
		Validator:     naming.NewValidator(naming.DefaultBannedWords()),
		WritesEnabled: writes,
		Logger:        quiet(),
		Now:           func() time.Time { return now },
	}

	if mutate != nil {
		mutate(&deps)
	}

	proc, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &harness{
		proc: proc, strava: api, provider: provider,
		store: memory, now: now, geo: geocoder,
	}
}

func (h *harness) enqueue(t *testing.T, aspect string) {
	t.Helper()

	if _, err := h.store.Enqueue(t.Context(), store.Pending{
		AthleteID: 4242, ActivityID: 777, Aspect: aspect,
		EnqueuedAt:   time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC),
		ProcessAfter: time.Date(2026, 8, 15, 15, 10, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

// Acceptance criterion: replaying the same create event and the self-caused
// update event results in exactly one PUT, the attribution appears exactly
// once, and third-party content survives byte-for-byte.
//
// Two independent things prevent the second write here, and this test asserts
// the outcome rather than either mechanism: the named log says the activity is
// done, and the title is no longer a Strava default so the classifier's gate
// declines it anyway. TestNamedLogAloneStopsASecondWrite isolates the first,
// because a test that passes for two reasons proves neither.
func TestIdempotencyAcrossReplayAndSelfCausedEvent(t *testing.T) {
	t.Parallel()

	thirdParty := "Xert: Difficult\r\n\r\nmyWindsock — CdA 0,31\t\nmybiketraffic: 87 🚗\n\n  trailing  \n"

	h := newHarness(t, true, nil)
	h.strava.activity.Description = thirdParty

	// The create event.
	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// A replay of the same create event, and the update event our own rename
	// caused. Both arrive after the activity has been named.
	h.enqueue(t, "create")
	h.enqueue(t, "update")

	for range 2 {
		if _, err := h.proc.Sweep(t.Context()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}

	writes := h.strava.writes()
	if len(writes) != 1 {
		t.Fatalf("%d PUTs, want exactly 1: %+v", len(writes), writes)
	}

	if !writes[0].hadDesc {
		t.Fatal("the write carried no description, so nothing was attributed")
	}

	if got := strings.Count(writes[0].description, "github.com/jkreileder/titelheld"); got != 1 {
		t.Errorf("attribution appears %d times, want 1:\n%s", got, writes[0].description)
	}

	suffix, ok := strings.CutPrefix(writes[0].description, naming.Attribution+"\n\n")
	if !ok {
		t.Fatalf("description does not begin with the attribution: %q", writes[0].description)
	}

	if suffix != thirdParty {
		t.Errorf("third-party content was altered:\n old: %q\n new: %q", thirdParty, suffix)
	}
}

// Dry run runs the whole pipeline — the LLM really is called — and writes
// nothing.
func TestDryRunNamesButDoesNotWrite(t *testing.T) {
	t.Parallel()

	h := newHarness(t, false, nil)
	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Named != 1 {
		t.Errorf("result = %+v, want one named", result)
	}

	if h.provider.calls != 1 {
		t.Errorf("the LLM was called %d times; dry run must still produce a would-be title", h.provider.calls)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("dry run wrote to Strava: %+v", writes)
	}

	// Nothing is recorded either, so flipping DRY_RUN off later still names it.
	if _, named, _ := h.store.Named(t.Context(), 4242, 777); named {
		t.Error("dry run marked the activity as named")
	}

	// And the entry is still queued, which is the half of that claim the
	// store cannot make on its own.
	due, err := h.store.Due(t.Context(), h.now)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	if len(due) != 1 {
		t.Fatalf("%d entries queued after a dry run, want the activity kept", len(due))
	}
}

// Turning writes on names what dry run only described.
//
// This is the claim the comment above makes, proven rather than asserted: a
// dry-run sweep must leave the queue exactly as it found it, or the review
// window silently eats the rides it was opened to observe — nothing records
// them as named, and nothing is left to name them afterwards, so they keep
// their Strava default forever.
func TestFlippingWritesOnNamesWhatDryRunDescribed(t *testing.T) {
	t.Parallel()

	h := newHarness(t, false, nil)
	h.enqueue(t, "create")

	// Several dry-run sweeps, as a paused-then-unpaused scheduler would do.
	for range 3 {
		if _, err := h.proc.Sweep(t.Context()); err != nil {
			t.Fatalf("dry run Sweep: %v", err)
		}
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Fatalf("dry run wrote to Strava: %+v", writes)
	}

	// One GET per sweep, not two. Dry run keeps the activity queued, so the
	// pipeline reruns on every sweep for as long as the window is open; a
	// second fetch per activity per sweep would spend the Strava budget on a
	// log line.
	h.strava.mu.Lock()
	gets := h.strava.getCalls
	h.strava.mu.Unlock()

	if gets != 3 {
		t.Errorf("%d GETs across 3 dry-run sweeps, want 3", gets)
	}

	// The operator flips DRY_RUN off.
	h.proc.deps.WritesEnabled = true

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	writes := h.strava.writes()
	if len(writes) != 1 {
		t.Fatalf("%d PUTs after enabling writes, want 1: %+v", len(writes), writes)
	}

	if writes[0].name != "Musterrunde am Musterbach" {
		t.Errorf("title %q, want the validated one", writes[0].name)
	}

	// And now it is done: the entry is gone and the named log has it.
	due, err := h.store.Due(t.Context(), h.now)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	if len(due) != 0 {
		t.Errorf("%d entries still queued after a real write", len(due))
	}

	if _, named, _ := h.store.Named(t.Context(), 4242, 777); !named {
		t.Error("the activity was not recorded as named")
	}
}

// One bad activity must not stall the sweep.
func TestOneFailureDoesNotStallTheSweep(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	api := &fakeStrava{activity: sportRide()}

	// The first activity fetched fails; the second succeeds.
	failing := &failFirstStrava{inner: api}

	proc, err := New(Deps{
		Store: memory, Activities: failing,
		Geo:        fakeGeo{},
		Provider:   &fakeProvider{},
		Classifier: classifier.DefaultConfig(),
		Validator:  naming.NewValidator(nil),
		Logger:     quiet(), WritesEnabled: true,
		Now: func() time.Time { return time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, id := range []int64{1, 2} {
		if _, err := memory.Enqueue(t.Context(), store.Pending{
			AthleteID: 4242, ActivityID: id, Aspect: "create",
			ProcessAfter: time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	result, err := proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep returned an error for a per-activity failure: %v", err)
	}

	if result.Due != 2 {
		t.Fatalf("result = %+v, want two due", result)
	}

	if result.Failed != 1 || result.Named != 1 {
		t.Errorf("result = %+v, want one failure and one success", result)
	}

	// The failed one is still queued for the next sweep; the other is gone.
	remaining, err := memory.Len(t.Context())
	if err != nil {
		t.Fatalf("Len: %v", err)
	}

	if remaining != 1 {
		t.Errorf("%d entries left in the queue, want the failed one only", remaining)
	}
}

// failFirstStrava fails the first GetActivity and then behaves.
type failFirstStrava struct {
	inner *fakeStrava
	calls int
}

func (f *failFirstStrava) GetGear(ctx context.Context, gearID string) (strava.Gear, error) {
	return f.inner.GetGear(ctx, gearID)
}

func (f *failFirstStrava) GetActivity(ctx context.Context, id int64) (*strava.Activity, error) {
	f.calls++

	if f.calls == 1 {
		return nil, errors.New("strava: unexpected status 429")
	}

	return f.inner.GetActivity(ctx, id)
}

func (f *failFirstStrava) UpdateActivityName(ctx context.Context, id int64, name string) (*strava.Activity, error) {
	return f.inner.UpdateActivityName(ctx, id, name)
}

func (f *failFirstStrava) UpdateActivityNameAndDescription(
	ctx context.Context, id int64, name, description string,
) (*strava.Activity, error) {
	return f.inner.UpdateActivityNameAndDescription(ctx, id, name, description)
}

// Attribution must never block a naming: a failed description fetch writes the
// title on its own.
func TestAttributionFailureDoesNotBlockTheTitle(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	api := &failSecondGet{inner: &fakeStrava{activity: sportRide()}}

	proc, err := New(Deps{
		Store: memory, Activities: api, Geo: fakeGeo{},
		Provider: &fakeProvider{}, Classifier: classifier.DefaultConfig(),
		Validator: naming.NewValidator(nil), WritesEnabled: true, Logger: quiet(),
		Now: func() time.Time { return time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := memory.Enqueue(t.Context(), store.Pending{
		AthleteID: 4242, ActivityID: 777, Aspect: "create",
		ProcessAfter: time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	writes := api.inner.writes()
	if len(writes) != 1 {
		t.Fatalf("%d PUTs, want the title to have gone out anyway", len(writes))
	}

	if writes[0].hadDesc {
		t.Error("a description was sent despite the fetch failing")
	}

	if writes[0].name == "" {
		t.Error("no title was written")
	}
}

// failSecondGet lets the classifier's fetch through and fails the one the
// writer makes for the description.
type failSecondGet struct {
	inner *fakeStrava
	calls int
}

func (f *failSecondGet) GetGear(ctx context.Context, gearID string) (strava.Gear, error) {
	return f.inner.GetGear(ctx, gearID)
}

func (f *failSecondGet) GetActivity(ctx context.Context, id int64) (*strava.Activity, error) {
	f.calls++

	if f.calls >= 2 {
		return nil, errors.New("strava: unexpected status 500")
	}

	return f.inner.GetActivity(ctx, id)
}

func (f *failSecondGet) UpdateActivityName(ctx context.Context, id int64, name string) (*strava.Activity, error) {
	return f.inner.UpdateActivityName(ctx, id, name)
}

func (f *failSecondGet) UpdateActivityNameAndDescription(
	ctx context.Context, id int64, name, description string,
) (*strava.Activity, error) {
	return f.inner.UpdateActivityNameAndDescription(ctx, id, name, description)
}

// A skipped activity leaves the queue rather than being retried forever.
func TestSkippedActivityIsDequeued(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.strava.activity.Name = "The Pink Panther Checks Inn" // a human title
	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Skipped != 1 || result.Named != 0 {
		t.Errorf("result = %+v, want one skip", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("a human-titled activity was written: %+v", writes)
	}

	if n, _ := h.store.Len(t.Context()); n != 0 {
		t.Errorf("%d entries left; a permanent skip should not be retried", n)
	}
}

// An unusable LLM response is a failure, not a bad title written to Strava.
func TestInvalidTitleIsNotWritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, func(d *Deps) {
		d.Provider = &fakeProvider{response: `{"title":"Epic Crushing Ride","language":"en"}`}
	})
	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("result = %+v, want a failure", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("a banned title was written: %+v", writes)
	}
}

// The named log alone must stop a second write.
//
// In the replay test the classifier also declines, because the activity now
// carries the title this service gave it. Here the title is put back to a
// Strava default before the replay, so the gate would let it through and only
// the named log is left to say no. That is the mechanism the spec names for
// self-caused events, and it is the one that still works if a human reverts
// the title by hand.
func TestNamedLogAloneStopsASecondWrite(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, named, err := h.store.Named(t.Context(), 4242, 777); err != nil || !named {
		t.Fatalf("the activity was not recorded as named (named=%v, err=%v)", named, err)
	}

	// Put the title back, so the classifier gate would allow another write.
	h.strava.mu.Lock()
	h.strava.activity.Name = "Afternoon Gravel Ride"
	h.strava.mu.Unlock()

	h.enqueue(t, "update")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Errorf("%d PUTs; the named log did not stop the second one", len(writes))
	}
}

// The named log is keyed on the queue's athlete, not the activity's.
//
// Strava's detailed activity carries athlete.id, but the named log is read
// with the athlete the webhook event named and was written with the one the
// activity reported. Those are the same number right up until they are not —
// a response without athlete.id makes Owner() zero — and then the record
// lands under athlete 0, the dedup read never finds it, and the next event
// for that activity renames it a second time.
func TestTheNamedLogIsKeyedOnTheQueuesAthlete(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)

	// A response that omits athlete.id, which is all it takes.
	h.strava.activity.Athlete.ID = 0
	h.strava.activity.AthleteID = 0

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Recorded under the athlete the queue named.
	if _, named, err := h.store.Named(t.Context(), 4242, 777); err != nil || !named {
		t.Fatalf("the named log was not written under athlete 4242 (named=%v, err=%v)", named, err)
	}

	// Put the title back so the classifier gate would allow another write,
	// leaving the named log as the only thing that can refuse.
	h.strava.mu.Lock()
	h.strava.activity.Name = "Afternoon Gravel Ride"
	h.strava.mu.Unlock()

	h.enqueue(t, "update")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Errorf("%d PUTs; the activity was renamed again under a different key", len(writes))
	}
}
