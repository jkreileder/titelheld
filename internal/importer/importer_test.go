package importer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// pages serves a fixed history, one page at a time.
type pages struct {
	activities []strava.Activity
	err        error
	calls      int
}

func (p *pages) ListActivities(_ context.Context, page, perPage int) ([]strava.Activity, error) {
	p.calls++

	if p.err != nil {
		return nil, p.err
	}

	start := (page - 1) * perPage
	if start >= len(p.activities) {
		return nil, nil
	}

	end := min(start+perPage, len(p.activities))

	return p.activities[start:end], nil
}

// activity builds a synthetic summary activity. Fake IDs, as everything here is.
func activity(id int64, name string, daysAgo int) strava.Activity {
	return strava.Activity{
		ID:        id,
		Name:      name,
		SportType: "GravelRide",
		StartDate: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC).
			AddDate(0, 0, -daysAgo),
	}
}

func deps(t *testing.T, memory *store.Memory, list *pages) Deps {
	t.Helper()

	return Deps{
		Activities:    list,
		Store:         memory,
		AthleteID:     4242,
		MachineTitles: classifier.DefaultMachineTitles(),
		PerPage:       2,
		Pause:         func(context.Context, time.Duration) error { return nil },
		Logger:        quiet(),
	}
}

// The athlete's own titles are seeded; Strava's and Xert's are not.
//
// A default is not a title — they repeat by design, so listing them under
// "never repeat" is wrong — and a machine title is the style this service
// exists to replace, which is the last thing a few-shot example should teach.
func TestImportSeedsOnlyTheAthletesOwnTitles(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		activity(1, "Gegenwind bis Musterdorf", 1),
		activity(2, "Morning Ride", 2),
		activity(3, "Difficult Mixed Breakaway Specialist Ride", 3),
		activity(4, "Nach Hause über den Berg", 4),
		activity(6, "The long way home", 6),
		activity(5, "Afternoon Gravel Ride", 5),
	}}

	result, err := Run(t.Context(), deps(t, memory, list))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Seen != 6 {
		t.Errorf("Seen = %d, want 6", result.Seen)
	}

	if result.Imported != 3 {
		t.Errorf("Imported = %d, want 3", result.Imported)
	}

	if result.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3 (two Strava defaults and one Xert title)", result.Skipped)
	}

	history, err := memory.RecentTitles(t.Context(), 4242, 25)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	got := make(map[string]store.NamedTitle, len(history))
	for _, entry := range history {
		got[entry.Title] = entry
	}

	for _, unwanted := range []string{
		"Morning Ride", "Afternoon Gravel Ride",
		"Difficult Mixed Breakaway Specialist Ride",
	} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("%q was seeded into the history", unwanted)
		}
	}

	entry, ok := got["Gegenwind bis Musterdorf"]
	if !ok {
		t.Fatal("the athlete's own title was not seeded")
	}

	if entry.Source != store.SourceImported {
		t.Errorf("source = %q, want %q", entry.Source, store.SourceImported)
	}

	// Detected, not defaulted: "Gegenwind bis Musterdorf" carries no marker,
	// so asserting German on it would pass with the heuristic gutted. The
	// English title below is what makes the language a result rather than a
	// constant.
	if entry.Language != "de" {
		t.Errorf("language = %q, want de", entry.Language)
	}

	english, ok := got["The long way home"]
	if !ok {
		t.Fatal("the English title was not seeded")
	}

	if english.Language != "en" {
		t.Errorf("language = %q, want en", english.Language)
	}
}

// Entries are dated by the ride, so the history reads newest-ride-first.
//
// Stamping them with the import's clock would tie every one of them and leave
// the order to the activity-ID tiebreak, which is the order Strava happened to
// assign IDs rather than the order the athlete rode.
func TestImportedTitlesAreDatedByTheRide(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		activity(10, "Die neueste Runde", 1),
		activity(11, "Eine ältere Runde", 30),
		activity(12, "Die älteste Runde", 365),
	}}

	if _, err := Run(t.Context(), deps(t, memory, list)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	history, err := memory.RecentTitles(t.Context(), 4242, 25)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	want := []string{"Die neueste Runde", "Eine ältere Runde", "Die älteste Runde"}
	for index, title := range want {
		if history[index].Title != title {
			t.Errorf("history[%d] = %q, want %q", index, history[index].Title, title)
		}
	}
}

// Running twice writes nothing the second time.
func TestImportIsIdempotent(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		activity(20, "Eine Runde", 1),
		activity(21, "Noch eine Runde", 2),
	}}

	first, err := Run(t.Context(), deps(t, memory, list))
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	second, err := Run(t.Context(), deps(t, memory, list))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if first.Imported != 2 {
		t.Errorf("first run imported %d, want 2", first.Imported)
	}

	if second.Imported != 0 {
		t.Errorf("second run imported %d, want 0", second.Imported)
	}

	if second.AlreadyKnown != 2 {
		t.Errorf("second run reported %d already known, want 2", second.AlreadyKnown)
	}
}

// A title this service wrote is never relabelled as imported.
func TestImportLeavesServiceWrittenTitlesAlone(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()

	if err := memory.MarkNamed(t.Context(), store.Naming{
		AthleteID: 4242, ActivityID: 30,
		Title: "Musterrunde am Musterbach", Language: "de",
		Source: store.SourceLLM, At: time.Now(),
	}); err != nil {
		t.Fatalf("MarkNamed: %v", err)
	}

	// Strava now reports a different title for the same activity, as it would
	// if the athlete edited it afterwards.
	list := &pages{activities: []strava.Activity{activity(30, "Vom Menschen umbenannt", 1)}}

	if _, err := Run(t.Context(), deps(t, memory, list)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	title, _, err := memory.Named(t.Context(), 4242, 30)
	if err != nil {
		t.Fatalf("Named: %v", err)
	}

	if title != "Musterrunde am Musterbach" {
		t.Errorf("the service-written title was overwritten with %q", title)
	}
}

// An interrupted run continues where the log ends, with no state of its own.
func TestImportResumesFromTheNamedLog(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	all := []strava.Activity{
		activity(40, "Runde eins", 1),
		activity(41, "Runde zwei", 2),
		activity(42, "Runde drei", 3),
		activity(43, "Runde vier", 4),
	}

	// A first run that fails after the first page.
	failing := &failAfterPage{inner: &pages{activities: all}, after: 1}

	partial, err := Run(t.Context(), deps(t, memory, nil).withActivities(failing))
	if err == nil {
		t.Fatal("the interrupted run reported success")
	}

	if partial.Imported != 2 {
		t.Fatalf("the interrupted run imported %d, want the first page's 2", partial.Imported)
	}

	// A second run, with Strava healthy again.
	resumed, err := Run(t.Context(), deps(t, memory, &pages{activities: all}))
	if err != nil {
		t.Fatalf("resumed Run: %v", err)
	}

	if resumed.Imported != 2 {
		t.Errorf("resumed run imported %d, want the remaining 2", resumed.Imported)
	}

	if resumed.AlreadyKnown != 2 {
		t.Errorf("resumed run reported %d already known, want 2", resumed.AlreadyKnown)
	}

	history, err := memory.RecentTitles(t.Context(), 4242, 25)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	if len(history) != 4 {
		t.Errorf("%d titles after resuming, want 4", len(history))
	}
}

// failAfterPage serves pages until it has served enough, then fails.
type failAfterPage struct {
	inner *pages
	after int
}

func (f *failAfterPage) ListActivities(
	ctx context.Context, page, perPage int,
) ([]strava.Activity, error) {
	if page > f.after {
		return nil, errors.New("strava: 429 rate limited")
	}

	return f.inner.ListActivities(ctx, page, perPage)
}

func (d Deps) withActivities(a Activities) Deps {
	d.Activities = a

	return d
}

// The listing is paged, and an empty page ends it.
func TestImportPagesUntilExhausted(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()

	activities := make([]strava.Activity, 0, 7)
	for index := range 7 {
		activities = append(activities,
			activity(int64(50+index), "Runde "+string(rune('a'+index)), index+1))
	}

	list := &pages{activities: activities}

	result, err := Run(t.Context(), deps(t, memory, list))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Imported != 7 {
		t.Errorf("imported %d, want 7", result.Imported)
	}

	// Three full pages of two and a fourth holding one. The short page ends
	// the listing, so no empty fifth is fetched — that request and its pause
	// would only confirm what the short page already said.
	if result.Pages != 4 {
		t.Errorf("Pages = %d, want 4", result.Pages)
	}
}

// Run refuses a configuration that cannot do anything.
func TestRunValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{}

	for _, tc := range []struct {
		name string
		deps Deps
		want string
	}{
		{name: "no client", deps: Deps{Store: memory, AthleteID: 1}, want: "Strava client"},
		{name: "no store", deps: Deps{Activities: list, AthleteID: 1}, want: "store"},
		{name: "no athlete", deps: Deps{Activities: list, Store: memory}, want: "athlete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Run(t.Context(), tc.deps)
			if err == nil {
				t.Fatalf("Run succeeded with %s", tc.name)
			}

			if got := err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("error %q does not name the missing %s", got, tc.want)
			}
		})
	}
}

// A cancelled import stops, and keeps what it already wrote.
//
// The operator interrupts it; the next run continues from the named log.
func TestImportStopsWhenCancelled(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		activity(60, "Runde eins", 1),
		activity(61, "Runde zwei", 2),
		activity(62, "Runde drei", 3),
		activity(63, "Runde vier", 4),
	}}

	ctx, cancel := context.WithCancel(t.Context())

	d := deps(t, memory, list)
	// Cancel while waiting between pages, which is where a real interruption
	// most likely lands.
	d.Pause = func(context.Context, time.Duration) error {
		cancel()

		return context.Canceled
	}

	result, err := Run(ctx, d)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}

	if result.Imported != 2 {
		t.Errorf("imported %d before cancelling, want the first page's 2", result.Imported)
	}

	history, _ := memory.RecentTitles(t.Context(), 4242, 25)
	if len(history) != 2 {
		t.Errorf("%d titles kept after cancelling, want 2", len(history))
	}
}

// A store that cannot be read stops the import rather than skipping activities.
//
// Treating an unreadable log as "not known" would re-import titles that are
// already there, and could relabel a service-written one as imported.
func TestImportStopsOnAnUnreadableLog(t *testing.T) {
	t.Parallel()

	list := &pages{activities: []strava.Activity{activity(70, "Eine Runde", 1)}}

	d := deps(t, store.NewMemory(), list)
	d.Store = failingHistory{err: errors.New("firestore: unavailable")}

	if _, err := Run(t.Context(), d); err == nil {
		t.Fatal("an unreadable named log was ignored")
	}
}

// A store that cannot be written stops it too.
func TestImportStopsOnAnUnwritableLog(t *testing.T) {
	t.Parallel()

	list := &pages{activities: []strava.Activity{activity(71, "Eine Runde", 1)}}

	d := deps(t, store.NewMemory(), list)
	d.Store = failingHistory{writeErr: errors.New("firestore: aborted")}

	if _, err := Run(t.Context(), d); err == nil {
		t.Fatal("an unwritable named log was ignored")
	}
}

// failingHistory fails the read, the write, or neither.
type failingHistory struct {
	err      error
	writeErr error
}

func (f failingHistory) Named(context.Context, int64, int64) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}

	return "", false, nil
}

func (f failingHistory) MarkNamed(context.Context, store.Naming) error {
	return f.writeErr
}

// The default pause really waits, and gives up when the context does.
func TestTheDefaultPauseRespectsCancellation(t *testing.T) {
	t.Parallel()

	if err := sleepContext(t.Context(), time.Millisecond); err != nil {
		t.Errorf("a short wait returned %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait returned %v, want context.Canceled", err)
	}
}

// The default page size is Strava's maximum, so a full history costs the
// fewest requests it can.
func TestImportDefaultsToTheLargestPage(t *testing.T) {
	t.Parallel()

	var asked int

	list := &sizeRecorder{perPage: &asked}

	d := Deps{
		Activities: list,
		Store:      store.NewMemory(),
		AthleteID:  4242,
		Pause:      func(context.Context, time.Duration) error { return nil },
		Logger:     quiet(),
	}

	if _, err := Run(t.Context(), d); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if asked != strava.MaxActivitiesPerPage {
		t.Errorf("asked for %d per page, want %d", asked, strava.MaxActivitiesPerPage)
	}
}

type sizeRecorder struct{ perPage *int }

func (s *sizeRecorder) ListActivities(_ context.Context, _, perPage int) ([]strava.Activity, error) {
	*s.perPage = perPage

	return nil, nil
}

// Run supplies a logger and a pause when it is given neither.
//
// The command passes both; a caller that does not must not panic on a nil
// logger or hang on a nil pause.
func TestRunSuppliesItsOwnLoggerAndPause(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()

	// One page, so the default pause is assigned but never waited on — the
	// loop only pauses between pages.
	list := &pages{activities: []strava.Activity{activity(80, "Eine Runde", 1)}}

	result, err := Run(t.Context(), Deps{
		Activities: list,
		Store:      memory,
		AthleteID:  4242,
		PerPage:    strava.MaxActivitiesPerPage,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Imported != 1 {
		t.Errorf("imported %d, want 1", result.Imported)
	}
}

// A context that is already done stops before the first request.
func TestRunStopsOnAnAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	list := &pages{activities: []strava.Activity{activity(81, "Eine Runde", 1)}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Run(ctx, deps(t, store.NewMemory(), list))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}

	if list.calls != 0 {
		t.Errorf("Strava was called %d times with a cancelled context", list.calls)
	}
}

// A history that fills its last page still needs the empty one to end.
//
// The short-page shortcut cannot apply when every page is full, so the
// listing has to ask once more and be told there is nothing.
func TestImportEndsOnAnEmptyPageWhenTheLastIsFull(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()

	activities := make([]strava.Activity, 0, 4)
	for index := range 4 {
		activities = append(activities,
			activity(int64(90+index), "Runde "+string(rune('a'+index)), index+1))
	}

	result, err := Run(t.Context(), deps(t, memory, &pages{activities: activities}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Imported != 4 {
		t.Errorf("imported %d, want 4", result.Imported)
	}

	// Two full pages, then the empty third.
	if result.Pages != 3 {
		t.Errorf("Pages = %d, want 3", result.Pages)
	}
}

// A caller that omits MachineTitles still skips them.
//
// The zero MachineTitles matches no title at all, so the omission would seed
// Xert's titles into the history as the athlete's own style — the one outcome
// the skip exists to prevent, reached by leaving a field out rather than by
// deciding anything.
func TestOmittedMachineTitlesStillSkipThem(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		activity(95, "Difficult Mixed Breakaway Specialist Ride", 1),
		activity(96, "Gegenwind bis Musterdorf", 2),
	}}

	result, err := Run(t.Context(), Deps{
		Activities: list,
		Store:      memory,
		AthleteID:  4242,
		PerPage:    2,
		Pause:      func(context.Context, time.Duration) error { return nil },
		Logger:     quiet(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want the machine title skipped", result.Skipped)
	}

	history, err := memory.RecentTitles(t.Context(), 4242, 10)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	for _, entry := range history {
		if entry.Title == "Difficult Mixed Breakaway Specialist Ride" {
			t.Error("a machine title was seeded because the field was left out")
		}
	}
}
