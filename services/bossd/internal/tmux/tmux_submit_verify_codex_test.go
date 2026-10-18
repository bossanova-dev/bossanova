package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

// TestRealCodexPastedContentPaneReadsAsNotSubmitted is the codex half of
// BOS-1506, against a real capture (codex-cli 0.161.0, capture_paste.sh): a
// 2 KB line pasted into the composer with no Enter collapses to "[Pasted
// Content 2181 chars]", which does not prefix-match the payload. It must read
// as held, while the rotating placeholder of a cleared composer
// (codex_submitted.txt) still reads as submitted — the reason the fix
// recognises the paste placeholder rather than treating any non-empty composer
// as pending.
func TestRealCodexPastedContentPaneReadsAsNotSubmitted(t *testing.T) {
	pane := readPaneFixture(t, "codex_paste_stuck_real.txt")
	if hint := composerHintOf(t, pane); !strings.HasPrefix(hint, "[Pasted Content") {
		t.Fatalf("invalid fixture: the live composer shows %q, not a collapsed paste", hint)
	}

	outcome, err := waitOnPane(t, pane, pasteFixtureLongLine())
	if outcome != OutcomeNotSubmitted || !errors.Is(err, errSubmissionPending) {
		t.Fatalf("outcome = %v, err = %v; want %v carrying errSubmissionPending", outcome, err, OutcomeNotSubmitted)
	}

	for _, submitted := range []struct{ fixture, payload string }{
		{"codex_paste_submitted_real.txt", pasteFixtureLongLine()},
		{"codex_submitted.txt", codexFixturePayload},
	} {
		outcome, err := waitOnPane(t, readPaneFixture(t, submitted.fixture), submitted.payload)
		if outcome != OutcomeSubmitted || err != nil {
			t.Fatalf("%s: outcome = %v, err = %v; want submitted", submitted.fixture, outcome, err)
		}
	}
}

// stuckComposerPayload is the send capture_codex_stuck_composer.sh types after
// the nine stuck lines, the tenth "$boss-verify" of the PR #3105 verify chat.
const stuckComposerPayload = "$boss-verify 3105 --claim a414c806"

// TestRealCodexStuckComposerReadsAsNotSubmitted replays the PR #3105 verify
// chat against real codex captures (capture_codex_stuck_composer.sh): nine
// earlier sends sit unsubmitted in the composer, so the "›" row shows the
// oldest of them, and the payload lands at the bottom of the block. Whether it
// sits on its own row or is appended to the last one, the composer holds it and
// the send is not submitted.
func TestRealCodexStuckComposerReadsAsNotSubmitted(t *testing.T) {
	for _, fixture := range []string{"codex_stuck_composer_real.txt", "codex_stuck_composer_appended_real.txt"} {
		t.Run(fixture, func(t *testing.T) {
			pane := readPaneFixture(t, fixture)
			if hint := composerHintOf(t, pane); hint != "$boss-verify 3105 --claim 308f9f37" {
				t.Fatalf("invalid fixture: the marker row shows %q, not the oldest stuck line", hint)
			}

			outcome, err := waitOnPane(t, pane, stuckComposerPayload)
			if outcome != OutcomeNotSubmitted || !errors.Is(err, errSubmissionPending) {
				t.Fatalf("outcome = %v, err = %v; want %v carrying errSubmissionPending", outcome, err, OutcomeNotSubmitted)
			}
			if composerEmptyOf(pane, stuckComposerPayload) {
				t.Fatal("composerEmptyOf reads a composer holding the payload as empty, which licenses a second copy")
			}
		})
	}
}

// TestComposerHoldsDraft separates a codex draft from the empty-composer
// placeholder on real styled captures, and pins the SGR reading that does it.
func TestComposerHoldsDraft(t *testing.T) {
	cases := []struct {
		name   string
		styled string
		want   bool
	}{
		{"real empty composer showing the dim placeholder", readPaneFixture(t, "codex_placeholder_styled_real.txt"), false},
		{"real composer holding nine stuck lines", readPaneFixture(t, "codex_stuck_composer_stale_styled_real.txt"), true},
		{"unstyled capture of the same draft carries no evidence", readPaneFixture(t, "codex_stuck_composer_stale_real.txt"), false},
		{"bare glyph", "\x1b[1m›\x1b[0m\n", false},
		{"draft on a continuation row only", "\x1b[1m›\x1b[0m \x1b[2mhint\x1b[0m\n  typed\n\n  footer\n", true},
		{"truecolor arguments are not read as dim", "\x1b[1m›\x1b[0m \x1b[2;38;2;2;2;2mhint\x1b[0m \x1b[38;2;2;2;2mtyped\x1b[39m\n", true},
		{"dim truecolor placeholder", "\x1b[1m›\x1b[0m \x1b[2;38;2;2;2;2mhint\x1b[0m\n", false},
		{"normal intensity clears dim", "\x1b[1m›\x1b[0m \x1b[2mhint\x1b[22m typed\n", true},
		{"no composer at all", "\x1b[1m• Working\x1b[0m\n", false},
		// Claude Code 2.1.302 renders a placeholder as inverse(p[0]) + dim(p[1:])
		// and its cursor as an inverse cell; these are those bytes as tmux 3.7
		// re-encodes them under capture-pane -e.
		{"claude placeholder behind the cursor", "────\n❯ \x1b[7mT\x1b[0;2mry \"fix typecheck errors\"\x1b[0m\n────\n", false},
		{"claude empty composer", "────\n❯ \x1b[7m \x1b[0m\n────\n", false},
		{"claude draft before the cursor", "────\n" + claudeGlyph + " half-written draft \x1b[7m \x1b[0m\n────\n", true},
		{"claude draft with the cursor on its first letter", "────\n" + claudeGlyph + " \x1b[7mh\x1b[0malf-written draft\n────\n", true},
		{"inverse is cleared by 27", claudeGlyph + " \x1b[7mT\x1b[27mry\n", true},
		{"truecolor arguments are not read as inverse", "❯ \x1b[38;2;7;7;7mtyped\x1b[0m\n", true},
		// Claude Code 2.1.302 draws an IDE selection as a pill in its "ide"
		// colour ahead of the placeholder or the text.
		{"ide pill before the placeholder", "────\n❯ \x1b[38;2;71;130;200m[⧉ 3 lines from main.go] \x1b[7;39mT\x1b[0;2mry \"fix typecheck errors\"\x1b[0m\n────\n", false},
		{"ide pill in an ansi colour", "────\n❯ \x1b[94m[⧉ In main.go] \x1b[39;7mT\x1b[0;2mry\x1b[0m\n────\n", false},
		{"ide pill in a 256 colour", "────\n❯ \x1b[38;5;68m[⧉ In main.go] \x1b[39;7mT\x1b[0;2mry\x1b[0m\n────\n", false},
		{"ide pill before a draft", "────\n❯ \x1b[38;2;71;130;200m[⧉ 3 lines from main.go]\x1b[39m half-written draft\x1b[7m \x1b[0m\n────\n", true},
		{"a pill typed in the default colour is a draft", "────\n" + claudeGlyph + " [⧉ 3 lines from main.go] \x1b[7m \x1b[0m\n────\n", true},
		{"a pill whose colour ends early is a draft", "────\n❯ \x1b[94m[⧉ In\x1b[39m main.go] \x1b[7m \x1b[0m\n────\n", true},
		{"a coloured bracket without the selection glyph is a draft", "────\n❯ \x1b[94m[main.go] \x1b[39;7mT\x1b[0;2mry\x1b[0m\n────\n", true},
		{"dim carries onto a wrapped row", "────\n❯ \x1b[2mhint that\nwraps\x1b[0m\n────\n", false},
		// Under NO_COLOR Claude keeps only the inverse cursor, so a box styled
		// with nothing else cannot tell a placeholder from a draft.
		{"no-colour placeholder carries no evidence", "────\n❯ \x1b[7mT\x1b[0mry \"fix typecheck errors\"\n────\n", false},
		{"no-colour draft carries no evidence", "────\n❯ half-written draft\x1b[7m \x1b[0m\n────\n", false},
		{"inverse styling outside the box is not evidence", "\x1b[1m⏺\x1b[0m reply\n────\n❯ \x1b[7mT\x1b[0mry\n────\n\x1b[2mfooter\x1b[0m\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composerHoldsDraft(tc.styled); got != tc.want {
				t.Fatalf("composerHoldsDraft = %v, want %v", got, tc.want)
			}
		})
	}
}

// claudeGlyph is Claude Code 2.1.302's prompt glyph in its default colour.
const claudeGlyph = "\x1b[38;2;153;153;153m❯\x1b[39m"

// composerHoldsOnlyPayload decides whether a not-submitted send may clear the
// composer, so it must never read an operator's text as the payload.
func TestComposerHoldsOnlyPayload(t *testing.T) {
	box := func(row string) string {
		return "⏺ reply\n────\n" + claudeGlyph + " " + row + "\n────\n  footer\n"
	}
	codex := func(rows string) string {
		return "\x1b[1m›\x1b[0m " + rows + "\n\n  \x1b[38;2;246;226;183mGPT-6.1-Sol default\x1b[39m\n"
	}
	cmd := composerDrawing{text: "/boss-repair"}
	short := composerDrawing{text: "summarise the repo"}
	threeLines := composerDrawing{text: "line one\nline two\nline three"}
	fourLines := composerDrawing{text: "line one\nline two\nline three\nline four", collapsed: true, placeholder: "[Pasted text #2 +3 lines]"}
	unread := composerDrawing{text: fourLines.text, collapsed: true}
	cases := []struct {
		name, styled string
		drawn        composerDrawing
		want         bool
	}{
		{"the payload before the cursor", box(cmd.text + "\x1b[7m \x1b[0m"), cmd, true},
		{"the payload and a dim argument hint", box(cmd.text + "\x1b[7m \x1b[0;2m<ticket-id>\x1b[0m"), cmd, true},
		{"the placeholder this send's paste drew", box("[Pasted text #2 +3 lines]\x1b[7m \x1b[0m"), fourLines, true},
		{"another paste's placeholder with the same line count", box("[Pasted text #5 +3 lines]\x1b[7m \x1b[0m"), fourLines, false},
		{"a placeholder when this send's own was never read", box("[Pasted text #2 +3 lines]\x1b[7m \x1b[0m"), unread, false},
		{"the payload wrapped over two rows", "────\n" + claudeGlyph + " line one\n  line two line three\n────\n", threeLines, true},
		{"a placeholder with another line count", box("[Pasted text #2 +5 lines]\x1b[7m \x1b[0m"), fourLines, false},
		{"a placeholder for a paste Claude draws as text", box("[Pasted text #2 +2 lines]\x1b[7m \x1b[0m"), threeLines, false},
		{"a placeholder for a short paste", box("[Pasted text #2]\x1b[7m \x1b[0m"), short, false},
		{"a placeholder for a typed command", box("[Pasted text #2]\x1b[7m \x1b[0m"), cmd, false},
		{"the text of a paste Claude collapses", "────\n" + claudeGlyph + " line one\n  line two\n  line three\n  line four\n────\n", fourLines, false},
		{"a partial command and its unfocused completion", box("/boss-re\x1b[2mpair\x1b[0m"), cmd, false},
		{"a partial command and its completion behind the cursor", box("/boss-re\x1b[7mp\x1b[0;2mair\x1b[0m"), cmd, false},
		{"the cursor on the payload's last letter", box("/boss-repai\x1b[7mr\x1b[0m"), cmd, false},
		{"text before the payload", box("half " + cmd.text + "\x1b[7m \x1b[0m"), cmd, false},
		{"text after the payload", box(cmd.text + " now\x1b[7m \x1b[0m"), cmd, false},
		{"the payload, a blank row and an operator's paragraph", "────\n" + claudeGlyph + " summarise the repo\n\n  my second paragraph\n────\n", short, false},
		{"two collapsed pastes", box("[Pasted text #1][Pasted text #2 +3 lines]\x1b[7m \x1b[0m"), fourLines, false},
		{"a collapsed paste and text", box("[Pasted text #2 +3 lines] and more\x1b[7m \x1b[0m"), fourLines, false},
		{"an empty composer", box("\x1b[7mT\x1b[0;2mry \"fix typecheck errors\"\x1b[0m"), cmd, false},
		{"a codex payload", codex(short.text), short, false},
		{"a codex payload, a blank row and an operator's paragraph", codex(short.text + "\n\n  my second paragraph the operator typed"), short, false},
		{"a codex collapsed paste", codex("[Pasted Content 2181 chars]"), fourLines, false},
		{"a codex draft", codex("$boss-verify 3105"), composerDrawing{text: "$boss-verify 3106"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composerHoldsOnlyPayload(tc.styled, tc.drawn); got != tc.want {
				t.Fatalf("composerHoldsOnlyPayload(%+v) = %v, want %v", tc.drawn, got, tc.want)
			}
		})
	}
}

// Claude Code 2.1.302 collapses a paste longer than 800 UTF-16 units or holding
// more than two newlines, after CR and CRLF become LF and a tab four spaces.
func TestClaudeCollapsesPaste(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		want bool
	}{
		"a short line":                 {"summarise the repo", false},
		"three lines":                  {"a\nb\nc", false},
		"four lines":                   {"a\nb\nc\nd", true},
		"three lines and a newline":    {"a\nb\nc\n", true},
		"CRLF lines":                   {"a\r\nb\r\nc", false},
		"CR lines":                     {"a\rb\rc\rd", true},
		"800 characters":               {strings.Repeat("x", 800), false},
		"801 characters":               {strings.Repeat("x", 801), true},
		"200 tabs":                     {strings.Repeat("\t", 200), false},
		"201 tabs":                     {strings.Repeat("\t", 201), true},
		"400 astral characters":        {strings.Repeat("😀", 400), false},
		"401 astral characters":        {strings.Repeat("😀", 401), true},
		"798 characters and two CRLFs": {strings.Repeat("x", 798) + "\r\n\r\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := claudeCollapsesPaste(tc.text); got != tc.want {
				t.Fatalf("claudeCollapsesPaste = %v, want %v", got, tc.want)
			}
		})
	}
}

// Claude composers drawn into a real tmux pane with the SGR sequences Claude
// Code 2.1.302 emits and read back with capture-pane -e
// (testdata/panes/capture_claude_composer_styled.sh), so the draft reading is
// pinned against tmux's own re-encoding rather than hand-written bytes.
func TestComposerHoldsDraftOnRenderedClaudeComposers(t *testing.T) {
	for name, want := range map[string]bool{
		"placeholder":          false,
		"suggestion":           false,
		"wrapped_suggestion":   false,
		"ide_pill_placeholder": false,
		"draft":                true,
		"ide_pill_draft":       true,
		"dim_row_above_draft":  true,
		"slash_completion":     true,
		"no_color_placeholder": false,
		"no_color_draft":       false,
	} {
		t.Run(name, func(t *testing.T) {
			styled := readPaneFixture(t, "claude_composer_"+name+"_styled_rendered.txt")
			if !strings.Contains(styled, "\x1b[") {
				t.Fatal("fixture carries no SGR; recapture with capture_claude_composer_styled.sh")
			}
			if got := composerHoldsDraft(styled); got != want {
				t.Fatalf("composerHoldsDraft = %v, want %v", got, want)
			}
		})
	}
}

// The same rendered composers, read for whether they hold only a given
// payload: the check a not-submitted send makes before clearing the box.
func TestComposerHoldsOnlyPayloadOnRenderedClaudeComposers(t *testing.T) {
	for _, tc := range []struct {
		fixture, payload string
		want             bool
	}{
		{"draft", "half-written draft", true},
		{"ide_pill_draft", "half-written draft", false},
		{"slash_completion", "/boss-repair", false},
		{"slash_completion", "/boss-re", false},
		{"placeholder", `Try "fix typecheck errors"`, false},
	} {
		t.Run(tc.fixture+" "+tc.payload, func(t *testing.T) {
			styled := readPaneFixture(t, "claude_composer_"+tc.fixture+"_styled_rendered.txt")
			if got := composerHoldsOnlyPayload(styled, composerDrawing{text: tc.payload}); got != tc.want {
				t.Fatalf("composerHoldsOnlyPayload = %v, want %v", got, tc.want)
			}
		})
	}
}

// codexSendOpts is a submit into a codex composer on test budgets.
func codexSendOpts() sendPlanOpts {
	return sendPlanOpts{
		deadline:         time.Second,
		pollInterval:     5 * time.Millisecond,
		readyMarker:      composerMarkerCodex,
		submitVerifyWait: 40 * time.Millisecond,
		submitVerifyTick: 5 * time.Millisecond,
	}
}

// TestSendRefusesCodexComposerHoldingDraft: a send into a codex composer that
// already holds a draft types nothing and comes back not submitted. Without the
// check, the frame captured before codex draws the payload shows only the stuck
// lines, whose oldest line does not match the payload, and the send read
// submitted — the PR #3105 incident.
func TestSendRefusesCodexComposerHoldingDraft(t *testing.T) {
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{readPaneFixture(t, "codex_stuck_composer_stale_real.txt")},
		styledCapture:      readPaneFixture(t, "codex_stuck_composer_stale_styled_real.txt"),
	}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendPlan(context.Background(), "boss-test-sess", stuckComposerPayload, codexSendOpts())
	if OutcomeOf(err) != OutcomeComposerOccupied || !errors.Is(err, ErrComposerOccupied) {
		t.Fatalf("err = %v (outcome %v); want %v carrying ErrComposerOccupied", err, OutcomeOf(err), OutcomeComposerOccupied)
	}
	for _, call := range fake.callsCopy() {
		if call.subcommand == "send-keys" {
			t.Fatalf("a key reached a composer holding a draft: %v", call)
		}
	}
}

// TestSendTypesIntoCodexComposerShowingPlaceholder: the dim placeholder of an
// empty codex composer is not a draft, so the send types and submits.
func TestSendTypesIntoCodexComposerShowingPlaceholder(t *testing.T) {
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"› Ask Codex to do anything\n"},
		styledCapture:      readPaneFixture(t, "codex_placeholder_styled_real.txt"),
	}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.sendPlan(context.Background(), "boss-test-sess", stuckComposerPayload, codexSendOpts()); err != nil {
		t.Fatalf("a send into an empty composer must submit: %v", err)
	}
	typed := false
	for _, call := range fake.callsCopy() {
		typed = typed || (call.subcommand == "send-keys" && slices.Contains(call.args, stuckComposerPayload))
	}
	if !typed {
		t.Fatal("the payload was never typed into an empty composer")
	}
}

// TestSendTypesIntoEmptyOpenCodeComposer: OpenCode draws its placeholder and
// its "┃  Build · <model>" status rail in plain colour rather than dim
// (opencode_empty_composer_styled_real.txt, opencode 1.18.21, capture-pane -e),
// so the codex draft reading would refuse every OpenCode send. An empty
// OpenCode composer takes the payload.
func TestSendTypesIntoEmptyOpenCodeComposer(t *testing.T) {
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"┃\n┃  Ask anything...\n"},
		styledCapture:      readPaneFixture(t, "opencode_empty_composer_styled_real.txt"),
	}
	c := NewClient(WithCommandFactory(fake.factory))
	opts := codexSendOpts()
	opts.readyMarker = "┃"
	opts.submitVerifyWait = 0

	if err := c.sendPlan(context.Background(), "boss-test-sess", "summarise the repo", opts); err != nil {
		t.Fatalf("a send into an empty OpenCode composer must not be refused: %v", err)
	}
	typed := false
	for _, call := range fake.callsCopy() {
		typed = typed || (call.subcommand == "send-keys" && slices.Contains(call.args, "summarise the repo"))
	}
	if !typed {
		t.Fatal("the payload was never typed into an empty OpenCode composer")
	}
}
