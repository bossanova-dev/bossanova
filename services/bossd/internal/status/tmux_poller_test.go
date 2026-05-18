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
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
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
	lastTurnCalls  atomic.Int64
	title          string
	titleCalls     atomic.Int64
	lastTitleReq   *pb.GetChatTitleRequest
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
func (c *claudeFakeClient) BuildInteractiveCommand(context.Context, *pb.BuildInteractiveCommandRequest) (*pb.BuildInteractiveCommandResponse, error) {
	return &pb.BuildInteractiveCommandResponse{}, nil
}
func (c *claudeFakeClient) ResolveInteractiveSessionID(context.Context, *pb.ResolveInteractiveSessionIDRequest) (*pb.ResolveInteractiveSessionIDResponse, error) {
	return &pb.ResolveInteractiveSessionIDResponse{}, nil
}
func (c *claudeFakeClient) ListIgnoredDirtyFiles(context.Context, *pb.ListIgnoredDirtyFilesRequest) (*pb.ListIgnoredDirtyFilesResponse, error) {
	return &pb.ListIgnoredDirtyFilesResponse{}, nil
}
func (c *claudeFakeClient) GetChatTitle(_ context.Context, req *pb.GetChatTitleRequest) (*pb.GetChatTitleResponse, error) {
	c.titleCalls.Add(1)
	c.lastTitleReq = &pb.GetChatTitleRequest{
		WorkDir:   req.GetWorkDir(),
		SessionId: req.GetSessionId(),
	}
	return &pb.GetChatTitleResponse{Supported: true, Title: c.title}, nil
}
func (c *claudeFakeClient) HasQuestionPrompt(_ context.Context, req *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	c.hasPromptCalls.Add(1)
	return &pb.HasQuestionPromptResponse{HasPrompt: statusdetect.HasQuestionPrompt(req.GetPaneContent())}, nil
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
func (m *mockChatStore) UpdateTmuxSessionName(_ context.Context, _ string, _ *string) error {
	return nil
}
func (m *mockChatStore) UpdateProviderSessionID(_ context.Context, _ string, _ *string) error {
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
func (m *mockSessionStore) UpdateStateConditional(_ context.Context, _ string, _, _ int) (bool, error) {
	return false, nil
}
func (m *mockSessionStore) UpdateRepairDiagnostics(_ context.Context, _ db.UpdateRepairDiagnosticsParams) error {
	return nil
}

// --- mock tmux command factory ---
// Uses scripts that write to temp files to simulate tmux has-session and capture-pane.

type mockTmuxFactory struct {
	mu       sync.Mutex
	sessions map[string]bool   // session name -> alive
	captures map[string]string // session name -> pane content
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

func TestTmuxStatusPoller_IdleDetected(t *testing.T) {
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
	poller.mu.Lock()
	poller.prevCaptures[agentSessionID] = captureEntry{
		content: content,
		at:      time.Now().Add(-10 * time.Second),
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

// TestTmuxStatusPoller_DoesNotOverwriteCustomChatTitle ensures we only
// refresh placeholder titles — once a real title is set (either by an
// earlier refresh or a user rename), the plugin must not be asked again.
func TestTmuxStatusPoller_DoesNotOverwriteCustomChatTitle(t *testing.T) {
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
	if client.titleCalls.Load() != 0 {
		t.Fatalf("GetChatTitle calls = %d, want 0", client.titleCalls.Load())
	}
}

// TestTmuxStatusPoller_LeavesTitleWhenPluginReturnsEmpty guards against
// wiping a placeholder before the user has typed their first message:
// codex's chatTitle returns "" until the rollout file contains a real
// user_message envelope, and overwriting "New chat" with "" would be a
// regression.
func TestTmuxStatusPoller_LeavesTitleWhenPluginReturnsEmpty(t *testing.T) {
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

	// Should also be registered in prevCaptures.
	poller.mu.Lock()
	_, exists := poller.prevCaptures[agentSessionID]
	poller.mu.Unlock()
	if !exists {
		t.Error("expected chat to be in prevCaptures after bootstrap")
	}
}

func TestTmuxStatusPoller_Bootstrap_QuestionDetected(t *testing.T) {
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
func (c *recordingAgentClient) BuildInteractiveCommand(context.Context, *pb.BuildInteractiveCommandRequest) (*pb.BuildInteractiveCommandResponse, error) {
	return &pb.BuildInteractiveCommandResponse{}, nil
}
func (c *recordingAgentClient) ResolveInteractiveSessionID(context.Context, *pb.ResolveInteractiveSessionIDRequest) (*pb.ResolveInteractiveSessionIDResponse, error) {
	return &pb.ResolveInteractiveSessionIDResponse{}, nil
}
func (c *recordingAgentClient) ListIgnoredDirtyFiles(context.Context, *pb.ListIgnoredDirtyFilesRequest) (*pb.ListIgnoredDirtyFilesResponse, error) {
	return &pb.ListIgnoredDirtyFilesResponse{}, nil
}
func (c *recordingAgentClient) GetChatTitle(context.Context, *pb.GetChatTitleRequest) (*pb.GetChatTitleResponse, error) {
	return &pb.GetChatTitleResponse{}, nil
}
func (c *recordingAgentClient) HasQuestionPrompt(_ context.Context, _ *pb.HasQuestionPromptRequest) (*pb.HasQuestionPromptResponse, error) {
	c.hasPromptCalls.Add(1)
	return &pb.HasQuestionPromptResponse{HasPrompt: c.hasPromptResponse}, nil
}
func (c *recordingAgentClient) LastTurnIsUser(context.Context, *pb.LastTurnIsUserRequest) (*pb.LastTurnIsUserResponse, error) {
	return &pb.LastTurnIsUserResponse{}, nil
}
func (c *recordingAgentClient) TranscriptExists(context.Context, *pb.TranscriptExistsRequest) (*pb.TranscriptExistsResponse, error) {
	return &pb.TranscriptExistsResponse{}, nil
}

// TestPollOnceDispatchesQuestionPromptByAgent proves pollOnce routes
// HasQuestionPrompt to the AgentRunnerClient registered under each chat's
// AgentName — the per-agent dispatch that lets the daemon stay agnostic to
// claude/codex pane formats. With two clients in the registry and two chats
// (one per agent), each client's HasQuestionPrompt should fire exactly once.
func TestPollOnceDispatchesQuestionPromptByAgent(t *testing.T) {
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
