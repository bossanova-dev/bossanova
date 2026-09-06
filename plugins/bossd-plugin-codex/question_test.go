package main

import (
	"crypto/sha256"
	"fmt"
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

// TestQuestionPromptIgnoresUserPromptHistoryNBSP verifies the history stripper
// still fires when the codex TUI renders the "›" prefix with a non-breaking
// space (U+00A0) instead of an ASCII space. A user who pasted "Press 1 or esc"
// into the chat earlier must not trip the approval detector. Without NBSP
// normalization the `bytes.HasPrefix(trimmed, "› ")` check would miss the line
// and codexApproval would false-fire.
func TestQuestionPromptIgnoresUserPromptHistoryNBSP(t *testing.T) {
	pane := []byte("› Press 1 or esc\n\nThe model thought about it and replied.\n")
	if hasCodexQuestionPrompt(pane) {
		t.Error("expected has_prompt=false: NBSP-prefixed user history should be stripped")
	}
}

// TestQuestionPromptFiresOnRequestUserInputCardNBSP verifies the
// request_user_input picker is still detected when the TUI renders its footer
// with non-breaking spaces (U+00A0) between tokens. The "\s"-based footer
// regex only matches after NBSP is normalized to ASCII space.
func TestQuestionPromptFiresOnRequestUserInputCardNBSP(t *testing.T) {
	pane := []byte("  Question 1/1 (1 unanswered)\n" +
		"  D2 -- Decide whether to proceed.\n\n" +
		"  › 1. A: Full seams (Recommended)\n" +
		"    2. B: Phase one only\n\n" +
		"  tab to add notes | enter to submit answer | esc to interrupt\n")
	if !hasCodexQuestionPrompt(pane) {
		t.Errorf("expected has_prompt=true for NBSP-rendered request_user_input card:\n%s", pane)
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

// realPaneFixtureDigest pins testdata/panes/question.txt, which is no longer
// only this module's business: services/bossd/internal/tmux keeps a byte copy
// (testdata/panes/codex_approval_menu.txt) and proves "this pane is refused
// with no keystroke sent" against it, while the test below proves "the real
// codex grammar calls this pane a modal". BOS-600's headline claim is the
// composition of the two, and it holds only while both sides read the same
// bytes. The module boundary forbids reading across it, so each side hashes
// its own copy against its own literal — a tripwire, not a proof: nothing here
// compares the two files, so a change that re-pins both literals would let them
// diverge green. What it buys is that divergence cannot happen QUIETLY: edit one
// copy and that side reddens, naming the other file in the failure.
const realPaneFixtureDigest = "82bc86a3bc9ff3425b94eee793731a34e70a4b5f8d5afc228ea7e7b5fe620c33"

// TestQuestionPromptRealPaneFixture verifies the detector fires on production
// output. It used to skip when the capture was absent — that was correct while
// nothing depended on the file, and wrong now: a missing or altered fixture
// would turn half of BOS-600's proof into a green skip. It fails instead.
func TestQuestionPromptRealPaneFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/question.txt")
	if err != nil {
		t.Fatalf("read real codex pane fixture: %v (services/bossd/internal/tmux copies this file; do not delete it)", err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != realPaneFixtureDigest {
		t.Fatalf("fixture digest = %s, want %s; services/bossd/internal/tmux/testdata/panes/codex_approval_menu.txt "+
			"must stay byte-identical and asserts against the same digest", got, realPaneFixtureDigest)
	}
	if !hasCodexQuestionPrompt(data) {
		t.Errorf("expected has_prompt=true on real codex pane fixture (%d bytes)", len(data))
	}
}

// TestQuestionPromptFiresOnReplyChoiceInstruction is BOS-1180's headline case.
// Codex asked a multiple-choice question as ordinary assistant prose — a
// numbered option list closed by "Reply 1, 2, or 3." — and left the composer
// live. None of the drawn-UI arms sees that pane, so the chat sat waiting with
// nobody told. The committed fixture is the reporter's own capture.
func TestQuestionPromptFiresOnReplyChoiceInstruction(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/reply_choice.txt")
	if err != nil {
		t.Fatalf("read reply-choice pane fixture: %v", err)
	}
	if !hasCodexQuestionPrompt(data) {
		t.Errorf("expected has_prompt=true on reply-choice pane fixture (%d bytes)", len(data))
	}
}

// TestReplyChoicePaneIsNotAModal is the regression that matters most in
// BOS-1180. The reply-choice pane's composer is LIVE — answering the question
// means typing into it — so the arm that recognises it must never reach
// blocks_input. It fails loudly if someone later folds the arm back above the
// modalOnly return in the shared body.
func TestReplyChoicePaneIsNotAModal(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/reply_choice.txt")
	if err != nil {
		t.Fatalf("read reply-choice pane fixture: %v", err)
	}
	if hasCodexModalPrompt(data) {
		t.Error("blocks_input = true for reply-choice pane; the composer is live and delivery must not be refused")
	}
}

// TestReplyChoiceInstructionGrammar pins the discriminator: two or more numeric
// options make a choice, one does not, and the word "reply" in ordinary prose
// is not an instruction at all. Each line is embedded in a pane so the assertion
// runs through the real detector rather than the bare regex.
func TestReplyChoiceInstructionGrammar(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"comma list with or", "Reply 1, 2, or 3.", true},
		{"two options with or", "Reply 1 or 2.", true},
		{"reply with", "Reply with 1, 2, or 3.", true},
		{"numeric range", "Reply 1-3.", true},
		{"respond verb", "Please respond 1 or 2.", true},
		{"answer verb", "Answer 1, 2, or 3", true},
		{"single option is not a choice", "Reply 1.", false},
		{"prose containing reply", "Reply to this when you can.", false},
		{"past tense with a number", "I replied 1 hour ago", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane := []byte("The agent weighed the tradeoffs and stopped.\n\n  " + tc.line + "\n")
			if got := hasCodexQuestionPrompt(pane); got != tc.want {
				t.Errorf("has_prompt = %v, want %v for line %q", got, tc.want, tc.line)
			}
		})
	}
}

// TestReplyChoiceIgnoresUserPromptHistory verifies a reply instruction replayed
// as user history cannot fire the arm. Codex prefixes prior user messages with
// "› " and the shared stripper deletes those lines before any matching runs.
// Note this pins the REGEX defence, not the stripper: codexBoxBorderClass
// excludes ›, so this pane stays negative even if the stripper is removed. No
// line the stripper drops can also match codexReplyChoiceInstruction — it only
// removes lines opening "› "/"• ", and neither glyph is in the class nor starts
// a verb — so on THIS arm the stripper is not what keeps a false positive out.
// It is still load-bearing here in the other direction: the tail is taken over
// the stripped buffer, so dropped history does not spend the 30-line window (a
// live "Reply 1, 2, or 3." followed by 31 "› " lines answers true today and
// false with the stripper removed). Its false-positive role belongs to the
// approval arm above, which its own tests cover.
func TestReplyChoiceIgnoresUserPromptHistory(t *testing.T) {
	pane := []byte("The agent finished the task and went idle.\n\n› Reply 1, 2, or 3.\n\n› Ask Codex to do anything\n")
	if hasCodexQuestionPrompt(pane) {
		t.Error("has_prompt = true for a reply instruction on a replayed user line, want false")
	}
}

// TestReplyChoiceIgnoresWorkingSpinner verifies the arm inherits the working
// guard for free: a turn still producing output is not a question, even when the
// instruction it is about to close with is already on screen.
func TestReplyChoiceIgnoresWorkingSpinner(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/reply_choice.txt")
	if err != nil {
		t.Fatalf("read reply-choice pane fixture: %v", err)
	}
	pane := append(append([]byte{}, data...), []byte("\n• Working (12s • esc to interrupt)\n")...)
	if hasCodexQuestionPrompt(pane) {
		t.Error("has_prompt = true while the working spinner is visible, want false")
	}
}

// TestReplyChoiceIgnoresScrolledOffInstruction pins the tail bound in the
// direction that stops an answered question wedging QUESTION for the rest of the
// session: once later output pushes the instruction above the rendered window,
// the arm stops firing.
//
// The appended lines are plain prose ON PURPOSE. The window is measured over the
// STRIPPED buffer, so "•"-prefixed activity bullets would be deleted before the
// count and the instruction would never leave the window — the test would pass
// while proving nothing. TestReplyChoiceScrollGuardIsNonVacuous asserts the same
// appended volume, minus the scroll, still fires.
func TestReplyChoiceIgnoresScrolledOffInstruction(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/reply_choice.txt")
	if err != nil {
		t.Fatalf("read reply-choice pane fixture: %v", err)
	}
	if hasCodexQuestionPrompt(append(append([]byte{}, data...), scrolledPastReplyChoice()...)) {
		t.Error("has_prompt = true for a reply instruction pushed above the rendered tail, want false")
	}
}

// TestReplyChoiceScrollGuardIsNonVacuous proves the test above measures the
// scroll rather than something the appended text did on its own: the identical
// pane with FEWER appended lines than the window still fires, so the false above
// is the window bound and not an accident of the filler.
func TestReplyChoiceScrollGuardIsNonVacuous(t *testing.T) {
	data, err := os.ReadFile("testdata/panes/reply_choice.txt")
	if err != nil {
		t.Fatalf("read reply-choice pane fixture: %v", err)
	}
	var short []byte
	for i := range 3 {
		short = append(short, []byte(fmt.Sprintf("  the agent kept printing ordinary prose, line %d\n", i))...)
	}
	if !hasCodexQuestionPrompt(append(append([]byte{}, data...), short...)) {
		t.Error("has_prompt = false with only 3 lines appended; the scroll test above proves nothing")
	}
}

// scrolledPastReplyChoice returns more rendered lines than codexModalTailLines,
// each one surviving the "›"/"•" stripper, so the fixture's instruction is
// pushed clear of the window.
func scrolledPastReplyChoice() []byte {
	var b []byte
	for i := range codexModalTailLines + 10 {
		b = append(b, []byte(fmt.Sprintf("  the agent kept printing ordinary prose, line %d\n", i))...)
	}
	return b
}
