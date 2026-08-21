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
}

// Franchises remembers how far along an ordered title series an athlete is.
//
// A franchise is a list of titles walked in order — the Pink Panther films on
// the gravel bike — and the only thing that has to persist is the position in
// it. The list itself is configuration, so this stores an integer and not the
// titles: renaming or reordering a franchise in config must not require a
// migration of anything stored here.
//
// Like the queue and the named log, it is re-derivable in principle — the
// titles this service wrote are the record — but re-deriving it means matching
// past titles against a series, so it is cheaper to remember. Losing it costs
// a repeated or skipped entry, not a wrong write.
type Franchises interface {
	// FranchisePosition returns how many entries of the named franchise this
	// athlete has already used. Zero for a franchise never used, which is
	// also the answer for one that does not exist — a franchise removed from
	// configuration should not error, it should simply stop being consulted.
	FranchisePosition(ctx context.Context, athleteID int64, franchise string) (int, error)

	// AdvanceFranchise records that one more entry has been used and returns
	// the new position.
	//
	// It advances by one rather than setting a value, so two callers cannot
	// race to the same position: the store decides the next number, not the
	// caller. That matters less at max-instances=1 than it will later, and
	// costs nothing now.
	AdvanceFranchise(ctx context.Context, athleteID int64, franchise string) (int, error)
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

	// Source says how the title was produced: [SourceLLM] for one a model
	// wrote, [SourceTemplate] for a commute or errand name that came from
	// configuration.
	//
	// Recorded because the two are not interchangeable to a reader of the
	// history. This athlete commutes, so a working week fills the newest
	// entries with two repeated strings — which would crowd the real titles
	// out of "never repeat these" and teach a model that a Saturday gravel
	// ride should be called "Zur Arbeit".
	Source string
}

// How a recorded title was produced.
const (
	SourceLLM      = "llm"
	SourceTemplate = "template"
)

// NamedTitle is one entry of the title history.
type NamedTitle struct {
	ActivityID int64
	Title      string
	Language   string
	Source     string
	NamedAt    time.Time
}
