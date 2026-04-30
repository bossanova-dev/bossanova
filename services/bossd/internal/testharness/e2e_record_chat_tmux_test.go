package testharness_test

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/testharness"
)

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

// recordChatSetup creates the harness, registers a repo, and opens a session,
// returning the bits each test needs. Centralised so each scenario stays
// focused on the specific RecordChat behavior under test.
func recordChatSetup(t *testing.T, fake *fakeTmux) (*testharness.Harness, context.Context, string) {
	t.Helper()
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
	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Tmux RecordChat",
		Plan:   "test plan",
	})
	return h, ctx, sess.Id
}

// TestE2E_RecordChat_CreatesTmuxSession verifies that a fresh RecordChat
// (resume=false) drives `tmux new-session` with `claude --session-id <id>`
// and persists the resulting session name on the chat row + response.
func TestE2E_RecordChat_CreatesTmuxSession(t *testing.T) {
	fake := &fakeTmux{} // hasSess=false, so new-session must fire
	h, ctx, sessionID := recordChatSetup(t, fake)

	resp, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId: sessionID,
		ClaudeId:  "claude-fresh-001",
		Title:     "fresh chat",
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
	chat, err := h.ClaudeChats.GetByClaudeID(ctx, "claude-fresh-001")
	if err != nil {
		t.Fatalf("GetByClaudeID: %v", err)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != resp.Msg.Chat.TmuxSessionName {
		t.Errorf("DB tmux name = %v, response = %q — must match",
			chat.TmuxSessionName, resp.Msg.Chat.TmuxSessionName)
	}
}

// TestE2E_RecordChat_ResumePassesResumeFlag verifies that resume=true
// causes the daemon to invoke `claude --resume <id>` instead of
// `--session-id` when minting the tmux host.
func TestE2E_RecordChat_ResumePassesResumeFlag(t *testing.T) {
	fake := &fakeTmux{}
	h, ctx, sessionID := recordChatSetup(t, fake)

	_, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId: sessionID,
		ClaudeId:  "claude-resume-007",
		Title:     "resumed",
		Resume:    true,
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
	fake := &fakeTmux{}
	h, ctx, sessionID := recordChatSetup(t, fake)

	first, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId: sessionID,
		ClaudeId:  "claude-idem-042",
		Title:     "first",
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
		SessionId: sessionID,
		ClaudeId:  "claude-idem-042",
		Title:     "ignored second title",
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
	dbPath := t.TempDir() + "/bossd.db"

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
	sess := createSessionFromStream(t, h1.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Pre-restart",
		Plan:   "p",
	})
	preResp, err := h1.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId: sess.Id,
		ClaudeId:  "claude-restart-1",
		Title:     "first attach",
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
		SessionId: sess.Id,
		ClaudeId:  "claude-restart-1",
		Title:     "ignored on second call",
		Resume:    true,
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

// TestE2E_RecordChat_TmuxUnavailableSkipsCreate verifies the no-tmux fallback
// path: when tmux is not on PATH (or otherwise unavailable), RecordChat
// still creates the chat row but leaves tmux_session_name empty and does
// not error. This keeps the daemon usable in CI / headless environments
// without tmux.
func TestE2E_RecordChat_TmuxUnavailableSkipsCreate(t *testing.T) {
	fake := &fakeTmux{availErr: true}
	h, ctx, sessionID := recordChatSetup(t, fake)

	resp, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId: sessionID,
		ClaudeId:  "claude-no-tmux",
		Title:     "headless",
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
