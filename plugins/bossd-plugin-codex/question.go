package main

import (
	"bytes"
	"regexp"
	"strings"
)

// codexWorking matches codex's "thinking" status line — the spinner that
// appears while the agent is working on a turn. We refuse to fire the
// question detector while this line is present so a slow turn is never
// mistaken for an approval prompt.
//
// Concrete shape per Lane 0 TUI grammar:
//
//   - Working (3s • esc to interrupt)
//
// Seconds are 1+ digits and the trailing "esc to interrupt" is stable.
var codexWorking = regexp.MustCompile(`• Working \(\d+s? • esc to interrupt\)`)

// codexApproval matches the trailing instruction line of a codex approval
// menu — the most stable, version-resilient anchor. Two grammars seen so
// far:
//
//   - 0.128.0 (Lane 0 spike): "Press 1-N or esc"
//   - 0.129.0 (live capture, testdata/panes/question.txt):
//     "Press enter to confirm or esc to cancel"
//
// Both are matched. The footer line never carries a "›" prefix so it
// survives the user-history stripper unchanged. Anchoring on the footer
// (rather than the numbered first row) avoids the 0.129.0 ambiguity where
// codex prepends "› " to row 1 of the menu, which collides with the
// user-prompt-history prefix the stripper removes.
//
// Multiline mode is required because the footer is one line of a
// multi-line menu.
var codexApproval = regexp.MustCompile(`(?m)(Press\s+enter\s+to\s+confirm\s+or\s+esc\s+to\s+cancel|Press\s+1[-/0-9]*\s+or\s+esc)`)

// codexRequestUserInput matches Codex's request_user_input picker. Unlike
// approval menus, this UI uses a notes/submit/interrupt footer and marks the
// active card with an unanswered-question counter.
var codexRequestUserInput = regexp.MustCompile(`(?ms)\bQuestion\s+[0-9]+/[0-9]+\s+\([1-9][0-9]*\s+unanswered\).*^\s*tab\s+to\s+add\s+notes\s+\|\s+enter\s+to\s+submit\s+answer\s+\|\s+esc\s+to\s+interrupt\s*$`)

// codexSessionComplete marks terminal output for a finished Codex session.
// Question UI above this marker is stale scrollback, not an active prompt.
var codexSessionComplete = regexp.MustCompile(`(?m)^\s*•\s+Session Complete\s*$`)

// hasCodexQuestionPrompt reports whether the given pane bytes look like a
// codex question/approval prompt the daemon should surface.
//
// The detector deliberately strips two classes of noise before matching:
//
//  1. User-prompt history lines beginning with U+203A "›". The codex TUI
//     replays prior user messages with a leading "› " prefix; if the user
//     ever typed "1. Yes" earlier in the chat that text would otherwise
//     trip the approval regex on every poll.
//
//  2. Activity bullets beginning with U+2022 "•". These include the
//     working spinner (which we additionally guard against by refusing to
//     fire while the working regex matches in the active pane) and
//     status lines codex prints between turns.
//
// We refuse to fire while codexWorking matches in the active pane —
// even if the approval regex would also match. A working spinner means
// the agent is producing output; treating it as a question state would
// trigger spurious notifications mid-turn.
func hasCodexQuestionPrompt(data []byte) bool {
	activeData := codexQuestionActivePane(data)
	if codexWorking.Match(activeData) {
		return false
	}
	if codexRequestUserInput.Match(activeData) {
		return true
	}

	// Strip "›" user-prompt-history and "•" activity-bullet lines so a
	// historical "1. Yes" in a user message doesn't trip the approval
	// regex. We rebuild the pane content line-by-line; bytes are kept on
	// the (intentionally rare) lines that survive both filters.
	var b strings.Builder
	b.Grow(len(activeData))
	for _, line := range bytes.Split(activeData, []byte{'\n'}) {
		trimmed := bytes.TrimLeft(line, " \t")
		if bytes.HasPrefix(trimmed, []byte("› ")) || bytes.HasPrefix(trimmed, []byte("• ")) {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}

	return codexApproval.MatchString(b.String())
}

func codexQuestionActivePane(data []byte) []byte {
	locs := codexSessionComplete.FindAllIndex(data, -1)
	if len(locs) == 0 {
		return data
	}
	return data[locs[len(locs)-1][1]:]
}
