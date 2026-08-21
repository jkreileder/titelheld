package strava

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxGearBytes bounds a gear response. A gear document is a handful of fields;
// this is orders of magnitude more than one encodes to.
const maxGearBytes = 64 << 10

// Gear is a bike or a pair of shoes.
//
// Only the name is read. The rest of what Strava returns — brand, model,
// distance, frame type — is not something a title should be built from, and
// what is never decoded cannot leak into a prompt.
type Gear struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetGear returns the gear an activity was recorded with.
//
// The activity carries only a gear ID, and a franchise keys on the name — the
// Pink Panther films go on the bike called "Pink Panther", which is a string
// the athlete typed into Strava and nothing here can derive.
//
// An empty ID is not an error: an activity recorded without gear simply has
// none, and the answer is the zero Gear.
func (c *Client) GetGear(ctx context.Context, gearID string) (Gear, error) {
	gearID = strings.TrimSpace(gearID)
	if gearID == "" {
		return Gear{}, nil
	}

	// The ID comes from an activity, which is to say from outside this
	// service. Escaped so it cannot climb out of the path and address a
	// different endpoint.
	path := "/gear/" + url.PathEscape(gearID)

	response, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Gear{}, err
	}
	defer drainAndClose(response)

	decoder := json.NewDecoder(io.LimitReader(response.Body, maxGearBytes))

	var gear Gear
	if err := decoder.Decode(&gear); err != nil {
		return Gear{}, fmt.Errorf("strava: decode gear %q: %w", gearID, err)
	}

	// One JSON value and nothing after it. Decode stops at the end of the
	// first value, so a valid object followed by anything at all — a second
	// object, or trailing junk — would otherwise be accepted silently, and
	// this reads a response the service did not produce.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Gear{}, fmt.Errorf("strava: gear %q: trailing data after the response", gearID)
	}

	return gear, nil
}
