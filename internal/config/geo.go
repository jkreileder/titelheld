package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jkreileder/titelheld/internal/geo"
)

// Environment variables for the geography layer. The prefix names the
// component: GEO_ configures how a route is sampled, NOMINATIM_ how the
// geocoder is asked and how its answer is read.
const (
	EnvGeoSampleCount       = "GEO_SAMPLE_COUNT"
	EnvNominatimZoom        = "NOMINATIM_ZOOM"
	EnvNominatimPlaceFields = "NOMINATIM_PLACE_FIELDS"
)

// Geo is the geography layer's configuration.
//
// Every field is safe at its zero value, which is the shipped default the geo
// package resolves. The defaults themselves live there, next to the code that
// applies them, so there is one owner for each of them.
type Geo struct {
	// SampleCount is how many points between the start and the farthest
	// point are geocoded. Zero means the shipped default.
	SampleCount int

	// Zoom is the granularity Nominatim is asked for. Zero means the
	// shipped default.
	Zoom int

	// PlaceFields is the order a point's name is resolved in. Empty means
	// the shipped order.
	PlaceFields []string
}

// loadGeo reads the geography settings, appending to errs rather than
// returning early, so one pass reports everything wrong at once.
func loadGeo(getenv func(string) string, errs *[]error) Geo {
	var config Geo

	if raw := strings.TrimSpace(getenv(EnvGeoSampleCount)); raw != "" {
		count, err := strconv.Atoi(raw)
		if err != nil || count < 1 || count > geo.MaxSampleCount {
			*errs = append(*errs, fmt.Errorf("config: %s must be between 1 and %d, got %q",
				EnvGeoSampleCount, geo.MaxSampleCount, raw))
		} else {
			config.SampleCount = count
		}
	}

	if raw := strings.TrimSpace(getenv(EnvNominatimZoom)); raw != "" {
		zoom, err := strconv.Atoi(raw)
		if err != nil || zoom < geo.MinZoom || zoom > geo.MaxZoom {
			*errs = append(*errs, fmt.Errorf("config: %s must be between %d and %d, got %q",
				EnvNominatimZoom, geo.MinZoom, geo.MaxZoom, raw))
		} else {
			config.Zoom = zoom
		}
	}

	config.PlaceFields = loadPlaceFields(getenv, errs)

	return config
}

// loadPlaceFields reads the resolution order.
//
// Validated here as well as in the geo package: a key nothing reads would
// otherwise be dropped where it is used, and a deployment would run with an
// order it never got. A startup error names the key and the set it was
// rejected against.
func loadPlaceFields(getenv func(string) string, errs *[]error) []string {
	fields := splitList(getenv(EnvNominatimPlaceFields))
	if len(fields) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(fields))

	for _, field := range fields {
		if !geo.IsPlaceField(field) {
			*errs = append(*errs, fmt.Errorf("config: %s names %q, which is not one of %s",
				EnvNominatimPlaceFields, field, strings.Join(geo.PlaceFields(), ", ")))

			return nil
		}

		if _, ok := seen[field]; ok {
			*errs = append(*errs, fmt.Errorf("config: %s lists %q twice",
				EnvNominatimPlaceFields, field))

			return nil
		}

		seen[field] = struct{}{}
	}

	return fields
}
