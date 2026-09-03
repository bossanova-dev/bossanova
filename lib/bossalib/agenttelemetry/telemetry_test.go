package agenttelemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTallyClaudeCountsAssistantToolsAndTokens(t *testing.T) {
	counts, err := TallyClaude(strings.NewReader(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","usage":{"output_tokens":10,"output_tokens_details":{"thinking_tokens":3}}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"tool_use"}`,
		`{"timestamp":"2026-08-26T01:00:02Z","type":"assistant","usage":{"output_tokens":7}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyClaude: %v", err)
	}
	if counts.ParentModelCallCount != 2 || counts.ToolCallCount != 1 {
		t.Fatalf("counts = parent %d tools %d, want 2/1", counts.ParentModelCallCount, counts.ToolCallCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want 17", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 3 {
		t.Fatalf("reasoning tokens = %v, want 3", counts.ReasoningTokenCount)
	}
}

func TestTallyClaudeCountsNestedAssistantToolUseBlocks(t *testing.T) {
	counts, err := TallyClaude(strings.NewReader(`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","name":"Bash"},{"type":"tool_use","name":"Read"}],"usage":{"output_tokens":10}}}`))
	if err != nil {
		t.Fatalf("TallyClaude: %v", err)
	}
	if counts.ParentModelCallCount != 1 || counts.ToolCallCount != 2 {
		t.Fatalf("counts = parent %d tools %d, want 1/2", counts.ParentModelCallCount, counts.ToolCallCount)
	}
}

func TestTallyCodexCountsTokenCountMessages(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"output_tokens":3,"reasoning_output_tokens":1},"total_token_usage":{"output_tokens":11,"reasoning_output_tokens":5}}}}`,
		`{"timestamp":"2026-08-26T01:00:02Z","type":"response_item","payload":{"type":"function_call"}}`,
		`{"timestamp":"2026-08-26T01:00:03Z","type":"response_item","payload":{"type":"function_call_output"}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.ParentModelCallCount != 2 || counts.ToolCallCount != 1 {
		t.Fatalf("counts = parent %d tools %d, want 2/1", counts.ParentModelCallCount, counts.ToolCallCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 11 {
		t.Fatalf("output tokens = %v, want 11", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 5 {
		t.Fatalf("reasoning tokens = %v, want 5", counts.ReasoningTokenCount)
	}
}

func TestTallyCodexUsesFinalCumulativeTokenTotals(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":10,"reasoning_output_tokens":4}}}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":17,"reasoning_output_tokens":9}}}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want final cumulative 17", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 9 {
		t.Fatalf("reasoning tokens = %v, want final cumulative 9", counts.ReasoningTokenCount)
	}
}

func TestTallyCodexSinceSubtractsPreRunCumulativeTokenTotals(t *testing.T) {
	since := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	before := since.Add(-time.Minute).Format(time.RFC3339Nano)
	after := since.Add(time.Minute).Format(time.RFC3339Nano)
	counts, err := TallyCodexSince(strings.NewReader(strings.Join([]string{
		`{"timestamp":` + strconv.Quote(before) + `,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":100,"reasoning_output_tokens":40}}}}`,
		`{"timestamp":` + strconv.Quote(after) + `,"type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":` + strconv.Quote(after) + `,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":117,"reasoning_output_tokens":49}}}}`,
	}, "\n")), since)
	if err != nil {
		t.Fatalf("TallyCodexSince: %v", err)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want resumed-run delta 17", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 9 {
		t.Fatalf("reasoning tokens = %v, want resumed-run delta 9", counts.ReasoningTokenCount)
	}
}

func TestTallyCodexPathsAggregatesRolloutSegments(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "rollout-first.jsonl")
	second := filepath.Join(dir, "rollout-second.jsonl")
	if err := os.WriteFile(first, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":7,"reasoning_output_tokens":3}}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := os.WriteFile(second, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:10:00Z","type":"response_item","payload":{"type":"function_call"}}`,
		`{"timestamp":"2026-08-26T01:10:01Z","type":"response_item","payload":{"type":"function_call_output"}}`,
		`{"timestamp":"2026-08-26T01:10:02Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":"2026-08-26T01:10:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":11,"reasoning_output_tokens":5}}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write second: %v", err)
	}

	counts, err := TallyCodexPaths([]string{first, second})
	if err != nil {
		t.Fatalf("TallyCodexPaths: %v", err)
	}
	if counts.ParentModelCallCount != 3 || counts.ToolCallCount != 1 {
		t.Fatalf("counts = parent %d tools %d, want 3/1", counts.ParentModelCallCount, counts.ToolCallCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 18 {
		t.Fatalf("output tokens = %v, want 18", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 8 {
		t.Fatalf("reasoning tokens = %v, want 8", counts.ReasoningTokenCount)
	}
}

func TestTallyCodexPathsSinceExcludesPriorRollouts(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "rollout-old.jsonl")
	currentPath := filepath.Join(dir, "rollout-current.jsonl")
	since := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	oldAt := since.Add(-time.Minute).Format(time.RFC3339Nano)
	currentAt := since.Add(time.Minute).Format(time.RFC3339Nano)
	if err := os.WriteFile(oldPath, []byte(strings.Join([]string{
		`{"timestamp":` + strconv.Quote(oldAt) + `,"type":"item.completed","item":{"type":"assistant_message","text":"old"}}`,
		`{"timestamp":` + strconv.Quote(oldAt) + `,"type":"turn.completed","usage":{"output_tokens":99,"reasoning_output_tokens":44}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(currentPath, []byte(strings.Join([]string{
		`{"timestamp":` + strconv.Quote(currentAt) + `,"type":"item.completed","item":{"type":"assistant_message","text":"current"}}`,
		`{"timestamp":` + strconv.Quote(currentAt) + `,"type":"turn.completed","usage":{"output_tokens":11,"reasoning_output_tokens":5}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}

	counts, err := TallyCodexPathsSince([]string{oldPath, currentPath}, since)
	if err != nil {
		t.Fatalf("TallyCodexPathsSince: %v", err)
	}
	if counts.ParentModelCallCount != 1 {
		t.Fatalf("parent calls = %d, want current rollout only", counts.ParentModelCallCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 11 {
		t.Fatalf("output tokens = %v, want 11", counts.OutputTokenCount)
	}
}

func TestTallyCodexPathsSinceCarriesPreRunCumulativeBaselineAcrossRollouts(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "rollout-old.jsonl")
	currentPath := filepath.Join(dir, "rollout-current.jsonl")
	since := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	oldAt := since.Add(-time.Minute).Format(time.RFC3339Nano)
	currentAt := since.Add(time.Minute).Format(time.RFC3339Nano)
	if err := os.WriteFile(oldPath, []byte(strings.Join([]string{
		`{"timestamp":` + strconv.Quote(oldAt) + `,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":100,"reasoning_output_tokens":40}}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(currentPath, []byte(strings.Join([]string{
		`{"timestamp":` + strconv.Quote(currentAt) + `,"type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":` + strconv.Quote(currentAt) + `,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":117,"reasoning_output_tokens":49}}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}

	counts, err := TallyCodexPathsSince([]string{oldPath, currentPath}, since)
	if err != nil {
		t.Fatalf("TallyCodexPathsSince: %v", err)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want resumed-run delta 17", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 9 {
		t.Fatalf("reasoning tokens = %v, want resumed-run delta 9", counts.ReasoningTokenCount)
	}
}

func TestTallyCodexPathsWindowExcludesReplacementRunTokens(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "rollout-old.jsonl")
	currentPath := filepath.Join(dir, "rollout-current.jsonl")
	since := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	until := since.Add(10 * time.Minute)
	oldAt := since.Add(-time.Minute).Format(time.RFC3339Nano)
	staleAt := since.Add(5 * time.Minute).Format(time.RFC3339Nano)
	replacementAt := until.Add(time.Second).Format(time.RFC3339Nano)
	if err := os.WriteFile(oldPath, []byte(strings.Join([]string{
		`{"timestamp":` + strconv.Quote(oldAt) + `,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":100,"reasoning_output_tokens":40}}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(currentPath, []byte(strings.Join([]string{
		`{"timestamp":` + strconv.Quote(staleAt) + `,"type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":` + strconv.Quote(staleAt) + `,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":112,"reasoning_output_tokens":45}}}}`,
		`{"timestamp":` + strconv.Quote(replacementAt) + `,"type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":` + strconv.Quote(replacementAt) + `,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":130,"reasoning_output_tokens":55}}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}

	counts, err := TallyCodexPathsWindowContext(context.Background(), []string{oldPath, currentPath}, since, until)
	if err != nil {
		t.Fatalf("TallyCodexPathsWindowContext: %v", err)
	}
	if counts.ParentModelCallCount != 1 {
		t.Fatalf("parent calls = %d, want stale window only", counts.ParentModelCallCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 12 {
		t.Fatalf("output tokens = %v, want stale-window delta 12", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 5 {
		t.Fatalf("reasoning tokens = %v, want stale-window delta 5", counts.ReasoningTokenCount)
	}
}

func TestTallyCodexPathsSinceContextHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := TallyCodexPathsSinceContext(ctx, []string{filepath.Join(t.TempDir(), "rollout.jsonl")}, time.Time{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TallyCodexPathsSinceContext err = %v, want context.Canceled", err)
	}
}

func TestTallyClaudePathWithChildrenSinceContextHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := TallyClaudePathWithChildrenSinceContext(ctx, filepath.Join(t.TempDir(), "parent.jsonl"), t.TempDir(), "parent", time.Time{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TallyClaudePathWithChildrenSinceContext err = %v, want context.Canceled", err)
	}
}

func TestCodexTranscriptPathsReturnsAllMatchesByModTime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "08", "26")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	first := filepath.Join(dir, "rollout-2026-08-26T01-00-00-provider-id.jsonl")
	second := filepath.Join(dir, "rollout-2026-08-26T01-10-00-provider-id.jsonl")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write rollout: %v", err)
		}
	}
	firstTime := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	secondTime := firstTime.Add(time.Minute)
	if err := os.Chtimes(first, firstTime, firstTime); err != nil {
		t.Fatalf("chtimes first: %v", err)
	}
	if err := os.Chtimes(second, secondTime, secondTime); err != nil {
		t.Fatalf("chtimes second: %v", err)
	}

	paths, err := CodexTranscriptPaths("provider-id")
	if err != nil {
		t.Fatalf("CodexTranscriptPaths: %v", err)
	}
	if len(paths) != 2 || paths[0] != first || paths[1] != second {
		t.Fatalf("paths = %#v, want [%q %q]", paths, first, second)
	}
}

func TestTallyCodexCountsToolOnlyTokenCountInvocations(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"event_msg","payload":{"type":"token_count","info":null}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"response_item","payload":{"type":"function_call"}}`,
		`{"timestamp":"2026-08-26T01:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":10,"reasoning_output_tokens":4}}}}`,
		`{"timestamp":"2026-08-26T01:00:03Z","type":"response_item","payload":{"type":"function_call_output"}}`,
		`{"timestamp":"2026-08-26T01:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":"2026-08-26T01:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":17,"reasoning_output_tokens":5}}}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.ParentModelCallCount != 2 || counts.ToolCallCount != 1 {
		t.Fatalf("counts = parent %d tools %d, want 2/1", counts.ParentModelCallCount, counts.ToolCallCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want final cumulative 17", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 5 {
		t.Fatalf("reasoning tokens = %v, want final cumulative 5", counts.ReasoningTokenCount)
	}
}

func TestTallyCodexCountsExecJSONLEvents(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"type":"thread.started","thread_id":"abc"}`,
		`{"type":"item.completed","item":{"type":"function_call","name":"exec_command"}}`,
		`{"type":"item.completed","item":{"type":"assistant_message","text":"done"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":12,"reasoning_output_tokens":5,"total_tokens":117}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.ParentModelCallCount != 2 || counts.ToolCallCount != 1 {
		t.Fatalf("counts = parent %d tools %d, want 2/1", counts.ParentModelCallCount, counts.ToolCallCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 12 {
		t.Fatalf("output tokens = %v, want 12", counts.OutputTokenCount)
	}
	if counts.ReasoningTokenCount == nil || *counts.ReasoningTokenCount != 5 {
		t.Fatalf("reasoning tokens = %v, want 5", counts.ReasoningTokenCount)
	}
}

func TestTallyCodexCountsCurrentCustomToolCalls(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"custom_tool_call"}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		`{"timestamp":"2026-08-26T01:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":13,"reasoning_output_tokens":4}}}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.ParentModelCallCount != 2 || counts.ToolCallCount != 1 {
		t.Fatalf("counts = parent %d tools %d, want 2/1", counts.ParentModelCallCount, counts.ToolCallCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 13 {
		t.Fatalf("output tokens = %v, want 13", counts.OutputTokenCount)
	}
}

func TestTallyCodexGroupsParallelToolCallsByInvocation(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"function_call"}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"response_item","payload":{"type":"custom_tool_call"}}`,
		`{"timestamp":"2026-08-26T01:00:02Z","type":"response_item","payload":{"type":"function_call_output"}}`,
		`{"timestamp":"2026-08-26T01:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.ParentModelCallCount != 2 || counts.ToolCallCount != 2 {
		t.Fatalf("counts = parent %d tools %d, want 2/2", counts.ParentModelCallCount, counts.ToolCallCount)
	}
}

func TestTallyCodexCustomToolCallOutputsResetInvocation(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"custom_tool_call"}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output"}}`,
		`{"timestamp":"2026-08-26T01:00:02Z","type":"response_item","payload":{"type":"custom_tool_call"}}`,
		`{"timestamp":"2026-08-26T01:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output"}}`,
		`{"timestamp":"2026-08-26T01:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.ParentModelCallCount != 3 || counts.ToolCallCount != 2 {
		t.Fatalf("counts = parent %d tools %d, want 3/2", counts.ParentModelCallCount, counts.ToolCallCount)
	}
}

func TestTallyCodexCountsSubagentCustomToolCalls(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"spawn_agent"}}`,
		`{"timestamp":"2026-08-26T01:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output"}}`,
		`{"timestamp":"2026-08-26T01:00:02Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec_command"}}`,
		`{"timestamp":"2026-08-26T01:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output"}}`,
		`{"timestamp":"2026-08-26T01:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.SubagentCount != 1 || counts.DirectSubagentCount != 1 {
		t.Fatalf("subagents = %d direct %d, want 1/1", counts.SubagentCount, counts.DirectSubagentCount)
	}
	if counts.ToolCallCount != 2 {
		t.Fatalf("tools = %d, want 2", counts.ToolCallCount)
	}
}

func TestTallyCodexExecCustomToolCallOutputsResetInvocation(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"type":"item.completed","item":{"type":"custom_tool_call","name":"exec_command"}}`,
		`{"type":"item.completed","item":{"type":"custom_tool_call_output"}}`,
		`{"type":"item.completed","item":{"type":"custom_tool_call","name":"exec_command"}}`,
		`{"type":"item.completed","item":{"type":"custom_tool_call_output"}}`,
		`{"type":"item.completed","item":{"type":"assistant_message","text":"done"}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.ParentModelCallCount != 3 || counts.ToolCallCount != 2 {
		t.Fatalf("counts = parent %d tools %d, want 3/2", counts.ParentModelCallCount, counts.ToolCallCount)
	}
}

func TestTallyCodexExecCountsSubagentCustomToolCalls(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(strings.Join([]string{
		`{"type":"item.completed","item":{"type":"custom_tool_call","name":"spawn_agent"}}`,
		`{"type":"item.completed","item":{"type":"custom_tool_call_output"}}`,
		`{"type":"item.completed","item":{"type":"custom_tool_call","name":"exec_command"}}`,
		`{"type":"item.completed","item":{"type":"custom_tool_call_output"}}`,
		`{"type":"item.completed","item":{"type":"assistant_message","text":"done"}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.SubagentCount != 1 || counts.DirectSubagentCount != 1 {
		t.Fatalf("subagents = %d direct %d, want 1/1", counts.SubagentCount, counts.DirectSubagentCount)
	}
	if counts.ToolCallCount != 2 {
		t.Fatalf("tools = %d, want 2", counts.ToolCallCount)
	}
}

func TestTallyClaudePathCountsChildSidecars(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.jsonl")
	if err := os.WriteFile(parent, []byte(`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":10,"output_tokens_details":{"thinking_tokens":4}}}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}
	childDir := filepath.Join(dir, "parent", "subagents")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatalf("mkdir child dir: %v", err)
	}
	child := filepath.Join(childDir, "agent-child.jsonl")
	if err := os.WriteFile(child, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:01:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":7}}}`,
		`{"timestamp":"2026-08-26T01:02:00Z","type":"tool_use"}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "agent-child.meta.json"), []byte(`{"parentAgentId":"parent","spawnDepth":1}`), 0o600); err != nil {
		t.Fatalf("write child meta: %v", err)
	}

	counts, err := TallyClaudePath(parent)
	if err != nil {
		t.Fatalf("TallyClaudePath: %v", err)
	}
	if counts.ParentModelCallCount != 1 || counts.ChildModelCallCount != 1 {
		t.Fatalf("model calls = parent %d child %d, want 1/1", counts.ParentModelCallCount, counts.ChildModelCallCount)
	}
	if counts.SubagentCount != 1 || counts.DirectSubagentCount != 1 {
		t.Fatalf("subagents = %d direct %d, want 1/1", counts.SubagentCount, counts.DirectSubagentCount)
	}
	if len(counts.Children) != 1 || counts.Children[0].SpawnDepth != 1 {
		t.Fatalf("children = %#v, want one direct child", counts.Children)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want 17", counts.OutputTokenCount)
	}
}

func TestTallyClaudePathWithChildrenSinceFiltersPreviousRun(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.jsonl")
	childDir := filepath.Join(dir, "parent", "subagents")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(parent, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-25T01:00:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":99}}}`,
		`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":10}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}
	child := filepath.Join(childDir, "child.jsonl")
	if err := os.WriteFile(child, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:01:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":7}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "child.meta.json"), []byte(`{"parentAgentId":"parent","spawnDepth":1}`), 0o600); err != nil {
		t.Fatalf("write child meta: %v", err)
	}
	oldChild := filepath.Join(childDir, "old-child.jsonl")
	if err := os.WriteFile(oldChild, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-25T01:01:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":70}}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write old child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "old-child.meta.json"), []byte(`{"parentAgentId":"parent","spawnDepth":1}`), 0o600); err != nil {
		t.Fatalf("write old child meta: %v", err)
	}
	since := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	counts, err := TallyClaudePathWithChildrenSince(parent, dir, "parent", since)
	if err != nil {
		t.Fatalf("TallyClaudePathWithChildrenSince: %v", err)
	}
	if counts.ParentModelCallCount != 1 || counts.ChildModelCallCount != 1 || len(counts.Children) != 1 {
		t.Fatalf("counts = parent %d child %d spans %d, want 1/1/1", counts.ParentModelCallCount, counts.ChildModelCallCount, len(counts.Children))
	}
	if counts.SubagentCount != 1 || counts.DirectSubagentCount != 1 {
		t.Fatalf("subagents = %d direct %d, want retained child only", counts.SubagentCount, counts.DirectSubagentCount)
	}
	if counts.OutputTokenCount == nil || *counts.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want current parent plus current child", counts.OutputTokenCount)
	}
}

func TestTallyClaudePathDoesNotCountUnknownDepthAsDirect(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.jsonl")
	if err := os.WriteFile(parent, []byte(`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}
	childDir := filepath.Join(dir, "parent", "subagents")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatalf("mkdir child dir: %v", err)
	}
	child := filepath.Join(childDir, "agent-child.jsonl")
	if err := os.WriteFile(child, []byte(`{"timestamp":"2026-08-26T01:01:00Z","type":"assistant","message":{"role":"assistant"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}

	counts, err := TallyClaudePath(parent)
	if err != nil {
		t.Fatalf("TallyClaudePath: %v", err)
	}
	if counts.SubagentCount != 1 || counts.DirectSubagentCount != 0 {
		t.Fatalf("subagents = %d direct %d, want 1/0 for missing spawnDepth", counts.SubagentCount, counts.DirectSubagentCount)
	}
}

func TestTallyCodexUnwrapsHeadlessRunnerTextLines(t *testing.T) {
	counts, err := TallyCodex(strings.NewReader(`{"ts":"2026-08-26T01:00:00Z","text":"{\"timestamp\":\"2026-08-26T01:00:00Z\",\"type\":\"assistant_message\"}"}`))
	if err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if counts.ParentModelCallCount != 1 {
		t.Fatalf("parent model calls = %d, want 1", counts.ParentModelCallCount)
	}
}

// TestTallyClaudePathWithChildrenSumsReviewerDispatches pins the fan-out the
// review-stack reference claims is verifiable from `boss cost`. On the primary
// path the parent dispatches exactly one marked subagent, which then dispatches
// boss-review and every lens and round -- so counting only the parent transcript
// records 1 regardless of how wide the review actually fanned out, and the bound
// it exists to verify becomes uncheckable.
//
// The child's terminal state deliberately does not roll up the same way: a
// reviewer subagent reporting BLOCKED is not the parent run's terminal state.
func TestTallyClaudePathWithChildrenSumsReviewerDispatches(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.jsonl")
	childDir := filepath.Join(dir, "parent", "subagents")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// The parent dispatches one marked reviewer subagent.
	if err := os.WriteFile(parent, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","input":{"prompt":"[bs-reviewer-dispatch]\nreview the branch"}}]}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}

	// That subagent fans out to three more marked reviewers.
	child := filepath.Join(childDir, "child.jsonl")
	if err := os.WriteFile(child, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:01:00Z","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","input":{"prompt":"[bs-reviewer-dispatch]\ngo lens"}}]}}`,
		`{"timestamp":"2026-08-26T01:02:00Z","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","input":{"prompt":"[bs-reviewer-dispatch]\ndb lens"}}]}}`,
		`{"timestamp":"2026-08-26T01:03:00Z","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"BLOCKED because the lens failed"}]}}`,
		`{"timestamp":"2026-08-26T01:04:00Z","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","input":{"prompt":"[bs-reviewer-dispatch]\napi lens"}}]}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "child.meta.json"), []byte(`{"parentAgentId":"parent","spawnDepth":1}`), 0o600); err != nil {
		t.Fatalf("write child meta: %v", err)
	}

	counts, err := TallyClaudePathWithChildren(parent, dir, "parent")
	if err != nil {
		t.Fatalf("TallyClaudePathWithChildren: %v", err)
	}
	if counts.ReviewerDispatchCount != 4 {
		t.Errorf("ReviewerDispatchCount = %d, want 4 (1 parent + 3 fanned out); counting the parent transcript alone records 1 whatever the fan-out", counts.ReviewerDispatchCount)
	}
	if counts.TerminalState != "" {
		t.Errorf("TerminalState = %q, want empty: a reviewer subagent's own BLOCKED must not become the parent's terminal state", counts.TerminalState)
	}
}
