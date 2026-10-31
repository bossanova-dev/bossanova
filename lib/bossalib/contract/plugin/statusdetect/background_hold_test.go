package statusdetect

import "testing"

func TestFinishedTurnBackgroundShellContract(t *testing.T) {
	pane := readCapturedPane(t, "claude_turn_done_background_shell_plain.txt")
	if _, spinner := NormalizeSpinner(pane); spinner {
		t.Fatal("finished turn has a live spinner")
	}
	if !HasWorkingIndicator(pane) {
		t.Fatal("background shell must retain the busy detector contract")
	}
}
