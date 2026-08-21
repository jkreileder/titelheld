package processor

import (
	"errors"
	"strings"
	"testing"

	"github.com/jkreileder/titelheld/internal/classifier"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/store"
)

// Synthetic geofences. Coordinates in this repository are invented; nothing
// here is anybody's home or workplace.
var (
	syntheticHome = classifier.Geofence{
		Center:       classifier.Point{Lat: 50.0, Lon: 10.0},
		RadiusMeters: 300,
	}
	syntheticWork = classifier.Geofence{
		Center:       classifier.Point{Lat: 50.05, Lon: 10.05},
		RadiusMeters: 300,
	}
)

// commuteConfig switches the safety net on.
func commuteConfig() classifier.Config {
	cfg := classifier.DefaultConfig()
	cfg.Home = syntheticHome
	cfg.Work = syntheticWork

	return cfg
}

// The commute safety net names without an LLM.
//
// This is the tier ActivityFix normally handles. When it has not, the title
// still has to be right, and it has to come from configuration rather than a
// model: there are two possible answers, and a model would only add variance
// and cost to a choice that has already been made.
func TestCommuteIsNamedFromConfigurationWithoutAnLLM(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		startLat  float64
		startLon  float64
		endLat    float64
		endLon    float64
		configure func(*classifier.Config)
		want      string
	}{
		{
			name:     "to work",
			startLat: 50.0, startLon: 10.0,
			endLat: 50.05, endLon: 10.05,
			want: "Zur Arbeit",
		},
		{
			name:     "to home",
			startLat: 50.05, startLon: 10.05,
			endLat: 50.0, endLon: 10.0,
			want: "Nach Hause",
		},
		{
			name:     "to work, athlete's own wording",
			startLat: 50.0, startLon: 10.0,
			endLat: 50.05, endLon: 10.05,
			configure: func(c *classifier.Config) { c.ToWorkTitle = "Ins Büro" },
			want:      "Ins Büro",
		},
		{
			name:     "to home, athlete's own wording",
			startLat: 50.05, startLon: 10.05,
			endLat: 50.0, endLon: 10.0,
			configure: func(c *classifier.Config) { c.ToHomeTitle = "Heimweg" },
			want:      "Heimweg",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, true, func(d *Deps) {
				cfg := commuteConfig()
				if tc.configure != nil {
					tc.configure(&cfg)
				}

				d.Classifier = cfg
			})

			h.strava.activity.Distance = 8000
			h.strava.activity.MovingTime = 1500
			h.strava.activity.SportType = "Ride"
			h.strava.activity.StartLatLng = []float64{tc.startLat, tc.startLon}
			h.strava.activity.EndLatLng = []float64{tc.endLat, tc.endLon}
			h.strava.activity.Name = "Morning Ride"

			h.enqueue(t, "create")

			if _, err := h.proc.Sweep(t.Context()); err != nil {
				t.Fatalf("Sweep: %v", err)
			}

			writes := h.strava.writes()
			if len(writes) != 1 {
				t.Fatalf("%d PUTs, want 1: %+v", len(writes), writes)
			}

			if writes[0].name != tc.want {
				t.Errorf("title %q, want %q", writes[0].name, tc.want)
			}

			if h.provider.calls != 0 {
				t.Errorf("the LLM was called %d times for a commute", h.provider.calls)
			}
		})
	}
}

// An errand gets a deliberately boring name, and the same one every time.
//
// The destination of an errand is exactly the thing a title must not reveal,
// so there is no geocoding and no model here. Determinism is the point of the
// assertion: the same activity replayed must not acquire a different name.
func TestErrandIsNamedDeterministicallyAndRevealsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, func(d *Deps) { d.Classifier = commuteConfig() })

	h.strava.activity.SportType = "Ride"
	h.strava.activity.Commute = true
	h.strava.activity.Distance = 4200
	h.strava.activity.MovingTime = 900
	h.strava.activity.Name = "Afternoon Ride"

	// Far from both geofences, so it is an errand rather than a commute.
	h.strava.activity.StartLatLng = []float64{51.0, 11.0}
	h.strava.activity.EndLatLng = []float64{51.01, 11.01}

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	writes := h.strava.writes()
	if len(writes) != 1 {
		t.Fatalf("%d PUTs, want 1: %+v", len(writes), writes)
	}

	// Activity 777 % 3 == 0.
	if want := "Besorgungen"; writes[0].name != want {
		t.Errorf("title %q, want %q", writes[0].name, want)
	}

	if h.provider.calls != 0 {
		t.Errorf("the LLM was called %d times for an errand", h.provider.calls)
	}

	if h.geoCalls() != 0 {
		t.Errorf("an errand was geocoded %d times; its destination is private", h.geoCalls())
	}
}

// An errand may be switched off entirely.
func TestErrandsCanBeLeftUnnamed(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, func(d *Deps) {
		cfg := commuteConfig()
		cfg.LeaveErrandsUnnamed = true
		d.Classifier = cfg
	})

	h.strava.activity.SportType = "Ride"
	h.strava.activity.Commute = true
	h.strava.activity.Distance = 4200
	h.strava.activity.MovingTime = 900
	h.strava.activity.Name = "Afternoon Ride"
	h.strava.activity.StartLatLng = []float64{51.0, 11.0}
	h.strava.activity.EndLatLng = []float64{51.01, 11.01}

	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Skipped != 1 || result.Named != 0 {
		t.Errorf("result %+v, want one skip and no naming", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("%d PUTs for an errand that should be left alone: %+v", len(writes), writes)
	}
}

// A ride with no polyline is named without place names rather than failing.
//
// GPS off, a lost signal, an indoor session: none of these is an error, and
// none of them should cost the ride its title.
func TestARideWithoutAPolylineIsStillNamed(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.strava.activity.Map.SummaryPolyline = ""

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Fatalf("%d PUTs, want 1: %+v", len(writes), writes)
	}

	if h.provider.calls != 1 {
		t.Errorf("the LLM was called %d times, want 1", h.provider.calls)
	}
}

// With no geocoder wired up at all, naming carries on.
func TestNoGeocoderIsNotAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, func(d *Deps) { d.Geo = nil })
	h.strava.activity.Map.SummaryPolyline = "_p~iF~ps|U"

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if writes := h.strava.writes(); len(writes) != 1 {
		t.Fatalf("%d PUTs, want 1: %+v", len(writes), writes)
	}
}

// Geocoding that fails leaves the activity queued for the next sweep.
//
// Nominatim rate-limits, and a title invented with no place names would be
// worse than one produced a sweep later. This is the one gathering failure
// that is worth retrying rather than working around.
func TestGeocodingFailureLeavesTheActivityQueued(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, func(d *Deps) {
		d.Geo = fakeGeo{err: errors.New("nominatim: 429")}
	})
	h.strava.activity.Map.SummaryPolyline = "_p~iF~ps|U"

	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("result %+v, want one failure", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("%d PUTs despite no geography: %+v", len(writes), writes)
	}

	due, err := h.store.Due(t.Context(), h.now)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}

	if len(due) != 1 {
		t.Errorf("%d entries still queued, want 1 for the retry", len(due))
	}
}

// A sport ride with no LLM configured fails loudly instead of inventing one.
func TestSportRideWithoutAProviderFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, func(d *Deps) { d.Provider = nil })

	h.enqueue(t, "create")

	result, err := h.proc.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("result %+v, want one failure", result)
	}

	if writes := h.strava.writes(); len(writes) != 0 {
		t.Errorf("%d PUTs with no provider: %+v", len(writes), writes)
	}
}

// A description that already carries the attribution is left untouched.
//
// The sentinel is the whole idempotency mechanism for the description, and it
// has to work on a description this service has never seen before — one
// restored from a backup, or copied by the athlete from another activity.
func TestAnAlreadyAttributedDescriptionIsNotRewritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	h.strava.activity.Description = naming.Attribution + "\n\nXert: Difficult"

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	writes := h.strava.writes()
	if len(writes) != 1 {
		t.Fatalf("%d PUTs, want 1: %+v", len(writes), writes)
	}

	if writes[0].hadDesc {
		t.Errorf("the description was rewritten: %q", writes[0].description)
	}
}

// Attribution can be switched off, and then nothing is fetched to prepend to.
func TestAttributionCanBeDisabled(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, func(d *Deps) { d.DisableAttribution = true })

	h.enqueue(t, "create")

	if _, err := h.proc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	writes := h.strava.writes()
	if len(writes) != 1 {
		t.Fatalf("%d PUTs, want 1: %+v", len(writes), writes)
	}

	if writes[0].hadDesc {
		t.Errorf("a description was sent with attribution disabled: %q", writes[0].description)
	}
}

// New refuses a processor that cannot work.
//
// Both of these would fail at the first activity instead, in a sweep, in
// production. Failing at construction is the difference between a service that
// will not start and one that starts and quietly names nothing.
func TestNewRequiresAStoreAndAClient(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		deps Deps
		want string
	}{
		{
			name: "no store",
			deps: Deps{Activities: &fakeStrava{}},
			want: "store",
		},
		{
			name: "no Strava client",
			deps: Deps{Store: store.NewMemory()},
			want: "Strava client",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proc, err := New(tc.deps)
			if err == nil {
				t.Fatalf("New succeeded without a %s", tc.name)
			}

			if proc != nil {
				t.Errorf("New returned a processor alongside an error")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the missing %s", err, tc.want)
			}
		})
	}
}

// New fills in a logger and a clock rather than panicking on nil.
func TestNewSuppliesALoggerAndAClock(t *testing.T) {
	t.Parallel()

	proc, err := New(Deps{Store: store.NewMemory(), Activities: &fakeStrava{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if proc.deps.Logger == nil {
		t.Error("no logger was supplied")
	}

	if proc.deps.Now == nil {
		t.Error("no clock was supplied")
	}
}

// Coordinates reach the classifier only when Strava sent a usable pair.
//
// Strava sends an empty array for an activity with no GPS, and the classifier
// takes a nil pointer to mean "no position". A zero-valued Point would instead
// mean the Gulf of Guinea, which is inside no geofence but is a real place the
// distance maths would happily measure from.
func TestOnlyCompleteCoordinatePairsReachTheClassifier(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		start    []float64
		end      []float64
		wantNil  bool
		checkEnd bool
	}{
		{name: "no coordinates at all", start: nil, end: nil, wantNil: true},
		{name: "empty arrays", start: []float64{}, end: []float64{}, wantNil: true},
		{name: "a truncated pair", start: []float64{50.0}, end: []float64{50.0}, wantNil: true},
		{name: "a complete pair", start: []float64{50.0, 10.0}, end: []float64{50.1, 10.1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			activity := sportRide()
			activity.StartLatLng = tc.start
			activity.EndLatLng = tc.end

			converted := toClassifierActivity(&activity)

			if (converted.Start == nil) != tc.wantNil {
				t.Errorf("Start is %v, wantNil=%v", converted.Start, tc.wantNil)
			}

			if (converted.End == nil) != tc.wantNil {
				t.Errorf("End is %v, wantNil=%v", converted.End, tc.wantNil)
			}
		})
	}
}

// An unhandled action is a programming error, and says so.
//
// Every action the classifier can return is handled above; this is the branch
// that catches a new one added without a matching case here.
func TestAnUnhandledActionIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, true, nil)
	activity := sportRide()

	_, err := h.proc.title(
		t.Context(), &activity,
		classifier.Decision{Action: classifier.Action(99)}, quiet())
	if err == nil {
		t.Fatal("an unknown action was accepted")
	}

	if !strings.Contains(err.Error(), "unhandled action") {
		t.Errorf("error %q does not say the action was unhandled", err)
	}
}
