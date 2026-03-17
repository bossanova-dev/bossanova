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
