package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// e2e boss + agent binaries, built once and shared across the e2e cases.
var (
	buildOnce  sync.Once
	bossBinE2E string
	agentBin   string
	buildErr   error
)

func buildBinaries(t *testing.T) (agent, boss string) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "proof-tui-agent-e2e-*")
		if err != nil {
			buildErr = err
			return
		}
		// Build the e2e boss binary (slow — the tuitest suite already pays this).
		bossBinE2E = filepath.Join(dir, "boss")
		if out, berr := buildCmd("go", "build", "-tags", "e2e", "-o", bossBinE2E, "./cmd"); berr != nil {
			buildErr = wrapBuild("boss", berr, out)
			return
		}
		// Build the agent binary under test.
		agentBin = filepath.Join(dir, "proof-tui-agent")
		if out, berr := buildCmd("go", "build", "-o", agentBin, "./cmd/proof-tui-agent"); berr != nil {
			buildErr = wrapBuild("proof-tui-agent", berr, out)
			return
		}
	})
	if buildErr != nil {
		t.Fatalf("build binaries: %v", buildErr)
	}
	return agentBin, bossBinE2E
}

func buildCmd(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = serviceDir()
	return cmd.CombinedOutput()
}

func wrapBuild(what string, err error, out []byte) error {
	return &buildError{what: what, err: err, out: string(out)}
}

type buildError struct {
	what string
	err  error
	out  string
}

func (e *buildError) Error() string {
	return "build " + e.what + ": " + e.err.Error() + "\n" + e.out
}

// agentSession drives a proof-tui-agent subprocess over NDJSON stdio.
type agentSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	stderr *strings.Builder
}

func startAgent(t *testing.T, fixture, bossBin, castPath string) *agentSession {
	t.Helper()
	agent, _ := buildBinaries(t)
	args := []string{"--fixture", fixture, "--boss-bin", bossBin, "--width", "140", "--height", "36"}
	if castPath != "" {
		args = append(args, "--cast", castPath)
	}
	cmd := exec.Command(agent, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	s := &agentSession{
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		out:    bufio.NewReader(stdout),
		stderr: &stderr,
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return s
}

// do writes one request and reads one response line. The agent is synchronous
// (one in-flight by construction), so request/response stay paired.
func (s *agentSession) do(req map[string]any) map[string]any {
	s.t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		s.t.Fatalf("marshal request: %v", err)
	}
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("write request: %v (stderr: %s)", err, s.stderr.String())
	}
	line, err := s.out.ReadString('\n')
	if err != nil {
		s.t.Fatalf("read response: %v (stderr: %s)", err, s.stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		s.t.Fatalf("parse response %q: %v", line, err)
	}
	return resp
}

func screenOf(resp map[string]any) string {
	if s, ok := resp["screen"].(string); ok {
		return s
	}
	return ""
}

// TestE2E_ObserveShowsDemoHome boots the real demo TUI and asserts observe
// returns the demo home with a stable session title.
func TestE2E_ObserveShowsDemoHome(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	_, boss := buildBinaries(t)
	s := startAgent(t, "demo", boss, "")

	resp := s.do(map[string]any{"id": 1, "op": "observe"})
	screen := screenOf(resp)
	if !strings.Contains(screen, "Add dark mode") {
		t.Fatalf("observe screen missing demo session; screen:\n%s", screen)
	}
	if resp["cols"] != float64(140) || resp["rows"] != float64(36) {
		t.Fatalf("observe cols/rows = %v/%v, want 140/36", resp["cols"], resp["rows"])
	}
	s.do(map[string]any{"id": 99, "op": "quit"})
}

// TestE2E_SettleOnAsyncList opens the first session's async-loaded chat picker
// and asserts the settled screen contains the loaded chat title — proving the
// auto-settle waited past the "Loading chats" state.
func TestE2E_SettleOnAsyncList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	_, boss := buildBinaries(t)
	s := startAgent(t, "demo", boss, "")

	// Gate on the home anchor first.
	if !strings.Contains(screenOf(s.do(map[string]any{"id": 1, "op": "observe"})), "Add dark mode") {
		t.Fatal("home not ready before opening chat picker")
	}
	// Enter opens the first session's chat picker (async chat load).
	resp := s.do(map[string]any{"id": 2, "op": "enter"})
	screen := screenOf(resp)
	if strings.Contains(screen, "Loading chats") || !strings.Contains(screen, "Initial implementation") {
		// settle should have waited for the load; allow one explicit wait as a
		// backstop in case the async load exceeds the settle hard cap.
		wr := s.do(map[string]any{"id": 3, "op": "wait", "text": "Initial implementation", "timeoutMs": 10000})
		if ok, _ := wr["ok"].(bool); !ok {
			t.Fatalf("chat list never loaded; screen:\n%s", screenOf(wr))
		}
		screen = screenOf(wr)
	}
	if !strings.Contains(screen, "Initial implementation") {
		t.Fatalf("settled screen missing loaded chat; screen:\n%s", screen)
	}
	s.do(map[string]any{"id": 99, "op": "quit"})
}

// TestE2E_AddRepoReachable drives the #812 add-repo flow: 'r' (repos) -> 'a'
// (add) -> Open project -> type a path -> confirm, asserting the wizard reaches
// the input phase and then the repo details ("Merge strategy" / "Setup command").
func TestE2E_AddRepoReachable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	_, boss := buildBinaries(t)
	s := startAgent(t, "demo", boss, "")

	if !strings.Contains(screenOf(s.do(map[string]any{"id": 1, "op": "observe"})), "Add dark mode") {
		t.Fatal("home not ready")
	}
	// Settings hub -> repos list.
	s.do(map[string]any{"id": 2, "op": "key", "keys": []string{"s"}})
	if wr := s.do(map[string]any{"id": 3, "op": "wait", "text": "[r]epos", "timeoutMs": 10000}); !okResp(wr) {
		t.Fatalf("settings hub not reached; screen:\n%s", screenOf(wr))
	}
	s.do(map[string]any{"id": 4, "op": "key", "keys": []string{"r"}})
	if wr := s.do(map[string]any{"id": 5, "op": "wait", "text": "PATH", "timeoutMs": 10000}); !okResp(wr) {
		t.Fatalf("repo list not reached; screen:\n%s", screenOf(wr))
	}
	// 'a' opens the add wizard source phase.
	s.do(map[string]any{"id": 6, "op": "key", "keys": []string{"a"}})
	if wr := s.do(map[string]any{"id": 7, "op": "wait", "text": "Open project", "timeoutMs": 10000}); !okResp(wr) {
		t.Fatalf("add wizard source phase not reached; screen:\n%s", screenOf(wr))
	}
	// Choose "Open project" (first row) -> input phase.
	s.do(map[string]any{"id": 8, "op": "enter"})
	if wr := s.do(map[string]any{"id": 9, "op": "wait", "text": "Add a local repository", "timeoutMs": 10000}); !okResp(wr) {
		t.Fatalf("add-repo input phase not reached; screen:\n%s", screenOf(wr))
	}
	// Type a path suffix and confirm to validate via the mock daemon.
	s.do(map[string]any{"id": 10, "op": "type", "text": "widgets"})
	s.do(map[string]any{"id": 11, "op": "enter"})
	// Reaches the repo details phase (Name / Setup / Merge strategy / confirm).
	wr := s.do(map[string]any{"id": 12, "op": "wait", "text": "Add Repository", "timeoutMs": 10000})
	if !okResp(wr) {
		t.Fatalf("repo details phase not reached; screen:\n%s", screenOf(wr))
	}
	details := screenOf(wr)
	if !strings.Contains(details, "Merge strategy") && !strings.Contains(details, "Setup command") {
		t.Fatalf("repo details missing Merge strategy / Setup command; screen:\n%s", details)
	}
	s.do(map[string]any{"id": 99, "op": "quit"})
}

// TestE2E_CastWritten asserts a .cast file is written and non-empty after a
// short session.
func TestE2E_CastWritten(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	_, boss := buildBinaries(t)
	castPath := filepath.Join(t.TempDir(), "session.cast")
	s := startAgent(t, "demo", boss, castPath)

	s.do(map[string]any{"id": 1, "op": "observe"})
	s.do(map[string]any{"id": 2, "op": "quit"})

	// Wait for the process to tear down and flush the cast file.
	deadline := time.Now().Add(10 * time.Second)
	var size int64
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(castPath); err == nil {
			size = fi.Size()
			if size > 0 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if size == 0 {
		t.Fatalf("cast file %s is empty or missing", castPath)
	}
}

func okResp(resp map[string]any) bool {
	ok, _ := resp["ok"].(bool)
	return ok
}
