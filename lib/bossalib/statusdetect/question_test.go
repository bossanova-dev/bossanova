package statusdetect

import (
	"bytes"
	"strings"
	"testing"
)

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello world", "hello world"},
		{"CSI color", "\x1b[32mgreen\x1b[0m", "green"},
		{"CSI cursor move", "\x1b[2Jhello", "hello"},
		{"OSC title", "\x1b]0;title\x07text", "text"},
		{"OSC with ST", "\x1b]8;;url\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"two-byte ESC(B", "\x1b(Bhello", "hello"},
		{"mixed", "\x1b[1m\x1b[33mwarn\x1b[0m: msg", "warn: msg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripANSI([]byte(tt.input))
			if !bytes.Equal(got, []byte(tt.want)) {
				t.Errorf("StripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestHasQuestionPrompt(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "empty input",
			data: "",
			want: false,
		},
		{
			name: "plain text",
			data: "just some regular output\nnothing special here\n",
			want: false,
		},
		{
			name: "AskUserQuestion prompt",
			data: `  Which approach should we use?

  ❯ Option A (Recommended)
    Use the simple approach

    Option B
    Use the complex approach

`,
			want: true,
		},
		{
			name: "permission prompt",
			data: `  Claude wants to run a command. Allow?

  ❯ Allow
    Allow once
    Deny
`,
			want: true,
		},
		{
			name: "selector with ANSI escapes",
			data: "\x1b[1m  Which one?\x1b[0m\n\n  \x1b[36m❯ First option\x1b[0m\n    Second option\n    Third option\n",
			want: true,
		},
		{
			name: "lone selector without options",
			data: "❯ just a single line with arrow\n",
			want: false,
		},
		{
			name: "code output with ❯ character",
			data: "$ echo '❯ test'\n❯ test\nCompiled successfully.\n",
			want: false,
		},
		{
			name: "realistic AskUserQuestion with many options",
			data: `  ────────────────────────────────

  Which library should we use for date formatting?

  ❯ date-fns (Recommended)
    Lightweight and tree-shakeable

    moment
    Feature-rich but large bundle

    luxon
    Modern Moment successor

    dayjs
    Tiny and Moment-compatible

`,
			want: true,
		},
		{
			name: "real Claude Code AskUserQuestion output",
			data: "─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n ☐ Test prompt\n\nWhat does this question prompt look like in your terminal? (Pick any option so we can see the PTY output pattern for detection)\n\n❯ 1. Option A\n     First test option\n  2. Option B\n     Second test option\n  3. Option C\n     Third test option\n  4. Type something.\n─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n  5. Chat about this\n",
			want: true,
		},
		{
			name: "Claude conversational question in response",
			data: "❯ ask me a question\n\n⏺ What would you like me to help you with on the add-a-status-for-questions branch? I see there's a modified test file (question_test.go) — are you looking to continue work on that feature, or is there something else\n  you'd like to tackle?\n",
			want: true,
		},
		{
			name: "Claude response without question",
			data: "❯ fix the bug\n\n⏺ Done! I've fixed the bug in main.go by correcting the off-by-one error on line 42.\n",
			want: false,
		},
		{
			name: "real Claude Code AskUserQuestion favorite lang",
			data: "─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n ☐ Favorite lang\n\nWhich programming language is your favorite?\n\n❯ 1. Go\n     Fast, simple, great for backend services and CLI tools\n  2. TypeScript\n     Type-safe JavaScript for web and full-stack development\n  3. Python\n     Versatile and readable, great for scripting and data science\n  4. Rust\n     Memory-safe systems programming with zero-cost abstractions\n  5. Type something.\n─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n  6. Chat about this\n",
			want: true,
		},
		{
			name: "real Claude Code AskUserQuestion with ANSI (bubbletea rendering)",
			data: "\x1b[?25l\x1b[2K─────────────────────────────────────────────────────────────\r\n" +
				"\x1b[2K \x1b[1m☐ Test prompt\x1b[0m\r\n" +
				"\x1b[2K\r\n" +
				"\x1b[2KWhat does this question prompt look like in your terminal? (Pick any option so we can see the PTY output pattern for detection)\r\n" +
				"\x1b[2K\r\n" +
				"\x1b[2K\x1b[36m❯ 1. Option A\x1b[0m\r\n" +
				"\x1b[2K\x1b[2m     First test option\x1b[0m\r\n" +
				"\x1b[2K  2. Option B\r\n" +
				"\x1b[2K\x1b[2m     Second test option\x1b[0m\r\n" +
				"\x1b[2K  3. Option C\r\n" +
				"\x1b[2K\x1b[2m     Third test option\x1b[0m\r\n" +
				"\x1b[2K  4. Type something.\r\n" +
				"\x1b[2K─────────────────────────────────────────────────────────────\r\n" +
				"\x1b[2K  5. Chat about this\r\n",
			want: true,
		},
		{
			name: "long response with squash commits question",
			data: "⏺ Here are the recent commits on this branch:\n\n" +
				"  - c09500f chore: [skip ci] create pull request\n" +
				"  - 26131cc chore(global): tighten lint rules for ignored files\n" +
				"  - b889553 feat(plugin): [#18] wire previously-rejected PR detection into PollTasks\n" +
				"  - 2d73d9e docs(plugin): [#18] add implementation plan and flight leg handoffs\n" +
				"  - ff8498a test(plugin): [#18] add comprehensive test coverage for task source plugin\n" +
				"  - a1b2c3d feat(plugin): [#18] implement task source plugin with GitHub PR polling\n" +
				"  - d4e5f6a chore(deps): update go.mod dependencies to latest versions\n" +
				"  - 1234567 fix(pty): handle edge case in ANSI strip for cursor-position sequences\n" +
				"  - 89abcde refactor(boss): extract ring buffer into dedicated package with tests\n" +
				"  - fedcba9 feat(boss): add configurable poll interval for task source plugins\n" +
				"  - 0011223 docs(README): update architecture diagram with new plugin system\n" +
				"  - 4455667 test(integration): add end-to-end test for PR review workflow\n\n" +
				"  There are 12 commits total since the branch diverged from main. Several of these are small fixups that could\n" +
				"  be combined. Would you like me to squash some of these commits before creating the PR?\n",
			want: true,
		},
		{
			name: "long response with flight plan question",
			data: "⏺ I've analyzed the codebase and here's the implementation plan:\n\n" +
				"  ## Flight Plan\n\n" +
				"  **Leg 1: Core Data Model**\n" +
				"  - Add new `QuestionDetector` interface in `question.go`\n" +
				"  - Implement `RegexDetector` with configurable patterns\n" +
				"  - Add unit tests for all pattern types\n\n" +
				"  **Leg 2: Integration Layer**\n" +
				"  - Wire detector into the PTY monitor loop\n" +
				"  - Add timeout handling for stale question detection\n" +
				"  - Integration test with mock PTY output\n\n" +
				"  **Leg 3: Configuration & Polish**\n" +
				"  - Add YAML config for custom question patterns\n" +
				"  - Documentation updates for the new detection system\n" +
				"  - Performance benchmarks comparing old vs new approach\n\n" +
				"  This plan has 3 legs with handoff checkpoints between each. Does this look like the right approach for the refactor?\n",
			want: true,
		},
		{
			name: "force-push permission question",
			data: "⏺ 31 files, 4,538+/321-. The diff is intact. Do I have permission to force-push?\n",
			want: true,
		},
		{
			name: "force-push question with response marker outside tail buffer",
			data: "4,538+/321-. The diff is intact. Do I have permission to force-push?\n" +
				strings.Repeat("─", 80) + "\n" +
				strings.Repeat("─", 80) + "\n" +
				"  Opus 4.6 | Context: 89% remaining | /Users/dave/Documents/Code/boss\n" +
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n" +
				"\n" +
				"❯\n",
			want: true,
		},
		{
			name: "non-question with response marker outside tail buffer",
			data: "I've fixed the bug in main.go by correcting the off-by-one error on line 42.\n" +
				strings.Repeat("─", 80) + "\n" +
				"  Opus 4.6 | Context: 89% remaining\n",
			want: false,
		},
		{
			name: "office-hours Demand question (user reported miss)",
			data: "─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n" +
				" ☐ Demand\n" +
				"\n" +
				"What's the strongest evidence you have that someone actually wants boss — not 'is interested,' not 'signed up for a waitlist,' but would be genuinely upset if it disappeared tomorrow?\n" +
				"\n" +
				"❯ 1. I'm the user\n" +
				"     I use it daily and would be upset without it\n" +
				"  2. Others want it\n" +
				"     Specific people have told me they need this\n" +
				"  3. Market signal\n" +
				"     I see the pain in how people work but haven't validated directly\n" +
				"  4. Honest: none yet\n" +
				"     I'm building on conviction, not evidence\n" +
				"  5. Type something.\n" +
				"─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n" +
				"  6. Chat about this\n",
			want: true,
		},
		{
			// Pattern 4 (additive): AskUserQuestion card rendered without a
			// left-column chevron cursor. Same structural signals as the
			// existing office-hours fixture (☐ header + question + numbered
			// options) but no chevron on any option line. Note: the existing
			// Pattern 3 (trailing-? fallback) actually catches this shape on
			// its own, but Pattern 1 cannot — guards against regressions if
			// Pattern 1 were ever generalized to drop its chevron requirement.
			name: "chevronless card without ⏺ marker",
			data: "─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n" +
				" ☐ Highlight style\n" +
				"\n" +
				"2B — How should the selected row be highlighted (the 'in our primary color' part)?\n" +
				"\n" +
				"ELI10: The TUI's selected row uses bold + blue foreground on the text.\n" +
				"\n" +
				"Recommendation: A (background tint + accent left border + bold text).\n" +
				"\n" +
				" 1. A) Background tint + left\n" +
				"   accent border + bold text\n" +
				"   (recommended)\n" +
				" 2. B) Strict TUI parity —\n" +
				"   bold + blue text only\n" +
				" 3. C) Background tint only,\n" +
				"   no left border\n" +
				"\n" +
				"                                  Notes: press n to add notes\n",
			want: true,
		},
		{
			// Pattern 4 (the user-reported miss): chevronless card with a
			// later ⏺ response marker BELOW the question. This is the shape
			// that defeats the existing detector:
			//   - Pattern 1 can't fire (no chevron on an option line).
			//   - Pattern 2 finds the LAST ⏺ below the card; the text after
			//     that marker has no trailing "?", so Pattern 2 hits its
			//     early-return-false branch and short-circuits Pattern 3.
			//   - Pattern 3 (trailing-? fallback) is never reached.
			// The card's ☐ header + question with "?" + 2+ numbered options
			// is the structural signal Pattern 4 keys on.
			name: "chevronless card with ⏺ marker below (user reported miss)",
			data: "⏺ Here's an outline of the design choices.\n" +
				"\n" +
				"─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n" +
				" ☐ Highlight style\n" +
				"\n" +
				"2B — How should the selected row be highlighted (the 'in our primary color' part)?\n" +
				"\n" +
				"ELI10: The TUI's selected row uses bold + blue foreground on the text.\n" +
				"\n" +
				"Recommendation: A (background tint + accent left border + bold text).\n" +
				"\n" +
				" 1. A) Background tint + left\n" +
				"   accent border + bold text\n" +
				"   (recommended)\n" +
				" 2. B) Strict TUI parity —\n" +
				"   bold + blue text only\n" +
				" 3. C) Background tint only,\n" +
				"   no left border\n" +
				"\n" +
				"                                  Notes: press n to add notes\n" +
				"\n" +
				"⏺ Working… (3s · ↑ 0 tokens)\n" +
				"  Opus 4.7 | Context: 89% remaining\n",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasQuestionPrompt([]byte(tt.data))
			if got != tt.want {
				// Show diagnostic info for debugging
				clean := StripANSI([]byte(tt.data))
				tail := LastNLines(clean, 30)
				t.Errorf("HasQuestionPrompt() = %v, want %v\n  clean (%d bytes): %q\n  tail30 (%d bytes): %q\n  selectorMatch: %v\n  optionMatches: %d",
					got, tt.want, len(clean), string(clean), len(tail), string(tail),
					selectorRe.Match(tail), len(optionRe.FindAll(tail, -1)))
			}
		})
	}
}

func TestHasQuestionPrompt_EmptyInputPrompt(t *testing.T) {
	// Regression: "❯ " on a line by itself is Claude Code's empty input prompt
	// waiting for user keystrokes, NOT an AskUserQuestion selector. Surrounding
	// indented status-bar lines must not be mistaken for selector options.
	data := "⏺ Here's a long response without any question at the end.\n" +
		"\n" +
		strings.Repeat("─", 80) + " crop-box-zoom-fix ──\n" +
		"❯ \n" +
		strings.Repeat("─", 80) + "\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · PR #110\n" +
		"  Opus 4.7 | Context: 89% remaining\n"
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should NOT detect question when ❯ is an empty input prompt with status-bar chrome")
	}
}

func TestHasQuestionPrompt_InterruptedToolCall(t *testing.T) {
	// Regression: user interrupts a tool call mid-execution. Claude Code renders
	// "⎿  Interrupted · What should Claude do instead?" as the tool result.
	// The most recent ⏺ response text has no "?", so this should NOT flag as a
	// question -- the "?" is Claude Code UI text, not Claude's words.
	tests := []struct {
		name string
		data string
	}{
		{
			name: "interrupted Read tool",
			data: "⏺ Now I'll review the new changes (since the last review) with fresh eyes.\n" +
				"\n" +
				"  Read 1 file (ctrl+o to expand)\n" +
				"  ⎿  Interrupted · What should Claude do instead?\n",
		},
		{
			name: "interrupted Bash tool",
			data: "⏺ Let me check the status.\n" +
				"\n" +
				"⏺ Bash(make test)\n" +
				"  ⎿  Interrupted · What should Claude do instead?\n",
		},
		{
			name: "interrupt in tail with response marker outside tail",
			// Pattern 3 fallback: ⏺ is pushed out of the 30-line window; only
			// content left in the tail is the interrupt artifact. Must NOT fire.
			data: func() string {
				var b strings.Builder
				b.WriteString("⏺ Here's a long response without any question at the end.\n")
				for range 40 {
					b.WriteString("  filler line to push the marker out of the tail\n")
				}
				b.WriteString("  ⎿  Interrupted · What should Claude do instead?\n")
				return b.String()
			}(),
		},
		{
			name: "interrupt followed by status bar (exact user-reported shape)",
			data: "⏺ Bash(git diff 01b58092..HEAD 2>&1 | head -400)\n" +
				"  ⎿  diff --git a/apps/frontend/src/engine/InputHandler.ts b/apps/frontend/src/engine/InputHandler.ts\n" +
				"     index d8ce2513..d7eb64a1 100644\n" +
				"     --- a/apps/frontend/src/engine/InputHandler.ts\n" +
				"     … +113 lines (ctrl+o to expand)\n" +
				"\n" +
				"⏺ Now I'll review the new changes (since the last review) with fresh eyes.\n" +
				"\n" +
				"  Read 1 file (ctrl+o to expand)\n" +
				"  ⎿  Interrupted · What should Claude do instead?\n" +
				"\n" +
				strings.Repeat("─", 80) + " crop-box-zoom-fix ──\n" +
				"❯ \n" +
				strings.Repeat("─", 80) + "\n" +
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · PR #110\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if HasQuestionPrompt([]byte(tt.data)) {
				clean := StripANSI([]byte(tt.data))
				tail := LastNLines(clean, 30)
				t.Errorf("should NOT detect question for interrupted tool call\n  clean: %q\n  tail: %q",
					string(clean), string(tail))
			}
		})
	}
}

func TestHasQuestionPrompt_QuestionMarkInsideToolOutput(t *testing.T) {
	// Tool output continuation lines that happen to end with "?" must not
	// trigger question detection -- tool output is system/external text.
	data := "⏺ Bash(grep -r 'TODO' src/)\n" +
		"  ⎿  src/foo.ts: // TODO: handle edge case?\n" +
		"     src/bar.ts: // TODO: verify this is correct?\n" +
		"     src/baz.ts: // done\n"
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should NOT detect question when only '?' is inside tool output continuation lines")
	}
}

func TestHasQuestionPrompt_QuestionAfterInterruptedTool(t *testing.T) {
	// Claude recovers from an interrupt and asks a real follow-up question.
	// The interrupt artifact should be stripped; the real question should fire.
	data := "⏺ I was about to read a file, but you interrupted.\n" +
		"\n" +
		"  Read 1 file (ctrl+o to expand)\n" +
		"  ⎿  Interrupted · What should Claude do instead?\n" +
		"\n" +
		"⏺ Should I skip the file read and proceed directly to the diff review?\n"
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question when Claude asks a follow-up after an interrupt")
	}
}

func TestHasQuestionPrompt_ClaudeCodeTips(t *testing.T) {
	// Claude Code renders contextual "Tip:" lines beneath its working/thinking
	// spinner. These tips often end with "?" ("Did you know you can …?") but
	// they are UI chrome, not questions from Claude. They must not trigger
	// question detection.
	negative := []struct {
		name string
		data string
	}{
		{
			name: "tip below working spinner (user-reported shape)",
			data: "✽ Newspapering… (11s · ↑ 682 tokens · thinking with xhigh effort)\n" +
				"  ⎿  Tip: Did you know you can drag and drop image files into your terminal?\n",
		},
		{
			name: "tip with ⎿ after a non-question response (Pattern 2 path)",
			data: "⏺ I've finished the refactor.\n" +
				"\n" +
				"  ⎿  Tip: Did you know you can drag and drop image files into your terminal?\n",
		},
		{
			name: "tip without ⎿ connector (bare indented form)",
			data: "✽ Working… (3s)\n" +
				"  Tip: Run /help for a list of commands?\n",
		},
		{
			name: "tip in tail with response marker pushed out of tail (Pattern 3 path)",
			data: func() string {
				var b strings.Builder
				b.WriteString("⏺ Here's a long response without any question at the end.\n")
				for range 40 {
					b.WriteString("  filler line to push the marker out of the tail\n")
				}
				b.WriteString("✽ Newspapering… (11s · ↑ 682 tokens)\n")
				b.WriteString("  ⎿  Tip: Did you know you can drag and drop image files into your terminal?\n")
				return b.String()
			}(),
		},
	}
	for _, tt := range negative {
		t.Run(tt.name, func(t *testing.T) {
			if HasQuestionPrompt([]byte(tt.data)) {
				clean := StripANSI([]byte(tt.data))
				tail := LastNLines(clean, 30)
				t.Errorf("should NOT detect question for Claude Code tip line\n  clean: %q\n  tail: %q",
					string(clean), string(tail))
			}
		})
	}

	// Positive guard: a real Claude question that contains the word "Tip:"
	// mid-sentence (not at line start) must still be detected. The tip filter
	// is anchored to line start, so this line is untouched.
	positive := "⏺ Tip: consider caching the result. Does that make sense to you?\n"
	if !HasQuestionPrompt([]byte(positive)) {
		t.Error("should detect real question even when response contains 'Tip:' mid-sentence")
	}
}

func TestHasQuestionPrompt_FalsePositiveScrollback(t *testing.T) {
	// Regression test: when scrollback is captured, an older response with "?"
	// is visible but the latest ⏺ response has no question. Pattern 2 should
	// find the marker and return false, preventing Pattern 3 from firing.
	data := "⏺ What would you like me to help you with?\n" +
		"\n" +
		"Some working output...\n" +
		"More output lines here\n" +
		"\n" +
		"⏺ Done! I've fixed the bug in main.go by correcting the off-by-one error on line 42.\n"
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should not detect question: latest response after ⏺ has no trailing '?'")
	}
}

func TestHasQuestionPrompt_ChevronlessCardNegatives(t *testing.T) {
	// Pattern 4 keys on three signals together: a "☐ <title>" header, a "?"
	// in the question region, and 2+ consecutive numbered options at the same
	// indent. These negative cases each break one of those signals and must
	// not flip status to "question".
	tests := []struct {
		name string
		data string
	}{
		{
			// Claude Code TODO lists also use ☐ but are a stack of task lines
			// with no question text and no numbered option block.
			name: "TODO list rendering with ☐ glyphs",
			data: "⏺ Update Todos\n" +
				"  ⎿  ☐ Investigate the bug in the parser\n" +
				"     ☐ Write a failing test\n" +
				"     ☐ Fix the off-by-one error\n" +
				"     ☒ Read the spec\n",
		},
		{
			// Numbered prose without a ☐ header must not match. Pattern 4
			// requires the card title; this rules out routine numbered lists
			// in tool output, changelogs, or response text.
			name: "numbered list without ☐ header",
			data: "⏺ Here's what changed in this release.\n" +
				"\n" +
				" 1. Faster startup\n" +
				" 2. New theme\n" +
				" 3. Bug fixes\n",
		},
		{
			// User answered the card; Claude has rendered a new ⏺ response
			// below with no remaining option block. Pattern 4 must not fire
			// just because the ☐ header is still in scrollback.
			name: "answered card with response below",
			data: "─────────────────────────────────────────────────────────────────\n" +
				" ☐ Highlight style\n" +
				"\n" +
				"How should the row be highlighted?\n" +
				"\n" +
				"⏺ Got it — going with option A.\n" +
				"  ⎿  Updated tr.css with the accent border.\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if HasQuestionPrompt([]byte(tt.data)) {
				clean := StripANSI([]byte(tt.data))
				tail := LastNLines(clean, 30)
				t.Errorf("should NOT detect question\n  clean: %q\n  tail: %q",
					string(clean), string(tail))
			}
		})
	}
}

func TestLastNLines(t *testing.T) {
	data := []byte("line1\nline2\nline3\nline4\nline5\n")

	got := LastNLines(data, 2)
	want := []byte("line4\nline5\n")
	if !bytes.Equal(got, want) {
		t.Errorf("LastNLines(data, 2) = %q, want %q", got, want)
	}

	got = LastNLines(data, 100)
	if !bytes.Equal(got, data) {
		t.Errorf("LastNLines(data, 100) = %q, want %q", got, data)
	}
}

func TestHasQuestionPrompt_ExactlyTwoOptions(t *testing.T) {
	data := `Choose one?

  ❯ First option
    Second option
`
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question with exactly 2 options")
	}
}

func TestHasQuestionPrompt_OnlyOneOption(t *testing.T) {
	data := `Choose one:

  ❯ Only option
`
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should not detect question with only 1 indented line")
	}
}

func TestHasQuestionPrompt_ThreeOptions(t *testing.T) {
	data := `  Choose one?

  ❯ First option
    Second option
    Third option
`
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question with 3 options")
	}
}

func TestHasQuestionPrompt_ResponseMarkerAtIndexZero(t *testing.T) {
	data := "⏺ What would you like to do?"
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question when response marker is at index 0")
	}
}

func TestHasQuestionPrompt_ResponseMarkerAtIndexOne(t *testing.T) {
	data := "\n⏺ What would you like to do?"
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question when response marker is at index 1")
	}
}

func TestLastNLines_TrailingNewlineAtIndexZero(t *testing.T) {
	data := []byte("\n")
	got := LastNLines(data, 5)
	want := []byte("\n")
	if !bytes.Equal(got, want) {
		t.Errorf("LastNLines(\"\\n\", 5) = %q, want %q", got, want)
	}
}

func TestLastNLines_DataStartingAtIndexZero(t *testing.T) {
	data := []byte("x\ny\n")
	got := LastNLines(data, 2)
	want := []byte("x\ny\n")
	if !bytes.Equal(got, want) {
		t.Errorf("LastNLines(%q, 2) = %q, want %q", data, got, want)
	}
}

func TestLastNLines_SingleCharacterBeforeNewline(t *testing.T) {
	data := []byte("a\n")
	got := LastNLines(data, 1)
	want := []byte("a\n")
	if !bytes.Equal(got, want) {
		t.Errorf("LastNLines(\"a\\n\", 1) = %q, want %q", got, want)
	}
}

// TestHasQuestionPrompt_FalsePositiveUserPromptWithSummaries reproduces the
// user-reported sticky-question shape: the user's previous prompt ("❯ yes fix
// both ...") is in the captured pane along with summary lines like "Read 4
// files..." and "Searched for 2 patterns..." and several Bash tool blocks.
// No "?" appears anywhere in Claude's words. Must NOT detect a question.
func TestHasQuestionPrompt_FalsePositiveUserPromptWithSummaries(t *testing.T) {
	data := "✻ Cogitated for 12m 24s\n" +
		"\n" +
		"❯ yes fix both of those issues and dig into the socket issue too\n" +
		"\n" +
		"  Read 4 files, listed 1 directory (ctrl+o to expand)\n" +
		"\n" +
		"⏺ Let me look at how the driver sends keys and how the boss process connects to the daemon.\n" +
		"\n" +
		"⏺ Bash(wc -l services/boss/internal/tuidriver/*.go)\n" +
		"  ⎿  Error: Exit code 1\n" +
		"     (eval):1: no matches found: lib/bossalib/tuidriver/*.go\n" +
		"\n" +
		"  Searched for 2 patterns, read 2 files (ctrl+o to expand)\n" +
		"\n" +
		"⏺ Bash(go test -count=10 -run=TestTUI_AttachView_BackKey -timeout 120s ./internal/tuitest)\n" +
		"  ⎿  ok         github.com/recurser/boss/internal/tuitest       6.373s\n" +
		"\n" +
		"⏺ Bash(go test -race -count=1 -timeout 300s ./internal/tuitest/)\n" +
		"  ⎿  ok         github.com/recurser/boss/internal/tuitest       35.5\n" +
		"     (1m 10s · timeout 10m)\n" +
		"\n" +
		"· Beboppin'… (3m 21s · ↓ 7.1k tokens · thought for 1s)\n" +
		"  ⎿  Tip: Use /btw to ask a quick side question without interrupting Claude's current work\n"
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should NOT detect question when user's prompt is in scrollback and no '?' appears in Claude's words")
	}
}

// TestHasQuestionPrompt_NoQuestionMarkAnywhere covers the general principle:
// if there's no "?" anywhere in the cleaned tail, this can't be a question --
// regardless of how many indented lines or selectors appear.
func TestHasQuestionPrompt_NoQuestionMarkAnywhere(t *testing.T) {
	data := "❯ run the build\n" +
		"\n" +
		"  Read 5 files\n" +
		"  Edited 2 files\n" +
		"  Ran 3 commands\n" +
		"\n" +
		"⏺ Done.\n"
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should NOT detect question when no '?' appears anywhere in the cleaned tail")
	}
}

// TestHasQuestionPrompt_UserPromptWithQuestionMark covers the case where the
// user's submitted prompt contains a "?" (e.g. "what does this do?"). The
// user's own "?" must not contribute to detection -- only Claude's words.
func TestHasQuestionPrompt_UserPromptWithQuestionMark(t *testing.T) {
	data := "❯ what does this do?\n" +
		"\n" +
		"⏺ It runs the build pipeline and uploads the artifacts.\n"
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should NOT detect question when user's prompt has '?' but Claude's response does not")
	}
}

// TestHasQuestionPrompt_OptionsBrokenByClaudeMarker guards the consecutive-
// options counter: if the lines after the selector are interrupted by a
// Claude marker (⏺/⎿/·/✻), the option count must stop there.
func TestHasQuestionPrompt_OptionsBrokenByClaudeMarker(t *testing.T) {
	data := "What did we do?\n" +
		"\n" +
		"❯ yes do that\n" +
		"  one summary line\n" +
		"⏺ Working on it now.\n" +
		"  another summary line\n"
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should NOT detect question when ⏺ marker breaks the option run after the selector")
	}
}

// TestHasQuestionPrompt_ExactlyOneOption covers the boundary mutation on
// `count >= 1` (line 211): with `count > 1`, a single option would not
// trigger detection. Catches CONDITIONALS_BOUNDARY at line 211.
func TestHasQuestionPrompt_ExactlyOneOption(t *testing.T) {
	data := `Want this?

  ❯ Yes please
    Just one option here
`
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question when selector is followed by exactly one indented option line")
	}
}

// TestHasQuestionPrompt_MultipleSelectorsLastIsReal verifies that when several
// "❯ " glyphs appear (e.g. user prompt history above a real AskUserQuestion),
// the iteration finds the LAST one with valid options.
// Catches mutations on selectorRe.FindAllIndex(tail, -1):
//   - INVERT_NEGATIVES: -1 → 1 returns only the first match (a user prompt
//     with no options below), failing detection.
//   - ARITHMETIC: -1 → +1 same story.
func TestHasQuestionPrompt_MultipleSelectorsLastIsReal(t *testing.T) {
	data := "❯ what's the plan?\n" +
		"\n" +
		"  Read 5 files (ctrl+o to expand)\n" +
		"\n" +
		"What should we do?\n" +
		"\n" +
		"  ❯ Refactor the API\n" +
		"    Move all handlers to /v2\n" +
		"    Use the new error helper\n"
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question on last selector (with options) even when an earlier user-prompt selector exists")
	}
}

// TestCountConsecutiveOptionLines_NewlineAtIndexZero covers nl < 0 boundary
// at line 151. When data starts with '\n', nl=0; the mutation `nl < 0`
// → `nl <= 0` would treat the entire data as a single line.
func TestCountConsecutiveOptionLines_NewlineAtIndexZero(t *testing.T) {
	// Data starts with '\n' (empty first line) followed by two valid option lines.
	data := []byte("\n  option one\n  option two\n")
	count, broken := countConsecutiveOptionLines(data)
	if broken {
		t.Errorf("should not be broken by marker, got broken=true")
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

// TestCountConsecutiveOptionLines_NoTrailingNewline covers the negation-style
// behavior on `nl < 0` at line 151: the LAST line of input may have no
// trailing newline, and must still be processed. With mutation `nl >= 0`,
// the function would never enter the no-newline branch and could mishandle
// the final line.
func TestCountConsecutiveOptionLines_NoTrailingNewline(t *testing.T) {
	// Three indented options, last has no trailing newline.
	data := []byte("  one\n  two\n  three")
	count, broken := countConsecutiveOptionLines(data)
	if broken {
		t.Errorf("should not be broken by marker, got broken=true")
	}
	if count != 3 {
		t.Errorf("count = %d, want 3 (last line lacks trailing \\n)", count)
	}
}

// TestHasQuestionPrompt_SelectorLineEndAtZero covers line 207 boundary:
// `lineEnd < 0` vs the mutation `lineEnd <= 0`. The selectorRe match ends
// right before a '\n' (lineEnd == 0). Unmutated must continue into option
// counting; mutated would skip (lineEnd <= 0 is true) and miss the prompt.
//
// The "?" is mid-sentence so trailingQuestionRe (Pattern 3) does not also
// fire — isolating Pattern 1 as the only path that can return true.
func TestHasQuestionPrompt_SelectorLineEndAtZero(t *testing.T) {
	data := "What's that? OK then:\n" +
		"  ❯ a\n" +
		"  option1\n" +
		"  option2\n"
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question via Pattern 1 when selector char is immediately followed by '\\n' (lineEnd == 0)")
	}
}

// TestLastNLines_NewlineAtIndexZero_n1 catches the boundary mutation on
// line 249 (`i >= 0 && data[i] == '\n'` → `i > 0 && ...`). With n=1 and
// data="\n", the mutation skips the trailing-newline strip and counts the
// '\n' as the first line, returning "" instead of "\n".
func TestLastNLines_NewlineAtIndexZero_n1(t *testing.T) {
	got := LastNLines([]byte("\n"), 1)
	want := []byte("\n")
	if !bytes.Equal(got, want) {
		t.Errorf("LastNLines(\"\\n\", 1) = %q, want %q", got, want)
	}
}

// TestLastNLines_LeadingNewline_n1 catches the boundary mutation on line 252
// (`for ; i >= 0; i--` → `for ; i > 0; i--`). With data="\nx" and n=1,
// the unmutated code finds the '\n' at i=0 (count hits n) and returns "x".
// The mutation stops the loop before i=0, so count never reaches n and the
// whole input is returned.
func TestLastNLines_LeadingNewline_n1(t *testing.T) {
	got := LastNLines([]byte("\nx"), 1)
	want := []byte("x")
	if !bytes.Equal(got, want) {
		t.Errorf("LastNLines(\"\\nx\", 1) = %q, want %q", got, want)
	}
}
