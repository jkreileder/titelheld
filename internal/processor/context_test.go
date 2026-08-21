package processor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/geo"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
)

// capturingProvider records the prompt it was given.
type capturingProvider struct {
	fakeProvider

	prompt naming.Prompt
}

func (c *capturingProvider) Complete(ctx context.Context, prompt naming.Prompt) (string, error) {
	c.prompt = prompt

	return c.fakeProvider.Complete(ctx, prompt)
}

// withCapture swaps in a provider that keeps the prompt.
func withCapture(h *harness) *capturingProvider {
	capture := &capturingProvider{}
	h.proc.deps.Provider = capture

	return capture
}

// Previously written titles reach the prompt, newest first.
//
// This is the whole point of the history: the prompt tells the model never to
// repeat a title under RECENT, and an empty RECENT makes that instruction
// meaningless.
func TestRecentTitlesReachThePrompt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	for index, title := range []string{"Erste Runde", "Zweite Runde", "Dritte Runde"} {
		if err := h.store.MarkNamed(t.Context(), store.Naming{
			AthleteID:  4242,
			ActivityID: int64(900 + index),
			Title:      title,
			Language:   "de",
			At:         h.now.Add(time.Duration(index) * time.Hour),
		}); err != nil {
			t.Fatalf("MarkNamed: %v", err)
		}
	}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	user := capture.prompt.User
	if !strings.Contains(user, "RECENT") {
		t.Fatalf("the prompt has no RECENT block:\n%s", user)
	}

	for _, title := range []string{"Erste Runde", "Zweite Runde", "Dritte Runde"} {
		if !strings.Contains(user, title) {
			t.Errorf("the prompt does not carry the previous title %q", title)
		}
	}

	// Newest first, so the ones most at risk of repetition come first.
	newest := strings.Index(user, "Dritte Runde")
	oldest := strings.Index(user, "Erste Runde")

	if newest > oldest {
		t.Error("recent titles reached the prompt oldest-first")
	}
}

// A ride on the franchise bike is offered the next entry in the series.
func TestAFranchiseRideIsOfferedTheNextEntry(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	h.strava.gearName = "Pink Panther"
	h.strava.activity.GearID = "b1234567"

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	first := naming.DefaultFranchises()[0].Titles[0]

	if !strings.Contains(capture.prompt.User, "FRANCHISE") {
		t.Fatalf("the prompt has no FRANCHISE block:\n%s", capture.prompt.User)
	}

	if !strings.Contains(capture.prompt.User, first) {
		t.Errorf("the prompt does not offer %q", first)
	}

	// And the position advanced, so the next ride gets the next film.
	position, err := h.store.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 1 {
		t.Errorf("position = %d, want 1 after one franchise naming", position)
	}
}

// A ride on a different bike gets no franchise.
func TestAnotherBikeGetsNoFranchise(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	h.strava.gearName = "Musterrad"
	h.strava.activity.GearID = "b7654321"

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if strings.Contains(capture.prompt.User, "FRANCHISE") {
		t.Errorf("a franchise was offered to another bike:\n%s", capture.prompt.User)
	}

	if position, _ := h.store.FranchisePosition(t.Context(), 4242, "pink-panther"); position != 0 {
		t.Errorf("the franchise advanced for another bike: position %d", position)
	}
}

// Dry run offers the franchise entry but does not consume it.
//
// The would-be title has to be the real one, or the review window is
// reviewing something the service would not actually write. Advancing would
// mean every dry-run sweep burned an entry, and the series would be gone
// before a single title was written.
func TestDryRunOffersAFranchiseWithoutConsumingIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, false, nil)
	capture := withCapture(h)

	h.strava.gearName = "Pink Panther"
	h.strava.activity.GearID = "b1234567"

	h.enqueue(t, "create")

	for range 3 {
		if _, err := h.proc.Sweep(t.Context()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}

	if !strings.Contains(capture.prompt.User, naming.DefaultFranchises()[0].Titles[0]) {
		t.Error("dry run did not offer the franchise entry")
	}

	position, err := h.store.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 0 {
		t.Errorf("dry run advanced the franchise to %d", position)
	}
}

// A gear lookup that fails does not stop the naming.
func TestAFailedGearLookupDoesNotBlockTheTitle(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)

	h.strava.gearErr = errors.New("strava: 500")
	h.strava.activity.GearID = "b1234567"

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Errorf("%d PUTs, want 1: a gear failure must not block a title", len(writes))
	}
}

// The gear name is read once and remembered.
func TestTheGearNameIsCached(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)

	h.strava.gearName = "Pink Panther"
	h.strava.activity.GearID = "b1234567"

	for index := range 3 {
		if _, err := h.store.Enqueue(t.Context(), store.Pending{
			AthleteID: 4242, ActivityID: int64(770 + index), Aspect: "create",
			EnqueuedAt:   h.now.Add(-time.Hour),
			ProcessAfter: h.now.Add(-time.Minute),
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	h.strava.mu.Lock()
	calls := h.strava.gearCalls
	h.strava.mu.Unlock()

	if calls != 1 {
		t.Errorf("%d gear lookups for one bike, want 1", calls)
	}
}

// A route ridden before is offered to the prompt as a callback.
func TestARepeatedRouteReachesThePrompt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	h.strava.activity.Map.SummaryPolyline = "_p~iF~ps|U_ulLnnqC_mqNvxq`@"

	// Ridden twice before, first on a date the callback should name.
	fingerprint := mustFingerprint(t, h.strava.activity.Map.SummaryPolyline)
	first := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)

	for _, at := range []time.Time{first, first.Add(240 * time.Hour)} {
		if _, err := h.store.RecordRoute(t.Context(), 4242, fingerprint, at); err != nil {
			t.Fatalf("RecordRoute: %v", err)
		}
	}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	user := capture.prompt.User
	if !strings.Contains(user, "Route history") {
		t.Fatalf("the prompt has no route history:\n%s", user)
	}

	if !strings.Contains(user, "3 May 2026") {
		t.Errorf("the prompt does not name the first ride:\n%s", user)
	}

	// This ride is the third.
	if !strings.Contains(user, "3 times") {
		t.Errorf("the prompt does not say how many times:\n%s", user)
	}

	// And it is now counted.
	route, ok, err := h.store.Route(t.Context(), 4242, fingerprint)
	if err != nil || !ok {
		t.Fatalf("Route: %v, %v", ok, err)
	}

	if route.Count != 3 {
		t.Errorf("route count = %d, want 3", route.Count)
	}
}

// A route ridden for the first time says nothing about repeats.
func TestAFirstRideMentionsNoRouteHistory(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	h.strava.activity.Map.SummaryPolyline = "_p~iF~ps|U_ulLnnqC_mqNvxq`@"

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if strings.Contains(capture.prompt.User, "Route history") {
		t.Errorf("a first ride was described as a repeat:\n%s", capture.prompt.User)
	}
}

// Dry run counts no routes, for the same reason it advances no franchise.
func TestDryRunCountsNoRoutes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, false, nil)
	h.strava.activity.Map.SummaryPolyline = "_p~iF~ps|U_ulLnnqC_mqNvxq`@"

	h.enqueue(t, "create")

	for range 3 {
		if _, err := h.proc.Sweep(t.Context()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}

	fingerprint := mustFingerprint(t, h.strava.activity.Map.SummaryPolyline)

	if _, ok, _ := h.store.Route(t.Context(), 4242, fingerprint); ok {
		t.Error("dry run counted a ride of the route")
	}
}

// An unreadable title history fails the activity rather than naming without it.
//
// The realistic cause is the composite index missing, which is a deployment
// error that fixes itself on the next apply. Naming without history in the
// meantime would produce exactly the repetition the history exists to
// prevent, with nothing in the log to say why.
func TestAnUnreadableHistoryFailsTheActivity(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.proc.deps.Store = &faultyStore{
		Store:           h.store,
		recentTitlesErr: errors.New("firestore: index not found"),
	}

	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("result %+v, want one failure", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("%d PUTs without the title history: %+v", len(writes), writes)
	}
}

func mustFingerprint(t *testing.T, polyline string) string {
	t.Helper()

	value, err := geo.Fingerprint(polyline)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	return value
}

// Few-shot examples are derived from the athlete's own titles.
//
// The spec asks for examples in the athlete's style rather than a committed
// set. The named log keeps the title and the language; the situation that
// produced it does not survive, so it is rebuilt by re-reading the activity.
func TestFewShotExamplesAreDerivedFromHistory(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	if err := h.store.MarkNamed(t.Context(), store.Naming{
		AthleteID:  4242,
		ActivityID: 901,
		Title:      "Gegenwind bis Musterdorf",
		Language:   "de",
		At:         h.now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	user := capture.prompt.User

	if !strings.Contains(user, "Gegenwind bis Musterdorf") {
		t.Errorf("the athlete's own title is not among the examples:\n%s", user)
	}

	// The situation was rebuilt from the re-read activity, so it describes the
	// shape of a ride rather than being blank.
	if !strings.Contains(user, "GravelRide") {
		t.Errorf("the example carries no situation:\n%s", user)
	}

	// And the language round-tripped, so the example is not rendered as "()".
	if strings.Contains(user, "()") {
		t.Errorf("an example lost its language:\n%s", user)
	}
}

// With nothing written yet, the shipped synthetic set is used.
func TestFewShotExamplesFallBackToTheSyntheticSet(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	first := naming.SyntheticExamples()[0]

	if !strings.Contains(capture.prompt.User, first.Title) {
		t.Errorf("the synthetic examples did not reach a cold-start prompt:\n%s",
			capture.prompt.User)
	}
}

// Deriving examples is paid for once, not on every sweep.
//
// Dry run leaves the activity queued and reruns the whole pipeline every five
// minutes. Re-reading six activities each time would spend the Strava budget
// on a prompt that has not changed — the history only moves when something is
// named.
func TestDerivedExamplesAreNotRefetchedEverySweep(t *testing.T) {
	t.Parallel()

	h := newHarness(t, false, nil)

	if err := h.store.MarkNamed(t.Context(), store.Naming{
		AthleteID: 4242, ActivityID: 901,
		Title: "Gegenwind bis Musterdorf", Language: "de", At: h.now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	h.enqueue(t, "create")

	for range 4 {
		if _, err := h.proc.Sweep(t.Context()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}

	h.strava.mu.Lock()
	gets := h.strava.getCalls
	h.strava.mu.Unlock()

	// Four sweeps: one activity fetch each, plus one example derivation in
	// total. Dry run does not re-read the description, so anything above five
	// means the examples were derived again.
	if gets != 5 {
		t.Errorf("%d GETs across 4 dry-run sweeps, want 5 (4 activities + 1 derivation)", gets)
	}
}

// An activity that cannot be re-read costs an example, not the naming.
func TestAFailedExampleRefetchDoesNotBlockTheTitle(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)

	if err := h.store.MarkNamed(t.Context(), store.Naming{
		AthleteID: 4242, ActivityID: 99999,
		Title: "Eine alte Runde", Language: "de", At: h.now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Errorf("%d PUTs, want 1: a failed example re-read must not block a title", len(writes))
	}
}

// The example set is bounded, and a history longer than it does not blow the
// Strava budget deriving one example per past ride.
func TestDerivedExamplesAreBounded(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	for index := range exampleCount + 4 {
		if err := h.store.MarkNamed(t.Context(), store.Naming{
			AthleteID:  4242,
			ActivityID: int64(900 + index),
			Title:      "Runde " + string(rune('A'+index)),
			Language:   "de",
			At:         h.now.Add(-time.Duration(index+1) * time.Hour),
		}); err != nil {
			t.Fatalf("MarkNamed: %v", err)
		}
	}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	examples := strings.Count(capture.prompt.User, " -> ")
	if examples != exampleCount {
		t.Errorf("%d examples in the prompt, want %d", examples, exampleCount)
	}
}

// If no past activity can be re-read, the synthetic set stands in.
//
// A prompt with no examples at all is a worse prompt than one with borrowed
// ones, and neither is worth failing a naming for.
func TestUnreadableHistoryFallsBackToSyntheticExamples(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	capture := withCapture(h)

	h.strava.getErrFor = map[int64]error{901: errors.New("strava: 404 deleted")}

	if err := h.store.MarkNamed(t.Context(), store.Naming{
		AthleteID: 4242, ActivityID: 901,
		Title: "Eine gelöschte Runde", Language: "de", At: h.now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Fatalf("%d PUTs, want 1", len(writes))
	}

	// The deleted activity contributed no example. Its title still appears
	// under RECENT — it was written, so it must not be repeated — which is
	// why this looks at the example lines rather than the whole prompt.
	for _, line := range strings.Split(capture.prompt.User, "\n") {
		if strings.Contains(line, " -> ") && strings.Contains(line, "Eine gelöschte Runde") {
			t.Errorf("an example was built from an activity that could not be read: %q", line)
		}
	}

	if !strings.Contains(capture.prompt.User, naming.SyntheticExamples()[0].Title) {
		t.Errorf("no examples reached the prompt:\n%s", capture.prompt.User)
	}
}

// An activity with no sport type still describes itself.
func TestSituationOfAnActivityWithoutASportType(t *testing.T) {
	t.Parallel()

	activity := sportRide()
	activity.SportType = ""

	got := situationOf(&activity)

	if !strings.Contains(got, "ride") {
		t.Errorf("situation %q does not say what it was", got)
	}
}

// A route is dated by the ride, not by when the sweep got round to it.
//
// The store keeps the earliest ride as the one a callback names. An activity
// uploaded days late is processed after more recent ones, so recording it
// under "now" would both date it wrongly and, once it is the earliest ride of
// a route, put the wrong day in a title.
func TestARouteIsDatedByTheRideNotTheSweep(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)

	polyline := "_p~iF~ps|U_ulLnnqC_mqNvxq`@"
	h.strava.activity.Map.SummaryPolyline = polyline

	// A ride from a fortnight ago, uploaded today.
	ridden := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)
	h.strava.activity.StartDateLocal = ridden

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	route, ok, err := h.store.Route(t.Context(), 4242, mustFingerprint(t, polyline))
	if err != nil || !ok {
		t.Fatalf("Route: %v, %v", ok, err)
	}

	if !route.FirstSeen.Equal(ridden.UTC()) {
		t.Errorf("FirstSeen = %v, want the day it was ridden (%v)", route.FirstSeen, ridden.UTC())
	}

	if route.FirstSeen.Equal(h.now.UTC()) {
		t.Error("the route was dated by the sweep rather than by the ride")
	}
}

// An activity with no start date still counts, dated by the sweep.
//
// The fallback exists because a route with no date at all would break the
// bounds the store keeps; "when we saw it" is a worse answer than the ride's
// own date and a better one than the zero time.
func TestARouteWithoutARideDateFallsBackToNow(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)

	polyline := "_p~iF~ps|U_ulLnnqC_mqNvxq`@"
	h.strava.activity.Map.SummaryPolyline = polyline
	h.strava.activity.StartDateLocal = time.Time{}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	route, ok, err := h.store.Route(t.Context(), 4242, mustFingerprint(t, polyline))
	if err != nil || !ok {
		t.Fatalf("Route: %v, %v", ok, err)
	}

	if !route.FirstSeen.Equal(h.now.UTC()) {
		t.Errorf("FirstSeen = %v, want the sweep's clock (%v)", route.FirstSeen, h.now.UTC())
	}
}
