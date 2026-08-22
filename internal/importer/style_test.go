package importer

import (
	"testing"

	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// sportActivity is [activity] with the sport type spelled out.
func sportActivity(id int64, name, sportType string, daysAgo int) strava.Activity {
	a := activity(id, name, daysAgo)
	a.SportType = sportType

	return a
}

// titlesIn reads back what a run seeded, newest first.
func titlesIn(t *testing.T, memory *store.Memory) []string {
	t.Helper()

	history, err := memory.RecentTitles(t.Context(), 4242, 100)
	if err != nil {
		t.Fatalf("RecentTitles: %v", err)
	}

	titles := make([]string, 0, len(history))
	for _, entry := range history {
		titles = append(titles, entry.Title)
	}

	return titles
}

// A title Zwift or Xert wrote is not the athlete's, whatever the sport says.
//
// Both tools title a ride that stays a Ride to Strava, so the sport-type gate
// does not catch them: a Zwift session recorded by a head unit rather than by
// Zwift itself arrives as a plain Ride carrying "Zwift - <route> in Watopia".
func TestImportSkipsWhatOtherToolsTitled(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		activity(1, "Zwift - Sugar Cookie in Watopia", 1),
		activity(2, "Feierabendrunde - Xert", 2),

		// The control: without it, a gate that skipped everything would pass.
		activity(3, "Gegenwind bis Musterdorf", 3),
	}}

	result, err := Run(t.Context(), deps(t, memory, list))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := titlesIn(t, memory); len(got) != 1 || got[0] != "Gegenwind bis Musterdorf" {
		t.Errorf("seeded %q, want only the athlete's own title", got)
	}

	if result.Skipped != 2 {
		t.Errorf("skipped %d, want 2", result.Skipped)
	}
}

// The titles this service writes itself are skipped — the configured ones.
//
// Not a fixed list of German words: the athlete who renamed their commute has
// their names skipped, and the shipped ones become ordinary titles again,
// because the list a run skips is the list that run would write.
func TestImportSkipsTheTemplatesThisRunWouldWrite(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		activity(1, "Ins Büro", 1),

		// Trailing space: Strava keeps whatever the title was saved with.
		activity(2, "Heimweg ", 2),

		// Configured away, so no longer a template — an ordinary title now.
		activity(3, "Zur Arbeit", 3),
	}}

	d := deps(t, memory, list)
	d.TemplateTitles = []string{"Ins Büro", "Heimweg"}

	if _, err := Run(t.Context(), d); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := titlesIn(t, memory); len(got) != 1 || got[0] != "Zur Arbeit" {
		t.Errorf("seeded %q, want only the title this configuration does not write", got)
	}
}

// Only rides are remembered, and being the wrong sport is not being skipped.
//
// The counters are separate because they answer different questions: one is a
// ride whose title is worth nothing to the history, the other an activity this
// service would never name at all.
func TestImportRemembersRidesOnly(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		sportActivity(1, "Gegenwind bis Musterdorf", "Ride", 1),
		sportActivity(2, "Schotter und Sonne", "GravelRide", 2),
		sportActivity(3, "Morgenlauf", "Run", 3),
		sportActivity(4, "Functional Fitness", "WeightTraining", 4),
		sportActivity(5, "Bergfahrt", "VirtualRide", 5),
	}}

	result, err := Run(t.Context(), deps(t, memory, list))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(titlesIn(t, memory)); got != 2 {
		t.Errorf("seeded %d titles, want the two rides", got)
	}

	if result.NotARide != 3 {
		t.Errorf("not a ride: %d, want 3", result.NotARide)
	}

	if result.Skipped != 0 {
		t.Errorf("skipped %d, want 0 — a run is not a ride with a bad title", result.Skipped)
	}
}

// An empty Deps.TemplateTitles means the shipped set, not "skip nothing".
//
// The same reasoning as the machine titles: a caller that left the field out
// would seed exactly what the field exists to exclude, and would report it as
// success.
func TestTheZeroTemplateListMeansTheShippedOne(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	list := &pages{activities: []strava.Activity{
		activity(1, "Zur Arbeit", 1),
		activity(2, "Besorgungen", 2),
		activity(3, "Gegenwind bis Musterdorf", 3),
	}}

	d := deps(t, memory, list)
	d.TemplateTitles = nil

	if _, err := Run(t.Context(), d); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := titlesIn(t, memory); len(got) != 1 || got[0] != "Gegenwind bis Musterdorf" {
		t.Errorf("seeded %q with no template list configured", got)
	}
}
