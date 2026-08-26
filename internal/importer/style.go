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

	if classifier.IsToolTitle(title) {
		return true
	}

	return slices.ContainsFunc(d.TemplateTitles, func(candidate string) bool {
		return strings.TrimSpace(candidate) == title
	})
}
