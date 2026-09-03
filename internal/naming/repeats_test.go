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
// The place names that could locate the athlete are invented, as everywhere
// in this repository. İstanbul appears by name because the dotted capital is
// the case under test and no invented word carries it as naturally; a world
// city on another continent locates nobody's front door.
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
			// A soft hyphen is a hint about where a word may break, invisible
			// wherever it does not, and dropping it rather than spacing it is
			// what keeps the compound one word.
			name:      "a soft hyphen inside a word",
			candidate: "Bade\u00adsee im Nebel",
			titles:    []string{"Badesee im Nebel"},
			want:      "Badesee im Nebel",
		},
		{
			name:      "the soft hyphen on the remembered side instead",
			candidate: "Badesee im Nebel",
			titles:    []string{"Bade\u00adsee im Nebel"},
			want:      "Bade\u00adsee im Nebel",
		},
		{
			// U+200B between the two halves of a compound: invisible to a
			// reader, and a repeat.
			name:      "a zero-width space between two words",
			candidate: "Muster\u200bsee im Nebel",
			titles:    []string{"Mustersee im Nebel"},
			want:      "Mustersee im Nebel",
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
// drift apart silently if the cap lived in two places.
//
// Every fixture here fits in [MaxPromptFieldRunes], so the line the prompt shows
// and the stored title are the same string and each title contributes one entry.
// A title the prompt has to cut short is the next test's case.
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

// A title the prompt has to cut short is refused in the form the model saw and
// in the form the store holds.
//
// [BuildPrompt] writes every RECENT line through [OneLine], so a stored title
// longer than [MaxPromptFieldRunes] reaches the model as its first 60 runes.
// That prefix is the only form of it the model can hand back, and a comparison
// against the stored title alone would take it for a fresh one. A candidate
// sharing the first 59 runes and differing at the 60th is a different title and
// is left alone.
func TestForbiddenTitlesCarryTheFormThePromptShows(t *testing.T) {
	t.Parallel()

	const stored = "Musterrunde durch das Mustertal und dann weiter hinauf zum Musterberg und zurück"

	if runes := len([]rune(stored)); runes != 80 {
		t.Fatalf("the fixture is %d runes; it has to outrun the prompt's cap of %d",
			runes, MaxPromptFieldRunes)
	}

	shown := OneLine(stored)
	if runes := len([]rune(shown)); runes != MaxPromptFieldRunes {
		t.Fatalf("OneLine(%q) = %q, %d runes, want %d", stored, shown, runes, MaxPromptFieldRunes)
	}

	promptContext := Context{RecentTitles: []string{stored}}
	prompt := BuildPrompt(Ride{SportType: "GravelRide"}, promptContext)

	// What the model reads is the cut line, and the whole title is nowhere in
	// the prompt. RECENT is the last block here, so the line it ends on carries
	// no trailing newline to match against.
	if !strings.Contains(prompt.User, "\n- "+shown) {
		t.Fatalf("the prompt does not carry %q:\n%s", shown, prompt.User)
	}

	if strings.Contains(prompt.User, stored) {
		t.Fatalf("the prompt carries the whole of %q:\n%s", stored, prompt.User)
	}

	forbidden := promptContext.ForbiddenTitles()

	// The prefix the model was shown, returned verbatim. This is the review's
	// case: without the prompt's own truncation on the stored side, it passes.
	known, repeated := RepeatsTitle(shown, forbidden)
	if !repeated {
		t.Errorf("RepeatsTitle(%q, %q) = false, want true", shown, forbidden)
	}

	// And the match names the line the model read, not the longer title behind
	// it.
	if repeated && known != shown {
		t.Errorf("RepeatsTitle(%q) matched %q, want the line the prompt showed", shown, known)
	}

	if _, repeated := RepeatsTitle(stored, forbidden); !repeated {
		t.Errorf("RepeatsTitle(%q, %q) = false, want true", stored, forbidden)
	}

	// One rune later the ride is a different one. "B" for the "M" that the cut
	// leaves mid-word: a letter that folding cannot map onto the original.
	near := string([]rune(shown)[:MaxPromptFieldRunes-1]) + "B"
	if matched, repeated := RepeatsTitle(near, forbidden); repeated {
		t.Errorf("RepeatsTitle(%q) = true (matched %q), want false", near, matched)
	}
}
