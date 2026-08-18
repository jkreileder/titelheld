// Package logsafe neutralises untrusted text before it reaches a log.
//
// Values that arrive from Strava, from a webhook body, or later from a
// geocoder or an LLM are attacker-influenced. Structured JSON logging already
// escapes them, but log output is read through many tools — a text handler, a
// terminal, a log viewer — and not all of them are as careful. Sanitising at
// the call site keeps that guarantee independent of which handler is installed.
package logsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLen bounds a sanitised value. Long enough for a Strava title or sport
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
		case unicode.IsControl(r):
			// Newlines, carriage returns and escape sequences are exactly what
			// a forged log line needs.
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}

		count++
	}

	return b.String()
}

// Strings sanitises every element of a slice.
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
