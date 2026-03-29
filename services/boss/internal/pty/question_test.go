package pty

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
			got := stripANSI([]byte(tt.input))
			if !bytes.Equal(got, []byte(tt.want)) {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
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
			data: `  Claude wants to run a command:

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
				"  - 26131cc chore(global): gitignore .beads/issues.jsonl\n" +
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
			name: "force-push question with ⏺ outside tail buffer",
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
			name: "non-question with ⏺ outside tail buffer",
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasQuestionPrompt([]byte(tt.data))
			if got != tt.want {
				// Show diagnostic info for debugging
				clean := stripANSI([]byte(tt.data))
				tail := lastNLines(clean, 30)
				t.Errorf("hasQuestionPrompt() = %v, want %v\n  clean (%d bytes): %q\n  tail30 (%d bytes): %q\n  selectorMatch: %v\n  optionMatches: %d",
					got, tt.want, len(clean), string(clean), len(tail), string(tail),
					selectorRe.Match(tail), len(optionRe.FindAll(tail, -1)))
			}
		})
	}
}

func TestLastNLines(t *testing.T) {
	data := []byte("line1\nline2\nline3\nline4\nline5\n")

	got := lastNLines(data, 2)
	want := []byte("line4\nline5\n")
	if !bytes.Equal(got, want) {
		t.Errorf("lastNLines(data, 2) = %q, want %q", got, want)
	}

	got = lastNLines(data, 100)
	if !bytes.Equal(got, data) {
		t.Errorf("lastNLines(data, 100) = %q, want %q", got, data)
	}
}

func TestHasQuestionPrompt_ExactlyTwoOptions(t *testing.T) {
	// Tests boundary: selector with exactly 2 indented options.
	// Catches mutation: len(matches) >= 2 changed to len(matches) > 2.
	// "Choose one:" must NOT have leading spaces, otherwise it matches optionRe
	// and gives 3 matches instead of the intended 2.
	data := `Choose one:

  ❯ First option
    Second option
`
	if !hasQuestionPrompt([]byte(data)) {
		t.Error("should detect question with exactly 2 options")
	}
}

func TestHasQuestionPrompt_OnlyOneOption(t *testing.T) {
	// Tests boundary: selector with only 1 indented line total.
	// Catches mutation: len(matches) >= 2 changed to len(matches) > 2.
	// We need exactly 1 match for optionRe (2+ leading spaces + non-space).
	// The selector line "  ❯ Only option" itself matches optionRe because it
	// starts with "  ❯" (2 spaces + non-space character ❯).
	// So we need NO other lines with 2+ leading spaces.
	data := `Choose one:

  ❯ Only option
`
	if hasQuestionPrompt([]byte(data)) {
		t.Error("should not detect question with only 1 indented line")
	}
}

func TestHasQuestionPrompt_ThreeOptions(t *testing.T) {
	// Tests boundary: selector with 3 indented options (well over 2).
	data := `  Choose one:

  ❯ First option
    Second option
    Third option
`
	if !hasQuestionPrompt([]byte(data)) {
		t.Error("should detect question with 3 options")
	}
}

func TestHasQuestionPrompt_ResponseMarkerAtIndexZero(t *testing.T) {
	// Tests boundary: ⏺ marker at index 0.
	// Catches mutation: idx >= 0 changed to idx > 0.
	data := "⏺ What would you like to do?"
	if !hasQuestionPrompt([]byte(data)) {
		t.Error("should detect question when ⏺ is at index 0")
	}
}

func TestHasQuestionPrompt_ResponseMarkerAtIndexOne(t *testing.T) {
	// Tests boundary: ⏺ marker at index 1 (after a newline).
	data := "\n⏺ What would you like to do?"
	if !hasQuestionPrompt([]byte(data)) {
		t.Error("should detect question when ⏺ is at index 1")
	}
}

func TestLastNLines_TrailingNewlineAtIndexZero(t *testing.T) {
	// Tests boundary: i >= 0 changed to i > 0 in lastNLines.
	// When data is just "\n" (length 1), i starts at 0.
	// The check `if i >= 0 && data[i] == '\n'` handles i=0 correctly by checking i >= 0.
	// If mutated to `i > 0`, this check would fail when i=0, breaking the trailing newline skip.
	data := []byte("\n")
	got := lastNLines(data, 5)
	// Returns original data since loop never runs after skipping trailing newline
	want := []byte("\n")
	if !bytes.Equal(got, want) {
		t.Errorf("lastNLines(\"\\n\", 5) = %q, want %q", got, want)
	}
}

func TestLastNLines_DataStartingAtIndexZero(t *testing.T) {
	// Tests boundary: for loop condition `i >= 0`.
	// When we need to scan all the way to index 0, the condition must allow i=0.
	// Mutation: i >= 0 changed to i > 0 would skip index 0.
	data := []byte("x\ny\n")
	got := lastNLines(data, 2)
	want := []byte("x\ny\n")
	if !bytes.Equal(got, want) {
		t.Errorf("lastNLines(%q, 2) = %q, want %q", data, got, want)
	}
}

func TestLastNLines_SingleCharacterBeforeNewline(t *testing.T) {
	// Tests boundary: i >= 0 in the for loop condition.
	// If data = "a\n", after skipping trailing newline, i = 0 (the 'a').
	// The loop should process i=0 and return "a\n".
	data := []byte("a\n")
	got := lastNLines(data, 1)
	want := []byte("a\n")
	if !bytes.Equal(got, want) {
		t.Errorf("lastNLines(\"a\\n\", 1) = %q, want %q", got, want)
	}
}
