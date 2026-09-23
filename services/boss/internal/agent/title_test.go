package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPathToProjectKey(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "simple path",
			path: "/Users/dave/foo",
			want: "-Users-dave-foo",
		},
		{
			name: "dotfile directory",
			path: "/Users/dave/Code/.worktrees/boss/my-branch",
			want: "-Users-dave-Code--worktrees-boss-my-branch",
		},
		{
			name: "multiple dots in path",
			path: "/Users/dave/.config/.local/share",
			want: "-Users-dave--config--local-share",
		},
		{
			name: "dot in filename",
			path: "/Users/dave/my.project/src",
			want: "-Users-dave-my-project-src",
		},
		{
			name: "real worktree path",
			path: "/Users/dave/Code/.worktrees/boss/blk-894-intelligent-home-screen-show-selection",
			want: "-Users-dave-Code--worktrees-boss-blk-894-intelligent-home-screen-show-selection",
		},
		{
			name: "no dots",
			path: "/Users/dave/Documents/Code/bossanova",
			want: "-Users-dave-Documents-Code-bossanova",
		},
		{
			name: "windows path",
			path: `C:\Users\dave\Code\.worktrees\foo`,
			want: "C--Users-dave-Code--worktrees-foo",
		},
		{
			// Regression: a repo registered with a trailing slash must encode
			// to the same key as its clean form, or --resume silently breaks
			// (Claude keys off the normalized getcwd, which has no trailing /).
			name: "trailing slash",
			path: "/Users/dave/Documents/Code/bossanova/",
			want: "-Users-dave-Documents-Code-bossanova",
		},
		{
			name: "redundant slashes",
			path: "/Users/dave//Code/foo",
			want: "-Users-dave-Code-foo",
		},
		{
			// Regression (the "Session ID already in use" bug): Claude folds "_"
			// to "-", so a worktree containing an underscore must too, or
			// TranscriptExists misses the file and the attach picks --session-id
			// (create) over --resume and collides with the existing transcript.
			name: "underscore in path",
			path: "/Users/dave/.bossanova/worktrees/bossanova/dave/bos-465-harden-bossd-rlimit_nofile-so-dev-mode",
			want: "-Users-dave--bossanova-worktrees-bossanova-dave-bos-465-harden-bossd-rlimit-nofile-so-dev-mode",
		},
		{
			name: "assorted non-alphanumerics all fold to dash",
			path: "/Users/dave/Code/a_b c@d~e",
			want: "-Users-dave-Code-a-b-c-d-e",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PathToProjectKey(tt.path)
			if got != tt.want {
				t.Errorf("PathToProjectKey(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestChatTitleInDir_StringContent(t *testing.T) {
	dir := t.TempDir()
	id := "test-session-id"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{"type": "file-history-snapshot"},
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "Fix the login bug"},
		},
		map[string]any{
			"type":    "assistant",
			"message": map[string]any{"role": "assistant", "content": "I'll fix that."},
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "Fix the login bug" {
		t.Errorf("got %q, want %q", got, "Fix the login bug")
	}
}

func TestChatTitleInDir_BlockContent(t *testing.T) {
	dir := t.TempDir()
	id := "block-content-session"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "Implement dark mode"},
				},
			},
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "Implement dark mode" {
		t.Errorf("got %q, want %q", got, "Implement dark mode")
	}
}

func TestChatTitleInDir_MultilineFirstMessage(t *testing.T) {
	dir := t.TempDir()
	id := "multiline-session"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": "First line of prompt\nSecond line with details\nThird line",
			},
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "First line of prompt" {
		t.Errorf("got %q, want %q", got, "First line of prompt")
	}
}

func TestChatTitleInDir_Truncation(t *testing.T) {
	dir := t.TempDir()
	id := "long-message-session"
	longMsg := strings.Repeat("x", 100)
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": longMsg},
		},
	)

	got := chatTitleInDir(dir, id)
	if len(got) != maxSummaryLen {
		t.Errorf("length = %d, want %d", len(got), maxSummaryLen)
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Errorf("got %q, want suffix '…'", got)
	}
}

func TestChatTitleInDir_NoUserMessage(t *testing.T) {
	dir := t.TempDir()
	id := "no-user-session"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{"type": "file-history-snapshot"},
		map[string]any{"type": "progress"},
	)

	got := chatTitleInDir(dir, id)
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestChatTitleInDir_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got := chatTitleInDir(dir, "nonexistent")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestChatTitleInDir_EmptyContent(t *testing.T) {
	dir := t.TempDir()
	id := "empty-content-session"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": ""},
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestChatTitleInDir_SkipsMetaCaveatLeadingLine(t *testing.T) {
	dir := t.TempDir()
	id := "caveat-session"
	// The first type:"user" line is Claude Code's synthetic caveat, flagged
	// with top-level isMeta:true. It must be skipped so the real user turn wins.
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "<local-command-caveat>Caveat: The messages below were generated by the user while running local commands. DO NOT respond to these messages or otherwise consider them in your response unless the user explicitly asks you to.</local-command-caveat>"},
			"isMeta":  true,
		},
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "Fix the login bug"},
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "Fix the login bug" {
		t.Errorf("got %q, want the real user message", got)
	}
	if strings.Contains(got, "Caveat: The messages below") {
		t.Errorf("got %q, must not contain the caveat text", got)
	}
}

func TestChatTitleInDir_AllMetaYieldsEmpty(t *testing.T) {
	dir := t.TempDir()
	id := "all-meta-session"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "<local-command-caveat>Caveat: The messages below were generated by the user while running local commands.</local-command-caveat>"},
			"isMeta":  true,
		},
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "another meta line"},
			"isMeta":  true,
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "" {
		t.Errorf("got %q, want empty string for all-meta window", got)
	}
}

func TestChatTitleInDir_MetaAfterRealFirstLineDoesNotDisplace(t *testing.T) {
	dir := t.TempDir()
	id := "meta-after-real-session"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "Fix the login bug"},
		},
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "<local-command-caveat>Caveat: The messages below were generated by the user while running local commands.</local-command-caveat>"},
			"isMeta":  true,
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "Fix the login bug" {
		t.Errorf("got %q, want the real first message to win", got)
	}
}

func TestChatTitleInDir_SkipsNonUserLines(t *testing.T) {
	dir := t.TempDir()
	id := "mixed-session"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{"type": "file-history-snapshot"},
		map[string]any{"type": "progress"},
		map[string]any{
			"type":    "assistant",
			"message": map[string]any{"role": "assistant", "content": "Hello!"},
		},
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "Add unit tests"},
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "Add unit tests" {
		t.Errorf("got %q, want %q", got, "Add unit tests")
	}
}

func TestChatTitleInDir_BlockContentSkipsNonText(t *testing.T) {
	dir := t.TempDir()
	id := "block-skip-session"
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "image", "source": "data:..."},
					{"type": "text", "text": "What is this image?"},
				},
			},
		},
	)

	got := chatTitleInDir(dir, id)
	if got != "What is this image?" {
		t.Errorf("got %q, want %q", got, "What is this image?")
	}
}

func TestChatTitleInDir_XMLTagsStripped(t *testing.T) {
	tests := []struct {
		name    string
		content any // string or []map[string]any for block content
		want    string
	}{
		{
			name:    "command-message tags",
			content: "<command-message>take-off</command-message>",
			want:    "take-off",
		},
		{
			name:    "mixed markup and text",
			content: "<foo>text</foo> more text",
			want:    "text more text",
		},
		{
			name:    "plain text unchanged",
			content: "no tags here",
			want:    "no tags here",
		},
		{
			name:    "nested tags",
			content: "<a><b>inner</b></a>",
			want:    "inner",
		},
		{
			name:    "self-closing tag",
			content: "<br/> hello",
			want:    "hello",
		},
		{
			name:    "block content with tags",
			content: []map[string]any{{"type": "text", "text": "<command-message>take-off</command-message>"}},
			want:    "take-off",
		},
		{
			name:    "markup-only block skipped for next block",
			content: []map[string]any{{"type": "text", "text": "<metadata/>"}, {"type": "text", "text": "real title"}},
			want:    "real title",
		},
		{
			name:    "angle brackets in comparisons preserved",
			content: "check if x < 10 and y > 5",
			want:    "check if x < 10 and y > 5",
		},
		{
			name:    "component name in angle brackets stripped",
			content: "fix the <Header> component",
			want:    "fix the  component",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			id := "xml-" + tt.name
			writeJSONL(t, filepath.Join(dir, id+".jsonl"),
				map[string]any{
					"type":    "user",
					"message": map[string]any{"role": "user", "content": tt.content},
				},
			)

			got := chatTitleInDir(dir, id)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChatTitleInDir_ExactlyMaxScanLines(t *testing.T) {
	// Tests boundary: user message at exactly line 50 (maxScanLines).
	// Catches mutation: i < maxScanLines changed to i <= maxScanLines.
	dir := t.TempDir()
	id := "boundary-session"

	// Create exactly maxScanLines (50) non-user lines, then user message at line 51.
	lines := make([]any, 0, maxScanLines+1)
	for i := 0; i < maxScanLines; i++ {
		lines = append(lines, map[string]any{"type": "progress"})
	}
	lines = append(lines, map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "Message at line 51"},
	})
	writeJSONL(t, filepath.Join(dir, id+".jsonl"), lines...)

	got := chatTitleInDir(dir, id)
	if got != "" {
		t.Errorf("got %q, want empty (should stop at line 50)", got)
	}
}

func TestChatTitleInDir_JustBeforeMaxScanLines(t *testing.T) {
	// Tests boundary: user message at line 49 (before maxScanLines).
	dir := t.TempDir()
	id := "before-boundary-session"

	lines := make([]any, 0, maxScanLines)
	for i := 0; i < maxScanLines-2; i++ {
		lines = append(lines, map[string]any{"type": "progress"})
	}
	lines = append(lines, map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "Message at line 49"},
	})
	writeJSONL(t, filepath.Join(dir, id+".jsonl"), lines...)

	got := chatTitleInDir(dir, id)
	if got != "Message at line 49" {
		t.Errorf("got %q, want %q", got, "Message at line 49")
	}
}

func TestFirstLine_NewlineAtStart(t *testing.T) {
	// Tests that we correctly handle strings with newlines.
	// Catches mutation: idx >= 0 changed to idx > 0.
	// While idx can't be 0 after TrimSpace (it removes leading newlines),
	// we test that we DO truncate when newlines are present.
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single line no newline",
			input: "single line",
			want:  "single line",
		},
		{
			name:  "first line with newline",
			input: "first\nsecond",
			want:  "first",
		},
		{
			name:  "empty first line",
			input: "\nsecond",
			want:  "second", // TrimSpace removes leading \n
		},
		{
			name:  "multiple newlines",
			input: "first\nsecond\nthird",
			want:  "first",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstLine(tt.input)
			if got != tt.want {
				t.Errorf("firstLine(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncate_ExactlyMaxLength(t *testing.T) {
	// Tests boundary: string exactly at maxSummaryLen (80).
	// Catches mutation: len(s) <= maxSummaryLen changed to len(s) < maxSummaryLen.
	s := strings.Repeat("x", maxSummaryLen)
	got := truncate(s)
	if got != s {
		t.Errorf("truncate() should not modify string of exactly maxSummaryLen")
	}
	if len(got) != maxSummaryLen {
		t.Errorf("length = %d, want %d", len(got), maxSummaryLen)
	}
}

func TestTruncate_BelowMaxLength(t *testing.T) {
	// A shorter title must not be expanded and suffixed with an ellipsis.
	// Catches the CONDITIONALS_BOUNDARY mutation of <= to >=.
	s := strings.Repeat("x", maxSummaryLen-1)
	if got := truncate(s); got != s {
		t.Errorf("truncate() = %q, want unchanged %q", got, s)
	}
}

func TestTruncate_OneOverMaxLength(t *testing.T) {
	// Tests boundary: string one character over maxSummaryLen.
	s := strings.Repeat("x", maxSummaryLen+1)
	got := truncate(s)
	if len(got) != maxSummaryLen {
		t.Errorf("length = %d, want %d", len(got), maxSummaryLen)
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Errorf("got %q, want suffix '…'", got)
	}
	// Should be maxSummaryLen-3 x's plus ellipsis
	want := strings.Repeat("x", maxSummaryLen-3) + ellipsis
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseSessionMeta_LargeJSONLine(t *testing.T) {
	// Tests that scanner buffer handles large lines correctly.
	// Catches mutation: titleScanMaxBytes arithmetic changed so the effective
	// ceiling drops below this line's size (8*1024*1024 -> 8+1024*1024, etc).
	dir := t.TempDir()
	id := "large-line-session"

	// Create a JSONL line larger than the default scanner buffer (64KB) and
	// larger than titleScanInitialBytes, so bufio has to GROW to read it —
	// which is the half of the split contract this test exercises.
	largeContent := strings.Repeat("x", 128*1024)
	writeJSONL(t, filepath.Join(dir, id+".jsonl"),
		map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": largeContent},
		},
	)

	got := chatTitleInDir(dir, id)
	// Should successfully read and truncate to maxSummaryLen
	if len(got) != maxSummaryLen {
		t.Errorf("length = %d, want %d (should successfully scan large line)", len(got), maxSummaryLen)
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Errorf("should truncate large content with '…'")
	}
}

func TestParseSessionMeta_BufferCapacityBoundary(t *testing.T) {
	// Pins the scanner buffer capacity contract in parseSessionMeta:
	//   scanner.Buffer(make([]byte, titleScanInitialBytes), titleScanMaxBytes)
	// The effective max token size must be titleScanMaxBytes.
	//
	// BOS-1281 moved this contract. It used to assert the EXACT 262144 product
	// of a single 256*1024 passed for BOTH the initial buffer and the ceiling —
	// and pinning that was what made the defect a contract: a Claude session
	// whose first line carried an inlined tool result (~742 KiB observed) came
	// back title-less. The two values are now separate constants, so the test
	// asserts against the CEILING and no longer against the allocation.
	//
	// A JSONL line whose total length sits just under the ceiling must scan
	// successfully; one over it must be dropped. Asserting both directions is
	// what keeps this a bound rather than an absent limit: an arithmetic swap
	// that lowers the effective ceiling (8*1024*1024 -> 8+1024*1024, say) drops
	// the just-under line and the title comes back empty.
	//
	// NOTE: Go's bufio.Scanner uses max(len(initialBuffer), maxArg) as the
	// effective cap, so a mutant that only shrinks the ceiling BELOW
	// titleScanInitialBytes is masked by the allocation. That is unchanged from
	// the pre-BOS-1281 version of this note and is not killable by a black-box
	// test; the split is precisely why the allocation is now the small number.
	const ceiling = titleScanMaxBytes

	t.Run("just under the ceiling scans", func(t *testing.T) {
		dir := t.TempDir()
		id := "under-ceiling"
		// Encoded line = {"type":"user","message":{"role":"user","content":"<xs>"}}\n
		// Keep the total under the ceiling while still far above BOTH the 64KB
		// bufio default AND the old 262144 cap this test used to pin — so this
		// arm alone fails against the pre-BOS-1281 shape.
		content := strings.Repeat("x", ceiling-1024)
		writeJSONL(t, filepath.Join(dir, id+".jsonl"),
			map[string]any{
				"type":    "user",
				"message": map[string]any{"role": "user", "content": content},
			},
		)
		got := chatTitleInDir(dir, id)
		if len(got) != maxSummaryLen {
			t.Errorf("len=%d, want %d (line ~%d bytes must scan within the %d ceiling)",
				len(got), maxSummaryLen, len(content), ceiling)
		}
		if !strings.HasSuffix(got, ellipsis) {
			t.Errorf("got %q, want truncated content with '…'", got)
		}
	})

	t.Run("over the ceiling dropped", func(t *testing.T) {
		dir := t.TempDir()
		id := "over-ceiling"
		// Encoded line exceeds the ceiling -> scanner returns token-too-long,
		// the line is skipped, and no title is produced. parseSessionMeta has no
		// error channel to classify into, unlike the runner plugin's
		// readTranscript; an empty title is the whole observable answer here.
		content := strings.Repeat("x", ceiling+1024)
		writeJSONL(t, filepath.Join(dir, id+".jsonl"),
			map[string]any{
				"type":    "user",
				"message": map[string]any{"role": "user", "content": content},
			},
		)
		got := chatTitleInDir(dir, id)
		if got != "" {
			t.Errorf("got %q, want empty (line over the %d-byte ceiling must be dropped)", got, ceiling)
		}
	})
}

func TestFirstLine_BoundaryNewlinePositions(t *testing.T) {
	// Guards the boundary check on title.go:124:
	//   if idx := strings.IndexByte(s, '\n'); idx >= 0 { s = s[:idx] }
	//
	// CONDITIONALS_BOUNDARY would swap `idx >= 0` for `idx > 0`. The only
	// value that distinguishes them is idx == 0 (a leading newline). Because
	// firstLine calls strings.TrimSpace(s) first, a leading newline is always
	// stripped, so idx can never be 0 in this function -- making the >= / >
	// mutant equivalent. These cases pin the surrounding behavior: a newline
	// at position >0 truncates, and no newline leaves the string intact.
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "no newline", input: "only line", want: "only line"},
		{name: "newline at position 1", input: "a\nb", want: "a"},
		{name: "newline immediately after trimmed start", input: "  z\nrest", want: "z"},
		{name: "trailing newline only", input: "tail\n", want: "tail"},
		{name: "leading newline is trimmed not truncated", input: "\nkept", want: "kept"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine(tt.input); got != tt.want {
				t.Errorf("firstLine(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseSessionMeta_LoopIncrement(t *testing.T) {
	// Tests that the loop counter increments forward (i++), not backward (i--).
	// Catches mutation: i++ changed to i--.
	// If the loop decremented, it would never terminate or behave incorrectly.
	dir := t.TempDir()
	id := "loop-increment-session"

	// Create exactly 10 non-user lines followed by a user message at line 11.
	lines := make([]any, 0, 11)
	for i := 0; i < 10; i++ {
		lines = append(lines, map[string]any{"type": "progress"})
	}
	lines = append(lines, map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "Message at line 11"},
	})
	writeJSONL(t, filepath.Join(dir, id+".jsonl"), lines...)

	got := chatTitleInDir(dir, id)
	// Should find the user message because we increment forward through lines
	if got != "Message at line 11" {
		t.Errorf("got %q, want %q (loop should increment forward)", got, "Message at line 11")
	}
}

func TestTranscriptAbsentOrEmptyInDir(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-3333-4444-555555555555"
	path := filepath.Join(dir, id+".jsonl")

	// Absent transcript → genuine orphan, safe to reap.
	if !transcriptAbsentOrEmptyInDir(dir, id) {
		t.Errorf("absent transcript: got false, want true")
	}

	// Zero-length transcript → orphan, safe to reap.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !transcriptAbsentOrEmptyInDir(dir, id) {
		t.Errorf("empty transcript: got false, want true")
	}

	// Non-empty transcript → MUST NOT be reaped. This is the data-loss
	// regression: a chat with real history must never be auto-deleted.
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if transcriptAbsentOrEmptyInDir(dir, id) {
		t.Errorf("non-empty transcript: got true, want false — history must never be reaped")
	}

	// A directory at the transcript path (ambiguous, not a plain empty file)
	// → MUST NOT be reaped.
	id2 := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := os.Mkdir(filepath.Join(dir, id2+".jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if transcriptAbsentOrEmptyInDir(dir, id2) {
		t.Errorf("directory at path: got true, want false — ambiguity must never delete")
	}

	// An unreadable parent dir yields a stat error that is NOT not-exist →
	// MUST NOT be reaped (a read failure must never delete). Root bypasses
	// permission bits, so skip there.
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(locked, 0o755) }()
		if transcriptAbsentOrEmptyInDir(locked, id) {
			t.Errorf("unreadable parent dir: got true, want false — a read failure must never delete")
		}
	}
}

// writeJSONL writes multiple JSON objects as a JSONL file.
func writeJSONL(t *testing.T, path string, lines ...any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTruncate_CutFallsInsideMultiByteRune pins the byte-budget contract: the
// 80-byte budget is denominated in bytes and the ellipsis marker is 3 bytes in
// UTF-8, so the reservation is unchanged — but a fixed byte offset can land
// inside a multi-byte rune, which used to emit invalid UTF-8. The cut backs off
// to a rune boundary instead.
func TestTruncate_CutFallsInsideMultiByteRune(t *testing.T) {
	// 76 ASCII bytes then 3-byte runes, so byte maxSummaryLen-3 (77) is a
	// continuation byte in the middle of the first multi-byte rune.
	in := strings.Repeat("a", 76) + strings.Repeat("あ", 3)
	if len(in) <= maxSummaryLen {
		t.Fatalf("fixture is %d bytes, need more than maxSummaryLen (%d) to truncate", len(in), maxSummaryLen)
	}
	if utf8.RuneStart(in[maxSummaryLen-len(ellipsis)]) {
		t.Fatalf("fixture does not exercise the bug: byte %d is already a rune boundary", maxSummaryLen-len(ellipsis))
	}

	got := truncate(in)

	if !utf8.ValidString(got) {
		t.Errorf("truncate(...) = %q, which is not valid UTF-8 — the cut split a rune", got)
	}
	if len(got) > maxSummaryLen {
		t.Errorf("truncate(...) = %q, %d bytes, want at most maxSummaryLen (%d)", got, len(got), maxSummaryLen)
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Errorf("truncate(...) = %q, want it to end in the ellipsis marker %q", got, ellipsis)
	}
	if want := strings.Repeat("a", 76) + ellipsis; got != want {
		t.Errorf("truncate(...) = %q, want %q", got, want)
	}
}

// TestChatTitleInDir_OversizedFirstLine pins the split initial/ceiling shape in
// the session-title parser (BOS-1281). Scanning only the first maxScanLines is
// not protection: an oversized FIRST line kills the read before any title can
// be found, which is what the old 256 KiB cap did to a session opening with a
// large pasted prompt.
func TestChatTitleInDir_OversizedFirstLine(t *testing.T) {
	const oldCap = 256 * 1024
	dir := t.TempDir()

	// A first line well over the old cap, carrying the title the parser must
	// still reach, followed by an ordinary second line.
	padded := "Fix the parser " + strings.Repeat("x", 742*1024)
	first, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": padded},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) <= oldCap {
		t.Fatalf("fixture first line is %d bytes, which does not exceed the old %d-byte cap", len(first), oldCap)
	}
	second, err := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"role": "assistant", "content": "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sess.jsonl")
	if err := os.WriteFile(path, append(append(first, '\n'), append(second, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}

	got := chatTitleInDir(dir, "sess")
	if got == "" {
		t.Fatal("chatTitleInDir = \"\" on an oversized first line; the scanner refused the transcript")
	}
	if !strings.HasPrefix(got, "Fix the parser") {
		t.Fatalf("chatTitleInDir = %q, want it to start with the first user message", got)
	}
}
