package main

import (
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func blockRequests(mock *mockHostClient) []*bossanovav1.FireSessionEventRequest {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	var out []*bossanovav1.FireSessionEventRequest
	for _, req := range mock.fireEventReqs {
		if req.GetEvent() == bossanovav1.SessionEvent_SESSION_EVENT_BLOCK {
			out = append(out, req)
		}
	}
	return out
}

func TestMaybeRepair_BlocksInsteadOfRetryingAfterRepeatedFailuresOnOneHead(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{{
		Id:                     "s1",
		State:                  bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		PrDisplayHeadSha:       "0123456789abcdef0123",
		LastRepairHeadSha:      "0123456789abcdef0123",
		LastRepairAttemptCount: maxRepairLoopAttempts,
		LastRepairExitError:    "agent went idle without completing (no output for 5m0s; watchdog)\nsecond line",
	}}
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool { return len(blockRequests(mock)) > 0 }, "session blocked")
	block := blockRequests(mock)[0]
	assert.Contains(t, block.GetBlockedReason(), "failed 5 times in a row on 0123456789ab")
	assert.Contains(t, block.GetBlockedReason(), "agent went idle without completing")
	assert.NotContains(t, block.GetBlockedReason(), "second line")
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "an exhausted session is blocked, not repaired again")
}

func TestMaybeRepair_FailuresOnAnEarlierHeadDoNotBlock(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{{
		Id:                     "s1",
		State:                  bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		PrDisplayHeadSha:       "new-head",
		LastRepairHeadSha:      "old-head",
		LastRepairAttemptCount: maxRepairLoopAttempts,
		LastRepairRunnerError:  "start agent: boom",
	}}
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool { c, _, _, _ := mock.snapshot(); return c > 0 }, "repair attempted on the new head")
	assert.Empty(t, blockRequests(mock), "failures on an earlier head must not block the new one")
}

func TestNoteRefusal_EscalatesOnlyForAPersistentStreakOnOneHead(t *testing.T) {
	rm := newTestMonitor(newTestMock())
	start := time.Now()

	for i := range maxRepairLoopAttempts {
		_, escalate := rm.noteRefusal("s1", "head-a", start.Add(time.Duration(i)*time.Minute))
		assert.False(t, escalate, "refusal %d within minutes must not block: a warming tracker refuses for ~5m after a restart", i+1)
	}
	streak, escalate := rm.noteRefusal("s1", "head-a", start.Add(repairRefusalEscalateAfter))
	assert.True(t, escalate, "refusals persisting past the window on one head must block")
	assert.Equal(t, maxRepairLoopAttempts+1, streak.count)

	_, escalate = rm.noteRefusal("s1", "head-b", start.Add(2*repairRefusalEscalateAfter))
	assert.False(t, escalate, "a new head starts a new streak")

	rm.clearRefusals("s1")
	streak, _ = rm.noteRefusal("s1", "head-b", start.Add(3*repairRefusalEscalateAfter))
	assert.Equal(t, 1, streak.count, "a run that actually started resets the streak")
}

func TestRunRepairAttempt_PersistentStartRefusalBlocksTheSession(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{{
		Id:               "s1",
		State:            bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		PrDisplayHeadSha: "head-a",
	}}
	mock.startErr = grpcstatus.Error(codes.FailedPrecondition, "displace blocked: chat agent-x is at a question (idle 14m0s)")
	rm := newTestMonitor(mock)
	rm.refusals["s1"] = refusalStreak{headSHA: "head-a", first: time.Now().Add(-repairRefusalEscalateAfter - time.Minute), count: maxRepairLoopAttempts - 1}

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool { return len(blockRequests(mock)) > 0 }, "session blocked after a persistent refusal streak")
	reason := blockRequests(mock)[0].GetBlockedReason()
	require.True(t, strings.HasPrefix(reason, "automatic repair could not start 5 times in a row"), reason)
	assert.Contains(t, reason, "is at a question")
}

// seedWarmUpRefusals records the refusals a cold chat tracker produces after a
// daemon restart (one a minute for about five minutes), started long enough ago
// that one more refusal on the same head would complete an escalating streak.
func seedWarmUpRefusals(t *testing.T, rm *repairMonitor, head string) {
	t.Helper()
	start := time.Now().Add(-repairRefusalEscalateAfter - 10*time.Minute)
	for i := range maxRepairLoopAttempts {
		_, escalate := rm.noteRefusal("s1", head, start.Add(time.Duration(i)*time.Minute))
		require.False(t, escalate, "warm-up refusal %d must not block", i+1)
	}
}

func setSessions(mock *mockHostClient, sessions ...*bossanovav1.Session) {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.sessions = sessions
}

func waitRepairDone(t *testing.T, rm *repairMonitor) {
	t.Helper()
	waitFor(t, func() bool {
		rm.mu.Lock()
		defer rm.mu.Unlock()
		return !rm.repairing["s1"]
	}, "repair attempt finished")
}

const snapshotRaceRefusal = "chat agent-op produced output after idle snapshot"

func TestMaybeRepair_AttendedSessionDoesNotInheritWarmUpRefusals(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	seedWarmUpRefusals(t, rm, "head-a")

	setSessions(mock, &bossanovav1.Session{
		Id:                 "s1",
		State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		PrDisplayHeadSha:   "head-a",
		HasActiveChat:      true,
		LastChatActivityAt: timestamppb.New(time.Now().Add(-time.Minute)),
	})
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	startCalls, _, _, _ := mock.snapshot()
	require.Zero(t, startCalls, "repair defers while the operator is working in the chat")

	setSessions(mock, &bossanovav1.Session{
		Id:                 "s1",
		State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		PrDisplayHeadSha:   "head-a",
		HasActiveChat:      true,
		LastChatActivityAt: timestamppb.New(time.Now().Add(-6 * time.Minute)),
	})
	mock.startErr = grpcstatus.Error(codes.FailedPrecondition, snapshotRaceRefusal)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	waitFor(t, func() bool { c, _, _, _ := mock.snapshot(); return c == 1 }, "repair start attempted once the chat went quiet")
	waitRepairDone(t, rm)

	assert.Empty(t, blockRequests(mock), "one snapshot-race refusal on an attended session must not block it")
	rm.mu.Lock()
	streak := rm.refusals["s1"]
	rm.mu.Unlock()
	assert.Equal(t, 1, streak.count, "the attended deferral ended the warm-up streak")
}

func TestMaybeRepair_UnblockedSessionGetsAFreshBudgetOnTheSameHead(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	seedWarmUpRefusals(t, rm, "head-a")

	setSessions(mock, &bossanovav1.Session{
		Id:                     "s1",
		State:                  bossanovav1.SessionState_SESSION_STATE_BLOCKED,
		PrDisplayHeadSha:       "head-a",
		LastRepairHeadSha:      "head-a",
		LastRepairAttemptCount: maxRepairLoopAttempts,
		LastRepairRunnerError:  "start agent: boom",
	})
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	startCalls, _, _, _ := mock.snapshot()
	require.Zero(t, startCalls, "a Blocked session is left to the operator")

	// The operator moves the session out of Blocked without pushing, and the
	// host resets the persisted failure count as it does so.
	setSessions(mock, &bossanovav1.Session{
		Id:                    "s1",
		State:                 bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS,
		PrDisplayHeadSha:      "head-a",
		LastRepairHeadSha:     "head-a",
		LastRepairRunnerError: "start agent: boom",
	})
	mock.startErr = grpcstatus.Error(codes.FailedPrecondition, snapshotRaceRefusal)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	waitFor(t, func() bool { c, _, _, _ := mock.snapshot(); return c == 1 }, "repair tried again on the same head")
	waitRepairDone(t, rm)

	assert.Empty(t, blockRequests(mock), "neither the old failure count nor the old refusal streak re-blocks an unblocked session")
}
