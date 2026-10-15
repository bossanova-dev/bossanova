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
