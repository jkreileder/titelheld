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

// maxEncodedPolylineBytes bounds an encoded summary polyline. Strava's summary
// polyline is a simplified track: half a megabyte is orders of magnitude more
// than a long ride encodes to, and still small enough to allocate freely.
const maxEncodedPolylineBytes = 512 << 10

// polylinePrecision is the 1e-5 scaling Google's encoded polyline algorithm
// uses, which is what Strava's summary_polyline is encoded with.
const polylinePrecision = 1e5

// The encoded alphabet, and the widest value the format can legitimately carry.
const (
	minPolylineByte = 63
	maxPolylineByte = 126
	maxValueBits    = 30
)

// DecodePolyline decodes Google's encoded polyline format.
//
// Implemented here rather than pulled in as a dependency: the algorithm is
// forty lines and the spec allows a polyline decoder, but not needing one at
// all is better.
func DecodePolyline(encoded string) ([]Point, error) {
	// The capacity below is sized from the input, so the input needs a bound
	// of its own: the polyline arrives from Strava and nothing upstream
	// promises it is the length a real ride produces.
	if len(encoded) > maxEncodedPolylineBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte limit",
			ErrBadPolyline, len(encoded), maxEncodedPolylineBytes)
	}

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

		point := Point{
			Lat: float64(lat) / polylinePrecision,
			Lon: float64(lon) / polylinePrecision,
		}

		// A corrupt polyline that still decodes would otherwise be geocoded:
		// Nominatim would answer for the wrong hemisphere, and a place name
		// from nowhere near the ride would become eligible for a title.
		if point.Lat < -90 || point.Lat > 90 || point.Lon < -180 || point.Lon > 180 {
			return nil, fmt.Errorf("%w: coordinate %v is out of range", ErrBadPolyline, point)
		}

		points = append(points, point)
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

		// The encoding maps each 5-bit chunk to a byte in 63..126. Anything
		// outside that is not polyline — a UTF-8 continuation byte from a
		// mis-transcoded field, say — and folding it in would silently produce
		// a plausible-looking coordinate.
		if encoded[index] < minPolylineByte || encoded[index] > maxPolylineByte {
			return 0, 0, fmt.Errorf("%w: byte %q is outside the polyline alphabet",
				ErrBadPolyline, encoded[index])
		}

		// Six chunks carry 30 bits, which is more than a coordinate delta
		// needs (±180e5 fits in 26 bits once zig-zagged). Checking before the
		// chunk is consumed is what stops a seventh being folded in and
		// truncated by the shift.
		if shift >= maxValueBits {
			return 0, 0, fmt.Errorf("%w: value does not terminate", ErrBadPolyline)
		}

		chunk = encoded[index] - minPolylineByte
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
