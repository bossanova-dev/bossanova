package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func capacityConflictSession(id string, rank *bossanovav1.TrackerRank) *bossanovav1.Session {
	return &bossanovav1.Session{
		Id:               id,
		RepoId:           "repo-1",
		RepoDisplayName:  "repo-1",
		BranchName:       "feature/" + id,
		State:            bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		DisplayStatus:    bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT,
		PrDisplayHeadSha: "head-" + id,
		TrackerRank:      rank,
	}
}

func queuedRequestsFor(reqs []*bossanovav1.SetRepairStatusRequest, sessionID string) []*bossanovav1.SetRepairStatusRequest {
	var out []*bossanovav1.SetRepairStatusRequest
	for _, r := range reqs {
		if r.GetSessionId() == sessionID && r.GetIsQueued() {
			out = append(out, r)
		}
	}
	return out
}

func TestRepairCapacityDefersPastCap(t *testing.T) {
	host := newTestMock()
	host.sessions = []*bossanovav1.Session{
		capacityConflictSession("s1", nil),
		capacityConflictSession("s2", nil),
		capacityConflictSession("s3", nil),
	}

	started := make(chan string, 4)
	finish := make(chan struct{})
	host.startFunc = func(req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
		started <- req.GetSessionId()
		return &bossanovav1.StartChatRunHostResponse{AgentSessionId: req.GetSessionId() + "-run"}, nil
	}
	host.waitFunc = func(_ context.Context, _ *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
		<-finish
		return &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}, nil
	}

	rm := newTestMonitor(host)
	t.Cleanup(rm.cancel)
	rm.config = &repairConfig{MaxConcurrentRepairs: 2}

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.maybeRepair("s2", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	require.ElementsMatch(t, []string{"s1", "s2"}, []string{<-started, <-started})

	rm.maybeRepair("s3", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.maybeRepair("s3", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)

	starts, _, _, statusReqs := host.snapshot()
	require.Equal(t, 2, starts, "s3 must not start while 2/2 slots are held")
	for _, req := range host.startRequestsSnapshot() {
		assert.NotEqual(t, "s3", req.GetSessionId())
	}
	assert.Empty(t, host.recordOutcomeSnapshot(), "deferred session must record no outcome")
	assert.Empty(t, blockRequests(host), "deferred session must record no blocked reason")

	s3Queued := queuedRequestsFor(statusReqs, "s3")
	require.Len(t, s3Queued, 2, "each capacity deferral must send SetRepairStatus{is_queued: true}")
	for _, req := range s3Queued {
		assert.False(t, req.GetIsRepairing())
		assert.True(t, req.GetIsQueued())
	}

	rm.mu.Lock()
	_, hasCooldown := rm.cooldowns["s3"]
	_, hasLastAttempt := rm.lastAttempt["s3"]
	_, hasBaseMove := rm.baseMoves["s3"]
	rm.mu.Unlock()
	assert.False(t, hasCooldown, "deferred session must record no cooldown")
	assert.False(t, hasLastAttempt, "deferred session must record no attempt")
	assert.False(t, hasBaseMove, "deferred session must record no base-move streak")

	close(finish)
	rm.wg.Wait()
}

func TestRepairCapacitySweepAdmitsInRankOrder(t *testing.T) {
	host := newTestMock()
	host.sessions = []*bossanovav1.Session{
		capacityConflictSession("s-unranked", nil),
		capacityConflictSession("s-low", &bossanovav1.TrackerRank{Tier: 4}),
		capacityConflictSession("s-urgent", &bossanovav1.TrackerRank{Tier: 1}),
		capacityConflictSession("s-high", &bossanovav1.TrackerRank{Tier: 2}),
	}

	started := make(chan string, 8)
	var gateMu sync.Mutex
	gates := map[string]chan struct{}{
		"s-urgent-run":   make(chan struct{}),
		"s-high-run":     make(chan struct{}),
		"s-low-run":      make(chan struct{}),
		"s-unranked-run": make(chan struct{}),
	}
	host.startFunc = func(req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
		started <- req.GetSessionId()
		return &bossanovav1.StartChatRunHostResponse{AgentSessionId: req.GetSessionId() + "-run"}, nil
	}
	host.waitFunc = func(_ context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
		gateMu.Lock()
		ch := gates[req.GetAgentSessionId()]
		gateMu.Unlock()
		<-ch
		return &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}, nil
	}

	rm := newTestMonitor(host)
	t.Cleanup(rm.cancel)
	rm.config = &repairConfig{MaxConcurrentRepairs: 2}

	rm.sweepExistingSessions(t.Context())
	gotFirst := []string{<-started, <-started}
	assert.ElementsMatch(t, []string{"s-urgent", "s-high"}, gotFirst)

	starts, _, _, statusReqs := host.snapshot()
	require.Equal(t, 2, starts)
	assert.Len(t, queuedRequestsFor(statusReqs, "s-low"), 1)
	assert.Len(t, queuedRequestsFor(statusReqs, "s-unranked"), 1)

	close(gates["s-urgent-run"])
	waitFor(t, func() bool {
		rm.mu.Lock()
		defer rm.mu.Unlock()
		return !rm.repairing["s-urgent"]
	}, "s-urgent should release its slot")

	rm.sweepExistingSessions(t.Context())
	require.Equal(t, "s-low", <-started)

	starts, _, _, statusReqs = host.snapshot()
	require.Equal(t, 3, starts)
	assert.Len(t, queuedRequestsFor(statusReqs, "s-unranked"), 2)

	close(gates["s-high-run"])
	close(gates["s-low-run"])
	close(gates["s-unranked-run"])
	rm.wg.Wait()
}

func TestRepairCapacityQueuedSkipsBaseMoveStreak(t *testing.T) {
	host := newTestMock()
	sess := conflictSessionAfterFailedRepair(0)
	sess.LastRepairHeadSha = "abc123"
	sess.LastRepairDisplayStatus = bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT
	sess.LastRepairBaseSha = "base-1"
	sess.LastRepairExitError = ""
	sess.LastRepairStartedAt = timestamppb.New(time.Now().Add(-24 * time.Hour))
	holder := capacityConflictSession("holder", nil)
	setSessions(host, holder, sess)

	rm := newTestMonitor(host)
	t.Cleanup(rm.cancel)
	rm.config = &repairConfig{
		MaxConcurrentRepairs:       1,
		PostRepairPollMilliseconds: 1,
		PostRepairWaitMilliseconds: 20,
	}
	rm.repairing["holder"] = true

	for i := 1; i <= maxRepairLoopAttempts+2; i++ {
		host.setBranchHead(fmt.Sprintf("base-%d", i+1))
		rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	}

	starts, _, _, statusReqs := host.snapshot()
	require.Zero(t, starts, "queued session must not start while cap is full")
	require.Empty(t, blockRequests(host), "queued session must not be blocked by base branch moves")
	require.Len(t, queuedRequestsFor(statusReqs, "s1"), maxRepairLoopAttempts+2)

	rm.mu.Lock()
	streak := rm.baseMoves["s1"]
	rm.mu.Unlock()
	require.Zero(t, streak.count, "base-move streak must not advance while queued")

	rm.mu.Lock()
	delete(rm.repairing, "holder")
	rm.mu.Unlock()

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
	rm.wg.Wait()

	starts, _, _, _ = host.snapshot()
	require.Equal(t, 1, starts, "session should be admitted once capacity frees")
	require.Empty(t, blockRequests(host))
	rm.mu.Lock()
	streak = rm.baseMoves["s1"]
	rm.mu.Unlock()
	assert.Equal(t, 1, streak.count)
}

func TestRepairCapacityCountsInheritedRepairs(t *testing.T) {
	t.Run("repair_active snapshot row counts toward cap", func(t *testing.T) {
		host := newTestMock()
		host.sessions = []*bossanovav1.Session{
			{Id: "inherited-lease", RepoId: "r", RepairActive: true},
			capacityConflictSession("candidate", nil),
		}
		host.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}

		rm := newTestMonitor(host)
		t.Cleanup(rm.cancel)
		rm.config = &repairConfig{MaxConcurrentRepairs: 1}

		rm.maybeRepair("candidate", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
		rm.wg.Wait()

		starts, _, _, statusReqs := host.snapshot()
		require.Zero(t, starts)
		require.Len(t, queuedRequestsFor(statusReqs, "candidate"), 1)
	})

	t.Run("has_active_chat and display_is_repairing counts toward cap", func(t *testing.T) {
		host := newTestMock()
		host.sessions = []*bossanovav1.Session{
			{Id: "inherited-chat", RepoId: "r", HasActiveChat: true, DisplayIsRepairing: true},
			capacityConflictSession("candidate", nil),
		}
		host.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}

		rm := newTestMonitor(host)
		t.Cleanup(rm.cancel)
		rm.config = &repairConfig{MaxConcurrentRepairs: 1}

		rm.maybeRepair("candidate", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
		rm.wg.Wait()

		starts, _, _, statusReqs := host.snapshot()
		require.Zero(t, starts)
		require.Len(t, queuedRequestsFor(statusReqs, "candidate"), 1)
	})

	t.Run("dead repair without lease or live chat does not count", func(t *testing.T) {
		host := newTestMock()
		host.sessions = []*bossanovav1.Session{
			{Id: "dead-repair", RepoId: "r", HasActiveChat: false, DisplayIsRepairing: true, RepairActive: false},
			capacityConflictSession("candidate", nil),
		}
		host.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}

		rm := newTestMonitor(host)
		t.Cleanup(rm.cancel)
		rm.config = &repairConfig{MaxConcurrentRepairs: 1}

		rm.maybeRepair("candidate", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
		rm.wg.Wait()

		starts, _, _, _ := host.snapshot()
		require.Equal(t, 1, starts)
	})

	t.Run("archived session does not count", func(t *testing.T) {
		host := newTestMock()
		host.sessions = []*bossanovav1.Session{
			{
				Id:                 "archived-holder",
				RepoId:             "r",
				RepairActive:       true,
				HasActiveChat:      true,
				DisplayIsRepairing: true,
				ArchivedAt:         timestamppb.Now(),
			},
			capacityConflictSession("candidate", nil),
		}
		host.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}

		rm := newTestMonitor(host)
		t.Cleanup(rm.cancel)
		rm.config = &repairConfig{MaxConcurrentRepairs: 1}

		rm.maybeRepair("candidate", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
		rm.wg.Wait()

		starts, _, _, _ := host.snapshot()
		require.Equal(t, 1, starts)
	})

	t.Run("same session in repairing and snapshot counts once", func(t *testing.T) {
		host := newTestMock()
		host.sessions = []*bossanovav1.Session{
			{Id: "holder", RepoId: "r", RepairActive: true, HasActiveChat: true, DisplayIsRepairing: true},
			capacityConflictSession("candidate", nil),
		}
		host.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}

		rm := newTestMonitor(host)
		t.Cleanup(rm.cancel)
		rm.config = &repairConfig{MaxConcurrentRepairs: 2}
		rm.repairing["holder"] = true

		rm.maybeRepair("candidate", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
		rm.wg.Wait()

		starts, _, _, _ := host.snapshot()
		require.Equal(t, 1, starts, "deduplicated holder uses only 1 of 2 slots")
	})

	t.Run("dependabot per-repo deferral still applies with free host slots", func(t *testing.T) {
		host := newTestMock()
		host.sessions = []*bossanovav1.Session{
			{Id: "dep-holder", RepoId: "r", BranchName: "dependabot/npm/a", HasActiveChat: true, DisplayIsRepairing: true},
			{
				Id:               "dep-candidate",
				RepoId:           "r",
				BranchName:       "dependabot/npm/b",
				State:            bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
				PrDisplayHeadSha: "head-b",
				PrNumber:         proto.Int32(2),
			},
		}
		host.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}

		rm := newTestMonitor(host)
		t.Cleanup(rm.cancel)
		rm.config = &repairConfig{MaxConcurrentRepairs: 3}

		rm.maybeRepair("dep-candidate", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)
		rm.wg.Wait()

		starts, _, _, statusReqs := host.snapshot()
		require.Zero(t, starts, "dependabot per-repo serialization must still defer")
		require.Empty(t, statusReqs, "dependabot deferral must not emit capacity-queued status")
	})
}

func TestRepairCapacityConfig(t *testing.T) {
	t.Parallel()

	t.Run("nil receiver returns default 3", func(t *testing.T) {
		t.Parallel()
		var c *repairConfig
		assert.Equal(t, 3, c.maxConcurrentRepairs())
	})

	t.Run("unset and non-positive fall back to 3", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 3, (&repairConfig{}).maxConcurrentRepairs())
		assert.Equal(t, 3, (&repairConfig{MaxConcurrentRepairs: 0}).maxConcurrentRepairs())
		assert.Equal(t, 3, (&repairConfig{MaxConcurrentRepairs: -1}).maxConcurrentRepairs())
		assert.Equal(t, 3, (&repairConfig{MaxConcurrentRepairs: -10}).maxConcurrentRepairs())
	})

	t.Run("positive values honoured", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 1, (&repairConfig{MaxConcurrentRepairs: 1}).maxConcurrentRepairs())
		assert.Equal(t, 5, (&repairConfig{MaxConcurrentRepairs: 5}).maxConcurrentRepairs())
	})

	t.Run("json parsing", func(t *testing.T) {
		t.Parallel()
		cfg, err := parseRepairConfig(`{"max_concurrent_repairs":5}`)
		require.NoError(t, err)
		assert.Equal(t, 5, cfg.maxConcurrentRepairs())

		cfgZero, err := parseRepairConfig(`{"max_concurrent_repairs":0}`)
		require.NoError(t, err)
		assert.Equal(t, 3, cfgZero.maxConcurrentRepairs())

		cfgNeg, err := parseRepairConfig(`{"max_concurrent_repairs":-3}`)
		require.NoError(t, err)
		assert.Equal(t, 3, cfgNeg.maxConcurrentRepairs())

		cfgEmpty, err := parseRepairConfig(`{}`)
		require.NoError(t, err)
		assert.Equal(t, 3, cfgEmpty.maxConcurrentRepairs())
	})
}

func TestRepairCapacityCapWithInheritedHolder_Sessions_AC268901(t *testing.T) {
	host := newTestMock()
	host.sessions = []*bossanovav1.Session{
		{Id: "inherited-holder", RepoId: "repo-1", HasActiveChat: true, DisplayIsRepairing: true},
		capacityConflictSession("s-urgent", &bossanovav1.TrackerRank{Tier: 1}),
		capacityConflictSession("s-high", &bossanovav1.TrackerRank{Tier: 2}),
		capacityConflictSession("s-normal", &bossanovav1.TrackerRank{Tier: 3}),
	}

	started := make(chan string, 4)
	finish := make(chan struct{})
	host.startFunc = func(req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
		started <- req.GetSessionId()
		return &bossanovav1.StartChatRunHostResponse{AgentSessionId: req.GetSessionId() + "-run"}, nil
	}
	host.waitFunc = func(_ context.Context, _ *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
		<-finish
		return &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}, nil
	}

	rm := newTestMonitor(host)
	t.Cleanup(rm.cancel)
	rm.config = &repairConfig{MaxConcurrentRepairs: 2}

	rm.sweepExistingSessions(t.Context())
	require.Equal(t, "s-urgent", <-started, "only one free slot remains beside the inherited holder")

	starts, _, _, statusReqs := host.snapshot()
	require.Equal(t, 1, starts, "inherited holder plus one new start fills the cap of 2")
	assert.Empty(t, host.recordOutcomeSnapshot())
	assert.Empty(t, blockRequests(host))

	for _, waitingID := range []string{"s-high", "s-normal"} {
		assert.Len(t, queuedRequestsFor(statusReqs, waitingID), 1)
		rm.mu.Lock()
		_, hasCooldown := rm.cooldowns[waitingID]
		_, hasAttempt := rm.lastAttempt[waitingID]
		_, hasBaseMove := rm.baseMoves[waitingID]
		rm.mu.Unlock()
		assert.False(t, hasCooldown)
		assert.False(t, hasAttempt)
		assert.False(t, hasBaseMove)
	}

	close(finish)
	rm.wg.Wait()
}

func TestRepairCapacityFreedSlotAdmitsBestRanked_Sessions_AC268903(t *testing.T) {
	host := newTestMock()
	host.sessions = []*bossanovav1.Session{
		capacityConflictSession("s-first", &bossanovav1.TrackerRank{Tier: 1}),
		capacityConflictSession("s-second", &bossanovav1.TrackerRank{Tier: 2}),
		capacityConflictSession("s-third", &bossanovav1.TrackerRank{Tier: 3}),
	}

	started := make(chan string, 4)
	gates := map[string]chan struct{}{
		"s-first-run":  make(chan struct{}),
		"s-second-run": make(chan struct{}),
		"s-third-run":  make(chan struct{}),
	}
	host.startFunc = func(req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
		started <- req.GetSessionId()
		return &bossanovav1.StartChatRunHostResponse{AgentSessionId: req.GetSessionId() + "-run"}, nil
	}
	host.waitFunc = func(_ context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
		<-gates[req.GetAgentSessionId()]
		return &bossanovav1.WaitChatRunHostResponse{ExitError: "repair failed"}, nil
	}

	rm := newTestMonitor(host)
	t.Cleanup(rm.cancel)
	rm.config = &repairConfig{MaxConcurrentRepairs: 1}

	rm.sweepExistingSessions(t.Context())
	require.Equal(t, "s-first", <-started)

	close(gates["s-first-run"])
	waitFor(t, func() bool {
		rm.mu.Lock()
		defer rm.mu.Unlock()
		return !rm.repairing["s-first"]
	}, "s-first should finish and free its slot")

	rm.sweepExistingSessions(t.Context())
	require.Equal(t, "s-second", <-started, "best-ranked waiting session should be admitted next")

	starts, _, _, statusReqs := host.snapshot()
	require.Equal(t, 2, starts)
	assert.Len(t, queuedRequestsFor(statusReqs, "s-second"), 1, "admitted session should not be re-queued on admission sweep")
	assert.Len(t, queuedRequestsFor(statusReqs, "s-third"), 2, "lower-ranked waiting session remains queued")

	close(gates["s-second-run"])
	close(gates["s-third-run"])
	rm.wg.Wait()
}
