package plugin

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

func TestFireSessionEvent_UnblockClearsPRBlock(t *testing.T) {
	srv, sessions, id := newFireSessionEventServer(t, false)
	blocked, count, pr := int(machine.Blocked), 3, 123
	reason, head := "automatic repair could not start", "old-head"
	reasonPtr, headPtr := &reason, &head
	prPtr := &pr
	if _, err := sessions.Update(t.Context(), id, db.UpdateSessionParams{State: &blocked, AttemptCount: &count, PRNumber: &prPtr, BlockedReason: &reasonPtr, LastAttemptHeadSHA: &headPtr}); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.FireSessionEvent(t.Context(), &pb.FireSessionEventRequest{SessionId: id, Event: pb.SessionEvent_SESSION_EVENT_UNBLOCK})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetNewState() != machine.AwaitingChecks.String() {
		t.Fatalf("new state: %s", resp.GetNewState())
	}
	stored, err := sessions.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != machine.AwaitingChecks || stored.BlockedReason != nil || stored.AttemptCount != 0 || stored.LastAttemptHeadSHA != nil {
		t.Fatalf("stale block: %+v", stored)
	}
}

// TestListSessions_CarriesRepairGuardInputs pins the fields the repair plugin
// reads from this hand-built session: without BaseBranch it cannot read the
// base tip, so a conflicted head never retries after the base moves; without
// DisplayHasChangesRequested a verify hold with open review feedback reads as
// settled after a repair.
func TestListSessions_CarriesRepairGuardInputs(t *testing.T) {
	reason := "automatic repair could not start 5 times in a row"
	repoID := "repo-1"
	srv := newTestHostServiceServer(&mockVCSProvider{})
	srv.repoStore = &fakeRepoStore{
		repos: []*models.Repo{{ID: repoID, DisplayName: "Test Repo", OriginURL: "https://github.com/owner/repo"}},
	}
	srv.sessionStore = &fakeSessionStoreWithListActive{
		sessionsByRepo: map[string][]*models.Session{
			repoID: {{
				ID:                "sess-1",
				RepoID:            repoID,
				BranchName:        "feature",
				BaseBranch:        "release/v2",
				LastRepairHeadSHA: "head-1",
				LastRepairBaseSHA: "base-1",
				BlockedReason:     &reason,
			}},
		},
	}
	srv.displayTracker = status.NewDisplayTracker()
	srv.displayTracker.Set("sess-1", vcs.DisplayInfo{
		Status:              vcs.DisplayStatusVerifying,
		HasChangesRequested: true,
		HeadSHA:             "head-1",
	})
	srv.agentChats = &fakeAgentChatStore{}

	resp, err := srv.ListSessions(t.Context(), &pb.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(resp.GetSessions()) != 1 {
		t.Fatalf("expected 1 session, got %d", len(resp.GetSessions()))
	}
	s := resp.GetSessions()[0]
	if s.GetBlockedReason() != reason {
		t.Errorf("BlockedReason=%q", s.GetBlockedReason())
	}
	if got := s.GetBaseBranch(); got != "release/v2" {
		t.Errorf("BaseBranch=%q, want release/v2", got)
	}
	if got := s.GetLastRepairBaseSha(); got != "base-1" {
		t.Errorf("LastRepairBaseSha=%q, want base-1", got)
	}
	if !s.GetDisplayHasChangesRequested() {
		t.Error("DisplayHasChangesRequested=false, want true from the display tracker")
	}
	if got := s.GetPrDisplayHeadSha(); got != "head-1" {
		t.Errorf("PrDisplayHeadSha=%q, want head-1", got)
	}
}
