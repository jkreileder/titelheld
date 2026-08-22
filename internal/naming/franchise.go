package naming

import (
	"strings"
	"unicode"
)

// Franchise is an ordered list of titles walked one entry at a time.
//
// Data, not code. The store remembers only how far along an athlete is, so
// editing a series here migrates nothing — and a franchise removed from this
// list simply stops applying. What is remembered is an index into this list
// though, so appending is free while reordering, inserting or deleting moves
// what the stored position points at, and renaming the franchise starts the
// series again. The README says which is which.
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

	// Reserved are entries the rotation never offers. They keep their place
	// in Titles — reserving is not deleting — and the athlete spends them by
	// hand. Matched against Titles case-insensitively and trimmed, because
	// both lists are typed into the same document by the same person.
	//
	// An entry named here that is not in Titles is simply inert, and one that
	// is reserved after the position has already passed it changes nothing:
	// the position only ever moves forward.
	Reserved []string
}

// Next returns the next offerable entry at or after a position, the index it
// sits at, and whether there is one.
//
// Reserved entries are stepped over rather than skipped past: the index comes
// back with the title so the caller advances to just after the entry it
// actually offered, and a reserved entry the rotation walked over is
// therefore never handed out later either.
//
// A position past the end is not an error and not a wrap-around: the series
// is finished, the franchise stops applying, and the ride is named normally.
func (f Franchise) Next(position int) (title string, index int, ok bool) {
	if position < 0 {
		return "", 0, false
	}

	for at := position; at < len(f.Titles); at++ {
		if f.reserved(f.Titles[at]) {
			continue
		}

		return f.Titles[at], at, true
	}

	return "", 0, false
}

// reserved reports whether an entry is the athlete's to spend by hand.
func (f Franchise) reserved(title string) bool {
	for _, entry := range f.Reserved {
		if strings.EqualFold(strings.TrimSpace(entry), strings.TrimSpace(title)) {
			return true
		}
	}

	return false
}

// Applies reports whether a ride belongs to this franchise.
func (f Franchise) Applies(sportType, gearName string) bool {
	// Both sides trimmed: the configured name is typed into a document, and a
	// trailing space there would make the series match nothing with no log
	// line to say why.
	configured := strings.TrimSpace(f.GearName)
	if configured != "" && !strings.EqualFold(strings.TrimSpace(gearName), configured) {
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
// The four entries already used by hand — "The Pink Panther Checks Inn", "The
// Pink Panther Strikes Again", "Revenge of the Pink Panther" and "Curse of the
// Pink Panther" — are deliberately absent. A stored position starts at zero, so
// listing them would hand out a title the athlete already has. A canon added
// later that has never been used needs no such care; one that has, needs its
// position seeded, and the README says how.
//
// Everything the athlete's own rotation has already passed is listed and
// reserved rather than dropped. The series is walked in release order and the
// athlete is at "Curse of the Pink Panther", so the four films before it and
// "Trail of the Pink Panther", which was deliberately stepped over, are the
// athlete's to spend by hand. That leaves "Son of the Pink Panther" as the one
// entry this service may offer, and it is offered at position zero without any
// position needing to be seeded.
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
				"Son of the Pink Panther",
			},
			Reserved: []string{
				"The Pink Panther",
				"A Shot in the Dark",
				"Inspector Clouseau",
				"The Return of the Pink Panther",
				"Trail of the Pink Panther",
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

// Offerable reports whether an entry can be used as a title at all.
//
// An entry longer than a title may not be offered. The prompt bounds every
// untrusted value it prints, so an over-long entry would reach the model as a
// prefix of itself — which no title can contain, so the containment check
// would decline it, the re-offer would decline it again, and every later ride
// would be offered the same unusable entry. Better to notice it: a franchise
// is typed into a document, and an entry that does not fit is a configuration
// error rather than something to work around.
//
// Nothing else is checked. An entry that fits but is awkward is the athlete's
// business.
func Offerable(entry string) bool {
	trimmed := strings.TrimSpace(entry)

	return trimmed != "" && len([]rune(trimmed)) <= MaxTitleRunes
}

// UsesEntry reports whether a title demonstrably uses a franchise entry.
//
// This is what decides whether an entry has been spent. The prompt asks the
// model to carry the entry's wording into the title and invites it to add to
// it; a request is not a guarantee, so the position moves on evidence rather
// than on having made the offer. Without this a title that ignored the series
// still consumed a film, and the athlete would find one missing with nothing
// to say where it went.
//
// The evidence is containment of the entry's core after normalization: both
// sides lowercased, punctuation flattened to spaces and whitespace collapsed,
// the core being the entry without a leading article, and the match made on
// token boundaries so "Pink Panther" does not match inside "Pink Panthers".
// "Son of the Pink Panther im Nebel" counts; "Der Panther im Nebel" does not.
//
// It fails closed. A translated or paraphrased entry is not recognized, the
// title stands and the entry stays unspent — the next ride is offered it
// again. The opposite error, treating a gesture at the series as a use, spends
// a film on a title that never carried it, and unlike a repeated offer that
// cannot be noticed afterwards.
func UsesEntry(title, entry string) bool {
	core := entryCore(entry)
	if core == "" {
		return false
	}

	normalized := normalizeForMatch(title)
	if normalized == "" {
		return false
	}

	// Padded on both sides so containment lands on whole tokens: without it
	// "a shot in the dark" would match inside "a shot in the darkness".
	return strings.Contains(" "+normalized+" ", " "+core+" ")
}

// entryCore is the part of an entry a title has to carry.
//
// The leading article is dropped because adapting an entry into a sentence
// usually loses it — "Pink Panther im Nebel" is the entry used, and "The" is
// the first thing a German title would shed. Nothing else is dropped: what is
// left has to appear, or the series is only being gestured at.
func entryCore(entry string) string {
	normalized := normalizeForMatch(entry)

	// English only, and deliberately so: a German article is also an ordinary
	// word — dropping the "die" from "Die Hard" would leave a one-word core
	// that matches almost anything.
	for _, article := range []string{"the ", "a ", "an "} {
		if rest, found := strings.CutPrefix(normalized, article); found {
			return rest
		}
	}

	return normalized
}

// normalizeForMatch reduces a title to lowercase words separated by single
// spaces.
//
// Punctuation goes because a model asked to adapt an entry will punctuate it —
// "Son of the Pink Panther: Gegenwind" is the entry used, and a comparison
// that fails on the colon would spend nothing. Case goes for the same reason.
func normalizeForMatch(text string) string {
	folded := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}

		return ' '
	}, text)

	return strings.Join(strings.Fields(folded), " ")
}
