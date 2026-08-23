package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
)

func withFastServerTurnStartObserver(t *testing.T) {
	t.Helper()
	oldBudget := status.TurnStartObservationBudget
	oldTick := status.TurnStartObservationTick
	status.TurnStartObservationBudget = 40 * time.Millisecond
	status.TurnStartObservationTick = time.Millisecond
	t.Cleanup(func() {
		status.TurnStartObservationBudget = oldBudget
		status.TurnStartObservationTick = oldTick
	})
}

func TestSendChatMessage_TurnStartDefaultPathUnchanged(t *testing.T) {
	withFastServerTurnStartObserver(t)
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	want := &pb.SendChatMessageResponse{
		TmuxSessionName: tmux.ChatSessionName("r1", "agent-1"),
		Delivered:       true,
		DeliveryState:   pb.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED,
	}

	start := time.Now()
	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "do the thing",
		Submit:         true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= status.TurnStartObservationBudget {
		t.Fatalf("default send took %v, want below observation budget %v", elapsed, status.TurnStartObservationBudget)
	}
	if !proto.Equal(resp.Msg, want) {
		t.Fatalf("response changed with should_observe_turn_start unset:\ngot  %v\nwant %v", resp.Msg, want)
	}
	if len(tmuxer.sentMessages) != 1 {
		t.Fatalf("sent messages = %d, want 1", len(tmuxer.sentMessages))
	}
	if tmuxer.sentMessages[0].beforeSubmitPresent {
		t.Fatal("default send carried a turn-start baseline hook")
	}
}

func TestSendChatMessage_TurnStartObserved(t *testing.T) {
	withFastServerTurnStartObserver(t)
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	baselineAt := time.Now().Add(-time.Minute)
	s.chatStatus.SetLiveness("agent-1", false, baselineAt, true)

	go func() {
		time.Sleep(5 * time.Millisecond)
		s.chatStatus.SetLiveness("agent-1", false, baselineAt.Add(time.Second), false)
	}()

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId:         "agent-1",
		Message:                "do the thing",
		Submit:                 true,
		ShouldObserveTurnStart: true,
		WakeIfAsleep:           false,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := resp.Msg.GetTurnStartState(); got != pb.SendChatMessageResponse_TURN_START_STATE_OBSERVED {
		t.Fatalf("turn_start_state = %v, want OBSERVED", got)
	}
	if resp.Msg.GetNoticeText() != "" {
		t.Fatalf("notice_text = %q, want empty for observed turn start", resp.Msg.GetNoticeText())
	}
	if len(tmuxer.sentMessages) != 1 || !tmuxer.sentMessages[0].beforeSubmitPresent {
		t.Fatalf("baseline hook recorded on sent messages = %+v, want one hooked send", tmuxer.sentMessages)
	}
}

func TestSendChatMessage_TurnStartNotObserved(t *testing.T) {
	withFastServerTurnStartObserver(t)
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	baselineAt := time.Now().Add(-time.Minute)
	s.chatStatus.SetLiveness("agent-1", false, baselineAt, false)

	go func() {
		time.Sleep(5 * time.Millisecond)
		s.chatStatus.SetLiveness("agent-1", false, baselineAt, false)
	}()

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId:         "agent-1",
		Message:                "do the thing",
		Submit:                 true,
		ShouldObserveTurnStart: true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := resp.Msg.GetTurnStartState(); got != pb.SendChatMessageResponse_TURN_START_STATE_NOT_OBSERVED {
		t.Fatalf("turn_start_state = %v, want NOT_OBSERVED", got)
	}
	if !strings.Contains(resp.Msg.GetNoticeText(), "do not resend") {
		t.Fatalf("notice_text = %q, want no-resend guidance", resp.Msg.GetNoticeText())
	}
	if !resp.Msg.GetDelivered() || resp.Msg.GetDeliveryState() != pb.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED {
		t.Fatalf("delivery changed: delivered=%v delivery_state=%v", resp.Msg.GetDelivered(), resp.Msg.GetDeliveryState())
	}
	if len(tmuxer.sentMessages) != 1 {
		t.Fatalf("sent messages = %d, want 1", len(tmuxer.sentMessages))
	}
	if !tmuxer.sentMessages[0].beforeSubmitPresent {
		t.Fatal("should_observe_turn_start submit did not carry a baseline hook")
	}
}

func TestSendChatMessage_TurnStartBaselineCapturedAtSubmit(t *testing.T) {
	withFastServerTurnStartObserver(t)
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	baselineAt := time.Now().Add(-time.Minute)
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	s.chatStatus.SetLiveness("agent-1", false, baselineAt, false)
	tmuxer.beforeSubmitSideEffect = func() {
		s.chatStatus.SetLiveness("agent-1", false, baselineAt.Add(time.Second), false)
	}

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId:         "agent-1",
		Message:                "do the thing",
		Submit:                 true,
		ShouldObserveTurnStart: true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := resp.Msg.GetTurnStartState(); got != pb.SendChatMessageResponse_TURN_START_STATE_UNOBSERVABLE {
		t.Fatalf("turn_start_state = %v, want UNOBSERVABLE for output before submit", got)
	}
	if !strings.Contains(resp.Msg.GetNoticeText(), "do not resend") {
		t.Fatalf("notice_text = %q, want no-resend guidance", resp.Msg.GetNoticeText())
	}
}

func TestSendChatMessage_TurnStartUnobservable(t *testing.T) {
	withFastServerTurnStartObserver(t)
	t.Run("nil tracker", func(t *testing.T) {
		chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
		sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
		tmuxer := &fakeTmuxClient{available: true, hasSession: true}
		s := newSendMessageTestServer(t, chat, sess, tmuxer)
		s.chatStatus = nil
		resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
			AgentSessionId:         "agent-1",
			Message:                "do the thing",
			Submit:                 true,
			ShouldObserveTurnStart: true,
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := resp.Msg.GetTurnStartState(); got != pb.SendChatMessageResponse_TURN_START_STATE_UNOBSERVABLE {
			t.Fatalf("turn_start_state = %v, want UNOBSERVABLE", got)
		}
	})

	t.Run("prefill", func(t *testing.T) {
		chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
		sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
		tmuxer := &fakeTmuxClient{available: true, hasSession: true}
		s := newSendMessageTestServer(t, chat, sess, tmuxer)
		resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
			AgentSessionId:         "agent-1",
			Message:                "prefill only",
			Submit:                 false,
			ShouldObserveTurnStart: true,
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := resp.Msg.GetTurnStartState(); got != pb.SendChatMessageResponse_TURN_START_STATE_UNOBSERVABLE {
			t.Fatalf("turn_start_state = %v, want UNOBSERVABLE", got)
		}
		if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED {
			t.Fatalf("delivery_state = %v, want UNSPECIFIED", got)
		}
	})

	t.Run("queued", func(t *testing.T) {
		resp, err := newQueuedTestServer(t, queuedPane, true).SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
			AgentSessionId:         "agent-1",
			Message:                queuedPayload,
			Submit:                 true,
			ShouldObserveTurnStart: true,
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_QUEUED {
			t.Fatalf("delivery_state = %v, want QUEUED", got)
		}
		if got := resp.Msg.GetTurnStartState(); got != pb.SendChatMessageResponse_TURN_START_STATE_UNOBSERVABLE {
			t.Fatalf("turn_start_state = %v, want UNOBSERVABLE", got)
		}
		if !strings.Contains(resp.Msg.GetNoticeText(), "do not resend") {
			t.Fatalf("notice_text = %q, want no-resend guidance", resp.Msg.GetNoticeText())
		}
	})
}

func TestSendChatMessage_TurnStartUnconfirmedIsUnobservable(t *testing.T) {
	withFastServerTurnStartObserver(t)
	s := newVerifierTestServer(t, verifierPaneFactory(func() *exec.Cmd {
		return exec.CommandContext(context.Background(), "false")
	}))
	s.chatStatus = status.NewTracker()
	s.chatStatus.SetLiveness("agent-1", false, time.Now().Add(-time.Minute), false)

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId:         "agent-1",
		Message:                "do the thing",
		Submit:                 true,
		ShouldObserveTurnStart: true,
	}))
	if err != nil {
		t.Fatalf("expected a response, got error: %v", err)
	}
	if got := resp.Msg.GetDeliveryState(); got != pb.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED {
		t.Fatalf("delivery_state = %v, want UNCONFIRMED", got)
	}
	if got := resp.Msg.GetTurnStartState(); got != pb.SendChatMessageResponse_TURN_START_STATE_UNOBSERVABLE {
		t.Fatalf("turn_start_state = %v, want UNOBSERVABLE", got)
	}
	if !strings.Contains(resp.Msg.GetNoticeText(), "do not resend") {
		t.Fatalf("notice_text = %q, want no-resend guidance", resp.Msg.GetNoticeText())
	}
}
