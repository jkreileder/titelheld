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
	"strconv"

	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/store"
)

// cachePrecision is the number of decimal places a coordinate is rounded to
// before it becomes a cache key: about 110 m, which is close enough that two
// points that near each other resolve to the same settlement.
const cachePrecision = 3

// waypointCount is how many evenly spaced points along the route are sampled,
// on top of the start and the bounding-box extremes.
const waypointCount = 3

// Summary is the verified geography of one route.
//
// Every field is a name. There is deliberately nowhere to put a coordinate.
type Summary struct {
	// Start is where the route began.
	Start store.Place

	// Along holds the other resolved places, in sample order, deduplicated.
	Along []store.Place

	// Region and Country are the coarsest containers seen on the route.
	Region  string
	Country string
}

// Empty reports whether nothing was resolved.
func (s Summary) Empty() bool {
	return s.Start.Empty() && len(s.Along) == 0
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

// Reverser resolves one coordinate into a place. [Nominatim] implements it;
// tests substitute their own.
type Reverser interface {
	Reverse(ctx context.Context, point Point) (store.Place, error)
}

// Describer turns an encoded polyline into verified place names.
type Describer struct {
	reverser Reverser
	cache    store.GeocodeCache
	logger   *slog.Logger
}

// NewDescriber builds a describer. The cache is required: Nominatim's usage
// policy expects results to be cached rather than refetched.
func NewDescriber(reverser Reverser, cache store.GeocodeCache, logger *slog.Logger) (*Describer, error) {
	if reverser == nil || cache == nil {
		return nil, errors.New("geo: a reverser and a cache are required")
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &Describer{reverser: reverser, cache: cache, logger: logger}, nil
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

	samples := SamplePoints(points)

	var (
		summary Summary
		seen    = make(map[string]struct{}, len(samples))
	)

	for index, point := range samples {
		key := CacheKey(point)

		// Dedupe on the cache key, not on the raw coordinate: an out-and-back
		// puts two bounding-box extremes on the same spot, and geocoding it
		// twice spends a second of the rate-limit budget for nothing.
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		place, err := d.resolve(ctx, key, point)
		if err != nil {
			return Summary{}, err
		}

		if place.Empty() {
			continue
		}

		if index == 0 {
			summary.Start = place
		} else {
			summary.Along = append(summary.Along, place)
		}

		if summary.Region == "" {
			summary.Region = place.Region
		}
		if summary.Country == "" {
			summary.Country = place.Country
		}
	}

	return summary, nil
}

// resolve returns a place from the cache, or fetches and caches it.
func (d *Describer) resolve(ctx context.Context, key string, point Point) (store.Place, error) {
	cached, ok, err := d.cache.Place(ctx, key)
	if err != nil {
		return store.Place{}, fmt.Errorf("geo: read geocode cache: %w", err)
	}

	if ok {
		return cached, nil
	}

	place, err := d.reverser.Reverse(ctx, point)
	if err != nil {
		return store.Place{}, err
	}

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

	rounded := float64(int64(value*scale+copysign(0.5, value))) / scale

	// Avoid a negative zero in the key, so -0.000 and 0.000 do not become two
	// cache entries for one place.
	if rounded == 0 {
		return 0
	}

	return rounded
}

func copysign(magnitude, sign float64) float64 {
	if sign < 0 {
		return -magnitude
	}

	return magnitude
}

// SamplePoints picks the points worth geocoding: the start, the four
// bounding-box extremes, and [waypointCount] evenly spaced points along the
// route.
//
// The start comes first because it is the only sample whose position in the
// result matters.
func SamplePoints(points []Point) []Point {
	if len(points) == 0 {
		return nil
	}

	start := points[0]
	samples := []Point{start}

	// The extremes are carried as values rather than indices, so there is no
	// index whose range has to be argued about.
	minLat, maxLat, minLon, maxLon := start, start, start, start

	for _, point := range points {
		if point.Lat < minLat.Lat {
			minLat = point
		}
		if point.Lat > maxLat.Lat {
			maxLat = point
		}
		if point.Lon < minLon.Lon {
			minLon = point
		}
		if point.Lon > maxLon.Lon {
			maxLon = point
		}
	}

	samples = append(samples, minLat, maxLat, minLon, maxLon)

	for step := 1; step <= waypointCount; step++ {
		index := len(points) * step / (waypointCount + 1)
		if index >= len(points) {
			index = len(points) - 1
		}

		samples = append(samples, points[index])
	}

	return samples
}
