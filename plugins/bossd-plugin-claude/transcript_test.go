package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

func TestGetChatTitle_FromJSONL(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	worktree := "/Users/dave/Code/myproj"
	claudeID := "abcd-1234"

	projectKey := strings.NewReplacer("/", "-", ".", "-").Replace(worktree)
	projectDir := filepath.Join(tmpHome, ".claude", "projects", projectKey)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"user","message":{"role":"user","content":"Fix the bug in foo.go"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, claudeID+".jsonl"), []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := &Server{logger: zerolog.Nop()}
	resp, err := srv.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir: worktree, SessionId: claudeID,
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if !resp.Supported {
		t.Error("Supported = false")
	}
	if resp.Title != "Fix the bug in foo.go" {
		t.Errorf("Title = %q, want %q", resp.Title, "Fix the bug in foo.go")
	}
}

func TestGetChatTitle_MissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := &Server{logger: zerolog.Nop()}
	resp, err := srv.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir: "/nope", SessionId: "missing",
	})
	if err != nil {
		t.Fatalf("GetChatTitle should not error on missing file, got: %v", err)
	}
	if !resp.Supported {
		t.Error("Supported should be true even when title is empty")
	}
	if resp.Title != "" {
		t.Errorf("Title = %q, want empty", resp.Title)
	}
}

func TestLastTurnIsUserReadsRealTranscript(t *testing.T) {
	dir := t.TempDir()
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"hello"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"again"}}` + "\n"
	path := filepath.Join(dir, "abc.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	if !lastTurnIsUser(path) {
		t.Error("expected last turn to be user")
	}
}

func TestLastTurnIsUserReturnsFalseWhenAssistantLast(t *testing.T) {
	dir := t.TempDir()
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"hello"}}` + "\n"
	path := filepath.Join(dir, "x.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}
	if lastTurnIsUser(path) {
		t.Error("expected false when assistant ends transcript")
	}
}

func TestLastTurnIsUserSkipsToolResultEntries(t *testing.T) {
	dir := t.TempDir()
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"using tool"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}` + "\n"
	path := filepath.Join(dir, "x.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}
	if lastTurnIsUser(path) {
		t.Error("expected false when last user entry is tool_result-only")
	}
}

func TestTranscriptExistsReturnsTrueForRealFile(t *testing.T) {
	work := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	key := pathToProjectKey(work)
	dir := filepath.Join(home, ".claude", "projects", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sess.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !transcriptExists(work, "sess") {
		t.Error("expected transcriptExists=true")
	}
}

func TestTranscriptExistsReturnsFalseForMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if transcriptExists(t.TempDir(), "missing") {
		t.Error("expected transcriptExists=false")
	}
}
