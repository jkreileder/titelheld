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
}

// NamedLog records what this service has written.
//
// It serves two purposes: an activity is named at most once ever, and the
// webhook update event caused by our own rename is recognized and dropped
// rather than treated as a human retitling the activity.
type NamedLog interface {
	// MarkNamed records that an activity was given a title by this service.
	MarkNamed(ctx context.Context, athleteID, activityID int64, title string) error

	// Named reports whether the activity has already been named, and with what
	// title.
	Named(ctx context.Context, athleteID, activityID int64) (string, bool, error)
}
