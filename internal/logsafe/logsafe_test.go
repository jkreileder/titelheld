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

func TestStringNeutralisesNonCcCategories(t *testing.T) {
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
