package route

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// TestDecodePolyline uses the worked example from Google's specification,
// which is the only fixture whose expected output is independently known.
func TestDecodePolyline(t *testing.T) {
	t.Parallel()

	points, err := DecodePolyline("_p~iF~ps|U_ulLnnqC_mqNvxq`@")
	if err != nil {
		t.Fatalf("DecodePolyline: %v", err)
	}

	want := []Point{
		{Lat: 38.5, Lon: -120.2},
		{Lat: 40.7, Lon: -120.95},
		{Lat: 43.252, Lon: -126.453},
	}

	if len(points) != len(want) {
		t.Fatalf("decoded %d points, want %d", len(points), len(want))
	}

	for i, expected := range want {
		if math.Abs(points[i].Lat-expected.Lat) > 1e-6 ||
			math.Abs(points[i].Lon-expected.Lon) > 1e-6 {
			t.Errorf("point %d = %+v, want %+v", i, points[i], expected)
		}
	}
}

func TestDecodePolylineRejectsGarbage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		encoded string
		wantMsg string // empty: only ErrBadPolyline is asserted
	}{
		{name: "truncated pair", encoded: "_p~iF"},
		{name: "byte below the printable range", encoded: "_p~iF\x01"},
		{name: "byte above the alphabet", encoded: "_p~iF~ps|U\x7f\x7f"},
		{name: "utf-8 continuation byte", encoded: "_p~iF~ps|U\xc3\xa9"},
		{
			// Seven '~' bytes are seven in-alphabet continuation chunks: six
			// fill the 30-bit value, and the seventh must trip the width
			// guard rather than be folded in. Bytes outside the alphabet
			// (\xff and friends) never get that far — they die at the
			// alphabet check above — so this is the ONLY case here that
			// exercises it.
			name:    "value wider than the format allows",
			encoded: "~~~~~~~",
			wantMsg: "does not terminate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodePolyline(tt.encoded)
			if !errors.Is(err, ErrBadPolyline) {
				t.Errorf("DecodePolyline(%q) = %v, want ErrBadPolyline", tt.encoded, err)
			}

			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("DecodePolyline(%q) = %q, want the %q guard", tt.encoded, err, tt.wantMsg)
			}
		})
	}
}

// An over-long polyline is refused before it sizes an allocation. The decoder
// derives its capacity hint from the input length, and the input comes from
// Strava rather than from anything this service controls.
//
// "??" is one point with a zero delta in both axes, so a long run of them
// stays at the origin: length is the only thing under test here, and a
// rejection cannot be coming from the coordinate-range check instead.
func TestDecodePolylineRejectsAnOverlongInput(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("??", (maxEncodedPolylineBytes/2)+1)

	if len(oversized) <= maxEncodedPolylineBytes {
		t.Fatalf("test input is %d bytes, which does not exceed the %d-byte limit",
			len(oversized), maxEncodedPolylineBytes)
	}

	_, err := DecodePolyline(oversized)
	if !errors.Is(err, ErrBadPolyline) {
		t.Fatalf("DecodePolyline(%d bytes) = %v, want ErrBadPolyline", len(oversized), err)
	}

	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("DecodePolyline rejected the input as %v, want the length guard", err)
	}
}

// An input exactly at the limit still decodes, so the guard bounds the input
// without narrowing what a real activity may carry.
func TestDecodePolylineAcceptsTheLimit(t *testing.T) {
	t.Parallel()

	atLimit := strings.Repeat("??", maxEncodedPolylineBytes/2)

	if len(atLimit) != maxEncodedPolylineBytes {
		t.Fatalf("test input is %d bytes, want exactly %d", len(atLimit), maxEncodedPolylineBytes)
	}

	points, err := DecodePolyline(atLimit)
	if err != nil {
		t.Fatalf("DecodePolyline(%d bytes) = %v, want no error", len(atLimit), err)
	}

	if len(points) != maxEncodedPolylineBytes/2 {
		t.Errorf("DecodePolyline decoded %d points, want %d", len(points), maxEncodedPolylineBytes/2)
	}
}

// A corrupt polyline that still decodes must be refused, not snapped into
// cells in whichever hemisphere its garbage lands in.
func TestDecodePolylineRejectsImpossibleCoordinates(t *testing.T) {
	t.Parallel()

	// A latitude past the pole, encoded legitimately.
	if _, err := DecodePolyline(encodeForTest([]Point{{Lat: 91, Lon: 0}})); !errors.Is(err, ErrBadPolyline) {
		t.Errorf("DecodePolyline of a 91° latitude = %v, want ErrBadPolyline", err)
	}

	// The extremes themselves stay valid.
	if _, err := DecodePolyline(encodeForTest([]Point{{Lat: 90, Lon: 180}})); err != nil {
		t.Errorf("DecodePolyline of the coordinate extremes = %v, want nil", err)
	}
}

// An empty polyline is not garbage: an activity recorded without GPS carries
// no summary polyline at all, and that must decode to no points, not to an
// error.
func TestDecodeEmptyPolyline(t *testing.T) {
	t.Parallel()

	points, err := DecodePolyline("")
	if err != nil || len(points) != 0 {
		t.Errorf("DecodePolyline(\"\") = %v, %v; want no points, no error", points, err)
	}
}

// Round-tripping random walks through encode and decode exercises pairs of
// deltas the fixed fixtures never produce — long same-direction runs, sign
// changes at odd magnitudes, and the ±180° wrap neighbourhood.
//
// The encoder quantises to 1e-5 degrees, so decoded output is compared with
// the quantised input, not the raw input: the format's resolution is the
// contract, bit-exactness is not on offer.
func TestDecodePolylineRoundTrips(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(1, 2))

	for trial := range 50 {
		var (
			lat = rng.Float64()*160 - 80
			lon = rng.Float64()*340 - 170
		)

		points := make([]Point, 0, 200)

		for range 200 {
			lat = math.Min(90, math.Max(-90, lat+rng.NormFloat64()/100))
			lon = math.Min(180, math.Max(-180, lon+rng.NormFloat64()/100))
			points = append(points, Point{Lat: lat, Lon: lon})
		}

		decoded, err := DecodePolyline(encodeForTest(points))
		if err != nil {
			t.Fatalf("trial %d: DecodePolyline: %v", trial, err)
		}

		if len(decoded) != len(points) {
			t.Fatalf("trial %d: decoded %d points, want %d", trial, len(decoded), len(points))
		}

		for i := range points {
			quantised := Point{
				Lat: math.Round(points[i].Lat*polylinePrecision) / polylinePrecision,
				Lon: math.Round(points[i].Lon*polylinePrecision) / polylinePrecision,
			}

			// Coordinates are quantised to 1e-5 degrees by the format itself.
			if math.Abs(decoded[i].Lat-quantised.Lat) > 1e-9 ||
				math.Abs(decoded[i].Lon-quantised.Lon) > 1e-9 {
				t.Fatalf("trial %d: point %d = %+v, want %+v", trial, i, decoded[i], quantised)
			}
		}
	}
}

// encodeForTest produces an encoded polyline from points, so fixtures can be
// written as coordinates rather than as opaque strings.
func encodeForTest(points []Point) string {
	var (
		encoded  []byte
		lat, lon int
	)

	for _, point := range points {
		latE5 := int(math.Round(point.Lat * polylinePrecision))
		lonE5 := int(math.Round(point.Lon * polylinePrecision))

		encoded = appendValue(encoded, latE5-lat)
		encoded = appendValue(encoded, lonE5-lon)

		lat, lon = latE5, lonE5
	}

	return string(encoded)
}

func appendValue(dst []byte, value int) []byte {
	shifted := value << 1
	if value < 0 {
		shifted = ^shifted
	}

	for shifted >= 0x20 {
		dst = append(dst, byte((0x20|(shifted&0x1f))+63))
		shifted >>= 5
	}

	return append(dst, byte(shifted+63))
}
