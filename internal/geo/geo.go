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

// waypointCount is how many evenly spaced points along the route are sampled,
// on top of the start and the bounding-box extremes.
const waypointCount = 3

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
// Strava deletes tokens from a title that look like a hostname. Observed live
// on 2026-08-24: "Über Ruhstorf a.d.Rott nach Pocking" was stored as
// "Über Ruhstorf  nach Pocking" — the token excised, both spaces left behind.
// `a.d.Rott` is label.label.label, which is what a naive link filter matches.
//
// Nominatim returns the official compact form and Bavaria is full of it —
// Ruhstorf a.d.Rott, Neustadt a.d.Donau, Rothenburg o.d.Tauber — so a title
// built from these names would be mangled routinely. Spacing the periods is
// also the correct German typography, and it stops the token looking like a
// host.
//
// Applied to every resolved place rather than to a list of German
// abbreviations: the shape is the problem, and anything genuinely URL-like has
// no business in a title either.
//
// Repeated until stable because the matches overlap: "a.d.Rott" needs two
// passes, the second seeing the "d" the first consumed.
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
		summary   Summary
		seen      = make(map[string]struct{}, len(samples))
		seenNames = make(map[string]struct{}, len(samples))
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
