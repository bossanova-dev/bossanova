package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func failingInputSession() *bossanovav1.Session {
	sess := conflictSessionAfterFailedRepair(0)
	sess.LastRepairHeadSha = ""
	sess.LastRepairDisplayStatus = bossanovav1.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED
	sess.LastRepairBaseSha = ""
	sess.LastRepairExitError = ""
	sess.DisplayStatus = bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING
	sess.DisplayHasFailures = true
	return sess
}

func clearInputCooldown(rm *repairMonitor) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	delete(rm.cooldowns, "s1")
}

// The shared host mock records outcomes without updating its session snapshot.
// Apply the attempted input and the host's failure-count rule between sweeps.
func persistInputOutcome(mock *mockHostClient, sess *bossanovav1.Session) *bossanovav1.Session {
	outcomes := mock.recordOutcomeSnapshot()
	outcome := outcomes[len(outcomes)-1]
	next := proto.Clone(sess).(*bossanovav1.Session)
	next.LastRepairHeadSha = outcome.GetHeadSha()
	next.LastRepairDisplayStatus = outcome.GetDisplayStatus()
	next.LastRepairBaseSha = outcome.GetBaseSha()
	next.LastRepairExitError = outcome.GetExitError()
	next.LastRepairRunnerError = outcome.GetRunnerError()
	next.LastRepairStartedAt = timestamppb.New(time.Now().Add(-24 * time.Hour))
	if outcome.GetExitError() != "" || outcome.GetRunnerError() != "" {
		next.LastRepairAttemptCount++
	} else {
		next.LastRepairAttemptCount = 0
	}
	setSessions(mock, next)
	return next
}

func TestMaybeRepair_FailingCleanNoChangeRecordsInputAndSkipsRepeatedSweeps(t *testing.T) {
	mock := newTestMock()
	sess := failingInputSession()
	setSessions(mock, sess)
	mock.setBranchHead("base-1")
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 20}

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	rm.wg.Wait()
	outcomes := mock.recordOutcomeSnapshot()
	require.Len(t, outcomes, 1)
	assert.Empty(t, outcomes[0].GetExitError(), "a clean no-change exit remains a successful runner exit")
	assert.Equal(t, "abc123", outcomes[0].GetHeadSha())
	assert.Equal(t, "base-1", outcomes[0].GetBaseSha(), "persist the base actually attempted for FAILING")
	persistInputOutcome(mock, sess)
	for range 3 {
		clearInputCooldown(rm)
		rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
		rm.wg.Wait()
	}
	starts, _, _, _ := mock.snapshot()
	assert.Equal(t, 1, starts)
	assert.Len(t, mock.recordOutcomeSnapshot(), 1)
}

func TestMaybeRepair_FailingInputMovesAllowOneNewAttempt(t *testing.T) {
	for _, move := range []string{"base", "head"} {
		t.Run(move, func(t *testing.T) {
			mock := newTestMock()
			sess := failingInputSession()
			sess.LastRepairHeadSha = "abc123"
			sess.LastRepairDisplayStatus = bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING
			sess.LastRepairBaseSha = "base-1"
			setSessions(mock, sess)
			mock.setBranchHead("base-1")
			rm := newTestMonitor(mock)
			rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 20}
			rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
			starts, _, _, _ := mock.snapshot()
			require.Zero(t, starts)
			if move == "base" {
				mock.setBranchHead("base-2")
			} else {
				sess = proto.Clone(sess).(*bossanovav1.Session)
				sess.PrDisplayHeadSha = "head-2"
				setSessions(mock, sess)
			}
			rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
			rm.wg.Wait()
			starts, _, _, _ = mock.snapshot()
			require.Equal(t, 1, starts, "a moved %s permits repair", move)
			persistInputOutcome(mock, sess)
			clearInputCooldown(rm)
			rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
			rm.wg.Wait()
			starts, _, _, _ = mock.snapshot()
			assert.Equal(t, 1, starts, "the newly attempted input is deduplicated")
		})
	}
}

func TestMaybeRepair_FailingUnreadableBaseSkipsPersistedInput(t *testing.T) {
	mock := newTestMock()
	sess := failingInputSession()
	sess.LastRepairHeadSha = "abc123"
	sess.LastRepairDisplayStatus = bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING
	sess.LastRepairBaseSha = "base-1"
	setSessions(mock, sess)
	mock.branchHeadErr = errors.New("github unavailable")
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	rm.wg.Wait()
	starts, _, _, _ := mock.snapshot()
	assert.Zero(t, starts)
	require.Len(t, mock.branchHeadSnapshot(), 1, "check the remote base before concluding that the input moved")
}

func TestMaybeRepair_FailingCleanBaseMovesRemainBounded(t *testing.T) {
	mock := newTestMock()
	sess := failingInputSession()
	sess.LastRepairHeadSha = "abc123"
	sess.LastRepairDisplayStatus = bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING
	sess.LastRepairBaseSha = "base-1"
	setSessions(mock, sess)
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 20}
	for i := 1; i <= maxRepairLoopAttempts; i++ {
		mock.setBranchHead(fmt.Sprintf("base-%d", i+1))
		clearInputCooldown(rm)
		rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
		rm.wg.Wait()
		starts, _, _, _ := mock.snapshot()
		require.Equal(t, i, starts)
		require.Len(t, mock.recordOutcomeSnapshot(), i)
		sess = persistInputOutcome(mock, sess)
		require.Zero(t, sess.LastRepairAttemptCount, "clean exits do not use the failure budget")
	}
	mock.setBranchHead("base-final")
	clearInputCooldown(rm)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	rm.wg.Wait()
	starts, _, _, _ := mock.snapshot()
	assert.Equal(t, maxRepairLoopAttempts, starts)
	require.Len(t, blockRequests(mock), 1, "clean but ineffective base-move repairs eventually require a person")
}

func TestMaybeRepair_FailingRealFailuresBlockAtCapAndStayBlockedAfterHeadMoves(t *testing.T) {
	mock := newTestMock()
	mock.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "exit status 1"}
	sess := failingInputSession()
	setSessions(mock, sess)
	rm := newTestMonitor(mock)
	for i := 1; i <= maxRepairLoopAttempts; i++ {
		mock.setBranchHead(fmt.Sprintf("base-%d", i))
		clearInputCooldown(rm)
		rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
		rm.wg.Wait()
		starts, _, _, _ := mock.snapshot()
		require.Equal(t, i, starts)
		require.Len(t, mock.recordOutcomeSnapshot(), i)
		sess = persistInputOutcome(mock, sess)
		require.Equal(t, int32(i), sess.LastRepairAttemptCount)
	}
	mock.setBranchHead("base-final")
	clearInputCooldown(rm)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	rm.wg.Wait()
	require.Len(t, blockRequests(mock), 1)
	sess = proto.Clone(sess).(*bossanovav1.Session)
	sess.State = bossanovav1.SessionState_SESSION_STATE_BLOCKED
	setSessions(mock, sess)
	for _, head := range []string{"abc123", "head-after-block"} {
		next := proto.Clone(sess).(*bossanovav1.Session)
		next.PrDisplayHeadSha = head
		setSessions(mock, next)
		clearInputCooldown(rm)
		rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
		rm.wg.Wait()
	}
	starts, _, _, _ := mock.snapshot()
	assert.Equal(t, maxRepairLoopAttempts, starts, "Blocked requires operator action even after a push")
}

// repairOutcomeFailureHost keeps diagnostics stale while recording the attempted writes.
type repairOutcomeFailureHost struct {
	*mockHostClient
}

func (h *repairOutcomeFailureHost) RecordRepairOutcome(ctx context.Context, req *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error) {
	_, _ = h.mockHostClient.RecordRepairOutcome(ctx, req)
	return nil, errors.New("outcome persistence unavailable")
}

func newOutcomeFailureMonitor() (*repairMonitor, *mockHostClient) {
	mock := newTestMock()
	sess := conflictSessionAfterFailedRepair(0)
	sess.LastRepairHeadSha = "head-0"
	sess.LastRepairExitError = ""
	setSessions(mock, sess)
	mock.setBranchHead("base-1")
	rm := newRepairMonitor(&repairOutcomeFailureHost{mock}, zerolog.Nop())
	rm.stopped = false
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 20}
	return rm, mock
}

func TestMaybeRepair_OutcomeFailureBaseMoveRetriesInMemoryAttempt(t *testing.T) {
	rm, mock := newOutcomeFailureMonitor()
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.wg.Wait()
	starts, _, _, _ := mock.snapshot()
	require.Equal(t, 1, starts)
	require.Len(t, mock.recordOutcomeSnapshot(), 1)
	require.Len(t, mock.branchHeadSnapshot(), 1, "a fresh input reads the base only after deciding to run")

	clearInputCooldown(rm)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.wg.Wait()
	starts, _, _, _ = mock.snapshot()
	require.Equal(t, 1, starts, "the same base is still a repeat when persistence fails")

	mock.setBranchHead("base-2")
	clearInputCooldown(rm)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.wg.Wait()
	starts, _, _, _ = mock.snapshot()
	require.Equal(t, 2, starts, "a base move unlocks the in-memory attempt despite stale diagnostics")
	outcomes := mock.recordOutcomeSnapshot()
	require.Len(t, outcomes, 2)
	assert.Equal(t, "base-2", outcomes[1].GetBaseSha())
}

func TestMaybeRepair_OutcomeFailureUnreadableBaseDeduplicates(t *testing.T) {
	rm, mock := newOutcomeFailureMonitor()
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.wg.Wait()
	mock.branchHeadErr = errors.New("github unavailable")
	clearInputCooldown(rm)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.wg.Wait()
	starts, _, _, _ := mock.snapshot()
	assert.Equal(t, 1, starts)
	assert.Len(t, mock.recordOutcomeSnapshot(), 1)
}

func TestMaybeRepair_OutcomeFailureBaseMovesRemainBounded(t *testing.T) {
	rm, mock := newOutcomeFailureMonitor()
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.wg.Wait()
	for i := 1; i <= maxRepairLoopAttempts; i++ {
		mock.setBranchHead(fmt.Sprintf("base-%d", i+1))
		clearInputCooldown(rm)
		rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
		rm.wg.Wait()
		starts, _, _, _ := mock.snapshot()
		require.Equal(t, i+1, starts)
	}
	mock.setBranchHead("base-final")
	clearInputCooldown(rm)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.wg.Wait()
	starts, _, _, _ := mock.snapshot()
	assert.Equal(t, maxRepairLoopAttempts+1, starts)
	blocks := blockRequests(mock)
	require.Len(t, blocks, 1)
	assert.Contains(t, blocks[0].GetBlockedReason(), "base branch moves")
}
