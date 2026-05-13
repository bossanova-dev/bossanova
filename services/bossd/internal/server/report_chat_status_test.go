package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

// TestReportChatStatus_IgnoresTmuxTrackedChat locks in the boss-CLI ↔
// daemon-poller arbitration: when a chat row has tmux_session_name set,
// the daemon's TmuxStatusPoller is the canonical status producer, and a
// concurrent boss heartbeat racing against it must not overwrite the
// tracker. Without this guard, both writers (each on a 3 s ticker) take
// turns overwriting each other and the session display label visibly
// flashes between "? question" and "draft" on every tick.
func TestReportChatStatus_IgnoresTmuxTrackedChat(t *testing.T) {
	tracker := status.NewTracker()
	tmuxName := "boss-test"
	chat := &models.AgentChat{
		AgentSessionID:  "abc",
		SessionID:       "sess",
		TmuxSessionName: &tmuxName,
	}
	s := &Server{
		chatStatus: tracker,
		agentChats: &chatStoreFake{chat: chat},
	}

	tracker.Update("abc", pb.ChatStatus_CHAT_STATUS_QUESTION, time.Now())

	if _, err := s.ReportChatStatus(context.Background(), connect.NewRequest(&pb.ReportChatStatusRequest{
		Reports: []*pb.ChatStatusReport{{
			AgentSessionId: "abc",
			Status:         pb.ChatStatus_CHAT_STATUS_STOPPED,
		}},
	})); err != nil {
		t.Fatalf("ReportChatStatus: %v", err)
	}

	got := tracker.Get("abc")
	if got == nil {
		t.Fatalf("tracker entry missing after report")
	} else if got.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Fatalf("expected QUESTION (daemon poller's value preserved), got %v", got.Status)
	}
}

// TestReportChatStatus_AcceptsNonTmuxChat keeps the legacy in-process boss
// PTY path working: chats without a tmux session row are not polled by the
// daemon's TmuxStatusPoller, so the boss-CLI heartbeat is the only writer
// and must populate the tracker.
func TestReportChatStatus_AcceptsNonTmuxChat(t *testing.T) {
	tracker := status.NewTracker()
	chat := &models.AgentChat{
		AgentSessionID: "xyz",
		SessionID:      "sess",
	}
	s := &Server{
		chatStatus: tracker,
		agentChats: &chatStoreFake{chat: chat},
	}

	if _, err := s.ReportChatStatus(context.Background(), connect.NewRequest(&pb.ReportChatStatusRequest{
		Reports: []*pb.ChatStatusReport{{
			AgentSessionId: "xyz",
			Status:         pb.ChatStatus_CHAT_STATUS_WORKING,
		}},
	})); err != nil {
		t.Fatalf("ReportChatStatus: %v", err)
	}

	got := tracker.Get("xyz")
	if got == nil {
		t.Fatalf("tracker entry missing after report")
	} else if got.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Fatalf("expected WORKING, got %v", got.Status)
	}
}
