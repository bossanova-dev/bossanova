package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The codex pane fixtures under testdata/panes are copies of the canonical
// captures owned by the codex plugin:
//
//	plugins/bossd-plugin-codex/testdata/panes/submit_submitted.txt -> codex_submitted.txt
//	plugins/bossd-plugin-codex/testdata/panes/submit_pending.txt   -> codex_pending.txt
//
// They are copied rather than shared because the plugin is a separate Go module,
// and this package follows the repo's "duplicate rather than depend across a
// module boundary" rule. Regenerate both sides with that package's
// capture-submit.sh, which pins the payload below and records the codex version
// in each pane's banner row.
//
// codexFixturePayload is the exact single-line payload the fixtures were
// captured with; the golden assertions below depend on it.
const codexFixturePayload = "write a haiku about autumn leaves"

// readPaneFixture loads a captured pane fixture. It fails rather than skips: a
// skip here would silently turn the BOS-597 regression guard vacuous in CI.
func readPaneFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "panes", name))
	if err != nil {
		t.Fatalf("read pane fixture %s: %v", name, err)
	}
	return string(data)
}

// TestLineStillAtPromptRealCodexPaneSubmitted is the BOS-597 regression guard,
// asserted against a REAL codex pane (codex-cli 0.146.0 — the version is
// self-recorded in the fixture's banner row) captured one second after a
// submitted single-line send, i.e. inside the window waitForSubmission polls.
//
// The captured pane refutes the "bottom-most marker is a history echo" theory
// and shows the actual mechanism:
//
//	13  › write a haiku about autumn leaves      <- history echo of the send
//	18  • Working (0s • esc to interrupt)        <- agent activity, ABOVE the box
//	21  › Improve documentation in @filename     <- the LIVE composer, cleared
//	23  gpt-5.6-terra high · … · Context 100% left
//
// The bottom-most marker row (21) really is the live input box, so resolving a
// different row would change nothing. It fails because a *cleared* codex
// composer renders a rotating placeholder hint instead of a bare glyph, so the
// glyph-only cleared-composer check reads it as "still holding the payload",
// while the working bullet sits ABOVE the composer where the activity scan
// never looks. Both positive signals miss, the budget elapses, and
// verifyWithEnterRetry fires a second Enter into a pane that already submitted.
func TestLineStillAtPromptRealCodexPaneSubmitted(t *testing.T) {
	pane := readPaneFixture(t, "codex_submitted.txt")

	// Fixture sanity: this must be a genuine post-submit pane whose composer
	// holds a placeholder rather than the payload — the exact shape that
	// produced the false "still pending".
	if !strings.Contains(pane, "• Working") {
		t.Fatal("invalid fixture: no codex working bullet, so this is not a post-submit pane")
	}
	if !strings.Contains(pane, "› Improve documentation in @filename") {
		t.Fatal("invalid fixture: no cleared composer rendering a placeholder hint")
	}

	if lineStillAtPrompt(pane, codexFixturePayload) {
		t.Fatalf("a real submitted codex pane must report submitted (false); "+
			"the payload %q has left the live composer", codexFixturePayload)
	}
}

// TestLineStillAtPromptRealCodexPanePending is the fail-safe half of BOS-597,
// asserted against a real codex pane captured with the payload typed into the
// live composer but Enter never sent. It must still report still-pending, so a
// swallowed Enter keeps surfacing loudly (BOS-89 / BOS-489).
//
// It also guards the tempting-but-wrong fix of scanning the WHOLE pane for
// agent activity: this fixture carries a "• You have 2 usage limit resets
// available." notice row that begins with the codex working bullet, above the
// composer. A whole-pane activity scan would read that as agent activity and
// report an unsubmitted payload as delivered — the silent no-op this cluster
// exists to prevent.
func TestLineStillAtPromptRealCodexPanePending(t *testing.T) {
	pane := readPaneFixture(t, "codex_pending.txt")

	if !strings.Contains(pane, "• You have") {
		t.Fatal("invalid fixture: missing the bullet-prefixed notice row this guards against")
	}
	if !strings.Contains(pane, "› "+codexFixturePayload) {
		t.Fatal("invalid fixture: the composer does not hold the unsubmitted payload")
	}

	if !lineStillAtPrompt(pane, codexFixturePayload) {
		t.Fatalf("a real codex pane still holding %q in its composer must report "+
			"still-at-prompt (true)", codexFixturePayload)
	}
}

// TestComposerHoldsPayload covers the composer-content predicate directly
// across both agents' layouts, including the wrapped-payload shape BOS-489
// hardened and the rotating codex placeholder hints that motivated BOS-597.
func TestComposerHoldsPayload(t *testing.T) {
	const payload = "implement the next ticket"
	long := strings.Repeat("verify the wide single-line payload wraps and still resolves ", 3)

	cases := []struct {
		name      string
		markerRow string
		payload   string
		want      bool
	}{
		{"claude composer holding the payload", "❯ " + payload, payload, true},
		{"claude composer cleared to a bare glyph", "❯", payload, false},
		{"codex composer holding the payload", "› " + payload, payload, true},
		{"codex composer cleared to a bare glyph", "›", payload, false},
		{
			name:      "codex cleared composer rendering a placeholder hint",
			markerRow: "› Improve documentation in @filename",
			payload:   payload,
			want:      false,
		},
		{
			name:      "codex cleared composer rendering a different placeholder hint",
			markerRow: "› Run /review on my current changes",
			payload:   payload,
			want:      false,
		},
		{
			// BOS-489: a payload wider than the pane wraps, so the composer row
			// holds only a prefix. It is still holding the payload.
			name:      "wrapped payload leaves only a prefix in the composer",
			markerRow: "❯ " + long[:78],
			payload:   long,
			want:      true,
		},
		{
			name:      "ascii marker composer holding the payload",
			markerRow: "> " + payload,
			payload:   payload,
			want:      true,
		},
		{"row carrying no prompt marker at all", "  some footer chrome", payload, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composerHoldsPayload(tc.markerRow, tc.payload); got != tc.want {
				t.Fatalf("composerHoldsPayload(%q, %q) = %v, want %v",
					tc.markerRow, tc.payload, got, tc.want)
			}
		})
	}
}
