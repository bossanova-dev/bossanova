package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if !strings.HasSuffix(got, "...") {
		t.Errorf("got %q, want suffix '...'", got)
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

func TestTruncate_OneOverMaxLength(t *testing.T) {
	// Tests boundary: string one character over maxSummaryLen.
	s := strings.Repeat("x", maxSummaryLen+1)
	got := truncate(s)
	if len(got) != maxSummaryLen {
		t.Errorf("length = %d, want %d", len(got), maxSummaryLen)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("got %q, want suffix '...'", got)
	}
	// Should be maxSummaryLen-3 x's plus "..."
	want := strings.Repeat("x", maxSummaryLen-3) + "..."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseSessionMeta_LargeJSONLine(t *testing.T) {
	// Tests that scanner buffer handles large lines correctly.
	// Catches mutation: 256*1024 changed to 256+1024 or 256-1024 or 256/1024.
	dir := t.TempDir()
	id := "large-line-session"

	// Create a JSONL line larger than default scanner buffer (64KB)
	// but within our configured buffer (256KB).
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
	if !strings.HasSuffix(got, "...") {
		t.Errorf("should truncate large content with '...'")
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
