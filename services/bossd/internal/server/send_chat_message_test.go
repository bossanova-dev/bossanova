package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/tmux"
)

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
