package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/domain"
)

type freshPaneSendRecorder struct {
	*fakeTmuxClient
	hooks   []domain.SendHooks
	outcome domain.SubmitOutcome
}

func (f *freshPaneSendRecorder) SendMessageWithHooks(_ context.Context, _ string, _ string, _ bool, _ string, _ tmux.ModalDetector, hooks domain.SendHooks) error {
	f.hooks = append(f.hooks, hooks)
	if f.outcome == domain.OutcomeSubmitted {
		return nil
	}
	return &freshPaneDeliveryError{f.outcome}
}

type freshPaneDeliveryError struct{ outcome domain.SubmitOutcome }

func (e *freshPaneDeliveryError) Error() string                       { return "delivery refused" }
func (e *freshPaneDeliveryError) SubmitOutcome() domain.SubmitOutcome { return e.outcome }

func TestSendChatMessageFreshPane(t *testing.T) {
	for _, source := range []string{"launched", "woken", "established", "expired"} {
		t.Run(source, func(t *testing.T) {
			chat := &domain.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
			sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
			f := &fakeTmuxClient{available: true, hasSession: source != "woken"}
			s := newSendMessageTestServer(t, chat, sess, f)
			recorder := &freshPaneSendRecorder{fakeTmuxClient: f, outcome: domain.OutcomeNotSubmitted}
			s.wakeHook.spawner = recorder
			if source == "launched" || source == "expired" {
				launched := time.Now()
				if source == "expired" {
					launched = launched.Add(-11 * time.Minute)
				}
				s.recordInteractiveAgentRunStart(context.Background(), sess, chat, launched)
			}
			send := func() {
				_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{AgentSessionId: chat.AgentSessionID, Message: "/boss-verify 1", Submit: true, WakeIfAsleep: true}))
				if recorder.outcome == domain.OutcomeNotSubmitted && (err == nil || !strings.Contains(err.Error(), "DELIVERY_NOT_SUBMITTED")) {
					t.Fatalf("refusal = %v", err)
				}
				if recorder.outcome == domain.OutcomeSubmitted && err != nil {
					t.Fatal(err)
				}
			}
			send()
			send()
			recorder.outcome = domain.OutcomeSubmitted
			send()
			send()
			fresh := source == "launched" || source == "woken"
			for i, hooks := range recorder.hooks {
				if want := fresh && i < 3; hooks.FreshPane != want {
					t.Fatalf("send %d FreshPane=%t want %t", i, hooks.FreshPane, want)
				}
			}
		})
	}
}

func TestFreshPaneRetentionByOutcome(t *testing.T) {
	for _, outcome := range []domain.SubmitOutcome{domain.OutcomeNotSubmitted, domain.OutcomeBlockedByModal, domain.OutcomeComposerOccupied, domain.OutcomeQueued, domain.OutcomeUnconfirmed} {
		s := &Server{}
		s.markFreshChatPane("c", time.Now())
		s.settleFreshChatPane("c", true, &freshPaneDeliveryError{outcome})
		want := outcome == domain.OutcomeNotSubmitted || outcome == domain.OutcomeBlockedByModal || outcome == domain.OutcomeComposerOccupied
		if got := s.isFreshChatPane("c"); got != want {
			t.Fatalf("outcome %v kept=%t want %t", outcome, got, want)
		}
	}
	// A non-submit never consumes the launch fact.
	s := &Server{}
	s.markFreshChatPane("c", time.Now())
	s.settleFreshChatPane("c", false, nil)
	if !s.isFreshChatPane("c") {
		t.Fatal(errors.New("prefill consumed first submit"))
	}
}
