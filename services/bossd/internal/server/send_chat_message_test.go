package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
)

// errTestDelivery is a canned delivery failure for exercising the error path.
var errTestDelivery = errors.New("delivery failed")

// newSendMessageTestServer wires a Server with the minimum surface SendChatMessage needs.
func newSendMessageTestServer(t *testing.T, chat *models.AgentChat, sess *models.Session, tmuxer *fakeTmuxClient) *Server {
	t.Helper()
	return &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        claudeArgvBuilder(),
		},
	}
}

func TestSendChatMessage_NotFound_UnknownChat(t *testing.T) {
	s := newSendMessageTestServer(t, nil, nil, &fakeTmuxClient{available: true, hasSession: false})
	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "missing",
		Message:        "hello",
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", connect.CodeOf(err))
	}
}

func TestSendChatMessage_LiveChat_Delivers(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.Msg.Delivered {
		t.Error("Delivered should be true")
	}
	wantTmuxName := tmux.ChatSessionName("r1", "agent-1")
	if resp.Msg.TmuxSessionName != wantTmuxName {
		t.Errorf("TmuxSessionName = %q, want %q", resp.Msg.TmuxSessionName, wantTmuxName)
	}

	// Verify SendMessage was called with the right session name and message.
	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()

	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].sessionName != wantTmuxName {
		t.Errorf("SendMessage sessionName = %q, want %q", msgs[0].sessionName, wantTmuxName)
	}
	if msgs[0].text != "do the thing" {
		t.Errorf("SendMessage text = %q, want %q", msgs[0].text, "do the thing")
	}
}

func TestSendChatMessage_LiveChat_HonorsPersistedTmuxName(t *testing.T) {
	// A legacy/relocated chat is live under a persisted name that differs from
	// the deterministic one. SendChatMessage must check liveness and deliver to
	// the persisted name, not the recomputed deterministic name.
	persisted := "boss-chat-legacy-name"
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", TmuxSessionName: &persisted}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if deterministic := tmux.ChatSessionName("r1", "agent-1"); deterministic == persisted {
		t.Fatalf("test precondition broken: persisted name %q equals deterministic name", persisted)
	}

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Msg.Delivered {
		t.Error("Delivered should be true")
	}
	if resp.Msg.TmuxSessionName != persisted {
		t.Errorf("TmuxSessionName = %q, want persisted %q", resp.Msg.TmuxSessionName, persisted)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].sessionName != persisted {
		t.Errorf("SendMessage sessionName = %q, want persisted %q", msgs[0].sessionName, persisted)
	}
}

func TestSendChatMessage_AsleepChat_WakeIfAsleep_WakesThenDelivers(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	// starts asleep
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "wake up and work",
		WakeIfAsleep:   true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Msg.Delivered {
		t.Error("Delivered should be true")
	}

	// Wake should have spawned a new session.
	tmuxer.mu.Lock()
	spawnCount := tmuxer.createdN
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()

	if spawnCount != 1 {
		t.Errorf("expected 1 spawn (wake), got %d", spawnCount)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call after wake, got %d", len(msgs))
	}
	if msgs[0].text != "wake up and work" {
		t.Errorf("SendMessage text = %q, want %q", msgs[0].text, "wake up and work")
	}
}

func TestSendChatMessage_AsleepChat_NoWake_FailedPrecondition(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "hello",
		WakeIfAsleep:   false,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got %v", connect.CodeOf(err))
	}
}

// TestSendChatMessage_HeadlessRunActive_FailedPrecondition guards against
// waking a tmux agent on a worktree whose headless run (codex exec / claude
// --print) is still in progress: the chat reports WORKING in the status
// tracker but has no tmux session, so wake_if_asleep must be refused (not
// spawn a second, competing agent on the same worktree).
func TestSendChatMessage_HeadlessRunActive_FailedPrecondition(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	s.chatStatus = status.NewTracker()
	s.chatStatus.Update("agent-1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "race me",
		WakeIfAsleep:   true,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got %v", connect.CodeOf(err))
	}

	// No tmux agent must have been spawned for the still-running headless run.
	tmuxer.mu.Lock()
	spawnCount := tmuxer.createdN
	tmuxer.mu.Unlock()
	if spawnCount != 0 {
		t.Errorf("expected 0 spawns while headless run active, got %d", spawnCount)
	}
}

// TestSendChatMessage_Submit_ThreadsSubmitAndClaudeMarker verifies the handler
// forwards submit=true and resolves the claude ready marker for a chat with no
// (legacy "") agent name.
func TestSendChatMessage_Submit_ThreadsSubmitAndClaudeMarker(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss-repair watch",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if !msgs[0].submit {
		t.Errorf("submit = false, want true")
	}
	if msgs[0].readyMarker != "❯" {
		t.Errorf("readyMarker = %q, want claude marker %q", msgs[0].readyMarker, "❯")
	}
}

// TestSendChatMessage_DefaultsToPrefill verifies that an omitted submit field
// defaults to false (prefill) — the behavior change the caller audit guards.
func TestSendChatMessage_DefaultsToPrefill(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "hello",
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].submit {
		t.Errorf("submit = true, want false (default prefill)")
	}
}

// TestSendChatMessage_CodexChat_UsesCodexMarker verifies the ready marker is
// resolved from the chat's agent so the submit path waits for codex's composer
// glyph, not claude's.
func TestSendChatMessage_CodexChat_UsesCodexMarker(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "codex"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/status",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmuxer.mu.Lock()
	msgs := append([]sentMessage(nil), tmuxer.sentMessages...)
	tmuxer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(msgs))
	}
	if msgs[0].readyMarker != "›" {
		t.Errorf("readyMarker = %q, want codex marker %q", msgs[0].readyMarker, "›")
	}
}

// TestSendChatMessage_DeliveryError_SurfacesInternal verifies a delivery/verify
// failure surfaces as a CodeInternal error rather than a silent "delivered".
func TestSendChatMessage_DeliveryError_SurfacesInternal(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true, sendMessageErr: errTestDelivery}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss-repair watch",
		Submit:         true,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("expected CodeInternal, got %v", connect.CodeOf(err))
	}
}

func TestSendChatMessage_WorktreeMissing_FailedPrecondition(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: "/nonexistent/path"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "hello",
		WakeIfAsleep:   true,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got %v", connect.CodeOf(err))
	}
}
