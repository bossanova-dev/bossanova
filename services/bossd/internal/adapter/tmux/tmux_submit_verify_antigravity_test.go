package tmux

import "testing"

// antigravityFixturePayload is the three-line prompt the 200x50 Antigravity
// fixtures were captured with (testdata/panes/antigravity_panes_provenance.txt).
const antigravityFixturePayload = "Create a file named hello.txt in the current directory\n" +
	"containing exactly the word hi, using your file writing tool.\n" +
	"Then reply with the word done."

// Antigravity's slash menu repeats the typed command on a ">" row below the
// input box, and the earlier turn above it is another ">" row. The send must
// read the box between them, or the Enter retry sees a foreign composer and the
// withdrawal finds no box to clear.
func TestAntigravityComposerIsTheRuledBoxBetweenEchoAndSlashMenu(t *testing.T) {
	const payload = "/agents"
	pane := readPaneFixture(t, "antigravity_slash_menu_real.txt")
	if got := composerFootprint(pane, payload); got != payload {
		t.Errorf("composerFootprint = %q, want the typed %q", got, payload)
	}
	if !lineStillAtPrompt(pane, payload) {
		t.Error("the typed command is still in the input box, so it must read as at the prompt")
	}
	styled := readPaneFixture(t, "antigravity_slash_menu_styled_real.txt")
	if !composerHoldsOnlyPayload(styled, composerDrawing{text: payload}) {
		t.Error("the withdrawal must read the ruled box as holding exactly the typed command")
	}
}

// While the approval menu hides the input box, the submitted prompt's own echo
// is the ">" row under the bottom two rules. Reading it as the composer would
// call the submit pending and send C-u, the payload and Enter into the menu,
// where Enter approves the first option.
func TestAntigravityApprovalMenuEchoIsNotAPendingComposer(t *testing.T) {
	pane := readPaneFixture(t, "antigravity_approval_modal_real.txt")
	if !multiLineSubmitted(pane, antigravityFixturePayload) {
		t.Error("a prompt the agent is already acting on must read as submitted")
	}
	if got := classifyComposerAfterClear(pane, antigravityFixturePayload); got == composerClearMissed {
		t.Error("the echo above the approval menu must not read as a composer still holding the payload")
	}
}

func TestAntigravityMultiLinePasteSubmission(t *testing.T) {
	if multiLineSubmitted(readPaneFixture(t, "antigravity_paste_multiline_stuck_real.txt"), antigravityFixturePayload) {
		t.Error("a paste still in the input box must not read as submitted")
	}
	if !multiLineSubmitted(readPaneFixture(t, "antigravity_idle_after_turn_real.txt"), antigravityFixturePayload) {
		t.Error("the idle pane after the turn, whose echo sits above an empty box, must read as submitted")
	}
}
