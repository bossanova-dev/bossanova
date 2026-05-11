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

// TestQuestionPromptFiresOnRequestUserInputCard verifies Codex's
// request_user_input picker is treated as a question prompt. This UI is not an
// approval menu, so it uses a notes/submit/interrupt footer instead of "Press
// 1-N or esc".
func TestQuestionPromptFiresOnRequestUserInputCard(t *testing.T) {
	pane := []byte("• Step 0 findings: existing extraction is real, but incomplete for the Linear fix.\n\n" +
		"  Question 1/1 (1 unanswered)\n" +
		"  D2 -- Decide whether WON-519 is phase-one extraction or full decomposition.\n\n" +
		"  › 1. A: Full seams (Recommended)  Add paste, autocomplete, selection, and drag seams with tests in this PR.\n" +
		"    2. B: Phase one only            Ship current chip/DOM extraction and capture remaining seams as follow-up work.\n" +
		"    3. None of the above            Optionally, add details in notes (tab).\n\n" +
		"  tab to add notes | enter to submit answer | esc to interrupt\n")
	if !hasCodexQuestionPrompt(pane) {
		t.Errorf("expected has_prompt=true for request_user_input card:\n%s", pane)
	}
}

func TestQuestionPromptFiresOnRequestUserInputCardAfterToolCall(t *testing.T) {
	pane := []byte("• Decision D2: full seams. Architecture review continues with that scope locked.\n\n" +
		"• Called\n" +
		"  └ context-mode.ctx_execute({\"language\":\"javascript\",\"code\":\"try{\\nconst fs=require('fs'), path=require('path');\\nconsole.log('## promptBarMentionFieldChips.ts')\\n}\\ncatch(e){console.log('ERR '+e.stack)}\",\"intent\":\"module coupling store DOM side effects line refs\",\"timeout\":10000})\n" +
		"    ## apps/frontend/src/components/PromptBar/promptBarMentionFieldChips.ts\n" +
		"    store import: 18:import type { Prefab, PrefabCategory } from '@/stores/prefabStore';\n\n" +
		"• PostToolUse hook (completed)\n" +
		"  hook context:\n\n" +
		"  Question 1/1 (1 unanswered)\n" +
		"  D3 -- Split the extracted chip module by responsibility.\n\n" +
		"  › 1. A: Split module (Recommended)  Separate factories, hydration, and reference-media sync with explicit inputs.\n" +
		"    2. B: Keep one module             Keep `promptBarMentionFieldChips.ts` as the extracted chip/hydration bucket for now.\n" +
		"    3. None of the above              Optionally, add details in notes (tab).\n\n" +
		"  tab to add notes | enter to submit answer | esc to interrupt\n")
	if !hasCodexQuestionPrompt(pane) {
		t.Errorf("expected has_prompt=true for request_user_input card after tool call:\n%s", pane)
	}
}

func TestQuestionPromptFiresOnRequestUserInputCardAfterPriorAnswer(t *testing.T) {
	pane := []byte("    answer: A: Split module (Recommended)\n\n" +
		"─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n\n" +
		"• Decision D3: split the extracted chip module. Architecture review continues.\n\n" +
		"  Question 1/1 (1 unanswered)\n" +
		"  D4 -- Choose reuse posture for existing PillEditor code.\n\n" +
		"  › 1. A: Share primitives (Recommended)  Reuse exact common helpers where contracts match; keep PromptBar-specific modules.\n" +
		"    2. B: Build shared editor             Unify PromptBar, PillEditor, and CanvasPillEditor around one shared pill system now.\n" +
		"    3. C: No sharing                      Keep PromptBar extraction fully separate and defer all DRY work.\n" +
		"    4. None of the above                  Optionally, add details in notes (tab).\n\n" +
		"  tab to add notes | enter to submit answer | esc to interrupt\n")
	if !hasCodexQuestionPrompt(pane) {
		t.Errorf("expected has_prompt=true for request_user_input card after prior answer:\n%s", pane)
	}
}

func TestQuestionPromptIgnoresSessionCompletePane(t *testing.T) {
	pane := []byte("• Waited for background terminal\n\n" +
		"─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n\n" +
		"• Session Complete\n\n" +
		"  Completed:\n\n" +
		"  - Fixed Codex question detection for request_user_input cards.\n" +
		"  - Added regressions for tool-output noise and prior-answer noise.\n" +
		"  - Squashed branch to one logical commit: 25d99be9 fix(codex): [#295] detect request input questions\n\n" +
		"  Quality gates:\n\n" +
		"  - make passed\n" +
		"  - make lint passed\n" +
		"  - make test passed\n\n" +
		"  Finalize status:\n\n" +
		"  - Pushed to origin with --force-with-lease\n" +
		"  - PR ready for review: https://github.com/recurser/bossanova/pull/295\n" +
		"  - Mergeable: MERGEABLE\n" +
		"  - GitHub checks: no failures; some pending, several passed\n" +
		"  - Working tree clean and up to date with origin\n\n" +
		"  Note: existing unrelated stashes remain in git stash list; I did not clear them.\n\n" +
		"─ Worked for 3m 41s ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n\n\n" +
		"› Explain this codebase\n")
	if hasCodexQuestionPrompt(pane) {
		t.Errorf("expected has_prompt=false for session complete pane:\n%s", pane)
	}
}

func TestQuestionPromptIgnoresStaleRequestUserInputCardBeforeSessionComplete(t *testing.T) {
	pane := []byte("Question 1/1 (1 unanswered)\n\n" +
		"How should the refactor be sliced?            Optionally, add details in notes (tab).\n\n" +
		"  tab to add notes | enter to submit answer | esc to interrupt\n\n" +
		"• Session Complete\n\n" +
		"  Completed:\n\n" +
		"  - Fixed Codex question detection for request_user_input cards.\n\n" +
		"─ Worked for 3m 41s ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n\n" +
		"› Explain this codebase\n")
	if hasCodexQuestionPrompt(pane) {
		t.Errorf("expected has_prompt=false for stale request_user_input card before session complete:\n%s", pane)
	}
}

func TestQuestionPromptIgnoresStaleWorkingSpinnerBeforeSessionComplete(t *testing.T) {
	pane := []byte("• Working (3s • esc to interrupt)\n\n" +
		"• Session Complete\n\n" +
		"Need approval?\n\n  1. Yes\n  2. No\n\nPress 1-2 or esc\n")
	if !hasCodexQuestionPrompt(pane) {
		t.Errorf("expected has_prompt=true for active question after stale working spinner:\n%s", pane)
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
