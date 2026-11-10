package main

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/stretchr/testify/require"
)

func TestDependabotRepairSerialization_Sessions_AC248001_AC248004(t *testing.T) {
	for _, tc := range []struct {
		name, branch, repo                    string
		active, persisted, live, wantDeferred bool
	}{
		{"in flight", "dependabot/npm/b", "r", true, false, false, true},
		{"plugin restart", "dependabot/npm/b", "r", false, true, true, true},
		{"dead repair", "dependabot/npm/b", "r", false, true, false, false},
		{"plan running", "dependabot/npm/b", "r", false, false, true, true},
		{"other repo", "dependabot/npm/b", "other", true, false, false, false},
		{"feature branch", "feature/b", "r", true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newTestMock()
			holder := &pb.Session{Id: "holder", RepoId: "r", BranchName: "dependabot/npm/a", DisplayIsRepairing: tc.persisted, HasActiveChat: tc.live}
			if tc.live && !tc.persisted {
				holder.State = pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
			}
			host.sessions = []*pb.Session{holder, {Id: "candidate", RepoId: tc.repo, BranchName: tc.branch, State: pb.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "h"}}
			host.waitResp = &pb.WaitChatRunHostResponse{ExitError: "repair failed"}
			m := newTestMonitor(host)
			t.Cleanup(m.cancel)
			m.repairing["holder"] = tc.active
			m.maybeRepair("candidate", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
			m.wg.Wait()
			starts, _, _, _ := host.snapshot()
			if tc.wantDeferred {
				require.Zero(t, starts)
				require.Empty(t, host.recordOutcomeReqs)
				require.Empty(t, host.setRepairStatusReqs)
				require.Empty(t, m.cooldowns)
				delete(m.repairing, "holder")
				holder.DisplayIsRepairing = false
				holder.HasActiveChat = false
				holder.State = pb.SessionState_SESSION_STATE_AWAITING_CHECKS
				m.maybeRepair("candidate", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
				m.wg.Wait()
				starts, _, _, _ = host.snapshot()
			}
			require.Equal(t, 1, starts)
		})
	}
}

func TestDependabotRepairsReleaseSlot_Sessions_AC248001(t *testing.T) {
	host := newTestMock()
	host.sessions = []*pb.Session{
		{Id: "holder", RepoId: "r", BranchName: "dependabot/npm/a", State: pb.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "a"},
		{Id: "candidate", RepoId: "r", BranchName: "dependabot/npm/b", State: pb.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "b"},
	}
	started, finish := make(chan struct{}), make(chan struct{})
	host.startFunc = func(req *pb.StartChatRunHostRequest) (*pb.StartChatRunHostResponse, error) {
		if req.GetSessionId() == "holder" {
			close(started)
		}
		return &pb.StartChatRunHostResponse{AgentSessionId: req.GetSessionId() + "-run"}, nil
	}
	host.waitFunc = func(context.Context, *pb.WaitChatRunHostRequest) (*pb.WaitChatRunHostResponse, error) {
		<-finish
		return &pb.WaitChatRunHostResponse{ExitError: "repair failed"}, nil
	}
	m := newTestMonitor(host)
	t.Cleanup(m.cancel)
	m.maybeRepair("holder", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	<-started
	m.maybeRepair("candidate", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	starts, _, _, _ := host.snapshot()
	require.Equal(t, 1, starts)
	m.mu.Lock()
	_, attempted := m.cooldowns["candidate"]
	m.mu.Unlock()
	require.False(t, attempted)
	close(finish)
	m.wg.Wait()
	m.maybeRepair("candidate", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	m.wg.Wait()
	starts, _, _, _ = host.snapshot()
	require.Equal(t, 2, starts)
	require.Equal(t, "candidate", host.startReqs[1].GetSessionId())
}
