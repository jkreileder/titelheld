package naming

import (
	"strings"
	"testing"
)

// figurePrompt is a prompt of the shape [BuildPrompt] writes, with figures in
// every block so that only RIDE and NOTES can be what states one.
const figurePrompt = `RIDE
- Sport: Ride
- Distance: 47.0 km
- Moving time: 109 min
- Average speed: 25.0 km/h
- Climbing: 26.3 m
- Weekday: Sunday
- Start hour: 15:00

ACHIEVEMENTS
- Sechs Kehren zum Musterberg
- Sprint 9

NOTES
- Headwind: 56%

RECENT
- Neun Brücken bis Musterdorf

EXAMPLES
- 3 h, 9 climbs -> Drei Stunden Musterwald (de)`

// The titles the figure rule was written against are refused, each against
// the prompt it was written for, and the figure the refusal names is the one
// the title spells.
//
// The prompt carries a six and a nine under ACHIEVEMENTS, RECENT and EXAMPLES
// on purpose: only RIDE and NOTES state a figure, and a reader that took the
// whole prompt would let "Teil sechs" and "9 auf einen Streich" through.
func TestUnstatedFigureRefusesInventedFigures(t *testing.T) {
	t.Parallel()

	stated := StatedFigures(figurePrompt)

	cases := []struct {
		title string
		want  string
	}{
		{"Zwei Hügel und ein Sprint", "Zwei"},
		{"Mittwochabend, zwei Brücken, kaum Wind", "zwei"},
		{"Zwei Drittel Gegenwind, trotzdem 25er Schnitt", "Zwei"},
		{"Rückenwind-Reue: Sechs Strecken gegen den Gegenwind", "Sechs"},
		{"Flache Sechzig, Sprints gegen den Wind", "Sechzig"},
		{"Sprints gegen den Wind, Sonntag halb drei", "halb drei"},
		{"Das Pink-Panther-Erbe, Teil sechs: Rückenwind nach Huckenham", "sechs"},
		{"Pink Panther, sechs Mal Sinzing, Wind von vorn", "sechs"},
		{"Neun auf einen Streich", "Neun"},
		{"9 auf einen Streich", "9"},
		{"Three Bridges and a Sprint", "Three"},
		{"Dreißig Kilometer Gegenwind", "Dreißig"},
		{"25km und 2 Anstiege", "2"},
		{"Fast 26 km/h über den Musterdamm", "26"},
		{"Start um 15:30", "15:30"},
		{"Gegenwind seit 2026", "2026"},
		{"Halb 3 am Musterbach", "Halb 3"},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			t.Parallel()

			got, unstated := UnstatedFigure(tc.title, stated)
			if !unstated || got != tc.want {
				t.Errorf("UnstatedFigure(%q) = %q, %v; want %q, true", tc.title, got, unstated, tc.want)
			}
		})
	}
}

// A title whose figures the prompt states stands, and so does one with no
// figure at all.
//
// Each case is the control for a refusal above: the same kind of figure,
// stated this time, so a check that refused every figure fails here.
func TestUnstatedFigureAcceptsStatedFigures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prompt string
		title  string
	}{
		{"a stated percentage", figurePrompt, "Sinzinger Segmente und 56% Gegenwind"},
		{"a stated figure without its sign", figurePrompt, "Gegenwind, 56 Prozent der Zeit"},
		{"a NOTES temperature", "RIDE\n- Sport: Ride\n\nNOTES\n- Temperature: 23 °C", "Herbst mit 23 Grad und Gegenwind"},
		{"a figure inside a longer token", figurePrompt, "Gegenwind, trotzdem 25er Schnitt"},
		{"a decimal comma against a point", "RIDE\n- Average speed: 25.9 km/h", "25,9 km/h über den Inndamm"},
		{"a decimal point against a point", "RIDE\n- Average speed: 25.9 km/h", "25.9 km/h über den Inndamm"},
		{"a decimal integer against its whole value", "RIDE\n- Average speed: 26.0 km/h", "26 km/h über den Inndamm"},
		{"a time against the same time", figurePrompt, "Start um 15:00"},
		{"another stated percentage", "RIDE\n- Sport: Ride\n\nNOTES\n- Headwind: 57%", "57 Prozent Gegenwind"},
		{"no figure", figurePrompt, "Gegenwind über den Naßberg"},
		{"an article is not a number", figurePrompt, "Ein Sprint und eine Brücke"},
		{"a word whose value a run states", "RIDE\n- Climbing: 3.0 m", "Drei Meter Musterhang"},
		{"a word the prompt spells out", "RIDE\n- Sport: Ride\n\nNOTES\n- Intervals: sechs", "Sechs Intervalle am Musterbach"},
		{"a digit against a word the prompt spells out", "RIDE\n- Sport: Ride\n\nNOTES\n- Intervals: sechs", "6 Intervalle am Musterbach"},
		{"a compound outside the table", figurePrompt, "Dreiländereck im Gegenwind"},
		{"halb without a number is not a time", figurePrompt, "Halb so windig wie gedacht"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got, unstated := UnstatedFigure(tc.title, StatedFigures(tc.prompt)); unstated {
				t.Errorf("UnstatedFigure(%q) = %q, true; want the title to stand", tc.title, got)
			}
		})
	}
}

// "halb N" is a time derived from the start hour and is refused whatever the
// prompt states: the phrase names the half hour before N, so no start-hour
// line states it, and the prompt forbids deriving a time at all.
func TestHalfHourIsAlwaysRefused(t *testing.T) {
	t.Parallel()

	for _, prompt := range []string{
		"RIDE\n- Start hour: 03:00",
		"RIDE\n- Start hour: 15:00",
		"RIDE\n- Climbing: 3.0 m\n- Start hour: 15:00",
		"RIDE\n- Sport: Ride",
	} {
		stated := StatedFigures(prompt)

		for _, title := range []string{"Sonntag halb drei", "Sonntag halb 3", "Sonntag halb 15"} {
			if got, unstated := UnstatedFigure(title, stated); !unstated || !strings.HasPrefix(got, "halb ") {
				t.Errorf("UnstatedFigure(%q) against %q = %q, %v; want the halb time refused", title, prompt, got, unstated)
			}
		}
	}
}

// The Bike line states no figure: the gear name is the athlete's own text.
func TestStatedFiguresSkipTheBikeLine(t *testing.T) {
	t.Parallel()

	bike := StatedFigures("RIDE\n- Sport: Ride\n- Bike: Trek Domane SL 6\n- Distance: 40.5 km")
	if got, unstated := UnstatedFigure("Sechs Mal über den Musterberg", bike); !unstated || got != "Sechs" {
		t.Errorf("UnstatedFigure = %q, %v; want the six from the gear name refused", got, unstated)
	}

	// Control: the same six stated by a measured line stands.
	measured := StatedFigures("RIDE\n- Sport: Ride\n- Bike: Musterrad\n- Distance: 6.0 km")
	if got, unstated := UnstatedFigure("Sechs Mal über den Musterberg", measured); unstated {
		t.Errorf("UnstatedFigure = %q, true; want a six the distance states to stand", got)
	}
}

// The offered franchise entry states its own figures: it is the operator's
// configuration, and a series entry carrying a number must stay writable.
func TestStatedFiguresIncludeTheOfferedEntry(t *testing.T) {
	t.Parallel()

	title := "The Pink Panther 2 im Gegenwind"

	if got, unstated := UnstatedFigure(title, StatedFigures(figurePrompt, "The Pink Panther 2")); unstated {
		t.Errorf("UnstatedFigure(%q) = %q, true; want the entry's figure to stand", title, got)
	}

	// Control: without the entry the same figure is unstated.
	if got, unstated := UnstatedFigure(title, StatedFigures(figurePrompt)); !unstated || got != "2" {
		t.Errorf("UnstatedFigure(%q) = %q, %v; want %q refused with no entry offered", title, got, unstated, "2")
	}
}

// Only RIDE and NOTES state figures, and a "Start hour" outside RIDE is not
// the start hour.
func TestStatedFiguresReadsRideAndNotesOnly(t *testing.T) {
	t.Parallel()

	stated := StatedFigures(figurePrompt)

	for _, title := range []string{"Sechs Kehren", "Sprint 9", "Neun Brücken", "Drei Stunden"} {
		if _, unstated := UnstatedFigure(title, stated); !unstated {
			t.Errorf("UnstatedFigure(%q) stood on a figure outside RIDE and NOTES", title)
		}
	}

	notes := StatedFigures("RIDE\n- Sport: Ride\n\nNOTES\n- Start hour: 03:00")
	if _, unstated := UnstatedFigure("halb drei", notes); !unstated {
		t.Error("a NOTES line was read as the start hour")
	}
}

// figureLines writes a prompt carrying the figure lines a ride's prompt had,
// in the shape [BuildPrompt] writes them.
func figureLines(distance, moving, speed, hour, headwind, temperature string) string {
	return "RIDE\n- Sport: Ride\n- Distance: " + distance + " km\n- Moving time: " + moving +
		" min\n- Average speed: " + speed + " km/h\n- Weekday: Wednesday\n- Start hour: " + hour +
		"\n\nNOTES\n- Headwind: " + headwind + "\n- Temperature: " + temperature + " °C"
}

// The figure lines of real prompts state their numbers whole, and nothing a
// decimal's fractional digits or a rounding would make of them.
//
// Every headwind line carries a speed range such as "1.1 - 2.6 m/s", whose
// fractional digits are a two and a six; a reader that took digit runs from
// it would let "zwei" and "sechs" through. "26" against "26.3" is a rounding
// and is refused like an invented figure.
func TestUnstatedFigureAgainstRealFigureLines(t *testing.T) {
	t.Parallel()

	prompt3 := figureLines("40.5", "97", "25.0", "14:00", "66% @ 1.1 - 2.6 m/s", "20.1-23")
	prompt7 := figureLines("58.0", "132", "26.3", "16:00", "56% @ 1.3 - 2.6 m/s", "19-21.9")
	prompt6 := figureLines("63.2", "141", "26.8", "14:00", "65% @ 1.5 - 1.8 m/s", "19.6-20")
	prompt5 := figureLines("34.0", "79", "25.8", "17:00", "33% @ 0.3 - 2.5 m/s", "18.4-22.7")
	prompt4 := "RIDE\n- Sport: Ride\n- Average speed: 25.6 km/h"

	cases := []struct {
		prompt string
		title  string
		want   string // empty: the title stands
	}{
		{prompt3, "Zwei Hügel und ein Sprint", "Zwei"},
		{prompt3, "Zwei Drittel Gegenwind, trotzdem 25er Schnitt", "Zwei"},
		{prompt3, "Pink Panther gegen den Gegenwind, Teil zwei", "zwei"},
		{prompt3, "Gravel, Wind und 25er Schnitt", ""},
		{prompt3, "Herbst mit 23 Grad und Gegenwind", ""},
		{prompt7, "Pink Panther, sechs Mal Sinzing, Wind von vorn", "sechs"},
		{prompt7, "Mittwochs 26 im Schnitt", "26"},
		{prompt7, "Feierabendsprint, 26er Schnitt", "26"},
		{prompt7, "Gegenwind, 56 Prozent der Zeit", ""},
		{prompt6, "Rückenwind-Reue: Sechs Strecken gegen den Gegenwind", "Sechs"},
		{prompt6, "Flache Sechzig, Sprints gegen den Wind", "Sechzig"},
		{prompt6, "Sprints gegen den Wind, Sonntag halb drei", "halb drei"},
		{prompt6, "Mit 65% Gegenwind gegen den Bus", ""},
		{prompt5, "Gravel nach Lohstadt, zwei Drittel Rückenwind", "zwei"},
		{prompt4, "Feierabendsprint, 25.6 im Schnitt", ""},
		{prompt4, "Mit 25,6 im Schnitt über die neue Brücke", ""},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			t.Parallel()

			got, unstated := UnstatedFigure(tc.title, StatedFigures(tc.prompt))
			if want := tc.want != ""; unstated != want || got != tc.want {
				t.Errorf("UnstatedFigure(%q) = %q, %v; want %q, %v", tc.title, got, unstated, tc.want, want)
			}
		})
	}
}
