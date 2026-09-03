// Package geo turns a route into verified place names.
//
// It decodes Strava's summary polyline, samples a handful of points from it,
// and reverse-geocodes them through Nominatim, caching every answer.
//
// What it returns carries no coordinates. That is the point: the naming layer
// is handed [Summary], so it cannot build a title from a position even by
// accident, and the names it does receive are limited to settlements, regions
// and natural features — never a point of interest the athlete visited.
package geo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"

	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/store"
)

// cachePrecision is the number of decimal places a coordinate is rounded to
// before it becomes a cache key: about 110 m, which is close enough that two
// points that near each other resolve to the same settlement.
const cachePrecision = 3

// DefaultSampleCount is how many points along the route are sampled between
// the start and the farthest point.
const DefaultSampleCount = 6

// MaxSampleCount bounds that count.
//
// The start and the farthest point are sampled on top of it, so this is what
// keeps one activity to at most eight reverse-geocoding requests — eight
// seconds of the rate-limit budget, spent while an athlete waits for a title.
const MaxSampleCount = 6

// earthRadiusMeters is the mean radius the haversine distance is scaled by.
const earthRadiusMeters = 6371000

// Summary is the verified geography of one route.
//
// Every field is a name. There is deliberately nowhere to put a coordinate.
type Summary struct {
	// Start is where the route began.
	Start store.Place

	// Along holds the other resolved places, in sample order, deduplicated by
	// name. Every entry has a name: a sample that resolved only to a country
	// contributes to Country and Region and is not listed here.
	Along []store.Place

	// Region and Country are the coarsest containers seen on the route.
	Region  string
	Country string
}

// Empty reports whether nothing at all was resolved.
//
// A summary can be non-empty and still have no names — a ride over open water
// resolves to a country and nothing else. Callers that need names should use
// [Summary.Names] and check its length rather than inferring it from here.
func (s Summary) Empty() bool {
	return s.Start.Empty() && len(s.Along) == 0 && s.Region == "" && s.Country == ""
}

// Names returns every distinct place name, start first. This is what a prompt
// builder passes to an LLM.
func (s Summary) Names() []string {
	names := make([]string, 0, len(s.Along)+1)
	seen := make(map[string]struct{}, len(s.Along)+1)

	for _, place := range append([]store.Place{s.Start}, s.Along...) {
		if place.Name == "" {
			continue
		}

		if _, ok := seen[place.Name]; ok {
			continue
		}

		seen[place.Name] = struct{}{}
		names = append(names, place.Name)
	}

	return names
}

// dottedAbbreviation matches a period sitting directly between two letters.
//
// Digits are excluded so a name carrying a decimal is left alone.
var dottedAbbreviation = regexp.MustCompile(`(\p{L})\.(\p{L})`)

// NormalizePlaceName spaces out dotted abbreviations in a place name.
//
// Strava deletes hostname-shaped tokens from a title, and Nominatim returns
// the compact official form: "Ruhstorf a.d.Rott" is label.label.label, so a
// title carrying it comes back with the town removed. Spacing the periods is
// also the correct German typography.
//
// Matched on the shape rather than on a list of German abbreviations —
// anything genuinely URL-like has no business in a title either — and
// repeated until stable, because "a.d.Rott" needs a second pass over the "d"
// the first one consumed.
func NormalizePlaceName(name string) string {
	for {
		spaced := dottedAbbreviation.ReplaceAllString(name, "$1. $2")
		if spaced == name {
			return name
		}

		name = spaced
	}
}

// Reverser resolves one coordinate into a place. [Nominatim] implements it;
// tests substitute their own.
type Reverser interface {
	Reverse(ctx context.Context, point Point) (store.Place, error)
}

// Describer turns an encoded polyline into verified place names.
type Describer struct {
	reverser    Reverser
	cache       store.GeocodeCache
	logger      *slog.Logger
	sampleCount int
}

// DescriberConfig configures a [Describer].
type DescriberConfig struct {
	// Reverser resolves one coordinate. Required.
	Reverser Reverser

	// Cache is required: Nominatim's usage policy expects results to be
	// cached rather than refetched.
	Cache store.GeocodeCache

	// Logger defaults to [slog.Default].
	Logger *slog.Logger

	// SampleCount is how many points between the start and the farthest
	// point are geocoded. Zero means [DefaultSampleCount], and anything
	// above [MaxSampleCount] is refused rather than clamped: the request
	// budget per activity is a property of the code, not of the
	// configuration that happens to be deployed.
	SampleCount int
}

// NewDescriber builds a describer.
func NewDescriber(cfg DescriberConfig) (*Describer, error) {
	if cfg.Reverser == nil || cfg.Cache == nil {
		return nil, errors.New("geo: a reverser and a cache are required")
	}

	if cfg.SampleCount < 0 || cfg.SampleCount > MaxSampleCount {
		return nil, fmt.Errorf("geo: sample count %d is outside 1 to %d (0 means %d)",
			cfg.SampleCount, MaxSampleCount, DefaultSampleCount)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Describer{
		reverser:    cfg.Reverser,
		cache:       cfg.Cache,
		logger:      logger,
		sampleCount: cfg.SampleCount,
	}, nil
}

// Describe resolves the geography of an encoded polyline.
//
// A route with no polyline — an indoor ride, or one recorded without GPS — is
// not an error: it yields an empty summary, and the caller decides what that
// means.
func (d *Describer) Describe(ctx context.Context, encodedPolyline string) (Summary, error) {
	if encodedPolyline == "" {
		return Summary{}, nil
	}

	points, err := DecodePolyline(encodedPolyline)
	if err != nil {
		return Summary{}, err
	}

	if len(points) == 0 {
		return Summary{}, nil
	}

	samples := SamplePoints(points, d.sampleCount)

	var (
		summary   Summary
		seen      = make(map[string]struct{}, len(samples))
		seenNames = make(map[string]struct{}, len(samples))
	)

	for index, point := range samples {
		key := CacheKey(point)

		// Dedupe on the cache key, not on the raw coordinate: an out-and-back
		// puts the outward and the homeward sample within the same rounded
		// key, and geocoding it twice spends a second of the rate-limit
		// budget for nothing.
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		place, err := d.resolve(ctx, key, point)
		if err != nil {
			return Summary{}, err
		}

		// Normalized here, once, so everything downstream — the names, the
		// region, the country, and the deduplication below — works on the
		// form a title can actually carry.
		place.Name = NormalizePlaceName(place.Name)
		place.Region = NormalizePlaceName(place.Region)
		place.Country = NormalizePlaceName(place.Country)

		if place.Empty() {
			continue
		}

		if summary.Region == "" {
			summary.Region = place.Region
		}
		if summary.Country == "" {
			summary.Country = place.Country
		}

		if place.Name == "" {
			continue
		}

		if index == 0 {
			summary.Start = place
			seenNames[place.Name] = struct{}{}

			continue
		}

		// Deduplicate on the resolved name, not just on the cache key. Eight
		// samples 110 m apart are eight distinct keys that routinely resolve to
		// one town, and a caller iterating Along would otherwise render it
		// eight times.
		if _, ok := seenNames[place.Name]; ok {
			continue
		}

		seenNames[place.Name] = struct{}{}
		summary.Along = append(summary.Along, place)
	}

	return summary, nil
}

// allowedOnly strips a place name that is not on the allow-list.
//
// Applied to every place this package hands back, whichever side it came
// from. The allow-list is the privacy contract, and a contract enforced only
// where a value is produced is enforced by convention everywhere it is used:
// a cached entry was filtered by whatever rules were in force when it was
// written, and a Reverser this package did not write was never filtered at
// all. [placeFrom] never sets a name without the Kind that justifies it, so
// the Kind is enough to re-check without the original payload.
//
// Only the name goes. Region and Country are coarse by construction, and are
// what a title falls back to when there is no usable place name.
func (d *Describer) allowedOnly(place store.Place, source string) store.Place {
	if place.Name == "" || IsAllowedKind(place.Kind) {
		return place
	}

	d.logger.Warn("dropped a place name that is not on the allow-list",
		"source", source, "kind", logsafe.String(place.Kind))

	place.Name = ""
	place.Kind = ""

	return place
}

// resolve returns a place from the cache, or fetches and caches it.
func (d *Describer) resolve(ctx context.Context, key string, point Point) (store.Place, error) {
	cached, ok, err := d.cache.Place(ctx, key)
	if err != nil {
		return store.Place{}, fmt.Errorf("geo: read geocode cache: %w", err)
	}

	if ok {
		return d.allowedOnly(cached, "cache"), nil
	}

	place, err := d.reverser.Reverse(ctx, point)
	if err != nil {
		return store.Place{}, err
	}

	// Filtered before it is stored, not only before it is returned. Reverser
	// is an interface this package publishes and does not implement alone, so
	// an implementation that answers with a point of interest must not get its
	// answer persisted next to the athlete's coordinates — the cache would
	// then hold exactly the thing the privacy rule exists to keep out of it.
	place = d.allowedOnly(place, "geocoder")

	// Empty answers are cached too. Nominatim has nothing to say about the
	// middle of a lake, and asking again every time would spend the budget on a
	// question already answered.
	if err := d.cache.SavePlace(ctx, key, place); err != nil {
		return store.Place{}, fmt.Errorf("geo: write geocode cache: %w", err)
	}

	d.logger.Debug("resolved a place",
		"kind", logsafe.String(place.Kind), "name", logsafe.String(place.Name))

	return place, nil
}

// CacheKey rounds a coordinate into the key its cached place is stored under.
//
// The key is the only place a coordinate is ever persisted, and it is rounded
// to roughly 110 m.
func CacheKey(point Point) string {
	return strconv.FormatFloat(round(point.Lat), 'f', cachePrecision, 64) + "," +
		strconv.FormatFloat(round(point.Lon), 'f', cachePrecision, 64)
}

func round(value float64) float64 {
	scale := 1.0
	for range cachePrecision {
		scale *= 10
	}

	rounded := math.Round(value*scale) / scale

	// Avoid a negative zero in the key, so -0.000 and 0.000 do not become two
	// cache entries for one place.
	if rounded == 0 {
		return 0
	}

	return rounded
}

// SamplePoints picks the points worth geocoding: the start, count points at
// equal arc length along the track, and the point farthest from the start.
// A count of zero or less means [DefaultSampleCount].
//
// Spacing is by distance traveled rather than by index. A summary polyline
// carries its vertices wherever the track bends, so index spacing follows the
// shape of the simplification and not the shape of the ride: a loop with a
// dense section puts every index-spaced sample inside it, and the rest of the
// route is never asked about.
//
// The start comes first because it is the only sample whose position in the
// result matters.
func SamplePoints(points []Point, count int) []Point {
	if len(points) == 0 {
		return nil
	}

	if count <= 0 {
		count = DefaultSampleCount
	}

	start := points[0]
	samples := []Point{start}

	cumulative, total := arcLengths(points)

	// A track that stands still has no arc to spread samples along, and
	// dividing by its length would put every sample on the start anyway.
	if total > 0 {
		for step := 1; step <= count; step++ {
			samples = append(samples, pointAtArc(points, cumulative, float64(step)*total/float64(count+1)))
		}
	}

	samples = append(samples, farthestFrom(start, points))

	return dedupePoints(samples)
}

// arcLengths returns the cumulative distance to each point and the total.
func arcLengths(points []Point) ([]float64, float64) {
	cumulative := make([]float64, len(points))

	var total float64

	for index := 1; index < len(points); index++ {
		total += Distance(points[index-1], points[index])
		cumulative[index] = total
	}

	return cumulative, total
}

// pointAtArc returns the position at the given arc length, interpolated within
// the segment that contains it.
//
// Interpolated rather than snapped to the nearer vertex: a simplified track
// has long straight segments, and snapping would collapse several samples onto
// one vertex and leave whole stretches of the ride unsampled. Every point on
// the segment is on the track the polyline describes.
func pointAtArc(points []Point, cumulative []float64, target float64) Point {
	for index := 1; index < len(points); index++ {
		span := cumulative[index] - cumulative[index-1]
		if cumulative[index] < target || span <= 0 {
			continue
		}

		fraction := (target - cumulative[index-1]) / span
		from, to := points[index-1], points[index]

		return Point{
			Lat: from.Lat + (to.Lat-from.Lat)*fraction,
			Lon: wrapLongitude(from.Lon + shortestLonDelta(from.Lon, to.Lon)*fraction),
		}
	}

	return points[len(points)-1]
}

// shortestLonDelta is the signed longitude difference along the shorter way
// around the globe.
//
// A segment is the track between two vertices, and the track takes the shorter
// way: a raw delta past 180° describes the longer one, which crosses every
// meridian the ride never saw. [Distance] measures the same segment the same
// way, so an arc length and the position it resolves to agree.
func shortestLonDelta(from, to float64) float64 {
	delta := to - from

	if delta > 180 {
		return delta - 360
	}

	if delta < -180 {
		return delta + 360
	}

	return delta
}

// wrapLongitude brings a longitude back into [-180, 180].
//
// One adjustment is enough: the inputs are a longitude already in range and a
// delta of at most 180°.
func wrapLongitude(lon float64) float64 {
	if lon > 180 {
		return lon - 360
	}

	if lon < -180 {
		return lon + 360
	}

	return lon
}

// farthestFrom returns the track point farthest from the start, which is the
// turning point of an out-and-back and the far side of a loop. Ties go to the
// first, so the result does not depend on iteration order.
func farthestFrom(start Point, points []Point) Point {
	farthest, distance := start, 0.0

	for _, point := range points {
		if d := Distance(start, point); d > distance {
			farthest, distance = point, d
		}
	}

	return farthest
}

// dedupePoints drops repeats while keeping the first occurrence, so the start
// of a loop is not geocoded twice as its own farthest point.
func dedupePoints(points []Point) []Point {
	seen := make(map[Point]struct{}, len(points))
	unique := points[:0]

	for _, point := range points {
		if _, ok := seen[point]; ok {
			continue
		}

		seen[point] = struct{}{}
		unique = append(unique, point)
	}

	return unique
}

// Distance is the great-circle distance between two points, in meters.
func Distance(from, to Point) float64 {
	const degreesToRadians = math.Pi / 180

	lat1 := from.Lat * degreesToRadians
	lat2 := to.Lat * degreesToRadians
	deltaLat := (to.Lat - from.Lat) * degreesToRadians
	deltaLon := (to.Lon - from.Lon) * degreesToRadians

	sinLat := math.Sin(deltaLat / 2)
	sinLon := math.Sin(deltaLon / 2)

	a := sinLat*sinLat + math.Cos(lat1)*math.Cos(lat2)*sinLon*sinLon

	return 2 * earthRadiusMeters * math.Asin(math.Min(1, math.Sqrt(a)))
}
