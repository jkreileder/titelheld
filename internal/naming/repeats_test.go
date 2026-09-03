package naming

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A candidate that is a title the prompt already carried is recognized, and one
// that merely resembles it is not.
//
// The forbidden list is RECENT plus EXAMPLES, and the two are the same input
// here: the rule does not care which block a title came from, only that the
// model was shown it and told not to repeat it.
//
// The place names are invented, as everywhere in this repository: a real one
// locates a real road.
func TestRepeatsTitle(t *testing.T) {
	t.Parallel()

	repeats := []struct {
		name      string
		candidate string
		titles    []string
		want      string
	}{
		{
			name:      "a few-shot example returned verbatim",
			candidate: "Die lange Version",
			titles:    []string{"Gegenwind bis Musterdorf", "Die lange Version"},
			want:      "Die lange Version",
		},
		{
			name:      "a recent title returned verbatim",
			candidate: "Curse of the Pink Panther",
			titles:    []string{"Curse of the Pink Panther", "Die Hausrunde, schon wieder"},
			want:      "Curse of the Pink Panther",
		},
		{
			name:      "the same title with an emoji appended",
			candidate: "Windig 🌬️",
			titles:    []string{"windig"},
			want:      "windig",
		},
		{
			name:      "the emoji on the remembered side instead",
			candidate: "Windig",
			titles:    []string{"windig 🌬️"},
			want:      "windig 🌬️",
		},
		{
			name:      "casing, and the folding that makes ss and ß one word",
			candidate: "GROSSE RUNDE",
			titles:    []string{"große runde"},
			want:      "große runde",
		},
		{
			name:      "punctuation the model added",
			candidate: "Zur Abwechslung: Musterbach.",
			titles:    []string{"zur abwechslung musterbach"},
			want:      "zur abwechslung musterbach",
		},
		{
			// Folding maps "İ" to "i" plus a combining dot, which is dropped
			// rather than spaced: a mark that survives folding separates no
			// words in the two languages this service writes.
			name:      "a dotted capital I against its plain spelling",
			candidate: "İstanbul Turu",
			titles:    []string{"Istanbul Turu"},
			want:      "Istanbul Turu",
		},
		{
			name:      "the dotted capital on the remembered side instead",
			candidate: "Istanbul Turu",
			titles:    []string{"İstanbul Turu"},
			want:      "İstanbul Turu",
		},
		{
			// NFKC first, so a ligature and the letters it stands for are one
			// spelling.
			name:      "a ligature the model returned",
			candidate: "Ausﬂug ins Mustertal",
			titles:    []string{"Ausflug ins Mustertal"},
			want:      "Ausflug ins Mustertal",
		},
	}

	for _, test := range repeats {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			known, repeated := RepeatsTitle(test.candidate, test.titles)
			if !repeated {
				t.Fatalf("RepeatsTitle(%q, %q) = false, want true", test.candidate, test.titles)
			}

			// The original is what comes back, not the normalized form: the
			// error names the title as the athlete would recognize it.
			if known != test.want {
				t.Errorf("RepeatsTitle(%q) matched %q, want %q", test.candidate, known, test.want)
			}
		})
	}

	passes := []struct {
		name      string
		candidate string
		titles    []string
	}{
		{
			name:      "a title sharing a suffix with a remembered one",
			candidate: "Probefahrt zum Badeweiher",
			titles:    []string{"Gravel Musterwehr zum Badeweiher"},
		},
		{
			name:      "a fresh title against a list that carries neither block's",
			candidate: "Nebel über der Musterhöhe",
			titles:    []string{"Gegenwind bis Musterdorf", "Die lange Version"},
		},
		{
			name:      "a candidate that normalizes to nothing",
			candidate: "🌬️",
			titles:    []string{"windig", ""},
		},
		{
			name:      "an empty candidate",
			candidate: "",
			titles:    []string{""},
		},
		{
			name:      "nothing to repeat",
			candidate: "Die lange Version",
			titles:    nil,
		},
	}

	for _, test := range passes {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if known, repeated := RepeatsTitle(test.candidate, test.titles); repeated {
				t.Errorf("RepeatsTitle(%q) = true (matched %q), want false", test.candidate, known)
			}
		})
	}
}

// The refusal list is what the prompt shows, including the cap on RECENT.
//
// Asserted against the prompt itself rather than against the cap: a title
// beyond it is neither shown to the model nor refused, and a list built from
// the uncapped history would refuse titles the model never saw. The two would
// drift apart silently if the truncation lived in two places.
func TestForbiddenTitlesAreTheOnesThePromptCarries(t *testing.T) {
	t.Parallel()

	recent := make([]string, 0, RecentTitleLimit+3)
	for i := 1; i <= RecentTitleLimit+3; i++ {
		recent = append(recent, fmt.Sprintf("Musterrunde %02d", i))
	}

	examples := []Example{
		{Situation: "88 km gravel, headwind out", Title: "Gegenwind bis Musterdorf", Language: German},
		{Situation: "42 km evening road ride", Title: "Die lange Version", Language: German},
	}

	promptContext := Context{RecentTitles: recent, Examples: examples}
	prompt := BuildPrompt(Ride{SportType: "GravelRide"}, promptContext)
	forbidden := promptContext.ForbiddenTitles()

	// Every refused title is one the prompt carries: a RECENT line, or an
	// example's title.
	for _, title := range forbidden {
		shown := strings.Contains(prompt.User, "\n- "+title+"\n") ||
			strings.Contains(prompt.User, "-> "+title+" (")
		if !shown {
			t.Errorf("%q is refused but the prompt does not carry it:\n%s", title, prompt.User)
		}
	}

	// And what the prompt left out is not refused. The overflow is what the cap
	// drops, so this is the assertion that fails if the refusal stops reading
	// RECENT through it.
	for _, title := range recent[RecentTitleLimit:] {
		if strings.Contains(prompt.User, title) {
			t.Fatalf("the prompt carries %q, which is past the cap:\n%s", title, prompt.User)
		}

		if slices.Contains(forbidden, title) {
			t.Errorf("%q is refused although the prompt never carried it", title)
		}
	}

	// Both blocks reach the list, and nothing reaches it twice.
	if want := RecentTitleLimit + len(examples); len(forbidden) != want {
		t.Errorf("ForbiddenTitles returned %d titles, want %d", len(forbidden), want)
	}
}
