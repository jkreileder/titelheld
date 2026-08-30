package naming

import "strings"

// Attribution is the line this service prepends to the description of every
// activity it names.
//
// Strava's PUT replaces the whole description, so adding one line is a
// read-modify-write: fetch what is there, put the line in front of it, send
// the result back. Everything already there has to survive that round trip
// byte for byte — it is Xert's, myWindsock's and mybiketraffic's output, and
// this service's job is to be the last writer, not the only one.
//
// The line is also the idempotency sentinel. There is no separate marker and
// no stored flag saying "attributed": if the line is anywhere in the
// description, this service has been here, and it does not come back. That
// makes the check work across replays, across a lost database, and across a
// description the athlete has since edited around.
const Attribution = "Title by titelheld — https://github.com/jkreileder/titelheld"

// sentinel is what the presence check matches on.
//
// The URL rather than the whole line: the prose in front of it could
// reasonably be reworded one day, and a reworded line must not cause every
// already-attributed activity to be attributed again. The URL is the part that
// identifies this service.
const sentinel = "https://github.com/jkreileder/titelheld"

// HasAttribution reports whether a description already carries the line.
func HasAttribution(description string) bool {
	return strings.Contains(description, sentinel)
}

// Describe returns the description to write, and whether anything changed.
//
// It reports false — write nothing — when the line is already present, which
// is the sentinel doing its job, and when the caller has attribution switched
// off.
//
// A blank line separates the attribution from what follows, so the athlete's
// own text keeps its shape in Strava's renderer. An empty description gets the
// line alone: no trailing blank line, because there is nothing for it to
// separate.
func Describe(existing string, enabled bool) (string, bool) {
	if !enabled || HasAttribution(existing) {
		return existing, false
	}

	if existing == "" {
		return Attribution, true
	}

	// existing is concatenated unmodified. No trimming, no newline
	// normalization, no re-encoding: whatever the other tools wrote comes back
	// exactly as it was, including trailing whitespace and \r\n.
	return Attribution + "\n\n" + existing, true
}

// RemoveAttribution takes the line back out, and reports whether it was there.
//
// The inverse of [Describe]: for every description that function produces, this
// returns the input it was given, byte for byte. Nothing else in the
// description is touched — not the whitespace in front of the line, not a
// second copy of it, not anything another tool wrote.
//
// It removes a *line*, not a substring. The attribution has to occupy a whole
// line — start at the beginning of the description or just after a line
// terminator, and end at the description's end or at one — because an athlete
// who wrote the URL into a sentence of their own has written a sentence, and
// cutting the middle out of it would be this service editing their prose. An
// occurrence that fails that test is stepped over, and a later whole-line one
// is still removed.
//
// It also matches the whole line and not the sentinel, which is the deliberate
// asymmetry. [HasAttribution] is loose so that rewording the prose can never
// cause an already-attributed activity to be attributed twice; this is exact,
// because it deletes text and a description is the athlete's. A description
// carrying some older wording of the line therefore keeps it, and the caller
// learns that from the reported false rather than from a line that silently
// took a neighboring sentence with it.
//
// What follows the line is removed with it: the line's own terminator and the
// blank line [Describe] writes after it, and at most those two. Either may be
// LF or CRLF — [Describe] writes LF, but the line survives whatever editor the
// athlete has been through since.
func RemoveAttribution(description string) (string, bool) {
	for searched := 0; ; {
		offset := strings.Index(description[searched:], Attribution)
		if offset < 0 {
			return description, false
		}

		start := searched + offset
		end := start + len(Attribution)

		if !startsLine(description, start) || !endsLine(description, end) {
			searched = end

			continue
		}

		return description[:start] + cutTerminator(cutTerminator(description[end:])), true
	}
}

// startsLine reports whether an index is at the beginning of a line.
func startsLine(s string, index int) bool {
	return index == 0 || s[index-1] == '\n'
}

// endsLine reports whether an index is at the end of a line.
func endsLine(s string, index int) bool {
	return index == len(s) || s[index] == '\n' || strings.HasPrefix(s[index:], "\r\n")
}

// cutTerminator removes one leading line terminator, if there is one. CRLF is
// tried first, so a CRLF is one terminator rather than a stray carriage return
// followed by an empty line.
func cutTerminator(s string) string {
	if rest, ok := strings.CutPrefix(s, "\r\n"); ok {
		return rest
	}

	rest, _ := strings.CutPrefix(s, "\n")

	return rest
}
