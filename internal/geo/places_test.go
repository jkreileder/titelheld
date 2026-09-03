package geo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jkreileder/titelheld/internal/store"
)

// Every coordinate in this file is synthetic and every place name is invented.
//
// The fixture is a loop whose vertices are distributed the way a simplified
// summary polyline distributes them: a dense cluster where the track twists at
// the start, and one vertex per bend over the kilometers between the places
// worth naming. Index-spaced sampling asks about the cluster and nothing else;
// equal-arc sampling asks about the ride.
const (
	// The cluster holds most of the vertices and almost none of the length.
	clusterVertices = 72

	// turnLon is the far end of the loop, where the outward leg becomes the
	// homeward one.
	turnLon = 0.130

	// homewardLat separates the two legs. The homeward leg runs north of the
	// outward one, so the recorded responses can differ along it.
	homewardLat = 0.0100
	outwardLat  = 0.0010

	// legBoundary is the latitude a point is read as homeward above.
	legBoundary = 0.005

	// startLon is where both fixture tracks begin, and where the loop ends.
	startLon = 0.0010

	// oneWayFinishLon is where the one-way track stops, far enough east that
	// it is also its farthest point from the start.
	oneWayFinishLon = 0.150
)

// The names each band answers with. Each appears in exactly one recorded
// response, so "this name is absent from the summary" is a claim about the one
// point that could have produced it.
const (
	townName         = "Musterstadt"
	hamletName       = "Musterweiler"
	hamletTownName   = "Andersstadt"
	suburbName       = "Mustervorstadt"
	suburbCityName   = "Grossmusterstadt"
	suburbUnionName  = "Musterverbund"
	municipalityName = "Musterverband"
	villageName      = "Musterdorf"

	// The names the endpoint cells answer with. Nothing else in the fixture
	// resolves to either, so their absence from a summary is a claim about the
	// two points a public title may never carry: km 0 and km end.
	startCellName  = "Musterheim"
	finishCellName = "Musterende"
)

// endpointCells maps a rounded cell to the name it answers with. The cells are
// where the fixture tracks begin and end.
//
// The answer is a village, which is an allow-listed kind the finest-first
// order asks for first: a sampler that asked about an endpoint would put the
// name straight into the summary, which is what makes its absence evidence.
var endpointCells = map[[2]float64]string{
	{round(outwardLat), round(startLon)}:        startCellName,
	{round(outwardLat), round(oneWayFinishLon)}: finishCellName,
}

// cellOf is the rounded cell a point falls in — the same rounding the cache
// key uses, so the fixture speaks about points the way the sampler does.
func cellOf(point Point) [2]float64 {
	return [2]float64{round(point.Lat), round(point.Lon)}
}

// loopTrack is the synthetic ride: a cluster at the start, an outward leg east
// through the bands, and a homeward leg to the north of it.
func loopTrack() []Point {
	points := make([]Point, 0, clusterVertices+16)

	points = append(points, startCluster()...)

	// Outward, east, with a vertex only where the track would bend.
	for _, lon := range []float64{0.010, 0.040, 0.075, 0.100, turnLon} {
		points = append(points, Point{Lat: outwardLat, Lon: lon})
	}

	// Homeward, north of the outward leg, back to the start.
	for _, lon := range []float64{turnLon, 0.100, 0.075, 0.040, 0.010, startLon} {
		points = append(points, Point{Lat: homewardLat, Lon: lon})
	}

	return append(points, points[0])
}

// oneWayTrack is the ride that ends where it turned around on the loop: a
// point-to-point track whose farthest point from the start is its last point.
func oneWayTrack() []Point {
	points := startCluster()

	for _, lon := range []float64{0.010, 0.040, 0.075, 0.100, turnLon, oneWayFinishLon} {
		points = append(points, Point{Lat: outwardLat, Lon: lon})
	}

	return points
}

// startCluster is the twisting start inside one settlement: a few hundred
// meters of track and most of the vertices.
func startCluster() []Point {
	points := make([]Point, 0, clusterVertices)

	for index := range clusterVertices {
		wiggle := float64(index%4) * 0.00005

		points = append(points, Point{Lat: outwardLat + wiggle/4, Lon: startLon + wiggle})
	}

	return points
}

// bandResponse is the recorded Nominatim reply for the band a point falls in.
// Hand-written: no test here has ever spoken to a live geocoder.
func bandResponse(point Point) string {
	// The endpoints answer for themselves, before any band.
	if name, ok := endpointCells[cellOf(point)]; ok {
		return `{"category":"place","type":"village","name":"` + name + `",
			"address":{"village":"` + name + `","state":"Musterregion","country":"Testland"}}`
	}

	// The cluster and the closing stretch: one town, whichever leg.
	if point.Lon < 0.020 {
		return `{"category":"place","type":"town","name":"` + townName + `",
			"address":{"town":"` + townName + `","state":"Musterregion","country":"Testland"}}`
	}

	hamlet := `{"category":"place","type":"hamlet","name":"` + hamletName + `",
		"address":{"hamlet":"` + hamletName + `","town":"` + hamletTownName + `",
		"state":"Musterregion","country":"Testland"}}`

	// A suburb inside a city inside a municipal union: three names for one
	// point, and only the finest of them is where the ride was.
	suburb := `{"category":"place","type":"suburb","name":"` + suburbName + `",
		"address":{"suburb":"` + suburbName + `","city":"` + suburbCityName + `",
		"municipality":"` + suburbUnionName + `","state":"Musterregion","country":"Testland"}}`

	if point.Lat < legBoundary {
		switch {
		case point.Lon < 0.050:
			return hamlet
		case point.Lon < 0.090:
			return suburb
		default:
			return `{"category":"place","type":"village","name":"` + villageName + `",
				"address":{"village":"` + villageName + `","state":"Musterregion",
				"country":"Testland"}}`
		}
	}

	switch {
	case point.Lon > 0.090:
		// Open country: the municipality is the finest thing reported here.
		return `{"category":"place","type":"municipality","name":"` + municipalityName + `",
			"address":{"municipality":"` + municipalityName + `","state":"Musterregion",
			"country":"Testland"}}`
	case point.Lon > 0.050:
		return suburb
	default:
		return hamlet
	}
}

// recordingGeocoder is a Nominatim stand-in that answers from the recorded
// bodies above and keeps every query it was sent.
type recordingGeocoder struct {
	server  *httptest.Server
	mu      sync.Mutex
	queries []url.Values
}

func newRecordingGeocoder(t *testing.T) *recordingGeocoder {
	t.Helper()

	recorder := &recordingGeocoder{}

	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()

		recorder.mu.Lock()
		recorder.queries = append(recorder.queries, query)
		recorder.mu.Unlock()

		lat, latErr := strconv.ParseFloat(query.Get("lat"), 64)
		lon, lonErr := strconv.ParseFloat(query.Get("lon"), 64)

		if latErr != nil || lonErr != nil {
			t.Errorf("the geocoder was asked about %q,%q, which is not a coordinate",
				query.Get("lat"), query.Get("lon"))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bandResponse(Point{Lat: lat, Lon: lon})))
	}))

	t.Cleanup(recorder.server.Close)

	return recorder
}

func (r *recordingGeocoder) recorded() []url.Values {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.queries)
}

// describeLoop runs the whole path — sample, geocode, filter, summarize — over
// the synthetic loop, against the recorded responses.
func describeLoop(t *testing.T, nominatim NominatimConfig, sampleCount int) (Summary, *recordingGeocoder) {
	t.Helper()

	return describeTrack(t, nominatim, sampleCount, loopTrack())
}

// describeTrack is describeLoop over any of the fixture tracks.
func describeTrack(
	t *testing.T, nominatim NominatimConfig, sampleCount int, track []Point,
) (Summary, *recordingGeocoder) {
	t.Helper()

	recorder := newRecordingGeocoder(t)

	nominatim.UserAgent = "titelheld-test/1.0 (test@example.invalid)"
	nominatim.BaseURL = recorder.server.URL
	nominatim.HTTPClient = recorder.server.Client()
	nominatim.Sleep = func(context.Context, time.Duration) error { return nil }

	client, err := NewNominatim(nominatim)
	if err != nil {
		t.Fatalf("NewNominatim: %v", err)
	}

	describer, err := NewDescriber(DescriberConfig{
		Reverser:    client,
		Cache:       store.NewMemory(),
		Logger:      quietLogger(),
		SampleCount: sampleCount,
	})
	if err != nil {
		t.Fatalf("NewDescriber: %v", err)
	}

	summary, err := describer.Describe(t.Context(), encodeForTest(track))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	return summary, recorder
}

// The samples spread over the ride, so the summary carries the places it
// crossed rather than the one its vertices happen to bunch in.
func TestPlacesSpreadOverTheRoute(t *testing.T) {
	t.Parallel()

	summary, recorder := describeLoop(t, NominatimConfig{}, 0)

	names := summary.Names()
	if len(names) < 3 {
		t.Fatalf("Names() = %v, want at least three distinct settlements", names)
	}

	for _, want := range []string{hamletName, suburbName, municipalityName, villageName} {
		if !slices.Contains(names, want) {
			t.Errorf("Names() = %v, missing %q", names, want)
		}
	}

	// The budget the equal-arc scheme is bounded by: the athlete waits a
	// second per request. The loop's farthest point is interior, so this is
	// the expensive shape — the interior samples plus that one.
	if calls := len(recorder.recorded()); calls != DefaultSampleCount+1 {
		t.Errorf("one activity cost %d reverse-geocoding requests, want %d",
			calls, DefaultSampleCount+1)
	}
}

// The two endpoints leave the candidate set entirely: a title is public, and a
// settlement at km 0 or km end is where the athlete lives.
//
// The loop begins and ends in the same cell, and that cell answers with a name
// no other point in the fixture reports. Both the summary and the wire are
// asserted, so neither a name filtered on the way out nor a request that was
// nevertheless sent passes.
func TestTheEndpointsAreNeverGeocoded(t *testing.T) {
	t.Parallel()

	summary, recorder := describeLoop(t, NominatimConfig{}, 0)

	if slices.Contains(summary.Names(), startCellName) {
		t.Errorf("Names() = %v, carries %q — the name of the cell the ride started and finished in",
			summary.Names(), startCellName)
	}

	assertNoRequestInCell(t, recorder, cellOf(Point{Lat: outwardLat, Lon: startLon}))
}

// On a one-way ride the farthest point from the start is the last point, and
// it is dropped rather than replaced: the finish is an endpoint like any other.
func TestAOneWayRideNeverNamesItsFinish(t *testing.T) {
	t.Parallel()

	summary, recorder := describeTrack(t, NominatimConfig{}, 0, oneWayTrack())

	for _, unwanted := range []string{startCellName, finishCellName} {
		if slices.Contains(summary.Names(), unwanted) {
			t.Errorf("Names() = %v, carries %q — an endpoint of a one-way ride",
				summary.Names(), unwanted)
		}
	}

	assertNoRequestInCell(t, recorder, cellOf(Point{Lat: outwardLat, Lon: oneWayFinishLon}))

	// The ride still has geography: dropping the endpoints costs the finish,
	// not the route.
	if len(summary.Names()) < 3 {
		t.Errorf("Names() = %v, want the settlements the ride crossed", summary.Names())
	}
}

func assertNoRequestInCell(t *testing.T, recorder *recordingGeocoder, cell [2]float64) {
	t.Helper()

	queries := recorder.recorded()
	if len(queries) == 0 {
		t.Fatal("nothing was asked of the geocoder")
	}

	for index, query := range queries {
		lat, _ := strconv.ParseFloat(query.Get("lat"), 64)
		lon, _ := strconv.ParseFloat(query.Get("lon"), 64)

		if cellOf(Point{Lat: lat, Lon: lon}) == cell {
			t.Errorf("request %d asked about %v, which is the cell the ride began or ended in",
				index, cell)
		}
	}
}

// A hamlet inside a town is named as the hamlet, and the container it sits in
// is not a place this ride visited.
func TestTheFinestSettlementWins(t *testing.T) {
	t.Parallel()

	summary, _ := describeLoop(t, NominatimConfig{}, 0)

	names := summary.Names()

	for _, want := range []string{hamletName, suburbName} {
		if !slices.Contains(names, want) {
			t.Errorf("Names() = %v, missing the finest name %q at that point", names, want)
		}
	}

	// Each of these was reported alongside a finer field that must have won.
	for _, coarser := range []string{hamletTownName, suburbCityName, suburbUnionName} {
		if slices.Contains(names, coarser) {
			t.Errorf("Names() = %v, carries %q — a coarser field beat the finer one at that point",
				names, coarser)
		}
	}

	// The kind travels with the name, so the allow-list check on the way out
	// of the cache knows what it is looking at.
	for _, place := range summary.Along {
		if place.Name == hamletName && place.Kind != "hamlet" {
			t.Errorf("%q was recorded as a %q, want a hamlet", place.Name, place.Kind)
		}

		if place.Name == suburbName && place.Kind != "suburb" {
			t.Errorf("%q was recorded as a %q, want a suburb", place.Name, place.Kind)
		}
	}
}

// A municipality is a container several settlements share, and it is the right
// answer only where the point has nothing finer to report.
func TestMunicipalityIsTheLastResort(t *testing.T) {
	t.Parallel()

	summary, _ := describeLoop(t, NominatimConfig{}, 0)

	found := false

	for _, place := range summary.Along {
		if place.Name != municipalityName {
			continue
		}

		found = true

		if place.Kind != "municipality" {
			t.Errorf("%q was recorded as a %q, want a municipality", place.Name, place.Kind)
		}
	}

	if !found {
		t.Errorf("Along = %+v, missing %q — the open-country point resolved to nothing",
			summary.Along, municipalityName)
	}
}

// The resolution order is configuration, and the order this service used
// before stays reachable: the same responses resolve to the coarser names
// under it.
func TestTheResolutionOrderIsConfigurable(t *testing.T) {
	t.Parallel()

	summary, _ := describeLoop(t, NominatimConfig{
		PlaceFields: []string{"village", "hamlet", "town", "city", "municipality", "suburb"},
	}, 0)

	names := summary.Names()

	if slices.Contains(names, suburbName) {
		t.Errorf("Names() = %v, carries the suburb — the configured order asks for the city first",
			names)
	}

	if !slices.Contains(names, suburbCityName) {
		t.Errorf("Names() = %v, missing %q, which the configured order ranks first",
			names, suburbCityName)
	}
}

// The zoom reaches the wire. A coarser response carries no hamlet key at all,
// so an order that prefers one would have nothing to prefer.
func TestZoomIsSentAndConfigurable(t *testing.T) {
	t.Parallel()

	t.Run("default", func(t *testing.T) {
		t.Parallel()

		_, recorder := describeLoop(t, NominatimConfig{}, 0)

		assertZoom(t, recorder, strconv.Itoa(DefaultZoom))
	})

	t.Run("configured", func(t *testing.T) {
		t.Parallel()

		_, recorder := describeLoop(t, NominatimConfig{Zoom: 12}, 0)

		assertZoom(t, recorder, "12")
	})
}

func assertZoom(t *testing.T, recorder *recordingGeocoder, want string) {
	t.Helper()

	queries := recorder.recorded()
	if len(queries) == 0 {
		t.Fatal("nothing was asked of the geocoder")
	}

	for index, query := range queries {
		if got := query.Get("zoom"); got != want {
			t.Errorf("request %d asked for zoom %q, want %q", index, got, want)
		}
	}
}

// The sample count is configuration, and it decides what one activity costs.
func TestSampleCountReachesTheRequests(t *testing.T) {
	t.Parallel()

	_, recorder := describeLoop(t, NominatimConfig{}, 2)

	// Two samples along the way, and the far side of the loop.
	if calls := len(recorder.recorded()); calls != 3 {
		t.Errorf("a sample count of 2 cost %d requests, want 3", calls)
	}
}

// The rules a resolution order is held to live here, once, and a bad order is
// refused before it reaches a request.
func TestValidatePlaceFields(t *testing.T) {
	t.Parallel()

	if err := ValidatePlaceFields(nil); err != nil {
		t.Errorf("ValidatePlaceFields(nil) = %v, want nil — an empty order means the shipped one", err)
	}

	// Every key the allow-list carries is an order this service accepts, so
	// the validator and the table it validates against cannot disagree.
	if err := ValidatePlaceFields(placeFieldKeys()); err != nil {
		t.Errorf("the full set of address keys was refused: %v", err)
	}

	// The shipped order passes its own validator.
	if err := ValidatePlaceFields(defaultPlaceFields); err != nil {
		t.Errorf("the shipped order was refused: %v", err)
	}

	// An order is set as one string, so every offending key is named at once.
	err := ValidatePlaceFields([]string{"hamlet", "road", "house_number"})
	if err == nil {
		t.Fatal("a road and a house number were accepted as place fields")
	}

	for _, want := range []string{"road", "house_number"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not name %q", err, want)
		}
	}

	// A duplicate and an unknown key in one order are both reported. The
	// duplicate is asserted through the phrase that names it: the error lists
	// every valid key, so the name alone appears in it either way.
	err = ValidatePlaceFields([]string{"hamlet", "road", "hamlet"})
	if err == nil {
		t.Fatal("a duplicated key alongside an unknown one was accepted")
	}

	for _, want := range []string{"road", "listed twice: hamlet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not report %q", err, want)
		}
	}
}

// A bad order refuses the client rather than being silently skipped where the
// order is applied.
func TestNewNominatimRefusesABadOrder(t *testing.T) {
	t.Parallel()

	for name, order := range map[string][]string{
		"unknown key": {"hamlet", "road"},
		"duplicate":   {"hamlet", "village", "hamlet"},
	} {
		_, err := NewNominatim(NominatimConfig{
			UserAgent:   "titelheld-test/1.0 (test@example.invalid)",
			PlaceFields: order,
		})
		if err == nil {
			t.Errorf("an order with a %s was accepted", name)
		}
	}
}

// Nominatim has no parameter for the resolution order — the order is applied
// to the response, not asked for — so the request carries the zoom and nothing
// else about naming. Pinned here so a future parameter is a deliberate change.
func TestTheRequestCarriesNoOrderParameter(t *testing.T) {
	t.Parallel()

	_, recorder := describeLoop(t, NominatimConfig{}, 0)

	queries := recorder.recorded()
	if len(queries) == 0 {
		t.Fatal("nothing was asked of the geocoder")
	}

	want := []string{
		"accept-language", "addressdetails", "extratags", "format", "lat", "lon",
		"namedetails", "zoom",
	}

	got := make([]string, 0, len(queries[0]))
	for key := range queries[0] {
		got = append(got, key)
	}

	slices.Sort(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the request carried %v, want %v", got, want)
	}
}
