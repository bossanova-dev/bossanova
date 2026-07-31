package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/recurser/bossalib/statusdetect"
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

// The request_user_input picker's grammar, kept as ONE source string per line
// so the four matchers below cannot desync: a tweak that reached only some of
// them would silently open a hole in the modal gate. The header is a `%s`
// template over the unanswered counter alone, because the locator and the
// liveness test must agree on what a header looks like — the locator has to
// match a strict superset, or it would slice from the wrong card.
//
// The header is anchored to the start of its line, past indentation and the
// TUI's box borders. Card bodies are free-form agent prose that routinely
// quotes prior terminal output, so an unanchored header would let a body line
// reading "…where Question 3/3 (0 unanswered) is shown…" pose as the start of
// the bottom-most card. Neither "›" nor "•" is in the border class: those
// prefix codex's replayed user history and activity bullets, which is exactly
// the text that must not be able to impersonate live UI.
const (
	codexQuestionCardHeaderFmt     = `^[\s\x{2502}\x{2503}\x{258C}\x{2588}|]*\bQuestion\s+[0-9]+/[0-9]+\s+\(%s\s+unanswered\)`
	codexQuestionCardAnyCount      = `[0-9]+`
	codexQuestionCardLiveCount     = `[1-9][0-9]*`
	codexQuestionCardFooterPattern = `^\s*tab\s+to\s+add\s+notes\s+\|\s+enter\s+to\s+submit\s+answer\s+\|\s+esc\s+to\s+interrupt\s*$`
)

func codexQuestionCardHeaderPattern(count string) string {
	return fmt.Sprintf(codexQuestionCardHeaderFmt, count)
}

// codexRequestUserInput matches Codex's request_user_input picker. Unlike
// approval menus, this UI uses a notes/submit/interrupt footer and marks the
// active card with an unanswered-question counter.
var codexRequestUserInput = regexp.MustCompile(`(?ms)` + codexQuestionCardHeaderPattern(codexQuestionCardLiveCount) + `.*` + codexQuestionCardFooterPattern)

// codexQuestionCardHeader matches a card header at any answered count — used to
// find where a card starts — and codexQuestionCardLive is the same header
// restricted to a card still waiting on someone. Keeping them separate lets
// "which card owns this footer" be asked apart from "is that card still open".
var (
	codexQuestionCardHeader = regexp.MustCompile(`(?m)` + codexQuestionCardHeaderPattern(codexQuestionCardAnyCount))
	codexQuestionCardLive   = regexp.MustCompile(`(?m)` + codexQuestionCardHeaderPattern(codexQuestionCardLiveCount))
)

// codexSessionComplete marks terminal output for a finished Codex session.
// Question UI above this marker is stale scrollback, not an active prompt.
var codexSessionComplete = regexp.MustCompile(`(?m)^\s*•\s+Session Complete\s*$`)

// codexUnicodeSpace matches Unicode space separators (category Zs): regular
// ASCII space U+0020, non-breaking space U+00A0, narrow NBSP U+202F, etc. The
// codex/Claude TUIs render prompt and layout padding with NBSP, but the "› "/
// "• " prefix checks and the "\s"-based approval/footer regexes below all
// assume ASCII whitespace; normalizing every Zs rune to U+0020 first keeps
// those matchers working regardless of which space byte the TUI emitted.
var codexUnicodeSpace = regexp.MustCompile(`\p{Zs}`)

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
	// Normalize NBSP and other Unicode spaces to ASCII space so the prefix
	// checks and "\s"-based regexes below match the TUI's NBSP-rendered output.
	data = codexUnicodeSpace.ReplaceAll(data, []byte(" "))
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

// codexModalTailLines bounds the "does this pane BLOCK input right now?" answer
// to the bottom of the capture, matching the ~30-line window the claude arm gets
// for free from bossalib/statusdetect (see hasQuestionPrompt's modalOnly path).
//
// The notification question ("has this chat asked something?") is answered over
// the whole pane on purpose: a prompt is worth surfacing wherever it is. The
// modal question is not the same question. Callers capture with scrollback — up
// to 1000 lines — and hasCodexQuestionPrompt's active-pane slice only truncates
// at "• Session Complete", which a long-running chat may never print. Neither
// the "›" nor the "•" stripper removes the approval footer, so ONE approval the
// user answered an hour ago stays in the buffer and, read pane-wide, would
// declare the composer blocked for the rest of the session — refusing every
// subsequent delivery into a pane that is in fact idle and ready. A modal is by
// definition drawn where the composer would be, so the tail is where it lives.
const codexModalTailLines = 30

// codexRequestUserInputFooter is codexRequestUserInput's closing line on its
// own. A card taller than the modal window loses its "Question N/M" header to
// the slice, but this footer is drawn last, so its presence in the window is
// what proves the card still owns the bottom of the pane.
var codexRequestUserInputFooter = regexp.MustCompile(`(?m)` + codexQuestionCardFooterPattern)

// hasCodexModalPrompt reports whether the pane is showing a codex selection UI
// that has taken the composer *now*. Same grammar as hasCodexQuestionPrompt,
// bounded to the tail; see codexModalTailLines for why the two differ.
func hasCodexModalPrompt(data []byte) bool {
	data = codexUnicodeSpace.ReplaceAll(data, []byte(" "))
	trimmed := bytes.TrimRight(data, " \t\r\n")
	tail := codexModalTail(trimmed)
	if hasCodexQuestionPrompt(tail) {
		return true
	}
	// The working spinner is read HERE, over the window, not pane-wide: a
	// spinner from an earlier turn survives in the buffer until "• Session
	// Complete" — which a long-running chat may never print — so a pane-wide
	// read would switch the tall-card path's only defence off. In the window it
	// still does its job, because a spinner drawn below a picker's footer is
	// codex telling us the picker is done and the agent has resumed.
	if codexWorking.Match(tail) {
		return false
	}
	return codexTallCardOwnsComposer(trimmed, tail)
}

// codexTallCardOwnsComposer reports whether a request_user_input card still
// waiting on an answer reaches the bottom of the pane.
//
// It is the one place allowed to read above the modal window, because this card
// is the one modal that can be taller than the window: its grammar spans a
// "Question N/M (K unanswered)" header down to a footer, so a slice that keeps
// the footer but cuts the header would turn an on-screen card into a false
// negative — and this predicate failing open means the daemon types into it.
//
// What it reads up there is deliberately narrow: the footer must be in the
// window, and only the header of the card owning THAT footer is fetched from
// above it. Anchoring on the footer rather than on the bottom-most header
// matters in both directions. A header with no footer below it is a card whose
// render is still in flight — a capture can land between the two rows — and
// treating that as "no card" would fail open on the frame in between. And
// taking the header nearest the footer, rather than the last header anywhere,
// stops an older card's "(2 unanswered)" vouching for a footer whose own card
// reads "(0 unanswered)": a finished picker must stop blocking, or the tall-card
// path becomes a way for stale scrollback to refuse delivery forever, which is
// the failure the window bound exists to prevent.
func codexTallCardOwnsComposer(trimmed, tail []byte) bool {
	footers := codexRequestUserInputFooter.FindAllIndex(tail, -1)
	if len(footers) == 0 {
		return false
	}
	// tail is a suffix of trimmed (statusdetect.LastNLines slices in place), so
	// the window's offset is the length difference.
	footerStart := len(trimmed) - len(tail) + footers[len(footers)-1][0]
	above := codexQuestionActivePane(trimmed[:footerStart])
	headers := codexQuestionCardHeader.FindAllIndex(above, -1)
	if len(headers) == 0 {
		return false
	}
	last := headers[len(headers)-1]
	return codexQuestionCardLive.Match(above[last[0]:last[1]])
}

// codexModalTail is the slice of a capture a modal could be drawn in: the last
// codexModalTailLines *rendered* lines.
//
// The trim is load-bearing, not tidiness. `tmux capture-pane` returns every row
// of the pane, so a capture of a 50-row pane holding 20 rows of conversation
// carries 30 trailing blank rows — and codex draws its menu directly under the
// last output line, not at the pane bottom. Counting lines from the raw end
// spends the whole window on padding: on the committed question.txt fixture the
// approval footer sits 26 rows up in a 60-row pane, leaving four rows of margin,
// and any shorter conversation or taller pane pushes the live menu out of the
// window entirely. That is the fail-open direction — no modal seen means deliver
// — so the bound has to be measured from rendered content.
//
// The trim is ASCII-only, so callers must normalize Unicode spaces first or a
// pane padded with NBSP is not recognised as padding. hasCodexModalPrompt, the
// only caller, does exactly that on the line above.
func codexModalTail(data []byte) []byte {
	return statusdetect.LastNLines(bytes.TrimRight(data, " \t\r\n"), codexModalTailLines)
}

func codexQuestionActivePane(data []byte) []byte {
	locs := codexSessionComplete.FindAllIndex(data, -1)
	if len(locs) == 0 {
		return data
	}
	return data[locs[len(locs)-1][1]:]
}
