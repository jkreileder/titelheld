package classifier_test

import (
	"slices"
	"testing"

	"github.com/jkreileder/titelheld/internal/classifier"
)

// The template list is derived from the configuration, not from constants.
//
// It is what the history import skips, so a list that ignored the athlete's
// own commute names would seed them as style while skipping two German words
// this deployment never writes.
func TestTemplateTitlesFollowTheConfiguration(t *testing.T) {
	t.Parallel()

	cfg := classifier.DefaultConfig()
	cfg.ToWorkTitle = "Ins Büro"

	titles := cfg.TemplateTitles()

	if !slices.Contains(titles, "Ins Büro") {
		t.Errorf("configured commute title missing from %q", titles)
	}

	// Left unset, so the shipped one still applies and is still skipped.
	if !slices.Contains(titles, "Nach Hause") {
		t.Errorf("default homeward title missing from %q", titles)
	}

	if slices.Contains(titles, "Zur Arbeit") {
		t.Errorf("%q still lists a commute title this configuration never writes", titles)
	}

	for _, errand := range classifier.DefaultErrandTitles() {
		if !slices.Contains(titles, errand) {
			t.Errorf("errand %q missing from %q", errand, titles)
		}
	}
}

// The errand pool survives a caller that reorders what it was given.
//
// The processor picks from it by activity ID, so the order decides which title
// every future errand gets: a caller that sorted the slice in place would
// silently rename them all.
func TestTheErrandPoolCannotBeReorderedFromOutside(t *testing.T) {
	t.Parallel()

	// Cloned, or this test cannot see the defect it exists for: if
	// DefaultErrandTitles hands back its own slice, reversing the second copy
	// reverses this one too and the comparison is a slice against itself.
	before := slices.Clone(classifier.DefaultErrandTitles())

	scrambled := classifier.DefaultErrandTitles()
	slices.Reverse(scrambled)

	if after := classifier.DefaultErrandTitles(); !slices.Equal(before, after) {
		t.Errorf("pool is now %q, was %q", after, before)
	}
}

// An unconfigured commute still has a name, in both directions.
func TestCommuteTitleHasGermanDefaults(t *testing.T) {
	t.Parallel()

	var unset classifier.Config

	if got := unset.CommuteTitle(classifier.DirectionToWork); got != "Zur Arbeit" {
		t.Errorf("to work: %q", got)
	}

	if got := unset.CommuteTitle(classifier.DirectionToHome); got != "Nach Hause" {
		t.Errorf("to home: %q", got)
	}
}

// The shipped pattern recognizes the Xert titles this athlete's account holds.
//
// Xert writes "<difficulty> [<specificity> ]<focus> Ride", and the pattern is
// held to that construction and to Xert's published vocabulary. A title it
// fails to match is skipped rather than overwritten — the safe direction — but
// it costs a naming every time Xert writes one.
func TestTheXertPatternMatchesTheObservedTitles(t *testing.T) {
	t.Parallel()

	machine := classifier.DefaultMachineTitles()

	for _, title := range []string{
		"Difficult Mixed Breakaway Specialist Ride",
		"Easy Pure Endurance Ride",
		"Moderate Polar Endurance Ride",
		"Difficult Polar Puncheur Ride",
		// The specificity slot is optional, and the hyphenated focus names
		// have to survive being spliced into the alternation.
		"Tough Endurance Ride",
		"Hard Mixed Time-Trialist Ride",
	} {
		if !machine.Matches(title) {
			t.Errorf("%q is not recognized as a machine title", title)
		}
	}

	// The other half of the asymmetry, and the more important one: a title
	// built from the same words by a person is not Xert's construction, and
	// matching it would overwrite what the athlete wrote.
	for _, title := range []string{
		"Puncheur-Tag",
		"Pure Endurance",
		"Endurance Ride",
		"Easy Pure Endurance Ride im Regen",
		"Eine Easy Pure Endurance Ride",
		// A word Xert does not publish, in either slot, keeps the title out.
		"Difficult Polar Domestique Ride",
		"Epic Puncheur Ride",
		"difficult polar puncheur ride",
	} {
		if machine.Matches(title) {
			t.Errorf("%q was taken for a machine title", title)
		}
	}
}

// A pattern gap must not turn a tool's title into the athlete's style.
//
// [classifier.IsToolTitle] is read only where the question is whether to record
// a title as the athlete's own, never by the overwrite gate, so it may match
// more than the pattern: a title with Xert's shape but a word the pattern does
// not name is still Xert's. The negative half is the cost of that looseness
// staying bounded — the athlete's own titles must survive it.
func TestToolTitlesIncludeTheXertShape(t *testing.T) {
	t.Parallel()

	for _, title := range []string{
		"Difficult Polar Domestique Ride",
		"Epic Puncheur Ride",
		"Tough Endurance Ride",
		"Zwift - Watopia",
		"Tough Endurance Ride - Xert",
	} {
		if !classifier.IsToolTitle(title) {
			t.Errorf("%q is not recognized as a tool's title", title)
		}
	}

	for _, title := range []string{
		"Puncheur-Tag",
		"Plan B auf dem Silbernen",
		"Der Silberne kann's noch",
		"Windschief",
		"Sonntagabendrunde",
		"Ride",
		"Morning Ride",
		// Xert's own words in a title Xert would not build: the construction is
		// never two words, and never opens on a specificity or focus name.
		"Endurance Ride",
		"Hard Ride",
		"Mixed Terrain Ride",
	} {
		if classifier.IsToolTitle(title) {
			t.Errorf("%q was taken for a tool's title", title)
		}
	}
}
