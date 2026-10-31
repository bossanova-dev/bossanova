package main

import (
	"context"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMaybeRepair_ReleasesStartBlock_Sessions_AC216401_AC216402_AC216403(t *testing.T) {
	for _, tc := range []struct {
		name, head, base, reason                string
		age                                     time.Duration
		refuseStart, refuseUnblock, wantRelease bool
	}{
		{name: "head moved", head: "head-b", base: "base-a", wantRelease: true},
		{name: "base moved", head: "head-a", base: "base-b", wantRelease: true},
		{name: "legacy head moved", head: "head-b", base: "base-a", reason: "automatic repair could not start 5 times in a row over 31m0s on head-a; last refusal: busy", wantRelease: true},
		{name: "due probe accepted", head: "head-a", base: "base-a", age: 16 * time.Minute, wantRelease: true},
		{name: "due probe refused", head: "head-a", base: "base-a", age: 16 * time.Minute, refuseStart: true, wantRelease: true},
		{name: "recent probe", head: "head-a", base: "base-a", age: time.Minute},
		{name: "unblock refused", head: "head-b", base: "base-a", refuseUnblock: true},
		{name: "failed attempt", head: "head-b", base: "base-b", reason: "automatic repair failed 5 times in a row on head-a; last error: boom"},
		{name: "base exhausted", head: "head-b", base: "base-b", reason: "automatic repair exhausted base moves; resolve conflict by hand"},
		{name: "fix exhausted", head: "head-b", base: "base-b", reason: "fix loop exhausted, needs human intervention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newTestMock()
			pr := int32(1)
			reason := tc.reason
			if reason == "" {
				reason = sessionreason.RepairCouldNotStart(5, 31*time.Minute, "head-a", "base-a", "chat busy")
			}
			mock.sessions = []*pb.Session{{Id: "s1", State: pb.SessionState_SESSION_STATE_BLOCKED, BlockedReason: &reason, PrDisplayHeadSha: tc.head, RepoOriginUrl: "https://github.com/o/r", BaseBranch: "main", PrNumber: &pr, LastRepairBlockedAt: timestamppb.New(time.Now().Add(-tc.age))}}
			mock.applySessionEvents = true
			mock.branchHeadSHA = tc.base
			if tc.refuseStart {
				mock.startErr = status.Error(codes.FailedPrecondition, "chat busy")
			}
			if tc.refuseUnblock {
				mock.fireEventErr = status.Error(codes.FailedPrecondition, "target held")
			}
			rm := newTestMonitor(mock)
			rm.maybeRepair("s1", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
			if tc.wantRelease {
				waitFor(t, func() bool { c, _, _, _ := mock.snapshot(); return c > 0 }, "repair started in same sweep")
				waitRepairDone(t, rm)
				mock.mu.Lock()
				require.Equal(t, pb.SessionEvent_SESSION_EVENT_UNBLOCK, mock.fireEventReqs[0].GetEvent())
				mock.mu.Unlock()
				if tc.refuseStart {
					blocks := blockRequests(mock)
					require.Len(t, blocks, 1)
					block, ok := sessionreason.ParseRepairCouldNotStart(&blocks[0].BlockedReason)
					require.True(t, ok)
					require.False(t, block.HeadMoved(tc.head))
					mock.mu.Lock()
					require.Equal(t, pb.SessionState_SESSION_STATE_BLOCKED, mock.sessions[0].GetState())
					require.Equal(t, blocks[0].GetBlockedReason(), mock.sessions[0].GetBlockedReason())
					mock.mu.Unlock()
				}
			} else {
				c, _, _, _ := mock.snapshot()
				require.Zero(t, c)
				mock.mu.Lock()
				defer mock.mu.Unlock()
				require.Equal(t, pb.SessionState_SESSION_STATE_BLOCKED, mock.sessions[0].GetState())
				if !tc.refuseUnblock {
					require.Empty(t, mock.fireEventReqs)
				}
			}
		})
	}
}

func TestMaybeRepair_ManualUnblockResetsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unblock   bool
		wantCount int
	}{
		{name: "manual unblock starts a fresh streak", unblock: true, wantCount: 1},
		{name: "ordinary refusals retain their history", wantCount: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newTestMock()
			pr := int32(1)
			mock.sessions = []*pb.Session{{Id: "s1", State: pb.SessionState_SESSION_STATE_AWAITING_CHECKS, PrDisplayHeadSha: "head-a", RepoOriginUrl: "https://github.com/o/r", BaseBranch: "main", PrNumber: &pr}}
			mock.applySessionEvents = true
			mock.branchHeadSHA = "base-a"
			mock.startErr = status.Error(codes.FailedPrecondition, "chat busy")
			rm := newTestMonitor(mock)
			rm.refusals["s1"] = refusalStreak{headSHA: "head-a", first: time.Now().Add(-time.Minute), count: 2}
			if tc.unblock {
				rm.refusals["s1"] = refusalStreak{headSHA: "head-a", first: time.Now().Add(-31 * time.Minute), count: maxRepairLoopAttempts, escalated: true}
				reason := sessionreason.RepairCouldNotStart(maxRepairLoopAttempts, 31*time.Minute, "head-a", "base-a", "chat busy")
				rm.blockSession(t.Context(), "s1", reason)
				_, err := mock.FireSessionEvent(t.Context(), &pb.FireSessionEventRequest{SessionId: "s1", Event: pb.SessionEvent_SESSION_EVENT_UNBLOCK})
				require.NoError(t, err)
			}
			rm.maybeRepair("s1", pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
			waitFor(t, func() bool { c, _, _, _ := mock.snapshot(); return c > 0 }, "repair start attempted")
			waitRepairDone(t, rm)
			mock.mu.Lock()
			sessionState := mock.sessions[0].GetState()
			mock.mu.Unlock()
			require.Equal(t, pb.SessionState_SESSION_STATE_AWAITING_CHECKS, sessionState)
			rm.mu.Lock()
			streak := rm.refusals["s1"]
			rm.mu.Unlock()
			require.Equal(t, tc.wantCount, streak.count)
			require.False(t, streak.escalated)
		})
	}
}

func (m *mockHostClient) FireSessionEvent(_ context.Context, req *pb.FireSessionEventRequest) (*pb.FireSessionEventResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fireEventCalls++
	m.fireEventReqs = append(m.fireEventReqs, req)
	if m.fireEventErr != nil {
		return nil, m.fireEventErr
	}
	if m.applySessionEvents {
		for _, sess := range m.sessions {
			if sess.GetId() == req.GetSessionId() {
				if req.GetEvent() == pb.SessionEvent_SESSION_EVENT_UNBLOCK {
					sess.State = pb.SessionState_SESSION_STATE_AWAITING_CHECKS
					sess.BlockedReason = nil
				} else if req.GetEvent() == pb.SessionEvent_SESSION_EVENT_BLOCK {
					sess.State = pb.SessionState_SESSION_STATE_BLOCKED
					reason := req.GetBlockedReason()
					sess.BlockedReason = &reason
				}
			}
		}
	}
	return &pb.FireSessionEventResponse{}, nil
}
