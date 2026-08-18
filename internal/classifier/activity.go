// Package classifier decides, for a single Strava activity, which naming tier
// it belongs to and what this service is allowed to do with it.
//
// The package is deliberately free of HTTP, Firestore and Strava-SDK imports:
// callers map their transport representation onto [Activity], pass a per-athlete
// [Config], and act on the returned [Decision]. Nothing here performs I/O, and
// nothing here produces a finished title — turning a [Decision] into a string is
// the naming layer's job.
package classifier

// Point is a WGS84 coordinate pair in degrees.
type Point struct {
	Lat float64
	Lon float64
}

// Activity is the transport-neutral view of a Strava activity that the tier
// rules need. Field names follow the Strava API's meaning, not its JSON
// spelling; mapping is the caller's responsibility.
type Activity struct {
	// Name is the activity's current title. The skip gate compares it against
	// the Strava default-title table (see defaults.go).
	Name string

	// Description is the activity's description. Whoop-sourced activities are
	// recognised by the word "Strain" appearing here.
	Description string

	// SportType is Strava's sport_type value ("Ride", "GravelRide",
	// "VirtualRide", "WeightTraining", ...), not the legacy type field.
	SportType string

	// Trainer reports Strava's trainer flag (is_trainer). Together with
	// SportType == "VirtualRide" it identifies indoor rides.
	Trainer bool

	// Commute reports Strava's commute flag (is_commute), which ActivityFix
	// sets for short rides near home.
	Commute bool

	// DistanceMeters is the recorded distance in metres.
	DistanceMeters float64

	// MovingTimeSeconds is Strava's moving_time in seconds.
	MovingTimeSeconds int

	// Start and End are the activity's start and end coordinates, or nil when
	// Strava reported none (indoor activities, or a stripped/privacy-zoned
	// activity). They are only consulted for the commute safety net.
	Start *Point
	End   *Point
}
