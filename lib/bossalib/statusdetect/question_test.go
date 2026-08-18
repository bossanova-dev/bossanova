package statusdetect

import (
	"bytes"
	"fmt"
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
		// Claude Code renders the input-box prompt and layout padding with
		// non-breaking spaces; normalize them so the ASCII-space detection
		// regexes (e.g. "❯ ") match.
		{"NBSP normalized to space", "❯ Did the e2e check pass?", "❯ Did the e2e check pass?"},
		{"narrow NBSP normalized", "a b", "a b"},
		{"figure space normalized", "a b", "a b"},
		{"tab preserved", "a\tb", "a\tb"},
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
			// Claude finished its turn and pre-filled the input box with a
			// suggested follow-up prompt ("❯ ...?"). Claude is suggesting a
			// prompt, not asking one, so this must NOT be a question. The last
			// ⏺ marker is the stop-hook line below the answer.
			name: "suggested follow-up prompt is not a question",
			data: "⏺ The PR is ready for human review. The only follow-up is the Low ticket in the review body.\n\n" +
				"⏺ Ran 1 stop hook (ctrl+o to expand)\n" +
				"  ⎿  Stop hook error: Failed with non-blocking status code: No stderr output\n\n" +
				"✻ Crunched for 21m 19s\n\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"❯ Want me to file the WON-1141 follow-up ticket now?\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"  Opus 4.8 (1M context) | Context: 88% remaining\n",
			want: false,
		},
		{
			// Same suggested-prompt false positive, but the last ⏺ marker is
			// the assistant's final message itself (no stop-hook marker below
			// it). The fix must hold regardless of which marker is last.
			name: "suggested follow-up prompt below final response marker",
			data: "⏺ All done. Tests pass and the branch is pushed.\n\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"❯ Want me to open a PR now?\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"  Opus 4.8 (1M context) | Context: 88% remaining\n",
			want: false,
		},
		{
			// Exact wondercanvas PR #385 repro. Claude Code renders the input
			// box as "❯<U+00A0>...", a NON-BREAKING space (U+00A0) after the
			// glyph -- not the ASCII space the "suggested follow-up" case above
			// uses. Without normalizing NBSP, stripUserPromptLines (regex "❯ ")
			// fails to remove this draft line and Pattern 3 matches its
			// trailing "?", firing a false "? question". The " " below is
			// the real byte sequence captured from the live pane.
			name: "input-box draft with NBSP separator is not a question",
			data: "⏺ The plane is landed. The boss-finalize workflow is complete.\n\n" +
				"⏺ Ran 1 stop hook (ctrl+o to expand)\n" +
				"  ⎿  Stop hook error: Failed with non-blocking status code: No stderr output\n\n" +
				"✻ Crunched for 30s\n\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"❯ Did the e2e deterministic check pass?\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"  Opus 4.8 (1M context) | Context: 91% remaining\n",
			want: false,
		},
		{
			// Same NBSP separator on a suggested follow-up prompt below the
			// final response marker (no stop-hook line). Mirror of the ASCII
			// "suggested follow-up prompt below final response marker" case.
			name: "suggested follow-up prompt with NBSP separator is not a question",
			data: "⏺ All done. Tests pass and the branch is pushed.\n\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"❯ Want me to open a PR now?\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"  Opus 4.8 (1M context) | Context: 88% remaining\n",
			want: false,
		},
		{
			// Positive guard: a real AskUserQuestion whose selector cursor and
			// the trailing question both use NBSP separators must STILL fire,
			// proving the normalization does not break Pattern 1/2 detection.
			name: "AskUserQuestion card with NBSP separators still detected",
			data: "  Which library should we use for date formatting?\n\n" +
				"❯ 1. date-fns (Recommended)\n" +
				"  2. moment\n" +
				"  3. luxon\n",
			want: true,
		},
		{
			name: "numbered suggested next prompts section is not a question",
			data: "⏺ Done. The change is implemented.\n\n" +
				"Suggested next prompts:\n" +
				"1. Want me to explain this?\n" +
				"2. Should I run tests?\n",
			want: false,
		},
		{
			name: "bulleted suggested follow-up prompts section is not a question",
			data: "⏺ Done. The branch is ready.\n\n" +
				"Suggested follow-up prompts:\n" +
				"- Want me to open the pull request?\n" +
				"- Should I add release notes?\n",
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
		{
			// Pattern 0 (long-body AskUserQuestion): ☐ header and the literal
			// "?" sit ABOVE the 30-line tail because the body (ELI10, Stakes,
			// Recommendation, Pros/cons A/B/C) is verbose. Detection has to
			// fall back on the structural footer signal -- the "Type something."
			// and "Chat about this" numbered options that terminate every
			// AskUserQuestion UI.
			name: "long-body AskUserQuestion: image bar visibility",
			data: "\n" +
				"  ---\n" +
				"  1. Architecture Review\n" +
				"\n" +
				strings.Repeat("─", 200) + "\n" +
				" ☐ Visibility\n" +
				"\n" +
				"Project: WonderCanvas. WON-392 leaves one design decision open: when does the percentage appear on the purple image bar?\n" +
				"\n" +
				"ELI10: Imagine a sticker on the corner of a photo telling you how big it is compared to the original.\n" +
				"\n" +
				"Stakes if we pick wrong: too noisy clutters every selected image; too hidden means production users won't see it during a resize.\n" +
				"\n" +
				"Recommendation: A (always-on while chrome is visible). Matches the existing chrome contract.\n" +
				"\n" +
				"Completeness: A=9/10, B=6/10, C=7/10.\n" +
				"\n" +
				"Pros / cons:\n" +
				"\n" +
				"A) Always visible whenever chrome is visible (recommended)\n" +
				"  ✅ Zero new visibility logic — reuses existing showChrome gate\n" +
				"  ✅ Always informative during a resize drag\n" +
				"  ❌ Shows '100%' on every freshly-spawned image\n" +
				"\n" +
				"B) Show only on hover over the bar (not on selection)\n" +
				"  ✅ Quietest — no extra text on a normally-selected image\n" +
				"  ❌ Defeats the actual use case during resize\n" +
				"  ❌ Adds a new visibility rule that doesn't exist for the name strip\n" +
				"\n" +
				"C) Show only when scale ≠ 100%\n" +
				"  ✅ Zero noise when image is at native size\n" +
				"  ❌ '100%' is a useful checkpoint — hiding it makes verification harder\n" +
				"  ❌ Threshold logic is a small new correctness surface\n" +
				"\n" +
				"Net: A is the boring-by-default choice and serves the resize-drag use case Imagica actually flagged.\n" +
				"\n" +
				"❯ 1. Always visible when chrome is\n" +
				"     Show percentage whenever the purple image bar is showing. Reuses existing showChrome gate. Recommended.\n" +
				"  2. Only on hover over the bar\n" +
				"     Hide by default, show only when the user hovers the bar itself.\n" +
				"  3. Only when scale ≠ 100%\n" +
				"     Hide at native size, show only when the image has actually been resized.\n" +
				"  4. Type something.\n" +
				strings.Repeat("─", 200) + "\n" +
				"  5. Chat about this\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · Esc to cancel\n",
			want: true,
		},
		{
			// Pattern 0: long-body AskUserQuestion with NO literal "?" anywhere
			// in the visible content -- the question is implied by the ☐ header
			// title alone. Pattern 1's "?" gate, Pattern 2's "?" requirement,
			// and Pattern 4's trailing-? all miss; Pattern 3 short-circuits to
			// false because of the leading ⏺ marker. Only the structural
			// footer signal (Type something. + Chat about this) catches it.
			name: "long-body AskUserQuestion with no ? in tail",
			data: "⏺ Important pushback. The user is questioning whether we need a format menu at all — the existing code already prefers originalS3Key (PNG/JPEG) before falling back to WebP. That changes the whole scope.\n" +
				strings.Repeat("─", 200) + "\n" +
				" ☐ Format strategy\n" +
				"\n" +
				"D2 (revised) — Format strategy: explicit menu vs auto-original.\n" +
				"\n" +
				"ELI10: You're right to push on this. The current handleDownload already prefers originalS3Key.\n" +
				"\n" +
				"Stakes if we pick wrong: pick A and 'Save As' means only picking the location.\n" +
				"\n" +
				"Recommendation: A. Honors your instinct and matches the actual user pain.\n" +
				"\n" +
				"Completeness: A=8/10, B=10/10, C=6/10\n" +
				"\n" +
				"Pros / cons:\n" +
				"\n" +
				"A) Auto-original + save-location picker only — no format menu (recommended)\n" +
				"  ✅ Honors the existing originalS3Key-first behavior\n" +
				"  ✅ 'Save As' becomes a single, focused feature\n" +
				"  ✅ Smallest possible diff\n" +
				"  ❌ Edge case: if originalS3Key is missing, user gets WebP\n" +
				"  ❌ Doesn't fully hit Linear AC text 'select format and/or save location'\n" +
				"\n" +
				"B) Auto-original by default + small menu offering format override\n" +
				"  ✅ Default click = save in original format with location picker\n" +
				"  ✅ Menu with 'Save as PNG / JPEG / WebP / Original' available\n" +
				"  ✅ Hits Linear AC fully\n" +
				"  ❌ More UI to build\n" +
				"  ❌ Format menu is dead weight 90% of the time\n" +
				"\n" +
				"C) Format menu, no save-location\n" +
				"  ✅ Simpler than B\n" +
				"  ❌ Doesn't address what you actually asked\n" +
				"  ❌ Misses the 'save location' half of the Linear AC entirely\n" +
				"\n" +
				"Net: If 'save-location' is the real user pain, A is the cleanest.\n" +
				"\n" +
				"❯ 1. A) Auto-original + location picker only (recommended)\n" +
				"     Keep originalS3Key behavior. 'Save As' = pick where to save. No format menu.\n" +
				"  2. B) Auto-original default + format override menu\n" +
				"     Default = original format. Menu lets power-users override.\n" +
				"  3. C) Format menu only, no location picker\n" +
				"     Original D2 answer — PNG/JPEG/WebP menu, no save-location.\n" +
				"  4. Type something.\n" +
				strings.Repeat("─", 200) + "\n" +
				"  5. Chat about this\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · Esc to cancel\n",
			want: true,
		},
		{
			// Pattern 0 (user reported miss): standard-layout AskUserQuestion
			// where Claude prints a plain-prose lead-in line ABOVE the divider
			// (not a ⏺ response marker) immediately before the card. The card
			// carries a literal "?" in the question, multi-line option
			// descriptions, the "Type something." / "Chat about this" numbered
			// terminators, and the instruction footer. The structural footer
			// signal (Type something. + Chat about this) catches it regardless
			// of how far the descriptions push the ☐ header up the buffer.
			name: "standard-layout AskUserQuestion with prose lead-in and footer",
			data: "  One destructive fork I shouldn't assume (auto-resolving is irreversible-ish):\n" +
				strings.Repeat("─", 200) + "\n" +
				" ☐ Headless sub-threshold\n" +
				"\n" +
				"In HEADLESS triage (no human present), what should happen to a below-threshold Sentry issue? Interactive mode already asks the user and resolves on yes — but headless can't ask.\n" +
				"\n" +
				"❯ 1. Skip, leave open (Recommended)\n" +
				"     Headless never resolves without consent. Below-threshold issues are left untouched in Sentry; only at-or-above-threshold issues get tickets. Safe and non-destructive.\n" +
				"  2. Auto-resolve below-threshold\n" +
				"     Headless resolves below-threshold issues in Sentry automatically. Maximal cleanup, but resolves real issues with no human confirmation — risky if scoring misjudges one.\n" +
				"  3. Make it a flag\n" +
				"     Default skip, but support an opt-in arg (e.g. `auto-resolve`) that callers pass when they explicitly want headless cleanup. More surface area to build and document.\n" +
				"  4. Type something.\n" +
				strings.Repeat("─", 200) + "\n" +
				"  5. Chat about this\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · Esc to cancel\n",
			want: true,
		},
		{
			// Pattern 0b (the reported miss): AskUserQuestion rendered in the
			// side-by-side PREVIEW layout (options carry `preview` content, so
			// the UI splits into a left option list + a right box-drawn panel).
			// Every body-structure pattern fails here:
			//   - Pattern 0: "Chat about this" is un-numbered, no "Type
			//     something." line.
			//   - Pattern 1: no ❯ chevron (selection is a background color,
			//     stripped by StripANSI).
			//   - Pattern 2: the preview panel's box-drawing chars (┌│└─) land
			//     on column-0 lines interleaved between the numbered options,
			//     breaking the consecutive-numbered-option run at count 1.
			//   - Pattern 3: the ⏺ Working spinner below has no trailing "?".
			// Only the instruction-footer fast-path catches it.
			name: "preview-layout AskUserQuestion (instruction footer)",
			data: " ☐ Which proof?\n" +
				"\n" +
				"Three identical passing proofs for WON-1174 already exist on PR #332. What should this invocation do?\n" +
				"\n" +
				" 1. Repeat-click (no dup\n" +
				"   pills)\n" +
				"┌──────────────────────────────┐\n" +
				"│ Generate img → Load setup ... │\n" +
				"  2. txt2img Load setup\n" +
				"  3. Re-run same proof\n" +
				" 4. Stop — proofs are\n" +
				"   sufficient\n" +
				"                                  Notes: press n to add notes\n" +
				"\n" +
				strings.Repeat("─", 50) + "\n" +
				"  Chat about this\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · n to add notes · Esc to cancel\n" +
				"\n" +
				"⏺ Working… (3s · ↑ 0 tokens)\n" +
				"  Opus 4.7 | Context: 89% remaining\n",
			want: true,
		},
		{
			// Pattern 0b: the no-notes footer variant. Same fast-path, footer
			// without the "n to add notes" segment.
			name: "preview-layout AskUserQuestion (no-notes footer)",
			data: " ☐ Pick one\n" +
				"\n" +
				" 1. Alpha\n" +
				"┌──────────┐\n" +
				"│ preview  │\n" +
				"  2. Beta\n" +
				"└──────────┘\n" +
				"\n" +
				"  Chat about this\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · Esc to cancel\n",
			want: true,
		},
		{
			// Negative guard: prose mentioning "Enter to select" mid-sentence
			// must NOT fire. The instruction-footer regex is anchored to a full
			// line and requires "to navigate" + "Esc to cancel" in order on that
			// same line, none of which a normal Claude response satisfies.
			name: "prose mentioning enter to select is not a footer",
			data: "⏺ To open the file, press Enter to select a file from the picker.\n",
			want: false,
		},
		{
			// Negative guard (review #692): the instruction footer appearing as
			// a verbatim line inside a tool-output block -- e.g. Claude reads or
			// prints a file/test fixture that literally contains
			// "Enter to select · ↑/↓ to navigate · Esc to cancel" -- must NOT
			// fire. The line arrives as a ⎿ continuation indented 4+ spaces,
			// exactly the shape the footer regex accepts, but Pattern 0b matches
			// the tool-output-filtered tail, so stripToolOutput removes the block
			// before the regex runs. Without that filtering this returned true.
			name: "instruction footer inside tool output is not a live prompt",
			data: "⏺ Here is the fixture I'm referencing.\n" +
				"  ⎿  Read footer.txt (1 line)\n" +
				"       Enter to select · ↑/↓ to navigate · Esc to cancel\n",
			want: false,
		},
		{
			// Negative guard (review #692, follow-up): the instruction footer
			// written as ordinary assistant response text -- e.g. Claude
			// documenting the TUI footer in prose -- must NOT fire. It is not
			// tool output, so stripToolOutput leaves it in cleanedTail, but
			// Pattern 0b requires the full card terminator (a "Chat about this"
			// line immediately followed by the footer), which a lone footer
			// mention lacks, so Pattern 3 then rejects the non-question response.
			name: "instruction footer in assistant prose without a card is not a prompt",
			data: "⏺ The AskUserQuestion footer renders as:\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · Esc to cancel\n" +
				"\n" +
				"That line sits below the option card.\n",
			want: false,
		},
		{
			// Negative guard (review #692, follow-up 2): a completed response
			// that documents the TUI with a Markdown numbered list plus the
			// footer line as prose must NOT fire. Pattern 0b now ties the footer
			// to the AskUserQuestion card terminator ("Chat about this"), which a
			// coincidental numbered list lacks, so the footer + a numbered item
			// no longer satisfies the card-context requirement.
			name: "footer with markdown numbered list but no card terminator is not a prompt",
			data: "⏺ The picker shows a numbered list, e.g.:\n" +
				"\n" +
				"  1. First choice\n" +
				"  2. Second choice\n" +
				"\n" +
				"and the footer reads:\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · Esc to cancel\n",
			want: false,
		},
		{
			// Negative guard (review #692, follow-up 3): assistant prose that
			// documents the card with a standalone "Chat about this" line AND
			// the footer line, but with prose between them, must NOT fire.
			// Pattern 0b requires the terminator and footer as one ordered,
			// adjacent sequence (blank lines only between), so independent
			// mentions anywhere in the tail no longer satisfy it. Without the
			// adjacency requirement (two independent regex matches) this fired.
			name: "non-adjacent chat-about-this and footer in prose is not a prompt",
			data: "⏺ The AskUserQuestion card ends with two lines:\n" +
				"\n" +
				"Chat about this\n" +
				"\n" +
				"and then, after a divider, the footer:\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · Esc to cancel\n",
			want: false,
		},
		{
			// Review #692, follow-up: a suggested-prompt section that mixes an
			// imperative item with a question-shaped one must have the question
			// item stripped. The non-question "1. Run tests" must not end the
			// section early and leak "2. Want me to open a PR?" into the tail,
			// which would otherwise fire Pattern 3/4 on an idle, completed pane.
			name: "mixed suggested-prompt list does not leak the question item",
			data: "⏺ Done — all set.\n" +
				"\n" +
				"Suggested next prompts:\n" +
				"1. Run tests\n" +
				"2. Want me to open a PR?\n",
			want: false,
		},
		{
			// Review #692, follow-up: an UNBULLETED suggested-prompt block. The
			// plain imperative line ("Run tests") must not reset the section, or
			// the later question-shaped suggestion ("Want me to open a PR?")
			// would leak into the tail and fire Pattern 3/4 on a completed pane.
			// The block now runs until a stop-marker line or EOF.
			name: "unbulleted suggested-prompt block does not leak the question item",
			data: "⏺ Done — all set.\n" +
				"\n" +
				"Suggested next prompts:\n" +
				"Run tests\n" +
				"Want me to open a PR?\n",
			want: false,
		},
		{
			// Review #692, follow-up: a suggested-prompt block followed by a
			// genuine follow-up question in the same response. A blank line ends
			// the block, so the real trailing question is preserved and detected
			// rather than stripped as if it were a suggestion. This guards
			// against over-stripping (a false negative) introduced when the
			// block was kept open to EOF.
			name: "real question after suggested-prompt block is still detected",
			data: "⏺ Here are some options.\n" +
				"\n" +
				"Suggested next prompts:\n" +
				"Run tests\n" +
				"\n" +
				"Before I continue, should I open a PR?\n",
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
			// Subset invariant, asserted across every fixture in this table
			// rather than in one hand-picked case: a modal is always also a
			// question. The reverse must NOT hold, which is the whole point of
			// the split -- see TestHasModalPrompt.
			if HasModalPrompt([]byte(tt.data)) && !got {
				t.Errorf("HasModalPrompt() = true while HasQuestionPrompt() = false; the modal set must be a subset")
			}
		})
	}
}

// TestHasModalPrompt pins the split BOS-600 depends on: a pane can be waiting
// on the user (notify) while still being perfectly safe to type into (deliver).
// Gating delivery on HasQuestionPrompt refused every pane in the "live composer"
// group below -- which is to say, it refused to answer the question Claude had
// just asked.
func TestHasModalPrompt(t *testing.T) {
	tests := []struct {
		name         string
		data         string
		wantQuestion bool
		wantModal    bool
	}{
		{
			// THE regression. Claude asks in prose and leaves the composer
			// drawn and empty; typing an answer is exactly the right move.
			name:         "conversational question with a live composer",
			data:         "⏺ I've updated the client. Want me to run the tests now?\n\n❯ \n  claude-opus-4 · ~/code/bossanova · ready\n",
			wantQuestion: true,
			wantModal:    false,
		},
		{
			// Pattern 4: the same shape with the ⏺ marker scrolled out of the
			// 30-line tail. Still a live composer, still safe to type into.
			name:         "trailing question mark with the response marker out of the tail",
			data:         strings.Repeat("some earlier output line\n", 40) + "Should I open the PR now?\n\n❯ \n",
			wantQuestion: true,
			wantModal:    false,
		},
		{
			name:         "idle composer with no question at all",
			data:         "⏺ Done. The tests pass.\n\n❯ \n",
			wantQuestion: false,
			wantModal:    false,
		},
		{
			// Pattern 1: selector + options. The composer is gone; Enter picks.
			name:         "permission prompt selector",
			data:         "  Claude wants to run a command. Allow?\n\n  ❯ Allow\n    Allow once\n    Deny\n",
			wantQuestion: true,
			wantModal:    true,
		},
		{
			// Pattern 2: ☐ card detected structurally, no selector cursor.
			name:         "AskUserQuestion card without a selector cursor",
			data:         " ☐ Test prompt\n\nWhich approach should we take?\n\n  1. Option A\n     First option\n  2. Option B\n     Second option\n",
			wantQuestion: true,
			wantModal:    true,
		},
		{
			// Pattern 0: footer fast-path, question text already scrolled off.
			name:         "AskUserQuestion footer with the question scrolled off",
			data:         "  1. Option A\n  2. Option B\n  4. Type something.\n  5. Chat about this\n",
			wantQuestion: true,
			wantModal:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasQuestionPrompt([]byte(tt.data)); got != tt.wantQuestion {
				t.Errorf("HasQuestionPrompt() = %v, want %v", got, tt.wantQuestion)
			}
			if got := HasModalPrompt([]byte(tt.data)); got != tt.wantModal {
				t.Errorf("HasModalPrompt() = %v, want %v -- a delivery gate reading this would %s",
					got, tt.wantModal,
					map[bool]string{true: "refuse a pane it can safely type into", false: "type into a menu"}[got])
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

func TestHasQuestionPrompt_FooterPhrasesInProse(t *testing.T) {
	// The Pattern 0 fast-path keys on "Type something." and "Chat about this"
	// as numbered option lines. Prose containing the same phrases mid-sentence
	// must NOT trigger detection -- only the indented numbered-option shape
	// counts.
	tests := []struct {
		name string
		data string
	}{
		{
			name: "phrases mid-sentence, not numbered options",
			data: "⏺ Here's how the picker works. You can Type something. or Chat about this in the prompt.\n",
		},
		{
			name: "phrases at column 0 (no leading indent)",
			data: "⏺ Steps below:\n" +
				"\n" +
				"1. Type something.\n" +
				"2. Chat about this\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if HasQuestionPrompt([]byte(tt.data)) {
				t.Errorf("should NOT detect question when footer phrases appear as prose, not as indented numbered options")
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
		// BOS-719: bossd captures panes with `tmux capture-pane -p -S -1000`
		// and NO `-J`, so a tip wider than the pane (four live agent panes are
		// 43 columns) arrives as a start line plus one or more continuation
		// lines. The trailing "?" lands on the continuation, which the old
		// single-line tip regex could never reach.
		{
			name: "wrapped ⎿ tip, continuation indented 2 spaces",
			data: "⏺ Done with the refactor.\n" +
				"\n" +
				"✽ Newspapering… (5m 4s · ↓ 19.9k tokens)\n" +
				"  ⎿  Tip: Did you know you can drag and\n" +
				"  drop image files into your terminal?\n",
		},
		{
			name: "wrapped ⎿ tip, continuation at column 0",
			data: "⏺ Done with the refactor.\n" +
				"\n" +
				"✽ Newspapering… (5m 4s · ↓ 19.9k tokens)\n" +
				"  ⎿  Tip: Did you know you can drag and\n" +
				"drop image files into your terminal?\n",
		},
		{
			name: "wrapped bare tip (no ⎿), continuation indented 2 spaces",
			data: "⏺ Done with the refactor.\n" +
				"\n" +
				"✽ Newspapering… (5m 4s · ↓ 19.9k tokens)\n" +
				"  Tip: Did you know you can drag and\n" +
				"  drop image files into your terminal?\n",
		},
		{
			name: "one-line tip introduced by a non-⎿ decoration glyph (※)",
			data: "⏺ Done with the refactor.\n" +
				"\n" +
				"✽ Newspapering… (5m 4s · ↓ 19.9k tokens)\n" +
				"  ※ Tip: Did you know you can drag and drop image files into your terminal?\n",
		},
		{
			// Guard: this shape already returned false before BOS-719 (the
			// 4+-space continuation made toolOutputBlockRe eat the whole
			// block). It must keep returning false after the tip-block
			// scanner replaces the single-line regex.
			name: "wrapped ⎿ tip, continuation indented 5 spaces (already passing guard)",
			data: "⏺ Done with the refactor.\n" +
				"\n" +
				"✽ Newspapering… (5m 4s · ↓ 19.9k tokens)\n" +
				"  ⎿  Tip: Did you know you can drag and\n" +
				"     drop image files into your terminal?\n",
		},
		// The remaining allowlisted decorations. ※ is covered above; these pin
		// the other three so a shrunken tipDecorations map is caught here.
		{
			name: "wrapped tip introduced by └",
			data: "⏺ Done with the refactor.\n" +
				"\n" +
				"  └ Tip: Did you know you can drag and\n" +
				"  drop image files into your terminal?\n",
		},
		{
			name: "wrapped tip introduced by •",
			data: "⏺ Done with the refactor.\n" +
				"\n" +
				"  • Tip: Did you know you can drag and\n" +
				"  drop image files into your terminal?\n",
		},
		{
			name: "wrapped tip introduced by ·",
			data: "⏺ Done with the refactor.\n" +
				"\n" +
				"  · Tip: Did you know you can drag and\n" +
				"  drop image files into your terminal?\n",
		},
		// A ⎿-decorated tip start is ALSO a tool-output block header. The
		// full-buffer tip pass deletes that header, so stripToolOutput can no
		// longer anchor on it -- if the tip sweep stopped at the row cap, every
		// row past it would be orphaned into the tail as ordinary text. These
		// two pin the shapes that regression produced: a long tool result, and a
		// pasted question card printed inside one.
		{
			name: "⎿ tip heading a tool-output block longer than the sweep cap",
			data: func() string {
				var b strings.Builder
				b.WriteString("⏺ Ran a command.\n")
				b.WriteString("  ⎿  Tip: Use --force to override\n")
				for i := range maxTipContinuationLines + 2 {
					fmt.Fprintf(&b, "     output row %d of the tool result\n", i)
				}
				b.WriteString("     did you mean to do that?\n")
				return b.String()
			}(),
		},
		// A bare "Tip:" row inside a FOREIGN ⎿ block -- one headed by a command,
		// not by a tip. This pass runs before stripToolOutput, so blanking that
		// row must keep its indent: a bare blank would truncate
		// toolOutputBlockRe's 4+-space run at that point and orphan every row
		// below it into the tail. The second case is the expensive direction --
		// a question card echoed by a tool reaching the MODAL patterns.
		{
			name: "bare Tip: row inside a foreign ⎿ tool block",
			data: func() string {
				var b strings.Builder
				b.WriteString("⏺ Ran a command.\n")
				b.WriteString("  ⎿  $ grep -rn Tip docs/\n")
				b.WriteString("     Tip: use the frozen lockfile\n")
				for i := range maxTipContinuationLines + 2 {
					fmt.Fprintf(&b, "     tool output row %d\n", i)
				}
				b.WriteString("     did you mean to do that?\n")
				return b.String()
			}(),
		},
		{
			name: "card echoed inside a foreign ⎿ tool block below a Tip: row",
			data: "⏺ Here is the card fixture I read.\n" +
				"  ⎿  $ cat testdata/card.txt\n" +
				"     Tip: the card renders like this\n" +
				"     filler row a\n     filler row b\n     filler row c\n" +
				"      ☐ Which approach?\n" +
				"     Which approach should I take?\n" +
				"       1. Rebase\n       2. Merge\n       3. Type something.\n" +
				"     ────────\n       4. Chat about this\n",
		},
		{
			name: "AskUserQuestion card pasted inside a ⎿ Tip: tool block",
			data: "⏺ Here is what the card looks like.\n" +
				"  ⎿  Tip: the card renders like this\n" +
				"      ☐ Which approach?\n" +
				"     Which approach should I take?\n" +
				"       1. Rebase\n" +
				"       2. Merge\n" +
				"       3. Type something.\n" +
				"     ────────\n" +
				"       4. Chat about this\n",
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

// TestStripTipLines_BlockStopConditions pins the tip-block continuation sweep's
// stop conditions directly. They are the only thing bounding a strictly more
// aggressive strip than the pre-BOS-719 single-line regex, and over-stripping
// fails SILENTLY -- a swallowed question never pings a human -- so each stop is
// asserted on its own rather than only through HasQuestionPrompt.
//
// Cases pin the rows that SURVIVE, not the exact output bytes, so the table is
// not coupled to how a dropped row is represented. The shape-preserving
// invariant -- every output row is either its input row verbatim or exactly
// that row's leading whitespace -- is asserted separately, for every case.
func TestStripTipLines_BlockStopConditions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		kept []string // the non-blank rows that must survive, in order
	}{
		{
			name: "blank line closes the block and is kept",
			in:   "  Tip: wrapped tip text\n  continuation row\n\nkept?\n",
			kept: []string{"kept?"},
		},
		{
			name: "optionStopMarker ⏺ closes the block and is kept",
			in:   "  Tip: wrapped tip text\n  continuation row\n⏺ Want me to continue?\n",
			kept: []string{"⏺ Want me to continue?"},
		},
		{
			name: "optionStopMarker ❯ closes the block and is kept",
			in:   "  Tip: wrapped tip text\n  continuation row\n❯ 1. Yes\n",
			kept: []string{"❯ 1. Yes"},
		},
		{
			name: "☐ card header closes the block and is kept",
			in:   "  ⎿  Tip: wrapped tip text\n  continuation row\n ☐ Which approach?\n  1. Rebase\n",
			kept: []string{" ☐ Which approach?", "  1. Rebase"},
		},
		{
			name: "• bullet closes the block and is kept",
			in:   "  Tip: wrapped tip text\n  continuation row\n  • Should I apply it now?\n",
			kept: []string{"  • Should I apply it now?"},
		},
		{
			name: "numbered option row closes the block and is kept",
			in:   "  Tip: wrapped tip text\n  continuation row\n  1. Rebase\n  2. Merge\n",
			kept: []string{"  1. Rebase", "  2. Merge"},
		},
		{
			name: "horizontal rule closes the block and is kept",
			in:   "  Tip: wrapped tip text\n  continuation row\n────────\n  5. Chat about this\n",
			kept: []string{"────────", "  5. Chat about this"},
		},
		{
			// The start line plus maxTipContinuationLines (3) rows are swept;
			// the 4th row below the start line survives even though nothing
			// about it stops the block.
			name: "sweep is bounded at maxTipContinuationLines",
			in: "  Tip: a very long tip that keeps\n" +
				"  wrapping onto row two\n" +
				"  and onto row three\n" +
				"  and onto row four\n" +
				"and here is real content?\n",
			kept: []string{"and here is real content?"},
		},
		{
			name: "end of input closes the block",
			in:   "⏺ Done.\n  Tip: wrapped tip text\n  continuation row\n",
			kept: []string{"⏺ Done."},
		},
		{
			// A ⎿ tip start is also a tool-output block header, so its 4+-space
			// rows follow toolOutputBlockRe's rule, NOT the row cap.
			name: "⎿ tip keeps the tool-block continuation rule past the cap",
			in: "  ⎿  Tip: heads a tool block\n" +
				"     row one\n     row two\n     row three\n     row four\n     row five?\n" +
				"⏺ Next turn.\n",
			kept: []string{"⏺ Next turn."},
		},
		{
			// The tool-block rule is keyed to ⎿ only: a bare tip's rows stay on
			// the bounded prose sweep even when indented 4+.
			name: "bare tip does not get the tool-block rule",
			in: "  Tip: bare tip\n" +
				"     row one\n     row two\n     row three\n     row four?\n",
			kept: []string{"     row four?"},
		},
		{
			// toolOutputBlockRe's continuation run is CONTIGUOUS: the first row
			// indented under 4 spaces ends the tool block for good. Letting the
			// tool branch re-engage on a later deep row would skip the stop
			// runes and the cap and swallow live UI.
			name: "⎿ tool block ends for good at the first shallow row",
			in: "  ⎿  Tip: heads a tool block\n     deep row\n" +
				"  shallow wrap?\n  p2\n  p3\n     deep row again?\n⏺ Next.\n",
			kept: []string{"     deep row again?", "⏺ Next."},
		},
		{
			name: "a second tip start inside a block re-arms the sweep",
			in:   "  Tip: first tip\n  continuation\n  ※ Tip: second tip\n  continuation\n\nkept\n",
			kept: []string{"kept"},
		},
		{
			name: "text with no tip is untouched",
			in:   "⏺ Done.\n  Not a tip line.\nShould I continue?\n",
			kept: []string{"⏺ Done.", "  Not a tip line.", "Should I continue?"},
		},
	}
	if maxTipContinuationLines != 3 {
		t.Fatalf("the bounded-sweep fixture assumes maxTipContinuationLines == 3, got %d", maxTipContinuationLines)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(stripTipLines([]byte(tt.in)))
			inRows := strings.Split(tt.in, "\n")
			outRows := strings.Split(got, "\n")
			// Shape-preserving invariant: this pass runs BEFORE stripToolOutput,
			// so every row must survive either verbatim or as exactly its own
			// leading whitespace. Losing the row would slide the LastNLines
			// window; losing the indent would truncate a ⎿ tool block at the
			// blank and orphan the rest of it into the tail.
			if len(inRows) != len(outRows) {
				t.Fatalf("row count changed: in=%d out=%d\n  got: %q", len(inRows), len(outRows), got)
			}
			var kept []string
			for i, in := range inRows {
				out := outRows[i]
				switch {
				case out == in:
					if strings.TrimSpace(out) != "" {
						kept = append(kept, out)
					}
				case out == in[:len(in)-len(strings.TrimLeft(in, " \t"))]:
					// Dropped, blanked in place with its indent intact.
				default:
					t.Errorf("row %d is neither kept nor indent-blanked\n  in:  %q\n  out: %q", i, in, out)
				}
			}
			if strings.Join(kept, "\x00") != strings.Join(tt.kept, "\x00") {
				t.Errorf("surviving rows\n  got:  %q\n  want: %q\n  full: %q", kept, tt.kept, got)
			}
		})
	}
}

// TestHasQuestionPrompt_RealQuestionBelowTip guards the direction the BOS-719
// tip-block sweep made newly risky: a REAL question sitting directly beneath a
// tip must survive. HasModalPrompt is asserted alongside HasQuestionPrompt for
// the card shape because HasModalPrompt gates delivery (BOS-600) -- a false
// negative there types a message into a pane whose keystrokes are consumed as
// selections.
func TestHasQuestionPrompt_RealQuestionBelowTip(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantModal bool
	}{
		{
			name: "AskUserQuestion card directly under a wrapped tip, no blank line",
			data: "⏺ Working on it.\n" +
				"  ⎿  Tip: Did you know you can drag and\n" +
				"  drop image files into your terminal?\n" +
				" ☐ Which approach should I take?\n" +
				"\n" +
				"Which approach should I take?\n" +
				"\n" +
				"  1. Rebase\n" +
				"  2. Merge\n" +
				"  3. Type something.\n" +
				"────────\n" +
				"  4. Chat about this\n",
			wantModal: true,
		},
		{
			name: "bulleted list whose first item starts with Tip:",
			data: "⏺ A few notes:\n" +
				"  • Tip: use the cache for repeat lookups\n" +
				"  • The migration is reversible\n" +
				"  • Should I apply it now?\n",
			wantModal: false,
		},
		{
			// A ⎿ tip heads a tool block, but that block ends for good at the
			// first row indented under 4 spaces. Everything 4+-indented BELOW
			// that break is live UI, not tool output, and must survive: if the
			// tool branch re-engaged it would skip the stop runes and the row
			// cap and swallow this whole card.
			name: "card indented 4+ below a shallow row under a ⎿ tip",
			data: "⏺ Here is the card.\n" +
				"  ⎿  Tip: wrapped tip text\n" +
				"  shallow wrap row\n" +
				"    ☐ Which approach?\n" +
				"    Which approach should I take?\n" +
				"    1. Rebase\n" +
				"    2. Merge\n" +
				"    3. Type something.\n" +
				"    ────────\n" +
				"    4. Chat about this\n",
			wantModal: true,
		},
		{
			name: "question further below a tip than the sweep bound",
			data: "⏺ Working.\n" +
				"  Tip: a very long tip that keeps\n" +
				"  wrapping onto row two\n" +
				"  and onto row three\n" +
				"  and onto row four\n" +
				"  Should I continue with the deploy?\n",
			wantModal: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !HasQuestionPrompt([]byte(tt.data)) {
				t.Errorf("should detect question below a tip\n  stripped: %q",
					string(stripTipLines(StripANSI([]byte(tt.data)))))
			}
			if got := HasModalPrompt([]byte(tt.data)); got != tt.wantModal {
				t.Errorf("HasModalPrompt = %v, want %v\n  stripped: %q", got, tt.wantModal,
					string(stripTipLines(StripANSI([]byte(tt.data)))))
			}
		})
	}
}

// TestHasQuestionPrompt_DecoratedRowUnderSelector pins the exclusion of └ and ※
// from tipBlockStops. Both open a tip block but must never CLOSE one: they are
// absent from optionStopMarkers, so a row kept below a stripped tip re-enters
// countConsecutiveOptionLines -- whose optionRe gate accepts any 2+-space row --
// counts as a live option under the "❯ " selector, and fires Pattern 1. That
// flips HasModalPrompt false->TRUE against base, a modal FALSE POSITIVE.
//
// This is not one of the two documented accepted-risk shapes; both of those are
// over-sweeping false negatives. Sweeping the decorated row instead matches the
// base commit's behaviour for these panes.
func TestHasQuestionPrompt_DecoratedRowUnderSelector(t *testing.T) {
	// └ and ※ are the two glyphs excluded from the stop set, plus ⎿ and · as
	// controls: both are in optionStopMarkers, so they are kept AND break the
	// option run, and they pass either way.
	//
	// • is deliberately absent. It is a stop (tipBlockExtraStops) and so shows
	// the same flip on this pane, but it cannot simply be swept: the
	// "bulleted list whose first item starts with Tip:" case in
	// TestHasQuestionPrompt_RealQuestionBelowTip needs a • row below a • tip to
	// survive, or a genuine question is swallowed. Resolving that needs • in
	// optionStopMarkers -- the same pre-existing root cause tracked for └ --
	// rather than a change to this sweep.
	for _, decoration := range []string{"└", "※", "⎿", "·"} {
		t.Run(decoration, func(t *testing.T) {
			data := "⏺ Let me look for it.\n" +
				"\n" +
				"❯ where is the config?\n" +
				"  · Tip: Press Ctrl-R to expand collapsed tool output\n" +
				"  " + decoration + "  Did you mean services/boss/config.go?\n"
			if HasModalPrompt([]byte(data)) {
				t.Errorf("HasModalPrompt = true, want false -- a %q row under a selector "+
					"must not count as a live option\n  stripped: %q",
					decoration, string(stripTipLines(StripANSI([]byte(data)))))
			}
		})
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

func TestHasQuestionPrompt_SelectedFirstOption(t *testing.T) {
	// Hardening: when the selection cursor sits on the FIRST option ("❯ 1."),
	// the blanket "❯ " strip used by the other patterns deletes option 1, so
	// Pattern 2's numbered run appeared to start at 2 and the
	// strictly-increasing-from-1 check rejected a real card. Pattern 2 now
	// keeps "❯ N." selector lines and normalizes the cursor to blanks.
	//
	// The positive inputs are crafted so EVERY other pattern fails, leaving
	// Pattern 2 as the only possible catcher:
	//   - 1-space-indented options (Pattern 1 requires 2-space option lines),
	//   - a mid-line "?" (Pattern 4 requires a trailing "?"),
	//   - no Type-something/Chat-about-this/footer (Patterns 0 and 0b),
	//   - no ⏺ response marker (Pattern 3 is skipped).
	positive := []struct{ name, data string }{
		{
			name: "selected first option, 1-space layout",
			data: strings.Repeat("─", 50) + "\n" +
				" ☐ Headless sub-threshold\n" +
				"\n" +
				"What should happen to a below-threshold Sentry issue? Pick one below.\n" +
				"\n" +
				"❯ 1. Skip, leave open\n" +
				" 2. Auto-resolve\n" +
				" 3. Make it a flag\n",
		},
	}
	for _, tt := range positive {
		t.Run(tt.name, func(t *testing.T) {
			if !HasQuestionPrompt([]byte(tt.data)) {
				clean := StripANSI([]byte(tt.data))
				t.Errorf("should detect question (selected first option)\n  clean: %q", string(clean))
			}
		})
	}

	// Negative guard: keeping "❯ N." selector lines for Pattern 2 must not make
	// a numbered list fire without a real card. With no "?" in the question
	// region between the ☐ header and the options, Pattern 2 stays off.
	negative := []struct{ name, data string }{
		{
			name: "selector numbered list but no question-region ?",
			data: strings.Repeat("─", 50) + "\n" +
				" ☐ Plan\n" +
				"\n" +
				"Here is the plan.\n" +
				"\n" +
				"❯ 1. Do the first thing\n" +
				" 2. Do the second thing\n",
		},
	}
	for _, tt := range negative {
		t.Run(tt.name, func(t *testing.T) {
			if HasQuestionPrompt([]byte(tt.data)) {
				clean := StripANSI([]byte(tt.data))
				t.Errorf("should NOT detect question\n  clean: %q", string(clean))
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

// TestCountConsecutiveNumberedOptions_NewlineAtIndexZero covers the boundary
// mutation on question.go:180 (`if nl < 0` → `nl <= 0`). When data starts
// with '\n' the first IndexByte returns nl==0. The unmutated code must take
// the else branch (line = data[:0] = "", advance past the newline) and treat
// it as a blank line to skip. The mutant `nl <= 0` swallows the ENTIRE input
// as one line, so the numbered options that follow are never parsed and the
// count collapses to 0.
func TestCountConsecutiveNumberedOptions_NewlineAtIndexZero(t *testing.T) {
	data := []byte("\n  1. first\n  2. second\n  3. third\n")
	count, broken := countConsecutiveNumberedOptions(data)
	if broken {
		t.Errorf("should not be broken by marker, got broken=true")
	}
	if count != 3 {
		t.Errorf("count = %d, want 3 (leading '\\n' is a blank line, options follow)", count)
	}
}

// TestCountConsecutiveNumberedOptions_ColumnZeroNonOption covers the
// CONDITIONALS_NEGATION mutation on question.go:199
// (`if line[0] != ' ' && line[0] != '\t'`). A non-blank, non-marker,
// non-option line at column 0 must END the run (return), while an indented
// continuation line must be ALLOWED (continue). Both sides are asserted so a
// flipped condition is caught either way.
func TestCountConsecutiveNumberedOptions_ColumnZeroNonOption(t *testing.T) {
	// Column-0 prose line after the first option terminates the run: count==1.
	endByColumnZero := []byte("  1. first\nprose at column zero\n  2. second\n")
	count, broken := countConsecutiveNumberedOptions(endByColumnZero)
	if broken {
		t.Errorf("column-0 line should not set brokenByMarker, got broken=true")
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (column-0 prose ends the run before option 2)", count)
	}

	// Indented continuation line between options is allowed: run continues to 2.
	allowContinuation := []byte("  1. first\n     wrapped continuation text\n  2. second\n")
	count, broken = countConsecutiveNumberedOptions(allowContinuation)
	if broken {
		t.Errorf("indented continuation should not set brokenByMarker, got broken=true")
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (indented continuation allowed between options)", count)
	}
}

// TestCountConsecutiveNumberedOptions_MultiDigit covers the ARITHMETIC_BASE
// mutation on question.go:207 (`n = n*10 + int(c-'0')`). A run reaching a
// two-digit option (10.) requires base-10 accumulation: parsing "10" must
// yield exactly 10 so the strictly-increasing check (n == prev+1) passes.
// The mutant `n+10` (or other swaps) parses "10" as 1+0=1, which breaks the
// run at option 10, collapsing the count to 9.
func TestCountConsecutiveNumberedOptions_MultiDigit(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("  ")
		b.WriteString(itoa(i))
		b.WriteString(". option\n")
	}
	count, broken := countConsecutiveNumberedOptions([]byte(b.String()))
	if broken {
		t.Errorf("should not be broken by marker, got broken=true")
	}
	if count != 10 {
		t.Errorf("count = %d, want 10 (two-digit option requires n*10 base-10 parse)", count)
	}
}

// TestHasQuestionPrompt_FirstSelectorNoOptions covers the INVERT_NEGATIVES and
// ARITHMETIC_BASE mutations on question.go:298
// (`selectorRe.FindAllIndex(tail, -1)`). The `-1` means "find ALL selector
// matches"; mutating it to `1` (invert/arith) makes FindAllIndex return only
// the FIRST match.
//
// Here the FIRST "❯ " selector (the user's prompt history) is immediately
// followed by a Claude marker line (⏺), so it has ZERO valid option lines
// after it. The LAST "❯ " selector is the real AskUserQuestion with options.
//
//   - Real code iterates newest-first over ALL matches, finds the last selector
//     with options → true.
//   - Mutant (only the first match) sees the user-prompt selector with no
//     options (run broken immediately by the ⏺ marker) → false.
//
// This distinguishes the mutant from real, unlike the prior multi-selector
// fixture whose first selector happened to have a passing option run.
func TestHasQuestionPrompt_FirstSelectorNoOptions(t *testing.T) {
	// The "?" is mid-sentence (no line ends with "?") so Patterns 3 and 4
	// cannot fire; there is no ☐ header (Pattern 2 out) and no footer
	// (Pattern 0 out). Pattern 1 is the only path that can return true.
	//
	// First selector "❯ first?" is immediately followed by a ⏺ marker line,
	// so countConsecutiveOptionLines aborts (brokenByMarker) with zero options.
	// The last selector has two indented option lines.
	data := "❯ first? then text\n" +
		"⏺ marker right after the first selector\n" +
		"\n" +
		"Which one? pick:\n" +
		"\n" +
		"  ❯ Refactor the API\n" +
		"    Move all handlers to /v2\n" +
		"    Use the new error helper\n"
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect question on the LAST selector with options when the first selector has none (FindAllIndex -1 -> all matches)")
	}
}

// TestHasQuestionPrompt_Pattern1ExactlyOneOptionIsolated covers the boundary
// mutation on question.go:307 (`if !broken && count >= 1` → `count > 1`).
// Pattern 1 must fire when the selector is followed by EXACTLY ONE indented
// option line. The mutant `count > 1` would require two options and miss it.
//
// The "?" is mid-sentence (no line ends with "?"), so trailingQuestionRe
// (Patterns 3 and 4) cannot fire. There is no ☐ header (Pattern 2 out) and no
// AskUserQuestion footer (Pattern 0 out). Pattern 1 is therefore the ONLY path
// that can return true, isolating the count>=1 boundary.
func TestHasQuestionPrompt_Pattern1ExactlyOneOptionIsolated(t *testing.T) {
	data := "What now? Pick below:\n" +
		"\n" +
		"  ❯ The single option\n" +
		"    only option detail line\n" +
		"done\n"
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect Pattern 1 with exactly one option line after the selector (count >= 1 boundary)")
	}
}

// TestHasQuestionPrompt_Pattern3MarkerAtIndexZeroShortCircuits covers the
// boundary mutation on question.go:342 (`if idx := ...; idx >= 0` → `idx > 0`).
// The ⏺ response marker sits at byte index 0, and Claude's question ("...?") is
// on the first line — pushed more than 30 lines above the bottom of the pane by
// trailing filler, so the "?" lives in Pattern 3's full-buffer scan but OUTSIDE
// Pattern 4's 30-line tail.
//
//   - Real code: idx==0 satisfies `idx >= 0`, Pattern 3 scans the whole buffer
//     after the marker and matches the trailing "?" → true.
//   - Mutant `idx > 0`: idx==0 fails, Pattern 3 is skipped, and Pattern 4 only
//     sees the last 30 lines (filler, no "?") → false.
//
// The differing result (true vs false) kills the boundary mutant.
func TestHasQuestionPrompt_Pattern3MarkerAtIndexZeroShortCircuits(t *testing.T) {
	var b strings.Builder
	b.WriteString("⏺ Does this approach work for you?\n")
	for range 35 {
		b.WriteString("filler status line with no trailing punctuation\n")
	}
	if !HasQuestionPrompt([]byte(b.String())) {
		t.Error("should detect question via Pattern 3 when ⏺ marker is at index 0 (idx >= 0 boundary)")
	}
}

// itoa is a tiny local helper to avoid an strconv import churn in tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestHasQuestionPrompt_Pattern2ExactlyTwoNumberedOptions covers the boundary
// mutation on question.go:332 (`if count >= 2` → `count > 2`). Pattern 2
// requires a ☐ header, a "?" in the question region, and a strictly-increasing
// numbered-option run. With EXACTLY two options the unmutated code returns true
// (2 >= 2); the mutant `count > 2` would miss it.
//
// The "?" is placed only in the question region (not at any line end) and a ⏺
// marker with no trailing "?" is appended so Pattern 3 short-circuits to false
// and Pattern 4's trailing-? cannot fire -- isolating Pattern 2 as the only
// path that can return true.
func TestHasQuestionPrompt_Pattern2ExactlyTwoNumberedOptions(t *testing.T) {
	data := strings.Repeat("─", 80) + "\n" +
		" ☐ Pick approach\n" +
		"\n" +
		"Which approach? Either works fine here.\n" +
		"\n" +
		" 1. First approach with details\n" +
		" 2. Second approach with details\n" +
		"\n" +
		"⏺ Working… (2s)\n"
	if !HasQuestionPrompt([]byte(data)) {
		t.Error("should detect Pattern 2 card with exactly 2 numbered options (count >= 2 boundary)")
	}
}

// TestHasQuestionPrompt_Pattern2OneNumberedOptionNegative is the lower-side
// guard for the same boundary at question.go:332: a single numbered option
// (count == 1) must NOT trigger Pattern 2. Combined with the test above this
// pins the `>= 2` threshold exactly. The trailing ⏺ with no "?" keeps Patterns
// 3 and 4 from masking the result.
func TestHasQuestionPrompt_Pattern2OneNumberedOptionNegative(t *testing.T) {
	data := strings.Repeat("─", 80) + "\n" +
		" ☐ Pick approach\n" +
		"\n" +
		"Which approach? Only one listed.\n" +
		"\n" +
		" 1. The only approach with details\n" +
		"\n" +
		"⏺ Working… (2s)\n"
	if HasQuestionPrompt([]byte(data)) {
		t.Error("should NOT detect Pattern 2 with only 1 numbered option (count >= 2 lower boundary)")
	}
}

func TestHasQuestionPrompt_SelectedChatAboutThis(t *testing.T) {
	// Review #692: when the user arrows down to the final "Chat about this"
	// action, Claude Code renders that live option with the same "❯ " selection
	// cursor as any other selected row ("❯ Chat about this" in the side-by-side
	// preview layout, "❯ N. Chat about this" in the standard layout). The
	// footer fast-path (Pattern 0b) used the cleanedTail view, whose
	// stripUserPromptLines deleted that "❯ " row before the card-terminator
	// regex ran -- so an active question card flipped to non-question merely by
	// moving the selection onto the last action. Pattern 0b must still detect
	// the card when its terminator carries the selection cursor.
	tests := []struct {
		name string
		data string
	}{
		{
			// Preview layout (Pattern 2 broken by the side-by-side panel), with
			// the selection cursor on the un-numbered "Chat about this".
			name: "preview-layout selected un-numbered Chat about this",
			data: " ☐ Pick one\n" +
				"\n" +
				" 1. Alpha\n" +
				"┌──────────┐\n" +
				"│ preview  │\n" +
				"  2. Beta\n" +
				"└──────────┘\n" +
				"\n" +
				"❯ Chat about this\n" +
				"\n" +
				"Enter to select · ↑/↓ to navigate · Esc to cancel\n",
		},
		{
			// Standard layout with the selection cursor on the numbered
			// "Chat about this", and the ☐ header / "?" pushed out of the tail so
			// only the footer fast-path can catch it. Pattern 0 misses it too:
			// the selector replaces the leading spaces its numbered-option regex
			// requires.
			name: "standard-layout selected numbered Chat about this",
			data: func() string {
				var b strings.Builder
				for range 35 {
					b.WriteString("  prior conversation filler line\n")
				}
				b.WriteString("  1. Option A\n")
				b.WriteString("  2. Option B\n")
				b.WriteString("  3. Type something.\n")
				b.WriteString(strings.Repeat("─", 50))
				b.WriteString("\n")
				b.WriteString("❯ 4. Chat about this\n")
				b.WriteString("\n")
				b.WriteString("Enter to select · ↑/↓ to navigate · Esc to cancel\n")
				return b.String()
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !HasQuestionPrompt([]byte(tt.data)) {
				clean := StripANSI([]byte(tt.data))
				tail := LastNLines(clean, 30)
				t.Errorf("should detect AskUserQuestion when the selection cursor sits on Chat about this\n  clean: %q\n  tail: %q",
					string(clean), string(tail))
			}
		})
	}
}

func TestHasQuestionPrompt_FooterInsideLongToolOutput(t *testing.T) {
	// Review #692: a long tool-output block whose ⎿ header sits more than 30
	// lines above the bottom leaves only indented continuation lines inside the
	// 30-line tail. If that pasted output ends with the AskUserQuestion footer
	// ("Chat about this" + "Enter to select … Esc to cancel"), stripping tool
	// output only AFTER tailing can no longer anchor on the (now absent) ⎿
	// header, so the orphaned continuation lines survive and Pattern 0b
	// false-fires. Stripping tool blocks from the full buffer before tailing
	// keeps the whole block out of the tail.
	var b strings.Builder
	b.WriteString("⏺ Here is the fixture I'm referencing.\n")
	b.WriteString("  ⎿  Read fixture.txt (38 lines)\n")
	for range 35 {
		b.WriteString("       a line of pasted fixture content\n")
	}
	b.WriteString("       Chat about this\n")
	b.WriteString("       Enter to select · ↑/↓ to navigate · Esc to cancel\n")
	data := b.String()
	if HasQuestionPrompt([]byte(data)) {
		clean := StripANSI([]byte(data))
		tail := LastNLines(clean, 30)
		t.Errorf("should NOT detect a question when the footer is inside a long tool-output block\n  tail: %q",
			string(tail))
	}
}

func TestHasWorkingIndicator(t *testing.T) {
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
			// The ticket's exact footer: a background shell keeps running while
			// the visible pane text is otherwise static.
			name: "one shell still running (ticket fixture)",
			data: "✻ Cooked for 48s · 1 shell still running\n",
			want: true,
		},
		{
			name: "multiple shells still running",
			data: "  ✽ Planning… · 3 shells still running\n",
			want: true,
		},
		{
			// Claude renders background work other than shells under the same
			// footer grammar. "monitor" is the noun that exposed the enumerated
			// `shells?` matcher as too narrow.
			name: "one monitor still running",
			data: "✻ Baked for 42m 54s · 1 monitor still running\n",
			want: true,
		},
		{
			name: "multiple monitors still running",
			data: "✻ Baked for 42m 54s · 2 monitors still running\n",
			want: true,
		},
		{
			// The reported fixture (BOS session 618377459a55a4f3, reported IDLE
			// for 42m). Two kinds of live background work join into ONE comma
			// list under a SHARED "still running" suffix, so nothing follows
			// "1 shell" — the old `[0-9]+ shells? still running` matcher missed
			// the shell it did know about, not just the monitor.
			name: "shell and monitor joined in one comma list (ticket fixture)",
			data: "✻ Baked for 42m 54s · 1 shell, 1 monitor still running\n",
			want: true,
		},
		{
			name: "plural shells and monitors joined in one comma list",
			data: "✻ Baked for 1m 2s · 2 shells, 3 monitors still running\n",
			want: true,
		},
		{
			name: "esc to interrupt spinner footer",
			data: "✻ Thinking… (esc to interrupt)\n",
			want: true,
		},
		{
			name: "working spinner with esc to interrupt",
			data: "· Working (3s · esc to interrupt)\n",
			want: true,
		},
		{
			name: "idle prompt with no marker",
			data: "❯ \n",
			want: false,
		},
		{
			name: "plain conversation text",
			data: "⏺ Done. The file has been updated.\n",
			want: false,
		},
		{
			// The AskUserQuestion footer says "Esc to cancel", not
			// "esc to interrupt" — it must not read as WORKING (QUESTION owns it).
			name: "AskUserQuestion Esc to cancel footer is not working",
			data: "  Chat about this\n\nEnter to select · ↑/↓ to navigate · Esc to cancel\n",
			want: false,
		},
		{
			// Prose mentioning shells without the exact "N shell(s) still
			// running" grammar must not false-positive.
			name: "prose about shells is not a working marker",
			data: "I ran the shell command and it finished.\n",
			want: false,
		},
		{
			// Claude's blocked-on-background-agents footer (singular).
			name: "one background agent to finish (ticket fixture)",
			data: "✻ Waiting for 1 background agent to finish\n",
			want: true,
		},
		{
			name: "two background agents to finish",
			data: "✻ Waiting for 2 background agents to finish\n",
			want: true,
		},
		{
			name: "three background agents to finish",
			data: "✻ Waiting for 3 background agents to finish\n",
			want: true,
		},
		{
			// Prose without the "N background agent(s) to finish" grammar must
			// not false-positive.
			name: "prose about a background job is not a working marker",
			data: "I was waiting for the background job.\n",
			want: false,
		},
		{
			// The footer phrase embedded mid-sentence in prose/tool output is NOT
			// the live footer (the real footer ends its line). The end-of-line
			// anchor must reject it so lingering narration cannot pin idle chats
			// as WORKING.
			name: "footer phrase embedded mid-sentence is not a working marker",
			data: "Still Waiting for 1 background agent to finish before I proceed.\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasWorkingIndicator([]byte(tt.data)); got != tt.want {
				t.Errorf("HasWorkingIndicator(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

// TestHasWorkingIndicator_StaleMarkerEvicted proves the detector reflects the
// CURRENT screen, not stale history. A "1 shell still running" footer from an
// earlier frame, followed by a full idle-screen re-render (the shell finished
// and Claude redrew the idle input box), must NOT report working. Without the
// current-screen scope the raw ring buffer -- and tmux scrollback -- still
// contains the old footer and would pin the pane WORKING until enough new
// output evicted it.
func TestHasWorkingIndicator_StaleMarkerEvicted(t *testing.T) {
	var b strings.Builder
	b.WriteString("✻ Cooked for 48s · 1 shell still running\n")
	// Fresh idle content re-rendered after the shell finished. This pushes the
	// stale footer above the current-screen window.
	for range 35 {
		b.WriteString("⏺ finished a step\n")
	}
	b.WriteString("╭──────────────╮\n")
	b.WriteString("│ ❯            │\n")
	b.WriteString("╰──────────────╯\n")
	if HasWorkingIndicator([]byte(b.String())) {
		t.Error("stale working marker above the current screen must not report working")
	}
}

// TestHasWorkingIndicator_CurrentScreenMarker guards the positive path: a marker
// rendered at the bottom of the current screen still reports working even when a
// lot of older output precedes it in the buffer.
func TestHasWorkingIndicator_CurrentScreenMarker(t *testing.T) {
	var b strings.Builder
	for range 60 {
		b.WriteString("⏺ older conversation line\n")
	}
	b.WriteString("✻ Cooked for 48s · 1 shell still running\n")
	if !HasWorkingIndicator([]byte(b.String())) {
		t.Error("current-screen working marker must report working")
	}
}

// TestHasWorkingIndicator_WaitingForBackgroundAgent_CurrentScreenMarker mirrors
// the reporter's realistic pane: the "✻ Waiting for 1 background agent to
// finish" footer, a task list, and a status bar whose git indicator line is
// "⏺ main". This guards that the trailing "⏺ main" glyph does NOT suppress this
// arm — unlike the shell footer, the background-agent footer is self-evicting
// and carries no freshness check, so a bare current-screen match is WORKING.
func TestHasWorkingIndicator_WaitingForBackgroundAgent_CurrentScreenMarker(t *testing.T) {
	pane := "" +
		"CI is now ALLGREEN (20/20 checks). Holding — the wc-auto-review subagent still owns the branch.\n" +
		"\n" +
		"✻ Waiting for 1 background agent to finish\n" +
		"\n" +
		"  6 tasks (4 done, 1 in progress, 1 open)\n" +
		"  ◼ Run wc-auto-review on PR #710\n" +
		"  ◻ Finalize: labels, proof decision, flip ready\n" +
		"  ✔ Check out won-1747 branch & rebase onto dev\n" +
		"  ✔ Fix Format (frontend) prettier failure\n" +
		"  ✔ Re-verify local gates (frontend lint/build/test)\n" +
		"   … +1 completed\n" +
		"\n" +
		"──────── WON-1747 implementation ──\n" +
		"❯ Waiting for CI to finish.\n" +
		"────────\n" +
		"  Opus 4.8 (1M context) | Context: 81% remaining | /Users/dave/Documents/Code/kamikai/wondercanvas-mono\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · PR #710 · ← for agents\n" +
		"\n" +
		"  ⏺ main\n"
	if !HasWorkingIndicator([]byte(pane)) {
		t.Error("a background-agent footer must report working even with a trailing ⏺ main status glyph")
	}
}

// TestHasWorkingIndicator_WaitingForBackgroundAgent_StaleMarkerEvicted mirrors
// TestHasWorkingIndicator_StaleMarkerEvicted for the background-agent footer: a
// "Waiting for 1 background agent to finish" line from an earlier frame followed
// by a full idle-screen re-render (the agent returned and Claude redrew the idle
// input box) must NOT report working — the current-screen scope evicts it.
func TestHasWorkingIndicator_WaitingForBackgroundAgent_StaleMarkerEvicted(t *testing.T) {
	var b strings.Builder
	b.WriteString("✻ Waiting for 1 background agent to finish\n")
	// Fresh idle content re-rendered after the agent returned. This pushes the
	// stale footer above the current-screen window.
	for range 35 {
		b.WriteString("⏺ finished a step\n")
	}
	b.WriteString("╭──────────────╮\n")
	b.WriteString("│ ❯            │\n")
	b.WriteString("╰──────────────╯\n")
	if HasWorkingIndicator([]byte(b.String())) {
		t.Error("stale background-agent marker above the current screen must not report working")
	}
}

// TestHasWorkingIndicator_CompletedTurnFooterNotLive reproduces a real stuck
// pane: a headless run finished, its background shells (leftover polling tasks)
// completed, and Claude went idle at its prompt. Each completed turn left a
// "N shell still running" summary footer frozen in the transcript, and because
// the run went idle right after, those footers survive inside the current-screen
// window — but a response marker (⏺ "Background command … completed") renders
// after them, proving the shell has since finished. Such a footer must NOT pin
// the chat WORKING; the pane is idle.
func TestHasWorkingIndicator_CompletedTurnFooterNotLive(t *testing.T) {
	pane := "" +
		"⏺ The run is complete: BOS-216 reached REVIEW_READY.\n" +
		"\n" +
		"✻ Baked for 3s · 1 shell still running\n" +
		"\n" +
		"⏺ Background command \"Wait and poll proof completion\" completed (exit code 0)\n" +
		"\n" +
		"⏺ The last leftover polling task has finished — no action needed.\n" +
		"\n" +
		"✻ Cogitated for 5s\n" +
		"※ recap: BOS-216 done; PR #1194 open, green, ready for review.\n" +
		"╭──────────────────────────────────────╮\n" +
		"│ ❯                                    │\n" +
		"╰──────────────────────────────────────╯\n"
	if HasWorkingIndicator([]byte(pane)) {
		t.Error("a completed-turn shell footer followed by later ⏺ output must not report working")
	}
}

// TestHasWorkingIndicator_LiveShellFooterBelowResponse guards the positive path
// for the same signal: while a background shell is genuinely running, the
// "N shell still running" footer is the bottom-most agent status line — no ⏺
// response marker follows it — so it must still report working even though an
// earlier ⏺ response sits above it.
func TestHasWorkingIndicator_LiveShellFooterBelowResponse(t *testing.T) {
	pane := "" +
		"⏺ Kicked off the proof run in the background.\n" +
		"\n" +
		"✻ Cooked for 48s · 1 shell still running\n"
	if !HasWorkingIndicator([]byte(pane)) {
		t.Error("a live shell footer with no later ⏺ output must report working")
	}
}

// TestHasWorkingIndicator_UsesLatestShellFooter distinguishes the latest live
// footer from an earlier, completed one. FindAllIndex must collect every match:
// limiting it to one would inspect only the stale first footer and report idle.
func TestHasWorkingIndicator_UsesLatestShellFooter(t *testing.T) {
	pane := "" +
		"✻ Cooked for 48s · 1 shell still running\n" +
		"⏺ Background command completed (exit code 0)\n" +
		"✻ Planning… · 2 shells still running\n"
	if !HasWorkingIndicator([]byte(pane)) {
		t.Error("a later live shell footer must override an earlier completed footer")
	}
}

// TestHasWorkingIndicator_CommaJoinedFooterStaleMarkerEvicted guards the
// freshness rule for the comma-joined footer shape. Widening the noun to match
// the list's LAST item moved the match's end offset, and lastFooterIsLive reads
// the region below that offset — so the eviction path needs its own coverage on
// this shape, not just on the single-noun one.
func TestHasWorkingIndicator_CommaJoinedFooterStaleMarkerEvicted(t *testing.T) {
	pane := "" +
		"✻ Baked for 42m 54s · 1 shell, 1 monitor still running\n" +
		"⏺ Background command \"make test\" completed (exit code 0)\n" +
		"╭──────────────────────────────────────╮\n" +
		"│ ❯                                    │\n" +
		"╰──────────────────────────────────────╯\n"
	if HasWorkingIndicator([]byte(pane)) {
		t.Error("a comma-joined footer followed by later ⏺ output must not report working")
	}
}

// TestHasWorkingIndicator_ProseCountIsNotEvictedByPrecedingMarker documents the
// cost of the generic noun: prose carrying the "N <thing> still running" shape
// pins the pane WORKING until a ⏺ marker follows it. That is the deliberate
// direction (see backgroundWorkRunningRe) — a busy chat misread as IDLE can be
// auto-prompted mid-work, whereas this only delays the idle flip.
func TestHasWorkingIndicator_ProseCountIsNotEvictedByPrecedingMarker(t *testing.T) {
	pane := "⏺ Heads up: there are 3 jobs still running on the box.\n"
	if !HasWorkingIndicator([]byte(pane)) {
		t.Error("generic-noun prose is expected to report working; if this now passes, the matcher was narrowed")
	}
}
