package statusdetect

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// claudeSpinner renders Claude's live working footer with the given gerund and
// elapsed counter. The separator is · (U+00B7) — the codex arm uses • (U+2022),
// which is why both grammars need their own fixtures.
func claudeSpinner(verb string, elapsed string) string {
	return "✻ " + verb + " (" + elapsed + " · esc to interrupt)"
}

// codexSpinner renders codex's live working footer. Note the • separator.
func codexSpinner(elapsed string) string {
	return "• Working (" + elapsed + " • esc to interrupt)"
}

func TestNormalizeSpinner_SpinnerOnlyRedrawIsNotSubstantive(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{
			name:   "claude elapsed counter ticks",
			before: "⏺ Reading the plan\n" + claudeSpinner("Working", "3s"),
			after:  "⏺ Reading the plan\n" + claudeSpinner("Working", "48s"),
		},
		{
			name:   "claude gerund rotates and counter ticks",
			before: "⏺ Reading the plan\n" + claudeSpinner("Thinking…", "3s"),
			after:  "⏺ Reading the plan\n" + claudeSpinner("Percolating…", "1m"),
		},
		{
			name:   "claude token counter grows",
			before: "⏺ Reading the plan\n✻ Cooking (12s · ↑ 1.2k tokens · esc to interrupt)",
			after:  "⏺ Reading the plan\n✻ Cooking (19s · ↑ 3.4k tokens · esc to interrupt)",
		},
		{
			name:   "codex elapsed counter ticks",
			before: "• Ran make test\n" + codexSpinner("3s"),
			after:  "• Ran make test\n" + codexSpinner("41s"),
		},
		{
			name:   "background-agent wait redraws",
			before: "⏺ Dispatching\n✻ Waiting for 1 background agent to finish",
			after:  "⏺ Dispatching\n✽ Waiting for 1 background agent to finish",
		},
		{
			name:   "completed-turn summary counter ticks",
			before: "⏺ Done\n✻ Cooked for 48s · 1 shell still running",
			after:  "⏺ Done\n✻ Cooked for 93s · 1 shell still running",
		},
		{
			// The other half of the completed-turn grammar: the token counter
			// grows between redraws for the same reason the elapsed one does.
			name:   "completed-turn summary token counter grows",
			before: "⏺ Done\n✻ Cooked for 48s · ↑ 1.2k tokens",
			after:  "⏺ Done\n✻ Cooked for 93s · ↑ 3.4k tokens",
		},
		{
			// Claude 2.1.312 ticks a running foreground tool's timer and
			// blinks its ⏺ glyph blank on alternate frames.
			name:   "running tool timer ticks and glyph blinks",
			before: "⏺ Counting files on the root filesystem · 7s\n" + claudeSpinner("Working", "13s"),
			after:  "  Counting files on the root filesystem · 1m 9s\n" + claudeSpinner("Working", "14s"),
		},
		{
			name:   "nbsp-rendered separator vs ascii separator",
			before: "⏺ Reading the plan\n✻ Working (3s · esc to interrupt)",
			after:  "⏺ Reading the plan\n✻ Working (3s · esc to interrupt)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeNorm, _ := NormalizeSpinner([]byte(tt.before))
			afterNorm, _ := NormalizeSpinner([]byte(tt.after))
			if string(beforeNorm) != string(afterNorm) {
				t.Errorf("spinner-only redraw changed the normalized content:\nbefore: %q\nafter:  %q", beforeNorm, afterNorm)
			}
			if tt.before == tt.after {
				t.Fatal("fixture is not actually a redraw — before and after are identical")
			}
		})
	}
}

// TestNormalizeSpinner_GenuineChangeStaysSubstantive is the over-normalization
// negative case: normalization must not swallow real agent output, including a
// single-character change immediately adjacent to (or on) a spinner line.
func TestNormalizeSpinner_GenuineChangeStaysSubstantive(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{
			name:   "one character changes on the line above the spinner",
			before: "⏺ Wrote 1 file\n" + claudeSpinner("Working", "3s"),
			after:  "⏺ Wrote 2 file\n" + claudeSpinner("Working", "3s"),
		},
		{
			name:   "one character changes on the line below the spinner",
			before: claudeSpinner("Working", "3s") + "\n  ⎿ ran 1 test",
			after:  claudeSpinner("Working", "3s") + "\n  ⎿ ran 2 test",
		},
		{
			name:   "new agent output appended under a codex spinner",
			before: "• Ran make test\n" + codexSpinner("3s"),
			after:  "• Ran make test\n• Ran make lint\n" + codexSpinner("3s"),
		},
		{
			name:   "spinner appears where there was none",
			before: "⏺ Idle at the composer",
			after:  "⏺ Idle at the composer\n" + claudeSpinner("Working", "3s"),
		},
		{
			name:   "shell count changes on a completed-turn summary",
			before: "⏺ Done\n✻ Cooked for 48s · 1 shell still running",
			after:  "⏺ Done\n✻ Cooked for 48s · 2 shells still running",
		},
		{
			name:   "prose that merely mentions the elapsed shape is untouched",
			before: "⏺ The build took 48s to finish",
			after:  "⏺ The build took 93s to finish",
		},
		{
			// The dual-use glyphs are the trap: • is codex's working frame AND
			// its ordinary output bullet, so a bulleted result line carrying a
			// duration must NOT be read as a status footer and have its number
			// erased. That would report "no substantive output" while the agent
			// is demonstrably emitting output — the silent false negative.
			name:   "codex bullet output whose only delta is a duration",
			before: "• Ran make test in 12s",
			after:  "• Ran make test in 47s",
		},
		{
			name:   "ascii bullet output whose only delta is a duration",
			before: "* Installed dependencies in 3s",
			after:  "* Installed dependencies in 9s",
		},
		{
			name:   "running tool description changes",
			before: "⏺ Counting files on the root filesystem · 7s",
			after:  "⏺ Counting files in the home directory · 7s",
		},
		{
			name:   "middot-led output whose only delta is a duration",
			before: "· Compiled 12 files in 4s",
			after:  "· Compiled 12 files in 8s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeNorm, _ := NormalizeSpinner([]byte(tt.before))
			afterNorm, _ := NormalizeSpinner([]byte(tt.after))
			if string(beforeNorm) == string(afterNorm) {
				t.Errorf("genuine content change was normalized away: %q", beforeNorm)
			}
		})
	}
}

func TestNormalizeSpinner_SpinnerPresent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "empty pane", content: "", want: false},
		{name: "idle composer", content: "⏺ All done\n\n❯ ", want: false},
		{name: "claude live spinner", content: "⏺ Working on it\n" + claudeSpinner("Working", "3s"), want: true},
		{name: "codex live spinner", content: "• Ran make test\n" + codexSpinner("3s"), want: true},
		{name: "codex live spinner with nbsp padding", content: "• Working (3s • esc to interrupt)", want: true},
		{name: "background agent wait", content: "⏺ Dispatching\n✻ Waiting for 2 background agents to finish", want: true},
		{
			name:    "spinner below trailing blank pane rows still counts",
			content: "⏺ Working on it\n" + claudeSpinner("Working", "3s") + strings.Repeat("\n", 40),
			want:    true,
		},
		{
			name:    "stale spinner scrolled far above the current screen does not count",
			content: claudeSpinner("Working", "3s") + strings.Repeat("\n⏺ later output", 40),
			want:    false,
		},
		{
			name:    "completed-turn summary is not a live spinner",
			content: "⏺ Done\n✻ Cooked for 48s · 1 shell still running",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := NormalizeSpinner([]byte(tt.content)); got != tt.want {
				t.Errorf("NormalizeSpinner spinnerPresent = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNormalizeSpinner_PreservesLineCount keeps the normalized buffer the same
// shape as the capture it came from, so a future caller that slices a tail off
// it sees the same window the raw capture would give.
func TestNormalizeSpinner_PreservesLineCount(t *testing.T) {
	content := "⏺ one\n" + claudeSpinner("Working", "3s") + "\n⏺ two\n"
	norm, _ := NormalizeSpinner([]byte(content))
	if got, want := strings.Count(string(norm), "\n"), strings.Count(content, "\n"); got != want {
		t.Errorf("normalized line count = %d, want %d", got, want)
	}
}

func TestCurrentClaudeSpinner(t *testing.T) {
	live := "· Beaming… (2m 34s · ↓ 5.8k tokens)"
	summary := "✻ Baked for 21m 28s · done Sunday 2:46 PM"
	for _, tt := range []struct {
		name, pane string
		want       bool
	}{
		{"current", live, true},
		{"second frame", "✳ Blanching… (1m 37s · ↓ 244 tokens)", true},
		{"custom verb", "* Reading all the files… (1h 2m 3s)", true},
		{"thought counter", "✻ Beaming… (2m 34s · ↓ 5.8k tokens · thought for 5s)", true},
		{"done", summary, false},
		{"stale frame", live + "\n" + summary, false},
		{"new turn", summary + "\n" + live, true},
		{"retry notice", live + "\nAPI Error: 500 Internal server error", true},
		{"output bullet", "• Ran make test in 12s", false},
		{"prose duration", "· Waited for 5m before retrying", false},
		{"parenthesized duration", "• Build finished (12s)", false},
		{"ellipsis output", "· Reading the config… done", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasWorkingIndicator([]byte(tt.pane)); got != tt.want {
				t.Errorf("HasWorkingIndicator = %v, want %v", got, tt.want)
			}
			_, got := NormalizeSpinner([]byte(tt.pane))
			if got != tt.want {
				t.Errorf("spinnerPresent = %v, want %v", got, tt.want)
			}
		})
	}
	a := []byte("output\n  " + live + "\n❯ \nstatus\n")
	b := []byte("output\n  ✻ Reading files… (3m 1s · ↓ 6k tokens · thought for 5s)\n❯ \nstatus\n")
	na, _ := NormalizeSpinner(a)
	nb, _ := NormalizeSpinner(b)
	if !bytes.Equal(na, nb) {
		t.Errorf("redraws differ: %q vs %q", na, nb)
	}
	if bytes.Count(na, []byte("\n")) != bytes.Count(a, []byte("\n")) {
		t.Fatal("normalization changed line count")
	}
	if isCompletedTurnSummary([]byte("✻ Beaming… (2m 34s · thought for 5s)")) {
		t.Error("live thought counter is not a completed turn")
	}
	if !isCompletedTurnSummary([]byte("✻ Worked for 19m 9s")) {
		t.Error("completed turn summary missed")
	}
}

func TestCurrentClaudeSpinnerRealPanes(t *testing.T) {
	for _, name := range []string{"claude_working_empty_composer_real.txt", "claude_swallowed_enter_midturn_real.txt"} {
		t.Run(name, func(t *testing.T) {
			pane, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			tail := LastNLines(StripANSI(pane), workingTailLines)
			if !bytes.Contains(tail, []byte("✳ Blanching… (1m")) || bytes.Contains(tail, []byte("esc to interrupt")) {
				t.Fatal("fixture must carry a current spinner without the legacy phrase")
			}
			if !HasWorkingIndicator(pane) {
				t.Error("real midturn pane must be working")
			}
			_, present := NormalizeSpinner(pane)
			if !present {
				t.Error("real midturn pane must have a live spinner")
			}
		})
	}
}

// readCapturedPane reads a pane written by testdata/capture-claude-spinner.sh.
func readCapturedPane(t *testing.T, name string) []byte {
	t.Helper()
	pane, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return pane
}

// TestCurrentClaudeSpinnerCapturedPanes pins detector verdicts against the
// script-captured panes. It asserts behaviour, never spinner verbs or counter
// values, so a recapture keeps passing unless detection regresses.
func TestCurrentClaudeSpinnerCapturedPanes(t *testing.T) {
	assertLive := func(t *testing.T, pane []byte) {
		t.Helper()
		if !HasWorkingIndicator(pane) {
			t.Error("captured midturn pane must be working")
		}
		if _, present := NormalizeSpinner(StripANSI(pane)); !present {
			t.Error("captured midturn pane must have a live spinner")
		}
	}

	t.Run("midturn plain", func(t *testing.T) {
		pane := readCapturedPane(t, "claude_spinner_midturn_plain.txt")
		if bytes.Contains(LastNLines(StripANSI(pane), workingTailLines), []byte("esc to interrupt")) {
			t.Fatal("fixture must exercise the current row, not the legacy phrase")
		}
		assertLive(t, pane)
	})
	t.Run("midturn styled", func(t *testing.T) {
		assertLive(t, readCapturedPane(t, "claude_spinner_midturn_styled.txt"))
	})
	t.Run("redraw pair", func(t *testing.T) {
		a := readCapturedPane(t, "claude_spinner_redraw_a_plain.txt")
		b := readCapturedPane(t, "claude_spinner_redraw_b_plain.txt")
		if bytes.Equal(a, b) {
			t.Fatal("redraw frames must differ raw")
		}
		normA, presentA := NormalizeSpinner(a)
		normB, presentB := NormalizeSpinner(b)
		if !presentA || !presentB {
			t.Errorf("both redraw frames must have a live spinner, got %v and %v", presentA, presentB)
		}
		if !bytes.Equal(normA, normB) {
			t.Error("spinner-only redraws must normalize to equal bytes")
		}
	})
	t.Run("narrow", func(t *testing.T) {
		assertLive(t, readCapturedPane(t, "claude_spinner_narrow_plain.txt"))
	})
	t.Run("completed turn", func(t *testing.T) {
		pane := readCapturedPane(t, "claude_turn_done_plain.txt")
		if HasWorkingIndicator(pane) {
			t.Error("completed turn must not be working")
		}
		if _, present := NormalizeSpinner(pane); present {
			t.Error("completed turn must not have a live spinner")
		}
	})
}
