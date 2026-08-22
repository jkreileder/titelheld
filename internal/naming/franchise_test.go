package naming

import (
	"slices"
	"strings"
	"testing"
)

// A franchise applies to the bike and sport types it names, and nothing else.
func TestFranchiseApplies(t *testing.T) {
	t.Parallel()

	gravelOnly := Franchise{
		Name:       "test",
		SportTypes: []string{"GravelRide"},
		GearName:   "Pink Panther",
	}

	for _, tc := range []struct {
		name      string
		sportType string
		gearName  string
		want      bool
	}{
		{name: "the right bike and sport", sportType: "GravelRide", gearName: "Pink Panther", want: true},
		{name: "case-insensitive gear", sportType: "GravelRide", gearName: "pink panther", want: true},
		{name: "surrounding space", sportType: "GravelRide", gearName: "  Pink Panther  ", want: true},
		{name: "case-insensitive sport", sportType: "gravelride", gearName: "Pink Panther", want: true},
		{name: "another bike", sportType: "GravelRide", gearName: "Musterrad", want: false},
		{name: "no bike at all", sportType: "GravelRide", gearName: "", want: false},
		{name: "another sport", sportType: "Run", gearName: "Pink Panther", want: false},
		{name: "a partial name", sportType: "GravelRide", gearName: "Pink", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := gravelOnly.Applies(tc.sportType, tc.gearName); got != tc.want {
				t.Errorf("Applies(%q, %q) = %v, want %v",
					tc.sportType, tc.gearName, got, tc.want)
			}
		})
	}
}

// An empty rule matches anything, so a franchise can be unconditional.
func TestAnUnrestrictedFranchiseApplies(t *testing.T) {
	t.Parallel()

	anything := Franchise{Name: "test"}

	if !anything.Applies("Run", "") {
		t.Error("a franchise with no rules did not apply")
	}
}

// A gear rule with no sport types still checks the gear.
func TestAGearOnlyFranchise(t *testing.T) {
	t.Parallel()

	gearOnly := Franchise{Name: "test", GearName: "Pink Panther"}

	if !gearOnly.Applies("Run", "Pink Panther") {
		t.Error("a gear-only franchise did not apply to the right bike")
	}

	if gearOnly.Applies("Run", "Musterrad") {
		t.Error("a gear-only franchise applied to another bike")
	}
}

// Positions walk the list and then stop. A finished series is not an error and
// does not wrap: the ride is named normally.
func TestFranchiseNext(t *testing.T) {
	t.Parallel()

	franchise := Franchise{Titles: []string{"first", "second"}}

	for position, want := range map[int]string{0: "first", 1: "second"} {
		got, index, ok := franchise.Next(position)
		if !ok || got != want || index != position {
			t.Errorf("Next(%d) = %q, %d, %v; want %q, %d, true",
				position, got, index, ok, want, position)
		}
	}

	for _, position := range []int{2, 3, 100, -1} {
		if got, _, ok := franchise.Next(position); ok {
			t.Errorf("Next(%d) = %q, true; want no entry", position, got)
		}
	}
}

// A reserved entry is never offered, and the index says where the rotation
// resumed from.
//
// The index is the whole point: a caller that advanced by one from a position
// that stepped over a reserved entry would offer that reserved entry next.
func TestFranchiseNextStepsOverReservedEntries(t *testing.T) {
	t.Parallel()

	franchise := Franchise{
		Titles:   []string{"first", "second", "third", "fourth"},
		Reserved: []string{"  FIRST ", "second"},
	}

	title, index, ok := franchise.Next(0)
	if !ok || title != "third" || index != 2 {
		t.Errorf("Next(0) = %q, %d, %v; want \"third\", 2, true", title, index, ok)
	}

	// And from a position already past them, nothing changes.
	title, index, ok = franchise.Next(3)
	if !ok || title != "fourth" || index != 3 {
		t.Errorf("Next(3) = %q, %d, %v; want \"fourth\", 3, true", title, index, ok)
	}
}

// A series whose remaining entries are all reserved offers nothing.
//
// Not an error and not a fallback to an entry the athlete is keeping: the ride
// is named normally, which is what an exhausted series does too.
func TestAFullyReservedFranchiseOffersNothing(t *testing.T) {
	t.Parallel()

	franchise := Franchise{
		Titles:   []string{"first", "second"},
		Reserved: []string{"first", "second"},
	}

	if title, _, ok := franchise.Next(0); ok {
		t.Errorf("Next(0) = %q, true; want no entry", title)
	}
}

// A reserved entry that is not in the list changes nothing.
func TestAnUnknownReservationIsInert(t *testing.T) {
	t.Parallel()

	franchise := Franchise{
		Titles:   []string{"first"},
		Reserved: []string{"a title that is not in this series"},
	}

	if title, _, ok := franchise.Next(0); !ok || title != "first" {
		t.Errorf("Next(0) = %q, %v; want \"first\", true", title, ok)
	}
}

// The shipped set omits the entries already used by hand.
//
// The store starts every franchise at zero and nothing can seed it, so a list
// that included them would hand out a title the athlete already has.
func TestDefaultFranchisesOmitTheUsedEntries(t *testing.T) {
	t.Parallel()

	franchises := DefaultProfile()
	if len(franchises) == 0 {
		t.Fatal("no franchise is shipped")
	}

	pink := franchises[0]

	if pink.Name != "pink-panther" {
		t.Errorf("the shipped franchise is %q", pink.Name)
	}

	for _, used := range []string{
		"The Pink Panther Checks Inn",
		"The Pink Panther Strikes Again",
		"Revenge of the Pink Panther",
		"Curse of the Pink Panther",
	} {
		for _, title := range pink.Titles {
			if strings.EqualFold(title, used) {
				t.Errorf("the shipped list includes %q, which has already been used", used)
			}
		}
	}

	if len(pink.Titles) == 0 {
		t.Error("the shipped franchise has no titles left")
	}
}

// The shipped rotation resumes after the entry the athlete used last.
//
// The athlete walks the canon in release order and is at "Curse of the Pink
// Panther"; "Trail of the Pink Panther" was stepped over deliberately and is
// theirs to spend. So the first entry this service may offer is the film after
// Curse, and it must be offered at position zero — nothing seeds a stored
// position, and a first offer of an earlier film would walk the series
// backwards.
func TestTheShippedFranchiseResumesAfterCurse(t *testing.T) {
	t.Parallel()

	pink := DefaultProfile()[0]

	title, _, ok := pink.Next(0)
	if !ok {
		t.Fatal("the shipped franchise offers nothing at position zero")
	}

	if title != "Son of the Pink Panther" {
		t.Errorf("the first entry offered is %q, want \"Son of the Pink Panther\"", title)
	}

	if !slices.Contains(pink.Reserved, "Trail of the Pink Panther") {
		t.Error("\"Trail of the Pink Panther\" is not reserved")
	}
}

// The first matching franchise wins, so order is precedence.
func TestFranchiseForTakesTheFirstMatch(t *testing.T) {
	t.Parallel()

	franchises := []Franchise{
		{Name: "specific", SportTypes: []string{"GravelRide"}, GearName: "Pink Panther"},
		{Name: "catch-all"},
	}

	got, ok := FranchiseFor(franchises, "GravelRide", "Pink Panther")
	if !ok || got.Name != "specific" {
		t.Errorf("FranchiseFor = %q, %v; want the specific one", got.Name, ok)
	}

	got, ok = FranchiseFor(franchises, "Run", "")
	if !ok || got.Name != "catch-all" {
		t.Errorf("FranchiseFor = %q, %v; want the catch-all", got.Name, ok)
	}

	if _, ok := FranchiseFor(nil, "GravelRide", "Pink Panther"); ok {
		t.Error("an empty franchise list matched")
	}

	if _, ok := FranchiseFor([]Franchise{franchises[0]}, "Run", "Musterrad"); ok {
		t.Error("a franchise matched a ride it does not apply to")
	}
}

// A title counts as using an entry only when the entry's wording is in it.
//
// This is the check that decides whether a film is spent, so the two errors
// are not symmetrical: a use that is not recognized costs a repeated offer,
// and a non-use that is recognized loses a film with nothing to say where it
// went. The table leans that way on purpose.
func TestUsesEntry(t *testing.T) {
	t.Parallel()

	const entry = "Curse of the Pink Panther"

	for _, tc := range []struct {
		name  string
		title string
		entry string
		want  bool
	}{
		{name: "verbatim", title: entry, entry: entry, want: true},
		{name: "extended", title: "Curse of the Pink Panther im Nebel", entry: entry, want: true},
		{name: "punctuated", title: "Curse of the Pink Panther: Gegenwind", entry: entry, want: true},
		{name: "different case", title: "CURSE OF THE PINK PANTHER", entry: entry, want: true},
		{name: "collapsed whitespace", title: "Curse  of the\tPink Panther", entry: entry, want: true},
		{name: "leading article dropped", title: "Pink Panther im Nebel", entry: "The Pink Panther", want: true},
		{name: "the article is still allowed", title: "The Pink Panther", entry: "The Pink Panther", want: true},

		// The negative the whole rule exists for: a title in the right key
		// that never names the entry.
		{name: "themed but not the entry", title: "Der Panther im Morgengrauen", entry: entry, want: false},
		{name: "another film in the series", title: "Revenge of the Pink Panther", entry: entry, want: false},
		{name: "the franchise but not the entry", title: "Pink Panther am Musterbach", entry: entry, want: false},
		{name: "translated", title: "Fluch des rosaroten Panthers", entry: entry, want: false},
		{name: "half of it", title: "Curse of the Panther", entry: entry, want: false},
		{name: "a longer word", title: "A Shot in the Darkness", entry: "A Shot in the Dark", want: false},
		{name: "no entry offered", title: "Musterrunde", entry: "", want: false},
		{name: "an entry of punctuation only", title: "Musterrunde", entry: "---", want: false},
		{name: "an empty title", title: "", entry: entry, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := UsesEntry(tc.title, tc.entry); got != tc.want {
				t.Errorf("UsesEntry(%q, %q) = %v, want %v", tc.title, tc.entry, got, tc.want)
			}
		})
	}
}
