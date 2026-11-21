package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
)

const (
	conversationA = "11111111-2222-4333-8444-555555555555"
	conversationB = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	conversationC = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	conversationD = "12121212-3434-4565-8787-909090909090"
)

// fakeAgy runs script with /bin/sh in place of agy; the script sees agy's
// argv as "$@".
func fakeAgy(script string) agentruntime.CommandFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", script, name}, args...)...)
	}
}

func initLine(id string) string {
	return `{"event":"init","conversation_id":"` + id + `","init":{"cwd":"/tmp/project"}}`
}

// newTestServer isolates HOME so the store never reads the developer's agy state.
func newTestServer(t *testing.T, opts ...Option) (*Server, string) {
	t.Helper()
	home := isolateHome(t)
	return newServer(nil, zerolog.Nop(), opts...), home
}

// waitRun blocks until the run behind id exits.
func waitRun(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.runner.Wait(ctx, s.runner.processKey(id))
	if ctx.Err() != nil {
		t.Fatalf("run %s never exited", id)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// installTranscript places a brain transcript for id under home's agy store.
func installTranscript(t *testing.T, home, id, content string) string {
	t.Helper()
	path := filepath.Join(home, ".gemini", "antigravity-cli", "brain", id, ".system_generated", "logs", "transcript_full.jsonl")
	writeFile(t, path, content)
	return path
}

func installJSON(t *testing.T, home, name string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "cache", name), string(data))
}

// isolateHome keeps cap-banner captures and agy state out of the developer's
// real directories.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".data"))
	return home
}
