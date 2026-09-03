package config

import (
	"strings"
	"testing"
)

// Unset means the shipped defaults, which live in the geo package. This
// package reports what the environment said and resolves none of them, so the
// zero value here and the default there cannot drift into two answers.
func TestGeoDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Geo.SampleCount != 0 || cfg.Geo.Zoom != 0 || cfg.Geo.PlaceFields != nil {
		t.Errorf("geo = %+v, want the zero value when nothing is set", cfg.Geo)
	}
}

func TestGeoSettingsAreRead(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{
		"GEO_SAMPLE_COUNT":       "4",
		"NOMINATIM_ZOOM":         "14",
		"NOMINATIM_PLACE_FIELDS": "village, hamlet ,town",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Geo.SampleCount != 4 {
		t.Errorf("SampleCount = %d, want 4", cfg.Geo.SampleCount)
	}

	if cfg.Geo.Zoom != 14 {
		t.Errorf("Zoom = %d, want 14", cfg.Geo.Zoom)
	}

	want := []string{"village", "hamlet", "town"}
	if strings.Join(cfg.Geo.PlaceFields, ",") != strings.Join(want, ",") {
		t.Errorf("PlaceFields = %v, want %v", cfg.Geo.PlaceFields, want)
	}
}

// The sample count decides how many reverse-geocoding requests one activity
// costs, and the ceiling is not a value a deployment may raise.
func TestGeoSampleCountIsBounded(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"0", "-1", "7", "6.5", "many"} {
		if _, err := Load(env(map[string]string{"GEO_SAMPLE_COUNT": bad})); err == nil {
			t.Errorf("GEO_SAMPLE_COUNT=%q was accepted", bad)
		}
	}

	for _, good := range []string{"1", "3", "6"} {
		if _, err := Load(env(map[string]string{"GEO_SAMPLE_COUNT": good})); err != nil {
			t.Errorf("GEO_SAMPLE_COUNT=%q was refused: %v", good, err)
		}
	}

	// The bound the geo package enforces is the bound reported here.
	_, err := Load(env(map[string]string{"GEO_SAMPLE_COUNT": "99"}))
	if err == nil || !strings.Contains(err.Error(), EnvGeoSampleCount) {
		t.Errorf("error %v does not name %s", err, EnvGeoSampleCount)
	}
}

func TestGeoZoomIsBounded(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"2", "19", "0", "-4", "sixteen"} {
		if _, err := Load(env(map[string]string{"NOMINATIM_ZOOM": bad})); err == nil {
			t.Errorf("NOMINATIM_ZOOM=%q was accepted", bad)
		}
	}

	for _, good := range []string{"3", "12", "16", "18"} {
		if _, err := Load(env(map[string]string{"NOMINATIM_ZOOM": good})); err != nil {
			t.Errorf("NOMINATIM_ZOOM=%q was refused: %v", good, err)
		}
	}
}

// A key the geo package does not read would be dropped where it is used, so it
// is refused where it is set. The rules are the geo package's; what this layer
// adds is the name of the variable that carried the order.
func TestGeoPlaceFieldsAreValidated(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{"NOMINATIM_PLACE_FIELDS": "hamlet,road"}))
	if err == nil {
		t.Fatal("a road was accepted as a place field")
	}

	if !strings.Contains(err.Error(), "road") {
		t.Errorf("error %v does not name the key it rejected", err)
	}

	if !strings.Contains(err.Error(), EnvNominatimPlaceFields) {
		t.Errorf("error %v does not name the variable the order came from", err)
	}

	if cfg.Geo.PlaceFields != nil {
		t.Errorf("a rejected order was still returned: %v", cfg.Geo.PlaceFields)
	}

	if _, err := Load(env(map[string]string{
		"NOMINATIM_PLACE_FIELDS": "hamlet,village,hamlet",
	})); err == nil {
		t.Error("a duplicated key was accepted")
	}
}

// One pass reports everything wrong at once, which the loader's own comment
// promises: an operator correcting an order one key per restart learns the
// rules a keystroke at a time.
func TestGeoPlaceFieldsReportEveryBadKey(t *testing.T) {
	t.Parallel()

	_, err := Load(env(map[string]string{
		"NOMINATIM_PLACE_FIELDS": "hamlet,road,house_number",
	}))
	if err == nil {
		t.Fatal("a road and a house number were accepted as place fields")
	}

	for _, want := range []string{"road", "house_number"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not name %q", err, want)
		}
	}
}
