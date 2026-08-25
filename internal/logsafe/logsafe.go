// Package logsafe neutralizes untrusted text before it reaches a log.
//
// Values that arrive from Strava, from a webhook body, or later from a
// geocoder or an LLM are attacker-influenced. Structured JSON logging already
// escapes them, but log output is read through many tools — a text handler, a
// terminal, a log viewer — and not all of them are as careful. Sanitizing at
// the call site keeps that guarantee independent of which handler is installed.
package logsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLen bounds a sanitized value. Long enough for a Strava title or sport
// type, short enough that a large field cannot flood the log.
const MaxLen = 256

// truncationMarker is appended when a value is cut short.
const truncationMarker = "…"

// String returns s with control characters removed and its length bounded, so
// it cannot forge log lines, move a terminal cursor, or fill a log file.
func String(s string) string {
	if s == "" {
		return ""
	}

	var b strings.Builder

	b.Grow(len(s))

	count := 0

	for _, r := range s {
		if count >= MaxLen {
			b.WriteString(truncationMarker)

			break
		}

		switch {
		case r == utf8.RuneError:
			// Invalid UTF-8; drop it rather than pass the replacement through.
			continue
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
			// Cc covers newlines, carriage returns and escape sequences. Zl and
			// Zp (U+2028, U+2029) end a line for anything JavaScript-based, and
			// Cf includes U+202E, which reverses the rendering of everything
			// after it.
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}

		count++
	}

	return b.String()
}

// MaxBlockLen bounds a sanitized multi-line value.
//
// Sixteen kilobytes: a naming prompt is a few, and this is a ceiling against a
// runaway rather than a budget. Far larger than [MaxLen] because a value logged
// through [Block] is one whose whole content is the point.
const MaxBlockLen = 16 << 10

// Block sanitizes a value whose line structure carries meaning.
//
// [String] is right for a title or a sport type: it flattens newlines, because
// a value that should be one line forging several is exactly the attack. A
// prompt is the opposite — it is a newline-delimited format with named blocks,
// and flattening it would destroy the thing being logged.
//
// So newlines and tabs survive, and everything else that can move a cursor,
// reorder rendering or end a line for a JavaScript-based viewer does not.
//
// This does not make the content trustworthy. It makes it unable to forge
// structure in the log that carries it.
func Block(s string) string {
	if s == "" {
		return ""
	}

	var b strings.Builder

	// Bounded by the cap, not by the input: a caller handing this a megabyte
	// should not make it reserve a megabyte to emit sixteen kilobytes.
	b.Grow(min(len(s), MaxBlockLen))

	// Counted in bytes, because MaxBlockLen is a size. Counting runes would let
	// sixteen thousand four-byte characters produce sixty-four kilobytes from a
	// limit that says sixteen.
	size := 0

	for _, r := range s {
		if r == utf8.RuneError {
			continue
		}

		kept := r
		if r != '\n' && r != '\t' &&
			(unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)) {
			kept = ' '
		}

		if size+utf8.RuneLen(kept) > MaxBlockLen {
			b.WriteString(truncationMarker)

			break
		}

		b.WriteRune(kept)

		size += utf8.RuneLen(kept)
	}

	return b.String()
}

// Strings sanitizes every element of a slice.
func Strings(values []string) []string {
	if values == nil {
		return nil
	}

	safe := make([]string, len(values))
	for i, value := range values {
		safe[i] = String(value)
	}

	return safe
}
