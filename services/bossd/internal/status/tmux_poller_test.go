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

	poller := NewTmuxStatusPoller(tracker, chatStore, tmuxClient, zerolog.Nop())
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

	poller := NewTmuxStatusPoller(tracker, chatStore, tmuxClient, zerolog.Nop())
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

	poller := NewTmuxStatusPoller(tracker, chatStore, tmuxClient, zerolog.Nop())

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

	poller := NewTmuxStatusPoller(tracker, chatStore, tmuxClient, zerolog.Nop())
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
	poller := NewTmuxStatusPoller(tracker, &mockChatStore{chats: map[string]*models.ClaudeChat{}}, tmuxClient, zerolog.Nop())

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

	poller := NewTmuxStatusPoller(tracker, chatStore, tmuxClient, zerolog.Nop())
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

	poller := NewTmuxStatusPoller(tracker, chatStore, tmuxClient, zerolog.Nop())
	poller.Bootstrap(context.Background())

	entry := tracker.Get(claudeID)
	if entry == nil {
		t.Fatal("expected entry after bootstrap")
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("expected QUESTION, got %v", entry.Status)
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

	poller := NewTmuxStatusPoller(tracker, chatStore, tmuxClient, zerolog.Nop())
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

	poller := NewTmuxStatusPoller(tracker, chatStore, tmuxClient, zerolog.Nop())
	poller.Bootstrap(context.Background())

	// Should be a no-op — no entries in tracker or prevCaptures.
	poller.mu.Lock()
	captureCount := len(poller.prevCaptures)
	poller.mu.Unlock()
	if captureCount != 0 {
		t.Errorf("expected 0 captures, got %d", captureCount)
	}
}
