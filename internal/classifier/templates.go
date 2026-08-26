package classifier

import (
	"slices"
	"strings"
)

// The titles this service writes without asking a model.
//
// A commute has two names and an errand has three, and both are meant to
// repeat: they are the correct title for that activity rather than a style
// anything should learn from. Two callers need the same list and neither may
// guess it — the processor picks one when it names, and the history import
// skips any title identical to one, so a template this service would write
// itself never enters the log as the athlete's own style.
//
// They live here, next to the commute defaults, rather than beside either
// caller. A second copy of "Besorgungen" somewhere else is a copy that can
// drift, and the failure it causes is silent: the import seeds three template
// titles as style and nothing reports it.
var defaultErrandTitles = []string{"Besorgungen", "In die Stadt", "Stadtrunde"}

// DefaultErrandTitles is the errand pool this service ships with.
//
// Cloned, because a caller that sorted or reordered the returned slice would
// change which title every future errand gets: the processor picks from it by
// activity ID, so the order is part of the behavior.
func DefaultErrandTitles() []string {
	return slices.Clone(defaultErrandTitles)
}

// CommuteTitle is the title this configuration gives a commute.
//
// The fallback lives here rather than at the call site so that the title the
// processor writes and the title the import skips are decided by one function.
// Configured empty means the shipped German name, not an empty title.
func (c Config) CommuteTitle(direction Direction) string {
	if direction == DirectionToHome {
		if c.ToHomeTitle != "" {
			return c.ToHomeTitle
		}

		return defaultToHomeTitle
	}

	if c.ToWorkTitle != "" {
		return c.ToWorkTitle
	}

	return defaultToWorkTitle
}

// TemplateTitles is every title this configuration produces deterministically.
//
// The import consults it to keep them out of the seeded history. It is derived
// from the configuration rather than listed as a constant, so an athlete who
// renames their commute keeps the skip: the list a run skips is the list that
// run would write.
func (c Config) TemplateTitles() []string {
	titles := make([]string, 0, 2+len(defaultErrandTitles))
	titles = append(titles, c.CommuteTitle(DirectionToWork), c.CommuteTitle(DirectionToHome))

	return append(titles, defaultErrandTitles...)
}

// Fragments that mark a title as another tool's output.
//
// Deliberately not part of [MachineTitles]. That set answers a different
// question — may this service overwrite the title? — and a Zwift ride's answer
// is no: the shipped virtual-ride policy keeps what Zwift wrote. This answers
// "did a person write it?", which the title history asks of every ride it
// records, whether by import or by the skip gate.
const (
	zwiftTitlePrefix = "Zwift - "
	xertTitleSuffix  = " - Xert"
)

// IsToolTitle reports whether a title is a tool's output rather than a
// person's: Zwift names a virtual ride after its route, and Xert marks what
// it touched. Both are the tool talking whatever the sport type says — a Zwift
// ride recorded as a plain Ride carries the same title. Trimmed, because Strava
// keeps whatever trailing space a title was saved with.
func IsToolTitle(title string) bool {
	title = strings.TrimSpace(title)

	return strings.HasPrefix(title, zwiftTitlePrefix) || strings.HasSuffix(title, xertTitleSuffix)
}
