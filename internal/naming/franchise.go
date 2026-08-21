package naming

import "strings"

// Franchise is an ordered list of titles walked one entry at a time.
//
// Data, not code. The store remembers only how far along an athlete is, so a
// series can be renamed, reordered or extended here without migrating
// anything — and a franchise removed from this list simply stops applying.
type Franchise struct {
	// Name is the key the position is stored under. It is not shown to the
	// model and never changes once a series has been started: renaming it
	// starts the athlete over at the first entry.
	Name string

	// SportTypes are the Strava sport types the franchise applies to. Empty
	// means any.
	SportTypes []string

	// GearName is the bike the franchise rides on, matched case-insensitively
	// and in full. Empty means any.
	GearName string

	// Titles are the entries, in order.
	Titles []string
}

// Next returns the entry at a position, and whether there is one.
//
// A position past the end is not an error and not a wrap-around: the series
// is finished, the franchise stops applying, and the ride is named normally.
func (f Franchise) Next(position int) (string, bool) {
	if position < 0 || position >= len(f.Titles) {
		return "", false
	}

	return f.Titles[position], true
}

// Applies reports whether a ride belongs to this franchise.
func (f Franchise) Applies(sportType, gearName string) bool {
	if f.GearName != "" && !strings.EqualFold(strings.TrimSpace(gearName), f.GearName) {
		return false
	}

	if len(f.SportTypes) == 0 {
		return true
	}

	for _, want := range f.SportTypes {
		if strings.EqualFold(sportType, want) {
			return true
		}
	}

	return false
}

// DefaultProfile is the franchise set a deployment starts with.
//
// Franchises are data, not code: the athlete's own list lives in their
// Firestore configuration document, where adding one needs no release. This is
// the default profile the spec asks for — what applies until a document is
// written, and what a first document is seeded from.
//
// The three entries already used by hand — "The Pink Panther Checks Inn",
// "The Pink Panther Strikes Again" and "Revenge of the Pink Panther" — are
// deliberately absent. A stored position starts at zero, so listing them would
// hand out a title the athlete already has. A canon added later that has never
// been used needs no such care; one that has, needs its position seeded, and
// the README says how.
func DefaultProfile() []Franchise {
	return []Franchise{
		{
			Name:       "pink-panther",
			SportTypes: []string{"GravelRide", "Ride"},
			GearName:   "Pink Panther",
			Titles: []string{
				"The Pink Panther",
				"A Shot in the Dark",
				"Inspector Clouseau",
				"The Return of the Pink Panther",
				"Trail of the Pink Panther",
				"Curse of the Pink Panther",
				"Son of the Pink Panther",
			},
		},
	}
}

// FranchiseFor returns the franchise a ride belongs to, if any.
//
// The first match wins, so order in the list is precedence.
func FranchiseFor(franchises []Franchise, sportType, gearName string) (Franchise, bool) {
	for _, franchise := range franchises {
		if franchise.Applies(sportType, gearName) {
			return franchise, true
		}
	}

	return Franchise{}, false
}
