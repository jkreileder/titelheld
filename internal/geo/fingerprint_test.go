package geo

import (
	"strings"
	"testing"
)

// encodePolyline is the inverse of DecodePolyline, so a test can build a route
// from coordinates rather than from a magic string.
func encodePolyline(points []Point) string {
	var b strings.Builder

	var prevLat, prevLon int

	for _, point := range points {
		lat := int(point.Lat*polylinePrecision + 0.5)
		lon := int(point.Lon*polylinePrecision + 0.5)

		encodeValue(&b, lat-prevLat)
		encodeValue(&b, lon-prevLon)

		prevLat, prevLon = lat, lon
	}

	return b.String()
}

func encodeValue(b *strings.Builder, value int) {
	shifted := value << 1
	if value < 0 {
		shifted = ^shifted
	}

	for shifted >= 0x20 {
		b.WriteByte(byte((0x20 | (shifted & 0x1f)) + 63))
		shifted >>= 5
	}

	b.WriteByte(byte(shifted + 63))
}

// A synthetic route. Invented coordinates, as everything here is.
func syntheticRoute() []Point {
	return []Point{
		{Lat: 50.000, Lon: 10.000},
		{Lat: 50.010, Lon: 10.012},
		{Lat: 50.021, Lon: 10.025},
		{Lat: 50.033, Lon: 10.031},
	}
}

// The same roads give the same answer, and different roads do not.
func TestFingerprintIdentifiesARoute(t *testing.T) {
	t.Parallel()

	route := encodePolyline(syntheticRoute())

	first, err := Fingerprint(route)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	if first == "" {
		t.Fatal("a route with points produced no fingerprint")
	}

	again, err := Fingerprint(route)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	if again != first {
		t.Errorf("the same route fingerprinted differently: %q then %q", first, again)
	}

	elsewhere := encodePolyline([]Point{
		{Lat: 51.000, Lon: 11.000},
		{Lat: 51.010, Lon: 11.012},
	})

	other, err := Fingerprint(elsewhere)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	if other == first {
		t.Error("two different routes share a fingerprint")
	}
}

// The same roads ridden the other way are the same route.
//
// An out-and-back reversed is the same ride to a person, and the spec asks for
// direction reversals to be detected rather than counted as something new.
func TestFingerprintIgnoresDirection(t *testing.T) {
	t.Parallel()

	forward := syntheticRoute()

	backward := make([]Point, len(forward))
	for index, point := range forward {
		backward[len(forward)-1-index] = point
	}

	first, err := Fingerprint(encodePolyline(forward))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	second, err := Fingerprint(encodePolyline(backward))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	if first != second {
		t.Errorf("the same route reversed fingerprinted differently: %q vs %q", first, second)
	}
}

// GPS scatter below the rounding is the same route.
func TestFingerprintToleratesScatter(t *testing.T) {
	t.Parallel()

	clean := syntheticRoute()

	jittered := make([]Point, len(clean))
	for index, point := range clean {
		// Well under the 110 m rounding.
		jittered[index] = Point{Lat: point.Lat + 0.0002, Lon: point.Lon - 0.0002}
	}

	first, err := Fingerprint(encodePolyline(clean))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	second, err := Fingerprint(encodePolyline(jittered))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	if first != second {
		t.Errorf("scatter below the rounding changed the fingerprint: %q vs %q", first, second)
	}
}

// A pause records the same point repeatedly, and must not make a new route.
func TestFingerprintCollapsesAStop(t *testing.T) {
	t.Parallel()

	moving := syntheticRoute()

	stopped := []Point{moving[0], moving[0], moving[0], moving[1], moving[1], moving[2], moving[3]}

	first, err := Fingerprint(encodePolyline(moving))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	second, err := Fingerprint(encodePolyline(stopped))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	if first != second {
		t.Errorf("a stop changed the fingerprint: %q vs %q", first, second)
	}
}

// No route is not a failure, and no fingerprint either.
func TestFingerprintOfNothing(t *testing.T) {
	t.Parallel()

	got, err := Fingerprint("")
	if err != nil {
		t.Fatalf("Fingerprint of an empty polyline: %v", err)
	}

	if got != "" {
		t.Errorf("an empty polyline produced %q, want no fingerprint", got)
	}
}

// A malformed polyline is reported rather than fingerprinted.
func TestFingerprintRejectsAMalformedPolyline(t *testing.T) {
	t.Parallel()

	if _, err := Fingerprint("\x01\x02not a polyline"); err == nil {
		t.Error("a malformed polyline was fingerprinted")
	}
}

// The digest reveals nothing about where the ride went.
//
// This is the privacy claim the store leans on: only the digest is persisted,
// and it must not carry a coordinate in it.
func TestFingerprintCarriesNoCoordinates(t *testing.T) {
	t.Parallel()

	got, err := Fingerprint(encodePolyline(syntheticRoute()))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	for _, fragment := range []string{"50.", "10.", ",", ";", "-"} {
		if strings.Contains(got, fragment) {
			t.Errorf("the fingerprint %q contains %q", got, fragment)
		}
	}

	// A sha256 digest in hex, and nothing else.
	if len(got) != 64 {
		t.Errorf("the fingerprint is %d characters, want a 64-character digest", len(got))
	}
}
