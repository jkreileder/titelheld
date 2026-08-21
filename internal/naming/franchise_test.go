package naming

import (
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
		got, ok := franchise.Next(position)
		if !ok || got != want {
			t.Errorf("Next(%d) = %q, %v; want %q, true", position, got, ok, want)
		}
	}

	for _, position := range []int{2, 3, 100, -1} {
		if got, ok := franchise.Next(position); ok {
			t.Errorf("Next(%d) = %q, true; want no entry", position, got)
		}
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
