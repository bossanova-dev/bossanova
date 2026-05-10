package main

import (
	"os"
	"testing"
)

// TestQuestionPromptFiresOnApprovalMenu verifies the approval-menu
// detector fires on a synthetic numbered approval menu.
func TestQuestionPromptFiresOnApprovalMenu(t *testing.T) {
	pane := []byte("Allow command `git push --force`?\n\n  1. Yes\n  2. No\n  3. Always allow\n\nPress 1-3 or esc\n")
	if !hasCodexQuestionPrompt(pane) {
		t.Errorf("expected has_prompt=true for numbered approval menu:\n%s", pane)
	}
}

// TestQuestionPromptIgnoresWorkingSpinner verifies the detector refuses to
// fire while codex is working — even if other approval-menu-shaped text
// happens to be on the pane (e.g. transcript history scroll-back).
func TestQuestionPromptIgnoresWorkingSpinner(t *testing.T) {
	pane := []byte("• Working (3s • esc to interrupt)\n\n  1. Yes\n  2. No\n\nPress 1 or esc\n")
	if hasCodexQuestionPrompt(pane) {
		t.Error("expected has_prompt=false while working spinner is visible")
	}
}

// TestQuestionPromptIgnoresUserPromptHistory verifies that a historical
// user message containing the literal "1. Yes" does not trip the detector.
// Codex TUI prefixes prior user messages with "› ", and the detector
// strips those before matching.
func TestQuestionPromptIgnoresUserPromptHistory(t *testing.T) {
	pane := []byte("› 1. Yes please write the doc\n\nThe model thought about it and replied.\n")
	if hasCodexQuestionPrompt(pane) {
		t.Error("expected has_prompt=false: historical user prompt should not trip approval detector")
	}
}

// TestQuestionPromptIgnoresActivityBullets verifies that activity bullets
// (codex emits "• <something>" status lines between turns) don't bleed
// into the approval-detection regex.
func TestQuestionPromptIgnoresActivityBullets(t *testing.T) {
	pane := []byte("• read main.go\n• write fix.patch\n• 1. Yes — apply?\n")
	// Note: bullets are stripped before the approval regex runs, so the
	// "1. Yes — apply?" line — if it had been a real numbered menu — would
	// not be detected here because it's prefixed with the bullet. This
	// mirrors the live TUI: real menus never use bullet prefixes.
	if hasCodexQuestionPrompt(pane) {
		t.Error("expected has_prompt=false for activity-bullet noise")
	}
}

// TestQuestionPromptRealPaneFixture is opt-in: when an operator drops a
// real codex pane capture into testdata/panes/question.txt the test
// verifies the detector fires on production output. Lane 0 didn't capture
// a pane fixture, so the file is absent on a fresh checkout — we skip
// rather than fail.
func TestQuestionPromptRealPaneFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/question.txt")
	if err != nil {
		t.Skip("no testdata/panes/question.txt fixture; skipping real-pane assertion")
	}
	if !hasCodexQuestionPrompt(data) {
		t.Errorf("expected has_prompt=true on real codex pane fixture (%d bytes)", len(data))
	}
}
