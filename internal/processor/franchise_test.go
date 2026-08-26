package processor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
)

// scriptedProvider answers one queued response per call and keeps every prompt.
//
// The fake in processor_test.go answers the same way every time, which cannot
// express the case this file is about: a model that declines the franchise
// entry once and takes it the second time, or the other way round.
type scriptedProvider struct {
	steps   []step
	prompts []naming.Prompt
	calls   int
}

// step is one scripted answer: a raw response, or a failure.
type step struct {
	response string
	err      error
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Complete(_ context.Context, prompt naming.Prompt) (string, error) {
	s.prompts = append(s.prompts, prompt)
	s.calls++

	if s.calls > len(s.steps) {
		return "", errors.New("the model was called more often than the script allows")
	}

	current := s.steps[s.calls-1]

	return current.response, current.err
}

// title is a response the validator accepts.
func title(text string) string {
	return `{"title":"` + text + `","language":"de"}`
}

// offersFranchise reports whether a prompt carries a FRANCHISE block.
func offersFranchise(prompt naming.Prompt) bool {
	return strings.Contains(prompt.User, "\nFRANCHISE\n")
}

// scripted swaps the scripted provider into a harness.
func scripted(h *harness, steps ...step) *scriptedProvider {
	provider := &scriptedProvider{steps: steps}
	h.proc.deps.Provider = provider

	return provider
}

// franchiseRide is a harness whose activity rides the franchise bike.
func franchiseRide(t *testing.T, writes bool) *harness {
	t.Helper()

	h := newHarness(t, writes, nil)
	h.strava.gearName = "Pink Panther"
	h.strava.activity.GearID = "b1234567"

	return h
}

// A title that does not use the entry does not spend it.
//
// This is the negative the rule exists for. "Der Panther im Morgengrauen" is
// unmistakably in the series' key and names none of its films; the athlete
// would have no way of telling afterwards which entry it had cost. The ride is
// still named — the franchise is garnish — and the position does not move.
func TestATitleThatIgnoresTheEntryDoesNotAdvanceTheFranchise(t *testing.T) {
	t.Parallel()

	h := franchiseRide(t, true)

	const themed = "Der Panther im Morgengrauen"

	provider := scripted(h,
		step{response: title(themed)},
		step{response: title(themed)},
		step{response: title(themed)},
	)

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	position, err := h.store.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 0 {
		t.Errorf("position = %d, want 0: a title that never named the entry spent it", position)
	}

	writes := h.strava.writes()
	if len(writes) != 1 {
		t.Fatalf("%d PUTs, want the ride named anyway", len(writes))
	}

	if writes[0].name != themed {
		t.Errorf("name is %q, want %q", writes[0].name, themed)
	}

	// The entry was offered twice and then dropped, and the title that got
	// written came from a prompt that was no longer reaching for it.
	if provider.calls != 3 {
		t.Fatalf("the model was called %d times, want 3", provider.calls)
	}

	// Literal counts rather than maxFranchiseOffers: a test that reads the
	// constant it is pinning would pass whatever the constant said.
	for attempt, prompt := range provider.prompts {
		if want := attempt < 2; offersFranchise(prompt) != want {
			t.Errorf("attempt %d offers the franchise = %v, want %v",
				attempt+1, offersFranchise(prompt), want)
		}
	}
}

// The second offer is a real second chance, and using it spends the entry.
func TestTheSecondOfferCanStillUseTheEntry(t *testing.T) {
	t.Parallel()

	h := franchiseRide(t, true)
	entry, index := shippedEntry(t)

	used := entry + " am Musterbach"
	provider := scripted(h,
		step{response: title("Der Panther im Morgengrauen")},
		step{response: title(used)},
	)

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if provider.calls != 2 {
		t.Errorf("the model was called %d times, want 2: no third call is needed", provider.calls)
	}

	writes := h.strava.writes()
	if len(writes) != 1 || writes[0].name != used {
		t.Fatalf("writes = %+v, want one PUT of %q", writes, used)
	}

	position, err := h.store.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if want := index + 1; position != want {
		t.Errorf("position = %d, want %d: the entry was used and not recorded", position, want)
	}
}

// An entry is offered twice at most, however many rides are named.
//
// The count is the point: a third offer is a paid call for a model that has
// already said no twice, and an unbounded loop would spend a sweep on one
// activity.
func TestAnEntryIsOfferedTwiceAtMost(t *testing.T) {
	t.Parallel()

	h := franchiseRide(t, false)

	provider := scripted(h,
		step{response: title("Musterrunde am Musterbach")},
		step{response: title("Musterrunde im Nebel")},
		step{response: title("Musterrunde bei Nacht")},
	)

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	offers := 0

	for _, prompt := range provider.prompts {
		if offersFranchise(prompt) {
			offers++
		}
	}

	// Two, spelled out. Comparing against maxFranchiseOffers would make this
	// test agree with any value the constant took.
	if offers != 2 {
		t.Errorf("the entry was offered %d times, want 2", offers)
	}

	if provider.calls != 3 {
		t.Errorf("the model was called %d times, want 3: two offers and one without", provider.calls)
	}
}

// A failed second offer keeps the title the first attempt produced.
//
// The title in hand is a good one that simply ignored the series. Losing it
// because a call made for a decoration failed would leave the ride at its
// Strava default.
func TestAFailedSecondOfferKeepsTheFirstTitle(t *testing.T) {
	t.Parallel()

	h := franchiseRide(t, true)

	const first = "Musterrunde am Musterbach"

	provider := scripted(h,
		step{response: title(first)},
		step{err: errors.New("vertex: 503 unavailable")},
	)

	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Named != 1 {
		t.Fatalf("result %+v, want the activity named anyway", result)
	}

	if provider.calls != 2 {
		t.Errorf("the model was called %d times, want 2", provider.calls)
	}

	writes := h.strava.writes()
	if len(writes) != 1 || writes[0].name != first {
		t.Fatalf("writes = %+v, want one PUT of %q", writes, first)
	}

	position, err := h.store.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 0 {
		t.Errorf("position = %d, want 0: an unused entry was spent", position)
	}
}

// A failed final call keeps the last title the model did produce.
func TestAFailedFinalCallKeepsTheTitleThatIgnoredTheEntry(t *testing.T) {
	t.Parallel()

	h := franchiseRide(t, true)

	const second = "Musterrunde im Nebel"

	provider := scripted(h,
		step{response: title("Musterrunde am Musterbach")},
		step{response: title(second)},
		step{err: errors.New("vertex: 503 unavailable")},
	)

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if provider.calls != 3 {
		t.Errorf("the model was called %d times, want 3", provider.calls)
	}

	writes := h.strava.writes()
	if len(writes) != 1 || writes[0].name != second {
		t.Fatalf("writes = %+v, want one PUT of %q", writes, second)
	}

	position, err := h.store.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 0 {
		t.Errorf("position = %d, want 0", position)
	}
}

// A reserved entry is never offered, and the position lands past the one that
// was.
//
// The whole path: a `reserved` list in the athlete's configuration document,
// through the store, into the rotation. Advancing by one step instead of past
// the offered index would leave the position on the reserved entry and offer
// it to the next ride, which is exactly what reserving it forbids.
func TestAReservedEntryIsNeverOffered(t *testing.T) {
	t.Parallel()

	h := franchiseRide(t, true)
	h.proc.deps.Franchises = nil
	capture := withCapture(h)

	if err := h.store.SaveAthleteConfig(t.Context(), 4242, store.AthleteConfig{
		Franchises: []store.Franchise{{
			Name:     "pink-panther",
			GearName: "Pink Panther",
			Titles:   []string{"Kept For Later", "Also Kept", "Offered"},
			Reserved: []string{"Kept For Later", "Also Kept"},
		}},
	}); err != nil {
		t.Fatalf("SaveAthleteConfig: %v", err)
	}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if !strings.Contains(capture.first().User, "Offered") {
		t.Errorf("the first entry that is not reserved was not offered:\n%s", capture.first().User)
	}

	for _, kept := range []string{"Kept For Later", "Also Kept"} {
		if strings.Contains(capture.first().User, kept) {
			t.Errorf("a reserved entry was offered:\n%s", capture.first().User)
		}
	}

	position, err := h.store.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 3 {
		t.Errorf("position = %d, want 3: the rotation did not land past the offered entry", position)
	}
}

// An entry that cannot be a title is not offered, and does not advance.
//
// The failure it prevents is silent and permanent: a truncated entry can never
// be contained in a title, so the ride would spend three model calls declining
// it and the next ride would be offered exactly the same thing.
func TestAnUnusableEntryIsNotOffered(t *testing.T) {
	t.Parallel()

	h := franchiseRide(t, true)
	h.proc.deps.Franchises = []naming.Franchise{{
		Name:     "pink-panther",
		GearName: "Pink Panther",
		Titles:   []string{strings.Repeat("a", naming.MaxTitleRunes+1)},
	}}

	capture := withCapture(h)

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if offersFranchise(capture.first()) {
		t.Errorf("an entry too long to be a title was offered:\n%s", capture.first().User)
	}

	// One call, not three: nothing was offered, so there was nothing to
	// re-offer or to fall back from.
	if capture.calls != 1 {
		t.Errorf("the model was called %d times, want 1", capture.calls)
	}

	position, err := h.store.FranchisePosition(t.Context(), 4242, "pink-panther")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 0 {
		t.Errorf("position = %d, want 0: an entry that was never offered was spent", position)
	}
}

// The named log records what Strava kept, not what was sent.
//
// Strava deletes tokens from a title that look like a hostname: a hand-written
// "Über Ruhstorf a.d.Rott nach Pocking" came back as "Über Ruhstorf  nach
// Pocking" on 2026-08-24. The named log is written before the PUT — on purpose,
// so a crash cannot rename an activity twice — so without a correction the row
// would hold a title that does not exist, RECENT would forbid repeating it, and
// a few-shot example could teach a form that never survives a write.
func TestTheNamedLogRecordsWhatStravaKept(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)

	const sent = "Über Ruhstorf a.d.Rott nach Pocking"

	const kept = "Über Ruhstorf  nach Pocking"

	h.provider.response = title(sent)
	h.strava.mangle = func(string) string { return kept }

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// What went out is still what the model produced.
	writes := h.strava.writes()
	if len(writes) != 1 || writes[0].name != sent {
		t.Fatalf("writes = %+v, want one PUT of %q", writes, sent)
	}

	stored, named, err := h.store.Named(t.Context(), 4242, 777)
	if err != nil {
		t.Fatalf("Named: %v", err)
	}

	if !named {
		t.Fatal("the activity is not in the named log")
	}

	if stored != kept {
		t.Errorf("named log holds %q, want what Strava kept: %q", stored, kept)
	}
}

// The correction is a second write, and only when there is something to
// correct — the counterpart to the assertion above.
func TestAMangledTitleCostsOneCorrection(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.provider.response = title("Über Ruhstorf a.d.Rott nach Pocking")
	h.strava.mangle = func(string) string { return "Über Ruhstorf  nach Pocking" }

	counting := &countingStore{Store: h.store}
	h.proc.deps.Store = counting

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if got := counting.count(); got != 2 {
		t.Errorf("the named log was written %d times, want 2: the record, then the correction", got)
	}
}

// countingStore records how often the named log is written.
//
// Reading the final title cannot tell one write from two: both store the same
// string, and the harness runs on a fixed clock so even the timestamp matches.
// The question here is whether a second write happens at all.
type countingStore struct {
	store.Store

	mu    sync.Mutex
	marks int
}

func (c *countingStore) MarkNamed(ctx context.Context, naming store.Naming) error {
	c.mu.Lock()
	c.marks++
	c.mu.Unlock()

	return c.Store.MarkNamed(ctx, naming)
}

func (c *countingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.marks
}

// A title Strava stores unchanged leaves the record alone.
//
// Not merely "the row is right afterwards" — a reconciliation that fired
// anyway would rewrite it to the same value and look identical. This asserts
// the write did not happen.
func TestAnUnchangedTitleIsNotRewritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.provider.response = title("Musterrunde am Musterbach")

	counting := &countingStore{Store: h.store}
	h.proc.deps.Store = counting

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	stored, _, err := h.store.Named(t.Context(), 4242, 777)
	if err != nil {
		t.Fatalf("Named: %v", err)
	}

	if stored != "Musterrunde am Musterbach" {
		t.Errorf("named log holds %q", stored)
	}

	if got := counting.count(); got != 1 {
		t.Errorf("the named log was written %d times, want 1: nothing was mangled, so nothing needed correcting", got)
	}
}

// A spent entry that Strava then removes is said out loud.
//
// The advance happens before the write and is monotonic, so it cannot be taken
// back. A franchise entry reaches the prompt verbatim and is never normalized —
// a series whose titles carry initials has exactly the shape Strava removes —
// so the entry can be spent on a title that no longer contains it, which is
// the failure advance-on-use exists to prevent.
func TestAFranchiseEntryLostToStravaIsReported(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer

	h := newHarness(t, true, func(d *Deps) {
		d.Logger = slog.New(slog.NewJSONHandler(&logged, nil))
		d.Franchises = []naming.Franchise{{
			Name: "initials", GearName: "Pink Panther", Titles: []string{"S.W.A.T. am Musterbach"},
		}}
	})

	h.strava.gearName = "Pink Panther"
	h.strava.activity.GearID = "b1234567"
	h.provider.response = title("S.W.A.T. am Musterbach")
	h.strava.mangle = func(string) string { return "am Musterbach" }

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	out := logged.String()

	if !strings.Contains(out, "reserve it again by hand") {
		t.Errorf("a franchise entry was spent on a title Strava rewrote, without saying so:\n%s", out)
	}

	// The position still moved: that is the ordering the design chose, and the
	// warning exists precisely because it cannot be undone here.
	position, err := h.store.FranchisePosition(t.Context(), 4242, "initials")
	if err != nil {
		t.Fatalf("FranchisePosition: %v", err)
	}

	if position != 1 {
		t.Errorf("position = %d, want 1", position)
	}
}

// A franchise title Strava keeps intact says nothing.
func TestAnIntactFranchiseEntryIsNotReported(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer

	h := newHarness(t, true, func(d *Deps) {
		d.Logger = slog.New(slog.NewJSONHandler(&logged, nil))
		d.Franchises = []naming.Franchise{{
			Name: "plain", GearName: "Pink Panther", Titles: []string{"Son of the Pink Panther"},
		}}
	})

	h.strava.gearName = "Pink Panther"
	h.strava.activity.GearID = "b1234567"
	h.provider.response = title("Son of the Pink Panther am Musterbach")

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if strings.Contains(logged.String(), "reserve it again by hand") {
		t.Error("an intact franchise title was reported as lost")
	}
}

// The stored shape and the one with behavior convert both ways without loss,
// so a profile the seeder writes is the profile a sweep reads back.
func TestFranchisesRoundTripThroughTheStoredShape(t *testing.T) {
	t.Parallel()

	// Not the shipped profile: a converter that returned DefaultProfile()
	// whatever it was given would round-trip that one perfectly.
	profile := []naming.Franchise{
		{
			Name: "silver-surfer", SportTypes: []string{"Ride"}, GearName: "Silver Surfer",
			Titles:   []string{"Herald of Galactus", "The Power Cosmic", "Rise of the Silver Surfer"},
			Reserved: []string{"Rise of the Silver Surfer"},
		},
		{Name: "second", Titles: []string{"Eins", "Zwei"}},
	}

	back := FranchisesFromStored(FranchisesToStored(profile))

	if len(back) != len(profile) {
		t.Fatalf("%d franchises came back, want %d", len(back), len(profile))
	}

	for index := range profile {
		want, got := profile[index], back[index]

		if got.Name != want.Name || got.GearName != want.GearName ||
			!slices.Equal(got.SportTypes, want.SportTypes) ||
			!slices.Equal(got.Titles, want.Titles) ||
			!slices.Equal(got.Reserved, want.Reserved) {
			t.Errorf("franchise %d changed in transit:\n got %+v\nwant %+v", index, got, want)
		}
	}

	// And the stored side is a copy of the data, not a rename of the type:
	// a reserved entry survives as a field the document can hold.
	stored := FranchisesToStored(profile)
	if len(stored[0].Reserved) != 1 || stored[0].Reserved[0] != "Rise of the Silver Surfer" {
		t.Errorf("the stored shape lost the reserved entries: %v", stored[0].Reserved)
	}
}
