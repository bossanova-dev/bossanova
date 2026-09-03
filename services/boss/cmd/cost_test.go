package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRunCostResponseToJSONUsesStableSchema(t *testing.T) {
	started := time.Date(2026, 8, 26, 1, 0, 0, 123, time.UTC)
	stopped := started.Add(time.Minute)
	outputTokens := int64(10)
	reasoningTokens := int64(3)
	parallelism := 1.5
	coverage := 0.25
	medianDispatches := int64(4)
	resp := &pb.GetRunCostResponse{
		Runs: []*pb.AgentRunCost{{
			Id:                   "run-1",
			SessionId:            "sess-1",
			AgentSessionId:       "agent-1",
			AgentName:            "codex",
			RepoDisplayName:      "bossanova",
			Model:                "gpt-5",
			Effort:               "high",
			StartedAt:            timestamppb.New(started),
			StoppedAt:            timestamppb.New(stopped),
			StopReason:           "clean",
			IsBackfilled:         true,
			WallClockMs:          60000,
			ParentOnlyMs:         45000,
			Parallelism:          parallelism,
			HasParallelism:       true,
			ChildCoverage:        coverage,
			HasChildCoverage:     true,
			ParentModelCallCount: 2,
			ChildModelCallCount:  1,
			ToolCallCount:        4,
			SubagentCount:        2,
			DirectSubagentCount:  1,
			OutputTokenCount:     &outputTokens,
			ReasoningTokenCount:  &reasoningTokens,

			ReviewerDispatchCount: 4,
			TerminalState:         "REVIEW_READY",
		}},
		Aggregate: &pb.RunCostAggregate{
			RunCount: 1, SessionCount: 1, BackfilledIncludedCount: 1,
			TerminalStateMix:            map[string]int64{"REVIEW_READY": 1},
			MedianReviewerDispatchCount: &medianDispatches,
		},
		BackfillSummary: &pb.RunCostBackfillSummary{
			InsertedCount: 2,
			SkippedCount:  3,
		},
	}

	data, err := json.Marshal(runCostResponseToJSON(resp))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	runs := got["runs"].([]any)
	run := runs[0].(map[string]any)
	if run["started_at"] != started.Format(time.RFC3339Nano) {
		t.Fatalf("started_at = %v, want RFC3339 string", run["started_at"])
	}
	if _, ok := run["started_at"].(map[string]any); ok {
		t.Fatal("started_at encoded as protobuf object")
	}
	if _, ok := got["backfill_summary"].(map[string]any)["inserted_count"]; !ok {
		t.Fatalf("backfill_summary = %#v, want snake_case inserted_count", got["backfill_summary"])
	}
	if run["reviewer_dispatch_count"] != float64(4) {
		t.Fatalf("reviewer_dispatch_count = %v, want 4", run["reviewer_dispatch_count"])
	}
	if run["terminal_state"] != "REVIEW_READY" {
		t.Fatalf("terminal_state = %v, want REVIEW_READY", run["terminal_state"])
	}
	agg := got["aggregate"].(map[string]any)
	mix, ok := agg["terminal_state_mix"].(map[string]any)
	if !ok || mix["REVIEW_READY"] != float64(1) {
		t.Fatalf("terminal_state_mix = %#v, want {REVIEW_READY: 1}", agg["terminal_state_mix"])
	}
	if agg["median_reviewer_dispatch_count"] != float64(4) {
		t.Fatalf("median_reviewer_dispatch_count = %v, want 4", agg["median_reviewer_dispatch_count"])
	}
}

func TestValidateCostBackfillRequestRejectsUnsupportedFilters(t *testing.T) {
	err := validateCostBackfillRequest(&pb.GetRunCostRequest{
		SessionId:      "sess-1",
		ShouldBackfill: true,
	})
	if err == nil || !strings.Contains(err.Error(), "--backfill only supports --since and --until") {
		t.Fatalf("validateCostBackfillRequest err = %v, want unsupported filters error", err)
	}
}

func TestCostRequestTimeoutExtendsBackfillRequests(t *testing.T) {
	if got := costRequestTimeout(costOptions{}); got != 10*time.Second {
		t.Fatalf("normal timeout = %s, want 10s", got)
	}
	if got := costRequestTimeout(costOptions{Backfill: true}); got != 30*time.Minute {
		t.Fatalf("backfill timeout = %s, want 30m", got)
	}
}

func TestRenderAggregateRunCostShowsExcludedRowsWhenEmpty(t *testing.T) {
	cmd := &cobra.Command{Use: "cost"}
	var out bytes.Buffer
	cmd.SetOut(&out)

	renderAggregateRunCost(cmd, &pb.RunCostAggregate{
		BackfilledExcludedCount: 2,
		OpenExcludedCount:       1,
	})

	text := out.String()
	if !strings.Contains(text, "--include-open: 1") || !strings.Contains(text, "--include-backfilled: 2") {
		t.Fatalf("output = %q, want excluded row hints", text)
	}
}

func TestRenderSingleRunCostsShowsReviewTelemetry(t *testing.T) {
	cmd := &cobra.Command{Use: "cost"}
	var out bytes.Buffer
	cmd.SetOut(&out)

	started := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	renderSingleRunCosts(cmd, []*pb.AgentRunCost{{
		Id:                    "run-1",
		SessionId:             "sess-1",
		StartedAt:             timestamppb.New(started),
		StoppedAt:             timestamppb.New(started.Add(time.Minute)),
		ReviewerDispatchCount: 4,
		TerminalState:         "REVIEW_READY",
	}})

	text := out.String()
	if !strings.Contains(text, "reviewer dispatches: 4") {
		t.Errorf("output = %q, want reviewer dispatch count", text)
	}
	if !strings.Contains(text, "REVIEW_READY") {
		t.Errorf("output = %q, want terminal state", text)
	}
}

func TestRenderSingleRunCostsMarksUnrecordedTerminalState(t *testing.T) {
	cmd := &cobra.Command{Use: "cost"}
	var out bytes.Buffer
	cmd.SetOut(&out)

	started := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	renderSingleRunCosts(cmd, []*pb.AgentRunCost{{
		Id:        "run-1",
		SessionId: "sess-1",
		StartedAt: timestamppb.New(started),
		StoppedAt: timestamppb.New(started.Add(time.Minute)),
	}})

	// A run that printed no terminal state must not read as a blank state that
	// happens to render as empty space next to a real one.
	if text := out.String(); !strings.Contains(text, "no terminal state printed") {
		t.Errorf("output = %q, want an explicit not-recorded reason", text)
	}
}

// TestRenderSingleRunCostsDistinguishesZeroDispatchesFromNoTelemetry pins the one
// case reviewer_dispatch_count cannot express on the wire. It is a plain int64 with
// no not-recorded sentinel, so a run predating this telemetry reads back as 0 and
// would otherwise print as a confident "0 dispatches". Its paired terminal_state
// does carry a sentinel, so the pair decides it.
func TestRenderSingleRunCostsDistinguishesZeroDispatchesFromNoTelemetry(t *testing.T) {
	started := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)

	// Both zero-valued: the run never reached the telemetry path.
	cmd := &cobra.Command{Use: "cost"}
	var out bytes.Buffer
	cmd.SetOut(&out)
	renderSingleRunCosts(cmd, []*pb.AgentRunCost{{
		Id:        "run-1",
		SessionId: "sess-1",
		StartedAt: timestamppb.New(started),
		StoppedAt: timestamppb.New(started.Add(time.Minute)),
	}})
	text := out.String()
	if strings.Contains(text, "reviewer dispatches: 0") {
		t.Errorf("output = %q, want no confident zero for a run carrying no review telemetry", text)
	}
	if !strings.Contains(text, "reviewer dispatches: not recorded") {
		t.Errorf("output = %q, want the dispatch count marked not recorded", text)
	}

	// A recorded terminal state proves the run reached the telemetry path, so a
	// zero count there is a real measurement and must render as one.
	cmd2 := &cobra.Command{Use: "cost"}
	var out2 bytes.Buffer
	cmd2.SetOut(&out2)
	renderSingleRunCosts(cmd2, []*pb.AgentRunCost{{
		Id:            "run-2",
		SessionId:     "sess-1",
		StartedAt:     timestamppb.New(started),
		StoppedAt:     timestamppb.New(started.Add(time.Minute)),
		TerminalState: "NO_CHANGE",
	}})
	if text := out2.String(); !strings.Contains(text, "reviewer dispatches: 0") {
		t.Errorf("output = %q, want a trustworthy zero beside a recorded terminal state", text)
	}
}

func TestRenderAggregateRunCostShowsTerminalStateMix(t *testing.T) {
	cmd := &cobra.Command{Use: "cost"}
	var out bytes.Buffer
	cmd.SetOut(&out)

	median := int64(3)
	renderAggregateRunCost(cmd, &pb.RunCostAggregate{
		RunCount:                    3,
		TerminalStateMix:            map[string]int64{"REVIEW_READY": 2, "": 1},
		MedianReviewerDispatchCount: &median,
	})

	text := out.String()
	if !strings.Contains(text, "REVIEW_READY 2") {
		t.Errorf("output = %q, want the terminal state mix", text)
	}
	if !strings.Contains(text, "not recorded 1") {
		t.Errorf("output = %q, want unrecorded runs labelled", text)
	}
	if !strings.Contains(text, "Median reviewer dispatches per run: 3") {
		t.Errorf("output = %q, want the median reviewer dispatches", text)
	}
}

func TestTerminalStateMixStringOrdersStatesForReading(t *testing.T) {
	got := terminalStateMixString(map[string]int64{
		"":             1,
		"NO_CHANGE":    2,
		"REVIEW_READY": 3,
		"BLOCKED":      4,
		"PARTIAL":      5,
		"WEIRD":        6,
	})
	// Fixed lifecycle order first so two runs of `boss cost` are comparable by
	// eye; unknown tokens sort after it rather than being dropped.
	want := "REVIEW_READY 3, PARTIAL 5, BLOCKED 4, NO_CHANGE 2, not recorded 1, WEIRD 6"
	if got != want {
		t.Fatalf("terminalStateMixString = %q, want %q", got, want)
	}
}

func TestTerminalStateMixStringWithoutRuns(t *testing.T) {
	if got := terminalStateMixString(nil); got != "n/a" {
		t.Fatalf("terminalStateMixString(nil) = %q, want n/a", got)
	}
}
