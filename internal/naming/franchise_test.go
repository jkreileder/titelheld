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

// An entry that cannot be a title is not offerable.
//
// The prompt bounds every value it prints, so an over-long entry would be
// shown as a prefix of itself — and no title can contain what was never shown.
// Offering it would cost three model calls on every ride, forever.
func TestOfferable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		entry string
		want  bool
	}{
		{name: "an ordinary entry", entry: "Son of the Pink Panther", want: true},
		{name: "exactly the limit", entry: strings.Repeat("a", MaxTitleRunes), want: true},
		{name: "one rune over", entry: strings.Repeat("a", MaxTitleRunes+1), want: false},
		{name: "long in runes, not bytes", entry: strings.Repeat("ü", MaxTitleRunes+1), want: false},

		// Counted in runes: sixty umlauts are a hundred and twenty bytes and
		// still a title this service would accept.
		{name: "umlauts at the limit", entry: strings.Repeat("ü", MaxTitleRunes), want: true},
		{name: "empty", entry: "", want: false},
		{name: "whitespace only", entry: "   ", want: false},

		// Nothing UsesEntry could ever find in a title. Offerable has to
		// refuse exactly what the spending check cannot recognize, or the
		// entry is declined on every ride forever.
		{name: "punctuation only", entry: "---", want: false},
		{name: "punctuation and spaces", entry: " — : ", want: false},

		// An article alone survives: the leading article is only dropped when
		// something follows it, so "The" is still a token a title can carry.
		// Absurd as an entry, and consistent, which is what matters here.
		{name: "an article alone", entry: "The", want: true},
		{name: "surrounding space does not count", entry: "  " + strings.Repeat("a", MaxTitleRunes) + "  ", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Offerable(tc.entry); got != tc.want {
				t.Errorf("Offerable(%d runes) = %v, want %v", len([]rune(tc.entry)), got, tc.want)
			}
		})
	}
}

// Offerable and UsesEntry agree: nothing offerable is unrecognizable.
//
// The pair is the contract — an entry that gets offered has to be one a title
// can demonstrably use, or the rotation stalls on it. Asserted over the two
// together rather than trusting that the same rule was written twice.
func TestNothingOfferableIsImpossibleToUse(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{
		"Son of the Pink Panther", "A Shot in the Dark", "The Pink Panther",
		"---", "The", "   ", "", "Ocean's Eleven", "8½",
		strings.Repeat("a", MaxTitleRunes), strings.Repeat("a", MaxTitleRunes+1),
	} {
		if !Offerable(entry) {
			continue
		}

		// The title a compliant model would return: the entry itself.
		if !UsesEntry(entry, entry) {
			t.Errorf("Offerable(%q) is true, but the entry used verbatim does not count as used",
				entry)
		}
	}
}

// The guard refuses a title that claims an entry, and lets a themed one
// through. The incident these cases come from: a ride on the "Pink Panther"
// gear was offered nothing, because every entry was reserved, and was named
// "Son of the Pink Panther" anyway.
func TestGuardedClaimed(t *testing.T) {
	t.Parallel()

	// The athlete's production series as it stood: six films, all six
	// reserved, nothing offerable.
	panther := Franchise{
		Name:     "pink-panther",
		GearName: "Pink Panther",
		Titles: []string{
			"The Pink Panther",
			"A Shot in the Dark",
			"Inspector Clouseau",
			"The Return of the Pink Panther",
			"Trail of the Pink Panther",
			"Son of the Pink Panther",
		},
		Reserved: []string{
			"The Pink Panther",
			"A Shot in the Dark",
			"Inspector Clouseau",
			"The Return of the Pink Panther",
			"Trail of the Pink Panther",
			"Son of the Pink Panther",
		},
	}

	for _, tt := range []struct {
		name    string
		title   string
		offered string
		want    string
	}{
		{
			name:  "the entry that was written",
			title: "Son of the Pink Panther",
			want:  "Son of the Pink Panther",
		},
		{
			// An entry extended into a sentence is the entry, and equality
			// would let it through while it spends the film just as surely.
			name:  "an adaptation of an entry",
			title: "Son of the Pink Panther nach Musterdorf",
			want:  "Son of the Pink Panther",
		},
		{
			name:  "casing and punctuation do not smuggle one through",
			title: "son of the pink panther!",
			want:  "Son of the Pink Panther",
		},
		{
			// The motif rule invites exactly this, and it names no film.
			name:  "themed but distinct",
			title: "The Pink Panther in the Wind",
			want:  "",
		},
		{
			name:  "themed in German",
			title: "Sonstwas für den Pink Panther",
			want:  "",
		},
		{
			// Nothing distinguishes the bare bike name from the 1963 film.
			name:  "the bike name alone reads as the film",
			title: "The Pink Panther",
			want:  "The Pink Panther",
		},
		{
			name:  "an ordinary title",
			title: "Gegenwind bis Musterstadt",
			want:  "",
		},
		{
			// What the prompt just asked for cannot be refused.
			name:    "the offered entry is not guarded",
			title:   "Inspector Clouseau im Nebel",
			offered: "Inspector Clouseau",
			want:    "",
		},
		{
			// Offering one entry does not release the others.
			name:    "another entry is still guarded while one is offered",
			title:   "Trail of the Pink Panther",
			offered: "Inspector Clouseau",
			want:    "Trail of the Pink Panther",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry, claimed := panther.Guard(tt.offered).Claimed(tt.title)

			if tt.want == "" {
				if claimed {
					t.Fatalf("Claimed(%q) = %q, want no claim", tt.title, entry)
				}

				return
			}

			if !claimed {
				t.Fatalf("Claimed(%q) = no claim, want %q", tt.title, tt.want)
			}

			if entry != tt.want {
				t.Errorf("Claimed(%q) = %q, want %q", tt.title, entry, tt.want)
			}
		})
	}
}

// A spent entry is guarded, and so is one the rotation has not reached. Both
// are the incident's shape: a title claiming either spends a film with the
// position left where it was, so the entry is handed out again later.
func TestGuardedCoversSpentAndFutureEntries(t *testing.T) {
	t.Parallel()

	// No gear name, so no entry is the motif and every one is matched by
	// containment.
	series := Franchise{
		Name:   "musterserie",
		Titles: []string{"Musterfilm Eins", "Musterfilm Zwei", "Musterfilm Drei"},
	}

	// The rotation is at the second entry: the first is spent, the third has
	// not been reached.
	offered, _, ok := series.Next(1)
	if !ok || offered != "Musterfilm Zwei" {
		t.Fatalf("Next(1) = %q, %v", offered, ok)
	}

	guard := series.Guard(offered)

	for _, tt := range []struct{ name, title, want string }{
		{"spent", "Musterfilm Eins am Abend", "Musterfilm Eins"},
		{"future", "Musterfilm Drei im Regen", "Musterfilm Drei"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry, claimed := guard.Claimed(tt.title)
			if !claimed || entry != tt.want {
				t.Errorf("Claimed(%q) = %q, %v, want %q, true", tt.title, entry, claimed, tt.want)
			}
		})
	}
}

// A series whose entries nest still offers its own entry.
//
// The trap this closes: "Die Hard 2" is offered, the model carries it into a
// title, and that title contains "Die Hard" — so a guard holding the shorter
// entry refuses the title the prompt asked for. The guard applies on the first
// call, so the refusal fails the activity, the sweep requeues it, and the next
// sweep asks the same question: one paid call every five minutes with nothing
// in the log but "claims". The shipped profile escapes this only because its
// one nesting entry happens to be the motif.
func TestAnOfferReleasesTheEntriesItCannotBeToldApartFrom(t *testing.T) {
	t.Parallel()

	// No gear name, so nothing here is the motif and every entry is matched
	// by containment.
	series := Franchise{
		Name:   "musterreihe",
		Titles: []string{"Musterhart", "Musterhart 2", "Musterhart mit Anlauf"},
	}

	guard := series.Guard("Musterhart 2")

	if entry, claimed := guard.Claimed("Musterhart 2 nach Musterdorf"); claimed {
		t.Errorf("the offered entry's own title was refused as %q", entry)
	}

	// Releasing the shorter entry does not release the rest of the series.
	if _, claimed := guard.Claimed("Musterhart mit Anlauf nach Musterdorf"); !claimed {
		t.Error("a sibling entry was accepted while another was offered")
	}

	// And offering the shorter one still guards the longer: a title claiming
	// it would spend an entry the rotation has yet to reach.
	if _, claimed := series.Guard("Musterhart").Claimed("Musterhart 2 im Regen"); !claimed {
		t.Error("a future entry was accepted because a prefix of it was offered")
	}
}

// Offering a later film does not release the motif entry.
//
// The widening above is by containment, and every Panther title contains the
// bike's name. The motif entry is matched by equality, so it stays guarded:
// offering "Son of the Pink Panther" must not make "The Pink Panther" legal.
func TestOfferingAnEntryDoesNotReleaseTheMotifEntry(t *testing.T) {
	t.Parallel()

	series := Franchise{
		Name:     "muster-panther",
		GearName: "Musterpanther",
		Titles:   []string{"The Musterpanther", "Son of the Musterpanther"},
	}

	guard := series.Guard("Son of the Musterpanther")

	if _, claimed := guard.Claimed("The Musterpanther"); !claimed {
		t.Error("the motif entry was released by offering a later one")
	}

	if entry, claimed := guard.Claimed("Son of the Musterpanther nach Musterdorf"); claimed {
		t.Errorf("the offered entry's own title was refused as %q", entry)
	}
}

// A gear name carrying an article still exempts its own entry.
//
// The motif exception compares cores on both sides. Comparing a normalized
// gear name against an entry core would miss here, and every themed title on a
// bike whose name begins with an article would be refused by containment.
func TestTheMotifExceptionSurvivesAnArticleInTheGearName(t *testing.T) {
	t.Parallel()

	series := Franchise{
		Name:     "muster",
		GearName: "The Musterpanther",
		Titles:   []string{"The Musterpanther", "Son of the Musterpanther"},
		Reserved: []string{"The Musterpanther", "Son of the Musterpanther"},
	}

	guard := series.Guard("")

	if entry, claimed := guard.Claimed("The Musterpanther im Nebel"); claimed {
		t.Errorf("a themed title was refused as %q", entry)
	}

	if _, claimed := guard.Claimed("The Musterpanther"); !claimed {
		t.Error("the entry itself was accepted")
	}

	if _, claimed := guard.Claimed("Son of the Musterpanther im Nebel"); !claimed {
		t.Error("an adaptation of a non-motif entry was accepted")
	}
}

// A ride that matched no franchise has an empty guard, which refuses nothing.
func TestZeroGuardRefusesNothing(t *testing.T) {
	t.Parallel()

	if entry, claimed := (Franchise{}).Guard("").Claimed("Son of the Pink Panther"); claimed {
		t.Errorf("the zero franchise claimed %q", entry)
	}
}
