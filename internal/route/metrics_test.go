//go:build routefixture

package route

// Measurement extension for the family-frame question (2026-08-23): the
// athlete confirmed aug19_west and aug21_west are deliberately varied repeats
// of one loop FAMILY, while aug16_west_long is a different loop sharing the
// home corridor. Binary same/different is the wrong frame; these tests
// measure two proposed refinements on the same fixtures without touching the
// matching API:
//
//  1. Rarity-weighted overlap — each cell weighted by inverse frequency
//     across the four rides (the stand-in for the last-100 window), so cells
//     every ride traverses stop dominating the score.
//  2. Novelty fraction — per ride, in date order, the share of its cells
//     absent from the union of all prior rides.
//
// Both are logged as tables for REPORT.md; where a number IS a finding, it
// is asserted so a fixture or snapping change announces itself.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// weightedOverlap returns Σ w(c)·[c ∈ A∩B] / Σ w(c)·[c ∈ A∪B]: the Jaccard
// ratio with each cell's contribution scaled by its corpus rarity. Binary
// membership makes min/max and in/out formulations identical, so there is
// exactly one natural definition here.
func weightedOverlap(a, b Set, weights map[Cell]float64) float64 {
	var (
		intersection float64
		union        float64
	)

	for cell := range a {
		union += weights[cell]
		if _, ok := b[cell]; ok {
			intersection += weights[cell]
		}
	}

	for cell := range b {
		if _, ok := a[cell]; !ok {
			union += weights[cell]
		}
	}

	if union == 0 {
		return 0
	}

	return intersection / union
}

// rarityWeights derives inverse-frequency weights: a cell carried by all
// four rides weighs 0.25, a cell unique to one ride weighs 1.
func rarityWeights(sets []Set) map[Cell]float64 {
	frequency := make(map[Cell]int)

	for _, set := range sets {
		for cell := range set {
			frequency[cell]++
		}
	}

	weights := make(map[Cell]float64, len(frequency))
	for cell, count := range frequency {
		weights[cell] = 1.0 / float64(count)
	}

	return weights
}

func TestWeightedOverlapMatrix(t *testing.T) {
	fixture, err := readFullFixture()
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	for _, size := range []float64{100, DefaultGridSizeMeters, 400} {
		grid := mustGrid(t, size)

		ids, fingerprints := sortedFingerprints(t, grid, fixture.points)

		sets := make([]Set, 0, len(ids))
		for _, id := range ids {
			sets = append(sets, fingerprints[id])
		}

		weights := rarityWeights(sets)

		var header strings.Builder
		fmt.Fprintf(&header, "%10s", "")
		for _, id := range ids {
			fmt.Fprintf(&header, "%10.10s", id)
		}

		t.Logf("=== weighted-J grid %.0f m ===", size)
		t.Log(header.String())

		for _, a := range ids {
			var row strings.Builder
			fmt.Fprintf(&row, "%10.10s", a)
			for _, b := range ids {
				fmt.Fprintf(&row, "%10.4f", weightedOverlap(fingerprints[a], fingerprints[b], weights))
			}

			t.Log(row.String())
		}

		family := weightedOverlap(fingerprints["aug19_west"], fingerprints["aug21_west"], weights)
		separate1 := weightedOverlap(fingerprints["aug16_west_long"], fingerprints["aug19_west"], weights)
		separate2 := weightedOverlap(fingerprints["aug16_west_long"], fingerprints["aug21_west"], weights)

		worstSeparate := math.Max(separate1, separate2)

		unweightedFamily := Jaccard(fingerprints["aug19_west"], fingerprints["aug21_west"])
		unweightedSeparate := math.Max(
			Jaccard(fingerprints["aug16_west_long"], fingerprints["aug19_west"]),
			Jaccard(fingerprints["aug16_west_long"], fingerprints["aug21_west"]))

		window := family - worstSeparate
		unweightedWindow := unweightedFamily - unweightedSeparate

		t.Logf("grid %.0f m: family=%.4f worst must-separate=%.4f window=%+.4f"+
			" (unweighted %+.4f)",
			size, family, worstSeparate, window, unweightedWindow)

		const epsilon = 5e-5 // the matrices are pure integer-derived set arithmetic

		switch size {
		case 100, DefaultGridSizeMeters:
			// Pinned measurement: weighting narrows the inversion but does
			// NOT close it below 400 m. If this fails because separation
			// appeared, the recommendation must be re-examined.
			if window > -epsilon {
				t.Errorf("grid %.0f m: weighted window %+.4f is no longer an inversion; re-examine REPORT.md",
					size, window)
			}

			if !(window > unweightedWindow+epsilon) {
				t.Errorf("grid %.0f m: weighting did not narrow the inversion (%+.4f vs unweighted %+.4f)",
					size, window, unweightedWindow)
			}
		case 400:
			if window <= 0.01 {
				t.Errorf("grid 400 m: weighted window %+.4f lost its usable width", window)
			}

			if !(window > unweightedWindow+epsilon) {
				t.Errorf("grid 400 m: weighting did not widen the feasible band (%+.4f vs unweighted %+.4f)",
					window, unweightedWindow)
			}
		}
	}
}

func TestNoveltyFraction(t *testing.T) {
	fixture, err := readFullFixture()
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	for _, size := range []float64{100, DefaultGridSizeMeters, 400} {
		grid := mustGrid(t, size)

		order := fixture.dateOrder()
		seen := Set{}

		t.Logf("=== novelty, grid %.0f m, date order %v ===", size, order.ids)

		for i, id := range order.ids {
			set := CellSet(grid, order.points[i])
			novel := Set{}

			for cell := range set {
				if _, ok := seen[cell]; !ok {
					novel[cell] = struct{}{}
				}
			}

			share := float64(len(novel)) / float64(len(set))
			t.Logf("%-14s (%s): %3d cells, %3d novel, novelty %.1f%%",
				id, order.dates[i], len(set), len(novel), 100*share)

			for cell := range set {
				seen[cell] = struct{}{}
			}
		}

		aug19 := CellSet(grid, fixture.points["aug19_west"])
		aug21 := CellSet(grid, fixture.points["aug21_west"])

		novelVsAug19 := 0
		for cell := range aug21 {
			if _, ok := aug19[cell]; !ok {
				novelVsAug19++
			}
		}

		share := float64(novelVsAug19) / float64(len(aug21))
		t.Logf("grid %.0f m: aug21 vs aug19 ALONE: %d/%d cells novel = %.1f%%",
			size, novelVsAug19, len(aug21), 100*share)

		// Pinned measurements: the family anchor's novelty share drifts
		// substantially with grid size — "N % new" is a per-grid number, not
		// a property of the route pair.
		expected := map[float64][2]int{100: {88, 210}, DefaultGridSizeMeters: {51, 149}, 400: {25, 95}}[size]
		if novelVsAug19 != expected[0] || len(aug21) != expected[1] {
			t.Errorf("grid %.0f m: aug21-vs-aug19 novelty is %d/%d cells, was pinned at %d/%d",
				size, novelVsAug19, len(aug21), expected[0], expected[1])
		}
	}
}

// fixtureData is the parsed fixture with points and dates retained.
type fixtureData struct {
	dates  map[string]string
	points map[string][]Point
}

// orderedRides is the fixture in date order, oldest first.
type orderedRides struct {
	ids    []string
	dates  []string
	points [][]Point
}

func (f fixtureData) dateOrder() orderedRides {
	ids := make([]string, 0, len(f.points))
	for id := range f.points {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool { return f.dates[ids[i]] < f.dates[ids[j]] })

	ordered := orderedRides{ids: ids}
	for _, id := range ids {
		ordered.dates = append(ordered.dates, f.dates[id])
		ordered.points = append(ordered.points, f.points[id])
	}

	return ordered
}

// readFullFixture parses the whole fixture, keeping the fields readFixture
// discards.
func readFullFixture() (fixtureData, error) {
	raw, err := os.ReadFile(filepath.Clean(fixturePath))
	if err != nil {
		return fixtureData{}, fmt.Errorf("reading %s: %w", fixturePath, err)
	}

	var parsed struct {
		Rides []rideFixture `json:"rides"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fixtureData{}, fmt.Errorf("decoding %s: %w", fixturePath, err)
	}

	data := fixtureData{
		dates:  make(map[string]string, len(parsed.Rides)),
		points: make(map[string][]Point, len(parsed.Rides)),
	}

	for _, ride := range parsed.Rides {
		points, err := DecodePolyline(ride.Polyline)
		if err != nil {
			return fixtureData{}, fmt.Errorf("decoding %s: %w", ride.ID, err)
		}

		data.dates[ride.ID] = ride.Date
		data.points[ride.ID] = points
	}

	return data, nil
}

// sortedFingerprints returns ride IDs sorted by name and their cell sets.
func sortedFingerprints(t *testing.T, grid Grid, points map[string][]Point) ([]string, map[string]Set) {
	t.Helper()

	ids := make([]string, 0, len(points))
	for id := range points {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	fingerprints := make(map[string]Set, len(ids))
	for _, id := range ids {
		fingerprints[id] = CellSet(grid, points[id])
	}

	return ids, fingerprints
}
