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

	before := classifier.DefaultErrandTitles()

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
