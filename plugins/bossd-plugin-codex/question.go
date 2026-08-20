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

// codexBootDecoration is the run of leading glyphs a *drawn* boot interstitial
// row may carry before its text: indentation, the TUI's box borders, and the
// header's decorative emoji. Letters and digits are excluded so the matchers
// below anchor on a row codex drew rather than on the same words quoted
// mid-sentence in agent prose.
//
// U+2022 "•" is excluded for the reason the question-card header states: "•"
// prefixes codex's activity bullets, which is replayed text, not live UI.
// U+203A "›" is excluded HERE but deliberately allowed on menu rows below —
// see codexBootInterstitialOptionRow for why the two differ.
//
// Excluding \p{N} is also a silent dependency on how the pane is captured:
// tmux.Client.CapturePane runs capture-pane WITHOUT -e, so no ANSI escapes
// reach these matchers. An -e capture would prefix the header row with
// "\x1b[0m", whose digits this class rejects, and BOTH alternations would stop
// matching a screen they match today — a false negative, the direction that
// costs a pane. Anyone adding an escape-preserving capture must re-test this
// grammar against it rather than assume it is escape-agnostic.
const codexBootDecoration = `[^\p{L}\p{N}\x{203A}\x{2022}]*`

// codexBootInterstitialHeader is the first of the two independent alternations
// that recognise codex's "Update available!" boot interstitial — the screen
// codex opens ON, instead of a composer, when it finds a newer release.
//
// It anchors on the words plus the version arrow ("0.147.0 -> 9.99.0"), never
// on the ✨ that precedes them. The emoji is separated from the word by U+200A
// HAIR SPACE, which codexUnicodeSpace normalises to an ASCII space before this
// runs, and a decorative glyph is exactly the part of a TUI most likely to be
// restyled between releases. Requiring the arrow, rather than the words alone,
// is what keeps a release note or a changelog line that merely says "update
// available" from blocking a send.
var codexBootInterstitialHeader = regexp.MustCompile(
	`(?m)^` + codexBootDecoration + `Update\s+available!\s+v?[0-9]+(?:\.[0-9]+)+\s*(?:->|=>|\x{2192})\s*v?[0-9]+(?:\.[0-9]+)+`)

// codexBootInterstitialFooter is the interstitial's closing instruction. On its
// own it proves nothing: "Press enter to continue" is ordinary English and
// appears in agent prose, installer output and pasted logs. It only counts
// alongside the menu shape below — see hasCodexBootInterstitial.
var codexBootInterstitialFooter = regexp.MustCompile(
	`(?m)^` + codexBootDecoration + `Press\s+enter\s+to\s+continue\b`)

// codexBootInterstitialOptionRow matches ONE numbered menu row ("2. Skip").
//
// Unlike the two matchers above this one ALLOWS a leading U+203A "›", because
// on this screen "›" is the menu's own selection cursor: codex draws row 1 as
// "› 1. Update now". That is the whole reason the interstitial is dangerous —
// the cursor makes row 1 indistinguishable from a composer row to
// composerRowIndex — so a matcher that refused it would miss the row the gate
// is being fooled by. The looseness is paid for structurally rather than
// lexically: a lone "› 1. Yes" replayed from user history cannot satisfy
// hasCodexBootInterstitial, which requires a RUN of these rows.
var codexBootInterstitialOptionRow = regexp.MustCompile(
	`^[^\p{L}\p{N}\x{2022}]*[0-9]+\.\s+\S`)

// codexBootInterstitialSelectedRow matches an option row carrying the menu's
// own selection cursor: "› 1. Update now".
//
// This is the one signal on the screen that ordinary agent output does not
// produce. A run of numbered rows is not enough by itself — an agent that
// writes "1. Install dependencies / 2. Run tests" and then, anywhere in the
// same 30 lines, the words "press enter to continue" satisfies both halves of
// the structural alternation while its composer is perfectly live, and the
// refusal that follows wedges an ordinary send until the text scrolls away.
// Proximity does not separate those two cases; a drawn cursor does, because
// codex renders the highlighted row and prose does not.
//
// It costs sensitivity, and the cost is stated rather than hidden: a codex
// release that both rewords the header AND draws its menu without a cursor
// stops matching. That is a compound restyle, alternation 1 covers the far
// likelier half of it, and the alternative was a structural pair loose enough
// to fire on prose the agent writes about this very screen.
var codexBootInterstitialSelectedRow = regexp.MustCompile(
	`^[^\p{L}\p{N}\x{2022}]*\x{203A}[^\p{L}\p{N}\x{2022}]*[0-9]+\.\s+\S`)

// codexBootInterstitialMinOptions is how many consecutive numbered rows make a
// menu. Two, not three: the count has to survive codex dropping an option
// (the "Skip until next version" row only exists once a version has been
// seen), and one row is not a shape — it is a sentence that starts with "1.".
const codexBootInterstitialMinOptions = 2

// codexBootInterstitialMenu reports whether the window contains a *run* of at
// least codexBootInterstitialMinOptions consecutive numbered option rows, at
// least one of which carries the selection cursor.
//
// Consecutive is load-bearing. Numbered lines are common in agent output —
// every ordered list the agent writes is a sequence of them — but a prose list
// is broken up by blank lines and continuation text, whereas a menu is a solid
// block. Any line that is not itself an option row ends the run, including a
// blank one, so scattered "1." and "2." lines separated by prose never
// accumulate into a menu.
//
// The cursor is load-bearing for the same reason in the other direction: a
// solid block of numbered rows is exactly what an agent writing a short
// ordered list produces, and consecutiveness alone does not tell the two
// apart. Both the run and the cursor reset together, so a cursor drawn on some
// unrelated earlier row cannot vouch for a later block.
func codexBootInterstitialMenu(tail []byte) bool {
	run, selected := 0, false
	for _, line := range bytes.Split(tail, []byte{'\n'}) {
		if codexBootInterstitialOptionRow.Match(line) {
			run++
			if codexBootInterstitialSelectedRow.Match(line) {
				selected = true
			}
			if run >= codexBootInterstitialMinOptions && selected {
				return true
			}
			continue
		}
		run, selected = 0, false
	}
	return false
}

// hasCodexBootInterstitial reports whether the modal window is showing codex's
// update-available boot screen.
//
// Two independent alternations, either of which is sufficient:
//
//  1. the header, with its version arrow; or
//  2. the menu shape — a run of numbered rows with the selection cursor drawn
//     on one of them — AND the "press enter to continue" footer together.
//
// Independent on purpose. The header is the most specific evidence but the most
// likely to be restyled; the structural pair survives a reworded header but
// needs both halves, because each alone is something ordinary output produces.
// A capture that loses the header to a redraw still refuses, and so does one
// whose footer wording changed.
//
// Why this screen needs its own clause at all: it is not a question and it is
// not an approval, so neither codexApproval nor codexRequestUserInput sees it —
// and hasCodexQuestionPrompt is structurally unable to, because its "›"
// stripper deletes "› 1. Update now", the only row that carries the menu's
// distinctive text. Meanwhile the readiness gate's composerRowIndex accepts
// that same "›" row as a live composer. So the pane looks ready, delivery types
// its message, and the Enter that follows lands on "Update now": codex runs
// `npm install -g @openai/codex` over its own running binary, the pane dies and
// the chat is destroyed. The fixture in testdata/panes/update_interstitial.txt
// is a real capture of it.
//
// Bounded by the modal window, in the direction that costs a chat: the clause
// only sees the last codexModalTailLines RENDERED lines, and the committed
// capture qualifies partly because the interstitial is the last thing drawn on
// that pane — its blank rows 11-50 are trailing whitespace, so the trim removes
// them. Draw one line at the bottom of that same 50-row pane and the banner is
// more than 30 rendered lines up: this returns false and delivery proceeds into
// the Enter. That is deliberate — BOS-600 narrowed the window precisely so a
// banner sitting in scrollback cannot wedge delivery forever — but it means the
// protection covers "the interstitial is what the pane is currently showing",
// not "the interstitial is somewhere on the pane".
// TestBootInterstitialWindowStopsAtDrawnContent asserts both sides of that
// boundary so it cannot move without a test moving with it.
//
// UNVERIFIED, recorded rather than assumed (the BOS-894 plan's "post-dismissal
// persistence" risk): what codex leaves on screen once the user answers this
// menu has never been captured. If a dismissed banner keeps its header inside
// the modal window, alternation 1 fires ALONE — it needs no footer — and every
// session start into that pane refuses until 30 rendered lines push the header
// out, which for a pane that never receives a delivery is never. The plan
// offered the footer requirement as the mitigation; it is not one, for exactly
// that reason. The failure is loud rather than the silent Enter this clause
// exists to prevent, which is the right direction to fail in, but treat it as
// open: capture a post-dismissal pane and add it as a negative fixture before
// widening either anchor.
func hasCodexBootInterstitial(tail []byte) bool {
	if codexBootInterstitialHeader.Match(tail) {
		return true
	}
	return codexBootInterstitialFooter.Match(tail) && codexBootInterstitialMenu(tail)
}

// hasCodexModalPrompt reports whether the pane is showing a codex selection UI
// that has taken the composer *now*: the hasCodexQuestionPrompt grammar bounded
// to the tail (see codexModalTailLines for why the two differ), PLUS the boot
// interstitial, which hasCodexQuestionPrompt deliberately does not match. That
// screen owns the composer without asking anything, so this predicate is not a
// superset or a subset of the notify one — do not infer either from the other.
//
// The boot interstitial is checked BEFORE the working-spinner early return
// below. That ordering is deliberate: the spinner return exists to stop a slow
// turn being read as a menu, but this screen is drawn by codex at process start,
// before any turn exists, so a spinner in the same window can only be scrollback
// from a previous process in a reused pane. Letting that stale spinner suppress
// the interstitial would hand back exactly the failure this clause exists to
// prevent.
func hasCodexModalPrompt(data []byte) bool {
	data = codexUnicodeSpace.ReplaceAll(data, []byte(" "))
	trimmed := bytes.TrimRight(data, " \t\r\n")
	tail := codexModalTail(trimmed)
	if hasCodexQuestionPrompt(tail) {
		return true
	}
	if hasCodexBootInterstitial(tail) {
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
