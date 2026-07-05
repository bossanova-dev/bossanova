package main

import (
	"context"
	"encoding/json"
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

func TestStripXMLTags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello world", "hello world"},
		{"empty", "", ""},
		{"single tag pair", "<thinking>plan</thinking>", "plan"},
		{"surrounding whitespace trimmed", "  <tag>x</tag>  ", "x"},
		{"self-closing tag", "before<br/>after", "beforeafter"},
		{"lone open tag", "<command-name>", ""},
		{"tag with attributes", `<a href="x">link</a>`, "link"},
		{"less-than not a tag", "a < b and c > d", "a < b and c > d"},
		{"unclosed angle bracket kept", "<unclosed", "<unclosed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripXMLTags(tt.in); got != tt.want {
				t.Errorf("stripXMLTags(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single line", "just one line", "just one line"},
		{"multi line keeps first", "first\nsecond\nthird", "first"},
		{"empty", "", ""},
		{"leading and trailing whitespace", "  padded  ", "padded"},
		{"leading newlines stripped", "\n\nfoo\nbar", "foo"},
		{"trailing newline", "solo\n", "solo"},
		{"whitespace before newline", "  line one  \n line two", "line one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine(tt.in); got != tt.want {
				t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	atLimit := strings.Repeat("a", maxSummaryLen)
	overLimit := strings.Repeat("b", maxSummaryLen+5)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short string unchanged", "short", "short"},
		{"empty unchanged", "", ""},
		{"exactly at limit unchanged", atLimit, atLimit},
		{"over limit truncated with ellipsis", overLimit, strings.Repeat("b", maxSummaryLen-3) + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.in)
			if got != tt.want {
				t.Errorf("truncate(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if len(got) > maxSummaryLen {
				t.Errorf("truncate output length %d exceeds maxSummaryLen %d", len(got), maxSummaryLen)
			}
		})
	}
}

func TestExtractText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain string content", `"Fix the bug"`, "Fix the bug"},
		{"empty raw", ``, ""},
		{"empty json string", `""`, ""},
		{"string strips xml and keeps first line", `"<thinking>x</thinking>do it\nignored"`, "xdo it"},
		{"array first text block", `[{"type":"text","text":"first block"},{"type":"text","text":"second"}]`, "first block"},
		{"array skips tool_result then text", `[{"type":"tool_result","text":"ignored"},{"type":"text","text":"real text"}]`, "real text"},
		{"array skips empty text block", `[{"type":"text","text":"   "},{"type":"text","text":"non-empty"}]`, "non-empty"},
		{"array all tool_result", `[{"type":"tool_result","text":"x"}]`, ""},
		{"invalid json", `{not json`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractText(json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("extractText(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestHasUserTextBlock(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"non-empty string", `"hello"`, true},
		{"whitespace-only string", `"   "`, false},
		{"empty raw", ``, false},
		{"empty json string", `""`, false},
		{"array with text block", `[{"type":"text"}]`, true},
		{"array with text among tool_results", `[{"type":"tool_result"},{"type":"text"}]`, true},
		{"array of only tool_results", `[{"type":"tool_result"}]`, false},
		{"empty array", `[]`, false},
		{"invalid json", `{broken`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasUserTextBlock(json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("hasUserTextBlock(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestPathToProjectKeyReplacesPathSeparators(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "posix path",
			path: "/Users/dave/Code/.worktrees/foo",
			want: "-Users-dave-Code--worktrees-foo",
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
			name: "posix path with trailing slash",
			path: "/Users/dave/Documents/Code/bossanova/",
			want: "-Users-dave-Documents-Code-bossanova",
		},
		{
			name: "posix path with redundant slashes",
			path: "/Users/dave//Code/foo",
			want: "-Users-dave-Code-foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathToProjectKey(tt.path)
			if got != tt.want {
				t.Fatalf("pathToProjectKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadTranscript_BasicMessages(t *testing.T) {
	work := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	key := pathToProjectKey(work)
	dir := filepath.Join(home, ".claude", "projects", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"user","message":{"role":"user","content":"Hello there"},"timestamp":"2024-01-15T10:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"Hi! How can I help?"},"timestamp":"2024-01-15T10:00:01Z"}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"Fix the bug"},"timestamp":"2024-01-15T10:00:02Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"Sure, I will fix it."},"timestamp":"2024-01-15T10:00:03Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sess.jsonl"), []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "sess.jsonl")
	messages, finalAssistant, exists, err := readTranscript(path, 0)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	if !exists {
		t.Error("exists = false, want true")
	}
	if len(messages) != 4 {
		t.Fatalf("len(messages) = %d, want 4", len(messages))
	}
	if messages[0].Role != "user" {
		t.Errorf("messages[0].Role = %q, want user", messages[0].Role)
	}
	if messages[0].Text != "Hello there" {
		t.Errorf("messages[0].Text = %q, want 'Hello there'", messages[0].Text)
	}
	if messages[0].Timestamp != "2024-01-15T10:00:00Z" {
		t.Errorf("messages[0].Timestamp = %q, want '2024-01-15T10:00:00Z'", messages[0].Timestamp)
	}
	if messages[0].Kind != "text" {
		t.Errorf("messages[0].Kind = %q, want 'text'", messages[0].Kind)
	}
	if messages[1].Role != "assistant" {
		t.Errorf("messages[1].Role = %q, want assistant", messages[1].Role)
	}
	if messages[3].Role != "assistant" {
		t.Errorf("messages[3].Role = %q, want assistant", messages[3].Role)
	}
	if finalAssistant != "Sure, I will fix it." {
		t.Errorf("finalAssistant = %q, want 'Sure, I will fix it.'", finalAssistant)
	}
}

func TestReadTranscript_NoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.jsonl")

	messages, finalAssistant, exists, err := readTranscript(path, 0)
	if err != nil {
		t.Fatalf("readTranscript: expected nil error for missing file, got %v", err)
	}
	if exists {
		t.Error("exists = true, want false")
	}
	if len(messages) != 0 {
		t.Errorf("len(messages) = %d, want 0", len(messages))
	}
	if finalAssistant != "" {
		t.Errorf("finalAssistant = %q, want empty", finalAssistant)
	}
}

func TestReadTranscript_MaxMessagesTail(t *testing.T) {
	dir := t.TempDir()
	jsonl := `{"type":"user","message":{"role":"user","content":"msg1"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"reply1"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"msg2"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"reply2"}}` + "\n"
	path := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	messages, finalAssistant, exists, err := readTranscript(path, 2)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	if !exists {
		t.Error("exists = false, want true")
	}
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2 (tail)", len(messages))
	}
	if messages[0].Text != "msg2" {
		t.Errorf("messages[0].Text = %q, want 'msg2'", messages[0].Text)
	}
	if messages[1].Text != "reply2" {
		t.Errorf("messages[1].Text = %q, want 'reply2'", messages[1].Text)
	}
	// FinalAssistantText should reflect the true last assistant turn (reply2).
	if finalAssistant != "reply2" {
		t.Errorf("finalAssistant = %q, want 'reply2'", finalAssistant)
	}
}

func TestReadTranscript_SkipsToolResultOnlyUserEntries(t *testing.T) {
	dir := t.TempDir()
	jsonl := `{"type":"user","message":{"role":"user","content":"hello"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"working on it"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"done"}}` + "\n"
	path := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	messages, finalAssistant, _, err := readTranscript(path, 0)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	// tool_result-only user entry should be skipped → 3 messages
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3 (tool_result-only user skipped)", len(messages))
	}
	if finalAssistant != "done" {
		t.Errorf("finalAssistant = %q, want 'done'", finalAssistant)
	}
}

func TestReadTranscript_ViaServer(t *testing.T) {
	work := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	key := pathToProjectKey(work)
	dir := filepath.Join(home, ".claude", "projects", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"user","message":{"role":"user","content":"help me"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"I can help"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "mysess.jsonl"), []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := &Server{logger: zerolog.Nop()}
	resp, err := srv.ReadTranscript(context.Background(), &bossanovav1.ReadTranscriptRequest{
		WorkDir:        work,
		AgentSessionId: "mysess",
		MaxMessages:    0,
	})
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if !resp.Exists {
		t.Error("Exists = false, want true")
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(resp.Messages))
	}
	if resp.FinalAssistantText != "I can help" {
		t.Errorf("FinalAssistantText = %q, want 'I can help'", resp.FinalAssistantText)
	}
}

func TestReadTranscript_ViaServer_MissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := &Server{logger: zerolog.Nop()}
	resp, err := srv.ReadTranscript(context.Background(), &bossanovav1.ReadTranscriptRequest{
		WorkDir:        t.TempDir(),
		AgentSessionId: "noexist",
	})
	if err != nil {
		t.Fatalf("ReadTranscript should not error on missing file, got: %v", err)
	}
	if resp.Exists {
		t.Error("Exists should be false for missing transcript")
	}
	if len(resp.Messages) != 0 {
		t.Errorf("Messages should be empty for missing transcript, got %d", len(resp.Messages))
	}
	if resp.FinalAssistantText != "" {
		t.Errorf("FinalAssistantText should be empty, got %q", resp.FinalAssistantText)
	}
}
