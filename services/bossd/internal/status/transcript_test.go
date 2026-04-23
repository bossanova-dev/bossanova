package status

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathToProjectKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/Users/dave/Code/bossanova", "-Users-dave-Code-bossanova"},
		{"/Users/dave/.worktrees/foo", "-Users-dave--worktrees-foo"},
		{"/tmp/dir.with.dots/x", "-tmp-dir-with-dots-x"},
	}
	for _, tc := range tests {
		if got := pathToProjectKey(tc.in); got != tc.want {
			t.Errorf("pathToProjectKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLastTurnIsUser(t *testing.T) {
	// Each fixture is a JSONL blob. One JSON object per non-empty line.
	// Keep these minimal but true to the real Claude Code transcript format
	// (type, message.role, message.content as string or array of blocks).
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "ends with assistant text",
			content: `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello there"}]}}
`,
			want: false,
		},
		{
			name: "ends with user string content",
			content: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"What now?"}]}}
{"type":"user","message":{"role":"user","content":"do the thing"}}
`,
			want: true,
		},
		{
			name: "ends with user array text block",
			content: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"?"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"yes"}]}}
`,
			want: true,
		},
		{
			name: "tool_result after assistant does not count as user turn",
			content: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"running tool"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"done"}]}}
`,
			want: false,
		},
		{
			name: "user text after interleaved tool use",
			content: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"thinking"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"ok"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"question?"}]}}
{"type":"user","message":{"role":"user","content":"my answer"}}
`,
			want: true,
		},
		{
			name:    "empty file",
			content: "",
			want:    false,
		},
		{
			name: "malformed last line falls back to previous",
			content: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"finished"}]}}
{not valid json
`,
			want: false,
		},
		{
			name: "mixed content blocks with text wins over tool_result",
			content: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"?"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"},{"type":"text","text":"and also this"}]}}
`,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "transcript.jsonl")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			got := lastTurnIsUser(path)
			if got != tc.want {
				t.Errorf("lastTurnIsUser = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLastTurnIsUser_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if got := lastTurnIsUser(filepath.Join(dir, "nope.jsonl")); got != false {
		t.Errorf("lastTurnIsUser(missing) = %v, want false", got)
	}
}

func TestLastTurnIsUser_TailWindow(t *testing.T) {
	// Build a file where the last meaningful turn is near the end but the
	// file overall exceeds transcriptTailSize — confirms we still see it.
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")

	// Pad with lots of tool_result noise...
	var b strings.Builder
	tool := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}]}}` + "\n"
	for b.Len() < transcriptTailSize*2 {
		b.WriteString(tool)
	}
	// ...then an assistant question followed by a user answer at the very end.
	b.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"?"}]}}` + "\n")
	b.WriteString(`{"type":"user","message":{"role":"user","content":"yes"}}` + "\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := lastTurnIsUser(path); got != true {
		t.Errorf("lastTurnIsUser(large tail, user answer at end) = %v, want true", got)
	}
}

func TestLastTurnIsUser_TailMissesTurnOutsideWindow(t *testing.T) {
	// File where the only meaningful turn is at the very start, far outside
	// the tail window. Documented fail-open behavior: returns false.
	dir := t.TempDir()
	path := filepath.Join(dir, "head_only.jsonl")

	var b strings.Builder
	b.WriteString(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n")
	tool := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"` + strings.Repeat("x", 120) + `"}]}}` + "\n"
	for b.Len() < transcriptTailSize*2 {
		b.WriteString(tool)
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := lastTurnIsUser(path); got != false {
		t.Errorf("lastTurnIsUser(turn outside tail) = %v, want false (fail-open)", got)
	}
}
