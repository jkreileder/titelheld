package naming

import "testing"

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
