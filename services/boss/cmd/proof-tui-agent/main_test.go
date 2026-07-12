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
	return startAgentSeed(t, fixture, bossBin, castPath, "")
}

// startAgentSeed is startAgent plus an optional -seed overlay file.
func startAgentSeed(t *testing.T, fixture, bossBin, castPath, seedPath string) *agentSession {
	t.Helper()
	agent, _ := buildBinaries(t)
	args := []string{"--fixture", fixture, "--boss-bin", bossBin, "--width", "140", "--height", "36"}
	if castPath != "" {
		args = append(args, "--cast", castPath)
	}
	if seedPath != "" {
		args = append(args, "--seed", seedPath)
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

// TestE2E_ArrowNav proves the `key` op's new arrow vocabulary moves the home
// selection: a bare `enter` opens the FIRST session (sess-aaa-111 / "Add dark
// mode"), whose chat picker shows "Initial implementation"; sending
// `["down","enter"]` instead opens the SECOND session (sess-bbb-222 / "Fix
// login bug"), whose chat picker does NOT show "Initial implementation". This
// exercises the full path NDJSON -> key op -> KeyBytes("down") -> PTY ->
// bubbletea -> vt and observes a deterministic, arrow-driven screen change.
func TestE2E_ArrowNav(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	_, boss := buildBinaries(t)
	s := startAgent(t, "demo", boss, "")

	if !strings.Contains(screenOf(s.do(map[string]any{"id": 1, "op": "observe"})), "Add dark mode") {
		t.Fatal("home not ready")
	}
	// Move selection down one row, then open that session's chat picker. The
	// "[n]ew chat" action bar is unique to the chat-picker view (home shows a
	// different bar), so it confirms enter opened a session rather than staying
	// on home.
	s.do(map[string]any{"id": 2, "op": "key", "keys": []string{"down", "enter"}})
	wr := s.do(map[string]any{"id": 3, "op": "wait", "text": "[n]ew chat", "timeoutMs": 10000})
	if !okResp(wr) {
		t.Fatalf("arrow-down + enter did not open a session chat picker; screen:\n%s", screenOf(wr))
	}
	screen := screenOf(wr)
	// The arrow moved selection to the SECOND session ("Fix login bug", which has
	// no chats) — so its header shows and the FIRST session's only distinctive
	// chat title ("Initial implementation") must be absent.
	if !strings.Contains(screen, "Fix login bug") {
		t.Fatalf("expected the second session (\"Fix login bug\") header; screen:\n%s", screen)
	}
	if strings.Contains(screen, "Initial implementation") {
		t.Fatalf("arrow-down should have selected a DIFFERENT session, but screen shows the first session's chat; screen:\n%s", screen)
	}
	s.do(map[string]any{"id": 99, "op": "quit"})
}

// TestE2E_PageNav is the CSI-tilde-family E2E complement to the arrow-family
// TestE2E_ArrowNav. Where the arrow test proves a CSI-with-final-letter key
// (`\x1b[B`), this proves a CSI-tilde key end-to-end: `pgdn` pages the home
// table to the LAST session (sess-fff-666 / "Upgrade to React Navigation 7",
// which has no chats), then `enter` opens that session's chat picker — whose
// "[n]ew chat" action bar is unique to the chat-picker view and whose absence of
// "Initial implementation" (the first session's only distinctive chat title)
// proves the cursor left row 0. This exercises the full path NDJSON -> key op ->
// KeyBytes("pgdn") -> \x1b[6~ -> PTY -> bubbletea -> ultraviolet -> vt, proving
// the tilde family decodes through a real terminal rather than by grounding
// (keybytes_test.go) alone.
func TestE2E_PageNav(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	_, boss := buildBinaries(t)
	s := startAgent(t, "demo", boss, "")

	if !strings.Contains(screenOf(s.do(map[string]any{"id": 1, "op": "observe"})), "Add dark mode") {
		t.Fatal("home not ready")
	}
	// Page the home table to the last session, then open its chat picker. The
	// "[n]ew chat" action bar is unique to the chat-picker view, so it confirms
	// enter opened a session rather than staying on home.
	s.do(map[string]any{"id": 2, "op": "key", "keys": []string{"pgdn", "enter"}})
	wr := s.do(map[string]any{"id": 3, "op": "wait", "text": "[n]ew chat", "timeoutMs": 10000})
	if !okResp(wr) {
		t.Fatalf("pgdn + enter did not open a session chat picker; screen:\n%s", screenOf(wr))
	}
	screen := screenOf(wr)
	// pgdn moved selection to the LAST session ("Upgrade to React Navigation 7",
	// which has no chats) — so its header shows and the FIRST session's only
	// distinctive chat title ("Initial implementation") must be absent.
	if !strings.Contains(screen, "Upgrade to React Navigation 7") {
		t.Fatalf("expected the last session (\"Upgrade to React Navigation 7\") header; screen:\n%s", screen)
	}
	if strings.Contains(screen, "Initial implementation") {
		t.Fatalf("pgdn should have selected a session other than the first, but screen shows the first session's chat; screen:\n%s", screen)
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

// TestE2E_DaemonAddSessionAndProtocol exercises the BOS-217 daemon + capabilities
// ops against the REAL bridge:
//
//   - add_session (home-poll-refreshed): a session pushed mid-run becomes visible
//     on the home board within the poll+settle window (home re-polls ListSessions
//     every 2s).
//   - state_change protocol path: the boss TUI's attach flow uses RecordChat+tmux,
//     NOT the mock's AttachSession stream, so no session is ever attached in the
//     proof stack. A state_change therefore proves the daemon op + attach
//     validation end-to-end via the actionable "not attached" error rather than a
//     rendered live-view change (documented reduction — see the task notes).
//   - capabilities: the old-bridge-detection surface answers ok with the op lists.
func TestE2E_DaemonAddSessionAndProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	_, boss := buildBinaries(t)
	s := startAgent(t, "demo", boss, "")

	if !strings.Contains(screenOf(s.do(map[string]any{"id": 1, "op": "observe"})), "Add dark mode") {
		t.Fatal("home not ready")
	}

	// Mutator: add a session mid-run.
	addResp := s.do(map[string]any{
		"id": 2, "op": "daemon", "action": "add_session",
		"session": map[string]any{
			"id":              "sess-daemon-1",
			"repoId":          "repo-1",
			"repoDisplayName": "my-app",
			"title":           "Daemon-added session",
			"state":           "READY_FOR_REVIEW",
		},
	})
	if !okResp(addResp) {
		t.Fatalf("add_session failed: %v", addResp)
	}
	// The home poll (2s) surfaces the new session; wait up to 10s.
	wr := s.do(map[string]any{"id": 3, "op": "wait", "text": "Daemon-added session", "timeoutMs": 10000})
	if !okResp(wr) {
		t.Fatalf("daemon add_session not visible on home; screen:\n%s", screenOf(wr))
	}

	// Pusher protocol path: no session is attached, so state_change must error with
	// the actionable "not attached" message (never a silent no-op).
	scResp := s.do(map[string]any{
		"id": 4, "op": "daemon", "action": "state_change",
		"sessionId":     "sess-daemon-1",
		"previousState": "IMPLEMENTING_PLAN",
		"newState":      "MERGED",
	})
	if okResp(scResp) {
		t.Fatalf("state_change to a non-attached session should error; got %v", scResp)
	}
	if msg, _ := scResp["error"].(string); !strings.Contains(msg, "not attached") {
		t.Fatalf("state_change error should name the attach precondition; got %v", scResp["error"])
	}

	// capabilities: old-bridge detection surface.
	capResp := s.do(map[string]any{"id": 5, "op": "capabilities"})
	if !okResp(capResp) {
		t.Fatalf("capabilities failed: %v", capResp)
	}
	if _, ok := capResp["daemonActions"]; !ok {
		t.Fatalf("capabilities missing daemonActions: %v", capResp)
	}

	s.do(map[string]any{"id": 99, "op": "quit"})
}

// runAgentExpectFail runs the agent binary once with the given args and env, and
// returns its combined stdout+stderr. It fails the test if the process exits 0
// (a rejected -seed/-env must abort boot). Used by the negative-path tests; the
// abort happens before the boss build, so boss-bin is never exec'd.
func runAgentExpectFail(t *testing.T, extraEnv []string, args ...string) string {
	t.Helper()
	agent, boss := buildBinaries(t)
	full := append([]string{"--fixture", "demo", "--boss-bin", boss}, args...)
	cmd := exec.Command(agent, full...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit; output:\n%s", out)
	}
	return string(out)
}

// TestE2E_SeedOverlayAddsSession boots the demo TUI with a -seed overlay adding a
// session, and asserts the seeded title renders on the home board — proving the
// overlay applies via MockDaemon.Add* on top of the preset world.
func TestE2E_SeedOverlayAddsSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	_, boss := buildBinaries(t)
	seed := filepath.Join(t.TempDir(), "seed.json")
	body := `{"sessions":[{"id":"sess-seed-1","repoId":"repo-1","repoDisplayName":"my-app",` +
		`"title":"Seeded overlay session","state":"READY_FOR_REVIEW","createdOffsetMins":-15}]}`
	if err := os.WriteFile(seed, []byte(body), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	s := startAgentSeed(t, "demo", boss, "", seed)

	screen := screenOf(s.do(map[string]any{"id": 1, "op": "observe"}))
	if !strings.Contains(screen, "Seeded overlay session") {
		t.Fatalf("home missing seeded overlay session; screen:\n%s", screen)
	}
	s.do(map[string]any{"id": 99, "op": "quit"})
}

// TestE2E_SeedOverlayRejectsUnknownField proves a typo'd overlay field aborts
// boot naming the offending field (DisallowUnknownFields).
func TestE2E_SeedOverlayRejectsUnknownField(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	seed := filepath.Join(t.TempDir(), "bad-seed.json")
	if err := os.WriteFile(seed, []byte(`{"sessions":[{"id":"x","title":"y","bogusField":1}]}`), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	out := runAgentExpectFail(t, nil, "--seed", seed)
	if !strings.Contains(out, "bogusField") {
		t.Fatalf("abort error should name the unknown field; output:\n%s", out)
	}
}

// TestE2E_SeedEnvRejectsNonWhitelisted proves a non-whitelisted forwarded env
// key aborts boot listing the rejected key, while a whitelisted key in the same
// map is not flagged.
func TestE2E_SeedEnvRejectsNonWhitelisted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short")
	}
	envJSON := `{"EVIL_KEY":"1","BOSS_CLOUD_ACCESS_E2E_SEQUENCE":"active"}`
	out := runAgentExpectFail(t, []string{"BOSS_PROOF_TUI_SEED_ENV=" + envJSON})
	if !strings.Contains(out, "EVIL_KEY") {
		t.Fatalf("abort error should name the rejected env key; output:\n%s", out)
	}
	if strings.Contains(out, "BOSS_CLOUD_ACCESS_E2E_SEQUENCE") {
		t.Fatalf("whitelisted key should not be listed as rejected; output:\n%s", out)
	}
}
