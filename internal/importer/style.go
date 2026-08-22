package importer

import (
	"slices"
	"strings"

	"github.com/jkreileder/titelheld/internal/classifier"
)

// importableSportTypes are the sport types whose titles are worth remembering.
//
// Rides and gravel rides: the two this service names with a model, and so the
// two whose past titles are the athlete's own naming of a ride. Everything else
// — a run, a walk, a strength session, a virtual ride Zwift titled after its
// route — is either never named here or never named by a person, and its title
// says nothing about how this athlete names a ride.
var importableSportTypes = []string{"Ride", "GravelRide"}

// Fragments that mark a title as another tool's output.
//
// Deliberately not part of [classifier.MachineTitles]. That set answers a
// different question — may this service overwrite the title? — and a Zwift
// ride's answer is no: the shipped virtual-ride policy keeps what Zwift wrote.
// Adding these there would make thousands of virtual rides renamable as a side
// effect of tidying an import, which is the kind of change that looks like
// housekeeping and is not.
const (
	zwiftTitlePrefix = "Zwift - "
	xertTitleSuffix  = " - Xert"
)

// importableSport reports whether an activity's titles belong in the history.
func importableSport(sportType string) bool {
	return slices.Contains(importableSportTypes, strings.TrimSpace(sportType))
}

// notTheAthletesStyle reports whether a title was written by something other
// than the athlete, or by this service on purpose to repeat.
//
// Trimmed once, up front: Strava keeps whatever trailing space a title was
// saved with, and a template that fails to match because of one is a template
// seeded as style.
func (d Deps) notTheAthletesStyle(title string) bool {
	title = strings.TrimSpace(title)

	if classifier.IsDefaultTitle(title) || d.MachineTitles.Matches(title) {
		return true
	}

	// Zwift names a virtual ride after the route it was ridden on, and Xert
	// marks what it touched. Both are the tool talking, whatever the sport
	// type says: a Zwift ride recorded as a plain Ride carries the same title.
	if strings.HasPrefix(title, zwiftTitlePrefix) || strings.HasSuffix(title, xertTitleSuffix) {
		return true
	}

	return slices.ContainsFunc(d.TemplateTitles, func(candidate string) bool {
		return strings.TrimSpace(candidate) == title
	})
}
