package status

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// --- mock ClaudeChatStore ---

type mockChatStore struct {
	mu    sync.Mutex
	chats map[string]*models.ClaudeChat
}

func (m *mockChatStore) Create(_ context.Context, _ db.CreateClaudeChatParams) (*models.ClaudeChat, error) {
	return nil, nil
}
func (m *mockChatStore) GetByClaudeID(_ context.Context, claudeID string) (*models.ClaudeChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.chats[claudeID]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return c, nil
}
func (m *mockChatStore) ListBySession(_ context.Context, _ string) ([]*models.ClaudeChat, error) {
	return nil, nil
}
func (m *mockChatStore) UpdateTitle(_ context.Context, _, _ string) error { return nil }
func (m *mockChatStore) UpdateTitleByClaudeID(_ context.Context, _, _ string) error {
	return nil
}
func (m *mockChatStore) UpdateTmuxSessionName(_ context.Context, _ string, _ *string) error {
	return nil
}
func (m *mockChatStore) DeleteByClaudeID(_ context.Context, _ string) error { return nil }
func (m *mockChatStore) ListWithTmuxSession(_ context.Context) ([]*models.ClaudeChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*models.ClaudeChat
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
	claudeID := "claude-1"

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "  Allow Claude to run this command?\n\n  ❯ Allow\n    Allow once\n    Deny\n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, zerolog.Nop())
	poller.RegisterChat(claudeID)
	poller.pollOnce(context.Background())

	entry := tracker.Get(claudeID)
	if entry == nil {
		t.Fatal("expected entry after poll")
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("expected QUESTION, got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_WorkingDetected(t *testing.T) {
	tracker := NewTracker()
	tmuxName := "boss-test-chat2"
	claudeID := "claude-2"

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "Working on some code changes...",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, zerolog.Nop())
	poller.RegisterChat(claudeID)
	poller.pollOnce(context.Background())

	entry := tracker.Get(claudeID)
	if entry == nil {
		t.Fatal("expected entry after poll")
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING, got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_IdleDetected(t *testing.T) {
	tracker := NewTracker()
	tmuxName := "boss-test-chat3"
	claudeID := "claude-3"
	content := "Some static content"

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{tmuxName: content},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, zerolog.Nop())

	// Simulate a previous capture that happened >5s ago with same content.
	// The content must match exactly what CapturePane returns (cat outputs
	// the file bytes verbatim, no trailing newline added).
	poller.mu.Lock()
	poller.prevCaptures[claudeID] = captureEntry{
		content: content,
		at:      time.Now().Add(-10 * time.Second),
	}
	poller.mu.Unlock()

	poller.pollOnce(context.Background())

	entry := tracker.Get(claudeID)
	if entry == nil {
		t.Fatal("expected entry after poll")
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Errorf("expected IDLE, got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_DeadSessionCleanup(t *testing.T) {
	tracker := NewTracker()
	claudeID := "claude-dead"
	tmuxName := "boss-test-dead"

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, TmuxSessionName: &tmuxName},
		},
	}

	// Tmux session is NOT alive.
	factory := &mockTmuxFactory{
		sessions: map[string]bool{},
		captures: map[string]string{},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, zerolog.Nop())
	poller.RegisterChat(claudeID)
	poller.pollOnce(context.Background())

	// Chat should have been cleaned up from prevCaptures.
	poller.mu.Lock()
	_, exists := poller.prevCaptures[claudeID]
	poller.mu.Unlock()
	if exists {
		t.Error("expected dead chat to be cleaned up from prevCaptures")
	}
}

func TestTmuxStatusPoller_RegisterUnregister(t *testing.T) {
	tracker := NewTracker()
	tmuxClient := tmux.NewClient()
	poller := NewTmuxStatusPoller(tracker, &mockChatStore{chats: map[string]*models.ClaudeChat{}}, nil, tmuxClient, zerolog.Nop())

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
	claudeID := "claude-boot-idle"

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "Some ordinary output from Claude",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(claudeID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Errorf("expected IDLE, got %v", entry.Status)
	}

	// Should also be registered in prevCaptures.
	poller.mu.Lock()
	_, exists := poller.prevCaptures[claudeID]
	poller.mu.Unlock()
	if !exists {
		t.Error("expected chat to be in prevCaptures after bootstrap")
	}
}

func TestTmuxStatusPoller_Bootstrap_QuestionDetected(t *testing.T) {
	tracker := NewTracker()
	tmuxName := "boss-boot-question"
	claudeID := "claude-boot-question"

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, TmuxSessionName: &tmuxName},
		},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{tmuxName: true},
		captures: map[string]string{
			tmuxName: "  Allow Claude to run this command?\n\n  ❯ Allow\n    Allow once\n    Deny\n",
		},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(claudeID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
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
	claudeID := "claude-boot-suppress"
	sessionID := "sess-boot-suppress"
	worktreePath := "/tmp/boss-boot-suppress-wt"

	home := t.TempDir()
	t.Setenv("HOME", home)

	projectDir := home + "/.claude/projects/" + pathToProjectKey(worktreePath)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	transcript := projectDir + "/" + claudeID + ".jsonl"
	userAnsweredFixture := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"proceed?"}]}}
{"type":"user","message":{"role":"user","content":"yes"}}
`
	if err := os.WriteFile(transcript, []byte(userAnsweredFixture), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, SessionID: sessionID, TmuxSessionName: &tmuxName},
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
	poller := NewTmuxStatusPoller(tracker, chatStore, sessionStore, tmuxClient, zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(claudeID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING (question suppressed, user already answered), got %v", entry.Status)
	}
}

func TestTmuxStatusPoller_Bootstrap_DeadSessionSkipped(t *testing.T) {
	tracker := NewTracker()
	tmuxName := "boss-boot-dead"
	claudeID := "claude-boot-dead"

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, TmuxSessionName: &tmuxName},
		},
	}

	// Tmux session is NOT alive.
	factory := &mockTmuxFactory{
		sessions: map[string]bool{},
		captures: map[string]string{},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(claudeID)
	if entry != nil {
		t.Errorf("expected no entry for dead session, got %v", entry.Status)
	}

	poller.mu.Lock()
	_, exists := poller.prevCaptures[claudeID]
	poller.mu.Unlock()
	if exists {
		t.Error("expected dead session to not be in prevCaptures")
	}
}

func TestTmuxStatusPoller_Bootstrap_NoChatsTmux(t *testing.T) {
	tracker := NewTracker()

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{},
	}

	factory := &mockTmuxFactory{
		sessions: map[string]bool{},
		captures: map[string]string{},
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(factory.factory))

	poller := NewTmuxStatusPoller(tracker, chatStore, nil, tmuxClient, zerolog.Nop())
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
	claudeID := "claude-suppress"
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
	transcript := projectDir + "/" + claudeID + ".jsonl"
	userAnsweredFixture := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"proceed?"}]}}
{"type":"user","message":{"role":"user","content":"yes"}}
`
	assistantQuestionFixture := `{"type":"user","message":{"role":"user","content":"start"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"proceed?"}]}}
`

	chatStore := &mockChatStore{
		chats: map[string]*models.ClaudeChat{
			claudeID: {ClaudeID: claudeID, SessionID: sessionID, TmuxSessionName: &tmuxName},
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
	poller := NewTmuxStatusPoller(tracker, chatStore, sessionStore, tmuxClient, zerolog.Nop())
	poller.RegisterChat(claudeID)
	poller.pollOnce(context.Background())

	if entry := tracker.Get(claudeID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING (question suppressed, user just answered), got %v", entry.Status)
	}

	// Case B: assistant still pending — transcript shows assistant last. Expect QUESTION.
	if err := os.WriteFile(transcript, []byte(assistantQuestionFixture), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	tracker2 := NewTracker()
	poller2 := NewTmuxStatusPoller(tracker2, chatStore, sessionStore, tmuxClient, zerolog.Nop())
	poller2.RegisterChat(claudeID)
	poller2.pollOnce(context.Background())

	if entry := tracker2.Get(claudeID); entry == nil {
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
	poller3 := NewTmuxStatusPoller(tracker3, chatStore, sessionStore, tmuxClient, zerolog.Nop())
	// Seed prevCaptures with the *same* question pane content but an old
	// timestamp — mirrors the "question was showing for a while" scenario.
	poller3.mu.Lock()
	poller3.prevCaptures[claudeID] = captureEntry{
		content: questionPane,
		at:      time.Now().Add(-2 * IdleThreshold),
	}
	poller3.mu.Unlock()
	poller3.pollOnce(context.Background())

	if entry := tracker3.Get(claudeID); entry == nil {
		t.Fatal("expected entry after poll")
	} else if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING after suppression with stale prev (not IDLE), got %v", entry.Status)
	}
}
