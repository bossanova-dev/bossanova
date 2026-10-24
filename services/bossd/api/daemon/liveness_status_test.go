package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// livenessTestChatID is deliberately distinct from the waiting tests' "agent-a"
// so the shared chatEntryByID helper is exercised with more than one id — the
// lookup must select by id, not return whatever entry happens to be first.
const livenessTestChatID = "agent-liveness"

// GetChatStatuses must carry the poller's spinner-aware liveness reading
// (BOS-805) out to the caller. Without the projection the daemon knows the
// difference between real work and a spinner redraw and no consumer can see it,
// which is the whole point of the ticket.
func TestGetChatStatuses_ProjectsLivenessSignals(t *testing.T) {
	substantiveAt := time.Now().Add(-40 * time.Minute)

	tests := []struct {
		name string
		// setLiveness is nil when the poller has recorded nothing for this chat.
		setLiveness        func(t *testing.T, s *Server)
		wantSpinnerPresent bool
		wantSubstantive    bool
		wantSeeded         bool
	}{
		{
			name: "working chat with a live spinner and stale substantive output",
			setLiveness: func(_ *testing.T, s *Server) {
				s.chatStatus.SetLiveness(livenessTestChatID, true, substantiveAt, false)
			},
			wantSpinnerPresent: true,
			wantSubstantive:    true,
			wantSeeded:         false,
		},
		{
			name: "freshly registered chat reports its timestamp as a seed",
			setLiveness: func(_ *testing.T, s *Server) {
				s.chatStatus.SetLiveness(livenessTestChatID, false, substantiveAt, true)
			},
			wantSpinnerPresent: false,
			wantSubstantive:    true,
			wantSeeded:         true,
		},
		{
			name: "no spinner and no substantive observation yet",
			setLiveness: func(_ *testing.T, s *Server) {
				s.chatStatus.SetLiveness(livenessTestChatID, false, time.Time{}, false)
			},
			wantSpinnerPresent: false,
			wantSubstantive:    false,
			wantSeeded:         false,
		},
		{
			name:               "chat the poller has never reported on",
			setLiveness:        nil,
			wantSpinnerPresent: false,
			wantSubstantive:    false,
			wantSeeded:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newWaitingStatusServer(t, livenessTestChatID)
			if tt.setLiveness != nil {
				tt.setLiveness(t, s)
			}

			resp, err := s.GetChatStatuses(context.Background(), connect.NewRequest(&pb.GetChatStatusesRequest{
				SessionId: "sess-1",
			}))
			if err != nil {
				t.Fatalf("GetChatStatuses: %v", err)
			}
			entry := chatEntryByID(resp.Msg.GetStatuses(), livenessTestChatID)
			if entry == nil {
				t.Fatal("no status entry for the liveness chat")
			}

			if got := entry.GetSpinnerPresent(); got != tt.wantSpinnerPresent {
				t.Errorf("spinner_present = %v, want %v", got, tt.wantSpinnerPresent)
			}
			if got := entry.GetLastOutputSeeded(); got != tt.wantSeeded {
				t.Errorf("last_output_seeded = %v, want %v", got, tt.wantSeeded)
			}
			if tt.wantSubstantive {
				ts := entry.GetLastSubstantiveOutputAt()
				if ts == nil {
					t.Fatal("last_substantive_output_at is unset, want the recorded observation")
				}
				if !ts.AsTime().Equal(substantiveAt.UTC()) {
					t.Errorf("last_substantive_output_at = %v, want %v", ts.AsTime(), substantiveAt.UTC())
				}
			} else if entry.GetLastSubstantiveOutputAt() != nil {
				t.Errorf("last_substantive_output_at = %v, want unset", entry.GetLastSubstantiveOutputAt())
			}

			// AC5: last_output_at keeps its existing value semantics — the new
			// fields sit alongside it, never in place of it.
			if entry.GetLastOutputAt() == nil {
				t.Error("last_output_at is unset, want the heartbeat value unchanged")
			}
		})
	}
}
