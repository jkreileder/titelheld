package naming

import (
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// numberWords maps the number words this service refuses unless the prompt
// states them, keyed in case-folded form, so "dreißig" is "dreissig".
//
// German and English 0–20, the tens, hundred and thousand. "ein" and "eine"
// are not in the table: they are articles far more often than numbers, and a
// title may say "ein Sprint". "eins" is. The table is finite by design: a
// number word outside it — "sechsundzwanzig", a compound such as
// "Dreiländereck" — passes, because matching is by whole word.
var numberWords = map[string]int{
	"null": 0, "eins": 1, "zwei": 2, "zwo": 2, "drei": 3, "vier": 4,
	"fünf": 5, "sechs": 6, "sieben": 7, "acht": 8, "neun": 9, "zehn": 10,
	"elf": 11, "zwölf": 12, "dreizehn": 13, "vierzehn": 14, "fünfzehn": 15,
	"sechzehn": 16, "siebzehn": 17, "achtzehn": 18, "neunzehn": 19,
	"zwanzig": 20, "dreissig": 30, "vierzig": 40, "fünfzig": 50,
	"sechzig": 60, "siebzig": 70, "achtzig": 80, "neunzig": 90,
	"hundert": 100, "tausend": 1000,

	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
	"twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50, "sixty": 60,
	"seventy": 70, "eighty": 80, "ninety": 90, "hundred": 100,
	"thousand": 1000,
}

// halfHour is the word that turns the number after it into a clock time.
const halfHour = "halb"

// bikeLabel is the one RIDE line that states no figure: the gear name is text
// the athlete typed into Strava, and "Trek Domane SL 6" must not license a
// six in a title.
const bikeLabel = "- Bike: "

// Figures is what the prompt's RIDE and NOTES lines state: the figures a
// service title may carry.
type Figures struct {
	// values holds the value of every number in those lines in canonical
	// form, so "25.0", "25,0" and "025" are all "25". A time contributes its
	// hour and nothing else, and a fractional digit is never a value of its
	// own: "2.6" states 2.6, not 2 or 6.
	values map[string]struct{}

	// times holds every time in those lines, hour canonical, so a title's
	// "15:00" is stated by "Start hour: 15:00".
	times map[string]struct{}

	// words holds every case-folded word in those lines, so a number word the
	// prompt spells out is stated by the same word.
	words map[string]struct{}
}

// StatedFigures reads the figures a prompt states from its RIDE and NOTES
// blocks, less the Bike line, plus any text given as also: the franchise
// entry the prompt offers, which is the operator's own configuration and may
// carry a number of its own ("The Pink Panther 2").
//
// Only those two blocks: they are what the service measured or parsed about
// the ride. A figure in ACHIEVEMENTS, RECENT or EXAMPLES is somebody else's
// wording or another ride's, and a title that carries it states nothing about
// this ride. The Bike line is left out for the same reason: the gear name is
// text the athlete typed. A block is headed by an uppercase word on its own
// line and ends at the next heading; every value in the prompt is flattened
// to one line before it is written, so a value cannot forge a heading.
func StatedFigures(userPrompt string, also ...string) Figures {
	stated := Figures{
		values: map[string]struct{}{},
		times:  map[string]struct{}{},
		words:  map[string]struct{}{},
	}

	heading := ""

	for line := range strings.SplitSeq(userPrompt, "\n") {
		if isHeading(line) {
			heading = line

			continue
		}

		if heading != "RIDE" && heading != "NOTES" {
			continue
		}

		if heading == "RIDE" && strings.HasPrefix(line, bikeLabel) {
			continue
		}

		stated.add(line)
	}

	for _, text := range also {
		stated.add(text)
	}

	return stated
}

// add records every figure one line states.
func (f *Figures) add(line string) {
	for _, token := range figureTokens(line) {
		switch token.kind {
		case tokenNumber:
			f.values[canonicalNumber(token.text)] = struct{}{}
		case tokenTime:
			hour, minute, _ := strings.Cut(token.text, ":")
			f.values[canonicalNumber(hour)] = struct{}{}
			f.times[canonicalNumber(hour)+":"+minute] = struct{}{}
		case tokenWord:
			f.words[token.folded] = struct{}{}

			// A number word the prompt spells out states its value as well
			// as its spelling, so "sechs" under NOTES states "6" too.
			if value, ok := numberWords[token.folded]; ok {
				f.values[strconv.Itoa(value)] = struct{}{}
			}
		}
	}
}

// UnstatedFigure reports the first figure in a title that the prompt does not
// state, as the title spells it.
//
// A figure is an integer, a decimal ("25,9" or "25.9"), a time ("15:00"), or a
// number word from the table. A number is stated only when its value equals
// the value of a number in a RIDE or NOTES line: "25" and "25er" are stated by
// "25.0", "25,9" by "25.9". A figure must be stated as it stands, never
// rounded, so "26" is not stated by "26.3"; and a fractional digit of a
// decimal states nothing on its own, so "2" is not stated by "2.6" and "6" is
// not by "56%". A range such as "20.1-23" is two numbers, and a time in a line
// states its hour and nothing else. A time in a title is stated by the same
// time. A number word is stated when its value equals a stated number or the
// word itself occurs in those lines. A figure inside a longer token still
// counts: "25er", "25km" and "Teil sechs" all carry one.
//
// "halb N", with N a number word or a digit run, is a clock time derived from
// the start hour and is always refused: the prompt states no such time, and
// the phrase names the half hour before N, so no start-hour line could state
// it as it stands.
//
// The cost is deliberate: a figure the model rounded, or a year, is refused
// along with an invented one. The model is sampled at temperature, so the next
// draw is a real second chance.
func UnstatedFigure(title string, stated Figures) (string, bool) {
	tokens := figureTokens(title)

	for index := 0; index < len(tokens); index++ {
		token := tokens[index]

		if token.kind == tokenWord && token.folded == halfHour && index+1 < len(tokens) {
			if _, ok := tokens[index+1].value(); ok {
				return token.text + " " + tokens[index+1].text, true
			}
		}

		if !stated.covers(token) {
			return token.text, true
		}
	}

	return "", false
}

// covers reports whether one title token is stated, or carries no figure.
func (f Figures) covers(token figureToken) bool {
	switch token.kind {
	case tokenNumber:
		_, ok := f.values[canonicalNumber(token.text)]

		return ok
	case tokenTime:
		hour, minute, _ := strings.Cut(token.text, ":")
		_, ok := f.times[canonicalNumber(hour)+":"+minute]

		return ok
	case tokenWord:
		value, isNumber := numberWords[token.folded]
		if !isNumber {
			return true
		}

		if _, ok := f.values[strconv.Itoa(value)]; ok {
			return true
		}

		_, ok := f.words[token.folded]

		return ok
	default:
		return true
	}
}

// canonicalNumber writes a number so that two spellings of one value are one
// string: either decimal separator, no leading zeros, no trailing fractional
// zeros. "025,50" and "25.5" are both "25.5"; "25.0" is "25".
func canonicalNumber(text string) string {
	whole, fraction, _ := strings.Cut(strings.ReplaceAll(text, ",", "."), ".")

	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}

	if fraction = strings.TrimRight(fraction, "0"); fraction == "" {
		return whole
	}

	return whole + "." + fraction
}

type tokenKind int

const (
	tokenWord tokenKind = iota
	tokenNumber
	tokenTime
)

// figureToken is one word or one number from a title or a prompt line.
type figureToken struct {
	kind tokenKind

	// text is the token as written, after NFKC.
	text string

	// folded is a word's case-folded form, the key into [numberWords].
	folded string
}

// value is the integer a token denotes, if it denotes one: a whole digit run,
// or a number word from the table.
func (t figureToken) value() (int, bool) {
	switch t.kind {
	case tokenNumber:
		value, err := strconv.Atoi(t.text)

		return value, err == nil
	case tokenWord:
		value, ok := numberWords[t.folded]

		return value, ok
	default:
		return 0, false
	}
}

// figureTokens splits text into words and numbers.
//
// A word is a run of letters, a number a run of digits with at most one
// decimal separator between two digits, and a time two digit runs joined by a
// colon; everything else separates tokens, so "20.1-23" is two numbers. A
// letter run and a digit run that touch are two tokens, so "25er" is the
// number "25" and the word "er". Any Unicode digit starts a number, and one
// outside ASCII is never stated by a prompt that writes ASCII digits, so it is
// refused rather than overlooked.
func figureTokens(text string) []figureToken {
	runes := []rune(norm.NFKC.String(text))
	fold := cases.Fold()

	var tokens []figureToken

	for index := 0; index < len(runes); {
		start := index

		switch {
		case unicode.IsDigit(runes[index]):
			index = digitRunEnd(runes, index)
			kind := tokenNumber

			if index+1 < len(runes) && unicode.IsDigit(runes[index+1]) {
				switch runes[index] {
				case '.', ',':
					index = digitRunEnd(runes, index+1)
				case ':':
					index = digitRunEnd(runes, index+1)
					kind = tokenTime
				}
			}

			tokens = append(tokens, figureToken{kind: kind, text: string(runes[start:index])})
		case unicode.IsLetter(runes[index]):
			for index < len(runes) && (unicode.IsLetter(runes[index]) || unicode.Is(unicode.Mn, runes[index])) {
				index++
			}

			word := string(runes[start:index])
			tokens = append(tokens, figureToken{kind: tokenWord, text: word, folded: fold.String(word)})
		default:
			index++
		}
	}

	return tokens
}

// digitRunEnd is the index just past the digit run starting at index.
func digitRunEnd(runes []rune, index int) int {
	for index < len(runes) && unicode.IsDigit(runes[index]) {
		index++
	}

	return index
}

// isHeading reports whether a prompt line is a block heading: an uppercase
// word on its own line.
func isHeading(line string) bool {
	if line == "" {
		return false
	}

	for _, r := range line {
		if r < 'A' || r > 'Z' {
			return false
		}
	}

	return true
}
