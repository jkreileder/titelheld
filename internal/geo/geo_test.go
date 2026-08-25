package geo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/jkreileder/titelheld/internal/store"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDecodePolyline uses the worked example from Google's specification, which
// is the only fixture whose expected output is independently known.
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
	}{
		{name: "truncated pair", encoded: "_p~iF"},
		{name: "byte below the printable range", encoded: "_p~iF\x01"},
		{name: "value never terminates", encoded: "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := DecodePolyline(tt.encoded); !errors.Is(err, ErrBadPolyline) {
				t.Errorf("DecodePolyline(%q) = %v, want ErrBadPolyline", tt.encoded, err)
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

func TestDecodeEmptyPolyline(t *testing.T) {
	t.Parallel()

	points, err := DecodePolyline("")
	if err != nil || len(points) != 0 {
		t.Errorf("DecodePolyline(\"\") = %v, %v", points, err)
	}
}

func TestCacheKeyRounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		point Point
		want  string
	}{
		{point: Point{Lat: 0, Lon: 0}, want: "0.000,0.000"},
		{point: Point{Lat: 0.0503, Lon: 0.0002}, want: "0.050,0.000"},
		{point: Point{Lat: 1.23456, Lon: 6.78901}, want: "1.235,6.789"},
		{point: Point{Lat: -1.23456, Lon: -6.78949}, want: "-1.235,-6.789"},
		// Two points 50 m apart share a key, which is the whole point.
		{point: Point{Lat: 1.23451, Lon: 6.78912}, want: "1.235,6.789"},
		// A negative that rounds to zero must not become "-0.000".
		{point: Point{Lat: -0.0001, Lon: -0.0002}, want: "0.000,0.000"},
	}

	for _, tt := range tests {
		if got := CacheKey(tt.point); got != tt.want {
			t.Errorf("CacheKey(%+v) = %q, want %q", tt.point, got, tt.want)
		}
	}
}

func TestSamplePoints(t *testing.T) {
	t.Parallel()

	if got := SamplePoints(nil); got != nil {
		t.Errorf("SamplePoints(nil) = %v, want nil", got)
	}

	// A synthetic out-and-back around Null Island.
	points := []Point{
		{Lat: 0.000, Lon: 0.000},
		{Lat: 0.010, Lon: 0.005},
		{Lat: 0.020, Lon: -0.005},
		{Lat: 0.030, Lon: 0.010},
		{Lat: 0.010, Lon: 0.005},
		{Lat: 0.000, Lon: 0.000},
	}

	samples := SamplePoints(points)

	// Start, four extremes, three waypoints.
	if len(samples) != 8 {
		t.Fatalf("sampled %d points, want 8", len(samples))
	}

	if samples[0] != points[0] {
		t.Errorf("samples[0] = %+v, want the start", samples[0])
	}

	// The extremes must actually be extreme.
	var minLat, maxLat, minLon, maxLon = points[0], points[0], points[0], points[0]

	for _, p := range points {
		if p.Lat < minLat.Lat {
			minLat = p
		}
		if p.Lat > maxLat.Lat {
			maxLat = p
		}
		if p.Lon < minLon.Lon {
			minLon = p
		}
		if p.Lon > maxLon.Lon {
			maxLon = p
		}
	}

	for _, want := range []Point{minLat, maxLat, minLon, maxLon} {
		found := false

		for _, sample := range samples {
			if sample == want {
				found = true

				break
			}
		}

		if !found {
			t.Errorf("the extreme %+v was not sampled", want)
		}
	}
}

func TestSampleSinglePoint(t *testing.T) {
	t.Parallel()

	samples := SamplePoints([]Point{{Lat: 1, Lon: 2}})
	for _, sample := range samples {
		if sample != (Point{Lat: 1, Lon: 2}) {
			t.Errorf("sample = %+v, want the only point", sample)
		}
	}
}

// fakeReverser records what it was asked and answers from a table.
type fakeReverser struct {
	calls  []Point
	places map[string]store.Place
	err    error
}

func (f *fakeReverser) Reverse(_ context.Context, point Point) (store.Place, error) {
	f.calls = append(f.calls, point)

	if f.err != nil {
		return store.Place{}, f.err
	}

	if place, ok := f.places[CacheKey(point)]; ok {
		return place, nil
	}

	return store.Place{Name: "Musterdorf", Kind: "village", Country: "Testland"}, nil
}

func newDescriber(t *testing.T, reverser Reverser) (*Describer, *store.Memory) {
	t.Helper()

	memory := store.NewMemory()

	describer, err := NewDescriber(reverser, memory, quietLogger())
	if err != nil {
		t.Fatalf("NewDescriber: %v", err)
	}

	return describer, memory
}

func TestNewDescriberRequiresItsCollaborators(t *testing.T) {
	t.Parallel()

	if _, err := NewDescriber(nil, store.NewMemory(), nil); err == nil {
		t.Error("NewDescriber without a reverser = nil error, want error")
	}
	if _, err := NewDescriber(&fakeReverser{}, nil, nil); err == nil {
		t.Error("NewDescriber without a cache = nil error, want error")
	}

	describer, err := NewDescriber(&fakeReverser{}, store.NewMemory(), nil)
	if err != nil {
		t.Fatalf("NewDescriber: %v", err)
	}
	if describer.logger == nil {
		t.Error("NewDescriber left the logger unset")
	}
}

func TestDescribe(t *testing.T) {
	t.Parallel()

	reverser := &fakeReverser{places: map[string]store.Place{
		"0.000,0.000": {Name: "Musterdorf", Kind: "village", Region: "Musterregion", Country: "Testland"},
	}}

	describer, _ := newDescriber(t, reverser)

	summary, err := describer.Describe(t.Context(), "_p~iF~ps|U_ulLnnqC_mqNvxq`@")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if summary.Empty() {
		t.Fatal("Describe returned an empty summary")
	}
	if summary.Country != "Testland" {
		t.Errorf("Country = %q", summary.Country)
	}
	if len(summary.Names()) == 0 {
		t.Error("Names() is empty")
	}
}

// An out-and-back puts several samples on the same rounded key; each place must
// be fetched once, because every fetch costs a second of the rate-limit budget.
func TestDescribeDeduplicatesByCacheKey(t *testing.T) {
	t.Parallel()

	reverser := &fakeReverser{}
	describer, _ := newDescriber(t, reverser)

	// A route that returns to its start: several samples collapse onto one key.
	points := []Point{
		{Lat: 0.0000, Lon: 0.0000},
		{Lat: 0.0001, Lon: 0.0001},
		{Lat: 0.0002, Lon: 0.0000},
		{Lat: 0.0000, Lon: 0.0000},
	}

	samples := SamplePoints(points)

	keys := make(map[string]struct{}, len(samples))
	for _, sample := range samples {
		keys[CacheKey(sample)] = struct{}{}
	}

	summary, err := describer.Describe(t.Context(), encodeForTest(points))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if len(reverser.calls) != len(keys) {
		t.Errorf("reverse-geocoded %d times for %d distinct keys", len(reverser.calls), len(keys))
	}
	if summary.Empty() {
		t.Error("summary is empty")
	}
}

func TestDescribeUsesAndFillsTheCache(t *testing.T) {
	t.Parallel()

	reverser := &fakeReverser{}
	describer, memory := newDescriber(t, reverser)

	const encoded = "_p~iF~ps|U_ulLnnqC_mqNvxq`@"

	if _, err := describer.Describe(t.Context(), encoded); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	firstPass := len(reverser.calls)
	if firstPass == 0 {
		t.Fatal("nothing was reverse-geocoded")
	}

	// Everything is cached now, so a second pass must hit Nominatim zero times.
	if _, err := describer.Describe(t.Context(), encoded); err != nil {
		t.Fatalf("second Describe: %v", err)
	}

	if len(reverser.calls) != firstPass {
		t.Errorf("second pass made %d more requests, want 0", len(reverser.calls)-firstPass)
	}

	// The cache holds places, and the coordinates survive only as rounded keys.
	place, ok, err := memory.Place(t.Context(), CacheKey(Point{Lat: 38.5, Lon: -120.2}))
	if err != nil || !ok {
		t.Fatalf("cache lookup = %+v, %v, %v", place, ok, err)
	}
}

// An empty answer is cached too: asking again every time spends the budget on a
// question already answered.
func TestEmptyAnswersAreCached(t *testing.T) {
	t.Parallel()

	reverser := &fakeReverser{places: map[string]store.Place{}}
	reverser.places["38.500,-120.200"] = store.Place{}

	describer, memory := newDescriber(t, reverser)

	if _, err := describer.Describe(t.Context(), "_p~iF~ps|U"); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	_, ok, err := memory.Place(t.Context(), "38.500,-120.200")
	if err != nil {
		t.Fatalf("cache lookup: %v", err)
	}
	if !ok {
		t.Error("an empty answer was not cached")
	}
}

func TestDescribeWithoutAPolyline(t *testing.T) {
	t.Parallel()

	reverser := &fakeReverser{}
	describer, _ := newDescriber(t, reverser)

	summary, err := describer.Describe(t.Context(), "")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if !summary.Empty() || len(reverser.calls) != 0 {
		t.Errorf("an indoor ride must resolve to nothing, got %+v after %d calls",
			summary, len(reverser.calls))
	}
}

func TestDescribeReportsFailures(t *testing.T) {
	t.Parallel()

	t.Run("bad polyline", func(t *testing.T) {
		t.Parallel()

		describer, _ := newDescriber(t, &fakeReverser{})

		if _, err := describer.Describe(t.Context(), "_p~iF"); !errors.Is(err, ErrBadPolyline) {
			t.Errorf("Describe = %v, want ErrBadPolyline", err)
		}
	})

	t.Run("reverser failure", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("nominatim unreachable")
		describer, _ := newDescriber(t, &fakeReverser{err: wantErr})

		if _, err := describer.Describe(t.Context(), "_p~iF~ps|U"); !errors.Is(err, wantErr) {
			t.Errorf("Describe = %v, want %v", err, wantErr)
		}
	})
}

func TestSummaryNamesAreDistinct(t *testing.T) {
	t.Parallel()

	summary := Summary{
		Start: store.Place{Name: "Musterdorf"},
		Along: []store.Place{
			{Name: "Musterstadt"},
			{Name: "Musterdorf"},
			{Name: ""},
			{Name: "Musterweiler"},
		},
	}

	names := summary.Names()
	want := []string{"Musterdorf", "Musterstadt", "Musterweiler"}

	if len(names) != len(want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}

	for i := range want {
		if names[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

// encodeForTest produces an encoded polyline from points, so route fixtures can
// be written as coordinates rather than as opaque strings.
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

// A byte outside the polyline alphabet, or a coordinate the format cannot
// legitimately carry, must fail rather than decode into a plausible position
// that then gets geocoded.
func TestDecodePolylineRejectsOutOfRangeInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		encoded string
	}{
		{name: "byte above the alphabet", encoded: "_p~iF~ps|U\x7f\x7f"},
		{name: "utf-8 continuation byte", encoded: "_p~iF~ps|U\xc3\xa9"},
		{name: "value wider than the format allows", encoded: "\xfe\xfe\xfe\xfe\xfe\xfe\xfe\xfe"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := DecodePolyline(tt.encoded); !errors.Is(err, ErrBadPolyline) {
				t.Errorf("DecodePolyline(%q) = %v, want ErrBadPolyline", tt.encoded, err)
			}
		})
	}
}

func TestDecodePolylineRejectsImpossibleCoordinates(t *testing.T) {
	t.Parallel()

	// A latitude past the pole, encoded legitimately.
	encoded := encodeForTest([]Point{{Lat: 91, Lon: 0}})

	if _, err := DecodePolyline(encoded); !errors.Is(err, ErrBadPolyline) {
		t.Errorf("DecodePolyline of a 91° latitude = %v, want ErrBadPolyline", err)
	}

	// The extremes themselves stay valid.
	if _, err := DecodePolyline(encodeForTest([]Point{{Lat: 90, Lon: 180}})); err != nil {
		t.Errorf("DecodePolyline of the coordinate extremes = %v, want nil", err)
	}
}

// Eight samples 110 m apart are eight distinct cache keys that routinely
// resolve to one town. Along must list it once.
func TestAlongIsDeduplicatedByName(t *testing.T) {
	t.Parallel()

	reverser := &fakeReverser{}
	describer, _ := newDescriber(t, reverser)

	points := []Point{
		{Lat: 0.000, Lon: 0.000},
		{Lat: 0.010, Lon: 0.005},
		{Lat: 0.020, Lon: -0.005},
		{Lat: 0.030, Lon: 0.010},
	}

	summary, err := describer.Describe(t.Context(), encodeForTest(points))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	// The fake resolves everything to Musterdorf, which is also the start.
	if len(summary.Along) != 0 {
		t.Errorf("Along = %+v, want empty — every sample resolved to the start's name", summary.Along)
	}

	if names := summary.Names(); len(names) != 1 || names[0] != "Musterdorf" {
		t.Errorf("Names() = %v, want [Musterdorf]", names)
	}
}

// A country-only answer contributes Country but is not a place "along" the
// route, and Empty() must not claim geography that Names() cannot produce.
func TestCountryOnlyAnswersAreNotListedAsPlaces(t *testing.T) {
	t.Parallel()

	reverser := &fakeReverser{places: map[string]store.Place{}}
	describer, _ := newDescriber(t, reverser)

	// Every key resolves to a country and nothing else.
	reverser.places = map[string]store.Place{}

	points := []Point{{Lat: 0, Lon: 0}, {Lat: 0.01, Lon: 0.01}}
	for _, p := range SamplePoints(points) {
		reverser.places[CacheKey(p)] = store.Place{Country: "Testland"}
	}

	summary, err := describer.Describe(t.Context(), encodeForTest(points))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if len(summary.Along) != 0 {
		t.Errorf("Along = %+v, want empty — no sample had a name", summary.Along)
	}
	if summary.Country != "Testland" {
		t.Errorf("Country = %q, want Testland", summary.Country)
	}
	if len(summary.Names()) != 0 {
		t.Errorf("Names() = %v, want none", summary.Names())
	}
	// Non-empty is correct here: the country did resolve. The doc comment says
	// so, and callers that need names must check Names().
	if summary.Empty() {
		t.Error("Empty() = true, but a country resolved")
	}
}

func TestEmptySummary(t *testing.T) {
	t.Parallel()

	if !(Summary{}).Empty() {
		t.Error("a zero Summary must be empty")
	}
	if (Summary{Country: "Testland"}).Empty() {
		t.Error("a summary with a country is not empty")
	}
}

// A cached place name that is not on the allow-list never reaches a title.
//
// The cache is written from filtered output today, so this is defense in
// depth rather than a live leak — but the allow-list is the privacy contract,
// and a contract enforced only where the value goes in is enforced by
// convention at the point it comes out. Tighten the list and every entry
// already cached would otherwise keep the old answer until it aged out.
func TestACachedPlaceIsRecheckedAgainstTheAllowList(t *testing.T) {
	t.Parallel()

	cache := store.NewMemory()

	// Seeded directly, as an entry written under older rules would be.
	for key, place := range map[string]store.Place{
		"poi":     {Name: "Dr Müller's Praxis", Kind: "doctors", Region: "Musterregion", Country: "Musterland"},
		"noKind":  {Name: "Somewhere", Region: "Musterregion", Country: "Musterland"},
		"village": {Name: "Musterdorf", Kind: "village", Region: "Musterregion", Country: "Musterland"},
		"river":   {Name: "Musterbach", Kind: "river", Region: "Musterregion", Country: "Musterland"},
	} {
		if err := cache.SavePlace(t.Context(), key, place); err != nil {
			t.Fatalf("SavePlace(%q): %v", key, err)
		}
	}

	describer, err := NewDescriber(refusingReverser{t}, cache, quietLogger())
	if err != nil {
		t.Fatalf("NewDescriber: %v", err)
	}

	for _, tc := range []struct {
		key      string
		wantName string
	}{
		{key: "poi", wantName: ""},
		{key: "noKind", wantName: ""},
		{key: "village", wantName: "Musterdorf"},
		{key: "river", wantName: "Musterbach"},
	} {
		got, err := describer.resolve(t.Context(), tc.key, Point{})
		if err != nil {
			t.Fatalf("resolve(%q): %v", tc.key, err)
		}

		if got.Name != tc.wantName {
			t.Errorf("resolve(%q).Name = %q, want %q", tc.key, got.Name, tc.wantName)
		}

		// The coarse fields survive either way: they are what a title falls
		// back to when there is no usable place name.
		if got.Region != "Musterregion" || got.Country != "Musterland" {
			t.Errorf("resolve(%q) lost its region or country: %+v", tc.key, got)
		}
	}
}

// refusingReverser fails if it is called, so the test can only be exercising
// the cache path.
type refusingReverser struct{ t *testing.T }

func (r refusingReverser) Reverse(_ context.Context, _ Point) (store.Place, error) {
	r.t.Error("the geocoder was called for a cached key")

	return store.Place{}, errors.New("must not be called")
}

// Every kind the allow-list can produce is accepted by the read-side check,
// so the two cannot drift into disagreeing about what is allowed.
func TestEveryAllowedKindPassesTheReadSideCheck(t *testing.T) {
	t.Parallel()

	for _, field := range addressFields {
		if !IsAllowedKind(field.kind) {
			t.Errorf("kind %q is produced by the allow-list but rejected on read", field.kind)
		}
	}

	for category, types := range naturalFeatures {
		for _, kind := range types {
			if !IsAllowedKind(kind) {
				t.Errorf("kind %q (%s) is produced by the allow-list but rejected on read",
					kind, category)
			}
		}
	}

	for _, kind := range []string{"", "doctors", "shop", "amenity", "building", "office"} {
		if IsAllowedKind(kind) {
			t.Errorf("kind %q is accepted on read but is not on the allow-list", kind)
		}
	}
}

// A Reverser that answers with a point of interest is filtered on the way in.
//
// Reverser is an interface this package publishes and does not implement
// alone, so the allow-list cannot live only inside the Nominatim client.
// Filtering on the way out of the cache is not enough either: the first
// request for a coordinate is a cache miss, so an unfiltered name would reach
// the prompt once and then be stored next to the athlete's coordinates —
// exactly what the privacy rule exists to prevent.
func TestAGeocoderCannotIntroduceAPointOfInterest(t *testing.T) {
	t.Parallel()

	poi := store.Place{
		Name:    "Dr Müller's Praxis",
		Kind:    "doctors",
		Region:  "Musterregion",
		Country: "Musterland",
	}

	cache := store.NewMemory()

	describer, err := NewDescriber(fixedReverser{poi}, cache, quietLogger())
	if err != nil {
		t.Fatalf("NewDescriber: %v", err)
	}

	// The first request, which is a cache miss.
	got, err := describer.resolve(t.Context(), "k", Point{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got.Name != "" {
		t.Errorf("a point of interest reached the caller on a cache miss: %q", got.Name)
	}

	if got.Region != "Musterregion" || got.Country != "Musterland" {
		t.Errorf("the coarse fields were lost: %+v", got)
	}

	// And it was never written to the cache, so it is not sitting in Firestore
	// next to the coordinate it describes.
	cached, ok, err := cache.Place(t.Context(), "k")
	if err != nil || !ok {
		t.Fatalf("the answer was not cached (ok=%v, err=%v)", ok, err)
	}

	if cached.Name != "" {
		t.Errorf("a point of interest was persisted: %q", cached.Name)
	}
}

// An allowed name from a custom Reverser still comes through.
func TestAGeocodersAllowedNameSurvives(t *testing.T) {
	t.Parallel()

	village := store.Place{Name: "Musterdorf", Kind: "village", Region: "Musterregion"}

	describer, err := NewDescriber(fixedReverser{village}, store.NewMemory(), quietLogger())
	if err != nil {
		t.Fatalf("NewDescriber: %v", err)
	}

	got, err := describer.resolve(t.Context(), "k", Point{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got.Name != "Musterdorf" || got.Kind != "village" {
		t.Errorf("an allowed place was altered: %+v", got)
	}
}

// fixedReverser answers with the same place every time.
type fixedReverser struct{ place store.Place }

func (f fixedReverser) Reverse(_ context.Context, _ Point) (store.Place, error) {
	return f.place, nil
}

// Strava deletes tokens from a title that look like a hostname, so a place
// name must not arrive at the prompt looking like one.
//
// Observed live on 2026-08-24: "Über Ruhstorf a.d.Rott nach Pocking" was
// stored by Strava as "Über Ruhstorf  nach Pocking" — the token excised and
// both spaces left behind. Nominatim returns the compact official form, and
// this region is full of it.
func TestNormalizePlaceName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "the observed case", in: "Ruhstorf a.d.Rott", want: "Ruhstorf a. d. Rott"},
		{name: "another river", in: "Neustadt a.d.Donau", want: "Neustadt a. d. Donau"},
		{name: "ob der", in: "Rothenburg o.d.Tauber", want: "Rothenburg o. d. Tauber"},
		{name: "a single abbreviation", in: "St.Wolfgang", want: "St. Wolfgang"},

		// Already spaced, or nothing to do.
		{name: "already correct", in: "Ruhstorf a. d. Rott", want: "Ruhstorf a. d. Rott"},
		{name: "an ordinary name", in: "Pocking", want: "Pocking"},
		{name: "a hyphenated name", in: "Bad Griesbach-Therme", want: "Bad Griesbach-Therme"},
		{name: "empty", in: "", want: ""},

		// A trailing period ends a sentence rather than joining two letters.
		{name: "trailing period", in: "Sankt Wolfgang i.", want: "Sankt Wolfgang i."},

		// Digits are left alone: a decimal is not an abbreviation.
		{name: "a decimal", in: "Kilometer 12.5", want: "Kilometer 12.5"},
		// Left alone: the rule needs a letter on both sides, so a run
		// containing digits is untouched even though it has the shape. A
		// decimal in a name is likelier than a numeric hostname, and the
		// consequence of splitting one is worse.
		{name: "digits in the run", in: "B.12.3", want: "B.12.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := NormalizePlaceName(tc.in); got != tc.want {
				t.Errorf("NormalizePlaceName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The normalized form is what a title would carry, so it must not look like a
// host to the filter that mangled the original.
func TestNormalizedNamesCarryNoHostnameShape(t *testing.T) {
	t.Parallel()

	hostLike := regexp.MustCompile(`\p{L}+\.\p{L}+\.\p{L}+`)

	for _, raw := range []string{
		"Ruhstorf a.d.Rott", "Neustadt a.d.Donau", "Rothenburg o.d.Tauber",
	} {
		if !hostLike.MatchString(raw) {
			t.Errorf("%q does not have the shape this is about; the test proves nothing", raw)
		}

		if got := NormalizePlaceName(raw); hostLike.MatchString(got) {
			t.Errorf("NormalizePlaceName(%q) = %q, still hostname-shaped", raw, got)
		}
	}
}

// Describe normalizes the names it hands back, not just the helper.
//
// The unit test above proves the rewrite; this proves the pipeline applies it.
// Without this, deleting the call from Describe leaves every test green while
// the prompt goes back to receiving names Strava will mangle.
func TestDescribeNormalizesResolvedNames(t *testing.T) {
	t.Parallel()

	// Every sample resolves to the same dotted name, so the assertion does not
	// depend on which point the sampler happens to pick first.
	describer, _ := newDescriber(t, dottedReverser{})

	summary, err := describer.Describe(t.Context(), "_p~iF~ps|U_ulLnnqC_mqNvxq`@")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	names := summary.Names()
	if len(names) == 0 {
		t.Fatal("Names() is empty")
	}

	if names[0] != "Ruhstorf a. d. Rott" {
		t.Errorf("start name = %q, want the spaced form", names[0])
	}

	// The region travels to the prompt too, on its own field.
	if summary.Region != "Nieder. Bayern" {
		t.Errorf("Region = %q, want the spaced form", summary.Region)
	}
}

// dottedReverser answers every point with a name in the compact official form
// Nominatim returns for these places.
type dottedReverser struct{}

func (dottedReverser) Reverse(_ context.Context, _ Point) (store.Place, error) {
	return store.Place{
		Name: "Ruhstorf a.d.Rott", Kind: "village",
		Region: "Nieder.Bayern", Country: "Testland",
	}, nil
}
