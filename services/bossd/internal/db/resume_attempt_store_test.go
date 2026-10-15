package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
)

func TestResumeAttemptStore_RecordListDelete(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewResumeAttemptStore(database)
	var changes []string
	store.SetOnChange(func(sessionID string) {
		// Read the ledger in the callback: the write's row must be closed first.
		if _, err := store.ListForRun(ctx, sessionID, "run-a"); err != nil {
			t.Fatalf("read ledger from change callback: %v", err)
		}
		changes = append(changes, sessionID)
	})

	t0 := time.Date(2026, 10, 13, 23, 17, 0, 0, time.UTC)
	first, err := store.Record(ctx, RecordResumeAttemptParams{
		SessionID: sess.ID, RunKey: "run-a", AttemptedAt: t0,
		FailureClass: "auth_expired", FailureDetail: "API Error: 401",
	})
	if err != nil {
		t.Fatalf("record first: %v", err)
	}
	if first.ID == "" || first.AttemptNumber != 1 || first.FailureClass != "auth_expired" ||
		first.FailureDetail != "API Error: 401" || !first.AttemptedAt.Equal(t0) || first.CreatedAt.IsZero() {
		t.Fatalf("first attempt = %+v", first)
	}
	second, err := store.Record(ctx, RecordResumeAttemptParams{
		SessionID: sess.ID, RunKey: "run-a", AttemptedAt: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("record second: %v", err)
	}
	if second.AttemptNumber != 2 || second.FailureClass != "unknown" {
		t.Fatalf("second attempt number/class = %d/%q, want 2/unknown", second.AttemptNumber, second.FailureClass)
	}
	// A different run starts its own numbering.
	other, err := store.Record(ctx, RecordResumeAttemptParams{
		SessionID: sess.ID, RunKey: "run-b", AttemptedAt: t0, FailureClass: "crash",
	})
	if err != nil {
		t.Fatalf("record other run: %v", err)
	}
	if other.AttemptNumber != 1 {
		t.Fatalf("other run attempt number = %d, want 1", other.AttemptNumber)
	}

	got, err := store.ListForRun(ctx, sess.ID, "run-a")
	if err != nil {
		t.Fatalf("list run-a: %v", err)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("list run-a = %+v, want [first, second]", got)
	}

	n, err := store.DeleteForRun(ctx, sess.ID, "run-a")
	if err != nil || n != 2 {
		t.Fatalf("delete run-a = %d, %v; want 2, nil", n, err)
	}
	if got, err := store.ListForRun(ctx, sess.ID, "run-a"); err != nil || len(got) != 0 {
		t.Fatalf("list run-a after delete = %v, %v; want empty", got, err)
	}
	if got, err := store.ListForRun(ctx, sess.ID, "run-b"); err != nil || len(got) != 1 {
		t.Fatalf("list run-b after deleting run-a = %v, %v; want 1 row", got, err)
	}
	if n, err := store.DeleteForRun(ctx, sess.ID, "run-a"); err != nil || n != 0 {
		t.Fatalf("delete empty run = %d, %v; want 0, nil", n, err)
	}
	if len(changes) != 4 {
		t.Fatalf("change callbacks = %v, want three records and one deletion", changes)
	}
	for _, sessionID := range changes {
		if sessionID != sess.ID {
			t.Fatalf("change session ID = %q, want %q", sessionID, sess.ID)
		}
	}
	// Replacing the callback from inside it must not hold the hook mutex.
	store.SetOnChange(func(sessionID string) {
		changes = append(changes, sessionID)
		store.SetOnChange(nil)
	})

	// After the budget reset the next attempt is numbered 1 again.
	again, err := store.Record(ctx, RecordResumeAttemptParams{
		SessionID: sess.ID, RunKey: "run-a", AttemptedAt: t0.Add(time.Hour),
	})
	if err != nil || again.AttemptNumber != 1 {
		t.Fatalf("record after reset = %+v, %v; want attempt 1", again, err)
	}
	if len(changes) != 5 {
		t.Fatalf("change callbacks after reset = %v, want five", changes)
	}
}

func TestResumeAttemptStore_RecordRejectsInvalidInput(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewResumeAttemptStore(database)
	store.SetOnChange(func(sessionID string) {
		t.Errorf("failed Record notified change for %q", sessionID)
	})
	now := time.Now()

	cases := map[string]RecordResumeAttemptParams{
		"missing session": {RunKey: "r", AttemptedAt: now},
		"missing run key": {SessionID: sess.ID, AttemptedAt: now},
		"missing time":    {SessionID: sess.ID, RunKey: "r"},
		"unknown class":   {SessionID: sess.ID, RunKey: "r", AttemptedAt: now, FailureClass: "bogus"},
		"unknown session": {SessionID: "no-such-session", RunKey: "r", AttemptedAt: now},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Record(ctx, params); err == nil {
				t.Fatal("Record succeeded, want error")
			}
		})
	}
}

func TestResumeAttemptStore_SessionDeleteRemovesAttempts(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sessions := NewSessionStore(database)
	sess := createTestSession(t, sessions, repo.ID)
	store := NewResumeAttemptStore(database)

	if _, err := store.Record(ctx, RecordResumeAttemptParams{
		SessionID: sess.ID, RunKey: "run-a", AttemptedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := sessions.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	var n int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_resume_attempts WHERE session_id = ?`, sess.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d resume attempts survived their session", n)
	}
}

func TestSetDismissedAlert_RoundTrips(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sessions := NewSessionStore(database)
	sess := createTestSession(t, sessions, repo.ID)

	fresh, err := sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if fresh.DismissedAlertKey != "" || fresh.DismissedAlertAt != nil {
		t.Fatalf("fresh session dismissal = %q/%v, want empty/nil", fresh.DismissedAlertKey, fresh.DismissedAlertAt)
	}

	at := time.Date(2026, 10, 13, 23, 30, 0, 0, time.UTC)
	if err := sessions.SetDismissedAlert(ctx, sess.ID, "resume:auth_expired", at); err != nil {
		t.Fatalf("SetDismissedAlert: %v", err)
	}
	got, err := sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DismissedAlertKey != "resume:auth_expired" || got.DismissedAlertAt == nil || !got.DismissedAlertAt.Equal(at) {
		t.Fatalf("dismissal = %q/%v, want resume:auth_expired/%v", got.DismissedAlertKey, got.DismissedAlertAt, at)
	}
	if !got.UpdatedAt.Equal(fresh.UpdatedAt) {
		t.Errorf("updated_at moved %v -> %v; a dismissal is not a content change", fresh.UpdatedAt, got.UpdatedAt)
	}

	// The list scans carry the fields too.
	rows, err := sessions.ListWithRepo(ctx, repo.ID)
	if err != nil {
		t.Fatalf("list with repo: %v", err)
	}
	if len(rows) != 1 || rows[0].DismissedAlertKey != "resume:auth_expired" || rows[0].DismissedAlertAt == nil {
		t.Fatalf("ListWithRepo dismissal not carried: %+v", rows)
	}

	// A later dismissal replaces the earlier one.
	if err := sessions.SetDismissedAlert(ctx, sess.ID, "resume:disk_full", at.Add(time.Minute)); err != nil {
		t.Fatalf("second SetDismissedAlert: %v", err)
	}
	if got, _ := sessions.Get(ctx, sess.ID); got.DismissedAlertKey != "resume:disk_full" {
		t.Fatalf("key after replace = %q, want resume:disk_full", got.DismissedAlertKey)
	}

	if err := sessions.SetDismissedAlert(ctx, "missing", "k", at); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing session: got %v, want sql.ErrNoRows", err)
	}
	if err := sessions.SetDismissedAlert(ctx, "", "k", at); err == nil {
		t.Fatal("empty id: want error")
	}
	if err := sessions.SetDismissedAlert(ctx, sess.ID, "", at); err == nil {
		t.Fatal("empty key: want error")
	}
}

func TestResumeAttemptStoreSetFailure(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	store := NewResumeAttemptStore(database)
	attempt, err := store.Record(ctx, RecordResumeAttemptParams{SessionID: sess.ID, RunKey: "run", AttemptedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetFailure(ctx, attempt.ID, "auth_expired"); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListForRun(ctx, sess.ID, "run")
	if err != nil || len(rows) != 1 || rows[0].FailureClass != "auth_expired" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if err := store.SetFailure(ctx, attempt.ID, "invalid"); err == nil {
		t.Fatal("invalid failure class accepted")
	}
}

func TestCommitRunResumeRejectsConcurrentCompletionOrArchive(t *testing.T) {
	for _, kind := range []string{"completion", "archive", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			database := setupTestDB(t)
			ctx := context.Background()
			store := NewSessionStore(database)
			repo := createTestRepo(t, NewRepoStore(database))
			sess := createTestSession(t, store, repo.ID)
			prior := "old"
			priorPtr := &prior
			working := int(machine.ImplementingPlan)
			if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{State: &working, AgentSessionID: &priorPtr}); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "completion":
				done := int(machine.Merged)
				if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{State: &done}); err != nil {
					t.Fatal(err)
				}
			case "archive":
				if err := store.Archive(ctx, sess.ID); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				other := "other"
				p := &other
				if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{AgentSessionID: &p}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.CommitRunResume(ctx, sess.ID, working, &prior, "new"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("err=%v want lost ownership", err)
			}
			current, err := store.Get(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.AgentSessionID != nil && *current.AgentSessionID == "new" {
				t.Fatal("overwrote newer session state")
			}
		})
	}
}

func TestObserveRunDeathPersistsAcrossRestartAndResetsForReplacement(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	store := NewSessionStore(database)
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, store, repo.ID)
	prior := "old"
	priorPtr := &prior
	working := int(machine.ImplementingPlan)
	row, err := store.Update(ctx, sess.ID, UpdateSessionParams{State: &working, AgentSessionID: &priorPtr})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	first, err := store.ObserveRunDeath(ctx, sess.ID, prior, working, at)
	if err != nil || !first.Equal(at) {
		t.Fatalf("death=%v err=%v", first, err)
	}
	// A fresh store after a daemon restart keeps the first observed death.
	restarted := NewSessionStore(database)
	again, err := restarted.ObserveRunDeath(ctx, sess.ID, prior, working, at.Add(30*time.Second))
	if err != nil || !again.Equal(at) {
		t.Fatalf("death=%v err=%v", again, err)
	}
	after, err := store.Get(ctx, sess.ID)
	if err != nil || !after.UpdatedAt.Equal(row.UpdatedAt) {
		t.Fatalf("observation changed UpdatedAt: before=%v after=%v err=%v", row.UpdatedAt, after.UpdatedAt, err)
	}
	if _, err = store.CommitRunResume(ctx, sess.ID, working, &prior, "new"); err != nil {
		t.Fatal(err)
	}
	next, err := store.ObserveRunDeath(ctx, sess.ID, "new", working, at.Add(time.Hour))
	if err != nil || !next.Equal(at.Add(time.Hour)) {
		t.Fatalf("new death=%v err=%v", next, err)
	}
}

func TestGiveUpRunResumePreservesConcurrentOwner(t *testing.T) {
	for _, kind := range []string{"exhausted", "completion", "finalization", "replacement", "archive", "terminal-telemetry"} {
		t.Run(kind, func(t *testing.T) {
			database := setupTestDB(t)
			ctx := context.Background()
			store := NewSessionStore(database)
			repo := createTestRepo(t, NewRepoStore(database))
			sess := createTestSession(t, store, repo.ID)
			prior := "old"
			priorPtr := &prior
			working := int(machine.ImplementingPlan)
			if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{State: &working, AgentSessionID: &priorPtr}); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "completion", "finalization":
				state := int(machine.Merged)
				if kind == "finalization" {
					state = int(machine.Finalizing)
				}
				if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{State: &state}); err != nil {
					t.Fatal(err)
				}
			case "archive":
				if err := store.Archive(ctx, sess.ID); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				other := "replacement"
				p := &other
				if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{AgentSessionID: &p}); err != nil {
					t.Fatal(err)
				}
			case "terminal-telemetry":
				if _, err := NewAgentRunStore(database).Start(ctx, AgentRun{SessionID: sess.ID, AgentSessionID: prior, TerminalState: AgentRunTerminalReviewReady}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.Get(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = store.GiveUpRunResume(ctx, sess.ID, working, &prior, "budget exhausted")
			if kind == "exhausted" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("err=%v want lost ownership", err)
			}
			after, err := store.Get(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "exhausted" {
				if after.State != machine.Blocked || after.BlockedReason == nil || *after.BlockedReason != "budget exhausted" {
					t.Fatalf("give-up not persisted: %+v", after)
				}
			} else if after.State != before.State || after.BlockedReason != nil || !after.UpdatedAt.Equal(before.UpdatedAt) || *after.AgentSessionID != *before.AgentSessionID || (after.ArchivedAt == nil) != (before.ArchivedAt == nil) {
				t.Fatalf("stale give-up changed newer owner: before=%+v after=%+v", before, after)
			}
		})
	}
}
