package geo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Fingerprint reduces a route to a value that answers "this again".
//
// It is not reversible and is not meant to be: a rounded, deduplicated point
// sequence is hashed, and only the digest is ever stored. Two rides of the
// same roads produce the same fingerprint; nothing about the fingerprint says
// which roads they were.
//
// It is also direction-insensitive. An out-and-back ridden the other way is
// the same route to a person, and would otherwise be a different one to the
// store — so the forward and reversed sequences are compared and the smaller
// is hashed. That is the whole of the "direction reversals" the spec asks for.
//
// An empty polyline is not an error: an indoor ride or a recording with GPS
// off has no route, and the answer is the empty string, meaning "do not
// count this".
func Fingerprint(encodedPolyline string) (string, error) {
	if encodedPolyline == "" {
		return "", nil
	}

	points, err := DecodePolyline(encodedPolyline)
	if err != nil {
		return "", fmt.Errorf("geo: fingerprint: %w", err)
	}

	rounded := roundPoints(points)
	if len(rounded) == 0 {
		return "", nil
	}

	forward := strings.Join(rounded, ";")

	reversed := make([]string, len(rounded))
	for index, value := range rounded {
		reversed[len(rounded)-1-index] = value
	}

	canonical := forward
	if backward := strings.Join(reversed, ";"); backward < canonical {
		canonical = backward
	}

	digest := sha256.Sum256([]byte(canonical))

	return hex.EncodeToString(digest[:]), nil
}

// roundPoints rounds each point and collapses consecutive duplicates.
//
// The rounding is [CacheKey]'s, which is the same 110 m this package already
// rounds coordinates to and, more to the point, already handles the negative
// zero: -0.0001 and 0.0001 round to one place, and formatting them naively
// gives "-0.000" and "0.000" — two fingerprints for one route.
//
// Collapsing matters as much as rounding. A ride paused at a traffic light
// records the same rounded point many times over, and a stop of a different
// length would otherwise be a different route.
func roundPoints(points []Point) []string {
	rounded := make([]string, 0, len(points))

	var previous string

	for _, point := range points {
		value := CacheKey(point)

		if value == previous {
			continue
		}

		rounded = append(rounded, value)
		previous = value
	}

	return rounded
}
