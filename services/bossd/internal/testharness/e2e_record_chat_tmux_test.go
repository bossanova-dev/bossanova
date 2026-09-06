package testharness_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/recurser/bossd/internal/tmux"
)

// writeFakeTranscript materialises ~/.claude/projects/<key>/<sid>.jsonl so
// spawnChatTmux's pre-flight transcript stat can decide --resume vs
// --session-id deterministically. Mirrors status.pathToProjectKey.
func writeFakeTranscript(t *testing.T, worktreePath, agentSessionID string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	key := strings.NewReplacer("/", "-", ".", "-").Replace(worktreePath)
	dir := filepath.Join(tmpHome, ".claude", "projects", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, agentSessionID+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

// fakeTmux is a programmable test double for the tmux CLI. It records every
// invocation and returns a configurable success/failure for each `tmux`
// subcommand keyed on its first argument (e.g. "has-session", "new-session").
// This lets RecordChat tests assert both *what* the daemon called and let
// each call return a canned outcome — without ever spawning a real tmux.
type fakeTmux struct {
	mu       sync.Mutex
	calls    [][]string
	hasSess  bool   // result for `tmux has-session`
	failOn   string // subcommand to fail (e.g. "new-session"), empty = none
	availErr bool   // when true, `tmux -V` returns non-zero
}

func (f *fakeTmux) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if name != "tmux" || len(args) == 0 {
		return exec.CommandContext(ctx, "true")
	}
	switch args[0] {
	case "-V":
		if f.availErr {
			return exec.CommandContext(ctx, "false")
		}
	case "has-session":
		if !f.hasSess {
			return exec.CommandContext(ctx, "false")
		}
	}
	if f.failOn != "" && args[0] == f.failOn {
		return exec.CommandContext(ctx, "false")
	}
	return exec.CommandContext(ctx, "true")
}

func (f *fakeTmux) findCall(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		// c[0] = "tmux", c[1] = subcommand
		if len(c) >= 2 && c[0] == "tmux" && c[1] == sub {
			return c
		}
	}
	return nil
}

func realTmuxFactory(t *testing.T) tmux.CommandFactory {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	label := fmt.Sprintf("bossd-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", label, "kill-server").Run()
	})
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "tmux" {
			args = append([]string{"-L", label}, args...)
		}
		return exec.CommandContext(ctx, name, args...)
	}
}

func installFakeCodexTUI(t *testing.T) string {
	t.Helper()
	fakePath, err := filepath.Abs("../../../../plugins/bossd-plugin-codex/testdata/fake_codex_tui.sh")
	if err != nil {
		t.Fatalf("abs fake codex tui: %v", err)
	}
	if _, err := os.Stat(fakePath); err != nil {
		t.Fatalf("fake codex tui missing: %v", err)
	}
	binDir := t.TempDir()
	codexPath := filepath.Join(binDir, "codex")
	if err := os.Symlink(fakePath, codexPath); err != nil {
		t.Fatalf("symlink fake codex tui: %v", err)
	}
	argvLog := filepath.Join(t.TempDir(), "fake-codex-argv.log")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CODEX_TUI_ARGV_LOG", argvLog)
	t.Setenv("FAKE_CODEX_TUI_ID_PREFIX", "record-chat-codex")
	t.Setenv("FAKE_CODEX_TUI_SLEEP_SECONDS", "30")
	return argvLog
}

// recordChatSetup creates the harness, registers a repo, and opens a session,
// returning the bits each test needs. Centralised so each scenario stays
// focused on the specific RecordChat behavior under test.
func recordChatSetup(t *testing.T, fake *fakeTmux) (*testharness.Harness, context.Context, string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))
	h := testharness.NewWithOptions(t, testharness.Options{TmuxCommandFactory: fake.factory})
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "tmux-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	agentName := "claude"
	sess := createSessionFromStream(t, h, ctx, &pb.CreateSessionRequest{
		RepoId:    repoResp.Msg.Repo.Id,
		Title:     "Tmux RecordChat",
		Plan:      "test plan",
		AgentName: &agentName,
	})
	return h, ctx, sess.Id
}

// TestE2E_RecordChat_CreatesTmuxSession verifies that a fresh RecordChat
// (resume=false) drives `tmux new-session` with `claude --session-id <id>`
// and persists the resulting session name on the chat row + response.
func TestE2E_RecordChat_CreatesTmuxSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	fake := &fakeTmux{} // hasSess=false, so new-session must fire
	h, ctx, sessionID := recordChatSetup(t, fake)

	resp, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: "claude-fresh-001",
		Title:          "fresh chat",
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if resp.Msg.Chat.TmuxSessionName == "" {
		t.Fatal("expected response chat.tmux_session_name to be populated")
	}
	if !strings.HasPrefix(resp.Msg.Chat.TmuxSessionName, "boss-") {
		t.Errorf("expected boss-* tmux name, got %q", resp.Msg.Chat.TmuxSessionName)
	}

	newSession := fake.findCall("new-session")
	if newSession == nil {
		t.Fatalf("expected `tmux new-session` to fire; calls=%v", fake.calls)
	}
	joined := strings.Join(newSession, " ")
	if !strings.Contains(joined, "claude --session-id claude-fresh-001") {
		t.Errorf("expected new-session to invoke `claude --session-id claude-fresh-001`, got %q", joined)
	}
	if strings.Contains(joined, "--resume") {
		t.Errorf("fresh attach should NOT pass --resume; got %q", joined)
	}

	// The persisted name on the chat row must match the name returned in
	// the response — otherwise lifecycle GC paths can't find the session.
	chat, err := h.AgentChats.GetByAgentSessionID(ctx, "claude-fresh-001")
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != resp.Msg.Chat.TmuxSessionName {
		t.Errorf("DB tmux name = %v, response = %q — must match",
			chat.TmuxSessionName, resp.Msg.Chat.TmuxSessionName)
	}
}

// TestE2E_RecordChat_ResumePassesResumeFlag verifies that resume=true
// causes the daemon to invoke `claude --resume <id>` instead of
// `--session-id` when minting the tmux host. The pre-flight transcript
// stat in spawnChatTmux requires the JSONL fixture to actually exist on
// disk for --resume to fire; without it, --session-id is the safe fallback.
func TestE2E_RecordChat_ResumePassesResumeFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	fake := &fakeTmux{}
	h, ctx, sessionID := recordChatSetup(t, fake)
	writeFakeTranscript(t, "/tmp/worktrees/tmux-app/tmux-recordchat", "claude-resume-007")

	_, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: "claude-resume-007",
		Title:          "resumed",
		Resume:         true,
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}

	newSession := fake.findCall("new-session")
	if newSession == nil {
		t.Fatalf("expected `tmux new-session` to fire; calls=%v", fake.calls)
	}
	joined := strings.Join(newSession, " ")
	if !strings.Contains(joined, "claude --resume claude-resume-007") {
		t.Errorf("expected --resume in new-session command, got %q", joined)
	}
	if strings.Contains(joined, "--session-id") {
		t.Errorf("resume path should NOT pass --session-id; got %q", joined)
	}
}

// TestE2E_RecordChat_IdempotentReuseExistingSession verifies that calling
// RecordChat twice with the same claude_id is a no-op on the chat row AND
// skips `tmux new-session` when has-session reports the tmux session is
// already alive — exactly the behavior boss attach relies on for "kill
// bossd, restart, reattach" to work without losing chat state.
func TestE2E_RecordChat_IdempotentReuseExistingSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	fake := &fakeTmux{}
	h, ctx, sessionID := recordChatSetup(t, fake)

	first, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: "claude-idem-042",
		Title:          "first",
	}))
	if err != nil {
		t.Fatalf("RecordChat #1: %v", err)
	}

	// Flip has-session to true and clear the call log so we can assert
	// new-session does NOT fire on the second call.
	fake.mu.Lock()
	fake.hasSess = true
	fake.calls = nil
	fake.mu.Unlock()

	second, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: "claude-idem-042",
		Title:          "ignored second title",
	}))
	if err != nil {
		t.Fatalf("RecordChat #2: %v", err)
	}

	if second.Msg.Chat.Id != first.Msg.Chat.Id {
		t.Errorf("expected same chat row id reused; got %q != %q",
			second.Msg.Chat.Id, first.Msg.Chat.Id)
	}
	if second.Msg.Chat.Title != "first" {
		t.Errorf("expected title to remain %q, got %q", "first", second.Msg.Chat.Title)
	}
	if second.Msg.Chat.TmuxSessionName != first.Msg.Chat.TmuxSessionName {
		t.Errorf("tmux name must be stable across calls; got %q != %q",
			second.Msg.Chat.TmuxSessionName, first.Msg.Chat.TmuxSessionName)
	}
	if fake.findCall("new-session") != nil {
		t.Errorf("expected NO new-session call when session already alive; calls=%v", fake.calls)
	}
	if fake.findCall("has-session") == nil {
		t.Errorf("expected has-session probe before deciding to recreate; calls=%v", fake.calls)
	}
}

// TestE2E_RecordChat_SurvivesDaemonRestart exercises the headline property
// of option A: a chat's tmux session name persists across a daemon restart,
// AND on the post-restart RecordChat the daemon detects the (still-alive)
// tmux session and skips re-creation. This is the property the user lost
// in PR #179 and is what this whole change exists to restore.
func TestE2E_RecordChat_SurvivesDaemonRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	dbPath := filepath.Join(t.TempDir(), "bossd.db")

	// --- First daemon "lifetime" ---
	fake1 := &fakeTmux{}
	h1 := testharness.NewWithOptions(t, testharness.Options{
		DBPath:             dbPath,
		TmuxCommandFactory: fake1.factory,
	})
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)
	repoResp, err := h1.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "restart-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	sess := createSessionFromStream(t, h1, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Pre-restart",
		Plan:   "p",
	})
	preResp, err := h1.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sess.Id,
		AgentSessionId: "claude-restart-1",
		Title:          "first attach",
	}))
	if err != nil {
		t.Fatalf("RecordChat #1: %v", err)
	}
	preName := preResp.Msg.Chat.TmuxSessionName
	if preName == "" {
		t.Fatal("pre-restart chat must have a tmux name")
	}

	// --- Simulate daemon stop ---
	h1.Close()

	// --- Second daemon "lifetime" — same DB, fresh tmux fake but configured
	// as if the underlying tmux server still hosts the session, which is
	// what would actually be true in production (tmux outlives bossd). ---
	fake2 := &fakeTmux{hasSess: true}
	h2 := testharness.NewWithOptions(t, testharness.Options{
		DBPath:             dbPath,
		TmuxCommandFactory: fake2.factory,
	})

	postResp, err := h2.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sess.Id,
		AgentSessionId: "claude-restart-1",
		Title:          "ignored on second call",
		Resume:         true,
	}))
	if err != nil {
		t.Fatalf("RecordChat post-restart: %v", err)
	}
	if postResp.Msg.Chat.TmuxSessionName != preName {
		t.Errorf("tmux name must survive restart; pre=%q post=%q",
			preName, postResp.Msg.Chat.TmuxSessionName)
	}
	if postResp.Msg.Chat.Id != preResp.Msg.Chat.Id {
		t.Errorf("chat row id must survive restart; pre=%q post=%q",
			preResp.Msg.Chat.Id, postResp.Msg.Chat.Id)
	}
	if fake2.findCall("new-session") != nil {
		t.Errorf("post-restart should NOT spawn a new tmux session when the "+
			"existing one is still alive; calls=%v", fake2.calls)
	}
	if fake2.findCall("has-session") == nil {
		t.Errorf("post-restart must probe has-session before deciding; calls=%v",
			fake2.calls)
	}
}

// TestE2E_RecordChat_CodexAgentSpawnsCodexCmd is the end-to-end regression
// test for the codex routing bug fixed in PR #254. A RecordChat call that
// names "codex" as the agent must drive `tmux new-session` with a `codex …`
// command, NOT the legacy hardcoded `claude …`. Pre-fix the daemon
// hardcoded "claude" inside spawnChatTmux and ignored the agent name on
// the chat row entirely.
func TestE2E_RecordChat_CodexAgentSpawnsCodexCmd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	fake := &fakeTmux{}
	h, ctx, sessionID := recordChatSetup(t, fake)

	codex := "codex"
	resp, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: "codex-fresh-001",
		Title:          "fresh codex chat",
		AgentName:      &codex,
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if resp.Msg.Chat.TmuxSessionName == "" {
		t.Fatal("expected response chat.tmux_session_name to be populated")
	}

	newSession := fake.findCall("new-session")
	if newSession == nil {
		t.Fatalf("expected `tmux new-session` to fire; calls=%v", fake.calls)
	}
	joined := strings.Join(newSession, " ")
	if !strings.Contains(joined, "codex") {
		t.Errorf("expected new-session to invoke codex, got %q", joined)
	}
	if strings.Contains(joined, "claude") {
		t.Errorf("codex agent must NOT spawn claude; got %q", joined)
	}
}

func TestE2E_RecordChat_CodexRealTmuxDiscoversProviderSessionID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	tmuxFactory := realTmuxFactory(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("BOSSD_TEST_CODEX_TUI_DISCOVERY_REQUIRED", "1")
	_ = installFakeCodexTUI(t)

	h := testharness.NewWithOptions(t, testharness.Options{TmuxCommandFactory: tmuxFactory})
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "real-codex-tmux",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	sess := createSessionFromStream(t, h, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Real Codex Tmux",
		Plan:   "test plan",
	})

	codex := "codex"
	const agentSessionID = "logical-codex-chat"
	resp, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sess.Id,
		AgentSessionId: agentSessionID,
		Title:          "real codex chat",
		AgentName:      &codex,
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if resp.Msg.Chat.TmuxSessionName == "" {
		t.Fatal("expected tmux session name")
	}
	t.Cleanup(func() {
		_ = h.Tmux.KillSession(context.Background(), resp.Msg.Chat.TmuxSessionName)
	})

	chat, err := h.AgentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if chat.ProviderSessionID == nil || *chat.ProviderSessionID == "" {
		t.Fatal("expected provider_session_id to be discovered and persisted")
	}
	if *chat.ProviderSessionID == agentSessionID {
		t.Fatalf("provider_session_id = logical id %q; want fake codex-generated id", agentSessionID)
	}
	rollout := filepath.Join(tmpHome, ".codex", "sessions", "2026", "05", "11",
		"rollout-2026-05-11T00-00-00-"+*chat.ProviderSessionID+".jsonl")
	if info, err := os.Stat(rollout); err != nil || info.Size() == 0 {
		t.Fatalf("expected rollout for provider id at %s, stat=%v info=%v", rollout, err, info)
	}
}

// TestE2E_RecordChat_EmptyLogPathDoesNotInvokeTee is the end-to-end
// regression for the LogTeeArgv empty-path bug. spawnChatTmux runs the
// user-attached interactive path, which legitimately has no bossd-side
// log path to tee into. Pre-fix LogTeeArgv produced
// `bash -c "set -o pipefail; <inner> 2>&1 | tee ”"` which made tee
// fail with "tee: : No such file or directory" the instant tmux launched
// the pane — the codex chat died immediately and the boss UI bounced
// straight back to the chat list. The fix is to short-circuit
// LogTeeArgv when logPath is empty; this test asserts the resulting
// tmux argv is well-formed (no empty `tee ”` appendage).
func TestE2E_RecordChat_EmptyLogPathDoesNotInvokeTee(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	fake := &fakeTmux{}
	h, ctx, sessionID := recordChatSetup(t, fake)

	codex := "codex"
	if _, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: "codex-no-logpath-001",
		Title:          "codex empty-logpath",
		AgentName:      &codex,
	})); err != nil {
		t.Fatalf("RecordChat: %v", err)
	}

	newSession := fake.findCall("new-session")
	if newSession == nil {
		t.Fatalf("expected `tmux new-session` to fire; calls=%v", fake.calls)
	}
	joined := strings.Join(newSession, " ")
	if strings.Contains(joined, "tee ''") {
		t.Errorf("empty LogPath produced `tee ''` argv (would kill tmux pane on launch); got %q", joined)
	}
	if strings.Contains(joined, "tee \"\"") {
		t.Errorf("empty LogPath produced `tee \"\"` argv (would kill tmux pane on launch); got %q", joined)
	}
}

// TestE2E_RecordChat_TmuxUnavailableSkipsCreate verifies the no-tmux fallback
// path: when tmux is not on PATH (or otherwise unavailable), RecordChat
// still creates the chat row but leaves tmux_session_name empty and does
// not error. This keeps the daemon usable in CI / headless environments
// without tmux.
func TestE2E_RecordChat_TmuxUnavailableSkipsCreate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	fake := &fakeTmux{availErr: true}
	h, ctx, sessionID := recordChatSetup(t, fake)

	resp, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: "claude-no-tmux",
		Title:          "headless",
	}))
	if err != nil {
		t.Fatalf("RecordChat must not error when tmux is unavailable: %v", err)
	}
	if resp.Msg.Chat.TmuxSessionName != "" {
		t.Errorf("expected empty tmux name when tmux unavailable, got %q", resp.Msg.Chat.TmuxSessionName)
	}
	if fake.findCall("new-session") != nil {
		t.Error("expected NO new-session call when tmux is unavailable")
	}
}

// TestE2E_RecordChat_CodexResumeTwicePreservesChatIdentity is the end-to-end
// regression for BOS-1143, and it resumes TWICE on purpose.
//
// The reported defect: a codex chat that was interrupted and reopened came back
// as a *claude* chat under the same agent_session_id, with its codex
// conversation id gone. StartTmuxChat resumed by DELETING the agent_chats row
// and Creating a new one, and the re-create omitted provider_session_id while
// recomputing agent_name / model / account_id from the parent *session* rather
// than from the row it had just destroyed.
//
// A single resume is not enough evidence. The eraser class recorded in BOS-1107
// is a partial-row rewrite that looks correct after one write and loses a column
// on the next, so this drives two consecutive resumes through the real SQLite
// store, the real migrations, and real tmux, and asserts the chat's identity
// after each one.
func TestE2E_RecordChat_CodexResumeTwicePreservesChatIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux e2e test in -short; run make test-bossd for coverage")
	}
	tmuxFactory := realTmuxFactory(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	argvLog := installFakeCodexTUI(t)

	h := testharness.NewWithOptions(t, testharness.Options{TmuxCommandFactory: tmuxFactory})
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "codex-resume-twice",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	// The parent session is deliberately a *claude* session: the defect was
	// resume re-deriving the chat's agent from the session, so a session whose
	// agent differs from the chat's is what makes the regression visible.
	sess := createSessionFromStream(t, h, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Codex Resume Twice",
		Plan:   "test plan",
	})

	codex := "codex"
	const agentSessionID = "logical-codex-resume-twice"
	resp, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      sess.Id,
		AgentSessionId: agentSessionID,
		Title:          "codex chat to interrupt",
		AgentName:      &codex,
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	t.Cleanup(func() {
		_ = h.Tmux.KillSession(context.Background(), resp.Msg.Chat.TmuxSessionName)
	})

	before, err := h.AgentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v", err)
	}
	if before.AgentName != "codex" {
		t.Fatalf("seeded chat agent_name = %q, want codex", before.AgentName)
	}
	chatID := before.ID

	// Give the row the identity columns the defect erased. The provider id is
	// stamped directly rather than waiting on TUI discovery: this test is about
	// what survives a resume, not about how the id is first discovered (that is
	// TestE2E_RecordChat_CodexRealTmuxDiscoversProviderSessionID's job).
	const providerSessionID = "codex-rollout-preserved"
	providerID := providerSessionID
	if err := h.AgentChats.UpdateProviderSessionID(ctx, agentSessionID, &providerID); err != nil {
		t.Fatalf("seed provider_session_id: %v", err)
	}
	if seeded, err := h.AgentChats.GetByAgentSessionID(ctx, agentSessionID); err != nil {
		t.Fatalf("read back seeded provider id: %v", err)
	} else if seeded.ProviderSessionID == nil || *seeded.ProviderSessionID != providerSessionID {
		t.Fatalf("provider_session_id seed did not take: %v", seeded.ProviderSessionID)
	}
	// provider_session_id was only one of the columns the delete+recreate
	// resume dropped: model and account_id were recomputed from the parent
	// session too. Seed both with values the *session* does not carry, so a
	// resume that re-derives them from the session is visible as a change
	// rather than agreeing by coincidence -- this claude session has neither
	// set, so a re-derived row would read back empty/NULL.
	const chatModel = "gpt-5-codex"
	const chatAccountID = "acct-codex-chat"
	chatAccount := chatAccountID
	chatAccountPtr := &chatAccount
	chatModelValue := chatModel
	if err := h.AgentChats.RebindResumedChat(ctx, agentSessionID, db.RebindResumedChatParams{
		Model:     &chatModelValue,
		AccountID: &chatAccountPtr,
	}); err != nil {
		t.Fatalf("seed model/account_id: %v", err)
	}
	if seeded, err := h.AgentChats.GetByAgentSessionID(ctx, agentSessionID); err != nil {
		t.Fatalf("read back seeded model/account_id: %v", err)
	} else {
		if seeded.Model != chatModel {
			t.Fatalf("model seed did not take: %q", seeded.Model)
		}
		if seeded.AccountID == nil || *seeded.AccountID != chatAccountID {
			t.Fatalf("account_id seed did not take: %v", seeded.AccountID)
		}
		// Prove the seeded values differ from the parent session's, or a
		// re-derived row would read back identical and assert nothing.
		parent, err := h.Sessions.Get(ctx, sess.Id)
		if err != nil {
			t.Fatalf("read parent session: %v", err)
		}
		if parent.Model == chatModel {
			t.Fatalf("parent session model = %q equals the chat's; the seed proves nothing", parent.Model)
		}
		if parent.AccountID != nil && *parent.AccountID == chatAccountID {
			t.Fatalf("parent session account_id equals the chat's; the seed proves nothing")
		}
	}
	// The fake codex TUI refuses `codex resume <id>` unless a rollout exists for
	// the id it is handed, and the relaunch hands it the provider id, so
	// materialise that rollout.
	rolloutDir := filepath.Join(tmpHome, ".codex", "sessions", "2026", "05", "11")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatalf("mkdir rollout dir: %v", err)
	}
	rollout := filepath.Join(rolloutDir, "rollout-2026-05-11T00-00-00-"+providerSessionID+".jsonl")
	if err := os.WriteFile(rollout, []byte(`{"type":"session_meta","payload":{"id":"`+providerSessionID+`"}}`+"\n"), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	for i := 1; i <= 2; i++ {
		// Interrupt: tear the pane down so the resume takes the relaunch path
		// rather than the live-pane reuse shortcut.
		current, err := h.AgentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil {
			t.Fatalf("resume #%d: read chat before interrupt: %v", i, err)
		}
		if current.TmuxSessionName != nil && *current.TmuxSessionName != "" {
			_ = h.Tmux.KillSession(ctx, *current.TmuxSessionName)
		}

		gotID, err := h.Lifecycle.StartTmuxChat(ctx, sess.Id, session.ChatInput{
			Prompt:               "carry on",
			Delivery:             session.DeliveryPrefillOnly,
			ResumeAgentSessionID: agentSessionID,
		}, "reopened codex chat", session.HookOpts{})
		if err != nil {
			t.Fatalf("resume #%d: StartTmuxChat: %v", i, err)
		}
		if gotID != agentSessionID {
			t.Fatalf("resume #%d: returned id = %q, want the reused id %q", i, gotID, agentSessionID)
		}
		// Prove the relaunch actually ran codex rather than short-circuiting on
		// a still-live pane: the fake TUI records every argv it was invoked
		// with, and each round must add one more `codex resume <provider id>`.
		argv, err := os.ReadFile(argvLog)
		if err != nil {
			t.Fatalf("resume #%d: read fake codex argv log: %v", i, err)
		}
		if got := strings.Count(string(argv), "resume\n"+providerSessionID+"\n"); got != i {
			t.Fatalf("resume #%d: fake codex saw %d `resume %s` invocations, want %d; log:\n%s",
				i, got, providerSessionID, i, argv)
		}

		after, err := h.AgentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil {
			t.Fatalf("resume #%d: GetByAgentSessionID: %v", i, err)
		}
		t.Cleanup(func() {
			if after.TmuxSessionName != nil {
				_ = h.Tmux.KillSession(context.Background(), *after.TmuxSessionName)
			}
		})

		// The row was updated in place, not deleted and rebuilt.
		if after.ID != chatID {
			t.Errorf("resume #%d: chat id = %q, want %q (the row must be updated, not re-created)",
				i, after.ID, chatID)
		}
		// The reported defect, verbatim.
		if after.AgentName != "codex" {
			t.Errorf("resume #%d: agent_name = %q, want codex (a resumed codex chat came back as claude)",
				i, after.AgentName)
		}
		if after.ProviderSessionID == nil || *after.ProviderSessionID != providerSessionID {
			got := "<nil>"
			if after.ProviderSessionID != nil {
				got = *after.ProviderSessionID
			}
			t.Errorf("resume #%d: provider_session_id = %q, want %q preserved",
				i, got, providerSessionID)
		}
		// The other two columns the re-create recomputed from the session.
		if after.Model != chatModel {
			t.Errorf("resume #%d: model = %q, want %q preserved (not re-derived from the session)",
				i, after.Model, chatModel)
		}
		if after.AccountID == nil || *after.AccountID != chatAccountID {
			got := "<nil>"
			if after.AccountID != nil {
				got = *after.AccountID
			}
			t.Errorf("resume #%d: account_id = %q, want %q preserved (not re-derived from the session)",
				i, got, chatAccountID)
		}

		// Exactly one row still carries the id -- the UNIQUE index is in force
		// and the resume left no duplicate behind.
		chats, err := h.AgentChats.ListBySession(ctx, sess.Id)
		if err != nil {
			t.Fatalf("resume #%d: list chats: %v", i, err)
		}
		var carrying int
		for _, c := range chats {
			if c.AgentSessionID == agentSessionID {
				carrying++
			}
		}
		if carrying != 1 {
			t.Errorf("resume #%d: %d rows carry agent_session_id %q, want exactly 1",
				i, carrying, agentSessionID)
		}
	}
}
