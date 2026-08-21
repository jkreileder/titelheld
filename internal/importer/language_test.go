package importer

import "testing"

// The heuristic labels this athlete's kind of titles.
//
// Synthetic titles in both languages — invented, as everything committed here
// is. It is a guess and only ever applied to imported titles: a title this
// service wrote carries the language the model reported.
func TestLanguage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		title string
		want  string
	}{
		// Umlauts and ß settle it on their own.
		{title: "Nach Hause über den Berg", want: "de"},
		{title: "Große Runde bei Regen", want: "de"},
		{title: "Müde nach der Arbeit", want: "de"},

		// Function words carry the rest.
		{title: "Gegenwind bis zum See", want: "de"},
		{title: "Endlich wieder Sonne", want: "de"},
		{title: "Zur Arbeit", want: "de"},

		{title: "The long way home", want: "en"},
		{title: "Into the hills with a headwind", want: "en"},
		{title: "Every climb was a mistake", want: "en"},
		{title: "Some laps in the rain", want: "en"},
	} {
		t.Run(tc.title, func(t *testing.T) {
			t.Parallel()

			if got := Language(tc.title); got != tc.want {
				t.Errorf("Language(%q) = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}

// German is the answer when there is nothing to go on.
//
// It is this athlete's language for the utility rides that make up most of a
// history, and a wrong label costs a slightly less well-matched few-shot
// example rather than a wrong title.
func TestLanguageDefaultsToGerman(t *testing.T) {
	t.Parallel()

	for _, title := range []string{"", "Watopia", "42", "🚴", "Musterbach"} {
		if got := Language(title); got != "de" {
			t.Errorf("Language(%q) = %q, want de", title, got)
		}
	}
}

// Words that belong to both languages must not decide anything on their own.
func TestLanguageIgnoresSharedWords(t *testing.T) {
	t.Parallel()

	// "wind", "warm" and "sun"/"Sonne" appear in both lists precisely so a
	// title carrying only those falls through to the default rather than
	// being labelled by whichever list happened to list it.
	for _, title := range []string{"Wind", "warm", "Wind und warm"} {
		if got := Language(title); got != "de" {
			t.Errorf("Language(%q) = %q; a shared word decided the answer", title, got)
		}
	}
}

// German has to win against English markers, not merely by default.
//
// Language falls back to German, so a case with no English signal proves
// nothing: it passes with the German markers emptied and the umlaut scoring
// deleted. These carry English markers, so German has to actually outscore
// them.
func TestGermanIsDetectedRatherThanAssumed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		title string
		why   string
	}{
		{title: "Über the hills", why: "an umlaut against an English article"},
		{title: "Nach Hause with the wind", why: "German markers against English ones"},
		{title: "Die große Runde in the rain", why: "umlaut and markers against English ones"},
		{title: "Straße to the top", why: "ß against an English preposition"},
	} {
		t.Run(tc.title, func(t *testing.T) {
			t.Parallel()

			if got := Language(tc.title); got != "de" {
				t.Errorf("Language(%q) = %q, want de — %s", tc.title, got, tc.why)
			}
		})
	}
}

// And English has to win where the signal really is English.
//
// Without this the fallback would look like detection for every case above.
func TestEnglishOutscoresTheDefault(t *testing.T) {
	t.Parallel()

	for _, title := range []string{
		"Into the hills with a headwind",
		"The long way home",
		"Every climb was a mistake",
	} {
		if got := Language(title); got != "en" {
			t.Errorf("Language(%q) = %q, want en", title, got)
		}
	}
}
