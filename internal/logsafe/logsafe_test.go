package logsafe

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain text is untouched", in: "GravelRide", want: "GravelRide"},
		{name: "umlauts survive", in: "Zur Arbeit über die Brücke", want: "Zur Arbeit über die Brücke"},
		{
			name: "a forged log line is defanged",
			in:   "create\nlevel=ERROR msg=\"everything is fine\"",
			want: "create level=ERROR msg=\"everything is fine\"",
		},
		{name: "carriage returns", in: "a\r\nb", want: "a  b"},
		{name: "tabs", in: "a\tb", want: "a b"},
		{name: "ANSI escapes", in: "a\x1b[2Kb", want: "a [2Kb"},
		{name: "null bytes", in: "a\x00b", want: "a b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := String(tt.in); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStringIsBounded(t *testing.T) {
	t.Parallel()

	got := String(strings.Repeat("x", MaxLen*4))

	if !strings.HasSuffix(got, truncationMarker) {
		t.Errorf("a long value was not marked as truncated: %q", got[len(got)-10:])
	}

	if runes := len([]rune(got)); runes != MaxLen+1 {
		t.Errorf("truncated to %d runes, want %d plus the marker", runes, MaxLen)
	}
}

func TestStringDropsInvalidUTF8(t *testing.T) {
	t.Parallel()

	if got := String("a\xffb"); got != "ab" {
		t.Errorf("String = %q, want invalid bytes dropped", got)
	}
}

func TestStrings(t *testing.T) {
	t.Parallel()

	if got := Strings(nil); got != nil {
		t.Errorf("Strings(nil) = %v, want nil", got)
	}

	got := Strings([]string{"activity:write", "read\nforged"})
	want := []string{"activity:write", "read forged"}

	if len(got) != len(want) {
		t.Fatalf("Strings returned %d values, want %d", len(got), len(want))
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Strings[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStringNeutralizesNonCcCategories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "line separator U+2028", in: "a\u2028b", want: "a b"},
		{name: "paragraph separator U+2029", in: "a\u2029b", want: "a b"},
		{name: "right-to-left override U+202E", in: "a\u202eb", want: "a b"},
		{name: "zero width joiner U+200D", in: "a\u200db", want: "a b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := String(tt.in); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Block keeps the structure that String is right to destroy.
func TestBlock(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain", in: "RIDE\n- Sport: Ride", want: "RIDE\n- Sport: Ride"},
		{name: "tabs survive", in: "a\tb", want: "a\tb"},
		{name: "carriage return goes", in: "a\r\nb", want: "a \nb"},

		// The reasons this exists: an escape sequence that moves a terminal
		// cursor, and the override that reverses everything after it.
		{name: "escape sequence", in: "a\x1b[31mred", want: "a [31mred"},
		{name: "bidi override", in: "a\u202eb", want: "a b"},
		{name: "line separator", in: "a\u2028b", want: "a b"},

		// Invalid UTF-8 is dropped rather than passed through as the
		// replacement character, the same as String does.
		{name: "invalid utf-8", in: "a\xffb", want: "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Block(tc.in); got != tc.want {
				t.Errorf("Block(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A prompt is a few kilobytes; a runaway is not the log's problem to carry.
//
// The bound is a size in bytes, so a multi-byte value has to be measured the
// same way: counting runes would let sixteen thousand four-byte characters
// produce sixty-four kilobytes from a limit that says sixteen.
func TestBlockIsBounded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fill string
	}{
		{name: "single-byte runes", fill: "x"},
		{name: "two-byte runes", fill: "ü"},
		{name: "three-byte runes", fill: "€"},
		{name: "four-byte runes", fill: "𝄞"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Twice the cap in bytes, whatever the rune width.
			repeats := (MaxBlockLen * 2) / len(tc.fill)

			got := Block(strings.Repeat(tc.fill, repeats))

			if !strings.HasSuffix(got, truncationMarker) {
				t.Fatal("a truncated block does not say it was truncated")
			}

			body := strings.TrimSuffix(got, truncationMarker)
			if len(body) > MaxBlockLen {
				t.Errorf("block body is %d bytes, over the %d-byte cap", len(body), MaxBlockLen)
			}

			// And it does not stop far short: a rune that would cross the cap
			// is dropped, so at most one rune's worth is missing.
			if len(body) < MaxBlockLen-len(tc.fill) {
				t.Errorf("block body is %d bytes, well under the %d-byte cap",
					len(body), MaxBlockLen)
			}
		})
	}
}

// A value that fits is returned whole, marker and all absent.
func TestBlockKeepsAValueThatFits(t *testing.T) {
	t.Parallel()

	in := strings.Repeat("ü", MaxBlockLen/2)

	if got := Block(in); got != in {
		t.Errorf("a value exactly at the cap was altered: %d bytes in, %d out",
			len(in), len(got))
	}
}

// String and Block disagree about newlines on purpose, and that is the whole
// distinction between them.
func TestBlockAndStringDifferOnNewlines(t *testing.T) {
	t.Parallel()

	const multi = "first\nsecond"

	if strings.Contains(String(multi), "\n") {
		t.Error("String kept a newline; a one-line value must not be able to forge two")
	}

	if !strings.Contains(Block(multi), "\n") {
		t.Error("Block flattened a newline; the structure is what it exists to preserve")
	}
}
