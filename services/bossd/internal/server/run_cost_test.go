package server

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"connectrpc.com/connect"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
)

type fakeAgentRunStore struct {
	listFilters []db.AgentRunFilter
	runs        []db.AgentRun
	backfilled  bool
	started     []db.AgentRun
	stopped     []string
	ops         []string
	telemetry   []fakeAgentRunTelemetry
	ctxErrs     []error
	deadlines   []time.Time

	recordTelemetryHook func(context.Context)
}

type fakeAgentRunTelemetry struct {
	runID     string
	telemetry db.AgentRunTelemetry
}

func (f *fakeAgentRunStore) Start(ctx context.Context, run db.AgentRun) (db.AgentRun, error) {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	if run.ID == "" {
		run.ID = "run-started"
	}
	f.started = append(f.started, run)
	f.ops = append(f.ops, "start:"+run.AgentSessionID)
	return run, nil
}

func (f *fakeAgentRunStore) Stop(ctx context.Context, agentSessionID string, _ string, _ time.Time) error {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	f.stopped = append(f.stopped, agentSessionID)
	f.ops = append(f.ops, "stop:"+agentSessionID)
	return nil
}

func (f *fakeAgentRunStore) StopRun(ctx context.Context, runID string, _ string, _ time.Time) error {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	for _, run := range f.runs {
		if run.ID == runID {
			f.stopped = append(f.stopped, runID)
			f.ops = append(f.ops, "stoprun:"+runID)
			return nil
		}
	}
	return sql.ErrNoRows
}

func (f *fakeAgentRunStore) RecordTelemetry(ctx context.Context, runID string, telemetry db.AgentRunTelemetry) error {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	f.telemetry = append(f.telemetry, fakeAgentRunTelemetry{runID: runID, telemetry: telemetry})
	if f.recordTelemetryHook != nil {
		f.recordTelemetryHook(ctx)
	}
	return nil
}

func (f *fakeAgentRunStore) RecordTelemetryByAgentSessionID(ctx context.Context, _ string, _ db.AgentRunTelemetry) error {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	return nil
}

func (f *fakeAgentRunStore) ReconcileOpen(context.Context, time.Time, []string) (int64, error) {
	return 0, nil
}

func (f *fakeAgentRunStore) List(_ context.Context, filter db.AgentRunFilter) ([]db.AgentRun, error) {
	f.listFilters = append(f.listFilters, filter)
	return f.runs, nil
}

func (f *fakeAgentRunStore) Backfill(context.Context, db.AgentRunBackfillParams) (db.AgentRunBackfillSummary, error) {
	f.backfilled = true
	return db.AgentRunBackfillSummary{InsertedCount: 2, SkippedCount: 3}, nil
}

func TestGetRunCostMapsRunDurationsAndAggregate(t *testing.T) {
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(10 * time.Minute)
	childStop := started.Add(5 * time.Minute)
	store := &fakeAgentRunStore{runs: []db.AgentRun{{
		ID:                   "run-1",
		SessionID:            "sess-1",
		AgentSessionID:       "agent-1",
		AgentName:            "codex",
		RepoDisplayName:      "bossanova",
		Model:                "gpt-5",
		Effort:               "high",
		StartedAt:            started,
		StoppedAt:            &stopped,
		StopReason:           db.AgentRunStopClean,
		ParentModelCallCount: 2,
		ChildModelCallCount:  1,
		ToolCallCount:        4,
		SubagentCount:        1,
		DirectSubagentCount:  1,
		Children: []db.AgentRunChild{{
			SpawnDepth:     1,
			StartedAt:      started.Add(time.Minute),
			StoppedAt:      &childStop,
			ModelCallCount: 1,
		}},
	}}}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{
		SessionId:               "sess-1",
		ShouldIncludeOpen:       true,
		ShouldIncludeAll:        true,
		ShouldIncludeBackfilled: true,
	}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	if len(store.listFilters) != 2 {
		t.Fatalf("List calls = %d, want 2", len(store.listFilters))
	}
	filter := store.listFilters[0]
	if !filter.IncludeOpen || !filter.IncludeAllReasons || !filter.IncludeBackfilled {
		t.Fatalf("filter = %#v, want include flags propagated", filter)
	}
	exclusionFilter := store.listFilters[1]
	if !exclusionFilter.OmitChildren {
		t.Fatalf("exclusion filter = %#v, want child hydration omitted", exclusionFilter)
	}
	runs := resp.Msg.GetRuns()
	if len(runs) != 1 {
		t.Fatalf("runs len = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.GetWallClockMs() != int64((10*time.Minute)/time.Millisecond) {
		t.Fatalf("wall clock ms = %d, want 600000", run.GetWallClockMs())
	}
	if run.GetParentOnlyMs() != int64((6*time.Minute)/time.Millisecond) {
		t.Fatalf("parent-only ms = %d, want 360000", run.GetParentOnlyMs())
	}
	if !run.GetHasParallelism() || run.GetParallelism() != 1 {
		t.Fatalf("parallelism = %f has=%v, want 1/true", run.GetParallelism(), run.GetHasParallelism())
	}
	agg := resp.Msg.GetAggregate()
	if agg.GetRunCount() != 1 || agg.GetSessionCount() != 1 || agg.MedianRunWallClockMs == nil {
		t.Fatalf("aggregate = %#v, want one run/session with medians", agg)
	}
}

func TestGetRunCostReportsExcludedBackfilledAndOpenRuns(t *testing.T) {
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(time.Minute)
	store := &fakeAgentRunStore{runs: []db.AgentRun{
		{ID: "included", SessionID: "sess-1", AgentSessionID: "agent-1", StartedAt: started, StoppedAt: &stopped, StopReason: db.AgentRunStopClean},
		{ID: "open", SessionID: "sess-1", AgentSessionID: "agent-2", StartedAt: started.Add(time.Minute), StopReason: db.AgentRunStopClean},
		{ID: "backfilled", SessionID: "sess-1", AgentSessionID: "agent-3", StartedAt: started.Add(2 * time.Minute), StoppedAt: &stopped, StopReason: db.AgentRunStopClean, IsBackfilled: true},
	}}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	agg := resp.Msg.GetAggregate()
	if agg.GetBackfilledExcludedCount() != 1 || agg.GetOpenExcludedCount() != 1 {
		t.Fatalf("excluded counts = backfilled %d open %d, want 1/1", agg.GetBackfilledExcludedCount(), agg.GetOpenExcludedCount())
	}
}

func TestGetRunCostExcludesOpenRunsFromDurationMedians(t *testing.T) {
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(10 * time.Minute)
	stoppedPrev := started.Add(-10 * time.Minute)
	store := &fakeAgentRunStore{runs: []db.AgentRun{
		{ID: "open", SessionID: "sess-open", AgentSessionID: "agent-open", StartedAt: started},
		{ID: "closed-same-session", SessionID: "sess-open", AgentSessionID: "agent-open-prev", StartedAt: started.Add(-20 * time.Minute), StoppedAt: &stoppedPrev},
		{ID: "closed", SessionID: "sess-closed", AgentSessionID: "agent-closed", StartedAt: started, StoppedAt: &stopped},
	}}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{ShouldIncludeOpen: true, ShouldIncludeAll: true}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	agg := resp.Msg.GetAggregate()
	if agg.GetRunCount() != 3 || agg.GetSessionCount() != 2 {
		t.Fatalf("aggregate counts = runs %d sessions %d, want 3/2", agg.GetRunCount(), agg.GetSessionCount())
	}
	if agg.GetMedianRunWallClockMs() != int64((10*time.Minute)/time.Millisecond) {
		t.Fatalf("median run wall = %d, want closed-run duration", agg.GetMedianRunWallClockMs())
	}
	if agg.GetMedianSessionWallClockMs() != int64((10*time.Minute)/time.Millisecond) {
		t.Fatalf("median session wall = %d, want closed-session duration", agg.GetMedianSessionWallClockMs())
	}
}

func TestGetRunCostBackfillPathReturnsSummary(t *testing.T) {
	store := &fakeAgentRunStore{}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{ShouldBackfill: true}))
	if err != nil {
		t.Fatalf("GetRunCost backfill: %v", err)
	}
	if !store.backfilled {
		t.Fatal("Backfill was not called")
	}
	if got := resp.Msg.GetBackfillSummary().GetInsertedCount(); got != 2 {
		t.Fatalf("inserted count = %d, want 2", got)
	}
	if len(resp.Msg.GetRuns()) != 0 {
		t.Fatalf("backfill response returned %d runs, want 0", len(resp.Msg.GetRuns()))
	}
}

func TestGetRunCostBackfillRejectsUnsupportedFilters(t *testing.T) {
	store := &fakeAgentRunStore{}
	srv := &Server{agentRuns: store}

	_, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{
		ShouldBackfill:    true,
		AgentName:         "codex",
		ShouldIncludeOpen: true,
	}))
	if err == nil {
		t.Fatal("GetRunCost backfill succeeded, want invalid argument")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want invalid_argument", connect.CodeOf(err))
	}
	if store.backfilled {
		t.Fatal("Backfill was called after invalid request")
	}
}

func TestGetRunCostMapsReviewTelemetryPerRun(t *testing.T) {
	started := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(time.Minute)
	store := &fakeAgentRunStore{runs: []db.AgentRun{
		{
			ID: "recorded", SessionID: "sess-1", AgentSessionID: "agent-1",
			StartedAt: started, StoppedAt: &stopped, StopReason: db.AgentRunStopClean,
			ReviewerDispatchCount: 4,
			TerminalState:         db.AgentRunTerminalReviewReady,
		},
		{
			// A run from a runner whose transcript is not parsed reads as
			// "not recorded", not as a zero-dispatch REVIEW_READY.
			ID: "unrecorded", SessionID: "sess-1", AgentSessionID: "agent-2",
			StartedAt: started, StoppedAt: &stopped, StopReason: db.AgentRunStopClean,
		},
	}}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	runs := resp.Msg.GetRuns()
	if len(runs) != 2 {
		t.Fatalf("runs len = %d, want 2", len(runs))
	}
	if got := runs[0].GetReviewerDispatchCount(); got != 4 {
		t.Errorf("recorded reviewer dispatch count = %d, want 4", got)
	}
	if got := runs[0].GetTerminalState(); got != db.AgentRunTerminalReviewReady {
		t.Errorf("recorded terminal state = %q, want %q", got, db.AgentRunTerminalReviewReady)
	}
	if got := runs[1].GetReviewerDispatchCount(); got != 0 {
		t.Errorf("unrecorded reviewer dispatch count = %d, want 0", got)
	}
	if got := runs[1].GetTerminalState(); got != "" {
		t.Errorf("unrecorded terminal state = %q, want empty", got)
	}
}

func TestGetRunCostAggregatesTerminalStateMixAndMedianDispatches(t *testing.T) {
	started := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(time.Minute)
	store := &fakeAgentRunStore{runs: []db.AgentRun{
		{ID: "r1", SessionID: "s1", AgentSessionID: "a1", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 2, TerminalState: db.AgentRunTerminalReviewReady},
		{ID: "r2", SessionID: "s1", AgentSessionID: "a2", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 4, TerminalState: db.AgentRunTerminalReviewReady},
		{ID: "r3", SessionID: "s1", AgentSessionID: "a3", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 6, TerminalState: db.AgentRunTerminalBlocked},
		// Open runs still carry recorded telemetry, so the mix must count them
		// even though they contribute no duration median.
		{ID: "r4", SessionID: "s1", AgentSessionID: "a4", StartedAt: started, ReviewerDispatchCount: 8, TerminalState: db.AgentRunTerminalPartial},
	}}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{ShouldIncludeOpen: true, ShouldIncludeAll: true}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	agg := resp.Msg.GetAggregate()
	mix := agg.GetTerminalStateMix()
	want := map[string]int64{
		db.AgentRunTerminalReviewReady: 2,
		db.AgentRunTerminalBlocked:     1,
		db.AgentRunTerminalPartial:     1,
	}
	if len(mix) != len(want) {
		t.Fatalf("terminal state mix = %v, want %v", mix, want)
	}
	for state, count := range want {
		if mix[state] != count {
			t.Errorf("terminal state mix[%q] = %d, want %d", state, mix[state], count)
		}
	}
	if agg.MedianReviewerDispatchCount == nil {
		t.Fatal("median reviewer dispatch count is nil, want a median over four runs")
	}
	if got := agg.GetMedianReviewerDispatchCount(); got != 5 {
		t.Errorf("median reviewer dispatch count = %d, want 5", got)
	}
}

func TestGetRunCostExcludesUnrecordedRunsFromMedianDispatches(t *testing.T) {
	started := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(time.Minute)
	// Pre-migration rows, unsupported runners and ordinary non-boss-build runs
	// all read back as (0, ""). Folding them in would sink the median to 0 on
	// the strength of runs that carry no dispatch observation at all -- and
	// since List cannot filter to boss-build runs, they normally outnumber the
	// recorded ones.
	store := &fakeAgentRunStore{runs: []db.AgentRun{
		{ID: "r1", SessionID: "s1", AgentSessionID: "a1", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 2, TerminalState: db.AgentRunTerminalReviewReady},
		{ID: "r2", SessionID: "s1", AgentSessionID: "a2", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 4, TerminalState: db.AgentRunTerminalReviewReady},
		{ID: "r3", SessionID: "s1", AgentSessionID: "a3", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 6, TerminalState: db.AgentRunTerminalBlocked},
		{ID: "u1", SessionID: "s1", AgentSessionID: "b1", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 0, TerminalState: db.AgentRunTerminalUnrecorded},
		{ID: "u2", SessionID: "s1", AgentSessionID: "b2", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 0, TerminalState: db.AgentRunTerminalUnrecorded},
		{ID: "u3", SessionID: "s1", AgentSessionID: "b3", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 0, TerminalState: db.AgentRunTerminalUnrecorded},
		{ID: "u4", SessionID: "s1", AgentSessionID: "b4", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 0, TerminalState: db.AgentRunTerminalUnrecorded},
	}}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{ShouldIncludeAll: true}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	agg := resp.Msg.GetAggregate()
	if agg.MedianReviewerDispatchCount == nil {
		t.Fatal("median reviewer dispatch count is nil, want a median over the three recorded runs")
	}
	if got := agg.GetMedianReviewerDispatchCount(); got != 4 {
		t.Errorf("median reviewer dispatch count = %d, want 4 (median of 2,4,6 with the unrecorded rows excluded)", got)
	}
	// The mix keeps them: "" is a named bucket that renders as "not recorded",
	// so an unrecorded run stays visible there rather than being absorbed.
	if got := agg.GetTerminalStateMix()[db.AgentRunTerminalUnrecorded]; got != 4 {
		t.Errorf("terminal state mix[unrecorded] = %d, want 4", got)
	}
}

func TestGetRunCostKeepsGenuineZeroDispatchesInMedian(t *testing.T) {
	started := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(time.Minute)
	// A recorded run that dispatched no reviewers is a real measurement, not an
	// absence: it is the (0, recorded) half of the pair, and it must survive.
	store := &fakeAgentRunStore{runs: []db.AgentRun{
		{ID: "z1", SessionID: "s1", AgentSessionID: "a1", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 0, TerminalState: db.AgentRunTerminalNoChange},
		{ID: "z2", SessionID: "s1", AgentSessionID: "a2", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 0, TerminalState: db.AgentRunTerminalNoChange},
		{ID: "z3", SessionID: "s1", AgentSessionID: "a3", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 4, TerminalState: db.AgentRunTerminalReviewReady},
	}}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{ShouldIncludeAll: true}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	agg := resp.Msg.GetAggregate()
	if agg.MedianReviewerDispatchCount == nil {
		t.Fatal("median reviewer dispatch count is nil, want recorded zeros to count")
	}
	if got := agg.GetMedianReviewerDispatchCount(); got != 0 {
		t.Errorf("median reviewer dispatch count = %d, want 0 (median of 0,0,4 -- recorded zeros are measurements)", got)
	}
}

func TestGetRunCostOmitsMedianDispatchesWhenEveryRunIsUnrecorded(t *testing.T) {
	started := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(time.Minute)
	store := &fakeAgentRunStore{runs: []db.AgentRun{
		{ID: "u1", SessionID: "s1", AgentSessionID: "a1", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 0, TerminalState: db.AgentRunTerminalUnrecorded},
		{ID: "u2", SessionID: "s1", AgentSessionID: "a2", StartedAt: started, StoppedAt: &stopped, ReviewerDispatchCount: 0, TerminalState: db.AgentRunTerminalUnrecorded},
	}}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{ShouldIncludeAll: true}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	agg := resp.Msg.GetAggregate()
	// Nothing was observed, so the honest rendering is `n/a`, not a confident 0.
	if agg.MedianReviewerDispatchCount != nil {
		t.Errorf("median reviewer dispatch count = %d, want unset when no run carries review telemetry", agg.GetMedianReviewerDispatchCount())
	}
}

func TestGetRunCostOmitsReviewMediansWithoutRuns(t *testing.T) {
	store := &fakeAgentRunStore{}
	srv := &Server{agentRuns: store}

	resp, err := srv.GetRunCost(context.Background(), connect.NewRequest(&bossanovav1.GetRunCostRequest{}))
	if err != nil {
		t.Fatalf("GetRunCost: %v", err)
	}
	agg := resp.Msg.GetAggregate()
	if agg.MedianReviewerDispatchCount != nil {
		t.Errorf("median reviewer dispatch count = %d, want unset with no runs", agg.GetMedianReviewerDispatchCount())
	}
	if len(agg.GetTerminalStateMix()) != 0 {
		t.Errorf("terminal state mix = %v, want empty with no runs", agg.GetTerminalStateMix())
	}
}
