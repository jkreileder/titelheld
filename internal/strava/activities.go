package strava

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// MaxActivitiesPerPage is the largest page Strava serves.
const MaxActivitiesPerPage = 200

// maxActivityListBytes bounds a page of summary activities.
//
// Two hundred summaries with their polylines: generous, and still far below
// what a page actually encodes to.
const maxActivityListBytes = 8 << 20

// ListActivities returns one page of the athlete's activities, newest first.
//
// Summary activities: everything the list endpoint carries, which includes the
// title and the start date and does not include the description. That is all
// the history import needs, and it is why importing a few hundred activities
// costs a handful of requests rather than one per activity.
//
// An empty page means the end of the history. Pages are one-based, as Strava
// numbers them.
func (c *Client) ListActivities(ctx context.Context, page, perPage int) ([]Activity, error) {
	if page < 1 {
		return nil, fmt.Errorf("strava: page must be 1 or greater, got %d", page)
	}

	if perPage < 1 || perPage > MaxActivitiesPerPage {
		return nil, fmt.Errorf(
			"strava: per page must be between 1 and %d, got %d", MaxActivitiesPerPage, perPage)
	}

	query := url.Values{}
	query.Set("page", strconv.Itoa(page))
	query.Set("per_page", strconv.Itoa(perPage))

	response, err := c.do(ctx, http.MethodGet, "/athlete/activities?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(response)

	// Same shape as the gear read: the reader is kept so a body that ended can
	// be told from one that hit the cap, rather than trusting an EOF the limit
	// produced.
	limited := &io.LimitedReader{R: response.Body, N: maxActivityListBytes + 1}
	decoder := json.NewDecoder(limited)

	var activities []Activity
	if err := decoder.Decode(&activities); err != nil {
		return nil, fmt.Errorf("strava: decode activity page %d: %w", page, err)
	}

	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("strava: activity page %d: trailing data after the response", page)
	}

	if limited.N <= 0 {
		return nil, fmt.Errorf(
			"strava: activity page %d exceeds %d bytes", page, maxActivityListBytes)
	}

	for index := range activities {
		activities[index].AthleteID = activities[index].Athlete.ID
	}

	return activities, nil
}
