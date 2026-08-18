package geo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
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
