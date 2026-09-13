package views

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// tenStepProgressBars rebuilds the ten-bar sequence from the BOS-1237 report: a
// single 94.7 MiB download redrawn ten times, each redraw newline-terminated
// because the setup child has no TTY. The fill grows by eight glyphs per step
// and the padding shrinks to match, so consecutive bars differ in the ■/space
// split, in the percentage, and in nothing else.
func tenStepProgressBars() []string {
	const width = 80
	bars := make([]string, 0, 10)
	for step := 1; step <= 10; step++ {
		filled := step * 8
		bars = append(bars, fmt.Sprintf("|%s%s| %3d%% of 94.7 MiB",
			strings.Repeat("■", filled), strings.Repeat(" ", width-filled), step*10))
	}
	return bars
}

// TestSetupOutputBuffer drives foldSetupLine over whole line sequences rather
// than single calls, because the rule under test is about what a sequence leaves
// behind: which element a redraw replaces, and which lines survive untouched.
func TestSetupOutputBuffer(t *testing.T) {
	bars := tenStepProgressBars()
	finalBar := bars[len(bars)-1]

	tests := []struct {
		name  string
		lines []string
		want  []string
	}{
		{
			// R1: the reported defect, inverted. Ten frames, one row.
			name:  "the ticket's ten-step sequence collapses to the final bar",
			lines: bars,
			want:  []string{finalBar},
		},
		{
			// R2: the retention cap evicted exactly this line before the fix.
			name:  "a non-progress line before the sequence survives it",
			lines: append([]string{"added 25 packages in 4s"}, bars...),
			want:  []string{"added 25 packages in 4s", finalBar},
		},
		{
			name:  "an empty buffer appends without attempting a replace",
			lines: []string{finalBar},
			want:  []string{finalBar},
		},
		{
			// Neither line can be a redraw of the other, and neither may panic
			// the helper on an empty-string index.
			name:  "an empty line and a whitespace-only line take the append path",
			lines: []string{"", "   ", ""},
			want:  []string{"", "   ", ""},
		},
		{
			// Digit-run erasure: 10 and 100 are different lengths, so only
			// replacing whole runs makes these two compare equal.
			name:  "10% collapses into 100% across a digit-run length change",
			lines: []string{"downloading 10% of 94.7 MiB", "downloading 100% of 94.7 MiB"},
			want:  []string{"downloading 100% of 94.7 MiB"},
		},
		{
			name:  "a changing trailing byte count still collapses",
			lines: []string{"fetching 40% of 94.7 MiB", "fetching 50% of 101.2 MiB"},
			want:  []string{"fetching 50% of 101.2 MiB"},
		},
		{
			// R2: percentages alone must not be enough — these are two distinct
			// metrics, and collapsing them would lose one outright.
			name:  "two different percentage-bearing metrics both survive",
			lines: []string{"Coverage: 82% statements", "Coverage: 91% branches"},
			want:  []string{"Coverage: 82% statements", "Coverage: 91% branches"},
		},
		{
			// KTD4: skeleton equality alone would collapse these, so the
			// percentage gate is what keeps them apart.
			name:  "equal skeletons with no percentage token both survive",
			lines: []string{"retrying connection 1", "retrying connection 2"},
			want:  []string{"retrying connection 1", "retrying connection 2"},
		},
		{
			// No backward scan: once other output has been printed, an earlier
			// row is finished and must never be rewritten.
			name:  "a progress line is not replaced across an intervening line",
			lines: []string{bars[0], "warning: 1 deprecated subdependency", bars[1]},
			want:  []string{bars[0], "warning: 1 deprecated subdependency", bars[1]},
		},
		{
			name:  "an embedded CR run is buffered as only its final segment",
			lines: []string{"bar 10%\rbar 20%\rbar 30%"},
			want:  []string{"bar 30%"},
		},
		{
			// The stored value is the sanitized one, so the next line compares
			// against `bar 30%` and not against the CR-joined blob.
			name:  "the next line compares against the sanitized value",
			lines: []string{"bar 10%\rbar 20%\rbar 30%", "bar 40%"},
			want:  []string{"bar 40%"},
		},
		{
			// A CR with nothing drawn after it returned the cursor and wrote
			// no replacement text, so it is not a redraw boundary. Collapsing
			// it to "" would blank a real line.
			name:  "a trailing CR does not blank the line",
			lines: []string{"added 25 packages in 4s\r"},
			want:  []string{"added 25 packages in 4s"},
		},
		{
			// Rune-wise collapsing: these bars differ only in how many
			// multi-byte ■ glyphs they carry.
			name:  "a multi-byte bar glyph collapses by rune",
			lines: []string{"[■■ ] 50%", "[■■■■■■] 100%"},
			want:  []string{"[■■■■■■] 100%"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf []string
			for _, line := range tt.lines {
				buf = foldSetupLine(buf, line)
			}
			if !slices.Equal(buf, tt.want) {
				t.Fatalf("foldSetupLine sequence =\n%#v\nwant\n%#v", buf, tt.want)
			}
		})
	}
}

// TestSetupOutputSkeletonCollapsesRunesNotBytes pins the rune-wise requirement
// directly. A byte-wise collapse leaves these two skeletons different — ■ is
// three distinct bytes (E2 96 A0), so no two adjacent bytes in a ■ run are ever
// equal and the run never collapses — which would make the ten-step bar
// sequence stack exactly as it does today.
func TestSetupOutputSkeletonCollapsesRunesNotBytes(t *testing.T) {
	short := setupLineSkeleton("|■■| 10%")
	long := setupLineSkeleton("|■■■■■■■■| 90%")
	if short != long {
		t.Fatalf("skeletons differ across ■ run length: %q vs %q (collapsing by byte, not by rune?)", short, long)
	}
}

// TestSetupOutputSkeletonStripsWhitespaceAndDigits pins the two erasures the
// bar comparison depends on, so a regression in either is reported as itself
// rather than as an opaque buffer-length mismatch.
func TestSetupOutputSkeletonStripsWhitespaceAndDigits(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "digit runs of different lengths erase alike", a: "x 7%", b: "x 100%", want: true},
		{name: "whitespace runs are stripped", a: "x   7%", b: "x 7%", want: true},
		{name: "digit runs separated by whitespace stay separate", a: "x 1 2%", b: "x 12%", want: false},
		{name: "a different literal still differs", a: "statements 7%", b: "branches 7%", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := setupLineSkeleton(tt.a) == setupLineSkeleton(tt.b); got != tt.want {
				t.Fatalf("setupLineSkeleton(%q)==setupLineSkeleton(%q) = %v (%q vs %q), want %v",
					tt.a, tt.b, got, setupLineSkeleton(tt.a), setupLineSkeleton(tt.b), tt.want)
			}
		})
	}
}
