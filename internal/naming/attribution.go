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
// It matches the whole line and not the sentinel, which is the asymmetry worth
// knowing about. [HasAttribution] is deliberately loose so that rewording the
// prose can never cause an already-attributed activity to be attributed twice;
// this is deliberately exact, because it deletes text and a description is the
// athlete's. A description carrying some older wording of the line therefore
// keeps it, and the caller learns that from the reported false rather than
// from a line that silently took a neighboring sentence with it.
//
// What follows the line is removed with it: the line's own terminator and the
// blank line [Describe] writes after it, and at most those two bytes.
func RemoveAttribution(description string) (string, bool) {
	index := strings.Index(description, Attribution)
	if index < 0 {
		return description, false
	}

	rest := description[index+len(Attribution):]
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\n")

	return description[:index] + rest, true
}
