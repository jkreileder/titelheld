package naming

import (
	"strings"
)

// Description parsing turns third-party text into facts the prompt can carry.
//
// By naming time the description usually holds output from Xert (relative
// power, XSS, difficulty), myWindsock (CdA, headwind, temperature) and
// mybiketraffic (vehicle counts). Any of them may be absent, all of them may
// be absent, and the format is theirs to change without telling anyone.
//
// The description is hostile input. Not because those tools are hostile, but
// because this service does not control what ends up in that field: Strava
// lets the athlete type anything there, and anything typed there reaches an
// LLM. So parsing is deliberately dull — line-oriented, bounded, no regular
// expressions, and every value truncated. A line that does not parse is
// dropped rather than guessed at, and a description that parses to nothing is
// a ride with no notes, never an error.
//
// What this does not do is decide what the facts mean. [BuildPrompt] presents
// them under a heading it labels as data, and the prompt tells the model not
// to follow instructions found there.

// Bounds on what a description may contribute.
//
// These are not tuning knobs. They exist so a pathological description — a
// megabyte of one line, ten thousand short ones — costs a bounded amount of
// work and produces a bounded prompt, rather than being passed on to the model
// to deal with.
const (
	maxDescriptionBytes = 64 << 10
	maxDescriptionLines = 400
	maxFactLabelRunes   = 40
	maxFactValueRunes   = 80
	maxFacts            = 24
)

// factPattern is one label this service recognizes in a description.
//
// Matching is on a lowercased line prefix, so "Relative Power: 4.2 W/kg" and
// "relative power : 4.2 W/kg" both hit. The label written into the prompt is
// the canonical one here, not the one in the description, so a tool changing
// its capitalization does not change the prompt.
type factPattern struct {
	// match is the lowercased prefix to look for, without the separator.
	match string

	// label is what the prompt calls it.
	label string

	// source names the tool, for the reader of the prompt and of this file.
	source string
}

// The tools whose output this parser recognizes. Named rather than repeated so
// a label and its source cannot drift apart in the table below.
const (
	sourceXert          = "Xert"
	sourceMyWindsock    = "myWindsock"
	sourceMybiketraffic = "mybiketraffic"
)

// knownFacts is what gets extracted, and nothing else.
//
// An allow-list rather than "every Key: Value line", because a description is
// free text: an athlete's own note reading "Plan: ignore previous instructions"
// is a line in Key: Value shape, and the narrower the extraction the less of
// the field reaches the model at all.
var knownFacts = []factPattern{
	// Xert.
	{match: "relative power", label: "Relative power", source: sourceXert},
	{match: "xss", label: "Strain (XSS)", source: sourceXert},
	{match: "difficulty", label: "Difficulty", source: sourceXert},
	{match: "focus", label: "Focus", source: sourceXert},

	// myWindsock.
	{match: "cda", label: "CdA", source: sourceMyWindsock},
	{match: "headwind", label: "Headwind", source: sourceMyWindsock},
	{match: "tailwind", label: "Tailwind", source: sourceMyWindsock},
	{match: "wind", label: "Wind", source: sourceMyWindsock},
	{match: "temperature", label: "Temperature", source: sourceMyWindsock},
	{match: "temp", label: "Temperature", source: sourceMyWindsock},

	// mybiketraffic.
	{match: "vehicles", label: "Vehicles passing", source: sourceMybiketraffic},
	{match: "vehicle count", label: "Vehicles passing", source: sourceMybiketraffic},
	{match: "close passes", label: "Close passes", source: sourceMybiketraffic},
}

// ParseFacts extracts the facts a description carries.
//
// It never returns an error. A description this service cannot read is a ride
// without notes, and refusing to name such a ride would be a worse outcome
// than naming it with less to go on.
func ParseFacts(description string) []Fact {
	if description == "" {
		return nil
	}

	if len(description) > maxDescriptionBytes {
		description = description[:maxDescriptionBytes]
	}

	var (
		facts []Fact
		seen  = make(map[string]struct{}, len(knownFacts))
	)

	for index, line := range strings.Split(description, "\n") {
		if index >= maxDescriptionLines || len(facts) >= maxFacts {
			break
		}

		label, value, ok := parseFactLine(line)
		if !ok {
			continue
		}

		// First value wins. myWindsock prints a summary and then a per-split
		// table, and the summary is the one worth carrying.
		if _, duplicate := seen[label]; duplicate {
			continue
		}

		seen[label] = struct{}{}
		facts = append(facts, Fact{Label: label, Value: value})
	}

	return facts
}

// parseFactLine reads one line, or reports that there is nothing in it.
func parseFactLine(line string) (label, value string, ok bool) {
	// A separator is required. Without one there is no claim being made, just
	// prose — and prose is what this deliberately does not forward.
	colon := strings.IndexAny(line, ":=")
	if colon < 0 {
		return "", "", false
	}

	rawLabel := strings.ToLower(strings.TrimSpace(line[:colon]))
	rawValue := strings.TrimSpace(line[colon+1:])

	if rawLabel == "" || rawValue == "" ||
		len([]rune(rawLabel)) > maxFactLabelRunes {
		return "", "", false
	}

	// Longest match first, so "vehicle count" is not shadowed by a shorter
	// pattern that happens to be a prefix of it.
	best := -1
	for i, pattern := range knownFacts {
		if !strings.HasPrefix(rawLabel, pattern.match) {
			continue
		}

		if best < 0 || len(pattern.match) > len(knownFacts[best].match) {
			best = i
		}
	}

	if best < 0 {
		return "", "", false
	}

	return knownFacts[best].label, sanitizeFactValue(rawValue), true
}

// sanitizeFactValue bounds one value and strips what a prompt should not carry.
//
// Control characters go because a value is rendered into a line-oriented
// prompt, and a newline inside one would let a description forge a heading —
// the cheapest possible prompt injection, and the one this format invites.
func sanitizeFactValue(value string) string {
	var b strings.Builder

	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteByte(' ')

			continue
		}

		if r < 0x20 || r == 0x7f {
			continue
		}

		b.WriteRune(r)
	}

	cleaned := strings.Join(strings.Fields(b.String()), " ")

	if runes := []rune(cleaned); len(runes) > maxFactValueRunes {
		cleaned = strings.TrimSpace(string(runes[:maxFactValueRunes])) + "…"
	}

	return cleaned
}
