package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	costQueryTimeout    = 10 * time.Second
	costBackfillTimeout = 30 * time.Minute
)

func costCmd() *cobra.Command {
	var agentName, repoName, sinceText, untilText string
	var includeOpen, includeAll, includeBackfilled, backfill bool
	cmd := &cobra.Command{
		Use:   "cost [session-id]",
		Short: "Report agent run cost telemetry",
		Long: "Report daemon-recorded agent run cost telemetry.\n\n" +
			"`boss tail` shows raw service and agent logs, but it has no per-turn structure and cannot answer wall-clock, token, model-call, subagent, or parent-only-time questions.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCost(cmd, args, costOptions{
				AgentName:         agentName,
				RepoDisplayName:   repoName,
				Since:             sinceText,
				Until:             untilText,
				IncludeOpen:       includeOpen,
				IncludeAll:        includeAll,
				IncludeBackfilled: includeBackfilled,
				Backfill:          backfill,
			})
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "only include runs for this agent")
	cmd.Flags().StringVar(&repoName, "repo", "", "only include runs for this repo display name")
	cmd.Flags().StringVar(&sinceText, "since", "", "only include runs starting at or after this RFC3339 time or YYYY-MM-DD date")
	cmd.Flags().StringVar(&untilText, "until", "", "only include runs starting before this RFC3339 time or YYYY-MM-DD date")
	cmd.Flags().BoolVar(&includeOpen, "include-open", false, "include runs without a recorded stop")
	cmd.Flags().BoolVar(&includeAll, "include-all", false, "include daemon_restart and unknown stop reasons")
	cmd.Flags().BoolVar(&includeBackfilled, "include-backfilled", false, "include backfilled rows in aggregate medians")
	cmd.Flags().BoolVar(&backfill, "backfill", false, "backfill local transcript archives into the daemon run-cost table")
	_ = cmd.Flags().MarkHidden("backfill")
	cmd.Flags().Bool(jsonFlagName, false, "emit JSON")
	return cmd
}

type costOptions struct {
	AgentName         string
	RepoDisplayName   string
	Since             string
	Until             string
	IncludeOpen       bool
	IncludeAll        bool
	IncludeBackfilled bool
	Backfill          bool
}

type runCostClient interface {
	client.BossClient
	GetRunCost(ctx context.Context, req *pb.GetRunCostRequest) (*pb.GetRunCostResponse, error)
}

func runCost(cmd *cobra.Command, args []string, opts costOptions) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)
	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	rc, ok := c.(runCostClient)
	if !ok {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("client does not support run cost telemetry"))
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), costRequestTimeout(opts))
	defer cancel()

	var sessionID string
	if len(args) == 1 {
		sessionID, err = resolveSessionID(c, ctx, args[0])
		if err != nil {
			return emitJSONFailure(cmd, asJSON, err)
		}
	}
	since, err := parseCostTime(opts.Since)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	until, err := parseCostTime(opts.Until)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	req := &pb.GetRunCostRequest{
		SessionId:               sessionID,
		AgentName:               opts.AgentName,
		RepoDisplayName:         opts.RepoDisplayName,
		Since:                   timestampOrNil(since),
		Until:                   timestampOrNil(until),
		ShouldIncludeOpen:       opts.IncludeOpen,
		ShouldIncludeAll:        opts.IncludeAll,
		ShouldIncludeBackfilled: opts.IncludeBackfilled,
		ShouldBackfill:          opts.Backfill,
	}
	if opts.Backfill {
		if err := validateCostBackfillRequest(req); err != nil {
			return emitJSONFailure(cmd, asJSON, err)
		}
	}
	resp, err := rc.GetRunCost(ctx, req)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	if resp.Runs == nil {
		resp.Runs = []*pb.AgentRunCost{}
	}
	if asJSON {
		return emitJSON(cmd, runCostResponseToJSON(resp))
	}
	if opts.Backfill {
		sum := resp.GetBackfillSummary()
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backfill complete: inserted %d, skipped %d\n", sum.GetInsertedCount(), sum.GetSkippedCount())
		return nil
	}
	if sessionID != "" {
		renderSingleRunCosts(cmd, resp.GetRuns())
		return nil
	}
	renderAggregateRunCost(cmd, resp.GetAggregate())
	return nil
}

func costRequestTimeout(opts costOptions) time.Duration {
	if opts.Backfill {
		return costBackfillTimeout
	}
	return costQueryTimeout
}

func renderSingleRunCosts(cmd *cobra.Command, runs []*pb.AgentRunCost) {
	out := cmd.OutOrStdout()
	if len(runs) == 0 {
		_, _ = fmt.Fprintln(out, "No agent runs recorded for this session.")
		return
	}
	for _, run := range runs {
		_, _ = fmt.Fprintf(out, "%s  %s  %s\n", run.GetId(), run.GetAgentName(), runCostDuration(run.GetWallClockMs()))
		_, _ = fmt.Fprintf(out, "  stop: %s%s\n", run.GetStopReason(), openSuffix(run.GetIsOpen()))
		_, _ = fmt.Fprintf(out, "  model: %s  effort: %s\n", notRecorded(run.GetModel(), "model"), notRecorded(run.GetEffort(), "requires BOS-1031"))
		_, _ = fmt.Fprintf(out, "  model calls: parent %d, child %d  tools: %d  subagents: %d direct / %d total\n",
			run.GetParentModelCallCount(), run.GetChildModelCallCount(), run.GetToolCallCount(), run.GetDirectSubagentCount(), run.GetSubagentCount())
		_, _ = fmt.Fprintf(out, "  tokens: output %s, reasoning %s\n", optionalIntString(run.OutputTokenCount), optionalIntString(run.ReasoningTokenCount))
		if run.GetHasParallelism() {
			_, _ = fmt.Fprintf(out, "  parallelism: %.2fx with %.0f%% child coverage  parent-only: %s\n",
				run.GetParallelism(), run.GetChildCoverage()*100, runCostDuration(run.GetParentOnlyMs()))
		} else {
			_, _ = fmt.Fprintf(out, "  parallelism: not applicable with 0%% child coverage  parent-only: %s\n", runCostDuration(run.GetParentOnlyMs()))
		}
	}
}

func renderAggregateRunCost(cmd *cobra.Command, agg *pb.RunCostAggregate) {
	out := cmd.OutOrStdout()
	if agg == nil || agg.GetRunCount() == 0 {
		_, _ = fmt.Fprintln(out, "No agent runs matched the filters.")
		renderExcludedRunHint(out, agg)
		return
	}
	_, _ = fmt.Fprintf(out, "Runs: %d across %d sessions\n", agg.GetRunCount(), agg.GetSessionCount())
	_, _ = fmt.Fprintf(out, "Median wall clock per session: %s\n", optionalDurationString(agg.MedianSessionWallClockMs))
	_, _ = fmt.Fprintf(out, "Median wall clock per agent run: %s\n", optionalDurationString(agg.MedianRunWallClockMs))
	_, _ = fmt.Fprintf(out, "Median parent-only per session: %s\n", optionalDurationString(agg.MedianSessionParentOnlyMs))
	_, _ = fmt.Fprintf(out, "Median parent-only per agent run: %s\n", optionalDurationString(agg.MedianRunParentOnlyMs))
	_, _ = fmt.Fprintf(out, "Median parallelism: %s\n", optionalFloatString(agg.MedianParallelism))
	_, _ = fmt.Fprintf(out, "Backfilled rows included: %d; excluded by default unless --include-backfilled: %d\n",
		agg.GetBackfilledIncludedCount(), agg.GetBackfilledExcludedCount())
	_, _ = fmt.Fprintf(out, "Open rows excluded by default unless --include-open: %d\n", agg.GetOpenExcludedCount())
	_, _ = fmt.Fprintf(out, "Effort not recorded: %d; review ledger not recorded: %d\n", agg.GetNoEffortCount(), agg.GetNoReviewRoundLedgerCount())
}

func renderExcludedRunHint(out interface{ Write([]byte) (int, error) }, agg *pb.RunCostAggregate) {
	if agg == nil {
		return
	}
	if agg.GetOpenExcludedCount() > 0 {
		_, _ = fmt.Fprintf(out, "Open rows excluded by default unless --include-open: %d\n", agg.GetOpenExcludedCount())
	}
	if agg.GetBackfilledExcludedCount() > 0 {
		_, _ = fmt.Fprintf(out, "Backfilled rows excluded by default unless --include-backfilled: %d\n", agg.GetBackfilledExcludedCount())
	}
}

func validateCostBackfillRequest(req *pb.GetRunCostRequest) error {
	if req.GetSessionId() != "" || req.GetAgentName() != "" || req.GetRepoDisplayName() != "" ||
		req.GetShouldIncludeOpen() || req.GetShouldIncludeAll() || req.GetShouldIncludeBackfilled() {
		return fmt.Errorf("--backfill only supports --since and --until filters")
	}
	return nil
}

type runCostJSON struct {
	Runs            []agentRunCostJSON       `json:"runs"`
	Aggregate       runCostAggregateJSON     `json:"aggregate"`
	BackfillSummary runCostBackfillSummaryJS `json:"backfill_summary"`
}

type agentRunCostJSON struct {
	ID                   string   `json:"id"`
	SessionID            string   `json:"session_id"`
	AgentSessionID       string   `json:"agent_session_id"`
	AgentName            string   `json:"agent_name"`
	RepoDisplayName      string   `json:"repo_display_name"`
	Model                string   `json:"model"`
	Effort               string   `json:"effort"`
	StartedAt            string   `json:"started_at"`
	StoppedAt            *string  `json:"stopped_at"`
	StopReason           string   `json:"stop_reason"`
	IsOpen               bool     `json:"is_open"`
	IsBackfilled         bool     `json:"is_backfilled"`
	WallClockMs          int64    `json:"wall_clock_ms"`
	ParentOnlyMs         int64    `json:"parent_only_ms"`
	Parallelism          *float64 `json:"parallelism"`
	ChildCoverage        *float64 `json:"child_coverage"`
	ParentModelCallCount int64    `json:"parent_model_call_count"`
	ChildModelCallCount  int64    `json:"child_model_call_count"`
	ToolCallCount        int64    `json:"tool_call_count"`
	SubagentCount        int64    `json:"subagent_count"`
	DirectSubagentCount  int64    `json:"direct_subagent_count"`
	OutputTokenCount     *int64   `json:"output_token_count"`
	ReasoningTokenCount  *int64   `json:"reasoning_token_count"`
}

type runCostAggregateJSON struct {
	RunCount                  int64    `json:"run_count"`
	SessionCount              int64    `json:"session_count"`
	BackfilledExcludedCount   int64    `json:"backfilled_excluded_count"`
	BackfilledIncludedCount   int64    `json:"backfilled_included_count"`
	OpenExcludedCount         int64    `json:"open_excluded_count"`
	NoReviewRoundLedgerCount  int64    `json:"no_review_round_ledger_count"`
	NoEffortCount             int64    `json:"no_effort_count"`
	MedianSessionWallClockMs  *int64   `json:"median_session_wall_clock_ms"`
	MedianRunWallClockMs      *int64   `json:"median_run_wall_clock_ms"`
	MedianSessionParentOnlyMs *int64   `json:"median_session_parent_only_ms"`
	MedianRunParentOnlyMs     *int64   `json:"median_run_parent_only_ms"`
	MedianParallelism         *float64 `json:"median_parallelism"`
}

type runCostBackfillSummaryJS struct {
	InsertedCount int64 `json:"inserted_count"`
	SkippedCount  int64 `json:"skipped_count"`
}

func runCostResponseToJSON(resp *pb.GetRunCostResponse) runCostJSON {
	out := runCostJSON{
		Runs:            []agentRunCostJSON{},
		Aggregate:       runCostAggregateToJSON(resp.GetAggregate()),
		BackfillSummary: runCostBackfillSummaryToJSON(resp.GetBackfillSummary()),
	}
	for _, run := range resp.GetRuns() {
		out.Runs = append(out.Runs, agentRunCostToJSON(run))
	}
	return out
}

func agentRunCostToJSON(run *pb.AgentRunCost) agentRunCostJSON {
	out := agentRunCostJSON{
		ID:                   run.GetId(),
		SessionID:            run.GetSessionId(),
		AgentSessionID:       run.GetAgentSessionId(),
		AgentName:            run.GetAgentName(),
		RepoDisplayName:      run.GetRepoDisplayName(),
		Model:                run.GetModel(),
		Effort:               run.GetEffort(),
		StartedAt:            protoTimestampString(run.GetStartedAt()),
		StopReason:           run.GetStopReason(),
		IsOpen:               run.GetIsOpen(),
		IsBackfilled:         run.GetIsBackfilled(),
		WallClockMs:          run.GetWallClockMs(),
		ParentOnlyMs:         run.GetParentOnlyMs(),
		ParentModelCallCount: run.GetParentModelCallCount(),
		ChildModelCallCount:  run.GetChildModelCallCount(),
		ToolCallCount:        run.GetToolCallCount(),
		SubagentCount:        run.GetSubagentCount(),
		DirectSubagentCount:  run.GetDirectSubagentCount(),
		OutputTokenCount:     run.OutputTokenCount,
		ReasoningTokenCount:  run.ReasoningTokenCount,
	}
	if run.GetStoppedAt() != nil {
		stopped := protoTimestampString(run.GetStoppedAt())
		out.StoppedAt = &stopped
	}
	if run.GetHasParallelism() {
		v := run.GetParallelism()
		out.Parallelism = &v
	}
	if run.GetHasChildCoverage() {
		v := run.GetChildCoverage()
		out.ChildCoverage = &v
	}
	return out
}

func runCostAggregateToJSON(agg *pb.RunCostAggregate) runCostAggregateJSON {
	return runCostAggregateJSON{
		RunCount:                  agg.GetRunCount(),
		SessionCount:              agg.GetSessionCount(),
		BackfilledExcludedCount:   agg.GetBackfilledExcludedCount(),
		BackfilledIncludedCount:   agg.GetBackfilledIncludedCount(),
		OpenExcludedCount:         agg.GetOpenExcludedCount(),
		NoReviewRoundLedgerCount:  agg.GetNoReviewRoundLedgerCount(),
		NoEffortCount:             agg.GetNoEffortCount(),
		MedianSessionWallClockMs:  agg.MedianSessionWallClockMs,
		MedianRunWallClockMs:      agg.MedianRunWallClockMs,
		MedianSessionParentOnlyMs: agg.MedianSessionParentOnlyMs,
		MedianRunParentOnlyMs:     agg.MedianRunParentOnlyMs,
		MedianParallelism:         agg.MedianParallelism,
	}
}

func runCostBackfillSummaryToJSON(summary *pb.RunCostBackfillSummary) runCostBackfillSummaryJS {
	return runCostBackfillSummaryJS{
		InsertedCount: summary.GetInsertedCount(),
		SkippedCount:  summary.GetSkippedCount(),
	}
}

func protoTimestampString(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().Format(time.RFC3339Nano)
}

func parseCostTime(value string) (*time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return &t, nil
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, fmt.Errorf("parse time %q: use RFC3339 or YYYY-MM-DD", value)
	}
	return &t, nil
}

func timestampOrNil(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func runCostDuration(ms int64) string {
	if ms <= 0 {
		return "0s"
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

func optionalDurationString(v *int64) string {
	if v == nil {
		return "n/a"
	}
	return runCostDuration(*v)
}

func optionalIntString(v *int64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d", *v)
}

func optionalFloatString(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2fx", *v)
}

func notRecorded(value, reason string) string {
	if value != "" {
		return value
	}
	return "not recorded (" + reason + ")"
}

func openSuffix(open bool) string {
	if open {
		return " (open)"
	}
	return ""
}
