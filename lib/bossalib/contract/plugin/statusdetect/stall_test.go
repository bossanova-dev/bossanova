package statusdetect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three fixtures under testdata/ are verbatim `tmux capture-pane -p -S -1000`
// dumps of the panes stalled by the 2026-08-16 bossd restart (BOS-889). Each one
// is a turn that died mid-background-wait: Claude never redrew its
// "✻ Waiting for 1 background agent to finish" footer away, so it froze in the
// transcript with the failure notice, the API-error banner, and the completed-turn
// summary rendered BELOW it. Measured against these bytes before the fix:
// HasWorkingIndicator = true, NormalizeSpinner spinnerPresent = true,
// IsTransientAPIError = false — all three wrong, and between them they pinned the
// chats WORKING for hours while the auto-resume lane stayed disarmed.
//
// The panes have since been recovered, so these captures cannot be re-taken. They
// are the only surviving evidence of the shape, which is why every claim the fix
// makes is asserted against them rather than against a hand-written imitation.
var stalledPaneFixtures = []struct {
	// name identifies the session the capture came from.
	name string
	// file is the capture's basename under testdata/.
	file string
	// summary is the completed-turn line that proves the turn ended. It is
	// asserted to be present so a fixture that lost it in transit fails loudly
	// instead of making the eviction tests pass for the wrong reason.
	summary string
}{
	{
		name:    "BOS-869 (PR #2068)",
		file:    "boss-f12ba00f-7a939ffb.plain.txt",
		summary: "✻ Worked for 19m 9s",
	},
	{
		name:    "BOS-737 (PR #2072)",
		file:    "boss-f12ba00f-e2170a1e.plain.txt",
		summary: "✻ Worked for 15m 24s",
	},
	{
		name:    "BOS-744 (PR #2069, narrow wrapped pane)",
		file:    "boss-f12ba00f-dbd5cfde.plain.txt",
		summary: "✻ Worked for 17m 41s",
	},
}

func readStalledPane(t *testing.T, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// TestStalledPaneFixturesCarryTheSignals is the anti-vacuity guard for every
// other test in this file. The eviction tests assert a detector returns false;
// a fixture that lost the footer in transit would satisfy them for entirely the
// wrong reason. So pin, first, that each capture really does contain the frozen
// footer inside the same current-screen window HasWorkingIndicator reads, and
// that the completed-turn summary really does render after it.
func TestStalledPaneFixturesCarryTheSignals(t *testing.T) {
	for _, tt := range stalledPaneFixtures {
		t.Run(tt.name, func(t *testing.T) {
			tail := LastNLines(StripANSI(readStalledPane(t, tt.file)), workingTailLines)
			footer := waitingForBackgroundAgentRe.FindAllIndex(tail, -1)
			if len(footer) == 0 {
				t.Fatalf("fixture no longer carries the background-agent footer inside the last %d lines", workingTailLines)
			}
			after := tail[footer[len(footer)-1][1]:]
			if !containsLine(after, tt.summary) {
				t.Fatalf("fixture no longer renders %q after the footer", tt.summary)
			}
		})
	}
}

// containsLine reports whether haystack contains a line whose trimmed content
// equals want. It exists only for the anti-vacuity guard above.
func containsLine(haystack []byte, want string) bool {
	for line := range strings.SplitSeq(string(haystack), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// TestHasWorkingIndicator_StalledBackgroundAgentPane is acceptance criterion 1:
// a turn that died mid-background-wait must not report WORKING. Before the fix
// all three returned true, on the strength of the frozen footer alone.
func TestHasWorkingIndicator_StalledBackgroundAgentPane(t *testing.T) {
	for _, tt := range stalledPaneFixtures {
		t.Run(tt.name, func(t *testing.T) {
			if HasWorkingIndicator(readStalledPane(t, tt.file)) {
				t.Error("a frozen background-agent footer under a completed-turn summary must not report working")
			}
		})
	}
}

// TestNormalizeSpinner_StalledBackgroundAgentPane is acceptance criterion 2.
// NormalizeSpinner computes spinnerPresent on its own path and feeds
// SetLiveness, so fixing only HasWorkingIndicator would leave this one lying.
func TestNormalizeSpinner_StalledBackgroundAgentPane(t *testing.T) {
	for _, tt := range stalledPaneFixtures {
		t.Run(tt.name, func(t *testing.T) {
			if _, present := NormalizeSpinner(readStalledPane(t, tt.file)); present {
				t.Error("a frozen background-agent footer under a completed-turn summary must not report a live spinner")
			}
		})
	}
}

// TestIsTransientAPIError_StalledBackgroundAgentPane is acceptance criterion 3.
// The banner these panes end on ("API Error: Connection lost mid-response…")
// names no status code and matched none of the original prose arms, so the
// auto-resume lane never armed for any of the three.
func TestIsTransientAPIError_StalledBackgroundAgentPane(t *testing.T) {
	for _, tt := range stalledPaneFixtures {
		t.Run(tt.name, func(t *testing.T) {
			if !IsTransientAPIError(readStalledPane(t, tt.file)) {
				t.Error("a connection-lost API-error banner must classify as transient")
			}
		})
	}
}

// TestHasWorkingIndicator_BackgroundAgentFooterEviction bounds the new freshness
// check in both directions on synthetic shapes, so a fixture drift cannot be the
// only thing holding the rule up. Over-eviction is the dangerous direction here:
// a genuinely-waiting chat misread as idle can be auto-prompted mid-work.
func TestHasWorkingIndicator_BackgroundAgentFooterEviction(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		{
			// The live shape: nothing but the agent's own dispatch chatter sits
			// under the footer.
			name: "live footer with no terminator below it",
			pane: "⏺ Dispatching the implementation extension:\n" +
				"\n" +
				"✻ Waiting for 1 background agent to finish\n" +
				"\n" +
				"  I'll pick up at the post-dispatch verification when it completes.\n",
			want: true,
		},
		{
			// A completed-turn summary from an EARLIER turn is not evidence about
			// this one — only a summary rendered after the footer proves the turn
			// carrying it has ended.
			name: "completed-turn summary above the footer does not evict",
			pane: "✻ Worked for 4m 2s\n" +
				"\n" +
				"⏺ Dispatching the implementation extension:\n" +
				"\n" +
				"✻ Waiting for 1 background agent to finish\n",
			want: true,
		},
		{
			name: "completed-turn summary below the footer evicts",
			pane: "✻ Waiting for 1 background agent to finish\n" +
				"\n" +
				"⏺ The dispatched extension died mid-run.\n" +
				"\n" +
				"✻ Worked for 19m 9s\n",
			want: false,
		},
		{
			// The second terminator shape: the turn can die on the banner without
			// ever printing a summary line.
			name: "api-error banner below the footer evicts",
			pane: "✻ Waiting for 2 background agents to finish\n" +
				"\n" +
				"⏺ API Error: Connection lost mid-response. The response above may be incomplete.\n",
			want: false,
		},
		{
			// The eviction must key on the LAST footer. A fresh wait dispatched
			// after a previous turn ended is live again.
			name: "a later footer below the summary is live again",
			pane: "✻ Waiting for 1 background agent to finish\n" +
				"✻ Worked for 19m 9s\n" +
				"⏺ Retrying the dispatch.\n" +
				"✻ Waiting for 1 background agent to finish\n",
			want: true,
		},
		{
			// spinnerElapsedRe anchors on "for <n><unit>"; a glyph-led line that
			// merely mentions a duration in prose position is ordinary output and
			// must not evict a live footer.
			name: "glyph-led prose mentioning a duration does not evict",
			pane: "✻ Waiting for 1 background agent to finish\n" +
				"· Compiled 12 files in 4s\n",
			want: true,
		},
		{
			// The dual-use half of spinnerGlyphs (· • *) is each agent's ordinary
			// output bullet as well as a working frame, and "for <n><unit>" is
			// ordinary English. A completed-turn summary is a ✻-family frame line,
			// so a bulleted output line quoting a duration must not evict a live
			// footer — over-eviction here auto-prompts a chat that is mid-work.
			name: "middle-dot output quoting a duration does not evict",
			pane: "✻ Waiting for 1 background agent to finish\n" +
				"· Waited for 5m before retrying\n",
			want: true,
		},
		{
			name: "codex bullet output quoting a duration does not evict",
			pane: "✻ Waiting for 1 background agent to finish\n" +
				"• Polled for 30s\n",
			want: true,
		},
		{
			name: "markdown bullet quoting a duration does not evict",
			pane: "✻ Waiting for 1 background agent to finish\n" +
				"* ran for 12s\n",
			want: true,
		},
		{
			// The design constraint from question_test.go's ⏺ main guard, restated
			// as a status-bar row: the status bar can forge a response marker, but
			// it cannot forge a completed-turn summary.
			name: "trailing status-bar rows do not evict",
			pane: "✻ Waiting for 1 background agent to finish\n" +
				"\n" +
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · PR #710 · ← for agents\n" +
				"\n" +
				"  ⏺ main\n",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasWorkingIndicator([]byte(tt.pane)); got != tt.want {
				t.Errorf("HasWorkingIndicator = %v, want %v", got, tt.want)
			}
			if _, got := NormalizeSpinner([]byte(tt.pane)); got != tt.want {
				t.Errorf("NormalizeSpinner spinnerPresent = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsTransientAPIError_ConnectionLossArm pins the new prose arm's edges: it
// covers the wordings the CLI actually renders, it does not fire on the agent
// merely narrating a connection loss, and — the expensive case — it cannot
// overturn an explicit status code, so a 429 stays owned by the usage-limit lane
// even when its body happens to mention a lost connection.
func TestIsTransientAPIError_ConnectionLossArm(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "connection lost mid-response",
			in:   "⏺ API Error: Connection lost mid-response. The response above may be incomplete.\n",
			want: true,
		},
		{
			name: "connection reset",
			in:   "API Error: Connection reset by peer\n",
			want: true,
		},
		{
			name: "connection closed",
			in:   "API Error: Connection closed before a response was received\n",
			want: true,
		},
		{
			// Whole-line anchoring, unchanged: the agent talking about a lost
			// connection is not a banner.
			name: "prose quoting the connection loss is not a banner",
			in:   "⏺ The log shows \"API Error: Connection lost mid-response\" so I will retry.\n",
			want: false,
		},
		{
			// Status-code precedence: a 429 body that mentions a lost connection
			// must still lose to the code. The usage-limit lane owns 429.
			name: "429 whose body mentions a lost connection is not transient",
			in:   `API Error: 429 {"type":"error","error":{"message":"connection lost"}}` + "\n",
			want: false,
		},
		{
			name: "parenthesised 429 with connection-loss prose is not transient",
			in:   "API Error (429 Too Many Requests): connection reset\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransientAPIError([]byte(tt.in)); got != tt.want {
				t.Errorf("IsTransientAPIError(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
