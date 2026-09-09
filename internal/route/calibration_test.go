//go:build routefixture

package route

// The calibration needs testdata/real_rides.json — real coordinates,
// deliberately untracked — so everything here sits behind this build tag and
// is absent from the default `go test ./...` run:
//
//	go test -tags routefixture ./internal/route/...
//
// Under this tag a missing fixture FAILS rather than skips: a build that
// promises the data must not stay silent when the data is gone. Every
// property these tests pin against real rides is also pinned against
// committed synthetic shapes in route_test.go, which is what CI runs.
import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pairFixture is one labeled expectation from the calibration fixture.
type pairFixture struct {
	A           string `json:"a"`
	B           string `json:"b"`
	Expectation string `json:"expectation"`
}

type labeledFixture struct {
	Rides        []rideFixture `json:"rides"`
	LabeledPairs []pairFixture `json:"labeled_pairs"`
}

// requirePairRides fails when a labeled pair names a ride the fixture no
// longer carries: a nil fingerprint would silently reclassify the pair
// instead of announcing that its data is gone.
func requirePairRides(t *testing.T, rides map[string][]Point, labeled []pairFixture) {
	t.Helper()

	for _, pair := range labeled {
		for _, id := range []string{pair.A, pair.B} {
			if len(rides[id]) == 0 {
				t.Fatalf("labeled pair references missing or empty ride %q", id)
			}
		}
	}
}

func loadLabeledPairs(t *testing.T) []pairFixture {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(fixturePath))
	if err != nil {
		t.Fatalf("reading %s: %v", fixturePath, err)
	}

	var parsed labeledFixture
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decoding %s: %v", fixturePath, err)
	}

	if len(parsed.Rides) == 0 || len(parsed.LabeledPairs) == 0 {
		t.Fatal("fixture carries no rides or no labeled pairs")
	}

	return parsed.LabeledPairs
}

// expectationKind classifies a labeled pair: must a similarity stay BELOW the
// threshold, or sit ABOVE it? Keyed to the fixture's actual vocabulary —
// "MUST stay below threshold", "clear negative", "candidate repeat" — and
// deliberately loud about anything else, so a reworded fixture fails here
// instead of silently changing what is being asserted.
func expectationKind(expectation string) string {
	switch {
	case containsFold(expectation, "must stay below") || containsFold(expectation, "clear negative"):
		return "negative"
	case containsFold(expectation, "candidate repeat"):
		return "positive"
	default:
		return "unknown"
	}
}

func containsFold(s, substring string) bool {
	for i := 0; i+len(substring) <= len(s); i++ {
		match := true

		for j := range substring {
			a, b := s[i+j], substring[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}

			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}

			if a != b {
				match = false

				break
			}
		}

		if match {
			return true
		}
	}

	return false
}

// TestCalibrationMatrices computes the full pairwise Jaccard matrix at every
// grid size under consideration and checks which thresholds satisfy ALL
// labeled expectations.
//
// The headline finding is asserted here on purpose — these are measured
// truths about the labeled data, and if the numbers move, the suite should
// say so:
//
//   - At the spec-default 200 m grid there is NO threshold that works: the
//     candidate repeat pair scores BELOW one of the must-stay-below pairs.
//   - At 100 m the same inversion holds.
//   - At 400 m a threshold window opens, but it is only ~0.03 wide.
func TestCalibrationMatrices(t *testing.T) {
	rides, ok := loadFixtureRides(t)
	if !ok {
		t.Fatal("testdata/real_rides.json is missing; the routefixture tag promises it is there")
	}

	labeled := loadLabeledPairs(t)
	requirePairRides(t, rides, labeled)

	const (
		recommendedGrid = 400.0
		recommendedT    = 0.53
	)

	type feasibility struct {
		grid    float64
		posMin  float64
		negMax  float64
		posPair [2]string
		negPair [2]string
	}

	var results = make([]feasibility, 0, 3)

	for _, size := range []float64{100, DefaultGridSizeMeters, recommendedGrid} {
		grid, err := NewGrid(size)
		if err != nil {
			t.Fatalf("NewGrid(%v): %v", size, err)
		}

		fingerprints := make(map[string]Set, len(rides))

		ids := make([]string, 0, len(rides))
		for id := range rides {
			ids = append(ids, id)
		}

		sort.Strings(ids)

		for _, id := range ids {
			fingerprints[id] = CellSet(grid, rides[id])
		}

		t.Logf("=== grid %.0f m ===", size)

		var header strings.Builder
		fmt.Fprintf(&header, "%10s", "")
		for _, id := range ids {
			fmt.Fprintf(&header, "%10.10s", id)
		}

		t.Log(header.String())

		for _, a := range ids {
			var row strings.Builder
			fmt.Fprintf(&row, "%10.10s", a)
			for _, b := range ids {
				fmt.Fprintf(&row, "%10.4f", Jaccard(fingerprints[a], fingerprints[b]))
			}

			t.Log(row.String())
		}

		for _, id := range ids {
			t.Logf("cells[%s] = %d", id, fingerprints[id].Len())
		}

		feas := feasibility{grid: size, posMin: math.Inf(1), negMax: 0}

		for _, pair := range labeled {
			similarity := Jaccard(fingerprints[pair.A], fingerprints[pair.B])
			kind := expectationKind(pair.Expectation)
			if kind == "unknown" {
				t.Fatalf("unclassifiable expectation for %s × %s: %q", pair.A, pair.B, pair.Expectation)
			}

			t.Logf("pair %-14s x %-14s J=%.4f [%s]",
				pair.A, pair.B, similarity, kind)

			switch kind {
			case "positive":
				if similarity < feas.posMin {
					feas.posMin, feas.posPair = similarity, [2]string{pair.A, pair.B}
				}
			case "negative":
				if similarity > feas.negMax {
					feas.negMax, feas.negPair = similarity, [2]string{pair.A, pair.B}
				}
			}
		}

		results = append(results, feas)

		t.Logf("grid %.0f m: min positive = %.4f (%v), max negative = %.4f (%v)",
			size, feas.posMin, feas.posPair, feas.negMax, feas.negPair)
	}

	byGrid := make(map[float64]feasibility, len(results))
	for _, feas := range results {
		byGrid[feas.grid] = feas
	}

	// The measured inversions are ASSERTED, not just logged: if new data or
	// different snapping makes these grids separable, this fails and forces
	// the report and its recommendation to be re-examined rather than the
	// change sailing through as a silent pass.
	if feas := byGrid[100]; feas.posMin > feas.negMax {
		t.Errorf("grid 100 m became separable (positive %.4f > negative %.4f); "+
			"the pinned inversion no longer holds — re-examine REPORT.md",
			feas.posMin, feas.negMax)
	} else {
		t.Logf("grid 100 m: inversion holds (positive %.4f ≤ worst negative %.4f)",
			feas.posMin, feas.negMax)
	}

	if feas := byGrid[DefaultGridSizeMeters]; feas.posMin > feas.negMax {
		t.Errorf("grid %.0f m became separable (positive %.4f > negative %.4f); "+
			"the headline finding no longer holds — re-examine REPORT.md",
			DefaultGridSizeMeters, feas.posMin, feas.negMax)
	} else {
		t.Logf("grid %.0f m: inversion holds (positive %.4f ≤ worst negative %.4f) — THE headline finding",
			DefaultGridSizeMeters, feas.posMin, feas.negMax)
	}

	feas := byGrid[recommendedGrid]
	if feas.posMin <= feas.negMax {
		t.Fatalf("grid %.0f m lost its separation window: positive %.4f ≤ negative %.4f",
			recommendedGrid, feas.posMin, feas.negMax)
	}

	margin := math.Min(feas.posMin-recommendedT, recommendedT-feas.negMax)
	if margin < 0.005 {
		t.Fatalf("recommended threshold %.2f sits too close to the window edge (%.4f margin)",
			recommendedT, margin)
	}

	// Every labeled pair classified correctly at the recommendation.
	grid := mustGrid(t, recommendedGrid)

	for _, pair := range labeled {
		verdict := Match(CellSet(grid, rides[pair.A]), CellSet(grid, rides[pair.B]),
			recommendedT, DefaultMinCells)

		wantMatched := expectationKind(pair.Expectation) == "positive"
		if verdict.Matched() != wantMatched {
			t.Errorf("%s × %s: matched=%v at T=%.2f (J=%.4f), want matched=%v — %s",
				pair.A, pair.B, verdict.Matched(), recommendedT, verdict.Similarity,
				wantMatched, pair.Expectation)
		}
	}
}

// TestCalibrationThresholdSweep walks thresholds coarsely enough to print but
// finely enough to find the feasible band, and asserts that no threshold
// rescues the smaller grids while the 400 m band contains the recommendation.
func TestCalibrationThresholdSweep(t *testing.T) {
	rides, ok := loadFixtureRides(t)
	if !ok {
		t.Fatal("testdata/real_rides.json is missing; the routefixture tag promises it is there")
	}

	labeled := loadLabeledPairs(t)
	requirePairRides(t, rides, labeled)

	for _, size := range []float64{100, DefaultGridSizeMeters, 400} {
		grid, err := NewGrid(size)
		if err != nil {
			t.Fatalf("NewGrid(%v): %v", size, err)
		}

		fingerprints := make(map[string]Set, len(rides))

		for id, points := range rides {
			fingerprints[id] = CellSet(grid, points)
		}

		passing := []float64{}

		for threshold := 0.05; threshold <= 0.95+1e-9; threshold += 0.01 {
			allSatisfied := true

			for _, pair := range labeled {
				matched := Match(fingerprints[pair.A], fingerprints[pair.B],
					threshold, DefaultMinCells).Matched()
				if matched != (expectationKind(pair.Expectation) == "positive") {
					allSatisfied = false

					break
				}
			}

			if allSatisfied {
				passing = append(passing, threshold)
			}
		}

		t.Logf("grid %3.0f m: thresholds satisfying all labels (step .01): %v",
			size, passing)

		switch size {
		case 100, DefaultGridSizeMeters:
			if len(passing) != 0 {
				t.Errorf("grid %.0f m: found satisfying thresholds %v, want none — the inversion was supposed to hold",
					size, passing)
			}
		case 400:
			if len(passing) == 0 || passing[0] > 0.54 || passing[len(passing)-1] < 0.51 {
				t.Errorf("grid 400 m: satisfying set %v does not bracket the 0.51–0.54 window", passing)
			}
		}
	}
}

// TestCalibrationJitterOnRealRides repeats the ±5 m property against real
// geometry, where Strava's simplification makes vertex spacing comparable to
// the cell size. The synthetic shapes pass comfortably; this is where the
// bound could embarrass.
func TestCalibrationJitterOnRealRides(t *testing.T) {
	rides, ok := loadFixtureRides(t)
	if !ok {
		t.Fatal("testdata/real_rides.json is missing; the routefixture tag promises it is there")
	}

	const (
		jitterMetres  = 5.0
		controlMetres = 120.0
		trials        = 30
		bound         = 0.9
	)

	for _, size := range []float64{DefaultGridSizeMeters, 400} {
		grid, err := NewGrid(size)
		if err != nil {
			t.Fatalf("NewGrid(%v): %v", size, err)
		}

		for id, points := range rides {
			original := CellSet(grid, points)

			worst := 1.0
			for seed := 1; seed <= trials; seed++ {
				rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)))
				jittered := Jaccard(original, CellSet(grid, jitterPoints(points, jitterMetres, rng)))
				worst = math.Min(worst, jittered)
			}

			t.Logf("±%v m, %v trials, %.0f m grid, %s: worst Jaccard %.4f",
				jitterMetres, trials, size, id, worst)

			if worst <= bound {
				t.Errorf("%s at %.0f m grid: ±%v m jitter drove Jaccard to %.4f, want > %v",
					id, size, jitterMetres, worst, bound)
			}

			// Negative control on one subject per grid.
			if id != "aug19_west" {
				continue
			}

			best := 0.0
			for seed := 1; seed <= trials; seed++ {
				rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)))
				smashed := Jaccard(original, CellSet(grid, jitterPoints(points, controlMetres, rng)))
				best = math.Max(best, smashed)
			}

			t.Logf("control ±%v m, %s, %.0f m grid: best Jaccard %.4f (must stay below %.1f)",
				controlMetres, id, size, best, bound)

			if best >= bound {
				t.Errorf("negative control: ±%v m kept %s at %.4f ≥ %.1f",
					controlMetres, id, best, bound)
			}
		}
	}
}

// TestContainmentDiagnosis logs why the repeat pair fails to separate: how
// much of each ride's cells the other covers. It asserts nothing beyond
// sanity; the numbers feed REPORT.md.
func TestContainmentDiagnosis(t *testing.T) {
	rides, ok := loadFixtureRides(t)
	if !ok {
		t.Fatal("testdata/real_rides.json is missing; the routefixture tag promises it is there")
	}

	labeled := loadLabeledPairs(t)
	requirePairRides(t, rides, labeled)
	grid := DefaultGrid()

	fingerprints := make(map[string]Set, len(rides))

	for id, points := range rides {
		fingerprints[id] = CellSet(grid, points)
	}

	for _, pair := range labeled {
		a, b := fingerprints[pair.A], fingerprints[pair.B]
		intersection := 0

		for cell := range a {
			if _, ok := b[cell]; ok {
				intersection++
			}
		}

		if len(a) == 0 || len(b) == 0 {
			t.Fatalf("empty fingerprint for %s × %s", pair.A, pair.B)
		}

		t.Logf("%-16s × %-16s |A|=%3d |B|=%3d ∩=%3d inA=%.3f inB=%.3f J=%.4f",
			pair.A, pair.B, len(a), len(b), intersection,
			float64(intersection)/float64(len(a)),
			float64(intersection)/float64(len(b)),
			Jaccard(a, b))
	}
}
