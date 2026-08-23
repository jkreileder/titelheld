package route

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// fixturePath reaches the calibration fixture: real summary polylines with
// labeled pairs. The file is untracked (it carries real coordinates), so its
// absence skips the tests that need it rather than failing them.
const fixturePath = "../../testdata/real_rides.json"

type rideFixture struct {
	ID         string  `json:"id"`
	Date       string  `json:"date"`
	DistanceKm float64 `json:"distance_km"`
	Notes      string  `json:"notes"`
	Polyline   string  `json:"polyline"`
}

type realRidesFixture struct {
	Rides []rideFixture `json:"rides"`
}

// readFixture decodes the fixture and returns nil (not an error) when it is
// absent, so a fresh clone of this spike still has a green test run.
func readFixture() (map[string][]Point, error) {
	raw, err := os.ReadFile(filepath.Clean(fixturePath))
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", fixturePath, err)
	}

	var parsed realRidesFixture
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", fixturePath, err)
	}

	rides := make(map[string][]Point, len(parsed.Rides))

	for _, ride := range parsed.Rides {
		points, err := DecodePolyline(ride.Polyline)
		if err != nil {
			return nil, fmt.Errorf("decoding %s: %w", ride.ID, err)
		}

		rides[ride.ID] = points
	}

	return rides, nil
}

// --- helpers -----------------------------------------------------------------

// degLat converts a north-south distance in metres to degrees of latitude.
func degLat(metres float64) float64 {
	return metres / metersPerDegree
}

// degLon converts an east-west distance in metres to degrees of longitude at
// the given latitude.
func degLon(metres, lat float64) float64 {
	return metres / (metersPerDegree * math.Cos(lat*math.Pi/180))
}

// line walks straight from `from` to `to` in the given number of segments,
// endpoints included.
func line(from, to Point, segments int) []Point {
	points := make([]Point, 0, segments+1)

	for i := range segments + 1 {
		fraction := float64(i) / float64(segments)
		points = append(points, Point{
			Lat: from.Lat + (to.Lat-from.Lat)*fraction,
			Lon: from.Lon + (to.Lon-from.Lon)*fraction,
		})
	}

	return points
}

func reversePoints(points []Point) []Point {
	reversed := make([]Point, len(points))

	for i, point := range points {
		reversed[len(points)-1-i] = point
	}

	return reversed
}

// jitterPoints displaces every point independently, each axis uniform in
// ±metres — deliberately harsher than GPS error, whose two axes do not reach
// their worst case simultaneously.
func jitterPoints(points []Point, metres float64, rng *rand.Rand) []Point {
	noisy := make([]Point, len(points))

	for i, point := range points {
		dLat := (rng.Float64()*2 - 1) * metres
		dLon := (rng.Float64()*2 - 1) * metres
		noisy[i] = Point{
			Lat: point.Lat + degLat(dLat),
			Lon: point.Lon + degLon(dLon, point.Lat),
		}
	}

	return noisy
}

// Synthetic shapes near, though not on, the real rides' area: far enough from
// the equator that the cos(latitude) shrinkage of longitude degrees is
// exercised, synthetic enough to commit to a public repo.
const (
	spikeLat0 = 48.40
	spikeLon0 = 13.00
)

// syntheticOpenArc is an open ride: start and end differ by kilometres.
func syntheticOpenArc() []Point {
	return append(
		line(Point{Lat: spikeLat0, Lon: spikeLon0}, Point{Lat: spikeLat0 + degLat(2500), Lon: spikeLon0 + degLon(6000, spikeLat0)}, 60),
		line(Point{Lat: spikeLat0 + degLat(2500), Lon: spikeLon0 + degLon(6000, spikeLat0)},
			Point{Lat: spikeLat0 + degLat(5200), Lon: spikeLon0 + degLon(7500, spikeLat0)}, 30)[1:]...)
}

// syntheticOutAndBack runs a 6 km corridor east and retraces it west.
func syntheticOutAndBack() []Point {
	eastEnd := Point{Lat: spikeLat0, Lon: spikeLon0 + degLon(6000, spikeLat0)}
	corridor := line(Point{Lat: spikeLat0, Lon: spikeLon0}, eastEnd, 90)

	return append(corridor, reversePoints(corridor)[1:]...)
}

// syntheticLoop shares its south leg — the exact corridor street — with the
// out-and-back, then returns along a parallel street 400 m north.
func syntheticLoop() []Point {
	northLat := spikeLat0 + degLat(400)
	eastEnd := Point{Lat: spikeLat0, Lon: spikeLon0 + degLon(6000, spikeLat0)}
	northEast := Point{Lat: northLat, Lon: eastEnd.Lon}
	northWest := Point{Lat: northLat, Lon: spikeLon0}

	loop := line(Point{Lat: spikeLat0, Lon: spikeLon0}, eastEnd, 90)
	loop = append(loop, line(eastEnd, northEast, 6)[1:]...)
	loop = append(loop, line(northEast, northWest, 90)[1:]...)
	loop = append(loop, line(northWest, Point{Lat: spikeLat0, Lon: spikeLon0}, 6)[1:]...)

	return loop
}

// loadFixtureRides decodes the fixture and reports whether it was present.
func loadFixtureRides(t *testing.T) (map[string][]Point, bool) {
	t.Helper()

	rides, err := readFixture()
	if err != nil {
		t.Fatalf("reading the real-ride fixture failed: %v", err)
	}

	return rides, rides != nil
}

// mustGrid builds a grid or fails the test.
func mustGrid(t *testing.T, size float64) Grid {
	t.Helper()

	grid, err := NewGrid(size)
	if err != nil {
		t.Fatalf("NewGrid(%v): %v", size, err)
	}

	return grid
}

// leakyFingerprint mimics a fingerprint that secretly depends on traversal
// order: the production cell set, plus a sentinel marking the cell the ride
// STARTED in. Property tests use it as a negative control — an implementation
// this broken must fail those tests.
func leakyFingerprint(grid Grid, points []Point) Set {
	cells := CellSet(grid, points)
	first := grid.CellAt(points[0])
	cells[Cell{Lat: first.Lat + 1, Lon: first.Lon}] = struct{}{}

	return cells
}

// setsEqual compares two cell sets.
func setsEqual(a, b Set) bool {
	if len(a) != len(b) {
		return false
	}

	for cell := range a {
		if _, ok := b[cell]; !ok {
			return false
		}
	}

	return true
}

// --- grid --------------------------------------------------------------------

func TestNewGridValidatesItsSize(t *testing.T) {
	t.Parallel()

	for _, size := range []float64{0, -200, math.NaN(), math.Inf(1), math.Inf(-1), 0.5} {
		if _, err := NewGrid(size); err == nil {
			t.Errorf("NewGrid(%v) succeeded, want error", size)
		}
	}

	grid, err := NewGrid(DefaultGridSizeMeters)
	if err != nil {
		t.Fatalf("NewGrid(%v): %v", DefaultGridSizeMeters, err)
	}

	wantStep := DefaultGridSizeMeters / metersPerDegree
	if grid.SizeMeters() != DefaultGridSizeMeters || grid.StepDegrees() != wantStep {
		t.Errorf("grid = %v m / %v°, want %v m / %v°",
			grid.SizeMeters(), grid.StepDegrees(), DefaultGridSizeMeters, wantStep)
	}
}

// A point fractionally either side of a lattice midpoint must land in the
// nearer cell. Coordinates are constructed FROM lattice marks (index times
// step) rather than by adding offsets to an arbitrary base: subtracting two
// large coordinates to recover a cell-relative position loses low bits, and
// the test would be measuring cancellation, not snapping. The exact midpoint
// itself is left untested — which cell owns a measure-zero tie cannot be
// asserted through floating-point division without testing the compiler.
func TestCellAtSnapsToTheNearestLatticeMark(t *testing.T) {
	t.Parallel()

	grid, err := NewGrid(DefaultGridSizeMeters)
	if err != nil {
		t.Fatalf("NewGrid: %v", err)
	}

	step := grid.StepDegrees()
	mark := func(index int64) float64 { return float64(index) * step }

	const (
		latIdx = 26939 // near 48.4°N on the default grid
		lonIdx = 7236  // near 13.0°E
	)

	tests := []struct {
		name  string
		point Point
		want  Cell
	}{
		{
			name:  "on a mark",
			point: Point{Lat: mark(latIdx), Lon: mark(lonIdx)},
			want:  Cell{Lat: latIdx, Lon: lonIdx},
		},
		{
			name: "just past the midpoint, east",
			point: Point{
				Lat: mark(latIdx),
				Lon: mark(lonIdx+2) - step*0.49,
			},
			want: Cell{Lat: latIdx, Lon: lonIdx + 2},
		},
		{
			name: "just short of the midpoint, east",
			point: Point{
				Lat: mark(latIdx),
				Lon: mark(lonIdx+2) - step*0.51,
			},
			want: Cell{Lat: latIdx, Lon: lonIdx + 1},
		},
		{
			name: "just past the midpoint, south",
			point: Point{
				Lat: mark(latIdx-1) - step*0.51,
				Lon: mark(lonIdx),
			},
			want: Cell{Lat: latIdx - 2, Lon: lonIdx},
		},
		{
			name: "just short of the midpoint, south",
			point: Point{
				Lat: mark(latIdx-1) - step*0.49,
				Lon: mark(lonIdx),
			},
			want: Cell{Lat: latIdx - 1, Lon: lonIdx},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := grid.CellAt(tt.point); got != tt.want {
				t.Errorf("CellAt(%+v) = %+v, want %+v", tt.point, got, tt.want)
			}
		})
	}
}

// Keys are what gets stored and logged, so neighbouring cells must render
// differently and a key must read as a coordinate near the cell's centre.
func TestGridKeysAreDistinctAndCoordinateShaped(t *testing.T) {
	t.Parallel()

	grid, err := NewGrid(DefaultGridSizeMeters)
	if err != nil {
		t.Fatalf("NewGrid: %v", err)
	}

	seen := make(map[string]Cell)

	for _, origin := range []Cell{{Lat: 0, Lon: 0}, {Lat: 436, Lon: 177}, {Lat: -436, Lon: -177}} {
		key := grid.Key(origin)
		if previous, clash := seen[key]; clash {
			t.Errorf("keys of %+v and %+v collide on %q", origin, previous, key)
		}

		seen[key] = origin

		for _, neighbour := range []Cell{
			{Lat: origin.Lat + 1, Lon: origin.Lon},
			{Lat: origin.Lat, Lon: origin.Lon + 1},
			{Lat: origin.Lat - 1, Lon: origin.Lon - 1},
		} {
			if grid.Key(neighbour) == key {
				t.Errorf("cells %+v and %+v share the key %q", origin, neighbour, key)
			}
		}
	}

	// The key renders the rounded coordinate itself, geocache-key style:
	// index 26939 sits at 26939·(200/111320)° = 48.39924°, index 7236 at
	// 13.00036°, each rendered to the four decimals the grid resolves.
	want := "48.3992,13.0004"
	if got := grid.Key(Cell{Lat: 26939, Lon: 7236}); got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}

	if got := grid.Key(Cell{}); got != "0.0000,0.0000" {
		t.Errorf("Key(origin) = %q, want \"0.0000,0.0000\"", got)
	}
}

func TestSetCellsAreOrdered(t *testing.T) {
	t.Parallel()

	grid, err := NewGrid(DefaultGridSizeMeters)
	if err != nil {
		t.Fatalf("NewGrid: %v", err)
	}

	set := CellSet(grid, []Point{
		{Lat: spikeLat0 + degLat(800), Lon: spikeLon0},
		{Lat: spikeLat0, Lon: spikeLon0 + degLon(400, spikeLat0)},
		{Lat: spikeLat0, Lon: spikeLon0},
	})

	first, second := set.Cells()[0], set.Cells()[1]
	if first.Lat > second.Lat || (first.Lat == second.Lat && first.Lon >= second.Lon) {
		t.Errorf("Cells() = %+v..., not ordered by latitude then longitude", set.Cells())
	}

	again := set.Cells()
	for i := range again {
		if again[i] != set.Cells()[i] {
			t.Fatal("Cells() is not stable between calls")
		}
	}
}

// --- Jaccard and Match --------------------------------------------------------

func TestJaccard(t *testing.T) {
	t.Parallel()

	a := Set{{Lat: 0, Lon: 0}: {}, {Lat: 1, Lon: 1}: {}, {Lat: 2, Lon: 2}: {}}
	b := Set{{Lat: 3, Lon: 3}: {}, {Lat: 4, Lon: 4}: {}}
	c := Set{{Lat: 0, Lon: 0}: {}, {Lat: 9, Lon: 9}: {}}
	d := Set{{Lat: 0, Lon: 0}: {}, {Lat: 1, Lon: 1}: {}}

	tests := []struct {
		name string
		got  float64
		want float64
	}{
		{name: "identical sets", got: Jaccard(a, a), want: 1},
		{name: "disjoint sets", got: Jaccard(a, b), want: 0},
		{name: "symmetry", got: Jaccard(b, a), want: Jaccard(a, b)},
		{name: "one shared of two plus two", got: Jaccard(c, d), want: 1.0 / 3.0},
		{name: "empty versus empty", got: Jaccard(nil, Set{}), want: 0},
		{name: "empty versus full", got: Jaccard(a, nil), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if math.Abs(tt.got-tt.want) > 1e-12 {
				t.Errorf("Jaccard = %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestMatchAppliesTheFloorBeforeSimilarity(t *testing.T) {
	t.Parallel()

	big := Set{}
	far := Set{}

	for i := range 30 {
		big[Cell{Lat: int64(i), Lon: 0}] = struct{}{}
		far[Cell{Lat: int64(i), Lon: 1000}] = struct{}{}
	}

	small := Set{{Lat: 0, Lon: 0}: {}, {Lat: 1, Lon: 0}: {}, {Lat: 2, Lon: 0}: {}}

	tests := []struct {
		name        string
		a, b        Set
		threshold   float64
		minCells    int
		wantMatched bool
		wantSmallA  bool
		wantSmallB  bool
	}{
		{
			name: "identical large sets match",
			a:    big, b: big, threshold: 0.7, minCells: 25,
			wantMatched: true,
		},
		{
			name: "an identical small set still does not match",
			a:    small, b: small, threshold: 0.7, minCells: 25,
			wantSmallA: true, wantSmallB: true,
		},
		{
			name: "one small side refuses the pair",
			a:    small, b: big, threshold: 0.7, minCells: 25,
			wantSmallA: true,
		},
		{
			name: "similarity below threshold refuses",
			a:    big, b: far, threshold: 0.7, minCells: 25,
		},
		{
			name: "garbage threshold fails closed",
			a:    big, b: big, threshold: 1.7, minCells: 25,
		},
		{
			name: "zero floor disables the floor",
			a:    small, b: small, threshold: 0.7, minCells: 0,
			wantMatched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			verdict := Match(tt.a, tt.b, tt.threshold, tt.minCells)
			if verdict.Matched() != tt.wantMatched {
				t.Errorf("Matched() = %v (similarity %v), want %v",
					verdict.Matched(), verdict.Similarity, tt.wantMatched)
			}

			if verdict.SmallA != tt.wantSmallA || verdict.SmallB != tt.wantSmallB {
				t.Errorf("floors = (%v, %v), want (%v, %v)",
					verdict.SmallA, verdict.SmallB, tt.wantSmallA, tt.wantSmallB)
			}
		})
	}
}

// The defaults must satisfy the contracts their own docs claim, or every
// caller trusting them inherits the lie.
func TestDefaultsHoldTheirContracts(t *testing.T) {
	t.Parallel()

	if _, err := NewGrid(DefaultGridSizeMeters); err != nil {
		t.Errorf("the default grid size is not a valid grid size: %v", err)
	}

	if DefaultThreshold < 0 || DefaultThreshold > 1 {
		t.Errorf("DefaultThreshold = %v, outside [0, 1]", DefaultThreshold)
	}

	if DefaultMinCells <= 0 {
		t.Errorf("DefaultMinCells = %d, must be positive", DefaultMinCells)
	}
}

// --- property: reversal invariance ---------------------------------------------

// Reversing a polyline must leave the fingerprint untouched: the set has no
// order to hold, so a ride and its mirror image are the same fingerprint by
// construction.
//
// Negative controls prove the test can fail:
//
//  1. A fingerprint with a hidden order dependency (a sentinel marking the
//     start cell) must differ under reversal — the harness detects leakage.
//  2. An ORDERED cell sequence, the design the spec rejected, must differ
//     under reversal — showing what sets buy.
func TestFingerprintIsReversalInvariant(t *testing.T) {
	t.Parallel()

	subjects := map[string][]Point{
		"open synthetic arc": syntheticOpenArc(),
		"out-and-back":       syntheticOutAndBack(),
	}

	if rides, ok := loadFixtureRides(t); ok {
		subjects["real aug15_east"] = rides["aug15_east"]
		subjects["real aug19_west"] = rides["aug19_west"]
	}

	for _, size := range []float64{DefaultGridSizeMeters, 400} {
		grid, err := NewGrid(size)
		if err != nil {
			t.Fatalf("NewGrid(%v): %v", size, err)
		}

		for name, points := range subjects {
			forward := CellSet(grid, points)
			backward := CellSet(grid, reversePoints(points))

			if !setsEqual(forward, backward) {
				t.Errorf("%v m grid, %s: %d cells forward, %d reversed",
					size, name, forward.Len(), backward.Len())
			}
		}
	}

	// Negative control 1: an order-leaking fingerprint must be caught.
	grid, err := NewGrid(DefaultGridSizeMeters)
	if err != nil {
		t.Fatalf("NewGrid: %v", err)
	}

	arc := syntheticOpenArc()
	if first, last := grid.CellAt(arc[0]), grid.CellAt(arc[len(arc)-1]); first == last {
		t.Fatal("negative control needs an open trace whose ends sit in different cells")
	}

	if setsEqual(leakyFingerprint(grid, arc), leakyFingerprint(grid, reversePoints(arc))) {
		t.Error("negative control: an order-leaking fingerprint passed the reversal check")
	}

	// Negative control 2: a SEQUENCE comparator over raw vertices — the
	// design family the spec replaced — must differ under reversal here.
	// Sorted set output cannot serve as this control: sorting already
	// destroyed the order, so it would pass both ways and prove nothing.
	vertexSequence := func(points []Point) []Cell {
		cells := make([]Cell, len(points))
		for i, point := range points {
			cells[i] = grid.CellAt(point)
		}

		return cells
	}

	forwardSequence := vertexSequence(arc)
	backwardSequence := vertexSequence(reversePoints(arc))
	if len(forwardSequence) == len(backwardSequence) {
		sameOrder := true

		for i := range forwardSequence {
			if forwardSequence[i] != backwardSequence[i] {
				sameOrder = false

				break
			}
		}

		if sameOrder {
			t.Error("negative control: a raw vertex sequence was reversal-invariant")
		}
	}
}

// --- property: jitter tolerance --------------------------------------------------

// Real GPS traces of the same streets never repeat vertex-for-vertex: Strava
// simplifies the summary polyline, satellites drift. So ±5 m of independent
// noise on EVERY point must leave a fingerprint recognisably the same ride:
// Jaccard against the untouched original stays above 0.9.
//
// Negative control: the same harness driven 24× harder (±120 m, most of a
// cell) must fall below the same bound — the measurement responds to real
// corruption and the bound is not vacuous.
func TestFingerprintToleratesFiveMetresOfJitter(t *testing.T) {
	t.Parallel()

	const (
		jitterMetres    = 5.0
		controlMetres   = 120.0
		trials          = 30
		similarityBound = 0.9
	)

	subjects := map[string][]Point{
		"out-and-back": syntheticOutAndBack(),
		"loop":         syntheticLoop(),
		"open arc":     syntheticOpenArc(),
	}

	if rides, ok := loadFixtureRides(t); ok {
		for _, id := range []string{"aug15_east", "aug16_west_long", "aug19_west", "aug21_west"} {
			subjects["real "+id] = rides[id]
		}
	}

	for _, size := range []float64{DefaultGridSizeMeters, 400} {
		grid, err := NewGrid(size)
		if err != nil {
			t.Fatalf("NewGrid(%v): %v", size, err)
		}

		for name, points := range subjects {
			original := CellSet(grid, points)

			worst := 1.0
			for seed := 1; seed <= trials; seed++ {
				rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)))
				jittered := Jaccard(original, CellSet(grid, jitterPoints(points, jitterMetres, rng)))
				worst = math.Min(worst, jittered)
			}

			if worst <= similarityBound {
				t.Errorf("%v m grid, %s: worst Jaccard over %d trials of ±%v m = %.4f, want > %v",
					size, name, trials, jitterMetres, worst, similarityBound)
			}

			// Negative control, run once per grid on the first subject: heavy
			// corruption must break through the bound.
			if name != "out-and-back" {
				continue
			}

			best := 0.0
			for seed := 1; seed <= trials; seed++ {
				rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)))
				smashed := Jaccard(original, CellSet(grid, jitterPoints(points, controlMetres, rng)))
				best = math.Max(best, smashed)
			}

			if best >= similarityBound {
				t.Errorf("negative control: ±%v m noise kept Jaccard at %.4f ≥ %v — the bound proves nothing",
					controlMetres, best, similarityBound)
			}
		}
	}
}

// --- property: the minimum-cell floor -------------------------------------------

// Below the floor there is no route memory, even for a trace compared with
// ITSELF: a fingerprint that small localises a rider too precisely to ever be
// worth matching.
//
// Negative control: the same short trace scores a PERFECT 1.0 similarity, so
// if the verdict nevertheless refuses the pair, the refusal demonstrably came
// from the floor and not from the score.
func TestTheMinCellFloorRefusesShortTraces(t *testing.T) {
	t.Parallel()

	grid, err := NewGrid(DefaultGridSizeMeters)
	if err != nil {
		t.Fatalf("NewGrid: %v", err)
	}

	shortTrace := syntheticOutAndBack()[:31] // roughly 2 km of the corridor
	shortSet := CellSet(grid, shortTrace)

	longRide := syntheticOpenArc()
	longSet := CellSet(grid, longRide)

	if shortSet.Len() >= DefaultMinCells {
		t.Fatalf("fixture is %d cells, want comfortably below the %d-cell floor",
			shortSet.Len(), DefaultMinCells)
	}

	if longSet.Len() < DefaultMinCells {
		t.Fatalf("fixture is only %d cells, want above the %d-cell floor",
			longSet.Len(), DefaultMinCells)
	}

	selfVerdict := Match(shortSet, shortSet, DefaultThreshold, DefaultMinCells)
	if selfVerdict.Matched() {
		t.Errorf("a %d-cell trace matched itself; the floor did not refuse it", shortSet.Len())
	}

	if !selfVerdict.SmallA || !selfVerdict.SmallB {
		t.Errorf("refusal flags = (%v, %v), want both sides flagged small",
			selfVerdict.SmallA, selfVerdict.SmallB)
	}

	// Negative control: the score alone would happily match this pair.
	if !(selfVerdict.Similarity >= DefaultThreshold) || selfVerdict.Similarity != 1 {
		t.Errorf("short-trace self-similarity = %v, want a perfect score above the threshold",
			selfVerdict.Similarity)
	}

	mixed := Match(shortSet, longSet, DefaultThreshold, DefaultMinCells)
	if mixed.Matched() || !mixed.SmallA || mixed.SmallB {
		t.Errorf("mixed pair verdict = matched(%v), small(%v, %v), want refused by side A only",
			mixed.Matched(), mixed.SmallA, mixed.SmallB)
	}

	legit := Match(longSet, longSet, DefaultThreshold, DefaultMinCells)
	if !legit.Matched() {
		t.Errorf("a %d-cell ride refused itself: the floor is biting real rides", longSet.Len())
	}
}

// --- property: out-and-back versus loop ------------------------------------------

// Two rides through the same area, one shape: an out-and-back down a single
// street, and a loop whose SOUTH LEG IS THAT STREET and which returns along a
// parallel street 400 m north. What "behaving sanely" means, decided BEFORE
// testing:
//
//  1. Multiplicity collapse: riding the corridor twice leaves exactly the
//     cells of riding it once. If visit counts leaked into the set, the
//     direction-blind story would be a lie.
//  2. Each shape matches itself perfectly — the positive control.
//  3. The pair is NOT a repeat: sharing the outbound street must not make
//     the loop score like a re-ride, because the spec's contract puts
//     different routes through one area BELOW threshold. Expect the cross
//     similarity well above zero (half the loop literally is the corridor)
//     and clearly below the threshold.
//  4. The verdict refuses the cross pair at the shipped defaults.
func TestAnOutAndBackAndALoopThroughTheSameArea(t *testing.T) {
	t.Parallel()

	grid := DefaultGrid()

	outAndBack := CellSet(grid, syntheticOutAndBack())
	loop := CellSet(grid, syntheticLoop())
	oneWay := CellSet(grid, syntheticOutAndBack()[:91])

	// 1. Riding a street twice leaves the same cells as riding it once.
	if !setsEqual(outAndBack, oneWay) {
		t.Errorf("out-and-back collapsed to %d cells, one-way to %d — visit counts leaked",
			outAndBack.Len(), oneWay.Len())
	}

	// 2. Positive controls: everything is a perfect repeat of itself.
	for name, set := range map[string]Set{"out-and-back": outAndBack, "loop": loop} {
		if got := Jaccard(set, set); got != 1 {
			t.Errorf("%s vs itself: Jaccard = %v, want 1", name, got)
		}

		if verdict := Match(set, set, DefaultThreshold, DefaultMinCells); !verdict.Matched() {
			t.Errorf("%s vs itself refused: %+v", name, verdict)
		}
	}

	// 3. The cross pair: high enough to prove the comparison sees the shared
	// street, low enough to honour the negative-case contract.
	cross := Jaccard(outAndBack, loop)
	t.Logf("out-and-back (%d cells) × loop (%d cells): J=%.4f",
		outAndBack.Len(), loop.Len(), cross)

	if cross <= 0 {
		t.Errorf("out-and-back vs loop = %v; the shared corridor went unseen", cross)
	}

	if cross >= DefaultThreshold {
		t.Errorf("out-and-back vs loop = %.4f ≥ %.2f: a different route through the same area counts as a repeat",
			cross, DefaultThreshold)
	}

	// 4. And the verdict refuses it — while refusing for the right reason.
	crossVerdict := Match(outAndBack, loop, DefaultThreshold, DefaultMinCells)
	if crossVerdict.Matched() {
		t.Errorf("cross pair matched: %+v", crossVerdict)
	}

	if crossVerdict.SmallA || crossVerdict.SmallB {
		t.Error("cross pair was refused by the floor, not by similarity — the discriminator did not fire")
	}

	// 5. Whatever grid the calibration ends up recommending, this shape pair
	// must survive it: a loop sharing a leg with an out-and-back stays a
	// non-repeat at 400 m / 0.53 just as at the spec defaults.
	grid400 := mustGrid(t, 400)
	cross400 := Jaccard(CellSet(grid400, syntheticOutAndBack()), CellSet(grid400, syntheticLoop()))
	t.Logf("out-and-back × loop at 400 m: J=%.4f", cross400)

	if cross400 >= 0.53 {
		t.Errorf("cross pair at 400 m = %.4f ≥ 0.53: the calibrated combination misclassifies the synthetic negative",
			cross400)
	}

	// Negative control: the cross pair must score strictly below BOTH
	// self-pairs. A comparator that inflated similarities indiscriminately
	// would fail exactly here.
	if !(cross < Jaccard(outAndBack, outAndBack)) || !(cross < Jaccard(loop, loop)) {
		t.Error("negative control: the cross pair is not separated from the self pairs")
	}
}
