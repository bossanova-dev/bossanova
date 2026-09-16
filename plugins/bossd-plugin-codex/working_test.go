package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func readPaneFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "panes", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// TestHasCodexWorkingIndicatorRealPanes runs the detector over the three live
// captures described in testdata/panes/working_indicator.provenance.txt. They
// are a discriminating set: two busy panes that differ in WHICH rule catches
// them, and one genuinely-idle control. The control is the load-bearing case —
// without it a detector that simply returned true for every ended codex turn
// would pass.
func TestHasCodexWorkingIndicatorRealPanes(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
		why     string
	}{
		{
			name:    "ended turn with a background terminal still running",
			fixture: "working_background_terminal.txt",
			want:    true,
			why:     "the reported bug: no spinner, composer available, child alive",
		},
		{
			name:    "live spinner with a background terminal",
			fixture: "working_spinner_background_terminals.txt",
			want:    true,
			why:     "esc-to-interrupt is on screen",
		},
		{
			name:    "ended turn with no background work",
			fixture: "idle_no_background_work.txt",
			want:    false,
			why:     "control: an ended codex turn is not working by itself",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasCodexWorkingIndicator(readPaneFixture(t, tt.fixture)); got != tt.want {
				t.Errorf("hasCodexWorkingIndicator(%s) = %v, want %v (%s)", tt.fixture, got, tt.want, tt.why)
			}
		})
	}
}

// TestHasCodexWorkingIndicatorBackgroundFooterIsTheOnlySignal proves the
// ended-turn pane is caught by the background-work footer specifically, and not
// incidentally by some other artefact of an ended turn. Deleting that one line
// from the busy fixture must turn the verdict over; otherwise the first test
// above would keep passing for the wrong reason.
func TestHasCodexWorkingIndicatorBackgroundFooterIsTheOnlySignal(t *testing.T) {
	pane := string(readPaneFixture(t, "working_background_terminal.txt"))
	if !hasCodexWorkingIndicator([]byte(pane)) {
		t.Fatal("fixture is not detected as working; the rest of this test proves nothing")
	}

	var kept []string
	removed := 0
	for _, line := range strings.Split(pane, "\n") {
		if codexBackgroundWork.MatchString(line) {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed != 1 {
		t.Fatalf("removed %d background-work lines, want exactly 1", removed)
	}
	if hasCodexWorkingIndicator([]byte(strings.Join(kept, "\n"))) {
		t.Error("pane still reads as working with the background-work footer removed")
	}
}

// TestHasCodexWorkingIndicatorSurvivesPanePadding pins the trailing-blank trim.
// tmux pads a capture to the pane height and codex draws its composer inline,
// so a tall terminal puts the chrome well above the raw end of the buffer. A
// window counted from the raw end would land entirely in padding and report a
// visibly-busy pane as idle.
func TestHasCodexWorkingIndicatorSurvivesPanePadding(t *testing.T) {
	pane := readPaneFixture(t, "working_background_terminal.txt")
	padded := append(append([]byte{}, pane...), []byte(strings.Repeat("\n", 40))...)
	if !hasCodexWorkingIndicator(padded) {
		t.Error("padded pane = false, want true (trailing blank rows ate the tail window)")
	}
}

// TestHasCodexWorkingIndicatorIgnoresProseAndScrollback covers the two ways a
// match could be forged. Agent prose mentioning background work never opens a
// line with the count, and a footer that has scrolled off the current screen is
// no longer evidence of anything.
func TestHasCodexWorkingIndicatorIgnoresProseAndScrollback(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		{
			name: "prose mentioning background work mid-line",
			pane: "• Summary\n\n  - Left 1 background terminal running for the smoke test.\n\n› Ask Codex to do anything\n",
			want: false,
		},
		{
			name: "markdown bullet leading the count",
			pane: "• Summary\n\n  - 2 background terminals running were cleaned up.\n\n› Ask Codex to do anything\n",
			want: false,
		},
		{
			name: "real footer shape still matches",
			pane: "• Summary\n\n  3 background terminals running · /ps to view · /stop to close\n\n› Ask Codex to do anything\n",
			want: true,
		},
		{
			name: "footer scrolled out of the current-screen window",
			pane: "  1 background terminal running · /ps to view · /stop to close\n" +
				strings.Repeat("filler output line\n", codexWorkingTailLines+5),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasCodexWorkingIndicator([]byte(tt.pane)); got != tt.want {
				t.Errorf("hasCodexWorkingIndicator = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHasWorkingIndicatorRPC pins the wiring: the RPC must report what the
// detector says, not the hardcoded false it returned before.
func TestHasWorkingIndicatorRPC(t *testing.T) {
	srv := &Server{}
	for _, tt := range []struct {
		fixture string
		want    bool
	}{
		{"working_background_terminal.txt", true},
		{"idle_no_background_work.txt", false},
	} {
		resp, err := srv.HasWorkingIndicator(context.Background(), &bossanovav1.HasWorkingIndicatorRequest{
			PaneContent: readPaneFixture(t, tt.fixture),
		})
		if err != nil {
			t.Fatalf("HasWorkingIndicator(%s): %v", tt.fixture, err)
		}
		if resp.GetIsWorking() != tt.want {
			t.Errorf("HasWorkingIndicator(%s) = %v, want %v", tt.fixture, resp.GetIsWorking(), tt.want)
		}
	}
}

// TestCodexWorkingMatchesMinuteScaleElapsed is the regression for the second
// defect found alongside the idle misreport: the spinner guard matched only
// sub-minute turns, so it went inert on exactly the long turns it protects.
func TestCodexWorkingMatchesMinuteScaleElapsed(t *testing.T) {
	for _, line := range []string{
		"• Working (3s • esc to interrupt)",
		"• Working (48s • esc to interrupt)",
		"• Working (34m 54s • esc to interrupt)",
		"• Working (1h 02m 03s • esc to interrupt)",
		"• Working (34m 54s • esc to interrupt) · 1 background terminal running · /ps to view · /stop to close",
	} {
		if !codexWorking.MatchString(line) {
			t.Errorf("codexWorking did not match live spinner line: %q", line)
		}
	}
	for _, line := range []string{
		"• Working on the parser now",
		"esc to interrupt",
		"• Worked for 25m 25s",
	} {
		if codexWorking.MatchString(line) {
			t.Errorf("codexWorking matched a non-spinner line: %q", line)
		}
	}
}

// TestCodexWorkingMatchesRealSpinnerFixture keeps the regex honest against a
// captured pane rather than only hand-written lines.
func TestCodexWorkingMatchesRealSpinnerFixture(t *testing.T) {
	if !codexWorking.Match(readPaneFixture(t, "working_spinner_background_terminals.txt")) {
		t.Error("codexWorking did not match the live spinner fixture")
	}
	if codexWorking.Match(readPaneFixture(t, "idle_no_background_work.txt")) {
		t.Error("codexWorking matched an idle pane")
	}
}
