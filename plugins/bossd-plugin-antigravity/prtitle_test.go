package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func streamResult(response string) string {
	data, _ := json.Marshal(map[string]any{"event": "result", "result": map[string]any{"status": "SUCCESS", "response": response}})
	return string(data)
}

func TestSuggestPRTitle(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		supported bool
		want      string
	}{
		{"plain title", "Add Antigravity runner\n", true, "Add Antigravity runner"},
		{"quoted title", "\"Add Antigravity runner\"", true, "Add Antigravity runner"},
		{"commentary instead of a title", "The current title accurately describes the change.", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("BOSS_TEST_OUT", dir)
			writeFile(t, filepath.Join(dir, "result"), initLine(conversationA)+"\n"+streamResult(tt.response)+"\n")
			script := `printf '%s\n' "$@" > "$BOSS_TEST_OUT/argv"; cat > "$BOSS_TEST_OUT/stdin"; cat "$BOSS_TEST_OUT/result"`
			s, _ := newTestServer(t, WithCommandFactory(fakeAgy(script)))

			resp, err := s.SuggestPRTitle(context.Background(), &bossanovav1.SuggestPRTitleRequest{
				WorkDir: t.TempDir(), CurrentTitle: "wip", GitLog: "feat: add runner", ShouldTrustProjectConfig: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetSupported() != tt.supported || resp.GetTitle() != tt.want {
				t.Fatalf("suggest = %v %q; want %v %q", resp.GetSupported(), resp.GetTitle(), tt.supported, tt.want)
			}
			argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
			if !strings.Contains(string(argv), "--mode\nplan\n") {
				t.Errorf("one-shot argv %q is not read-only plan mode", argv)
			}
			stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
			if !strings.Contains(string(stdin), "feat: add runner") {
				t.Errorf("prompt did not arrive on stdin: %q", stdin)
			}
		})
	}
}

func TestSuggestPRTitleSkipsUntrustedRepos(t *testing.T) {
	spawned := filepath.Join(t.TempDir(), "spawned")
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`touch "`+spawned+`"`)))
	resp, err := s.SuggestPRTitle(context.Background(), &bossanovav1.SuggestPRTitleRequest{WorkDir: t.TempDir(), CurrentTitle: "wip"})
	if err != nil || resp.GetSupported() {
		t.Fatalf("suggest = %v, %v; want unsupported", resp, err)
	}
	if _, err := os.Stat(spawned); err == nil {
		t.Fatal("agy ran for an untrusted repository")
	}
}
