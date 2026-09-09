package route

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// metersPerDegree is the length of one degree of latitude, used to translate
// a metric grid size into degrees. The true meridian degree varies between
// 110.6 km at the equator and 111.7 km at the poles; a single constant is
// well inside the tolerance a "~200 m grid" carries.
//
// One degree of LONGITUDE is shorter by cos(latitude), so a cell is square in
// degrees and not in metres: at Regensburg's 48.4°N the east-west edge of the
// default grid is about two thirds of its north-south edge. The grid stays a
// fixed lattice regardless — every ride in a comparison lives on the same
// lattice, which is all set overlap needs.
const metersPerDegree = 111_320.0

// DefaultGridSizeMeters is the shipped grid size: a ~200 m cell edge, the
// scale at which "same streets" and "different streets" separate for the
// ride distances this serves. The calibration study behind REPORT.md
// measures what it actually buys.
const DefaultGridSizeMeters = 200.0

// minGridSizeMeters keeps the lattice sane: below a metre, cells stop being a
// route fingerprint and become a coordinate copy, while [Grid.Key] would need
// an unbounded number of decimal places to name them.
const minGridSizeMeters = 1.0

// Cell is one grid square, identified by its rounded coordinate — the lattice
// index of the point that fell inside it. Two points are in the same cell
// exactly when their indices are equal, so this is the only state a cell
// needs.
type Cell struct {
	Lat int64
	Lon int64
}

// Grid is the snapping lattice.
type Grid struct {
	// sizeMeters is the requested north-south edge length.
	sizeMeters float64

	// stepDegrees is the lattice pitch both axes share.
	stepDegrees float64
}

// NewGrid returns a lattice whose north-south cell edge is sizeMeters long.
// East-west edges shrink with cos(latitude); see [metersPerDegree].
func NewGrid(sizeMeters float64) (Grid, error) {
	// The NaN/Inf checks are spelled out rather than folded into <= 0 so the
	// error names what was actually wrong with the number it was given.
	switch {
	case math.IsNaN(sizeMeters):
		return Grid{}, errors.New("route: grid size is not a number")
	case math.IsInf(sizeMeters, 0):
		return Grid{}, errors.New("route: grid size is infinite")
	case sizeMeters < minGridSizeMeters:
		return Grid{}, fmt.Errorf("route: grid size %v m is below the %v m minimum",
			sizeMeters, minGridSizeMeters)
	}

	return Grid{
		sizeMeters:  sizeMeters,
		stepDegrees: sizeMeters / metersPerDegree,
	}, nil
}

// DefaultGrid is the grid this package ships with.
func DefaultGrid() Grid {
	grid, err := NewGrid(DefaultGridSizeMeters)
	if err != nil {
		panic("route: " + err.Error()) // the constant satisfies NewGrid's contract
	}

	return grid
}

// SizeMeters returns the north-south cell edge the grid was built for.
func (g Grid) SizeMeters() float64 { return g.sizeMeters }

// StepDegrees returns the lattice pitch shared by both axes.
func (g Grid) StepDegrees() float64 { return g.stepDegrees }

// CellAt snaps a point to its cell.
//
// Rounding (nearest, ties away from zero) rather than flooring puts the
// lattice marks ON rounded coordinates — the same family as the geocache
// keys, where CacheKey rounds to whole thousandths of a degree — and keeps
// the family property that a cell's key reads as a coordinate near its
// centre. Whether a partition rounds or floors is invisible to set overlap:
// both are fixed partitions, and every ride is snapped by the same rule.
func (g Grid) CellAt(p Point) Cell {
	return Cell{
		Lat: int64(math.Round(p.Lat / g.stepDegrees)),
		Lon: int64(math.Round(p.Lon / g.stepDegrees)),
	}
}

// Key renders a cell as the rounded coordinate pair it is named by, the same
// "lat,lon" shape the geocode cache uses for its keys. Enough decimal places
// are emitted that neighbouring cells cannot render identically, so keys can
// be stored, logged or diffed without carrying the grid along.
func (g Grid) Key(c Cell) string {
	digits := keyDigits(g.stepDegrees)

	return strconv.FormatFloat(float64(c.Lat)*g.stepDegrees, 'f', digits, 64) + "," +
		strconv.FormatFloat(float64(c.Lon)*g.stepDegrees, 'f', digits, 64)
}

// keyDigits picks enough decimal places that adjacent lattice marks differ in
// their rendered form: d places resolve 10^-d, so d works when
// stepDegrees > 2·10^-d. With the metre-level floor on grid size, six places
// always suffice; the loop finds the smallest such d anyway so coarser grids
// render short keys.
func keyDigits(stepDegrees float64) int {
	resolution := 1.0

	for digits := 1; digits <= 6; digits++ {
		resolution /= 10
		if stepDegrees > 2*resolution {
			return digits
		}
	}

	return 6
}
