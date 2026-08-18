package geo

import (
	"errors"
	"fmt"
)

// Point is a WGS84 coordinate pair in degrees.
//
// Points exist inside this package only. Nothing it returns carries one: the
// naming layer receives place names, so a title can never be derived from a
// coordinate that was never handed over.
type Point struct {
	Lat float64
	Lon float64
}

// ErrBadPolyline means the encoded string is not a valid polyline.
var ErrBadPolyline = errors.New("geo: malformed polyline")

// polylinePrecision is the 1e-5 scaling Google's encoded polyline algorithm
// uses, which is what Strava's summary_polyline is encoded with.
const polylinePrecision = 1e5

// DecodePolyline decodes Google's encoded polyline format.
//
// Implemented here rather than pulled in as a dependency: the algorithm is
// forty lines and the spec allows a polyline decoder, but not needing one at
// all is better.
func DecodePolyline(encoded string) ([]Point, error) {
	points := make([]Point, 0, len(encoded)/4)

	var lat, lon int

	for index := 0; index < len(encoded); {
		deltaLat, next, err := decodeValue(encoded, index)
		if err != nil {
			return nil, err
		}

		deltaLon, next, err := decodeValue(encoded, next)
		if err != nil {
			return nil, err
		}

		lat += deltaLat
		lon += deltaLon
		index = next

		points = append(points, Point{
			Lat: float64(lat) / polylinePrecision,
			Lon: float64(lon) / polylinePrecision,
		})
	}

	return points, nil
}

// decodeValue reads one signed varint and returns it with the next index.
func decodeValue(encoded string, index int) (int, int, error) {
	var (
		result uint32
		shift  uint
		chunk  byte
	)

	for {
		if index >= len(encoded) {
			return 0, 0, fmt.Errorf("%w: truncated value", ErrBadPolyline)
		}

		if encoded[index] < 63 {
			return 0, 0, fmt.Errorf("%w: byte %q is out of range", ErrBadPolyline, encoded[index])
		}

		if shift >= 32 {
			return 0, 0, fmt.Errorf("%w: value does not terminate", ErrBadPolyline)
		}

		chunk = encoded[index] - 63
		index++
		result |= uint32(chunk&0x1f) << shift
		shift += 5

		if chunk < 0x20 {
			break
		}
	}

	// The low bit is the sign, and negatives are stored one's-complemented.
	value := int(result >> 1)
	if result&1 != 0 {
		value = ^value
	}

	return value, index, nil
}
