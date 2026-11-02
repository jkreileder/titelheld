package route

import "sort"

// Set is the distinct cells a ride traverses. It is the whole fingerprint:
// no order, no visit counts, which is what makes matching direction-blind by
// construction — a corridor ridden twice leaves the same set as once.
type Set map[Cell]struct{}

// CellSet snaps every point to its cell and returns the distinct result.
//
// A ride's summary polyline visits far more points than cells: consecutive
// vertices of Strava's simplified track are tens to hundreds of metres apart,
// so most cells collect several vertices before the track moves on.
func CellSet(grid Grid, points []Point) Set {
	cells := make(Set, len(points))
	for _, point := range points {
		cells[grid.CellAt(point)] = struct{}{}
	}

	return cells
}

// Len returns the number of distinct cells — the fingerprint size the
// minimum-cell floor is applied to.
func (s Set) Len() int { return len(s) }

// Cells returns the cells in a stable order (by latitude, then longitude), so
// logs, dumps and stored documents for the same ride always read identically.
func (s Set) Cells() []Cell {
	cells := make([]Cell, 0, len(s))
	for cell := range s {
		cells = append(cells, cell)
	}

	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Lat != cells[j].Lat {
			return cells[i].Lat < cells[j].Lat
		}

		return cells[i].Lon < cells[j].Lon
	})

	return cells
}

// Jaccard returns |a ∩ b| / |a ∪ b| for the two cell sets.
//
// An empty operand yields 0 rather than the 0/0 that pure mathematics would
// argue about: two rides with no cells in common have nothing evidencing a
// repeat, and "no evidence" must not score as a perfect match. The
// minimum-cell floor refuses empty sets anyway; this convention keeps the
// similarity function honest on its own.
func Jaccard(a, b Set) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}

	// Counting membership over the smaller set keeps the intersection walk
	// short; the union follows from inclusion-exclusion without building it.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}

	intersection := 0
	for cell := range small {
		if _, ok := large[cell]; ok {
			intersection++
		}
	}

	return float64(intersection) / float64(len(a)+len(b)-intersection)
}
