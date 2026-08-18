package classifier

import (
	"math"
	"strings"
)

// Tier is the taxonomy bucket an activity falls into. Tiers are evaluated in
// ascending order and the first match wins.
type Tier int

const (
	// TierNone means no tier rule matched — a Run, a Swim, or a ride below the
	// sport-ride thresholds. Nothing is written.
	TierNone Tier = 0

	// TierSkip covers activities this service never names: strength and
	// non-GPS work, walks, hikes, and anything pushed by Whoop.
	TierSkip Tier = 1

	// TierVirtual covers indoor and virtual rides, whose coordinates are
	// fictional and must never be geocoded.
	TierVirtual Tier = 2

	// TierCommute is the work commute safety net, reached only when
	// ActivityFix did not title the ride itself.
	TierCommute Tier = 3

	// TierErrand covers commute-tagged errands that still carry a Strava
	// default title.
	TierErrand Tier = 4

	// TierSportRide is the full LLM naming pipeline: the tier this service
	// exists for.
	TierSportRide Tier = 5
)

// String renders the tier for structured logs.
func (t Tier) String() string {
	switch t {
	case TierNone:
		return "none"
	case TierSkip:
		return "skip"
	case TierVirtual:
		return "virtual"
	case TierCommute:
		return "commute"
	case TierErrand:
		return "errand"
	case TierSportRide:
		return "sport_ride"
	default:
		return "unknown"
	}
}

// Action is what the caller should do with the activity. It is decided
// separately from [Tier]: an activity is classified first, and only then does
// the default-title gate (plus per-tier config) decide whether anything may be
// written. That separation is what lets a commute that another tool already
// titled still be reported as [TierCommute] while its action stays
// [ActionSkip].
type Action int

const (
	// ActionSkip writes nothing.
	ActionSkip Action = iota

	// ActionCommuteTemplate asks the naming layer for the deterministic
	// commute title matching Decision.Direction. No LLM call.
	ActionCommuteTemplate

	// ActionErrandTemplate asks the naming layer for a deterministic errand
	// title from the configured template pool and POI whitelist. No LLM call.
	ActionErrandTemplate

	// ActionLLM runs the full naming pipeline: geography, memory, franchise
	// state, LLM call.
	ActionLLM

	// ActionLLMIndoor runs the LLM on power, duration and time of year only.
	// The caller must not geocode: the coordinates of a virtual ride are
	// fictional.
	ActionLLMIndoor
)

// String renders the action for structured logs.
func (a Action) String() string {
	switch a {
	case ActionSkip:
		return "skip"
	case ActionCommuteTemplate:
		return "commute_template"
	case ActionErrandTemplate:
		return "errand_template"
	case ActionLLM:
		return "llm"
	case ActionLLMIndoor:
		return "llm_indoor"
	default:
		return "unknown"
	}
}

// Direction is the travel direction of a commute.
type Direction int

const (
	// DirectionNone is the zero value and is set for every tier but
	// [TierCommute].
	DirectionNone Direction = iota

	// DirectionToWork is a ride ending at the work geofence.
	DirectionToWork

	// DirectionToHome is a ride from work back home.
	DirectionToHome
)

// String renders the direction for structured logs.
func (d Direction) String() string {
	switch d {
	case DirectionNone:
		return "none"
	case DirectionToWork:
		return "to_work"
	case DirectionToHome:
		return "to_home"
	default:
		return "unknown"
	}
}

// ZwiftMode selects how virtual rides are handled.
type ZwiftMode string

const (
	// ZwiftKeep leaves the existing title alone. Zwift titles already encode
	// the workout, so this is the shipped default.
	ZwiftKeep ZwiftMode = "keep"

	// ZwiftLLMIndoor names virtual rides from power, duration and time of year.
	ZwiftLLMIndoor ZwiftMode = "llm_indoor"
)

// Geofence is a circular area. A zero Geofence matches nothing.
type Geofence struct {
	Center       Point
	RadiusMeters float64
}

func (g Geofence) contains(p Point) bool {
	if g.RadiusMeters <= 0 {
		return false
	}

	return distanceMeters(g.Center, p) <= g.RadiusMeters
}

// Config is the per-athlete classifier configuration. It is the classifier's
// slice of the athlete's config document, and every field is safe at its zero
// value: an unset threshold falls back to the shipped default, an unset
// geofence matches nothing, and an unset ZwiftMode means [ZwiftKeep].
type Config struct {
	// ZwiftMode selects the virtual-ride policy. Empty means [ZwiftKeep].
	ZwiftMode ZwiftMode

	// Home and Work drive the commute safety net. Leave them zero to disable
	// geofence-based commute detection entirely.
	Home Geofence
	Work Geofence

	// ToWorkTitle and ToHomeTitle are the titles ActivityFix writes for
	// commutes. They are matched to recognise an already-titled commute as
	// [TierCommute] — such an activity is still skipped by the default-title
	// gate, so this only affects which tier gets reported. Empty disables the
	// respective match.
	ToWorkTitle string
	ToHomeTitle string

	// LeaveErrandsUnnamed opts out of naming tier-4 errands, leaving them at
	// their Strava default. Zero value: errands are named.
	LeaveErrandsUnnamed bool

	// SportMinDistanceMeters and SportMinMovingTimeSeconds are the tier-5
	// thresholds; meeting either one is enough. Zero means the default.
	SportMinDistanceMeters    float64
	SportMinMovingTimeSeconds int
}

// Shipped defaults, applied field-wise wherever Config leaves a value unset.
const (
	defaultSportMinDistanceMeters    = 15000.0
	defaultSportMinMovingTimeSeconds = 45 * 60
	defaultToWorkTitle               = "Zur Arbeit"
	defaultToHomeTitle               = "Nach Hause"
)

// DefaultConfig returns the configuration this service ships with.
func DefaultConfig() Config {
	return Config{
		ZwiftMode:                 ZwiftKeep,
		ToWorkTitle:               defaultToWorkTitle,
		ToHomeTitle:               defaultToHomeTitle,
		SportMinDistanceMeters:    defaultSportMinDistanceMeters,
		SportMinMovingTimeSeconds: defaultSportMinMovingTimeSeconds,
	}
}

// withDefaults fills unset fields so a zero Config behaves like DefaultConfig
// rather than treating every ride as a sport ride. ToWorkTitle and ToHomeTitle
// are deliberately not filled in: empty means the match is disabled.
func (c Config) withDefaults() Config {
	if c.ZwiftMode == "" {
		c.ZwiftMode = ZwiftKeep
	}
	if c.SportMinDistanceMeters <= 0 {
		c.SportMinDistanceMeters = defaultSportMinDistanceMeters
	}
	if c.SportMinMovingTimeSeconds <= 0 {
		c.SportMinMovingTimeSeconds = defaultSportMinMovingTimeSeconds
	}

	return c
}

// Decision is the classifier's verdict.
type Decision struct {
	// Tier is the taxonomy bucket, assigned independently of whether anything
	// may be written.
	Tier Tier

	// Action is what the caller may do.
	Action Action

	// Direction is set for [TierCommute] only.
	Direction Direction

	// Reason is a short, stable explanation for logs.
	Reason string
}

// Strava sport_type values the tier rules reason about.
const (
	sportTypeRide           = "Ride"
	sportTypeGravelRide     = "GravelRide"
	sportTypeVirtualRide    = "VirtualRide"
	sportTypeWalk           = "Walk"
	sportTypeHike           = "Hike"
	sportTypeWorkout        = "Workout"
	sportTypeWeightTraining = "WeightTraining"
)

// Decision reasons, kept as constants so logs and tests share one spelling.
const (
	reasonNotDefaultTitle = "title is not a Strava default"
	reasonCommuteErrand   = "commute-tagged errand"
	reasonErrandsDisabled = "errand naming disabled"
	reasonSportRide       = "sport ride"
	reasonBelowThresholds = "ride below the sport-ride thresholds"
)

// neverNamedSportTypes are tier-1 sport types: never named, whatever else is
// true of the activity.
var neverNamedSportTypes = map[string]struct{}{
	sportTypeWeightTraining: {},
	sportTypeWorkout:        {},
	sportTypeWalk:           {},
	sportTypeHike:           {},
}

// rideSportTypes bound tiers 3 to 5. Everything the tier rules describe —
// commutes, errands, sport rides — is a ride; a Run tagged as a commute must
// not pick up an errand title, so it falls through to [TierNone] instead.
var rideSportTypes = map[string]struct{}{
	sportTypeRide:       {},
	sportTypeGravelRide: {},
}

// whoopMarker appears in the description of every Whoop-pushed activity.
const whoopMarker = "Strain"

// Classify assigns an activity's tier and decides what may be done with it.
//
// Tier rules are evaluated in order and the first match wins. The tier is
// assigned regardless of the activity's current title; the default-title gate
// is applied afterwards and downgrades the action to [ActionSkip] for anything
// a human or another tool has already named.
func Classify(activity Activity, cfg Config) Decision {
	cfg = cfg.withDefaults()

	// Tier 1 comes first and is unconditional: a strength session recorded on a
	// trainer must not be captured by the virtual-ride rule below.
	if _, ok := neverNamedSportTypes[activity.SportType]; ok {
		return Decision{
			Tier:   TierSkip,
			Action: ActionSkip,
			Reason: "sport type " + activity.SportType + " is never named",
		}
	}
	if strings.Contains(activity.Description, whoopMarker) {
		return Decision{
			Tier:   TierSkip,
			Action: ActionSkip,
			Reason: "Whoop activity (description mentions " + whoopMarker + ")",
		}
	}

	tier, direction := classifyTier(activity, cfg)

	// The skip gate. A title that is not a Strava default was written by a
	// human or by another tool (ActivityFix, Xert), and this service is the
	// last writer, not an overwriter.
	if !IsDefaultTitle(activity.Name) {
		return Decision{
			Tier:      tier,
			Action:    ActionSkip,
			Direction: direction,
			Reason:    reasonNotDefaultTitle,
		}
	}

	switch tier {
	case TierVirtual:
		if cfg.ZwiftMode == ZwiftLLMIndoor {
			return Decision{
				Tier:   TierVirtual,
				Action: ActionLLMIndoor,
				Reason: "virtual ride, zwift_mode=" + string(ZwiftLLMIndoor),
			}
		}

		return Decision{
			Tier:   TierVirtual,
			Action: ActionSkip,
			Reason: "virtual ride, zwift_mode=" + string(ZwiftKeep),
		}

	case TierCommute:
		return Decision{
			Tier:      TierCommute,
			Action:    ActionCommuteTemplate,
			Direction: direction,
			Reason:    "commute safety net (" + direction.String() + ")",
		}

	case TierErrand:
		if cfg.LeaveErrandsUnnamed {
			return Decision{
				Tier:   TierErrand,
				Action: ActionSkip,
				Reason: reasonErrandsDisabled,
			}
		}

		return Decision{
			Tier:   TierErrand,
			Action: ActionErrandTemplate,
			Reason: reasonCommuteErrand,
		}

	case TierSportRide:
		return Decision{
			Tier:   TierSportRide,
			Action: ActionLLM,
			Reason: reasonSportRide,
		}

	case TierNone, TierSkip:
		// Nothing more to decide: fall through to the skip below. TierSkip is
		// returned earlier by Classify and cannot arrive here, but naming it
		// keeps this switch exhaustive over Tier.
	}

	return Decision{
		Tier:   tier,
		Action: ActionSkip,
		Reason: noTierReason(activity),
	}
}

// classifyTier walks tiers 2 to 5. Tier 1 is handled by [Classify] before this
// is reached.
func classifyTier(activity Activity, cfg Config) (Tier, Direction) {
	// A virtual ride is virtual whatever else is true of it.
	if activity.SportType == sportTypeVirtualRide {
		return TierVirtual, DirectionNone
	}

	// Every tier below describes a ride, so anything else stops here. Without
	// this the trainer flag alone would claim a treadmill run for the virtual
	// tier and, under ZwiftLLMIndoor, put an indoor-cycling title on it.
	if _, ok := rideSportTypes[activity.SportType]; !ok {
		return TierNone, DirectionNone
	}

	// A ride recorded on a trainer: fictional coordinates, never geocoded.
	if activity.Trainer {
		return TierVirtual, DirectionNone
	}

	if direction, ok := commuteDirection(activity, cfg); ok {
		return TierCommute, direction
	}

	if activity.Commute {
		return TierErrand, DirectionNone
	}

	if meetsSportThresholds(activity, cfg) {
		return TierSportRide, DirectionNone
	}

	return TierNone, DirectionNone
}

// meetsSportThresholds reports whether a ride is big enough for tier 5.
// Meeting either threshold is enough.
func meetsSportThresholds(activity Activity, cfg Config) bool {
	return activity.DistanceMeters >= cfg.SportMinDistanceMeters ||
		activity.MovingTimeSeconds >= cfg.SportMinMovingTimeSeconds
}

// commuteDirection recognises a work commute.
//
// The primary signal is the title ActivityFix writes; that path exists so an
// already-handled commute is still reported as such. The geofence fallback is
// the actual safety net: when ActivityFix failed there is no title, no commute
// tag and no gear to key on, and the start/end positions are all that is left.
func commuteDirection(activity Activity, cfg Config) (Direction, bool) {
	// A title ActivityFix wrote is direct evidence, and is taken at face value
	// whatever the ride's size. An empty configured title disables its match,
	// which would otherwise swallow every untitled activity.
	title := strings.TrimSpace(activity.Name)

	switch {
	case cfg.ToWorkTitle != "" && title == cfg.ToWorkTitle:
		return DirectionToWork, true
	case cfg.ToHomeTitle != "" && title == cfg.ToHomeTitle:
		return DirectionToHome, true
	}

	// The geofence path only infers a commute, so it is bounded by the tier-5
	// thresholds: a long ride that merely happens to finish at work is a sport
	// ride, not a commute.
	if meetsSportThresholds(activity, cfg) {
		return DirectionNone, false
	}

	// ActivityFix rule 3: a ride ending at work is a ride to work.
	if activity.End != nil && cfg.Work.contains(*activity.End) {
		return DirectionToWork, true
	}
	// ActivityFix rule 4: work to home.
	if activity.Start != nil && cfg.Work.contains(*activity.Start) &&
		activity.End != nil && cfg.Home.contains(*activity.End) {
		return DirectionToHome, true
	}

	return DirectionNone, false
}

// noTierReason explains a [TierNone] outcome.
func noTierReason(activity Activity) string {
	if _, ok := rideSportTypes[activity.SportType]; ok {
		return reasonBelowThresholds
	}

	return "sport type " + activity.SportType + " has no tier rule"
}

// earthRadiusMeters is the mean Earth radius used by the haversine formula.
const earthRadiusMeters = 6371000.0

// distanceMeters returns the great-circle distance between two coordinates.
func distanceMeters(a, b Point) float64 {
	latA := a.Lat * math.Pi / 180
	latB := b.Lat * math.Pi / 180
	deltaLat := (b.Lat - a.Lat) * math.Pi / 180
	deltaLon := (b.Lon - a.Lon) * math.Pi / 180

	h := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(latA)*math.Cos(latB)*math.Sin(deltaLon/2)*math.Sin(deltaLon/2)

	return 2 * earthRadiusMeters * math.Asin(math.Sqrt(h))
}
