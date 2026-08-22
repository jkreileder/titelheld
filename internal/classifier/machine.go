package classifier

import (
	"fmt"
	"regexp"
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

// xertDifficulties are the difficulty words Xert puts at the front of a title.
//
// Sourced from the observed title "Difficult Mixed Breakaway Specialist Ride"
// plus the surrounding scale Xert uses. Any word absent here simply fails to
// match, leaving the activity skipped, so an incomplete list is the safe kind
// of wrong.
var xertDifficulties = []string{
	"Easy",
	"Moderate",
	"Challenging",
	"Difficult",
	"Hard",
	"Very Hard",
	"Extreme",
	"Epic",
}

// xertFocusTypes are Xert's rider focus-type names, optionally prefixed by
// "Mixed" in the observed title.
//
// "Pure Endurance" and "Polar Endurance" were added after a history import
// found two titles Xert had written that this list did not recognize — the
// safe kind of wrong, since an unmatched title is skipped rather than
// overwritten, but a naming opportunity lost every time it happens.
var xertFocusTypes = []string{
	"Breakaway Specialist",
	"Climber",
	"GC Specialist",
	"Polar Endurance",
	"Puncheur",
	"Pure Endurance",
	"Rouleur",
	"Sprinter",
	"Time Trialist",
	"Triathlete",
	"All-Rounder",
}

// XertTitlePattern is the machine-title pattern this service ships with.
//
// It matches, case-sensitively, "<difficulty> [Mixed ]<focus type> Ride" —
// Xert's own construction and nothing wider.
func XertTitlePattern() string {
	return `^(?:` + strings.Join(xertDifficulties, "|") + `) ` +
		`(?:Mixed )?(?:` + strings.Join(xertFocusTypes, "|") + `) Ride$`
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

	for _, pattern := range m.patterns {
		if pattern.MatchString(trimmed) {
			return true
		}
	}

	return false
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
