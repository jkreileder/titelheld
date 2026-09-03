package naming

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// RepeatsTitle reports whether a candidate title is one of the titles the
// prompt listed as not to be repeated, and which one.
//
// The prompt states the rule for RECENT and for EXAMPLES; this is what makes
// it binding, the way [CopiesAchievement] binds the segment rule and
// [Guarded.Claimed] the franchise rule. A repeat is refused rather than
// rewritten: the model is sampled at temperature, so the next draw is a real
// second chance.
//
// The comparison is equality after a normalization stricter than the franchise
// matcher's. That one lowercases; this one folds, which maps "ß" to "ss", so
// "GROSSE RUNDE" and "große runde" are the one title they are to a reader. NFKC
// comes first, so a compatibility spelling is not a new title either. What is
// left is words separated by single spaces, with emoji and punctuation gone:
// "Windig 🌬️" is the title "windig", and refusing it is the point.
//
// Equality and not containment: a title that carries a previous one inside a
// longer sentence is a new title, and the variety rule the prompt states is
// about the move, not the substring.
func RepeatsTitle(candidate string, titles []string) (string, bool) {
	normalized := normalizeForRepeat(candidate)
	if normalized == "" {
		return "", false
	}

	for _, title := range titles {
		if known := normalizeForRepeat(title); known != "" && normalized == known {
			return title, true
		}
	}

	return "", false
}

// ForbiddenTitles are the titles a candidate may not be: every RECENT line the
// prompt carries and every few-shot example's title.
//
// Both blocks and nothing else. RECENT and EXAMPLES are what the model is shown
// and told not to repeat, so they are what [RepeatsTitle] measures against; a
// title the prompt never carried is a fresh one.
//
// The RECENT lines are the ones [BuildPrompt] writes, truncation included: the
// refusal and the prompt read the same list through the same cap, so a title
// beyond the cap is neither shown nor refused.
func (c Context) ForbiddenTitles() []string {
	recent := capTitles(c.RecentTitles)

	titles := make([]string, 0, len(recent)+len(c.Examples))
	titles = append(titles, recent...)

	for _, example := range c.Examples {
		titles = append(titles, example.Title)
	}

	return titles
}

// normalizeForRepeat reduces a title to the words a reader would compare.
//
// NFKC first, so a compatibility form and its canonical spelling are one
// string. Then full case folding rather than lowercasing: folding maps "ß" to
// "ss", which lowercasing does not, and "GROSSE RUNDE" and "große runde" are
// the same title to everyone but a byte comparison. Emoji and the pieces that
// build them are removed rather than turned into spaces, so one sitting inside
// a word does not split it; everything else that is not a letter or a digit
// becomes a space, and whitespace runs collapse.
//
// Nonspacing marks are removed rather than spaced, and that is bounded by the
// languages this service writes in. [Language] admits German and English only,
// and NFKC has already composed every mark either of them spells a letter with,
// so a mark that survives — one with no precomposed form, or the combining dot
// folding leaves behind where it maps "İ" to "i" — tells no two words apart,
// while spacing it would split "İstanbul" in two. In a script where a mark is
// itself the distinction between two words, dropping it would make one title of
// two, so the rule holds for de and en and is not a general one.
func normalizeForRepeat(text string) string {
	folded := cases.Fold().String(norm.NFKC.String(text))

	mapped := strings.Map(func(r rune) rune {
		switch {
		case isEmojiPart(r) || unicode.Is(unicode.Mn, r):
			return -1
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return r
		default:
			return ' '
		}
	}, folded)

	return strings.Join(strings.Fields(mapped), " ")
}
