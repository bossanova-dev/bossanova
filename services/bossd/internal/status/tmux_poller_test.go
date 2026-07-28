package status

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/statusdetect"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status/questionsignal"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// claudeTranscriptPathForTest mirrors the path resolution that the real
// claude plugin performs. Duplicated here (rather than imported) because
// the daemon-side transcript helper has been deleted in D.7 and importing
// the plugin from a daemon test would create a layering violation.
func claudeTranscriptPathForTest(worktreePath, agentSessionID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	key := strings.NewReplacer("/", "-", ".", "-").Replace(worktreePath)
	return filepath.Join(home, ".claude", "projects", key, agentSessionID+".jsonl"), nil
}

// pathToProjectKey mirrors Claude Code's project-directory encoding: both "/"
// and "." become "-". Inlined here (rather than imported from the deleted
// daemon transcript helper) so the bootstrap+suppression test fixtures can
// continue to write JSONL files at the canonical path.
func pathToProjectKey(path string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(path)
}

// claudeLastTurnIsUserForTest re-implements the JSONL tail-reading the real
// claude plugin uses, scoped to test fixtures. Same semantics as the
// migrated lastTurnIsUser: returns true iff the most recent meaningful
// transcript entry is a user text turn.
func claudeLastTurnIsUserForTest(path string) bool {
	const tail = 32 * 1024
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}
	var offset int64
	if info.Size() > tail {
		offset = info.Size() - tail
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	if offset > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			return false
		}
		data = data[nl+1:]
	}
	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		switch entry.Type {
		case "assistant":
			return false
		case "user":
			if entry.Message.Role != "user" {
				continue
			}
			c := bytes.TrimSpace(entry.Message.Content)
			if len(c) == 0 {
				continue
			}
			switch c[0] {
			case '"':
				var s string
				if err := json.Unmarshal(c, &s); err == nil && strings.TrimSpace(s) != "" {
					return true
				}
			case '[':
				var blocks []struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(c, &blocks); err == nil {
					for _, b := range blocks {
						if b.Type == "text" {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// claudeFakeClient is a per-test stand-in for the real claude AgentRunnerClient.
// It mirrors what bossd-plugin-claude does in production: HasQuestionPrompt
// runs the shared statusdetect regex; LastTurnIsUser walks the JSONL transcript
// at ~/.claude/projects/<project_key>/<agentSessionID>.jsonl. By keeping this
// behaviour in a tiny test fake we avoid coupling tmux_poller_test to the
// claude plugin module while still preserving the existing pane/transcript
// fixtures these tests use.
type claudeFakeClient struct {
	hasPromptCalls atomic.Int64
	workingCalls   atomic.Int64
	lastTurnCalls  atomic.Int64
	title          string
	titleExplicit  bool
	titleCalls     atomic.Int64
	lastTitleReq   *pb.GetChatTitleRequest
	// limited, when true, makes DetectUsageLimit report a usage-cap banner.
	// limitResetAt (when non-zero) is surfaced as the reset_at timestamp.
	limited      bool
	limitResetAt time.Time
	limitCalls   atomic.Int64
}

func (c *claudeFakeClient) GetInfo(context.Context) (*pb.PluginInfo, error) {
	return &pb.PluginInfo{Name: "claude"}, nil
}
func (c *claudeFakeClient) StartRun(context.Context, *pb.StartAgentRunRequest) (*pb.StartAgentRunResponse, error) {
	return &pb.StartAgentRunResponse{}, nil
}
func (c *claudeFakeClient) StopRun(context.Context, *pb.StopAgentRunRequest) (*pb.StopAgentRunResponse, error) {
	return &pb.StopAgentRunResponse{}, nil
}
func (c *claudeFakeClient) IsRunning(context.Context, *pb.IsAgentRunningRequest) (*pb.IsAgentRunningResponse, error) {
	return &pb.IsAgentRunningResponse{}, nil
}
func (c *claudeFakeClient) ExitStatus(context.Context, *pb.AgentExitStatusRequest) (*pb.AgentExitStatusResponse, error) {
	return &pb.AgentExitStatusResponse{}, nil
}
func (c *claudeFakeClient) ConfigureFinalizeHook(context.Context, *pb.ConfigureFinalizeHookRequest) (*pb.ConfigureFinalizeHookResponse, error) {
	return &pb.ConfigureFinalizeHookResponse{}, nil
}
func (c *claudeFakeClient) RemoveAgentRunHook(context.Context, *pb.RemoveAgentRunHookRequest) (*pb.RemoveAgentRunHookResponse, error) {
	return &pb.RemoveAgentRunHookResponse{IsSupported: true}, nil
}
func (c *claudeFakeClient) BuildInteractiveCommand(context.Context, *pb.BuildInteractiveCommandRequest) (*pb.BuildInteractiveCommandResponse, error) {
	return &pb.BuildInteractiveCommandResponse{}, nil
}
func (c *claudeFakeClient) ResolveInteractiveSessionID(context.Context, *pb.ResolveInteractiveSessionIDRequest) (*pb.ResolveInteractiveSessionIDResponse, error) {
	return &pb.ResolveInteractiveSessionIDResponse{}, nil
}
func (c *claudeFakeClient) ListIgnoredDirtyFiles(context.Context, *pb.ListIgnoredDirtyFilesRequest) (*pb.ListIgnoredDirtyFilesResponse, error) {
	return &pb.ListIgnoredDirtyFilesResponse{}, nil
}
func (c *claudeFakeClient) SuggestPRTitle(context.Context, *pb.SuggestPRTitleRequest) (*pb.SuggestPRTitleResponse, error) {
	return &pb.SuggestPRTitleResponse{}, nil
}

func (c *claudeFakeClient) GetChatTitle(_ context.Context, req *pb.GetChatTitleRequest) (*pb.GetChatTitleResponse, error) {
	c.titleCalls.Add(1)
	c.lastTitleReq = &pb.GetChatTitleRequest{
		WorkDir:   req.GetWorkDir(),
		SessionId: req.GetSessionId(),
	}
	return &pb.GetChatTitleResponse{Supported: true, Title: c.title, Explicit: c.titleExplicit}, nil
}
func (c *claudeFakeClient) HasQuestionPrompt(_ context.Context, req *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	c.hasPromptCalls.Add(1)
	return &pb.HasQuestionPromptResponse{HasPrompt: statusdetect.HasQuestionPrompt(req.GetPaneContent())}, nil
}
func (c *claudeFakeClient) DetectUsageLimit(context.Context, *pb.DetectUsageLimitRequest) (*pb.DetectUsageLimitResponse, error) {
	c.limitCalls.Add(1)
	resp := &pb.DetectUsageLimitResponse{Limited: c.limited}
	if c.limited && !c.limitResetAt.IsZero() {
		resp.ResetAt = timestamppb.New(c.limitResetAt)
	}
	return resp, nil
}
func (c *claudeFakeClient) ProbeRateLimit(context.Context, *pb.ProbeRateLimitRequest) (*pb.ProbeRateLimitResponse, error) {
	return &pb.ProbeRateLimitResponse{}, nil
}
func (c *claudeFakeClient) HasWorkingIndicator(_ context.Context, req *pb.HasWorkingIndicatorRequest) (*pb.HasWorkingIndicatorResponse, error) {
	c.workingCalls.Add(1)
	return &pb.HasWorkingIndicatorResponse{IsWorking: statusdetect.HasWorkingIndicator(req.GetPaneContent())}, nil
}
func (c *claudeFakeClient) LastTurnIsUser(_ context.Context, req *pb.LastTurnIsUserRequest) (*pb.LastTurnIsUserResponse, error) {
	c.lastTurnCalls.Add(1)
	path, err := claudeTranscriptPathForTest(req.GetWorkDir(), req.GetAgentSessionId())
	if err != nil {
		return &pb.LastTurnIsUserResponse{}, nil
	}
	return &pb.LastTurnIsUserResponse{IsUser: claudeLastTurnIsUserForTest(path)}, nil
}
func (c *claudeFakeClient) TranscriptExists(_ context.Context, req *pb.TranscriptExistsRequest) (*pb.TranscriptExistsResponse, error) {
	path, err := claudeTranscriptPathForTest(req.GetWorkDir(), req.GetAgentSessionId())
	if err != nil {
		return &pb.TranscriptExistsResponse{}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return &pb.TranscriptExistsResponse{}, nil
	}
	return &pb.TranscriptExistsResponse{Exists: !info.IsDir() && info.Size() > 0}, nil
}
func (c *claudeFakeClient) ReadTranscript(_ context.Context, _ *pb.ReadTranscriptRequest) (*pb.ReadTranscriptResponse, error) {
	return &pb.ReadTranscriptResponse{}, nil
}
func (c *claudeFakeClient) RotationCapability(_ context.Context, _ *pb.RotationCapabilityRequest) (*pb.RotationCapabilityResponse, error) {
	return &pb.RotationCapabilityResponse{}, nil
}
func (c *claudeFakeClient) MaterializeAccount(_ context.Context, _ *pb.MaterializeAccountRequest) (*pb.MaterializeAccountResponse, error) {
	return &pb.MaterializeAccountResponse{}, nil
}

// claudeAgentClients is shorthand for the per-name registry expected by
// NewTmuxStatusPoller. Tests pass this for the common single-agent case.
func claudeAgentClients() map[string]agent.AgentRunnerClient {
	return map[string]agent.AgentRunnerClient{"claude": &claudeFakeClient{}}
}

type codexRecordingClient struct {
	claudeFakeClient
	lastTurnReq *pb.LastTurnIsUserRequest
}

func (c *codexRecordingClient) GetInfo(context.Context) (*pb.PluginInfo, error) {
	return &pb.PluginInfo{Name: "codex"}, nil
}

func (c *codexRecordingClient) HasQuestionPrompt(context.Context, *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	return &pb.HasQuestionPromptResponse{HasPrompt: true}, nil
}

func (c *codexRecordingClient) DetectUsageLimit(context.Context, *pb.DetectUsageLimitRequest) (*pb.DetectUsageLimitResponse, error) {
	return &pb.DetectUsageLimitResponse{}, nil
}

func (c *codexRecordingClient) ProbeRateLimit(context.Context, *pb.ProbeRateLimitRequest) (*pb.ProbeRateLimitResponse, error) {
	return &pb.ProbeRateLimitResponse{}, nil
}

func (c *codexRecordingClient) LastTurnIsUser(_ context.Context, req *pb.LastTurnIsUserRequest) (*pb.LastTurnIsUserResponse, error) {
	c.lastTurnReq = &pb.LastTurnIsUserRequest{
		WorkDir:        req.GetWorkDir(),
		AgentSessionId: req.GetAgentSessionId(),
	}
	return &pb.LastTurnIsUserResponse{}, nil
}

// --- mock AgentChatStore ---

type mockChatStore struct {
	mu    sync.Mutex
	chats map[string]*models.AgentChat
}

func (m *mockChatStore) Create(_ context.Context, _ db.CreateAgentChatParams) (*models.AgentChat, error) {
	return nil, nil
}
func (m *mockChatStore) GetByAgentSessionID(_ context.Context, agentSessionID string) (*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.chats[agentSessionID]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return c, nil
}
func (m *mockChatStore) ListBySession(_ context.Context, _ string) ([]*models.AgentChat, error) {
	return nil, nil
}
func (m *mockChatStore) ListBySessions(_ context.Context, _ []string) (map[string][]*models.AgentChat, error) {
	return nil, nil
}
func (m *mockChatStore) UpdateTitle(_ context.Context, _, _ string) error { return nil }
func (m *mockChatStore) UpdateTitleByAgentSessionID(_ context.Context, agentSessionID, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.chats[agentSessionID]
	if !ok {
		return fmt.Errorf("not found")
	}
	c.Title = title
	return nil
}
func (m *mockChatStore) UpdateAgentSessionID(_ context.Context, _ string, oldAgentSessionID, newAgentSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.chats[oldAgentSessionID]
	if !ok {
		return nil
	}
	delete(m.chats, oldAgentSessionID)
	c.AgentSessionID = newAgentSessionID
	m.chats[newAgentSessionID] = c
	return nil
}
func (m *mockChatStore) UpdateTmuxSessionName(_ context.Context, _ string, _ *string) error {
	return nil
}
func (m *mockChatStore) UpdateProviderSessionID(_ context.Context, _ string, _ *string) error {
	return nil
}
func (m *mockChatStore) UpdateAccountIDByAgentSessionID(_ context.Context, _ string, _ *string) error {
	return nil
}
func (m *mockChatStore) MarkStartFailed(_ context.Context, _, _ string) error     { return nil }
func (m *mockChatStore) DeleteByAgentSessionID(_ context.Context, _ string) error { return nil }
func (m *mockChatStore) ListWithTmuxSession(_ context.Context) ([]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*models.AgentChat
	for _, c := range m.chats {
		if c.TmuxSessionName != nil && *c.TmuxSessionName != "" {
			result = append(result, c)
		}
	}
	return result, nil
}

func (m *mockChatStore) ListRoutableChats(_ context.Context) ([]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*models.AgentChat
	for _, c := range m.chats {
		hasTmux := c.TmuxSessionName != nil && *c.TmuxSessionName != ""
		failed := c.StartError != nil && *c.StartError != ""
		if hasTmux || !failed {
			result = append(result, c)
		}
	}
	return result, nil
}

// --- mock SessionStore (only Get is exercised by the poller) ---

type mockSessionStore struct {
	sessions map[string]*models.Session
}

func (m *mockSessionStore) Create(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	return s, nil
}
func (m *mockSessionStore) List(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStore) ListActive(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStore) ListActiveWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	return nil, nil
}
func (m *mockSessionStore) ListWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	return nil, nil
}
func (m *mockSessionStore) ListByRepoAndPR(_ context.Context, _ string, _ int) ([]*db.SessionWithRepo, error) {
	return nil, nil
}
func (m *mockSessionStore) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStore) Update(_ context.Context, _ string, _ db.UpdateSessionParams) (*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStore) Archive(_ context.Context, _ string) error   { return nil }
func (m *mockSessionStore) Resurrect(_ context.Context, _ string) error { return nil }
func (m *mockSessionStore) Delete(_ context.Context, _ string) error    { return nil }
func (m *mockSessionStore) AdvanceOrphanedSessions(_ context.Context) (int64, error) {
	return 0, nil
}
func (m *mockSessionStore) ListByState(_ context.Context, _ int) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStore) ListByStates(_ context.Context, _ []int) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStore) UpdateStateConditionalFrom(_ context.Context, _ string, _ int, _ []int) (bool, error) {
	return false, nil
}
func (m *mockSessionStore) UpdateStateConditional(_ context.Context, _ string, _, _ int) (bool, error) {
	return false, nil
}
func (m *mockSessionStore) UpdateRepairDiagnostics(_ context.Context, _ db.UpdateRepairDiagnosticsParams) error {
	return nil
}

func (m *mockSessionStore) UpdateRepairBlocked(_ context.Context, _ string, _ time.Time, _ string) error {
	return nil
}

// --- mock tmux command factory ---
// Uses scripts that write to temp files to simulate tmux has-session and capture-pane.

type mockTmuxFactory struct {
	mu       sync.Mutex
	sessions map[string]bool   // session name -> alive
	captures map[string]string // session name -> pane content
	dead     map[string]bool   // session name -> pane_dead (remain-on-exit zombie)
	killed   map[string]bool   // session name -> kill-session was requested
}

func (f *mockTmuxFactory) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(args) > 0 {
		switch args[0] {
		case "has-session":
			// args = ["has-session", "-t", sessName]
			if len(args) >= 3 {
				sessName := args[2]
				if f.sessions[sessName] {
					return exec.CommandContext(ctx, "true")
				}
				return exec.CommandContext(ctx, "false")
			}
		case "display-message":
			// args = ["display-message", "-p", "-t", sessName, "#{pane_dead}"]
			var sessName string
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					sessName = args[i+1]
					break
				}
			}
			if f.dead[sessName] {
				return exec.CommandContext(ctx, "printf", "%s", "1\n")
			}
			return exec.CommandContext(ctx, "printf", "%s", "0\n")
		case "kill-session":
			// args = ["kill-session", "-t", sessName]
			if len(args) >= 3 {
				sessName := args[2]
				if f.killed == nil {
					f.killed = map[string]bool{}
				}
				f.killed[sessName] = true
				// The pane is now gone: reflect that so a later has-session probe
				// reports the reaped session absent.
				delete(f.sessions, sessName)
			}
			return exec.CommandContext(ctx, "true")
		case "capture-pane":
			// Find session name after "-t" flag (supports additional flags like -S -1000).
			var sessName string
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					sessName = args[i+1]
					break
				}
			}
			if sessName != "" {
				if content, ok := f.captures[sessName]; ok {
					// Write content to a temp file and cat it, so we get exact content.
					tmpFile, err := os.CreateTemp("", "tmux-capture-*")
					if err != nil {
						return exec.CommandContext(ctx, "false")
					}
					_, _ = tmpFile.WriteString(content)
					_ = tmpFile.Close()
					// Use a shell command that cats the file and cleans up.
					return exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("cat %q && rm -f %q", tmpFile.Name(), tmpFile.Name()))
				}
			}
			return exec.CommandContext(ctx, "false")
		case "-V":
			return exec.CommandContext(ctx, "echo", "tmux 3.4")
		}
	}
	return exec.CommandContext(ctx, "true")
}

func TestTmuxStatusPoller_QuestionDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-chat1"
	agentSessionID := "claude-1"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "  Allow Claude to run this command?\n\n  ❯ Allow\n    Allow once\n    Deny\n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after poll")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("expected QUESTION, got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_WorkingDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-chat2"
	agentSessionID := "claude-2"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "Working on some code changes...",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after poll")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING, got %v", entry.Status)
	}
}

func (f *mockTmuxFactory) wasKilled(sessName string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed[sessName]
}

// TestTmuxStatusPoller_DeadPaneCapturedAndReaped covers the BOS-477 death path:
// a chat whose pane is pane_dead (remain-on-exit zombie) has its final output
// captured into the tracker, its tmux session reaped, and its status set to
// STOPPED — so no zombie survives and the agent's own error is preserved.
func TestTmuxStatusPoller_DeadPaneCapturedAndReaped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-deadchat"
	agentSessionID := "claude-dead"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	// remain-on-exit keeps the session present (has-session true) but pane_dead,
	// with the agent's final error still on the pane.
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		dead:     map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "Error: Session ID claude-dead is already in use\n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	// The agent's final output is captured verbatim.
	if got := tracker.CapturedOutput(agentSessionID); !strings.Contains(got, "Session ID claude-dead is already in use") {
		t.Fatalf("CapturedOutput = %q, want the agent's final error", got)
	}
	// The dead pane is reaped so no zombie survives.
	if !factory.wasKilled(tmuxName) {
		t.Fatalf("expected the dead pane's tmux session to be killed (reaped)")
	}
	// Status is STOPPED.
	entry := tracker.Get(agentSessionID)
	if entry == nil || entry.Status != pb.ChatStatus_CHAT_STATUS_STOPPED {
		t.Fatalf("expected STOPPED entry after reap, got %v", entry)
	}
}

func TestTmuxStatusPoller_LimitedDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-limited1"
	agentSessionID := "claude-limited-1"
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "You've hit your usage limit.\n\n❯ "},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	fake := &claudeFakeClient{limited: true, limitResetAt: reset}
	clients := map[string]agent.AgentRunnerClient{"claude": fake}

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, clients, zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after poll")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Errorf("expected LIMITED, got %v", entry.Status)
	}
	if !entry.ResetAt.Equal(reset) {
		t.Errorf("expected ResetAt %v, got %v", reset, entry.ResetAt)
	}
}

func TestTmuxStatusPoller_LimitedRevertsWhenBannerLeaves(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-limited2"
	agentSessionID := "claude-limited-2"

	captures := map[string]string{tmuxName: "You've hit your usage limit.\n\n❯ "}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: captures,
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	fake := &claudeFakeClient{limited: true, limitResetAt: time.Now().Add(time.Hour)}
	clients := map[string]agent.AgentRunnerClient{"claude": fake}

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, clients, zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if entry := tracker.Get(agentSessionID); entry == nil || entry.Status != pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Fatalf("expected LIMITED on first poll, got %v", entry)
	}

	// Banner redraws away: the pane changes and DetectUsageLimit no longer
	// reports limited. The chat must fall through to WORKING (captureChanged).
	fake.limited = false
	captures[tmuxName] = "Working on the next step...\n"
	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after second poll")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING after banner leaves, got %v", entry.Status)
	}
	if !entry.ResetAt.IsZero() {
		t.Errorf("expected zero ResetAt after revert, got %v", entry.ResetAt)
	}
}

func TestTmuxStatusPoller_LimitedMissingClientNeverLimited(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-limited3"
	agentSessionID := "claude-limited-3"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "You've hit your usage limit.\n\n❯ "},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	// No client registered for "claude" — limitState must fail open.
	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, nil, zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after poll")
		return
	}
	if entry.Status == pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Errorf("missing client must never yield LIMITED, got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_Bootstrap_LimitedSeeded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-limited-boot"
	agentSessionID := "claude-limited-boot"
	reset := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "You've hit your usage limit.\n\n❯ "},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	fake := &claudeFakeClient{limited: true, limitResetAt: reset}
	clients := map[string]agent.AgentRunnerClient{"claude": fake}

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, clients, zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Errorf("expected LIMITED seeded on bootstrap, got %v", entry.Status)
	}
	if !entry.ResetAt.Equal(reset) {
		t.Errorf("expected ResetAt %v, got %v", reset, entry.ResetAt)
	}
}

func TestTmuxStatusPoller_RegisterChatEmptyPaneSeedsLastOutputAt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-empty"
	agentSessionID := "claude-empty"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: ""},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after poll")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING, got %v", entry.Status)
	}
	if entry.LastOutputAt.IsZero() {
		t.Fatal("LastOutputAt is zero")
	}

	poller.mu.Lock()
	prev := poller.prevCaptures[agentSessionID]
	poller.mu.Unlock()
	if prev.at.IsZero() {
		t.Fatal("prev capture timestamp is zero")
	}
	if !entry.LastOutputAt.Equal(prev.at) {
		t.Errorf("LastOutputAt = %v, want seeded capture time %v", entry.LastOutputAt, prev.at)
	}
}

func TestTmuxStatusPoller_IdleDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-chat3"
	agentSessionID := "claude-3"
	content := "Some static content"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: content},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())

	// Simulate a previous capture that happened >5s ago with same content.
	// The content must match exactly what CapturePane returns (cat outputs
	// the file bytes verbatim, no trailing newline added).
	lastOutputAt := time.Now().Add(-10 * time.Second)
	poller.mu.Lock()
	poller.prevCaptures[agentSessionID] = captureEntry{
		content: content,
		at:      lastOutputAt,
	}
	poller.mu.Unlock()

	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after poll")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Errorf("expected IDLE, got %v", entry.Status)
	}
	if !entry.LastOutputAt.Equal(lastOutputAt) {
		t.Errorf("LastOutputAt = %v, want previous capture time %v", entry.LastOutputAt, lastOutputAt)
	}
}

// TestTmuxStatusPoller_WorkingIndicatorKeepsWorking is the BOS-152 regression:
// a chat whose pane is UNCHANGED and aged past IdleThreshold, but whose content
// carries an "N shells still running" marker, must be reported WORKING, not
// IDLE. Without the working-indicator override this pane would flip to IDLE
// even though a background shell keeps the agent busy. It also proves the
// HasWorkingIndicator RPC is consulted in the would-be-idle branch.
func TestTmuxStatusPoller_WorkingIndicatorKeepsWorking(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-working-marker"
	agentSessionID := "claude-working-marker"
	content := "✻ Cooked for 48s · 2 shells still running"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: content},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	client := &claudeFakeClient{}
	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient,
		map[string]agent.AgentRunnerClient{"claude": client}, zerolog.Nop())

	// Previous capture with identical content >IdleThreshold ago: the pane is
	// static and would otherwise be classified IDLE.
	poller.mu.Lock()
	poller.prevCaptures[agentSessionID] = captureEntry{content: content, at: time.Now().Add(-10 * time.Second)}
	poller.mu.Unlock()

	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after poll")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING (background shell running), got %v", entry.Status)
	}
	if client.workingCalls.Load() == 0 {
		t.Error("expected HasWorkingIndicator to be consulted in the would-be-idle branch")
	}
}

// TestTmuxStatusPoller_Bootstrap_WorkingIndicatorSeedsWorking mirrors the
// pollOnce regression for daemon-restart recovery: a session restored while a
// background shell is running must seed WORKING, not IDLE.
func TestTmuxStatusPoller_Bootstrap_WorkingIndicatorSeedsWorking(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-boot-working-marker"
	agentSessionID := "claude-boot-working-marker"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "✻ Cooked for 12s · 1 shell still running"},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING (restored mid-background-shell), got %v", entry.Status)
	}
}

// TestTmuxStatusPoller_RefreshesPlaceholderChatTitle is the regression test
// for codex chats stuck at the "New chat" placeholder. The poller must
// dispatch GetChatTitle to the chat's AgentRunner plugin (not the
// Claude-only filesystem helper) and persist the extracted title back to
// the chat store so the web UI — which reads chat.title directly from the
// database — can render a real chat name. The session ID sent to the
// plugin must be the provider session ID (codex's rollout key), not the
// daemon-internal AgentSessionID.
func TestTmuxStatusPoller_RefreshesPlaceholderChatTitle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-title"
	agentSessionID := "codex-local"
	providerSessionID := "codex-provider"
	worktreePath := "/tmp/title-worktree"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {
				SessionID:         "sess-1",
				AgentSessionID:    agentSessionID,
				ProviderSessionID: &providerSessionID,
				AgentName:         "codex",
				Title:             "New chat",
				TmuxSessionName:   &tmuxName,
			},
		},
	}
	sessionStore := &mockSessionStore{
		sessions: map[string]*models.Session{
			"sess-1": {ID: "sess-1", WorktreePath: worktreePath},
		},
	}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "Working on descriptive title..."},
	}
	client := &claudeFakeClient{title: "Fix Codex chat names"}
	poller := NewTmuxStatusPoller(
		tracker,
		chatStore,
		sessionStore,
		tmux.NewClient(tmux.WithCommandFactory(factory.factory)),
		map[string]agent.AgentRunnerClient{"codex": client},
		zerolog.Nop(),
	)

	poller.pollOnce(context.Background())

	if got := chatStore.chats[agentSessionID].Title; got != "Fix Codex chat names" {
		t.Fatalf("chat title = %q, want extracted title", got)
	}
	if client.titleCalls.Load() != 1 {
		t.Fatalf("GetChatTitle calls = %d, want 1", client.titleCalls.Load())
	}
	if client.lastTitleReq.GetWorkDir() != worktreePath {
		t.Fatalf("GetChatTitle work_dir = %q, want %q", client.lastTitleReq.GetWorkDir(), worktreePath)
	}
	if client.lastTitleReq.GetSessionId() != providerSessionID {
		t.Fatalf("GetChatTitle session_id = %q, want provider id", client.lastTitleReq.GetSessionId())
	}
}

func TestRefreshChatTitle(t *testing.T) {
	tests := []struct {
		name           string
		currentTitle   string
		pluginTitle    string
		pluginExplicit bool
		wantTitle      string
		wantCalls      int64
	}{
		{
			name:         "placeholder backfilled by non-explicit title",
			currentTitle: "New chat",
			pluginTitle:  "First user prompt",
			wantTitle:    "First user prompt",
			wantCalls:    1,
		},
		{
			name:           "explicit rename overwrites non-placeholder title",
			currentTitle:   "Manual investigation",
			pluginTitle:    "the new name",
			pluginExplicit: true,
			wantTitle:      "the new name",
			wantCalls:      1,
		},
		{
			name:         "non-explicit title does not clobber non-placeholder title",
			currentTitle: "Manual investigation",
			pluginTitle:  "First user prompt",
			wantTitle:    "Manual investigation",
			wantCalls:    1,
		},
		{
			name:         "empty title preserves placeholder",
			currentTitle: "New chat",
			pluginTitle:  "",
			wantTitle:    "New chat",
			wantCalls:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentSessionID := "agent-session"
			chatStore := &mockChatStore{
				chats: map[string]*models.AgentChat{
					agentSessionID: {
						SessionID:      "sess-1",
						AgentSessionID: agentSessionID,
						AgentName:      "claude",
						Title:          tt.currentTitle,
					},
				},
			}
			sessionStore := &mockSessionStore{
				sessions: map[string]*models.Session{
					"sess-1": {ID: "sess-1", WorktreePath: "/tmp/title-worktree"},
				},
			}
			client := &claudeFakeClient{title: tt.pluginTitle, titleExplicit: tt.pluginExplicit}
			poller := NewTmuxStatusPoller(
				NewTracker(),
				chatStore,
				sessionStore,
				nil,
				map[string]agent.AgentRunnerClient{"claude": client},
				zerolog.Nop(),
			)

			poller.refreshChatTitle(context.Background(), chatStore.chats[agentSessionID])

			if got := chatStore.chats[agentSessionID].Title; got != tt.wantTitle {
				t.Fatalf("chat title = %q, want %q", got, tt.wantTitle)
			}
			if got := client.titleCalls.Load(); got != tt.wantCalls {
				t.Fatalf("GetChatTitle calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

// TestTmuxStatusPoller_DoesNotOverwriteCustomChatTitle ensures first-message
// title heuristics cannot overwrite a real title. The plugin is still asked so
// explicit agent rename lines can be discovered later.
func TestTmuxStatusPoller_DoesNotOverwriteCustomChatTitle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-title-custom"
	agentSessionID := "codex-custom"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {
				SessionID:       "sess-1",
				AgentSessionID:  agentSessionID,
				AgentName:       "codex",
				Title:           "Manual investigation",
				TmuxSessionName: &tmuxName,
			},
		},
	}
	sessionStore := &mockSessionStore{
		sessions: map[string]*models.Session{
			"sess-1": {ID: "sess-1", WorktreePath: "/tmp/title-worktree"},
		},
	}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "Working on descriptive title..."},
	}
	client := &claudeFakeClient{title: "Extracted title"}
	poller := NewTmuxStatusPoller(
		tracker,
		chatStore,
		sessionStore,
		tmux.NewClient(tmux.WithCommandFactory(factory.factory)),
		map[string]agent.AgentRunnerClient{"codex": client},
		zerolog.Nop(),
	)

	poller.pollOnce(context.Background())

	if got := chatStore.chats[agentSessionID].Title; got != "Manual investigation" {
		t.Fatalf("chat title = %q, want custom title preserved", got)
	}
	if client.titleCalls.Load() != 1 {
		t.Fatalf("GetChatTitle calls = %d, want 1", client.titleCalls.Load())
	}
}

// TestTmuxStatusPoller_LeavesTitleWhenPluginReturnsEmpty guards against
// wiping a placeholder before the user has typed their first message:
// codex's chatTitle returns "" until the rollout file contains a real
// user_message envelope, and overwriting "New chat" with "" would be a
// regression.
func TestTmuxStatusPoller_LeavesTitleWhenPluginReturnsEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-title-empty"
	agentSessionID := "codex-empty"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {
				SessionID:       "sess-1",
				AgentSessionID:  agentSessionID,
				AgentName:       "codex",
				Title:           "New chat",
				TmuxSessionName: &tmuxName,
			},
		},
	}
	sessionStore := &mockSessionStore{
		sessions: map[string]*models.Session{
			"sess-1": {ID: "sess-1", WorktreePath: "/tmp/title-worktree"},
		},
	}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "Pre-prompt..."},
	}
	client := &claudeFakeClient{title: ""}
	poller := NewTmuxStatusPoller(
		tracker,
		chatStore,
		sessionStore,
		tmux.NewClient(tmux.WithCommandFactory(factory.factory)),
		map[string]agent.AgentRunnerClient{"codex": client},
		zerolog.Nop(),
	)

	poller.pollOnce(context.Background())

	if got := chatStore.chats[agentSessionID].Title; got != "New chat" {
		t.Fatalf("chat title = %q, want placeholder preserved when plugin returns empty", got)
	}
	if client.titleCalls.Load() != 1 {
		t.Fatalf("GetChatTitle calls = %d, want 1", client.titleCalls.Load())
	}
}

func TestShouldRefreshChatTitle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tests := []struct {
		title string
		want  bool
	}{
		{"", true},
		{"   ", true},
		{"New chat", true},
		{"new chat", true},
		{"  New Chat  ", true},
		{"NEW CHAT", true},
		{"Fix Codex chat names", false},
		{"Working on something", false},
		{"new chat extended", false},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := shouldRefreshChatTitle(tt.title); got != tt.want {
				t.Errorf("shouldRefreshChatTitle(%q) = %v, want %v", tt.title, got, tt.want)
			}
		})
	}
}

func TestTmuxStatusPoller_DeadSessionCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	agentSessionID := "claude-dead"
	tmuxName := "boss-test-dead"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	// Tmux session is NOT alive.
	factory := &mockTmuxFactory{
		sessions: map[string]bool{},
		captures: map[string]string{},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	// Chat should have been cleaned up from prevCaptures.
	poller.mu.Lock()
	_, exists := poller.prevCaptures[agentSessionID]
	poller.mu.Unlock()
	if exists {
		t.Error("expected dead chat to be cleaned up from prevCaptures")
	}
}

func TestTmuxStatusPoller_DeadSessionStopsTracker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	agentSessionID := "claude-dead-working"
	tmuxName := "boss-test-dead-working"

	// Seed the exact stale-UI shape: a previous poll marked the chat working,
	// then the tmux session disappeared before the next display recompute.
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	var updates atomic.Int32
	tracker.SetOnUpdate(func(string) { updates.Add(1) })

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{},
		captures: map[string]string{},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected stopped tracker entry after dead tmux session")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_STOPPED {
		t.Errorf("tracker status = %v, want STOPPED", entry.Status)
	}
	if got := updates.Load(); got != 1 {
		t.Errorf("status-update hook calls = %d, want 1", got)
	}
}

// TestTmuxStatusPoller_RediscoversDroppedChat proves the poller is self-healing:
// a chat present in the DB with a live tmux session must be polled even when it
// is absent from prevCaptures. This guards the regression where a transient
// GetByAgentSessionID or HasSession failure would permanently exclude a chat
// from polling until daemon restart, leaving the UI showing IDLE while the pane
// actually displayed a question prompt.
func TestTmuxStatusPoller_RediscoversDroppedChat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-rediscover"
	agentSessionID := "claude-rediscover"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			// AgentName is required: the poller routes HasQuestionPrompt
			// per-agent via the agentClients map keyed by name. Without
			// it the chat is rediscovered but classified WORKING (the
			// fallback when no agent client can answer).
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "  Allow Claude to run this command?\n\n  ❯ Allow\n    Allow once\n    Deny\n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())

	// Deliberately do NOT call RegisterChat or Bootstrap. prevCaptures is empty
	// — the same state the poller ends up in if a transient error caused the
	// chat to be dropped from prevCaptures and never re-added.
	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after rediscovery poll, got nil — chat was not rediscovered from DB")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("expected QUESTION after rediscovery, got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_RegisterUnregister(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxClient := tmux.NewClient()
	poller := NewTmuxStatusPoller(tracker, &mockChatStore{chats: map[string]*models.AgentChat{}}, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())

	poller.RegisterChat("c1")
	poller.mu.Lock()
	if _, ok := poller.prevCaptures["c1"]; !ok {
		t.Error("expected c1 in prevCaptures after register")
	}
	poller.mu.Unlock()

	poller.UnregisterChat("c1")
	poller.mu.Lock()
	if _, ok := poller.prevCaptures["c1"]; ok {
		t.Error("expected c1 removed from prevCaptures after unregister")
	}
	poller.mu.Unlock()
}

func TestTmuxStatusPoller_Bootstrap_IdleByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-boot-idle"
	agentSessionID := "claude-boot-idle"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "Some ordinary output from Claude",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Errorf("expected IDLE, got %v", entry.Status)
	}

	// Bootstrap has not observed any genuine agent output — it only captured
	// pre-existing (possibly stale) pane content on restart. LastOutputAt must
	// therefore be seeded in the past (>= IdleThreshold ago), NOT `now`, so a
	// stalled pane that survived the restart does not read as freshly active via
	// last_agent_activity_at. (ReceivedAt stays fresh — that is the heartbeat.)
	if entry.LastOutputAt.IsZero() {
		t.Fatal("expected a seeded LastOutputAt after bootstrap")
	}
	if age := time.Since(entry.LastOutputAt); age < IdleThreshold {
		t.Errorf("bootstrap LastOutputAt is only %v old, want >= IdleThreshold (%v) in the past", age, IdleThreshold)
	}

	// Should also be registered in prevCaptures.
	poller.mu.Lock()
	_, exists := poller.prevCaptures[agentSessionID]
	poller.mu.Unlock()
	if !exists {
		t.Error("expected chat to be in prevCaptures after bootstrap")
	}
}

func TestTmuxStatusPoller_Bootstrap_QuestionDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-boot-question"
	agentSessionID := "claude-boot-question"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "  Allow Claude to run this command?\n\n  ❯ Allow\n    Allow once\n    Deny\n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("expected QUESTION, got %v", entry.Status)
	}
}

// TestTmuxStatusPoller_Bootstrap_QuestionSuppressedReportsWorking proves that
// Bootstrap mirrors pollOnce's explicit WORKING branch for suppressed questions:
// when a tmux pane shows a question prompt but the transcript shows the user
// has already answered, Bootstrap must report WORKING (not IDLE) so the UI
// doesn't flash IDLE before the first poll corrects it.
func TestTmuxStatusPoller_Bootstrap_QuestionSuppressedReportsWorking(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tmuxName := "boss-boot-suppress"
	agentSessionID := "claude-boot-suppress"
	sessionID := "sess-boot-suppress"
	worktreePath := "/tmp/boss-boot-suppress-wt"

	home := t.TempDir()
	t.Setenv("HOME", home)

	projectDir := home + "/.claude/projects/" + pathToProjectKey(worktreePath)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	transcript := projectDir + "/" + agentSessionID + ".jsonl"
	userAnsweredFixture := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"proceed?"}]}}
{"type":"user","message":{"role":"user","content":"yes"}}
`
	if err := os.WriteFile(transcript, []byte(userAnsweredFixture), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", SessionID: sessionID, TmuxSessionName: &tmuxName},
		},
	}
	sessionStore := &mockSessionStore{
		sessions: map[string]*models.Session{
			sessionID: {ID: sessionID, WorktreePath: worktreePath},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "  Allow Claude to run this command?\n\n  ❯ Allow\n    Allow once\n    Deny\n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	tracker := NewTracker()
	poller := NewTmuxStatusPoller(tracker, chatStore, sessionStore, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING (question suppressed, user already answered), got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_LastTurnIsUserUsesProviderSessionID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tmuxName := "boss-provider-status"
	providerSessionID := "codex-real-id"
	sessionID := "sess-provider-status"
	worktreePath := "/tmp/boss-provider-status-wt"

	chat := &models.AgentChat{
		AgentSessionID:    "boss-chat-id",
		ProviderSessionID: &providerSessionID,
		AgentName:         "codex",
		SessionID:         sessionID,
		TmuxSessionName:   &tmuxName,
	}
	sessionStore := &mockSessionStore{sessions: map[string]*models.Session{
		sessionID: {ID: sessionID, WorktreePath: worktreePath},
	}}
	client := &codexRecordingClient{}
	poller := NewTmuxStatusPoller(NewTracker(), nil, sessionStore, nil, map[string]agent.AgentRunnerClient{
		"codex": client,
	}, zerolog.Nop())

	paneShowsQuestion, _ := poller.questionState(context.Background(), chat, "codex question")
	if !paneShowsQuestion {
		t.Fatal("expected pane question prompt")
	}
	if client.lastTurnReq == nil {
		t.Fatal("expected LastTurnIsUser request")
	}
	if got := client.lastTurnReq.GetAgentSessionId(); got != "codex-real-id" {
		t.Fatalf("LastTurnIsUser AgentSessionId = %q, want codex-real-id", got)
	}
	if got := client.lastTurnReq.GetWorkDir(); got != worktreePath {
		t.Fatalf("LastTurnIsUser WorkDir = %q, want %q", got, worktreePath)
	}
}

func TestTmuxStatusPoller_Bootstrap_DeadSessionSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-boot-dead"
	agentSessionID := "claude-boot-dead"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	// Tmux session is NOT alive.
	factory := &mockTmuxFactory{
		sessions: map[string]bool{},
		captures: map[string]string{},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry != nil {
		t.Errorf("expected no entry for dead session, got %v", entry.Status)
	}

	poller.mu.Lock()
	_, exists := poller.prevCaptures[agentSessionID]
	poller.mu.Unlock()
	if exists {
		t.Error("expected dead session to not be in prevCaptures")
	}
}

func TestTmuxStatusPoller_Bootstrap_NoChatsTmux(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{},
		captures: map[string]string{},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.Bootstrap(context.Background())

	// Should be a no-op — no entries in tracker or prevCaptures.
	poller.mu.Lock()
	captureCount := len(poller.prevCaptures)
	poller.mu.Unlock()
	if captureCount != 0 {
		t.Errorf("expected 0 captures, got %d", captureCount)
	}
}

// TestTmuxStatusPoller_QuestionSuppressedWhenUserAnswered proves the
// transcript-aware check: when HasQuestionPrompt matches the pane but the
// JSONL transcript's last meaningful turn is a user message, status is
// downgraded out of QUESTION and falls through to normal working detection.
func TestTmuxStatusPoller_QuestionSuppressedWhenUserAnswered(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tmuxName := "boss-test-suppress"
	agentSessionID := "claude-suppress"
	sessionID := "sess-suppress"
	worktreePath := "/tmp/boss-test-suppress-wt"

	// Redirect os.UserHomeDir() to a per-test HOME so transcriptPath()
	// resolves under t.TempDir() rather than the real ~/.claude.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Write the JSONL fixture at the path Claude Code would use.
	projectDir := home + "/.claude/projects/" + pathToProjectKey(worktreePath)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	transcript := projectDir + "/" + agentSessionID + ".jsonl"
	userAnsweredFixture := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"proceed?"}]}}
{"type":"user","message":{"role":"user","content":"yes"}}
`
	assistantQuestionFixture := `{"type":"user","message":{"role":"user","content":"start"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"proceed?"}]}}
`

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", SessionID: sessionID, TmuxSessionName: &tmuxName},
		},
	}
	sessionStore := &mockSessionStore{
		sessions: map[string]*models.Session{
			sessionID: {ID: sessionID, WorktreePath: worktreePath},
		},
	}

	questionPane := "  Allow Claude to run this command?\n\n  ❯ Allow\n    Allow once\n    Deny\n"
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: questionPane},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	// Case A: user has answered — transcript shows user last. Expect WORKING.
	if err := os.WriteFile(transcript, []byte(userAnsweredFixture), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	tracker := NewTracker()
	poller := NewTmuxStatusPoller(tracker, chatStore, sessionStore, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if entry := tracker.Get(agentSessionID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING (question suppressed, user just answered), got %v", entry.Status)
	}

	// Case B: assistant still pending — transcript shows assistant last. Expect QUESTION.
	if err := os.WriteFile(transcript, []byte(assistantQuestionFixture), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	tracker2 := NewTracker()
	poller2 := NewTmuxStatusPoller(tracker2, chatStore, sessionStore, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller2.RegisterChat(agentSessionID)
	poller2.pollOnce(context.Background())

	if entry := tracker2.Get(agentSessionID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("expected QUESTION (assistant last), got %v", entry.Status)
	}

	// Case C (regression guard): question has been showing long enough that
	// prev.at is older than IdleThreshold, THEN the user answers. Without the
	// explicit WORKING branch for suppressed questions, the unchanged content
	// plus stale timestamp would drop us straight to IDLE.
	if err := os.WriteFile(transcript, []byte(userAnsweredFixture), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	tracker3 := NewTracker()
	poller3 := NewTmuxStatusPoller(tracker3, chatStore, sessionStore, tmuxClient, claudeAgentClients(), zerolog.Nop())
	// Seed prevCaptures with the *same* question pane content but an old
	// timestamp — mirrors the "question was showing for a while" scenario.
	poller3.mu.Lock()
	poller3.prevCaptures[agentSessionID] = captureEntry{
		content: questionPane,
		at:      time.Now().Add(-2 * IdleThreshold),
	}
	poller3.mu.Unlock()
	poller3.pollOnce(context.Background())

	if entry := tracker3.Get(agentSessionID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING after suppression with stale prev (not IDLE), got %v", entry.Status)
	}
}

// TestTmuxStatusPoller_StructuredSignalYieldsQuestion — a fresh pending record
// in the question-signal store drives QUESTION even though the pane content
// would NOT trip the regex detector (structured signal is primary, BOS-485).
func TestTmuxStatusPoller_StructuredSignalYieldsQuestion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-structured"
	agentSessionID := "claude-structured"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}
	// Benign pane: the regex detector would NOT report a question here.
	fake := &claudeFakeClient{}
	if statusdetect.HasQuestionPrompt([]byte("just some normal output\n")) {
		t.Fatal("precondition failed: benign pane must not trip the question regex")
	}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "just some normal output\n"},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	store := questionsignal.NewStore(time.Minute)
	store.SetPending(agentSessionID, "claude-notification")

	// sessions=nil so questionState surfaces the structured signal unsuppressed.
	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient,
		map[string]agent.AgentRunnerClient{"claude": fake}, zerolog.Nop())
	poller.SetQuestionSignals(store)
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if entry := tracker.Get(agentSessionID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("expected QUESTION from structured signal, got %v", entry.Status)
	}
	// Structured signal is primary: the regex detector must NOT have been the
	// decider (it wasn't consulted at all when a record is present).
	if got := fake.hasPromptCalls.Load(); got != 0 {
		t.Errorf("HasQuestionPrompt called %d times; structured signal should short-circuit the regex path", got)
	}
}

// TestTmuxStatusPoller_RegexFallbackWithStoreButNoSignal — with a store wired
// but NO pending record, questionState uses the regex path exactly as before:
// a question pane → QUESTION, and HasQuestionPrompt IS consulted.
func TestTmuxStatusPoller_RegexFallbackWithStoreButNoSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-fallback"
	agentSessionID := "claude-fallback"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}
	fake := &claudeFakeClient{}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "  Allow Claude to run this command?\n\n  ❯ Allow\n    Allow once\n    Deny\n"},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	store := questionsignal.NewStore(time.Minute) // empty — no record for this chat
	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient,
		map[string]agent.AgentRunnerClient{"claude": fake}, zerolog.Nop())
	poller.SetQuestionSignals(store)
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if entry := tracker.Get(agentSessionID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("expected QUESTION via regex fallback, got %v", entry.Status)
	}
	if got := fake.hasPromptCalls.Load(); got == 0 {
		t.Error("HasQuestionPrompt should be consulted when no structured record is present")
	}
}

// TestTmuxStatusPoller_StructuredSignalClearedOnUserAnswer — a pending record
// plus a transcript whose last real turn is the user clears the record and
// reports WORKING (suppressed), mirroring the regex reconcile.
func TestTmuxStatusPoller_StructuredSignalClearedOnUserAnswer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tmuxName := "boss-test-structclear"
	agentSessionID := "claude-structclear"
	sessionID := "sess-structclear"
	worktreePath := "/tmp/boss-test-structclear-wt"

	home := t.TempDir()
	t.Setenv("HOME", home)
	projectDir := home + "/.claude/projects/" + pathToProjectKey(worktreePath)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	transcript := projectDir + "/" + agentSessionID + ".jsonl"
	userAnsweredFixture := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"proceed?"}]}}
{"type":"user","message":{"role":"user","content":"yes"}}
`
	if err := os.WriteFile(transcript, []byte(userAnsweredFixture), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", SessionID: sessionID, TmuxSessionName: &tmuxName},
		},
	}
	sessionStore := &mockSessionStore{
		sessions: map[string]*models.Session{sessionID: {ID: sessionID, WorktreePath: worktreePath}},
	}
	// Benign pane so only the structured record could assert a question.
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "just some normal output\n"},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	store := questionsignal.NewStore(time.Minute)
	store.SetPending(agentSessionID, "claude-notification")

	tracker := NewTracker()
	poller := NewTmuxStatusPoller(tracker, chatStore, sessionStore, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.SetQuestionSignals(store)
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if entry := tracker.Get(agentSessionID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING (user answered, question suppressed), got %v", entry.Status)
	}
	if _, ok := store.Get(agentSessionID); ok {
		t.Error("pending record should have been cleared once the user answered")
	}
}

// TestTmuxStatusPoller_StaleSignalAgesOut — a pending record older than the
// store TTL no longer asserts a question; the poller falls back to the regex
// path (benign pane → not QUESTION).
func TestTmuxStatusPoller_StaleSignalAgesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-stale"
	agentSessionID := "claude-stale"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "just some normal output\n"},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	// Controllable clock: record is set at t0, then time jumps past the TTL.
	clk := &pollerFakeClock{t: time.Unix(1000, 0)}
	store := questionsignal.NewStoreWithClock(30*time.Second, clk.now)
	store.SetPending(agentSessionID, "claude-notification")
	clk.advance(time.Hour) // well past TTL

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.SetQuestionSignals(store)
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if entry := tracker.Get(agentSessionID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status == pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Error("stale (aged-out) record should not assert QUESTION")
	}
}

// pollerFakeClock is a monotonic test clock for the TTL age-out test.
type pollerFakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *pollerFakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *pollerFakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingAgentClient is a per-test fake that records every HasQuestionPrompt
// call so the dispatch test can prove pollOnce routes by chat.AgentName.
// Returns the configured response without actually inspecting pane content.
type recordingAgentClient struct {
	name              string
	hasPromptResponse bool
	hasPromptCalls    atomic.Int64
}

func (c *recordingAgentClient) GetInfo(context.Context) (*pb.PluginInfo, error) {
	return &pb.PluginInfo{Name: c.name}, nil
}
func (c *recordingAgentClient) StartRun(context.Context, *pb.StartAgentRunRequest) (*pb.StartAgentRunResponse, error) {
	return &pb.StartAgentRunResponse{}, nil
}
func (c *recordingAgentClient) StopRun(context.Context, *pb.StopAgentRunRequest) (*pb.StopAgentRunResponse, error) {
	return &pb.StopAgentRunResponse{}, nil
}
func (c *recordingAgentClient) IsRunning(context.Context, *pb.IsAgentRunningRequest) (*pb.IsAgentRunningResponse, error) {
	return &pb.IsAgentRunningResponse{}, nil
}
func (c *recordingAgentClient) ExitStatus(context.Context, *pb.AgentExitStatusRequest) (*pb.AgentExitStatusResponse, error) {
	return &pb.AgentExitStatusResponse{}, nil
}
func (c *recordingAgentClient) ConfigureFinalizeHook(context.Context, *pb.ConfigureFinalizeHookRequest) (*pb.ConfigureFinalizeHookResponse, error) {
	return &pb.ConfigureFinalizeHookResponse{}, nil
}
func (c *recordingAgentClient) RemoveAgentRunHook(context.Context, *pb.RemoveAgentRunHookRequest) (*pb.RemoveAgentRunHookResponse, error) {
	return &pb.RemoveAgentRunHookResponse{IsSupported: true}, nil
}
func (c *recordingAgentClient) BuildInteractiveCommand(context.Context, *pb.BuildInteractiveCommandRequest) (*pb.BuildInteractiveCommandResponse, error) {
	return &pb.BuildInteractiveCommandResponse{}, nil
}
func (c *recordingAgentClient) ResolveInteractiveSessionID(context.Context, *pb.ResolveInteractiveSessionIDRequest) (*pb.ResolveInteractiveSessionIDResponse, error) {
	return &pb.ResolveInteractiveSessionIDResponse{}, nil
}
func (c *recordingAgentClient) ListIgnoredDirtyFiles(context.Context, *pb.ListIgnoredDirtyFilesRequest) (*pb.ListIgnoredDirtyFilesResponse, error) {
	return &pb.ListIgnoredDirtyFilesResponse{}, nil
}
func (c *recordingAgentClient) SuggestPRTitle(context.Context, *pb.SuggestPRTitleRequest) (*pb.SuggestPRTitleResponse, error) {
	return &pb.SuggestPRTitleResponse{}, nil
}

func (c *recordingAgentClient) GetChatTitle(context.Context, *pb.GetChatTitleRequest) (*pb.GetChatTitleResponse, error) {
	return &pb.GetChatTitleResponse{}, nil
}
func (c *recordingAgentClient) HasQuestionPrompt(_ context.Context, _ *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	c.hasPromptCalls.Add(1)
	return &pb.HasQuestionPromptResponse{HasPrompt: c.hasPromptResponse}, nil
}
func (c *recordingAgentClient) DetectUsageLimit(context.Context, *pb.DetectUsageLimitRequest) (*pb.DetectUsageLimitResponse, error) {
	return &pb.DetectUsageLimitResponse{}, nil
}
func (c *recordingAgentClient) ProbeRateLimit(context.Context, *pb.ProbeRateLimitRequest) (*pb.ProbeRateLimitResponse, error) {
	return &pb.ProbeRateLimitResponse{}, nil
}
func (c *recordingAgentClient) HasWorkingIndicator(_ context.Context, _ *pb.HasWorkingIndicatorRequest) (*pb.HasWorkingIndicatorResponse, error) {
	return &pb.HasWorkingIndicatorResponse{}, nil
}
func (c *recordingAgentClient) LastTurnIsUser(context.Context, *pb.LastTurnIsUserRequest) (*pb.LastTurnIsUserResponse, error) {
	return &pb.LastTurnIsUserResponse{}, nil
}
func (c *recordingAgentClient) TranscriptExists(context.Context, *pb.TranscriptExistsRequest) (*pb.TranscriptExistsResponse, error) {
	return &pb.TranscriptExistsResponse{}, nil
}
func (c *recordingAgentClient) ReadTranscript(context.Context, *pb.ReadTranscriptRequest) (*pb.ReadTranscriptResponse, error) {
	return &pb.ReadTranscriptResponse{}, nil
}
func (c *recordingAgentClient) RotationCapability(context.Context, *pb.RotationCapabilityRequest) (*pb.RotationCapabilityResponse, error) {
	return &pb.RotationCapabilityResponse{}, nil
}
func (c *recordingAgentClient) MaterializeAccount(context.Context, *pb.MaterializeAccountRequest) (*pb.MaterializeAccountResponse, error) {
	return &pb.MaterializeAccountResponse{}, nil
}

// TestPollOnceDispatchesQuestionPromptByAgent proves pollOnce routes
// HasQuestionPrompt to the AgentRunnerClient registered under each chat's
// AgentName — the per-agent dispatch that lets the daemon stay agnostic to
// claude/codex pane formats. With two clients in the registry and two chats
// (one per agent), each client's HasQuestionPrompt should fire exactly once.
func TestPollOnceDispatchesQuestionPromptByAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	claudeName := "boss-dispatch-claude"
	codexName := "boss-dispatch-codex"
	claudeAgentSessionID := "claude-dispatch"
	codexAgentSessionID := "codex-dispatch"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			claudeAgentSessionID: {AgentSessionID: claudeAgentSessionID, AgentName: "claude", TmuxSessionName: &claudeName},
			codexAgentSessionID:  {AgentSessionID: codexAgentSessionID, AgentName: "codex", TmuxSessionName: &codexName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{claudeName: true, codexName: true},
		captures: map[string]string{
			claudeName: "any pane content — recording client decides",
			codexName:  "any pane content — recording client decides",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	claudeClient := &recordingAgentClient{name: "claude"}
	codexClient := &recordingAgentClient{name: "codex"}

	clients := map[string]agent.AgentRunnerClient{
		"claude": claudeClient,
		"codex":  codexClient,
	}

	tracker := NewTracker()
	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, clients, zerolog.Nop())
	poller.RegisterChat(claudeAgentSessionID)
	poller.RegisterChat(codexAgentSessionID)
	poller.pollOnce(context.Background())

	if got := claudeClient.hasPromptCalls.Load(); got != 1 {
		t.Errorf("claude HasQuestionPrompt calls = %d, want 1", got)
	}
	if got := codexClient.hasPromptCalls.Load(); got != 1 {
		t.Errorf("codex HasQuestionPrompt calls = %d, want 1", got)
	}
}

// TestPollOnceMissingAgentClientFallsThroughToIdle proves a chat referencing
// an unloaded agent name still gets a status update (not a panic, not a
// stuck IDLE-forever loop). The pane is treated as not-a-question, so the
// chat falls through to working/idle detection based on capture diffs.
func TestPollOnceMissingAgentClientFallsThroughToIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	t.Parallel()
	tracker := NewTracker()
	tmuxName := "boss-missing-agent"
	agentSessionID := "ghost-1"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "ghost", TmuxSessionName: &tmuxName},
		},
	}
	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: "stable content"},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	// Seed prev capture so the unchanged-content path triggers IDLE.
	poller.mu.Lock()
	poller.prevCaptures[agentSessionID] = captureEntry{
		content: "stable content",
		at:      time.Now().Add(-2 * IdleThreshold),
	}
	poller.mu.Unlock()

	poller.pollOnce(context.Background())

	entry := tracker.Get(agentSessionID)
	if entry == nil {
		t.Fatal("expected entry after poll")
		return
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Errorf("expected IDLE for chat with missing agent client, got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_AuthFailedDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-authfail"
	agentSessionID := "claude-auth"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "Invalid API key · Please run /login\n❯ \n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if !tracker.AuthFailed(agentSessionID) {
		t.Error("expected auth-failed marker after polling a login-required pane")
	}
}

func TestTmuxStatusPoller_AuthFailedClearedOnNormalOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-authclear"
	agentSessionID := "claude-clear"
	// Pre-seed a stale auth marker; a normal pane must clear it.
	tracker.SetAuthFailed(agentSessionID, true)

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "Working on some code changes...",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if tracker.AuthFailed(agentSessionID) {
		t.Error("expected auth-failed marker cleared after a normal pane")
	}
}

// TestBoundedTail covers the BOS-477 tail bounding: short input is unchanged,
// long input is truncated to <= cap and starts at a whole-line boundary.
func TestBoundedTail(t *testing.T) {
	if got := boundedTail("short line\n", 4096); got != "short line\n" {
		t.Fatalf("short input mutated: %q", got)
	}

	// Build > cap of many short lines; the tail must be <= cap and not begin
	// mid-line (i.e. its first line equals a whole original line).
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "line-number-%05d\n", i)
	}
	got := boundedTail(sb.String(), 4096)
	if len(got) > 4096 {
		t.Fatalf("boundedTail len = %d, want <= 4096", len(got))
	}
	first := got
	if nl := strings.IndexByte(got, '\n'); nl >= 0 {
		first = got[:nl]
	}
	if !strings.HasPrefix(first, "line-number-") || len(first) != len("line-number-00000") {
		t.Fatalf("tail begins mid-line: first line = %q", first)
	}
}

// TestTmuxStatusPoller_TransientAPIErrorDetected covers the BOS-518 detect path:
// a pane whose turn ended on a 5xx API-error banner must set the marker from the
// same capture the poller already took for status.
func TestTmuxStatusPoller_TransientAPIErrorDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-transient"
	agentSessionID := "claude-transient"

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "⏺ Reading the file...\n\nAPI Error: 502 Bad Gateway\n\n❯ \n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if !tracker.TransientAPIError(agentSessionID) {
		t.Error("expected transient-API-error marker after polling a 502 banner pane")
	}
}

// TestTmuxStatusPoller_TransientAPIErrorClearedOnNormalOutput proves the marker
// self-clears the moment the pane no longer ends on the banner (e.g. the agent
// retried and moved on), so a stale flag can never trigger an auto-resume.
func TestTmuxStatusPoller_TransientAPIErrorClearedOnNormalOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-transient-clear"
	agentSessionID := "claude-transient-clear"
	// Pre-seed the marker; a healthy pane must clear it.
	tracker.SetTransientAPIError(agentSessionID, true)

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "Working on some code changes...",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if tracker.TransientAPIError(agentSessionID) {
		t.Error("expected transient-API-error marker cleared after a healthy pane")
	}
}

// TestTmuxStatusPoller_TransientAPIErrorClearedWhenSessionGone covers the STOPPED
// path: a chat whose tmux session vanished can't be resumed from a banner it is
// no longer showing, so the marker must be cleared alongside the auth marker.
func TestTmuxStatusPoller_TransientAPIErrorClearedWhenSessionGone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux status poller test in -short; run make test-bossd for coverage")
	}
	tracker := NewTracker()
	tmuxName := "boss-test-transient-gone"
	agentSessionID := "claude-transient-gone"
	tracker.SetTransientAPIError(agentSessionID, true)

	chatStore := &mockChatStore{
		chats: map[string]*models.AgentChat{
			agentSessionID: {AgentSessionID: agentSessionID, AgentName: "claude", TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{},
		captures: map[string]string{},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, claudeAgentClients(), zerolog.Nop())
	poller.RegisterChat(agentSessionID)
	poller.pollOnce(context.Background())

	if tracker.TransientAPIError(agentSessionID) {
		t.Error("expected transient-API-error marker cleared when the tmux session is gone")
	}
}
