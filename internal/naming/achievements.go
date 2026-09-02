package naming

// CopiesAchievement reports whether a candidate title is one of the given
// achievement names taken whole, and which one.
//
// A segment is named by whoever created it, so a title that is nothing but
// that name is somebody else's words on the athlete's ride. The prompt asks
// for what the ride did on the stretch instead; this is what makes the
// request binding, the way [Guarded.Claimed] binds the franchise rule.
//
// The comparison is the franchise matcher's: equality after normalization —
// both sides lowercased, punctuation flattened, whitespace collapsed — and
// equality against the article-dropped core, the same two tests a claimed
// entry faces. Equality and not containment, deliberately: a title about the
// stretch is the invited angle, so a title that mentions the segment inside
// its own words must stand. A near-copy that rewords the name passes too —
// the line is drawn at the name as the entire title, because that is the
// case with no words of the athlete's voice in it at all.
func CopiesAchievement(title string, achievements []string) (string, bool) {
	normalized := normalizeForMatch(title)
	if normalized == "" {
		return "", false
	}

	for _, name := range achievements {
		if candidate := normalizeForMatch(name); candidate != "" &&
			(normalized == candidate || normalized == entryCore(name)) {
			return name, true
		}
	}

	return "", false
}
