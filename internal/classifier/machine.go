package classifier

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Machine titles are titles another tool wrote, which this service may replace.
//
// The default-title gate exists because a title this service did not recognize
// was written by a human, and a human title is never overwritten. Xert breaks
// that assumption: it renames sport rides with its own focus pattern shortly
// after upload, so an activity can arrive carrying a title that looks authored
// but is not.
//
// The asymmetry of the gate is unchanged, and it decides how these patterns are
// written. A pattern that fails to match costs a naming opportunity. A pattern
// that matches too much overwrites something the athlete wrote, which is the
// one outcome the whole design forbids — so the shipped pattern is anchored on
// Xert's own vocabulary at both ends rather than on a shape like
// "<word> <words> Ride", which "Sunday Morning Ride" would also satisfy.

// Xert builds a title as "<difficulty> [<specificity> ]<focus> Ride", and the
// three lists below are that construction's vocabulary. Each is drawn from
// Xert's published glossary and from the titles this athlete's account carries;
// nothing else belongs in them. A word that is absent fails to match, which
// leaves the activity skipped rather than overwritten — the safe kind of wrong.

// xertDifficulties is Xert's difficulty scale, complete as published:
// https://www.baronbiosys.com/glossary/difficulty-rating/
var xertDifficulties = []string{
	"Easy",
	"Moderate",
	"Difficult",
	"Tough",
	"Hard",
}

// xertSpecificities is Xert's specificity scale, complete as published:
// https://www.baronbiosys.com/glossary/specificity-rating/
//
// The slot is optional; Xert leaves it out of some titles.
var xertSpecificities = []string{
	"Pure",
	"Mixed",
	"Polar",
}

// xertFocusTypes are Xert's athlete-type names:
// https://www.baronbiosys.com/glossary/athlete-type/, plus the older twelve-type
// list at https://www.baronbiosys.com/training-right-for-your-event/. Xert's own
// pages spell the time-trial types both "Time-Triallist" and "Time-Trialist", so
// the list carries both.
//
// "Endurance" is the one entry that is not a published focus name — the glossary
// pairs it as "Endurance/Triathlete" — and is taken from the titles Xert has
// written on this account.
var xertFocusTypes = []string{
	"Power Sprinter",
	"Road Sprinter",
	"Pursuiter",
	"Puncheur",
	"Breakaway Specialist",
	"Rouleur",
	"GC Specialist",
	"Climber",
	"Sprint Time-Triallist",
	"Sprint Time-Trialist",
	"Time-Triallist",
	"Time-Trialist",
	"Century Rider",
	"Triathlete",
	"Endurance",
}

// XertTitlePattern is the machine-title pattern this service ships with.
//
// It matches, case-sensitively, "<difficulty> [<specificity> ]<focus> Ride" —
// Xert's own construction and nothing wider.
func XertTitlePattern() string {
	return `^(?:` + strings.Join(xertDifficulties, "|") + `) ` +
		`(?:(?:` + strings.Join(xertSpecificities, "|") + `) )?` +
		`(?:` + strings.Join(xertFocusTypes, "|") + `) Ride$`
}

// The two halves of the looser shape test: a title opening on one of Xert's
// difficulty words, and one of its specificity or focus names standing as a
// whole word after the title's first word.
var (
	xertDifficultyPrefix = regexp.MustCompile(`^(?:` + strings.Join(xertDifficulties, "|") + `) `)
	xertVocabularyWord   = regexp.MustCompile(
		`\b(?:` + strings.Join(slices.Concat(xertSpecificities, xertFocusTypes), "|") + `)\b`)
)

// hasXertShape reports whether a title is built the way Xert builds one, even
// where [XertTitlePattern] does not match it: at least three words ending in
// " Ride", and either an opening difficulty word or one of Xert's specificity
// or focus names after the first word.
//
// Deliberately wider than the pattern, and read only by [IsToolTitle], which
// answers a different question — did a person write this? A vocabulary gap that
// costs the pattern a naming must not also make the title a style example, so
// the shape is enough here while the pattern stays exact for the overwrite
// decision. The width is bounded by what Xert's construction always has: the
// difficulty slot is mandatory, so a Xert title is never two words and never
// opens on a specificity or focus name. "Endurance Ride" and "Hard Ride" are a
// person's, and a person's title that the shape swallowed would never be
// recorded as theirs — a rename to it would leave the attribution line in
// place. Trimming is the caller's, and the " Ride" ending is tested in code
// rather than as `.* Ride$` so the check does not lean on what `.` means around
// a newline.
func hasXertShape(title string) bool {
	if !strings.HasSuffix(title, " Ride") {
		return false
	}

	words := strings.Fields(title)
	if len(words) < 3 {
		return false
	}

	return xertDifficultyPrefix.MatchString(title) ||
		xertVocabularyWord.MatchString(strings.Join(words[1:], " "))
}

// MachineTitles decides whether a title was written by a tool this service may
// overwrite. The zero value matches nothing, so a configuration that says
// nothing about machine titles behaves exactly as before: only Strava defaults
// are renamable.
type MachineTitles struct {
	patterns []*regexp.Regexp
}

// NewMachineTitles compiles a pattern list.
//
// Compiling here rather than at classify time means a malformed pattern is a
// configuration error, reported once at startup, instead of a per-activity
// failure discovered in production.
func NewMachineTitles(patterns []string) (MachineTitles, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))

	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			continue
		}

		// Anchored to the whole title, whatever the pattern says. A regexp
		// matches anywhere by default, so an unanchored "Ride" would accept
		// "The Pink Panther Ride" and this service would overwrite a title the
		// athlete wrote — the one outcome the gate exists to prevent, reachable
		// from configuration alone. \A and \z rather than ^ and $ because those
		// mean line boundaries under (?m), which a configured pattern may set.
		//
		// Wrapping an already-anchored pattern is harmless: the shipped one
		// keeps matching exactly what it matched before.
		expression, err := regexp.Compile(`\A(?:` + pattern + `)\z`)
		if err != nil {
			return MachineTitles{}, fmt.Errorf("classifier: machine-title pattern %q: %w", pattern, err)
		}

		compiled = append(compiled, expression)
	}

	return MachineTitles{patterns: compiled}, nil
}

// DefaultMachineTitles is the shipped set: Xert's pattern and nothing else.
//
// ActivityFix's commute titles are deliberately absent. They are machine
// written too, but they are the *correct* title for that activity — replacing
// "Zur Arbeit" with a generated one would be a regression, not a naming.
func DefaultMachineTitles() MachineTitles {
	titles, err := NewMachineTitles([]string{XertTitlePattern()})
	if err != nil {
		// The shipped pattern is a constant; a failure here is a programming
		// error rather than anything a deployment can cause.
		panic("classifier: the shipped machine-title pattern does not compile: " + err.Error())
	}

	return titles
}

// Matches reports whether the title was written by a recognized machine.
func (m MachineTitles) Matches(title string) bool {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return false
	}

	return slices.ContainsFunc(m.patterns, func(pattern *regexp.Regexp) bool {
		return pattern.MatchString(trimmed)
	})
}

// renamable reports whether the gate lets this title be replaced: either Strava
// never got around to naming it, or a recognized tool did and may be corrected.
func (m MachineTitles) renamable(title string) bool {
	return IsDefaultTitle(title) || m.Matches(title)
}

// IsEmpty reports whether no patterns are configured.
//
// A zero MachineTitles matches no title, which reads as "recognize nothing"
// and is almost never what a caller meant — so callers that can default to
// the shipped set need to be able to tell.
func (m MachineTitles) IsEmpty() bool {
	return len(m.patterns) == 0
}
