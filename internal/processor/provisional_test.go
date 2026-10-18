package processor

import (
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// The rides these tests are built from: two short afternoon errands, uploaded
// a second apart and still untagged when the process delay elapsed.
//
// ActivityFix sets the commute flag after the upload, and Strava sends no
// webhook event for that change — its updates carry title, type and private
// and nothing else — so a skip made final on the first sweep that sees such a
// ride leaves it at "Afternoon Ride" with nothing left to bring it back.
const (
	firstErrandID  = 901
	secondErrandID = 902

	provisionalAthlete = 4242
)

// The wall clock these tests step through. Every instant is derived from it,
// so the arithmetic the deadline does is visible in the test rather than
// hidden in a duration.
var (
	firstEnqueuedAt  = time.Date(2026, 9, 7, 16, 59, 43, 0, time.UTC)
	secondEnqueuedAt = time.Date(2026, 9, 7, 16, 59, 44, 0, time.UTC)

	// commuteTaggedAt is when ActivityFix sets the flag: after the delay has
	// elapsed and after the sweep that first looked at the ride.
	commuteTaggedAt = time.Date(2026, 9, 7, 17, 15, 7, 0, time.UTC)
)

func sweepAt(hour, minute int) time.Time {
	return time.Date(2026, 9, 7, hour, minute, 0, 0, time.UTC)
}

// smallErrand is one of the two rides, below both sport thresholds and at its
// Strava default title.
func smallErrand(id int64, distance float64, movingTime int) strava.Activity {
	return strava.Activity{
		ID:             id,
		Name:           "Afternoon Ride",
		SportType:      "Ride",
		Distance:       distance,
		MovingTime:     movingTime,
		StartDateLocal: time.Date(2026, 9, 7, 16, 40, 0, 0, time.UTC),
		Athlete: struct {
			ID int64 `json:"id"`
		}{ID: provisionalAthlete},
	}
}

// provisionalHarness is the two rides, queued from their create events, with
// a clock the test steps and an optional instant at which the fake starts
// reporting the commute flag.
//
// tagAt is consulted on every fetch rather than applied between sweeps, so
// the flag appears at the instant it appears rather than at a sweep boundary:
// 17:15:07 is after the 17:15 sweep read the activity and before the 17:20
// one does.
type provisionalHarness struct {
	*harness

	clock time.Time
}

func newProvisionalHarness(t *testing.T, window time.Duration, tagAt time.Time) *provisionalHarness {
	t.Helper()

	prov := &provisionalHarness{clock: sweepAt(17, 10)}

	prov.harness = newHarness(t, true, func(d *Deps) {
		d.ProvisionalSkipWindow = window
		d.Now = func() time.Time { return prov.clock }
	})

	api := prov.strava
	api.byID = map[int64]strava.Activity{
		firstErrandID:  smallErrand(firstErrandID, 2900, 600),
		secondErrandID: smallErrand(secondErrandID, 2700, 544),
	}

	if !tagAt.IsZero() {
		// Runs inside GetActivity, before the byID lookup, so the flag is
		// visible to the very fetch that crosses the instant.
		api.getHook = func() {
			if prov.clock.Before(tagAt) {
				return
			}

			for id, activity := range api.byID {
				activity.Commute = true
				api.byID[id] = activity
			}
		}
	}

	for id, enqueuedAt := range map[int64]time.Time{
		firstErrandID:  firstEnqueuedAt,
		secondErrandID: secondEnqueuedAt,
	} {
		if _, err := prov.store.Enqueue(t.Context(), store.Pending{
			AthleteID: provisionalAthlete, ActivityID: id, Aspect: "create",
			EnqueuedAt:   enqueuedAt,
			ProcessAfter: enqueuedAt.Add(10 * time.Minute),
		}); err != nil {
			t.Fatalf("Enqueue %d: %v", id, err)
		}
	}

	return prov
}

// sweep runs one sweep with the clock moved to the given instant.
func (p *provisionalHarness) sweep(t *testing.T, at time.Time) Result {
	t.Helper()

	p.clock = at

	result, err := p.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep at %s: %v", at.Format(time.TimeOnly), err)
	}

	return result
}

func (p *provisionalHarness) queued(t *testing.T) int {
	t.Helper()

	n, err := p.store.Len(t.Context())
	if err != nil {
		t.Fatalf("Len: %v", err)
	}

	return n
}

func (p *provisionalHarness) getCalls() int {
	p.strava.mu.Lock()
	defer p.strava.mu.Unlock()

	return p.strava.getCalls
}

// The race the window closes: a ride tagged after the sweep that first saw it
// is still named, from the queue entry that was kept.
//
// Both rides are below the sport thresholds and untagged at 17:10 and at
// 17:15. ActivityFix tags them at 17:15:07, and the 17:20 sweep — reading
// Strava again before it decides — reaches the errand tier and names them.
func TestAProvisionalSkipIsNamedOnceTheCommuteFlagArrives(t *testing.T) {
	t.Parallel()

	h := newProvisionalHarness(t, time.Hour, commuteTaggedAt)

	for _, at := range []time.Time{sweepAt(17, 10), sweepAt(17, 15)} {
		result := h.sweep(t, at)

		if result.Provisional != 2 || result.Skipped != 0 || result.Named != 0 {
			t.Errorf("the %s sweep = %+v, want two provisional and nothing else",
				at.Format(time.TimeOnly), result)
		}

		if n := h.queued(t); n != 2 {
			t.Errorf("%d entries queued after the %s sweep, want 2 — a provisional "+
				"skip that is dequeued has nothing left to bring it back",
				n, at.Format(time.TimeOnly))
		}

		if writes := h.strava.writes(); len(writes) != 0 {
			t.Errorf("an untagged ride was written at %s: %+v", at.Format(time.TimeOnly), writes)
		}
	}

	result := h.sweep(t, sweepAt(17, 20))

	if result.Named != 2 || result.Provisional != 0 || result.Skipped != 0 {
		t.Errorf("the 17:20 sweep = %+v, want two namings", result)
	}

	if n := h.queued(t); n != 0 {
		t.Errorf("%d entries left after both rides were named, want 0", n)
	}

	// The errand pool is picked from the activity ID, so the titles are the
	// ones a replay would produce again.
	want := map[string]bool{"In die Stadt": true, "Stadtrunde": true}

	writes := h.strava.writes()
	if len(writes) != 2 {
		t.Fatalf("%d PUTs, want 2: %+v", len(writes), writes)
	}

	for _, write := range writes {
		if !want[write.name] {
			t.Errorf("title %q, want one of the errand templates %v", write.name, want)
		}
	}

	if h.provider.calls != 0 {
		t.Errorf("the LLM was called %d times for an errand", h.provider.calls)
	}
}

// The control on the timeline above: a ride nobody ever tags is finalized when
// the window closes rather than re-checked forever.
//
// The deadlines are 17:59:43 and 17:59:44, so the 17:55 sweep still re-checks
// and the 18:00 one finalizes. The sweep after that must find nothing left to
// read, which is what bounds the cost: one Strava read per sweep per ride
// still provisional, and no more once the window has closed.
func TestAnUntaggedProvisionalSkipIsFinalizedWhenTheWindowCloses(t *testing.T) {
	t.Parallel()

	h := newProvisionalHarness(t, time.Hour, time.Time{})

	sweeps := []time.Time{
		sweepAt(17, 10), sweepAt(17, 15), sweepAt(17, 20), sweepAt(17, 25),
		sweepAt(17, 30), sweepAt(17, 35), sweepAt(17, 40), sweepAt(17, 45),
		sweepAt(17, 50), sweepAt(17, 55),
	}

	for _, at := range sweeps {
		result := h.sweep(t, at)

		if result.Provisional != 2 || result.Skipped != 0 {
			t.Errorf("the %s sweep = %+v, want two provisional", at.Format(time.TimeOnly), result)
		}
	}

	result := h.sweep(t, sweepAt(18, 0))

	if result.Skipped != 2 || result.Provisional != 0 {
		t.Errorf("the 18:00 sweep = %+v, want two final skips", result)
	}

	if n := h.queued(t); n != 0 {
		t.Errorf("%d entries left after the window closed, want 0", n)
	}

	// One read per sweep per ride, for the ten re-checks plus the sweep that
	// finalized: 11 each, 22 for the pair.
	const wantGets = 22

	t.Logf("Strava reads for the pair across %d sweeps: %d", len(sweeps)+1, h.getCalls())

	if got := h.getCalls(); got != wantGets {
		t.Errorf("%d Strava reads, want %d", got, wantGets)
	}

	// Nothing is left to read: the entries are gone, so the count does not
	// move. Without the deadline the previous sweep would have kept them and
	// this one would spend two more reads.
	if result := h.sweep(t, sweepAt(18, 5)); result.Due != 0 {
		t.Errorf("the 18:05 sweep = %+v, want an empty queue", result)
	}

	if got := h.getCalls(); got != wantGets {
		t.Errorf("%d Strava reads after a sweep with nothing due, want %d", got, wantGets)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("a ride nobody tagged was written: %+v", writes)
	}
}

// A skip for any other reason is final on first sight, at one read.
//
// The walk is a tier-1 sport type and the small ride carries Xert's title:
// another tool has already had its say about it, and a commute flag arriving
// later would not change that. Narrowing the re-checks to rides still at a
// Strava default is what keeps the cost to the case the race can reach.
func TestANonProvisionalSkipIsDequeuedOnFirstSight(t *testing.T) {
	t.Parallel()

	h := newProvisionalHarness(t, time.Hour, time.Time{})

	walk := smallErrand(firstErrandID, 2900, 600)
	walk.SportType = "Walk"
	walk.Name = "Afternoon Walk"

	xert := smallErrand(secondErrandID, 2700, 544)
	xert.Name = "Difficult Mixed Breakaway Specialist Ride"

	h.strava.byID = map[int64]strava.Activity{
		firstErrandID:  walk,
		secondErrandID: xert,
	}

	result := h.sweep(t, sweepAt(17, 10))

	if result.Skipped != 2 || result.Provisional != 0 {
		t.Errorf("the 17:10 sweep = %+v, want two final skips", result)
	}

	if n := h.queued(t); n != 0 {
		t.Errorf("%d entries left, want 0 — neither skip can be reconsidered", n)
	}

	if got := h.getCalls(); got != 2 {
		t.Errorf("%d Strava reads, want one per activity", got)
	}
}

// The deadline itself finalizes, rather than buying one more sweep.
//
// A sweep landing exactly on the deadline is the boundary the arithmetic can
// get wrong in either direction, and the cost bound counts on this one: the
// re-check span is the window minus the process delay, and an off-by-one here
// adds a Strava read per provisional ride.
func TestTheDeadlineItselfFinalizesTheSkip(t *testing.T) {
	t.Parallel()

	h := newProvisionalHarness(t, time.Hour, time.Time{})

	// Only the first ride, so the instant under test is exactly its deadline
	// rather than a second past the other's.
	if err := h.store.Remove(t.Context(), provisionalAthlete, secondErrandID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	deadline := firstEnqueuedAt.Add(time.Hour)

	result := h.sweep(t, deadline)

	if result.Skipped != 1 || result.Provisional != 0 {
		t.Errorf("the sweep at the deadline %s = %+v, want one final skip",
			deadline.Format(time.TimeOnly), result)
	}

	if n := h.queued(t); n != 0 {
		t.Errorf("%d entries left at the deadline, want 0", n)
	}
}

// The window is the rollback knob: zero disables the re-checks, restoring the
// pre-feature behavior with no release. An unset variable is not zero — it
// gets the shipped default, which internal/app asserts through config.Load.
func TestAZeroWindowMakesEverySkipFinal(t *testing.T) {
	t.Parallel()

	h := newProvisionalHarness(t, 0, commuteTaggedAt)

	result := h.sweep(t, sweepAt(17, 10))

	if result.Skipped != 2 || result.Provisional != 0 {
		t.Errorf("the 17:10 sweep = %+v, want two final skips", result)
	}

	if n := h.queued(t); n != 0 {
		t.Errorf("%d entries left with the window disabled, want 0", n)
	}
}
