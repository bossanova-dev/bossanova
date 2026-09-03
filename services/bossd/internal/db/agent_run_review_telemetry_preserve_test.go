package db

import (
	"context"
	"testing"
	"time"
)

// TestRecordTelemetryPreservesReviewFieldsOnZeroValuedWrite pins that a telemetry
// write which carries no review fields leaves an already-recorded reviewer count
// and terminal state alone.
//
// The RecordRunTelemetry gRPC request has no field for either column, so its
// handler necessarily builds AgentRunTelemetry with both at their zero values.
// While the UPDATE assigned them unconditionally, any such write erased what the
// transcript extractor had already recorded -- silently, and only for runs that
// happened to record telemetry twice, which is why no existing test caught it.
func TestRecordTelemetryPreservesReviewFieldsOnZeroValuedWrite(t *testing.T) {
	ctx := context.Background()
	database := setupTestDB(t)
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)

	run, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-1",
		Model:          "gpt-5",
		StartedAt:      time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The extractor records the run's review telemetry.
	if err := store.RecordTelemetry(ctx, run.ID, AgentRunTelemetry{
		ToolCallCount:         3,
		ReviewerDispatchCount: 8,
		TerminalState:         AgentRunTerminalReviewReady,
	}); err != nil {
		t.Fatalf("RecordTelemetry (extractor): %v", err)
	}

	// A second recorder that cannot carry review fields writes afterwards.
	if err := store.RecordTelemetry(ctx, run.ID, AgentRunTelemetry{
		ToolCallCount: 5,
	}); err != nil {
		t.Fatalf("RecordTelemetry (gRPC-shaped): %v", err)
	}

	var dispatches int64
	var terminal string
	var tools int64
	if err := database.QueryRow(
		`SELECT reviewer_dispatch_count, terminal_state, tool_call_count FROM agent_runs WHERE id = ?`,
		run.ID,
	).Scan(&dispatches, &terminal, &tools); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if dispatches != 8 {
		t.Errorf("reviewer_dispatch_count = %d, want 8 preserved -- a recorder that cannot carry review telemetry erased it", dispatches)
	}
	if terminal != AgentRunTerminalReviewReady {
		t.Errorf("terminal_state = %q, want %q preserved", terminal, AgentRunTerminalReviewReady)
	}
	// The fields the second write DID carry must still land, or the preserve
	// would have turned into a blanket ignore of later telemetry.
	if tools != 5 {
		t.Errorf("tool_call_count = %d, want 5 -- the non-review fields must still overwrite", tools)
	}
}

// TestRecordTelemetryStillOverwritesRealReviewValues guards the other direction:
// preserving on a zero-valued write must not stop a real value from replacing an
// earlier one, which would freeze the first count ever recorded.
func TestRecordTelemetryStillOverwritesRealReviewValues(t *testing.T) {
	ctx := context.Background()
	database := setupTestDB(t)
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewAgentRunStore(database)

	run, err := store.Start(ctx, AgentRun{
		SessionID:      sess.ID,
		AgentSessionID: "agent-1",
		Model:          "gpt-5",
		StartedAt:      time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := store.RecordTelemetry(ctx, run.ID, AgentRunTelemetry{
		ReviewerDispatchCount: 2,
		TerminalState:         AgentRunTerminalPartial,
	}); err != nil {
		t.Fatalf("RecordTelemetry (first): %v", err)
	}
	if err := store.RecordTelemetry(ctx, run.ID, AgentRunTelemetry{
		ReviewerDispatchCount: 9,
		TerminalState:         AgentRunTerminalBlocked,
	}); err != nil {
		t.Fatalf("RecordTelemetry (second): %v", err)
	}

	var dispatches int64
	var terminal string
	if err := database.QueryRow(
		`SELECT reviewer_dispatch_count, terminal_state FROM agent_runs WHERE id = ?`,
		run.ID,
	).Scan(&dispatches, &terminal); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if dispatches != 9 {
		t.Errorf("reviewer_dispatch_count = %d, want 9 -- a real value must still overwrite", dispatches)
	}
	if terminal != AgentRunTerminalBlocked {
		t.Errorf("terminal_state = %q, want %q -- a real value must still overwrite", terminal, AgentRunTerminalBlocked)
	}
}
