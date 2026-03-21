package pty

import (
	"bytes"
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
