package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeJSONL(t *testing.T, path string, lines []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverChatsInDir_Empty(t *testing.T) {
	dir := t.TempDir()
	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 0 {
		t.Fatalf("expected 0 chats, got %d", len(chats))
	}
}

func TestDiscoverChatsInDir_NonExistent(t *testing.T) {
	chats, err := DiscoverChatsInDir("/tmp/nonexistent-claude-test-dir")
	if err != nil {
		t.Fatal(err)
	}
	if chats != nil {
		t.Fatalf("expected nil, got %v", chats)
	}
}

func TestDiscoverChatsInDir_BasicChat(t *testing.T) {
	dir := t.TempDir()

	writeJSONL(t, filepath.Join(dir, "abc-123.jsonl"), []map[string]any{
		{
			"type": "user",
			"slug": "happy-blue-fish",
			"message": map[string]any{
				"role":    "user",
				"content": "Fix the login bug",
			},
		},
	})

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}

	c := chats[0]
	if c.UUID != "abc-123" {
		t.Errorf("UUID = %q, want %q", c.UUID, "abc-123")
	}
	if c.Slug != "happy-blue-fish" {
		t.Errorf("Slug = %q, want %q", c.Slug, "happy-blue-fish")
	}
	if c.Summary != "Fix the login bug" {
		t.Errorf("Summary = %q, want %q", c.Summary, "Fix the login bug")
	}
}

func TestDiscoverChatsInDir_ContentArray(t *testing.T) {
	dir := t.TempDir()

	writeJSONL(t, filepath.Join(dir, "def-456.jsonl"), []map[string]any{
		{
			"type": "user",
			"slug": "cool-red-cat",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "Add dark mode support"},
				},
			},
		},
	})

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}
	if chats[0].Summary != "Add dark mode support" {
		t.Errorf("Summary = %q, want %q", chats[0].Summary, "Add dark mode support")
	}
}

func TestDiscoverChatsInDir_SkipsNoUserMessage(t *testing.T) {
	dir := t.TempDir()

	// File with only non-user messages.
	writeJSONL(t, filepath.Join(dir, "no-user.jsonl"), []map[string]any{
		{
			"type": "assistant",
			"slug": "quiet-green-dog",
			"message": map[string]any{
				"role":    "assistant",
				"content": "I can help with that.",
			},
		},
	})

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 0 {
		t.Fatalf("expected 0 chats (no user messages), got %d", len(chats))
	}
}

func TestDiscoverChatsInDir_SortsByMtime(t *testing.T) {
	dir := t.TempDir()

	// Create older file.
	olderPath := filepath.Join(dir, "older.jsonl")
	writeJSONL(t, olderPath, []map[string]any{
		{
			"type": "user",
			"slug": "old-slug",
			"message": map[string]any{
				"role":    "user",
				"content": "Old message",
			},
		},
	})
	oldTime := time.Now().Add(-2 * time.Hour)
	os.Chtimes(olderPath, oldTime, oldTime)

	// Create newer file.
	writeJSONL(t, filepath.Join(dir, "newer.jsonl"), []map[string]any{
		{
			"type": "user",
			"slug": "new-slug",
			"message": map[string]any{
				"role":    "user",
				"content": "New message",
			},
		},
	})

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 {
		t.Fatalf("expected 2 chats, got %d", len(chats))
	}
	if chats[0].UUID != "newer" {
		t.Errorf("first chat UUID = %q, want %q", chats[0].UUID, "newer")
	}
	if chats[1].UUID != "older" {
		t.Errorf("second chat UUID = %q, want %q", chats[1].UUID, "older")
	}
}

func TestDiscoverChatsInDir_TruncatesSummary(t *testing.T) {
	dir := t.TempDir()

	longMsg := "This is a very long message that exceeds the maximum summary length and should be truncated with ellipsis at the end"
	writeJSONL(t, filepath.Join(dir, "long.jsonl"), []map[string]any{
		{
			"type": "user",
			"slug": "long-slug",
			"message": map[string]any{
				"role":    "user",
				"content": longMsg,
			},
		},
	})

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}
	if len(chats[0].Summary) > maxSummaryLen {
		t.Errorf("summary length = %d, want <= %d", len(chats[0].Summary), maxSummaryLen)
	}
	if chats[0].Summary[len(chats[0].Summary)-3:] != "..." {
		t.Errorf("summary should end with '...', got %q", chats[0].Summary)
	}
}

func TestDiscoverChatsInDir_FirstLineOnly(t *testing.T) {
	dir := t.TempDir()

	writeJSONL(t, filepath.Join(dir, "multiline.jsonl"), []map[string]any{
		{
			"type": "user",
			"slug": "multi-slug",
			"message": map[string]any{
				"role":    "user",
				"content": "First line of message\nSecond line\nThird line",
			},
		},
	})

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}
	if chats[0].Summary != "First line of message" {
		t.Errorf("Summary = %q, want %q", chats[0].Summary, "First line of message")
	}
}

func TestDiscoverChatsInDir_SkipsNonJSONL(t *testing.T) {
	dir := t.TempDir()

	// Write a .txt file — should be ignored.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a chat"), 0644)

	// Write a proper JSONL file.
	writeJSONL(t, filepath.Join(dir, "real.jsonl"), []map[string]any{
		{
			"type": "user",
			"slug": "real-slug",
			"message": map[string]any{
				"role":    "user",
				"content": "Real chat",
			},
		},
	})

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}
	if chats[0].UUID != "real" {
		t.Errorf("UUID = %q, want %q", chats[0].UUID, "real")
	}
}

func TestDiscoverChatsInDir_SlugFromNonUserLine(t *testing.T) {
	dir := t.TempDir()

	// Slug appears on a progress line before the user message.
	writeJSONL(t, filepath.Join(dir, "slug-first.jsonl"), []map[string]any{
		{
			"type": "progress",
			"slug": "early-slug",
		},
		{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": "Hello there",
			},
		},
	})

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}
	if chats[0].Slug != "early-slug" {
		t.Errorf("Slug = %q, want %q", chats[0].Slug, "early-slug")
	}
	if chats[0].Summary != "Hello there" {
		t.Errorf("Summary = %q, want %q", chats[0].Summary, "Hello there")
	}
}

func TestPathToProjectKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Users/dave/foo", "-Users-dave-foo"},
		{"/Users/dave/Documents/Code/bossanova", "-Users-dave-Documents-Code-bossanova"},
		{"relative/path", "relative-path"},
	}
	for _, tt := range tests {
		got := pathToProjectKey(tt.input)
		if got != tt.want {
			t.Errorf("pathToProjectKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDiscoverChatsInDir_CapsAt20(t *testing.T) {
	dir := t.TempDir()

	// Create 25 JSONL files.
	for i := 0; i < 25; i++ {
		name := filepath.Join(dir, "chat-"+string(rune('a'+i))+".jsonl")
		writeJSONL(t, name, []map[string]any{
			{
				"type": "user",
				"slug": "slug",
				"message": map[string]any{
					"role":    "user",
					"content": "Message " + string(rune('a'+i)),
				},
			},
		})
	}

	chats, err := DiscoverChatsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != maxChats {
		t.Errorf("expected %d chats, got %d", maxChats, len(chats))
	}
}
