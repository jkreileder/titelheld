package strava

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Activity is the subset of Strava's detailed activity this service reads.
//
// Field names follow Strava's JSON. The classifier consumes a transport-neutral
// value derived from this, never this type.
type Activity struct {
	ID          int64  `json:"id"`
	AthleteID   int64  `json:"-"`
	Name        string `json:"name"`
	Description string `json:"description"`
	SportType   string `json:"sport_type"`
	Type        string `json:"type"`

	Distance      float64 `json:"distance"`
	MovingTime    int     `json:"moving_time"`
	ElapsedTime   int     `json:"elapsed_time"`
	TotalElevGain float64 `json:"total_elevation_gain"`
	AverageSpeed  float64 `json:"average_speed"`
	MaxSpeed      float64 `json:"max_speed"`

	Trainer bool `json:"trainer"`
	Commute bool `json:"commute"`
	Private bool `json:"private"`
	Manual  bool `json:"manual"`

	// StartDateLocal is the athlete's local wall-clock time, and it is what a
	// title should be built from: a ride at 23:30 local on a Saturday is
	// Sunday in UTC, and Strava's own default titles ("Morning Ride") come
	// from local time too.
	//
	// Strava sends it with a "Z" suffix despite it not being UTC, so this
	// parses to a time whose Hour and Weekday are the local ones but whose
	// location says UTC. Read the fields; do not convert. Calling UTC or In on
	// it moves the ride to a different clock than the one it happened on.
	StartDateLocal time.Time `json:"start_date_local"`

	// StartDate is the real instant, in UTC.
	StartDate time.Time `json:"start_date"`

	// Timezone is Strava's descriptive form, e.g. "(GMT+01:00) Europe/Berlin".
	Timezone string `json:"timezone"`

	StartLatLng []float64 `json:"start_latlng"`
	EndLatLng   []float64 `json:"end_latlng"`

	GearID string `json:"gear_id"`

	Athlete struct {
		ID int64 `json:"id"`
	} `json:"athlete"`

	Map struct {
		Polyline        string `json:"polyline"`
		SummaryPolyline string `json:"summary_polyline"`
	} `json:"map"`

	// SegmentEfforts are the athlete's efforts on named segments. Strava
	// returns the notable ones with a detailed activity; which ones it counts
	// as notable is Strava's decision and not one to build on, so a caller
	// selects again from the fields below.
	//
	// Only the name is ever meant to leave this package for a prompt. The
	// times and identifiers are here because they arrive in the same object,
	// not because a title may use them.
	SegmentEfforts []SegmentEffort `json:"segment_efforts"`
}

// SegmentEffort is one ride over a named segment.
//
// Deliberately narrow. Strava sends a great deal more — coordinates,
// identifiers, split times — and none of it is a title's business, so it is
// not decoded and cannot be handed to a model by accident.
type SegmentEffort struct {
	// Name is the segment's name as Strava reports it on the effort. It is
	// free text somebody typed, like a gear name, and is treated as such.
	Name string `json:"name"`

	// PRRank is 1, 2 or 3 when this effort is one of the athlete's three
	// fastest on the segment, and 0 otherwise.
	PRRank int `json:"pr_rank"`

	// Achievements are the ranks Strava awarded this effort. Only whether
	// there are any is used.
	Achievements []SegmentAchievement `json:"achievements"`

	// Segment carries the name again for responses that leave the effort's own
	// name empty.
	Segment struct {
		Name string `json:"name"`
	} `json:"segment"`
}

// SegmentAchievement is one rank Strava awarded an effort.
type SegmentAchievement struct {
	Type string `json:"type"`
	Rank int    `json:"rank"`
}

// Owner returns the athlete this activity belongs to.
func (a *Activity) Owner() int64 {
	if a.AthleteID != 0 {
		return a.AthleteID
	}

	return a.Athlete.ID
}

// maxActivityBytes caps what a decode will read from an activity response.
//
// drainAndClose already refuses to read an unbounded body, but it runs after
// the decode, so the decoder needs its own ceiling: without one a response
// that never ends makes this process allocate until it is killed. A detailed
// activity carries a summary polyline and free-text fields, so a megabyte is
// generous rather than tight.
const maxActivityBytes = 1 << 20

// GetActivity fetches one activity by ID.
func (c *Client) GetActivity(ctx context.Context, activityID int64) (*Activity, error) {
	path := "/activities/" + strconv.FormatInt(activityID, 10)

	response, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(response)

	var activity Activity
	if err := json.NewDecoder(io.LimitReader(response.Body, maxActivityBytes)).Decode(&activity); err != nil {
		return nil, fmt.Errorf("strava: decode activity %d: %w", activityID, err)
	}

	activity.AthleteID = activity.Athlete.ID

	return &activity, nil
}

// UpdateActivityName renames one activity and changes nothing else.
//
// This is the only place a title reaches Strava. It refuses with [ErrDryRun]
// unless the client was built with [WriteModeEnabled]; the transport repeats
// that check, so neither this guard nor a future mutating method can be
// bypassed.
//
// Sport type and gear belong to other tools and are never sent. The
// description is sent only by [UpdateActivityNameAndDescription] and
// [UpdateActivityDescription], and only to carry the attribution line or to
// take it away again.
func (c *Client) UpdateActivityName(ctx context.Context, activityID int64, name string) (*Activity, error) {
	return c.update(ctx, activityID, &name, nil)
}

// UpdateActivityNameAndDescription renames an activity and replaces its
// description in the same call.
//
// One call rather than two, because Strava emits a webhook event per update:
// two PUTs would produce two events, and the second would arrive after this
// service had already recorded the activity as named, which is exactly the
// shape a self-caused event is supposed to have. One write, one event.
//
// The description is sent whole because Strava's PUT replaces it. Building the
// new value — preserving what other tools wrote — belongs to the caller; see
// the writer.
func (c *Client) UpdateActivityNameAndDescription(
	ctx context.Context, activityID int64, name, description string,
) (*Activity, error) {
	return c.update(ctx, activityID, &name, &description)
}

// UpdateActivityDescription replaces the description and leaves the title
// alone.
//
// The title is omitted from the form rather than sent back unchanged. Sending
// it would be a rename to the value Strava already holds, on an activity whose
// title is the athlete's — and Strava rewrites a title it reads as containing
// a link, so a round trip through this call is a chance to corrupt a title
// this service has no business touching. What is not in the form cannot be
// rewritten.
//
// The description is sent whole, because Strava's PUT replaces it. Building
// the new value belongs to the caller; see the writer.
func (c *Client) UpdateActivityDescription(
	ctx context.Context, activityID int64, description string,
) (*Activity, error) {
	return c.update(ctx, activityID, nil, &description)
}

// update is the single mutating path. Every exported method lands here so the
// dry-run guard cannot be reached around.
//
// A nil field is left out of the form entirely, which is how a caller says
// "leave this alone": Strava changes only what the PUT names.
func (c *Client) update(
	ctx context.Context, activityID int64, name, description *string,
) (*Activity, error) {
	if c.writeMode != WriteModeEnabled {
		return nil, fmt.Errorf("strava: refusing to update activity %d: %w", activityID, ErrDryRun)
	}

	if name != nil && strings.TrimSpace(*name) == "" {
		return nil, fmt.Errorf("strava: refusing to rename activity %d to an empty title", activityID)
	}

	if name == nil && description == nil {
		return nil, fmt.Errorf("strava: refusing to update activity %d with no fields", activityID)
	}

	path := "/activities/" + strconv.FormatInt(activityID, 10)

	form := url.Values{}
	if name != nil {
		form.Set("name", *name)
	}

	if description != nil {
		form.Set("description", *description)
	}
	body := func() (*strings.Reader, string) {
		return strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	}

	response, err := c.do(ctx, http.MethodPut, path, body)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(response)

	var activity Activity
	if err := json.NewDecoder(io.LimitReader(response.Body, maxActivityBytes)).Decode(&activity); err != nil {
		return nil, fmt.Errorf("strava: decode updated activity %d: %w", activityID, err)
	}

	activity.AthleteID = activity.Athlete.ID

	return &activity, nil
}
