//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
)

// requireTmux skips the test cleanly when the tmux binary is not on PATH —
// CI without tmux installed should not fail this suite, just skip.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

func realTmuxIntegrationClient(t *testing.T) *tmux.Client {
	t.Helper()
	requireTmux(t)
	label := fmt.Sprintf("bossd-integration-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", label, "kill-server").Run()
	})
	return tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "tmux" {
			args = append([]string{"-L", label}, args...)
		}
		return exec.CommandContext(ctx, name, args...)
	}))
}

func installFakeCodexTUI(t *testing.T, prefix string) string {
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
	t.Setenv("FAKE_CODEX_TUI_ID_PREFIX", prefix)
	t.Setenv("FAKE_CODEX_TUI_SLEEP_SECONDS", "30")
	return argvLog
}

func codexRolloutPath(home, id string) string {
	return filepath.Join(home, ".codex", "sessions", "2026", "05", "11", "rollout-2026-05-11T00-00-00-"+id+".jsonl")
}

func countCodexRollouts(t *testing.T, home string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		t.Fatalf("glob codex rollouts: %v", err)
	}
	return len(matches)
}

type codexTUITestClient struct{}

var _ agent.AgentRunnerClient = (*codexTUITestClient)(nil)

func (*codexTUITestClient) GetInfo(context.Context) (*pb.PluginInfo, error) {
	return &pb.PluginInfo{Name: "codex", Version: "test"}, nil
}
func (*codexTUITestClient) StartRun(context.Context, *pb.StartAgentRunRequest) (*pb.StartAgentRunResponse, error) {
	return &pb.StartAgentRunResponse{}, nil
}
func (*codexTUITestClient) StopRun(context.Context, *pb.StopAgentRunRequest) (*pb.StopAgentRunResponse, error) {
	return &pb.StopAgentRunResponse{}, nil
}
func (*codexTUITestClient) IsRunning(context.Context, *pb.IsAgentRunningRequest) (*pb.IsAgentRunningResponse, error) {
	return &pb.IsAgentRunningResponse{}, nil
}
func (*codexTUITestClient) ExitStatus(context.Context, *pb.AgentExitStatusRequest) (*pb.AgentExitStatusResponse, error) {
	return &pb.AgentExitStatusResponse{IsComplete: true}, nil
}
func (*codexTUITestClient) ConfigureFinalizeHook(context.Context, *pb.ConfigureFinalizeHookRequest) (*pb.ConfigureFinalizeHookResponse, error) {
	return &pb.ConfigureFinalizeHookResponse{IsSupported: false}, nil
}
func (*codexTUITestClient) BuildInteractiveCommand(_ context.Context, req *pb.BuildInteractiveCommandRequest) (*pb.BuildInteractiveCommandResponse, error) {
	args := []string{"codex"}
	if req.GetResume() {
		args = append(args, "resume", req.GetSessionId())
	}
	return &pb.BuildInteractiveCommandResponse{Argv: args}, nil
}
func (*codexTUITestClient) ResolveInteractiveSessionID(_ context.Context, req *pb.ResolveInteractiveSessionIDRequest) (*pb.ResolveInteractiveSessionIDResponse, error) {
	launchedAfter := time.Time{}
	if req.GetLaunchedAfter() != nil {
		launchedAfter = req.GetLaunchedAfter().AsTime()
	}
	id, ambiguous, reason := discoverCodexTUIRollout(req.GetWorkDir(), launchedAfter)
	return &pb.ResolveInteractiveSessionIDResponse{
		Found:     id != "",
		SessionId: id,
		Ambiguous: ambiguous,
		Reason:    reason,
	}, nil
}
func (*codexTUITestClient) ListIgnoredDirtyFiles(context.Context, *pb.ListIgnoredDirtyFilesRequest) (*pb.ListIgnoredDirtyFilesResponse, error) {
	return &pb.ListIgnoredDirtyFilesResponse{}, nil
}
func (*codexTUITestClient) GetChatTitle(context.Context, *pb.GetChatTitleRequest) (*pb.GetChatTitleResponse, error) {
	return &pb.GetChatTitleResponse{}, nil
}
func (*codexTUITestClient) HasQuestionPrompt(context.Context, *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	return &pb.HasQuestionPromptResponse{}, nil
}
func (*codexTUITestClient) LastTurnIsUser(context.Context, *pb.LastTurnIsUserRequest) (*pb.LastTurnIsUserResponse, error) {
	return &pb.LastTurnIsUserResponse{}, nil
}
func (*codexTUITestClient) TranscriptExists(_ context.Context, req *pb.TranscriptExistsRequest) (*pb.TranscriptExistsResponse, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return &pb.TranscriptExistsResponse{}, nil
	}
	info, err := os.Stat(codexRolloutPath(home, req.GetAgentSessionId()))
	return &pb.TranscriptExistsResponse{Exists: err == nil && !info.IsDir() && info.Size() > 0}, nil
}

type codexRolloutMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID         string `json:"id"`
		CWD        string `json:"cwd"`
		Originator string `json:"originator"`
	} `json:"payload"`
}

func discoverCodexTUIRollout(workDir string, launchedAfter time.Time) (string, bool, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, "no matching codex-tui rollout found"
	}
	notBefore := launchedAfter.Add(-2 * time.Second)
	var found string
	ambiguous := false
	root := filepath.Join(home, ".codex", "sessions")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || ambiguous {
			return nil
		}
		if matched, _ := filepath.Match("rollout-*.jsonl", filepath.Base(path)); !matched {
			return nil
		}
		info, err := d.Info()
		if err != nil || (!launchedAfter.IsZero() && info.ModTime().Before(notBefore)) {
			return nil
		}
		meta, ok := readCodexRolloutMeta(path)
		if !ok || meta.Payload.ID == "" || meta.Payload.Originator != "codex-tui" || !sameWorkDir(meta.Payload.CWD, workDir) {
			return nil
		}
		if found != "" && found != meta.Payload.ID {
			found = ""
			ambiguous = true
			return nil
		}
		found = meta.Payload.ID
		return nil
	})
	if ambiguous {
		return "", true, "multiple matching codex-tui rollouts found"
	}
	if found == "" {
		return "", false, "no matching codex-tui rollout found"
	}
	return found, false, ""
}

func readCodexRolloutMeta(path string) (codexRolloutMeta, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return codexRolloutMeta{}, false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	var meta codexRolloutMeta
	if err := json.Unmarshal([]byte(line), &meta); err != nil || meta.Type != "session_meta" {
		return codexRolloutMeta{}, false
	}
	return meta, true
}

func sameWorkDir(a, b string) bool {
	aa, errA := filepath.EvalSymlinks(a)
	if errA != nil {
		aa, _ = filepath.Abs(a)
	}
	bb, errB := filepath.EvalSymlinks(b)
	if errB != nil {
		bb, _ = filepath.Abs(b)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

type claudeIntegrationClient struct{}

var _ agent.AgentRunnerClient = (*claudeIntegrationClient)(nil)

func (*claudeIntegrationClient) GetInfo(context.Context) (*pb.PluginInfo, error) {
	return &pb.PluginInfo{Name: "claude", Version: "test"}, nil
}
func (*claudeIntegrationClient) StartRun(context.Context, *pb.StartAgentRunRequest) (*pb.StartAgentRunResponse, error) {
	return &pb.StartAgentRunResponse{}, nil
}
func (*claudeIntegrationClient) StopRun(context.Context, *pb.StopAgentRunRequest) (*pb.StopAgentRunResponse, error) {
	return &pb.StopAgentRunResponse{}, nil
}
func (*claudeIntegrationClient) IsRunning(context.Context, *pb.IsAgentRunningRequest) (*pb.IsAgentRunningResponse, error) {
	return &pb.IsAgentRunningResponse{}, nil
}
func (*claudeIntegrationClient) ExitStatus(context.Context, *pb.AgentExitStatusRequest) (*pb.AgentExitStatusResponse, error) {
	return &pb.AgentExitStatusResponse{IsComplete: true}, nil
}
func (*claudeIntegrationClient) ConfigureFinalizeHook(context.Context, *pb.ConfigureFinalizeHookRequest) (*pb.ConfigureFinalizeHookResponse, error) {
	return &pb.ConfigureFinalizeHookResponse{IsSupported: true}, nil
}
func (*claudeIntegrationClient) BuildInteractiveCommand(_ context.Context, req *pb.BuildInteractiveCommandRequest) (*pb.BuildInteractiveCommandResponse, error) {
	flag := "--session-id"
	if req.GetResume() {
		flag = "--resume"
	}
	return &pb.BuildInteractiveCommandResponse{Argv: []string{"claude", flag, req.GetSessionId()}}, nil
}
func (*claudeIntegrationClient) ResolveInteractiveSessionID(context.Context, *pb.ResolveInteractiveSessionIDRequest) (*pb.ResolveInteractiveSessionIDResponse, error) {
	return &pb.ResolveInteractiveSessionIDResponse{}, nil
}
func (*claudeIntegrationClient) ListIgnoredDirtyFiles(context.Context, *pb.ListIgnoredDirtyFilesRequest) (*pb.ListIgnoredDirtyFilesResponse, error) {
	return &pb.ListIgnoredDirtyFilesResponse{}, nil
}
func (*claudeIntegrationClient) GetChatTitle(context.Context, *pb.GetChatTitleRequest) (*pb.GetChatTitleResponse, error) {
	return &pb.GetChatTitleResponse{}, nil
}
func (*claudeIntegrationClient) HasQuestionPrompt(context.Context, *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	return &pb.HasQuestionPromptResponse{}, nil
}
func (*claudeIntegrationClient) LastTurnIsUser(context.Context, *pb.LastTurnIsUserRequest) (*pb.LastTurnIsUserResponse, error) {
	return &pb.LastTurnIsUserResponse{}, nil
}
func (*claudeIntegrationClient) TranscriptExists(_ context.Context, req *pb.TranscriptExistsRequest) (*pb.TranscriptExistsResponse, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return &pb.TranscriptExistsResponse{}, nil
	}
	projectDir := filepath.Join(home, ".claude", "projects", pathToProjectKey(req.GetWorkDir()))
	info, err := os.Stat(filepath.Join(projectDir, req.GetAgentSessionId()+".jsonl"))
	return &pb.TranscriptExistsResponse{Exists: err == nil && !info.IsDir() && info.Size() > 0}, nil
}

// buildStubClaude builds the testdata stub-claude binary and returns the
// absolute path of the resulting binary. The output lives in t.TempDir() so
// each test gets its own copy and cleanup is automatic.
func buildStubClaude(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "stub-claude")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = "testdata/stub-claude"
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub-claude: %v: %s", err, b)
	}
	return out
}

// promoteToClaude renames the stub binary to "claude" inside its parent
// directory and prepends that directory to PATH so the daemon's tmux exec
// of "claude" picks up the stub. t.Setenv automatically restores PATH at
// test cleanup.
func promoteToClaude(t *testing.T, stub string) {
	t.Helper()
	claudePath := filepath.Join(filepath.Dir(stub), "claude")
	if err := os.Rename(stub, claudePath); err != nil {
		t.Fatalf("rename stub: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(claudePath)+":"+os.Getenv("PATH"))
}

// pathToProjectKey mirrors Claude Code's project-directory encoding (both
// "/" and "." become "-"). Duplicated here rather than imported from
// internal/status to keep this integration test honest about the on-disk
// layout it depends on.
func pathToProjectKey(path string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(path)
}

// TestWakeChatIntegration_FreshFallback_NoTranscript exercises the
// fresh-fallback branch end-to-end against a real tmux server and the
// stub-claude binary. With no transcript on disk, WakeChat must spawn a
// tmux session running `claude --session-id <id>`.
func TestWakeChatIntegration_FreshFallback_NoTranscript(t *testing.T) {
	requireTmux(t)
	stub := buildStubClaude(t)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	promoteToClaude(t, stub)
	// Keep the stub alive long enough that HasSession can observe the
	// tmux session before the stub exits and tmux tears it down.
	t.Setenv("STUB_CLAUDE_TICK_MS", "30000")

	wd := t.TempDir()
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-fresh"}
	sess := &models.Session{ID: "s1", RepoID: "r123", WorktreePath: wd}
	tmuxClient := tmux.NewClient()
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		tmux:       tmuxClient,
		agentClients: map[string]agent.AgentRunnerClient{
			"claude": &claudeIntegrationClient{},
		},
	}
	tmuxName := tmux.ChatSessionName("r123", "agent-fresh")
	t.Cleanup(func() {
		_ = tmuxClient.KillSession(context.Background(), tmuxName)
	})

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{AgentSessionId: "agent-fresh"}))
	if err != nil {
		t.Fatalf("WakeChat: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want FRESH_FALLBACK", resp.Msg.Outcome)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tmuxClient.HasSession(context.Background(), tmuxName) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !tmuxClient.HasSession(context.Background(), tmuxName) {
		t.Fatalf("tmux session %q never appeared", tmuxName)
	}
}

// TestWakeChatIntegration_ResumedWhenTranscriptExists pre-creates a
// transcript file at the path the live oracle (status.TranscriptExists)
// inspects and verifies that WakeChat reports OUTCOME_RESUMED.
func TestWakeChatIntegration_ResumedWhenTranscriptExists(t *testing.T) {
	requireTmux(t)
	stub := buildStubClaude(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	promoteToClaude(t, stub)
	t.Setenv("STUB_CLAUDE_TICK_MS", "30000")

	wd := t.TempDir()
	// Pre-create the transcript so the pre-flight stat returns true.
	projectDir := filepath.Join(tmpHome, ".claude", "projects", pathToProjectKey(wd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "agent-resume.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-resume"}
	sess := &models.Session{ID: "s1", RepoID: "r123", WorktreePath: wd}
	tmuxClient := tmux.NewClient()
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		tmux:       tmuxClient,
		agentClients: map[string]agent.AgentRunnerClient{
			"claude": &claudeIntegrationClient{},
		},
	}
	tmuxName := tmux.ChatSessionName("r123", "agent-resume")
	t.Cleanup(func() {
		_ = tmuxClient.KillSession(context.Background(), tmuxName)
	})

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{AgentSessionId: "agent-resume"}))
	if err != nil {
		t.Fatalf("WakeChat: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_RESUMED {
		t.Fatalf("got %v, want RESUMED", resp.Msg.Outcome)
	}
}

func TestWakeChatIntegration_CodexRealTmuxResumeAndMissingTranscriptFallback(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	argvLog := installFakeCodexTUI(t, "wake-codex")
	tmuxClient := realTmuxIntegrationClient(t)

	wd := t.TempDir()
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "logical-codex-chat", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r123", WorktreePath: wd}
	store := &chatStoreFake{chat: chat}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		tmux:       tmuxClient,
		agentClients: map[string]agent.AgentRunnerClient{
			"codex": &codexTUITestClient{},
		},
	}
	tmuxName := tmux.ChatSessionName("r123", "logical-codex-chat")
	t.Cleanup(func() {
		_ = tmuxClient.KillSession(context.Background(), tmuxName)
	})

	freshResp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{AgentSessionId: "logical-codex-chat"}))
	if err != nil {
		t.Fatalf("WakeChat fresh: %v", err)
	}
	if freshResp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("fresh outcome = %v, want FRESH_FALLBACK", freshResp.Msg.Outcome)
	}
	if store.updateProvider == nil || *store.updateProvider == "" {
		t.Fatal("fresh wake did not persist discovered provider_session_id")
	}
	firstProviderID := *store.updateProvider
	firstRollout := codexRolloutPath(tmpHome, firstProviderID)
	if _, err := os.Stat(firstRollout); err != nil {
		t.Fatalf("fresh rollout missing: %v", err)
	}
	if count := countCodexRollouts(t, tmpHome); count != 1 {
		t.Fatalf("fresh wake created %d rollouts, want 1", count)
	}

	if err := tmuxClient.KillSession(context.Background(), tmuxName); err != nil {
		t.Fatalf("kill tmux before resume wake: %v", err)
	}
	beforeResumeRollouts := countCodexRollouts(t, tmpHome)
	resumeResp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{AgentSessionId: "logical-codex-chat"}))
	if err != nil {
		t.Fatalf("WakeChat resume: %v", err)
	}
	if resumeResp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_RESUMED {
		t.Fatalf("resume outcome = %v, want RESUMED", resumeResp.Msg.Outcome)
	}
	logBytes, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	if !strings.Contains(string(logBytes), "resume\n"+firstProviderID+"\n") {
		t.Fatalf("fake codex did not receive resume %q; argv log:\n%s", firstProviderID, string(logBytes))
	}
	if afterResumeRollouts := countCodexRollouts(t, tmpHome); afterResumeRollouts != beforeResumeRollouts {
		t.Fatalf("resume wake created new rollout: before=%d after=%d", beforeResumeRollouts, afterResumeRollouts)
	}

	if err := tmuxClient.KillSession(context.Background(), tmuxName); err != nil {
		t.Fatalf("kill tmux before fallback wake: %v", err)
	}
	if err := os.Remove(firstRollout); err != nil {
		t.Fatalf("delete rollout: %v", err)
	}
	fallbackResp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{AgentSessionId: "logical-codex-chat"}))
	if err != nil {
		t.Fatalf("WakeChat fallback: %v", err)
	}
	if fallbackResp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("fallback outcome = %v, want FRESH_FALLBACK", fallbackResp.Msg.Outcome)
	}
	if store.updateProvider == nil || *store.updateProvider == "" || *store.updateProvider == firstProviderID {
		t.Fatalf("fallback provider_session_id = %v, want new id different from %q", store.updateProvider, firstProviderID)
	}
	if _, err := os.Stat(codexRolloutPath(tmpHome, *store.updateProvider)); err != nil {
		t.Fatalf("fallback rollout missing for new provider id: %v", err)
	}
}
