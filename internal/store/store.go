// Package store holds the service's persistent state and the interfaces over
// it.
//
// Everything here is keyed by athlete ID so a second athlete needs no schema
// change. The only state that genuinely must survive a restart is the OAuth
// token pair; the rest is a cache or a work queue and is re-derivable from the
// Strava API, so it is deliberately kept small.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jkreileder/titelheld/internal/strava"
)

// ErrNotFound is returned when a lookup has no result.
var ErrNotFound = errors.New("store: not found")

// Pending is an activity waiting out the processing delay.
type Pending struct {
	AthleteID  int64
	ActivityID int64

	// Aspect is the webhook aspect_type that queued it ("create" or "update").
	Aspect string

	// EnqueuedAt is when the event arrived, ProcessAfter when the activity
	// becomes eligible for naming.
	EnqueuedAt   time.Time
	ProcessAfter time.Time
}

// Due reports whether the delay has elapsed.
func (p Pending) Due(now time.Time) bool {
	return !now.Before(p.ProcessAfter)
}

// Queue holds activities waiting out the processing delay.
//
// The delay is served by a Cloud Scheduler sweep rather than Cloud Tasks: the
// due query below is the whole mechanism, it needs no second GCP service and no
// client library, and a failed activity simply stays queued until the next
// sweep instead of needing a separate retry policy. Ten-minute precision makes
// the scheduler's coarse granularity irrelevant.
type Queue interface {
	// Enqueue records an activity as pending. It reports whether the activity
	// was added; false means an entry already existed, which is how a repeated
	// webhook delivery collapses into one unit of work.
	Enqueue(ctx context.Context, pending Pending) (bool, error)

	// Due returns every pending activity whose delay has elapsed, oldest
	// first.
	Due(ctx context.Context, now time.Time) ([]Pending, error)

	// Remove drops a pending entry, whether it was processed or abandoned.
	Remove(ctx context.Context, athleteID, activityID int64) error

	// Len reports how many entries are queued. For logging and tests.
	Len(ctx context.Context) (int, error)
}

// Place is a verified place name from reverse geocoding.
//
// It holds names and nothing else. That is the point: the naming layer receives
// values of this type, so it cannot be handed raw coordinates even by accident,
// and the fields are limited to administrative and natural features so a title
// can never reveal a point of interest the athlete visited.
type Place struct {
	// Name is the most specific settlement or natural feature, e.g. a village
	// or a river.
	Name string

	// Kind describes what Name is ("city", "village", "river", ...).
	Kind string

	// Region and Country are the coarser containers.
	Region  string
	Country string
}

// Empty reports whether nothing usable was resolved.
func (p Place) Empty() bool {
	return p.Name == "" && p.Region == "" && p.Country == ""
}

// GeocodeCache stores reverse-geocoding results.
//
// Nominatim's usage policy requires results to be cached rather than re-fetched,
// and the key is a rounded coordinate, so nearby points on the same route share
// an entry. The cache holds only [Place] values — never the coordinates that
// produced them.
type GeocodeCache interface {
	// Place returns the cached place for a rounded-coordinate key.
	Place(ctx context.Context, key string) (Place, bool, error)

	// SavePlace records a place against a rounded-coordinate key.
	SavePlace(ctx context.Context, key string, place Place) error
}

// Store is everything the service persists. Both the in-memory and the
// Firestore implementations satisfy it, and the conformance suite in
// storetest exercises them through it.
type Store interface {
	strava.TokenStore
	Queue
	NamedLog
	GeocodeCache
	Franchises
	AthleteConfigs
}

// AthleteConfigs holds the per-athlete configuration document.
//
// The one collection besides the tokens that is not re-derivable from Strava:
// it is written by a person, not computed from anything, so losing it means
// typing it again rather than replaying an API.
//
// It exists so that configuration is data. A franchise added here needs no
// release, which is the whole distinction the spec draws between a series of
// titles and the code that walks it.
type AthleteConfigs interface {
	// AthleteConfig returns the athlete's configuration, and whether one has
	// been written. An athlete with no document is not an error: the caller
	// uses its own defaults, which is what every deployment starts with.
	AthleteConfig(ctx context.Context, athleteID int64) (AthleteConfig, bool, error)

	// SaveAthleteConfig replaces the document.
	SaveAthleteConfig(ctx context.Context, athleteID int64, config AthleteConfig) error
}

// AthleteConfig is the per-athlete configuration document.
//
// Only franchises for now. Tiers, geofences, banned words and language
// preferences belong here too and still live in code; this is the collection
// they move into, one at a time.
type AthleteConfig struct {
	// Franchises are ordered title series, in precedence order.
	Franchises []Franchise
}

// Franchise is one ordered series as it is stored.
//
// Deliberately a persistence type rather than the naming package's, so the
// stored schema is owned here and changing the naming layer's shape cannot
// silently change what is on disk. The conversion is a few lines and lives
// with the caller that needs it.
type Franchise struct {
	// Name keys the position and is never shown to a model. Renaming it
	// starts the athlete at the first entry again.
	Name string

	// SportTypes the series applies to. Empty means any.
	SportTypes []string

	// GearName the series rides on, matched case-insensitively and in full.
	// Empty means any bike.
	GearName string

	// Titles are the entries, in order.
	Titles []string

	// Reserved are entries the rotation never offers, matched against Titles
	// case-insensitively and trimmed. They keep their place in the series;
	// the athlete spends them by hand.
	Reserved []string
}

// Franchises remembers how far along an ordered title series an athlete is.
//
// A franchise is a list of titles walked in order — the Pink Panther films on
// the gravel bike — and the only thing that has to persist is the position in
// it. The list itself is configuration, so this stores an integer and not the
// titles: editing a franchise in config must not require a migration of
// anything stored here. It is an index into that list, so an edit that moves
// entries around moves what it points at — a configuration question, not a
// storage one.
//
// Like the queue and the named log, it is re-derivable in principle — the
// titles this service wrote are the record — but re-deriving it means matching
// past titles against a series, so it is cheaper to remember. Losing it costs
// a repeated or skipped entry, not a wrong write.
type Franchises interface {
	// FranchisePosition returns the index the named franchise's rotation
	// resumes at. Not a count of what has been used: an entry the athlete
	// reserved and spent by hand never moves it, which is what reserving one
	// means.
	//
	// Zero for a franchise never used, which is also the answer for one that
	// does not exist — a franchise removed from configuration should not
	// error, it should simply stop being consulted.
	FranchisePosition(ctx context.Context, athleteID int64, franchise string) (int, error)

	// AdvanceFranchisePast records that the entry at an index has been used
	// and returns the new position.
	//
	// The index rather than a step, because the rotation steps over reserved
	// entries: the caller knows which entry it offered, and advancing by one
	// from a position that skipped two would offer a reserved title next.
	//
	// The move is monotonic — the stored position becomes the greater of what
	// it was and index+1 — and it happens inside the store rather than in a
	// caller that read, decided and wrote back. Two callers naming two rides
	// at once therefore cannot rewind each other, and a replay of an older
	// naming cannot hand out a title the series has already spent.
	//
	// A negative index is an error rather than a silent no-op: it can only
	// come from a caller that did not have an entry to record.
	AdvanceFranchisePast(
		ctx context.Context, athleteID int64, franchise string, index int,
	) (int, error)
}

// NamedLog records what this service has written.
//
// It serves two purposes: an activity is named at most once ever, and the
// webhook update event caused by our own rename is recognized and dropped
// rather than treated as a human retitling the activity.
type NamedLog interface {
	// MarkNamed records that an activity was given a title by this service.
	MarkNamed(ctx context.Context, naming Naming) error

	// Named reports whether the activity has already been named, and with what
	// title.
	Named(ctx context.Context, athleteID, activityID int64) (string, bool, error)

	// RecentTitles returns the titles most recently written for an athlete,
	// newest first, at most limit of them.
	//
	// The prompt carries these so the model does not repeat itself and can
	// refer back. A limit of zero or less returns nothing rather than
	// everything, because an unbounded read of this collection grows with the
	// athlete's riding and the only caller wants the last handful.
	RecentTitles(ctx context.Context, athleteID int64, limit int) ([]NamedTitle, error)
}

// Naming is one title this service wrote.
//
// A struct rather than four positional arguments, two of which are int64s
// meaning different things and two of which are strings meaning different
// things. Swapping either pair compiles.
type Naming struct {
	AthleteID  int64
	ActivityID int64

	// Title is what was written.
	Title string

	// Language is the language it was written in, as the naming layer reported
	// it. Stored because it cannot be recovered afterwards: re-reading the
	// activity gives the title back but never says which language was chosen,
	// and few-shot examples derived from history need it.
	Language string

	// At is when it was written. Supplied by the caller rather than stamped by
	// the store, as [Pending] does, so both implementations order the same way
	// and a test can say what "newest" means.
	At time.Time

	// Source says how the title was produced: [SourceService] for one this
	// service's naming pipeline wrote, [SourceTemplate] for a commute or errand
	// name that came from configuration, [SourceImported] for one seeded from
	// the athlete's Strava past, [SourceHuman] for one the athlete wrote on a
	// ride this service would otherwise have named.
	//
	// Recorded because the sources are not interchangeable to a reader of the
	// history. This athlete commutes, so a working week fills the newest
	// entries with two repeated strings — which would crowd the real titles
	// out of "never repeat these" and teach a model that a Saturday gravel
	// ride should be called "Zur Arbeit".
	Source string
}

// How a recorded title was produced.
const (
	// SourceService is a title this service's naming pipeline produced — the
	// only source a few-shot example may come from. A template is written by
	// this service too and is deliberately not this: it is a fixed string
	// chosen from a list, and teaching a model to reproduce it would teach it
	// to call a Saturday gravel ride "Zur Arbeit".
	SourceService  = "service"
	SourceTemplate = "template"

	// SourceImported is a title the athlete already had, seeded from their
	// Strava history rather than written here.
	//
	// It feeds the no-repeat list and nothing else. Imported rows never teach
	// style: a decade of a person's own shorthand is bare town names, private
	// jokes and whatever a tool left behind, and an example set built from it
	// teaches a model to answer with the name of a town. That the athlete wrote
	// it makes it theirs; it does not make it what this service should imitate.
	SourceImported = "imported"

	// SourceHuman is a title the athlete wrote on a sport ride, recorded when
	// the skip gate declined to name the ride. Not a title this service wrote
	// and not one it may ever write: the row is the dedup record, so the ride
	// is final, and it was recorded from the skip path, which cannot reach
	// Strava.
	//
	// It feeds the no-repeat list, because a title live on the athlete's feed
	// is exactly what must not be invented again, and it teaches style: the
	// athlete's current hand-namings are the best style data there will ever
	// be. An imported row is barred from the examples, this one is not, and
	// the difference is what the two sources are for — a decade of shorthand
	// says what not to repeat, a title written last week says what a title
	// should sound like.
	SourceHuman = "human"
)

// NamedTitle is one entry of the title history.
type NamedTitle struct {
	ActivityID int64
	Title      string
	Language   string
	Source     string
	NamedAt    time.Time
}
