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
// This is the only mutating call in the service, and the only place a title
// reaches Strava. It refuses with [ErrDryRun] unless the client was built with
// [WriteModeEnabled]; the transport repeats that check, so neither this guard
// nor a future mutating method can be bypassed.
//
// Sport type, gear and description belong to other tools and are never sent.
func (c *Client) UpdateActivityName(ctx context.Context, activityID int64, name string) (*Activity, error) {
	if c.writeMode != WriteModeEnabled {
		return nil, fmt.Errorf("strava: refusing to rename activity %d: %w", activityID, ErrDryRun)
	}

	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("strava: refusing to rename activity %d to an empty title", activityID)
	}

	path := "/activities/" + strconv.FormatInt(activityID, 10)

	form := url.Values{"name": {name}}
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
		return nil, fmt.Errorf("strava: decode renamed activity %d: %w", activityID, err)
	}

	activity.AthleteID = activity.Athlete.ID

	return &activity, nil
}
