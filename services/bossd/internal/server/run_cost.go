package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/runmetrics"
)

func (s *Server) GetRunCost(ctx context.Context, req *connect.Request[bossanovav1.GetRunCostRequest]) (*connect.Response[bossanovav1.GetRunCostResponse], error) {
	if s.agentRuns == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent run store not configured"))
	}
	since := protoTimePtr(req.Msg.GetSince())
	until := protoTimePtr(req.Msg.GetUntil())
	if req.Msg.GetShouldBackfill() {
		if err := validateRunCostBackfillRequest(req.Msg); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		summary, err := s.agentRuns.Backfill(ctx, db.AgentRunBackfillParams{Since: since, Until: until})
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("backfill run cost: %w", err))
		}
		return connect.NewResponse(&bossanovav1.GetRunCostResponse{
			Runs:      []*bossanovav1.AgentRunCost{},
			Aggregate: &bossanovav1.RunCostAggregate{},
			BackfillSummary: &bossanovav1.RunCostBackfillSummary{
				InsertedCount: summary.InsertedCount,
				SkippedCount:  summary.SkippedCount,
			},
		}), nil
	}
	runs, err := s.agentRuns.List(ctx, db.AgentRunFilter{
		SessionID:         req.Msg.GetSessionId(),
		AgentName:         req.Msg.GetAgentName(),
		RepoDisplayName:   req.Msg.GetRepoDisplayName(),
		Since:             since,
		Until:             until,
		IncludeOpen:       req.Msg.GetShouldIncludeOpen(),
		IncludeAllReasons: req.Msg.GetShouldIncludeAll(),
		IncludeBackfilled: req.Msg.GetShouldIncludeBackfilled(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list run cost: %w", err))
	}
	exclusionRuns, err := s.agentRuns.List(ctx, db.AgentRunFilter{
		SessionID:         req.Msg.GetSessionId(),
		AgentName:         req.Msg.GetAgentName(),
		RepoDisplayName:   req.Msg.GetRepoDisplayName(),
		Since:             since,
		Until:             until,
		IncludeOpen:       true,
		IncludeAllReasons: true,
		IncludeBackfilled: true,
		OmitChildren:      true,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list excluded run cost: %w", err))
	}
	resp := &bossanovav1.GetRunCostResponse{
		Runs:            make([]*bossanovav1.AgentRunCost, 0, len(runs)),
		Aggregate:       aggregateRunCosts(runs, exclusionCounts(exclusionRuns, req.Msg)),
		BackfillSummary: &bossanovav1.RunCostBackfillSummary{},
	}
	for _, run := range runs {
		resp.Runs = append(resp.Runs, runCostToProto(run))
	}
	return connect.NewResponse(resp), nil
}

func validateRunCostBackfillRequest(req *bossanovav1.GetRunCostRequest) error {
	if req.GetSessionId() != "" || req.GetAgentName() != "" || req.GetRepoDisplayName() != "" ||
		req.GetShouldIncludeOpen() || req.GetShouldIncludeAll() || req.GetShouldIncludeBackfilled() {
		return fmt.Errorf("backfill only supports since and until filters")
	}
	return nil
}

func runCostToProto(run db.AgentRun) *bossanovav1.AgentRunCost {
	stoppedAt := time.Time{}
	if run.StoppedAt != nil {
		stoppedAt = *run.StoppedAt
	}
	childSpans := directChildSpans(run, stoppedAt)
	parent := runmetrics.Span{Start: run.StartedAt, Stop: stoppedAt}
	parallelism, hasParallelism := runmetrics.Parallelism(parent, childSpans)
	coverage, hasCoverage := runmetrics.Coverage(parent, childSpans)
	out := &bossanovav1.AgentRunCost{
		Id:                   run.ID,
		SessionId:            run.SessionID,
		AgentSessionId:       run.AgentSessionID,
		AgentName:            run.AgentName,
		RepoDisplayName:      run.RepoDisplayName,
		Model:                run.Model,
		Effort:               run.Effort,
		StartedAt:            timestamppb.New(run.StartedAt),
		StopReason:           run.StopReason,
		IsOpen:               run.StoppedAt == nil,
		IsBackfilled:         run.IsBackfilled,
		WallClockMs:          runmetrics.Duration(run.StartedAt, stoppedAt).Milliseconds(),
		ParentOnlyMs:         runmetrics.ParentOnlyDuration(parent, childSpans).Milliseconds(),
		Parallelism:          parallelism,
		HasParallelism:       hasParallelism,
		ChildCoverage:        coverage,
		HasChildCoverage:     hasCoverage,
		ParentModelCallCount: run.ParentModelCallCount,
		ChildModelCallCount:  run.ChildModelCallCount,
		ToolCallCount:        run.ToolCallCount,
		SubagentCount:        run.SubagentCount,
		DirectSubagentCount:  run.DirectSubagentCount,
		OutputTokenCount:     run.OutputTokenCount,
		ReasoningTokenCount:  run.ReasoningTokenCount,

		ReviewerDispatchCount: run.ReviewerDispatchCount,
		TerminalState:         run.TerminalState,
	}
	if run.StoppedAt != nil {
		out.StoppedAt = timestamppb.New(*run.StoppedAt)
	}
	return out
}

type runCostExclusions struct {
	backfilled int64
	open       int64
}

func exclusionCounts(runs []db.AgentRun, req *bossanovav1.GetRunCostRequest) runCostExclusions {
	var out runCostExclusions
	for _, run := range runs {
		if !req.GetShouldIncludeBackfilled() && run.IsBackfilled {
			out.backfilled++
		}
		if !req.GetShouldIncludeOpen() && run.StoppedAt == nil {
			out.open++
		}
	}
	return out
}

func aggregateRunCosts(runs []db.AgentRun, exclusions runCostExclusions) *bossanovav1.RunCostAggregate {
	agg := &bossanovav1.RunCostAggregate{
		RunCount:                 int64(len(runs)),
		NoReviewRoundLedgerCount: int64(len(runs)),
		BackfilledIncludedCount:  0,
		BackfilledExcludedCount:  exclusions.backfilled,
		OpenExcludedCount:        exclusions.open,
		NoEffortCount:            0,
	}
	sessionDurations := map[string]time.Duration{}
	sessionParentOnly := map[string]time.Duration{}
	seenSessions := map[string]struct{}{}
	runDurations := make([]time.Duration, 0, len(runs))
	parentOnlyDurations := make([]time.Duration, 0, len(runs))
	parallelisms := make([]time.Duration, 0, len(runs))
	terminalStates := make([]string, 0, len(runs))
	reviewerDispatches := make([]int64, 0, len(runs))
	for _, run := range runs {
		if run.IsBackfilled {
			agg.BackfilledIncludedCount++
		}
		if run.Effort == "" {
			agg.NoEffortCount++
		}
		if _, ok := seenSessions[run.SessionID]; !ok {
			agg.SessionCount++
			seenSessions[run.SessionID] = struct{}{}
		}
		// Terminal state and reviewer dispatches are recorded telemetry, not
		// derived from a stop time, so an open run still contributes — the mix
		// would otherwise silently drop every run still in flight.
		//
		// The two are appended on different terms. The mix keeps every run: "" is
		// one of its named buckets and renders as "not recorded", so an unrecorded
		// run stays visible as itself. A median has no such bucket -- it is a
		// single number, and an unrecorded run folded into it reads as a measured
		// zero, dragging the median toward 0 on the strength of runs that were
		// never observed. reviewer_dispatch_count carries no not-recorded sentinel
		// of its own, so pair it with terminal_state exactly as `boss cost` does
		// per run (reviewerDispatchString): both zero-valued means the run never
		// reached the telemetry path. A 0 beside a recorded terminal state is a
		// real measurement and is kept.
		terminalStates = append(terminalStates, run.TerminalState)
		if run.ReviewerDispatchCount != 0 || run.TerminalState != db.AgentRunTerminalUnrecorded {
			reviewerDispatches = append(reviewerDispatches, run.ReviewerDispatchCount)
		}
		if run.StoppedAt == nil {
			continue
		}
		stoppedAt := *run.StoppedAt
		parent := runmetrics.Span{Start: run.StartedAt, Stop: stoppedAt}
		children := directChildSpans(run, stoppedAt)
		wall := runmetrics.Duration(run.StartedAt, stoppedAt)
		parentOnly := runmetrics.ParentOnlyDuration(parent, children)
		runDurations = append(runDurations, wall)
		parentOnlyDurations = append(parentOnlyDurations, parentOnly)
		sessionDurations[run.SessionID] += wall
		sessionParentOnly[run.SessionID] += parentOnly
		if p, ok := runmetrics.Parallelism(parent, children); ok {
			parallelisms = append(parallelisms, time.Duration(p*1000))
		}
	}
	setMedianDuration(&agg.MedianRunWallClockMs, runDurations)
	setMedianDuration(&agg.MedianRunParentOnlyMs, parentOnlyDurations)
	setMedianDuration(&agg.MedianSessionWallClockMs, mapDurations(sessionDurations))
	setMedianDuration(&agg.MedianSessionParentOnlyMs, mapDurations(sessionParentOnly))
	if median, ok := runmetrics.MedianDuration(parallelisms); ok {
		v := float64(median) / 1000
		agg.MedianParallelism = &v
	}
	agg.TerminalStateMix = runmetrics.TerminalStateMix(terminalStates)
	setMedianInt64(&agg.MedianReviewerDispatchCount, reviewerDispatches)
	return agg
}

func directChildSpans(run db.AgentRun, parentStop time.Time) []runmetrics.Span {
	spans := make([]runmetrics.Span, 0, len(run.Children))
	for _, child := range run.Children {
		if child.SpawnDepth != 1 {
			continue
		}
		stop := parentStop
		if child.StoppedAt != nil {
			stop = *child.StoppedAt
		}
		spans = append(spans, runmetrics.Span{Start: child.StartedAt, Stop: stop})
	}
	return spans
}

func mapDurations(values map[string]time.Duration) []time.Duration {
	out := make([]time.Duration, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func setMedianInt64(dst **int64, values []int64) {
	if median, ok := runmetrics.MedianInt64(values); ok {
		v := median
		*dst = &v
	}
}

func setMedianDuration(dst **int64, values []time.Duration) {
	if median, ok := runmetrics.MedianDuration(values); ok {
		v := median.Milliseconds()
		*dst = &v
	}
}

func protoTimePtr(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
