package processor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/geo"
	"github.com/jkreileder/titelheld/internal/store"
)

// faultyStore fails one operation and delegates the rest.
//
// The store is the only thing standing between this service and a double
// rename, so each of its failures needs its own answer rather than a shared
// one: a queue that cannot be read aborts the sweep, a named log that cannot
// be read or written must stop the write, and a dequeue that fails must not
// undo work already done.
type faultyStore struct {
	store.Store

	dueErr       error
	namedErr     error
	markNamedErr error
	removeErr    error
}

func (f *faultyStore) Due(ctx context.Context, at time.Time) ([]store.Pending, error) {
	if f.dueErr != nil {
		return nil, f.dueErr
	}

	return f.Store.Due(ctx, at)
}

func (f *faultyStore) Named(ctx context.Context, athleteID, activityID int64) (string, bool, error) {
	if f.namedErr != nil {
		return "", false, f.namedErr
	}

	return f.Store.Named(ctx, athleteID, activityID)
}

func (f *faultyStore) MarkNamed(ctx context.Context, athleteID, activityID int64, title string) error {
	if f.markNamedErr != nil {
		return f.markNamedErr
	}

	return f.Store.MarkNamed(ctx, athleteID, activityID, title)
}

func (f *faultyStore) Remove(ctx context.Context, athleteID, activityID int64) error {
	if f.removeErr != nil {
		return f.removeErr
	}

	return f.Store.Remove(ctx, athleteID, activityID)
}

// A queue that cannot be read aborts the sweep and says why.
func TestAnUnreadableQueueAbortsTheSweep(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("firestore: unavailable")

	h := newHarness(t, true, nil)
	h.proc.deps.Store = &faultyStore{Store: h.store, dueErr: sentinel}

	if _, err := h.proc.Sweep(t.Context()); !errors.Is(err, sentinel) {
		t.Fatalf("Sweep error %v, want the store's own error", err)
	}
}

// A named log that cannot be read must not lead to a write.
//
// Not knowing whether an activity has been named is the one case where doing
// nothing is clearly right: naming it might be a second rename, and the entry
// stays queued so the next sweep can decide with a working store.
func TestAnUnreadableNamedLogPreventsAWrite(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.proc.deps.Store = &faultyStore{Store: h.store, namedErr: errors.New("firestore: deadline exceeded")}
	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("result %+v, want one failure", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("%d PUTs without knowing whether the activity was named: %+v", len(writes), writes)
	}

	if due, _ := h.store.Due(t.Context(), h.now); len(due) != 1 {
		t.Errorf("%d entries queued, want the activity kept for a retry", len(due))
	}
}

// A named log that cannot be written must not lead to a write either.
//
// This is the ordering in the pipeline doing its job. The log is written
// first precisely so that this case ends with no rename at all rather than
// with a rename nothing recorded — the latter would be renamed again on the
// next sweep, and again after that.
func TestAFailedNamedLogPreventsTheRename(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.proc.deps.Store = &faultyStore{Store: h.store, markNamedErr: errors.New("firestore: aborted")}
	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("result %+v, want one failure", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("%d PUTs that nothing recorded: %+v", len(writes), writes)
	}
}

// A failed dequeue is logged, not escalated.
//
// The activity has been named and recorded by this point. Failing the sweep
// would not un-name it, and the named log already stops the retry that the
// still-queued entry will cause.
func TestAFailedDequeueDoesNotUndoTheNaming(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.proc.deps.Store = &faultyStore{Store: h.store, removeErr: errors.New("firestore: aborted")}
	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Named != 1 {
		t.Errorf("result %+v, want the activity counted as named", result)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Fatalf("%d PUTs, want 1: %+v", len(writes), writes)
	}

	// The entry is still queued, and the named log is what stops the retry.
	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Errorf("%d PUTs after the retry, want still 1: %+v", len(writes), writes)
	}
}

// A rename Strava rejects leaves the activity queued.
func TestAFailedRenameIsReportedAndRetried(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.strava.updateErr = errors.New("strava: 500")
	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("result %+v, want one failure", result)
	}

	if due, _ := h.store.Due(t.Context(), h.now); len(due) != 1 {
		t.Errorf("%d entries queued, want the activity kept for a retry", len(due))
	}
}

// An LLM that errors leaves the activity queued rather than naming it badly.
func TestAnLLMFailureLeavesTheActivityQueued(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.provider.err = errors.New("vertex: 429")
	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("result %+v, want one failure", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("%d PUTs with no title from the model: %+v", len(writes), writes)
	}
}

// A cancelled sweep stops between activities, not part-way through one.
//
// Cloud Run gives a container a short grace period on shutdown. Stopping at an
// activity boundary means the worst case is an entry left queued, never a
// rename sent with nothing recorded.
func TestACancelledSweepStopsAtAnActivityBoundary(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)

	for _, id := range []int64{777, 778, 779} {
		if _, err := h.store.Enqueue(t.Context(), store.Pending{
			AthleteID: 4242, ActivityID: id, Aspect: "create",
			EnqueuedAt:   h.now.Add(-time.Hour),
			ProcessAfter: h.now.Add(-time.Minute),
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := h.proc.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Due != 3 {
		t.Errorf("Due is %d, want all 3 reported as eligible", result.Due)
	}

	if !result.Cancelled {
		t.Error("the sweep did not report that it stopped early")
	}

	if result.Named+result.Skipped+result.Failed != 0 {
		t.Errorf("result %+v, want nothing processed", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("%d PUTs after cancellation: %+v", len(writes), writes)
	}
}

// With no titles configured, the commute safety net still has an answer.
//
// Config is data and every field has to be safe at its zero value, so an
// athlete who never set these gets German defaults rather than an empty title.
func TestTheCommuteSafetyNetHasDefaultsOfItsOwn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		direction classifier.Direction
		want      string
	}{
		{name: "to work", direction: classifier.DirectionToWork, want: "Zur Arbeit"},
		{name: "to home", direction: classifier.DirectionToHome, want: "Nach Hause"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Deliberately blank, which is what an unconfigured athlete has.
			cfg := classifier.Config{Home: syntheticHome, Work: syntheticWork}

			got := commuteTitle(classifier.Decision{Direction: tc.direction}, cfg)
			if got != tc.want {
				t.Errorf("title %q, want %q", got, tc.want)
			}
		})
	}
}

// A ride whose geography resolves to nothing is named anyway.
func TestAnEmptyGeographyStillNames(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, func(d *Deps) { d.Geo = fakeGeo{summary: geo.Summary{}} })
	h.strava.activity.Map.SummaryPolyline = "_p~iF~ps|U"

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Errorf("%d PUTs, want 1: %+v", len(writes), writes)
	}
}
