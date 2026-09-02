package naming

import "testing"

// A candidate that is an achievement name taken whole is recognized, and one
// that merely writes about the stretch is not.
//
// The names are invented: this is a public repository, and a real segment
// name locates a real road.
func TestCopiesAchievement(t *testing.T) {
	t.Parallel()

	achievements := []string{
		"Gravel Musterwehr zum Badeweiher",
		"Der Mustersprint",
	}

	copies := []string{
		// The verbatim copy, and the copy under the normalizer's blindnesses:
		// case, punctuation, padding.
		"Gravel Musterwehr zum Badeweiher",
		"gravel musterwehr zum badeweiher",
		"  Gravel Musterwehr zum Badeweiher! ",
		// The article-dropped core, the same comparison a claimed franchise
		// entry faces.
		"Der Mustersprint",
	}

	for _, title := range copies {
		if _, copied := CopiesAchievement(title, achievements); !copied {
			t.Errorf("CopiesAchievement(%q) = false, want true", title)
		}
	}

	passes := []string{
		// A title about the stretch is the invited angle.
		"Gravel am Musterwehr",
		// A near-copy that rewords the name is allowed by design: the line is
		// equality, because containment would forbid every title that takes
		// the segment as its subject.
		"Gravel vom Musterwehr zum Badeweiher",
		"Mustersprint im Regen",
		"",
	}

	for _, title := range passes {
		if name, copied := CopiesAchievement(title, achievements); copied {
			t.Errorf("CopiesAchievement(%q) = true (matched %q), want false", title, name)
		}
	}

	// The negative control for the mechanism itself: with no achievements
	// there is nothing to copy.
	if _, copied := CopiesAchievement("Gravel Musterwehr zum Badeweiher", nil); copied {
		t.Error("CopiesAchievement matched against an empty list")
	}
}
