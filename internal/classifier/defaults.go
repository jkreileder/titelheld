package classifier

import "strings"

// Strava default titles, as data rather than a regular expression, so a locale
// can be added by extending a table.
//
// The gate this feeds is deliberately asymmetric: a title that is *not* in the
// table is treated as human- or app-authored and the activity is skipped. A
// missing pattern therefore costs a naming opportunity, never a wrong write —
// which is why best-effort localisation is acceptable here.
//
// English is authoritative: it is the set this athlete's account actually
// produces and it matches the pattern given in the build spec.
var (
	englishDayparts = []string{
		"Morning",
		"Lunch",
		"Afternoon",
		"Evening",
		"Night",
	}

	englishActivities = []string{
		"Ride",
		"Gravel Ride",
		"Run",
		"Walk",
		"Hike",
		"Swim",
		"Workout",
		"Weight Training",
	}

	// German defaults use "<activity noun> <daypart phrase>" rather than a
	// leading adjective. Only "Gewichtstraining am Abend" has been observed on
	// real data (Whoop-pushed strength sessions arrive with the German
	// default); the remaining nouns follow the same construction and are
	// unverified. They are safe to carry: an unrecognised title fails closed.
	germanDayparts = []string{
		"am Morgen",
		"am Mittag",
		"am Nachmittag",
		"am Abend",
		"in der Nacht",
	}

	germanActivities = []string{
		"Radfahrt",
		"Gravel-Fahrt",
		"Lauf",
		"Spaziergang",
		"Wanderung",
		"Schwimmen",
		"Workout",
		"Gewichtstraining",
	}
)

// defaultTitles holds every recognised Strava default title. Built once at
// package initialisation; exact-match lookup replaces an anchored regex.
var defaultTitles = buildDefaultTitles()

func buildDefaultTitles() map[string]struct{} {
	titles := make(map[string]struct{},
		len(englishDayparts)*len(englishActivities)+len(germanDayparts)*len(germanActivities))

	for _, daypart := range englishDayparts {
		for _, activity := range englishActivities {
			titles[daypart+" "+activity] = struct{}{}
		}
	}
	for _, daypart := range germanDayparts {
		for _, activity := range germanActivities {
			titles[activity+" "+daypart] = struct{}{}
		}
	}

	return titles
}

// IsDefaultTitle reports whether title is still a Strava-generated default.
//
// Surrounding whitespace is ignored; matching is otherwise exact and
// case-sensitive. Anything else — including an empty title — is treated as
// authored by a human or another tool, which means this service must leave it
// alone.
func IsDefaultTitle(title string) bool {
	_, ok := defaultTitles[strings.TrimSpace(title)]

	return ok
}
