package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/recurser/bossalib/models"
)

func createTestSession(t *testing.T, sessionStore *SQLiteSessionStore, repoID string) *models.Session {
	t.Helper()
	sess, err := sessionStore.Create(context.Background(), CreateSessionParams{
		RepoID:       repoID,
		Title:        "Workflow test session",
		WorktreePath: "/tmp/wt/workflow-test",
		BranchName:   "feat/workflow-test",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}
	return sess
}

func TestWorkflowStore_CreateAndGet(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	sha := "abc123"
	cfg := `{"max_flight_legs": 10}`
	w, err := store.Create(ctx, CreateWorkflowParams{
		SessionID:      sess.ID,
		RepoID:         repo.ID,
		PlanPath:       "docs/plans/test-plan.md",
		MaxLegs:        10,
		StartCommitSHA: &sha,
		ConfigJSON:     &cfg,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.ID == "" {
		t.Error("id should not be empty")
	}
	if w.SessionID != sess.ID {
		t.Errorf("session_id = %q, want %q", w.SessionID, sess.ID)
	}
	if w.RepoID != repo.ID {
		t.Errorf("repo_id = %q, want %q", w.RepoID, repo.ID)
	}
	if w.PlanPath != "docs/plans/test-plan.md" {
		t.Errorf("plan_path = %q, want %q", w.PlanPath, "docs/plans/test-plan.md")
	}
	if w.Status != models.WorkflowStatusPending {
		t.Errorf("status = %q, want %q", w.Status, models.WorkflowStatusPending)
	}
	if w.CurrentStep != models.WorkflowStepPlan {
		t.Errorf("current_step = %q, want %q", w.CurrentStep, models.WorkflowStepPlan)
	}
	if w.FlightLeg != 0 {
		t.Errorf("flight_leg = %d, want 0", w.FlightLeg)
	}
	if w.MaxLegs != 10 {
		t.Errorf("max_legs = %d, want 10", w.MaxLegs)
	}
	if w.StartCommitSHA == nil || *w.StartCommitSHA != "abc123" {
		t.Errorf("start_commit_sha = %v, want %q", w.StartCommitSHA, "abc123")
	}
	if w.ConfigJSON == nil || *w.ConfigJSON != `{"max_flight_legs": 10}` {
		t.Errorf("config_json = %v, want %q", w.ConfigJSON, `{"max_flight_legs": 10}`)
	}

	// Get round-trip.
	got, err := store.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != w.ID {
		t.Errorf("id = %q, want %q", got.ID, w.ID)
	}
	if got.PlanPath != w.PlanPath {
		t.Errorf("plan_path = %q, want %q", got.PlanPath, w.PlanPath)
	}
}

func TestWorkflowStore_GetNonexistent(t *testing.T) {
	db := setupTestDB(t)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexistent-id")
	if err != sql.ErrNoRows {
		t.Errorf("get nonexistent: got %v, want sql.ErrNoRows", err)
	}
}

func TestWorkflowStore_Update(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	w, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID,
		RepoID:    repo.ID,
		PlanPath:  "docs/plans/update-test.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update status and step.
	newStatus := string(models.WorkflowStatusRunning)
	newStep := string(models.WorkflowStepImplement)
	newLeg := 1
	updated, err := store.Update(ctx, w.ID, UpdateWorkflowParams{
		Status:      &newStatus,
		CurrentStep: &newStep,
		FlightLeg:   &newLeg,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != models.WorkflowStatusRunning {
		t.Errorf("status = %q, want %q", updated.Status, models.WorkflowStatusRunning)
	}
	if updated.CurrentStep != models.WorkflowStepImplement {
		t.Errorf("current_step = %q, want %q", updated.CurrentStep, models.WorkflowStepImplement)
	}
	if updated.FlightLeg != 1 {
		t.Errorf("flight_leg = %d, want 1", updated.FlightLeg)
	}

	// Update with error.
	errMsg := "plan validation failed"
	errPtr := &errMsg
	updated, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		LastError: &errPtr,
	})
	if err != nil {
		t.Fatalf("update error: %v", err)
	}
	if updated.LastError == nil || *updated.LastError != "plan validation failed" {
		t.Errorf("last_error = %v, want %q", updated.LastError, "plan validation failed")
	}

	// Clear error.
	var nilErr *string
	updated, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		LastError: &nilErr,
	})
	if err != nil {
		t.Fatalf("clear error: %v", err)
	}
	if updated.LastError != nil {
		t.Errorf("last_error should be nil after clear, got %q", *updated.LastError)
	}
}

func TestWorkflowStore_UpdateNonexistent(t *testing.T) {
	db := setupTestDB(t)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	newStatus := "running"
	_, err := store.Update(ctx, "nonexistent-id", UpdateWorkflowParams{
		Status: &newStatus,
	})
	if err != sql.ErrNoRows {
		t.Errorf("update nonexistent: got %v, want sql.ErrNoRows", err)
	}
}

func TestWorkflowStore_List(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	// Empty list.
	workflows, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(workflows) != 0 {
		t.Errorf("list empty: got %d, want 0", len(workflows))
	}

	// Create two workflows.
	_, err = store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID,
		RepoID:    repo.ID,
		PlanPath:  "docs/plans/plan-1.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("create w1: %v", err)
	}
	_, err = store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID,
		RepoID:    repo.ID,
		PlanPath:  "docs/plans/plan-2.md",
		MaxLegs:   10,
	})
	if err != nil {
		t.Fatalf("create w2: %v", err)
	}

	workflows, err = store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(workflows) != 2 {
		t.Fatalf("list: got %d, want 2", len(workflows))
	}
}

func TestWorkflowStore_ListByStatus(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	// Create two workflows: one pending, one running.
	w1, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID,
		RepoID:    repo.ID,
		PlanPath:  "docs/plans/pending.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("create w1: %v", err)
	}
	w2, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID,
		RepoID:    repo.ID,
		PlanPath:  "docs/plans/running.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("create w2: %v", err)
	}

	// Update w2 to running.
	runningStatus := string(models.WorkflowStatusRunning)
	if _, err := store.Update(ctx, w2.ID, UpdateWorkflowParams{
		Status: &runningStatus,
	}); err != nil {
		t.Fatalf("update w2: %v", err)
	}

	// ListByStatus pending.
	pending, err := store.ListByStatus(ctx, string(models.WorkflowStatusPending))
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(pending))
	}
	if pending[0].ID != w1.ID {
		t.Errorf("pending[0].ID = %q, want %q", pending[0].ID, w1.ID)
	}

	// ListByStatus running.
	running, err := store.ListByStatus(ctx, string(models.WorkflowStatusRunning))
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("running count = %d, want 1", len(running))
	}
	if running[0].ID != w2.ID {
		t.Errorf("running[0].ID = %q, want %q", running[0].ID, w2.ID)
	}

	// ListByStatus completed — empty.
	completed, err := store.ListByStatus(ctx, string(models.WorkflowStatusCompleted))
	if err != nil {
		t.Fatalf("list completed: %v", err)
	}
	if len(completed) != 0 {
		t.Errorf("completed count = %d, want 0", len(completed))
	}
}

func TestWorkflowStore_CreateWithNilOptionals(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	w, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID,
		RepoID:    repo.ID,
		PlanPath:  "docs/plans/minimal.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.StartCommitSHA != nil {
		t.Errorf("start_commit_sha = %v, want nil", w.StartCommitSHA)
	}
	if w.ConfigJSON != nil {
		t.Errorf("config_json = %v, want nil", w.ConfigJSON)
	}
	if w.LastError != nil {
		t.Errorf("last_error = %v, want nil", w.LastError)
	}
}
