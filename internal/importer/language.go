package importer

import (
	"strings"
	"unicode"
)

// germanMarkers are words and letters that a German title is likely to carry
// and an English one is not.
//
// Short function words, because titles are short. "im", "am", "zum" and the
// rest carry more signal per character than nouns do, and the umlauts and ß
// settle most cases on their own.
var germanMarkers = map[string]struct{}{
	"der": {}, "die": {}, "das": {}, "den": {}, "dem": {}, "des": {},
	"ein": {}, "eine": {}, "einen": {}, "einem": {}, "einer": {},
	"und": {}, "oder": {}, "aber": {}, "nicht": {}, "kein": {}, "keine": {},
	"im": {}, "am": {}, "um": {}, "zum": {}, "zur": {}, "vom": {}, "beim": {},
	"auf": {}, "aus": {}, "bei": {}, "mit": {}, "nach": {}, "von": {}, "vor": {},
	"über": {}, "unter": {}, "durch": {}, "gegen": {}, "ohne": {}, "für": {},
	"ist": {}, "war": {}, "sind": {}, "wird": {}, "hat": {}, "habe": {},
	"ich": {}, "mein": {}, "meine": {}, "sich": {}, "noch": {}, "schon": {},
	"heute": {}, "gestern": {}, "morgen": {}, "wieder": {}, "endlich": {},
	"runde": {}, "fahrt": {}, "tour": {}, "berg": {}, "wald": {}, "see": {},
	"regen": {}, "wind": {}, "sonne": {}, "kalt": {}, "warm": {}, "schnell": {},
	"hause": {}, "arbeit": {}, "stadt": {}, "abend": {}, "nacht": {},
}

// englishMarkers are the same idea, the other way.
var englishMarkers = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "but": {}, "not": {},
	"in": {}, "on": {}, "at": {}, "to": {}, "from": {}, "with": {}, "without": {},
	"over": {}, "under": {}, "through": {}, "against": {}, "into": {}, "for": {},
	"is": {}, "was": {}, "are": {}, "were": {}, "has": {}, "have": {}, "had": {},
	"my": {}, "of": {}, "this": {}, "that": {}, "some": {}, "every": {},
	"ride": {}, "loop": {}, "climb": {}, "hills": {}, "lap": {}, "laps": {},
	"rain": {}, "wind": {}, "sun": {}, "cold": {}, "warm": {}, "fast": {},
	"home": {}, "work": {}, "morning": {}, "evening": {}, "night": {},
}

// Language guesses the language a title was written in.
//
// A heuristic, and only ever applied to imported titles: a title this service
// wrote carries the language the model reported, which is authoritative and is
// stored with it. There is nothing to recover that from on an old activity —
// re-reading it from Strava returns the words and no more — so the choice is
// between guessing and storing nothing.
//
// It guesses, and defaults to German, because that is this athlete's language
// for the utility rides that make up most of a history and because a wrong
// label costs a slightly less well-matched few-shot example rather than a
// wrong title. "wind", "warm" and a few others are deliberately in both lists
// and cancel out.
func Language(title string) string {
	german, english := 0, 0

	for _, r := range title {
		switch r {
		case 'ä', 'ö', 'ü', 'Ä', 'Ö', 'Ü', 'ß':
			// Decisive on their own: no English word carries one.
			german += 2
		}
	}

	for _, word := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r)
	}) {
		if _, ok := germanMarkers[word]; ok {
			german++
		}

		if _, ok := englishMarkers[word]; ok {
			english++
		}
	}

	if english > german {
		return "en"
	}

	return "de"
}
