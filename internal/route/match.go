package route

// DefaultThreshold is the similarity above which two cell sets count as the
// same route. It ships at 0.7 because the spec names that number; REPORT.md
// holds the calibration evidence on what the labeled rides actually support.
const DefaultThreshold = 0.7

// DefaultMinCells is the smallest distinct-cell count that becomes a
// fingerprint at all: below it, no route memory for that ride. The number is
// a spike choice, not a spec'd one — 25 cells is roughly ten kilometres of
// travelled corridor on the default grid, comfortably under every sport ride
// (the only rides that get fingerprinted) and well above anything that could
// be triangulated back to a single place.
const DefaultMinCells = 25

// Verdict is what comparing two fingerprints concludes, with the inputs kept
// visible so a caller can log why a ride was refused rather than just that it
// was.
type Verdict struct {
	// Similarity is the Jaccard similarity of the two sets.
	Similarity float64

	// Threshold is what Similarity was measured against.
	Threshold float64

	// MinCells is the floor both sets were measured against.
	MinCells int

	// SmallA and SmallB record which side fell below MinCells, if either.
	SmallA bool
	SmallB bool
}

// Matched reports whether the pair counts as the same route.
//
// Both floors must clear before similarity is even considered — that is the
// fail-closed shape the negative case needs: a short trace cannot match,
// whatever its score.
func (v Verdict) Matched() bool {
	return !v.SmallA && !v.SmallB && v.Similarity >= v.Threshold
}

// Match compares two cell sets and returns the verdict.
//
// A threshold outside [0, 1] or a non-positive floor simply never matches;
// garbage configuration fails closed rather than panicking or matching.
func Match(a, b Set, threshold float64, minCells int) Verdict {
	return Verdict{
		Similarity: Jaccard(a, b),
		Threshold:  threshold,
		MinCells:   minCells,
		SmallA:     len(a) < minCells,
		SmallB:     len(b) < minCells,
	}
}
