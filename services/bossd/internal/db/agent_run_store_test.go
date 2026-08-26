package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/agenttelemetry"
	"github.com/recurser/bossalib/sqlutil"
)

func TestAgentRunStoreRecordsLifecycleAndTelemetry(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)

	run, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-1",
		Model:          "gpt-5",
		Effort:         "high",
		StartedAt:      started,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	outputTokens := int64(123)
	reasoningTokens := int64(45)
	childStop := started.Add(3 * time.Minute)
	if err := store.RecordTelemetry(ctx, run.ID, AgentRunTelemetry{
		ParentModelCallCount: 2,
		ChildModelCallCount:  1,
		ToolCallCount:        7,
		SubagentCount:        2,
		DirectSubagentCount:  1,
		OutputTokenCount:     &outputTokens,
		ReasoningTokenCount:  &reasoningTokens,
		Children: []AgentRunChild{{
			AgentSessionID:      "child-1",
			ParentAgentID:       "agent-1",
			SpawnDepth:          1,
			StartedAt:           started.Add(time.Minute),
			StoppedAt:           &childStop,
			ModelCallCount:      1,
			ToolCallCount:       3,
			OutputTokenCount:    &outputTokens,
			ReasoningTokenCount: &reasoningTokens,
		}},
	}); err != nil {
		t.Fatalf("RecordTelemetry: %v", err)
	}

	stopped := started.Add(10 * time.Minute)
	if err := store.Stop(ctx, "agent-1", AgentRunStopClean, stopped); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("List returned %d runs, want 1", len(runs))
	}
	got := runs[0]
	if got.StopReason != AgentRunStopClean || got.StoppedAt == nil || !got.StoppedAt.Equal(stopped) {
		t.Fatalf("stopped run = reason %q stopped %v, want clean at %s", got.StopReason, got.StoppedAt, stopped)
	}
	if got.ParentModelCallCount != 2 || got.ChildModelCallCount != 1 || got.ToolCallCount != 7 {
		t.Fatalf("counts = parent %d child %d tools %d, want 2/1/7", got.ParentModelCallCount, got.ChildModelCallCount, got.ToolCallCount)
	}
	if got.OutputTokenCount == nil || *got.OutputTokenCount != 123 || got.ReasoningTokenCount == nil || *got.ReasoningTokenCount != 45 {
		t.Fatalf("tokens = output %v reasoning %v, want 123/45", got.OutputTokenCount, got.ReasoningTokenCount)
	}
	if len(got.Children) != 1 || got.Children[0].AgentSessionID != "child-1" {
		t.Fatalf("children = %#v, want child-1", got.Children)
	}
}

func TestAgentRunStoreListFiltersByRunAgentName(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	stopped := started.Add(time.Minute)

	run, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-1",
		AgentName:      "codex",
		StartedAt:      started,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.Stop(ctx, run.AgentSessionID, AgentRunStopClean, stopped); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	codexRuns, err := store.List(ctx, AgentRunFilter{AgentName: "codex"})
	if err != nil {
		t.Fatalf("List codex: %v", err)
	}
	if len(codexRuns) != 1 || codexRuns[0].ID != run.ID {
		t.Fatalf("codex filtered runs = %#v, want run %q", codexRuns, run.ID)
	}
	sessionAgentRuns, err := store.List(ctx, AgentRunFilter{AgentName: sess.AgentName})
	if err != nil {
		t.Fatalf("List session agent: %v", err)
	}
	if len(sessionAgentRuns) != 0 {
		t.Fatalf("session-agent filtered runs = %#v, want none while run agent is codex", sessionAgentRuns)
	}
}

func TestAgentRunStoreFiltersOpenBackfilledAndUnknownByDefault(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)

	if _, err := store.Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: "open", StartedAt: started, StopReason: AgentRunStopUnknown}); err != nil {
		t.Fatalf("start open: %v", err)
	}
	if _, err := store.Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: "unknown", StartedAt: started.Add(time.Minute), StopReason: AgentRunStopUnknown}); err != nil {
		t.Fatalf("start unknown: %v", err)
	}
	if err := store.Stop(ctx, "unknown", AgentRunStopUnknown, started.Add(2*time.Minute)); err != nil {
		t.Fatalf("stop unknown: %v", err)
	}
	if _, err := store.Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: "backfilled", StartedAt: started.Add(3 * time.Minute), StopReason: AgentRunStopClean, IsBackfilled: true}); err != nil {
		t.Fatalf("start backfilled: %v", err)
	}
	if err := store.Stop(ctx, "backfilled", AgentRunStopClean, started.Add(4*time.Minute)); err != nil {
		t.Fatalf("stop backfilled: %v", err)
	}

	runs, err := store.List(ctx, AgentRunFilter{})
	if err != nil {
		t.Fatalf("List default: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("default List returned %d runs, want 0", len(runs))
	}
	runs, err = store.List(ctx, AgentRunFilter{IncludeOpen: true})
	if err != nil {
		t.Fatalf("List include open: %v", err)
	}
	if len(runs) != 1 || runs[0].AgentSessionID != "open" {
		t.Fatalf("include-open List returned %#v, want only the open unknown row", runs)
	}
	runs, err = store.List(ctx, AgentRunFilter{IncludeOpen: true, IncludeAllReasons: true, IncludeBackfilled: true})
	if err != nil {
		t.Fatalf("List include all: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("include-all List returned %d runs, want 3", len(runs))
	}
}

func TestAgentRunStoreRecordTelemetryRejectsNegativeCount(t *testing.T) {
	database := setupTestDB(t)
	store := NewAgentRunStore(database)

	err := store.RecordTelemetry(context.Background(), "missing", AgentRunTelemetry{ToolCallCount: -1})
	if err == nil {
		t.Fatal("RecordTelemetry accepted a negative count")
	}
}

func TestAgentRunStoreRecordTelemetryRejectsInvalidChildCounts(t *testing.T) {
	database := setupTestDB(t)
	store := NewAgentRunStore(database)
	stopped := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	started := stopped.Add(time.Minute)

	err := store.RecordTelemetry(context.Background(), "missing", AgentRunTelemetry{
		Children: []AgentRunChild{{
			SpawnDepth:     -1,
			StartedAt:      started,
			StoppedAt:      &stopped,
			ModelCallCount: -1,
		}},
	})
	if err == nil {
		t.Fatal("RecordTelemetry accepted invalid child telemetry")
	}
}

func TestAgentRunStoreAllowsMultipleCompletedRunsForSameAgentSession(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)

	if _, err := store.Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: "agent-1", StartedAt: started}); err != nil {
		t.Fatalf("start first: %v", err)
	}
	if err := store.Stop(ctx, "agent-1", AgentRunStopClean, started.Add(time.Minute)); err != nil {
		t.Fatalf("stop first: %v", err)
	}
	if _, err := store.Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: "agent-1", StartedAt: started.Add(2 * time.Minute), StopReason: AgentRunStopClean}); err != nil {
		t.Fatalf("start resumed: %v", err)
	}
	if err := store.RecordTelemetryByAgentSessionID(ctx, "agent-1", AgentRunTelemetry{ParentModelCallCount: 9}); err != nil {
		t.Fatalf("record resumed telemetry: %v", err)
	}
	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID, IncludeOpen: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	if runs[0].ParentModelCallCount != 9 || runs[1].ParentModelCallCount != 0 {
		t.Fatalf("parent counts = latest %d previous %d, want 9/0", runs[0].ParentModelCallCount, runs[1].ParentModelCallCount)
	}
}

func TestAgentRunStoreReconcileOpenKeepsTmuxBackedRunsOpen(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	chatStore := NewAgentChatStore(database)
	store := NewAgentRunStore(database)
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)

	if _, err := store.Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: "live-agent", StartedAt: started}); err != nil {
		t.Fatalf("start live: %v", err)
	}
	if _, err := store.Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: "dead-agent", StartedAt: started}); err != nil {
		t.Fatalf("start dead: %v", err)
	}
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{SessionID: sess.ID, AgentSessionID: "live-agent", Title: "Live"}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	tmuxName := "boss-live-agent"
	if err := chatStore.UpdateTmuxSessionName(ctx, "live-agent", &tmuxName); err != nil {
		t.Fatalf("set tmux session: %v", err)
	}

	n, err := store.ReconcileOpen(ctx, started.Add(time.Hour), []string{"live-agent"})
	if err != nil {
		t.Fatalf("ReconcileOpen: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled = %d, want only the non-tmux run", n)
	}
	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID, IncludeOpen: true, IncludeAllReasons: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byAgent := map[string]AgentRun{}
	for _, run := range runs {
		byAgent[run.AgentSessionID] = run
	}
	if byAgent["live-agent"].StoppedAt != nil {
		t.Fatalf("live-agent stopped_at = %v, want open", byAgent["live-agent"].StoppedAt)
	}
	if byAgent["dead-agent"].StopReason != AgentRunStopDaemonRestart || byAgent["dead-agent"].StoppedAt == nil {
		t.Fatalf("dead-agent = %#v, want daemon_restart stopped", byAgent["dead-agent"])
	}
}

func TestAgentRunStoreRecordsTelemetryByAgentSessionID(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)

	if _, err := store.Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: "agent-key", StartedAt: started}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.Stop(ctx, "agent-key", AgentRunStopClean, started.Add(time.Minute)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := store.RecordTelemetryByAgentSessionID(ctx, "agent-key", AgentRunTelemetry{ParentModelCallCount: 3}); err != nil {
		t.Fatalf("RecordTelemetryByAgentSessionID: %v", err)
	}

	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 || runs[0].ParentModelCallCount != 3 {
		t.Fatalf("runs = %#v, want one run with parent_model_call_count=3", runs)
	}
}

func TestAgentRunStoreStopMissingRunReturnsNoRows(t *testing.T) {
	database := setupTestDB(t)
	store := NewAgentRunStore(database)

	err := store.Stop(context.Background(), "missing", AgentRunStopClean, time.Now())
	if err != sql.ErrNoRows {
		t.Fatalf("Stop missing err = %v, want sql.ErrNoRows", err)
	}
}

func TestAgentRunStoreStopRunScopesToRunID(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	started := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	first, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-1",
		StartedAt:      started,
	})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	second, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-2",
		StartedAt:      started.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Start second: %v", err)
	}

	stopped := started.Add(2 * time.Minute)
	if err := store.StopRun(ctx, first.ID, AgentRunStopStopped, stopped); err != nil {
		t.Fatalf("StopRun: %v", err)
	}
	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID, IncludeOpen: true, IncludeAllReasons: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]AgentRun{}
	for _, run := range runs {
		byID[run.ID] = run
	}
	if got := byID[first.ID]; got.StoppedAt == nil || !got.StoppedAt.Equal(stopped) || got.StopReason != AgentRunStopStopped {
		t.Fatalf("first stopped run = %#v, want stopped at %s", got, stopped)
	}
	if got := byID[second.ID]; got.StoppedAt != nil {
		t.Fatalf("second run stopped_at = %v, want still open", got.StoppedAt)
	}
}

func TestAgentRunStoreStopRunMissingOrClosedReturnsNoRows(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	run, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-1",
		StartedAt:      time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.StopRun(ctx, run.ID, AgentRunStopStopped, time.Now()); err != nil {
		t.Fatalf("StopRun first: %v", err)
	}
	if err := store.StopRun(ctx, run.ID, AgentRunStopStopped, time.Now()); err != sql.ErrNoRows {
		t.Fatalf("StopRun closed err = %v, want sql.ErrNoRows", err)
	}
	if err := store.StopRun(ctx, "missing", AgentRunStopStopped, time.Now()); err != sql.ErrNoRows {
		t.Fatalf("StopRun missing err = %v, want sql.ErrNoRows", err)
	}
}

func TestAgentRunStoreBackfillImportsMatchingClaudeTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	setSessionCreatedAtForBackfillTest(t, database, sess.ID, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))
	setSessionAgentNameForBackfillTest(t, database, sess.ID, "claude")
	setSessionModelForBackfillTest(t, database, sess.ID, "raw-model", "configured-model")
	store := NewAgentRunStore(database)

	projectDir := filepath.Join(home, ".claude", "projects", projectKey(sess.WorktreePath))
	if err := os.MkdirAll(filepath.Join(projectDir, "agent-parent", "subagents"), 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	parent := filepath.Join(projectDir, "agent-parent.jsonl")
	if err := os.WriteFile(parent, []byte(`{"timestamp":"2026-08-21T01:00:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":10,"output_tokens_details":{"thinking_tokens":3}}}}`+"\n"+
		`{"timestamp":"2026-08-21T01:03:00Z","type":"tool_use"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}
	child := filepath.Join(projectDir, "agent-parent", "subagents", "agent-child.jsonl")
	if err := os.WriteFile(child, []byte(`{"timestamp":"2026-08-21T01:01:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":7}}}`+"\n"+
		`{"timestamp":"2026-08-21T01:02:00Z","type":"tool_use"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "agent-parent", "subagents", "agent-child.meta.json"), []byte(`{"parentAgentId":"agent-parent","spawnDepth":1}`), 0o600); err != nil {
		t.Fatalf("write child meta: %v", err)
	}

	since := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	summary, err := store.Backfill(ctx, AgentRunBackfillParams{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if summary.InsertedCount != 1 || summary.SkippedCount != 0 {
		t.Fatalf("summary = %+v, want 1 inserted / 0 skipped", summary)
	}
	repeat, err := store.Backfill(ctx, AgentRunBackfillParams{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("repeat Backfill: %v", err)
	}
	if repeat.InsertedCount != 0 || repeat.SkippedCount != 1 {
		t.Fatalf("repeat summary = %+v, want 0 inserted / 1 skipped", repeat)
	}
	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID, IncludeBackfilled: true, IncludeAllReasons: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	got := runs[0]
	if !got.IsBackfilled || got.ParentModelCallCount != 1 || got.ChildModelCallCount != 1 || got.DirectSubagentCount != 1 {
		t.Fatalf("backfilled run = %#v, want parent/child/direct counts", got)
	}
	if got.OutputTokenCount == nil || *got.OutputTokenCount != 17 || got.ReasoningTokenCount == nil || *got.ReasoningTokenCount != 3 {
		t.Fatalf("tokens = output %v reasoning %v, want 17/3", got.OutputTokenCount, got.ReasoningTokenCount)
	}
	if got.Model != "configured-model" {
		t.Fatalf("backfilled model = %q, want effective model", got.Model)
	}
	if len(got.Children) != 1 {
		t.Fatalf("children = %d, want 1", len(got.Children))
	}
}

func TestAgentRunStoreBackfillSkipsLiveRunWindowMatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	setSessionCreatedAtForBackfillTest(t, database, sess.ID, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))
	setSessionAgentNameForBackfillTest(t, database, sess.ID, "claude")
	store := NewAgentRunStore(database)

	projectDir := filepath.Join(home, ".claude", "projects", projectKey(sess.WorktreePath))
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	parent := filepath.Join(projectDir, "agent-parent.jsonl")
	if err := os.WriteFile(parent, []byte(`{"timestamp":"2026-08-21T01:00:00Z","type":"assistant","message":{"role":"assistant"}}`+"\n"+
		`{"timestamp":"2026-08-21T01:05:00Z","type":"assistant","message":{"role":"assistant"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}
	if _, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-parent",
		AgentName:      "claude",
		StartedAt:      time.Date(2026, 8, 21, 1, 1, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("start live: %v", err)
	}

	since := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	summary, err := store.Backfill(ctx, AgentRunBackfillParams{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if summary.InsertedCount != 0 || summary.SkippedCount != 1 {
		t.Fatalf("summary = %+v, want 0 inserted / 1 skipped", summary)
	}
	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID, IncludeOpen: true, IncludeAllReasons: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 || runs[0].AgentSessionID != "agent-parent" || runs[0].StoppedAt != nil {
		t.Fatalf("runs = %#v, want only the live row", runs)
	}
}

func TestAgentRunStoreBackfillSkipsLiveCodexProviderSessionMatch(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	t.Setenv("HOME", filepath.Join(home, "regular-home"))
	t.Setenv("CODEX_HOME", codexHome)
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess, err := NewSessionStore(database).Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Codex provider match",
		WorktreePath: filepath.Join(home, "work"),
		BranchName:   "feat/codex-provider-match",
		BaseBranch:   "main",
		AgentName:    "codex",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	setSessionCreatedAtForBackfillTest(t, database, sess.ID, time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC))
	store := NewAgentRunStore(database)
	chatStore := NewAgentChatStore(database)

	providerSessionID := "codex-rollout-provider"
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:         sess.ID,
		AgentSessionID:    "boss-correlation",
		ProviderSessionID: &providerSessionID,
		Title:             "Codex chat",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if _, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "boss-correlation",
		AgentName:      "codex",
		StartedAt:      time.Date(2026, 8, 26, 1, 1, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("start live: %v", err)
	}

	dir := filepath.Join(codexHome, "sessions", "2026", "08", "26")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir codex sessions: %v", err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","payload":{"id":` + strconv.Quote(providerSessionID) + `,"timestamp":"2026-08-26T01:00:00Z","cwd":` + strconv.Quote(sess.WorktreePath) + `}}`,
		`{"timestamp":"2026-08-26T01:05:00Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write codex rollout: %v", err)
	}

	since := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	summary, err := store.Backfill(ctx, AgentRunBackfillParams{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if summary.InsertedCount != 0 || summary.SkippedCount != 1 {
		t.Fatalf("summary = %+v, want 0 inserted / 1 skipped", summary)
	}
	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID, IncludeOpen: true, IncludeAllReasons: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 || runs[0].AgentSessionID != "boss-correlation" || runs[0].StoppedAt != nil {
		t.Fatalf("runs = %#v, want only the live provider-mapped row", runs)
	}
}

func TestAgentRunStoreBackfillRejectsInvalidChildrenAtomically(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	stoppedAt := time.Date(2026, 8, 26, 1, 5, 0, 0, time.UTC)

	_, err := store.startBackfilledRun(ctx, AgentRun{
		SessionID:            sess.ID,
		AgentSessionID:       "agent-atomic",
		AgentName:            "claude",
		StartedAt:            time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC),
		StoppedAt:            &stoppedAt,
		StopReason:           AgentRunStopClean,
		ParentModelCallCount: 1,
		IsBackfilled:         true,
	}, AgentRunTelemetry{
		ParentModelCallCount: 1,
		Children: []AgentRunChild{{
			AgentSessionID: "child-bad",
			StartedAt:      time.Date(2026, 8, 26, 1, 1, 0, 0, time.UTC),
			ModelCallCount: -1,
		}},
	})
	if err == nil {
		t.Fatal("startBackfilledRun succeeded with invalid child telemetry")
	}
	var count int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_runs WHERE agent_session_id = ?`, "agent-atomic").Scan(&count); err != nil {
		t.Fatalf("count agent_runs: %v", err)
	}
	if count != 0 {
		t.Fatalf("agent_runs count = %d, want 0", count)
	}
}

func TestAgentRunStoreRecordTelemetryRejectsChildMissingStartedAtAtomically(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	run, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-missing-start",
		StartedAt:      time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = store.RecordTelemetry(ctx, run.ID, AgentRunTelemetry{
		ParentModelCallCount: 3,
		ChildModelCallCount:  1,
		SubagentCount:        1,
		Children: []AgentRunChild{{
			AgentSessionID: "child-missing-start",
			ModelCallCount: 1,
		}},
	})
	if err == nil {
		t.Fatal("RecordTelemetry succeeded with missing child StartedAt")
	}
	var parentModelCalls, childModelCalls, subagents int64
	if err := database.QueryRowContext(ctx,
		`SELECT parent_model_call_count, child_model_call_count, subagent_count FROM agent_runs WHERE id = ?`,
		run.ID).Scan(&parentModelCalls, &childModelCalls, &subagents); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if parentModelCalls != 0 || childModelCalls != 0 || subagents != 0 {
		t.Fatalf("counters = parent %d child %d subagents %d, want unchanged 0/0/0", parentModelCalls, childModelCalls, subagents)
	}
	var children int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_run_children WHERE agent_run_id = ?`, run.ID).Scan(&children); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if children != 0 {
		t.Fatalf("children = %d, want 0", children)
	}
}

func TestAgentRunStoreBackfillRejectsChildMissingStartedAtAtomically(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)
	stoppedAt := time.Date(2026, 8, 26, 1, 5, 0, 0, time.UTC)

	_, err := store.startBackfilledRun(ctx, AgentRun{
		SessionID:            sess.ID,
		AgentSessionID:       "agent-backfill-missing-start",
		AgentName:            "claude",
		StartedAt:            time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC),
		StoppedAt:            &stoppedAt,
		StopReason:           AgentRunStopClean,
		ParentModelCallCount: 1,
		ChildModelCallCount:  1,
		SubagentCount:        1,
		IsBackfilled:         true,
	}, AgentRunTelemetry{
		ParentModelCallCount: 1,
		ChildModelCallCount:  1,
		SubagentCount:        1,
		Children: []AgentRunChild{{
			AgentSessionID: "child-missing-start",
			ModelCallCount: 1,
		}},
	})
	if err == nil {
		t.Fatal("startBackfilledRun succeeded with missing child StartedAt")
	}
	var count int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_runs WHERE agent_session_id = ?`, "agent-backfill-missing-start").Scan(&count); err != nil {
		t.Fatalf("count agent_runs: %v", err)
	}
	if count != 0 {
		t.Fatalf("agent_runs count = %d, want 0", count)
	}
}

func TestMatchBackfillSessionUsesAgentSessionOrTimeBounds(t *testing.T) {
	older := time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	archived := newer.Add(30 * time.Minute)
	candidate := backfillCandidate{
		AgentSessionID: "candidate-agent",
		AgentName:      "codex",
		WorktreePath:   "/repo",
		StartedAt:      newer.Add(10 * time.Minute),
		StoppedAt:      newer.Add(20 * time.Minute),
	}

	exact, ok := matchBackfillSession(candidate, []backfillSession{
		{ID: "wrong-time", AgentName: "codex", AgentSessionID: "other-agent", WorktreePath: "/repo", CreatedAt: newer},
		{ID: "exact", AgentName: "codex", AgentSessionID: "candidate-agent", WorktreePath: "/elsewhere", CreatedAt: older},
	})
	if !ok || exact.ID != "exact" {
		t.Fatalf("exact match = %#v,%v want exact,true", exact, ok)
	}

	got, ok := matchBackfillSession(candidate, []backfillSession{
		{ID: "old", AgentName: "codex", WorktreePath: "/repo", CreatedAt: older},
		{ID: "too-new", AgentName: "codex", WorktreePath: "/repo", CreatedAt: candidate.StoppedAt.Add(time.Minute)},
		{ID: "archived-before-run", AgentName: "codex", WorktreePath: "/repo", CreatedAt: older, ArchivedAt: &archived},
		{ID: "newest-covering", AgentName: "codex", WorktreePath: "/repo", CreatedAt: newer},
	})
	if !ok || got.ID != "newest-covering" {
		t.Fatalf("time-bounded match = %#v,%v want newest-covering,true", got, ok)
	}
}

func TestMatchBackfillSessionAllowsExactAgentSessionAcrossStaleAgentName(t *testing.T) {
	got, ok := matchBackfillSession(backfillCandidate{
		AgentSessionID: "candidate-agent",
		AgentName:      "codex",
	}, []backfillSession{
		{ID: "exact", AgentName: "claude", AgentSessionID: "candidate-agent"},
	})
	if !ok || got.ID != "exact" {
		t.Fatalf("exact match = %#v,%v want exact,true despite stale agent name", got, ok)
	}
}

func TestMatchBackfillSessionAllowsProviderSessionAcrossStaleAgentName(t *testing.T) {
	got, ok := matchBackfillSession(backfillCandidate{
		AgentSessionID: "provider-codex",
		AgentName:      "codex",
		WorktreePath:   "/repo",
		StartedAt:      time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC),
		StoppedAt:      time.Date(2026, 8, 26, 1, 5, 0, 0, time.UTC),
	}, []backfillSession{
		{
			ID:               "provider-match",
			AgentName:        "claude",
			AgentSessionID:   "logical-chat",
			ProviderSessions: []backfillProviderSession{{ID: "provider-codex", Model: "codex-chat-model", Effort: "high"}},
			WorktreePath:     "/repo",
			Model:            "claude-session-model",
			Effort:           "claude-session-effort",
			CreatedAt:        time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC),
		},
	})
	if !ok || got.ID != "provider-match" {
		t.Fatalf("provider match = %#v,%v want provider-match,true despite stale agent name", got, ok)
	}
	if got.Model != "codex-chat-model" {
		t.Fatalf("provider match model = %q, want codex-chat-model", got.Model)
	}
	if got.Effort != "high" {
		t.Fatalf("provider match effort = %q, want high", got.Effort)
	}
}

func TestAgentRunStoreBackfillUsesProviderChatModel(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	t.Setenv("HOME", filepath.Join(home, "regular-home"))
	t.Setenv("CODEX_HOME", codexHome)
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess, err := NewSessionStore(database).Create(ctx, CreateSessionParams{
		RepoID:         repo.ID,
		Title:          "Switched provider model",
		WorktreePath:   filepath.Join(home, "work"),
		BranchName:     "feat/provider-model",
		BaseBranch:     "main",
		AgentName:      "claude",
		Model:          "claude-raw-model",
		EffectiveModel: "claude-effective-model",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	setSessionCreatedAtForBackfillTest(t, database, sess.ID, time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC))
	providerSessionID := "provider-codex-model"
	otherProviderSessionID := "provider-other-model"
	if _, err := NewAgentChatStore(database).Create(ctx, CreateAgentChatParams{
		SessionID:         sess.ID,
		AgentSessionID:    "boss-correlation-other",
		ProviderSessionID: &otherProviderSessionID,
		AgentName:         "codex",
		Model:             "other-chat-model",
		Title:             "Other Codex chat",
	}); err != nil {
		t.Fatalf("create other chat: %v", err)
	}
	if _, err := NewAgentChatStore(database).Create(ctx, CreateAgentChatParams{
		SessionID:         sess.ID,
		AgentSessionID:    "boss-correlation-model",
		ProviderSessionID: &providerSessionID,
		AgentName:         "codex",
		Model:             "codex-chat-model",
		Title:             "Codex chat",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	dir := filepath.Join(codexHome, "sessions", "2026", "08", "26")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir codex sessions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout.jsonl"), []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","payload":{"id":` + strconv.Quote(providerSessionID) + `,"timestamp":"2026-08-26T01:00:00Z","cwd":` + strconv.Quote(sess.WorktreePath) + `}}`,
		`{"timestamp":"2026-08-26T01:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write codex rollout: %v", err)
	}
	store := NewAgentRunStore(database)
	since := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	summary, err := store.Backfill(ctx, AgentRunBackfillParams{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if summary.InsertedCount != 1 {
		t.Fatalf("summary = %+v, want one inserted provider-chat run", summary)
	}
	runs, err := store.List(ctx, AgentRunFilter{SessionID: sess.ID, IncludeBackfilled: true, IncludeAllReasons: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v, want one backfilled run", runs)
	}
	if runs[0].AgentName != "codex" || runs[0].Model != "codex-chat-model" || runs[0].Effort != "medium" {
		t.Fatalf("run agent/model/effort = %q/%q/%q, want codex/codex-chat-model/medium", runs[0].AgentName, runs[0].Model, runs[0].Effort)
	}
}

func TestAgentRunStoreBackfillSessionsCollectsProviderChats(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess, err := NewSessionStore(database).Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Multiple provider chats",
		WorktreePath: "/work",
		BranchName:   "feat/providers",
		BaseBranch:   "main",
		AgentName:    "claude",
		Model:        "claude-model",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	firstProviderID := "provider-first"
	secondProviderID := "provider-second"
	chatStore := NewAgentChatStore(database)
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:         sess.ID,
		AgentSessionID:    "agent-first",
		ProviderSessionID: &firstProviderID,
		AgentName:         "codex",
		Model:             "first-model",
		Title:             "First",
	}); err != nil {
		t.Fatalf("create first chat: %v", err)
	}
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:         sess.ID,
		AgentSessionID:    "agent-second",
		ProviderSessionID: &secondProviderID,
		AgentName:         "codex",
		Model:             "second-model",
		Title:             "Second",
	}); err != nil {
		t.Fatalf("create second chat: %v", err)
	}

	sessions, err := NewAgentRunStore(database).backfillSessions(ctx)
	if err != nil {
		t.Fatalf("backfillSessions: %v", err)
	}
	var got *backfillSession
	for i := range sessions {
		if sessions[i].ID == sess.ID {
			got = &sessions[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("session %q not found in %#v", sess.ID, sessions)
	}
	if len(got.ProviderSessions) != 2 {
		t.Fatalf("provider sessions = %#v, want two collected provider chats", got.ProviderSessions)
	}
	if got.ProviderSessions[0].ID != firstProviderID || got.ProviderSessions[0].AgentName != "codex" || got.ProviderSessions[0].Model != "first-model" || got.ProviderSessions[0].Effort != "medium" {
		t.Fatalf("first provider = %#v, want %s/codex/first-model/medium", got.ProviderSessions[0], firstProviderID)
	}
	if got.ProviderSessions[1].ID != secondProviderID || got.ProviderSessions[1].AgentName != "codex" || got.ProviderSessions[1].Model != "second-model" || got.ProviderSessions[1].Effort != "medium" {
		t.Fatalf("second provider = %#v, want %s/codex/second-model/medium", got.ProviderSessions[1], secondProviderID)
	}
}

func TestAgentRunStoreBackfillHonorsCodexHome(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "custom-codex")
	t.Setenv("HOME", filepath.Join(home, "regular-home"))
	t.Setenv("CODEX_HOME", codexHome)
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess, err := NewSessionStore(database).Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Codex home",
		WorktreePath: filepath.Join(home, "work"),
		BranchName:   "feat/codex-home",
		BaseBranch:   "main",
		AgentName:    "codex",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	setSessionCreatedAtForBackfillTest(t, database, sess.ID, time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC))
	store := NewAgentRunStore(database)

	dir := filepath.Join(codexHome, "sessions", "2026", "08", "26")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir codex sessions: %v", err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		`{"timestamp":"2026-08-26T01:00:00Z","payload":{"id":"codex-parent","timestamp":"2026-08-26T01:00:00Z","cwd":` + strconv.Quote(sess.WorktreePath) + `}}`,
		`{"timestamp":"2026-08-26T01:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write codex rollout: %v", err)
	}

	since := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	summary, err := store.Backfill(ctx, AgentRunBackfillParams{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if summary.InsertedCount != 1 {
		t.Fatalf("summary = %+v, want one CODEX_HOME import", summary)
	}
}

func TestGroupCodexBackfillCandidatesMergesSameSessionRolloutFiles(t *testing.T) {
	outputA := int64(5)
	outputB := int64(7)
	start := time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC)

	got := groupCodexBackfillCandidates([]backfillCandidate{
		{
			AgentSessionID: "codex-session",
			AgentName:      "codex",
			WorktreePath:   "/tmp/work",
			Path:           "b.jsonl",
			StartedAt:      start.Add(time.Minute),
			StoppedAt:      start.Add(2 * time.Minute),
			Counts: agenttelemetry.Counts{
				ParentModelCallCount: 1,
				ToolCallCount:        2,
				OutputTokenCount:     &outputB,
			},
		},
		{
			AgentSessionID: "codex-session",
			AgentName:      "codex",
			WorktreePath:   "/tmp/work",
			Path:           "a.jsonl",
			StartedAt:      start,
			StoppedAt:      start.Add(time.Minute),
			Counts: agenttelemetry.Counts{
				ParentModelCallCount: 2,
				ToolCallCount:        1,
				OutputTokenCount:     &outputA,
			},
		},
	})

	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1", len(got))
	}
	if got[0].Counts.ParentModelCallCount != 3 || got[0].Counts.ToolCallCount != 3 {
		t.Fatalf("counts = parent %d tools %d, want 3/3", got[0].Counts.ParentModelCallCount, got[0].Counts.ToolCallCount)
	}
	if !got[0].StartedAt.Equal(start) || !got[0].StoppedAt.Equal(start.Add(2*time.Minute)) {
		t.Fatalf("bounds = %s..%s, want merged bounds", got[0].StartedAt, got[0].StoppedAt)
	}
	if got[0].Counts.OutputTokenCount == nil || *got[0].Counts.OutputTokenCount != 12 {
		t.Fatalf("output tokens = %v, want 12", got[0].Counts.OutputTokenCount)
	}
}

func TestGroupCodexBackfillCandidatesKeepsDistantRunsSeparate(t *testing.T) {
	start := time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC)
	got := groupCodexBackfillCandidates([]backfillCandidate{
		{
			AgentSessionID: "codex-session",
			AgentName:      "codex",
			WorktreePath:   "/tmp/work",
			StartedAt:      start,
			StoppedAt:      start.Add(time.Minute),
		},
		{
			AgentSessionID: "codex-session",
			AgentName:      "codex",
			WorktreePath:   "/tmp/work",
			StartedAt:      start.Add(2 * time.Hour),
			StoppedAt:      start.Add(2*time.Hour + time.Minute),
		},
	})
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 separate runs for distant starts", len(got))
	}
}

func TestCodexBackfillWindowAppliesAfterGrouping(t *testing.T) {
	start := time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC)
	grouped := groupCodexBackfillCandidates([]backfillCandidate{
		{
			AgentSessionID: "codex-session",
			AgentName:      "codex",
			WorktreePath:   "/tmp/work",
			StartedAt:      start,
			StoppedAt:      start.Add(10 * time.Minute),
		},
		{
			AgentSessionID: "codex-session",
			AgentName:      "codex",
			WorktreePath:   "/tmp/work",
			StartedAt:      start.Add(20 * time.Minute),
			StoppedAt:      start.Add(25 * time.Minute),
		},
	})
	if len(grouped) != 1 {
		t.Fatalf("grouped candidates = %d, want 1", len(grouped))
	}

	until := start.Add(15 * time.Minute)
	got := filterBackfillWindow(grouped, AgentRunBackfillParams{Until: &until})
	if len(got) != 1 || !got[0].StoppedAt.Equal(start.Add(25*time.Minute)) {
		t.Fatalf("until-filtered candidates = %#v, want complete grouped run", got)
	}

	since := start.Add(15 * time.Minute)
	got = filterBackfillWindow(grouped, AgentRunBackfillParams{Since: &since})
	if len(got) != 0 {
		t.Fatalf("since-filtered candidates = %#v, want grouped run excluded by original start", got)
	}
}

func setSessionCreatedAtForBackfillTest(t *testing.T, database *sql.DB, sessionID string, createdAt time.Time) {
	t.Helper()
	if _, err := database.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, sqlutil.FormatTime(createdAt), sessionID); err != nil {
		t.Fatalf("set session created_at: %v", err)
	}
}

func setSessionAgentNameForBackfillTest(t *testing.T, database *sql.DB, sessionID, agentName string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE sessions SET agent_name = ? WHERE id = ?`, agentName, sessionID); err != nil {
		t.Fatalf("set session agent_name: %v", err)
	}
}

func setSessionModelForBackfillTest(t *testing.T, database *sql.DB, sessionID, model, effectiveModel string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE sessions SET model = ?, effective_model = ? WHERE id = ?`, model, effectiveModel, sessionID); err != nil {
		t.Fatalf("set session model: %v", err)
	}
}
