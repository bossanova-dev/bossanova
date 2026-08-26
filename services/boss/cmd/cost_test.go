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
		}},
		Aggregate: &pb.RunCostAggregate{RunCount: 1, SessionCount: 1, BackfilledIncludedCount: 1},
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
