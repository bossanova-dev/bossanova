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
