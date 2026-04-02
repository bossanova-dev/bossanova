package db

import (
	"context"
	"database/sql"
	"sync"
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

func TestWorkflowStore_FullLifecycle(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	// 1. Create — starts as pending/plan/leg 0.
	w, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID,
		RepoID:    repo.ID,
		PlanPath:  "docs/plans/lifecycle.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.Status != models.WorkflowStatusPending {
		t.Fatalf("initial status = %q, want pending", w.Status)
	}

	// 2. Transition to running/plan/leg 1.
	running := string(models.WorkflowStatusRunning)
	planStep := string(models.WorkflowStepPlan)
	leg1 := 1
	w, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		Status: &running, CurrentStep: &planStep, FlightLeg: &leg1,
	})
	if err != nil {
		t.Fatalf("update to running: %v", err)
	}
	if w.Status != models.WorkflowStatusRunning || w.FlightLeg != 1 {
		t.Fatalf("after running: status=%q leg=%d", w.Status, w.FlightLeg)
	}

	// 3. Progress through implement/leg 2.
	implStep := string(models.WorkflowStepImplement)
	leg2 := 2
	w, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		CurrentStep: &implStep, FlightLeg: &leg2,
	})
	if err != nil {
		t.Fatalf("update to implement: %v", err)
	}
	if w.CurrentStep != models.WorkflowStepImplement || w.FlightLeg != 2 {
		t.Fatalf("after implement: step=%q leg=%d", w.CurrentStep, w.FlightLeg)
	}

	// 4. Handoff loop: resume/leg 3.
	resumeStep := string(models.WorkflowStepResume)
	leg3 := 3
	w, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		CurrentStep: &resumeStep, FlightLeg: &leg3,
	})
	if err != nil {
		t.Fatalf("update to resume: %v", err)
	}

	// 5. Verify step/leg 4.
	verifyStep := string(models.WorkflowStepVerify)
	leg4 := 4
	w, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		CurrentStep: &verifyStep, FlightLeg: &leg4,
	})
	if err != nil {
		t.Fatalf("update to verify: %v", err)
	}

	// 6. Land step/leg 5.
	landStep := string(models.WorkflowStepLand)
	leg5 := 5
	w, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		CurrentStep: &landStep, FlightLeg: &leg5,
	})
	if err != nil {
		t.Fatalf("update to land: %v", err)
	}

	// 7. Complete.
	completed := string(models.WorkflowStatusCompleted)
	w, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		Status: &completed,
	})
	if err != nil {
		t.Fatalf("update to completed: %v", err)
	}
	if w.Status != models.WorkflowStatusCompleted {
		t.Errorf("final status = %q, want completed", w.Status)
	}
	if w.CurrentStep != models.WorkflowStepLand {
		t.Errorf("final step = %q, want land", w.CurrentStep)
	}
	if w.FlightLeg != 5 {
		t.Errorf("final leg = %d, want 5", w.FlightLeg)
	}

	// Verify it appears in completed list.
	completedList, err := store.ListByStatus(ctx, string(models.WorkflowStatusCompleted))
	if err != nil {
		t.Fatalf("list completed: %v", err)
	}
	if len(completedList) != 1 || completedList[0].ID != w.ID {
		t.Errorf("completed list: got %d entries", len(completedList))
	}
}

func TestWorkflowStore_ListActiveBySessionIDsExcludesTerminalStatuses(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	// Create an active workflow that SHOULD be returned.
	wRunning, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID, RepoID: repo.ID, PlanPath: "running.md", MaxLegs: 3,
	})
	if err != nil {
		t.Fatalf("create running: %v", err)
	}
	runningStatus := string(models.WorkflowStatusRunning)
	if _, err := store.Update(ctx, wRunning.ID, UpdateWorkflowParams{Status: &runningStatus}); err != nil {
		t.Fatalf("update to running: %v", err)
	}

	// Create workflows in various terminal states.
	wFailed, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID, RepoID: repo.ID, PlanPath: "failed.md", MaxLegs: 1,
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	failedStatus := string(models.WorkflowStatusFailed)
	if _, err := store.Update(ctx, wFailed.ID, UpdateWorkflowParams{Status: &failedStatus}); err != nil {
		t.Fatalf("update to failed: %v", err)
	}

	wCancelled, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID, RepoID: repo.ID, PlanPath: "cancelled.md", MaxLegs: 1,
	})
	if err != nil {
		t.Fatalf("create cancelled: %v", err)
	}
	cancelledStatus := string(models.WorkflowStatusCancelled)
	if _, err := store.Update(ctx, wCancelled.ID, UpdateWorkflowParams{Status: &cancelledStatus}); err != nil {
		t.Fatalf("update to cancelled: %v", err)
	}

	wCompleted, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID, RepoID: repo.ID, PlanPath: "completed.md", MaxLegs: 1,
	})
	if err != nil {
		t.Fatalf("create completed: %v", err)
	}
	completedStatus := string(models.WorkflowStatusCompleted)
	if _, err := store.Update(ctx, wCompleted.ID, UpdateWorkflowParams{Status: &completedStatus}); err != nil {
		t.Fatalf("update to completed: %v", err)
	}

	results, err := store.ListActiveBySessionIDs(ctx, []string{sess.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// Active workflow should be included; all terminal states should be excluded.
	ids := make(map[string]bool)
	for _, w := range results {
		ids[w.ID] = true
	}
	if !ids[wRunning.ID] {
		t.Error("running workflow should be in results")
	}
	if ids[wFailed.ID] {
		t.Error("failed workflow should NOT be in results")
	}
	if ids[wCancelled.ID] {
		t.Error("cancelled workflow should NOT be in results")
	}
	if ids[wCompleted.ID] {
		t.Error("completed workflow should NOT be in results")
	}
}

func TestWorkflowStore_ConcurrentAccess(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	// Create a workflow.
	w, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID,
		RepoID:    repo.ID,
		PlanPath:  "docs/plans/concurrent.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Concurrent reads should not race.
	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for range 10 {
		wg.Go(func() {
			_, err := store.Get(ctx, w.ID)
			if err != nil {
				errCh <- err
			}
		})
	}
	for range 10 {
		wg.Go(func() {
			_, err := store.List(ctx)
			if err != nil {
				errCh <- err
			}
		})
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent access error: %v", err)
	}
}

func TestWorkflowStore_FailureWithError(t *testing.T) {
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
		PlanPath:  "docs/plans/failure.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Set to running, then fail with an error.
	running := string(models.WorkflowStatusRunning)
	_, err = store.Update(ctx, w.ID, UpdateWorkflowParams{Status: &running})
	if err != nil {
		t.Fatalf("update to running: %v", err)
	}

	failed := string(models.WorkflowStatusFailed)
	errMsg := "claude process crashed: exit code 1"
	errPtr := &errMsg
	w, err = store.Update(ctx, w.ID, UpdateWorkflowParams{
		Status:    &failed,
		LastError: &errPtr,
	})
	if err != nil {
		t.Fatalf("update to failed: %v", err)
	}
	if w.Status != models.WorkflowStatusFailed {
		t.Errorf("status = %q, want failed", w.Status)
	}
	if w.LastError == nil || *w.LastError != "claude process crashed: exit code 1" {
		t.Errorf("last_error = %v, want %q", w.LastError, "claude process crashed: exit code 1")
	}

	// Verify it doesn't appear in running list.
	runningList, err := store.ListByStatus(ctx, string(models.WorkflowStatusRunning))
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(runningList) != 0 {
		t.Errorf("running list should be empty, got %d", len(runningList))
	}
}

func TestWorkflowStore_FailOrphaned(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)

	// Create three workflows: one pending (DB default), one running, one completed.
	wPending, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID, RepoID: repo.ID, PlanPath: "pending.md", MaxLegs: 1,
	})
	if err != nil {
		t.Fatalf("create pending: %v", err)
	}

	wRunning, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID, RepoID: repo.ID, PlanPath: "running.md", MaxLegs: 1,
	})
	if err != nil {
		t.Fatalf("create running: %v", err)
	}
	running := string(models.WorkflowStatusRunning)
	if _, err := store.Update(ctx, wRunning.ID, UpdateWorkflowParams{Status: &running}); err != nil {
		t.Fatalf("update to running: %v", err)
	}

	wCompleted, err := store.Create(ctx, CreateWorkflowParams{
		SessionID: sess.ID, RepoID: repo.ID, PlanPath: "completed.md", MaxLegs: 1,
	})
	if err != nil {
		t.Fatalf("create completed: %v", err)
	}
	completed := string(models.WorkflowStatusCompleted)
	if _, err := store.Update(ctx, wCompleted.ID, UpdateWorkflowParams{Status: &completed}); err != nil {
		t.Fatalf("update to completed: %v", err)
	}

	// FailOrphaned should affect the pending and running workflows.
	n, err := store.FailOrphaned(ctx)
	if err != nil {
		t.Fatalf("FailOrphaned: %v", err)
	}
	if n != 2 {
		t.Errorf("FailOrphaned affected %d rows, want 2", n)
	}

	// Verify pending workflow is now failed.
	got, err := store.Get(ctx, wPending.ID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if got.Status != models.WorkflowStatusFailed {
		t.Errorf("pending workflow status = %q, want failed", got.Status)
	}
	if got.LastError == nil || *got.LastError != "daemon restarted" {
		t.Errorf("pending workflow last_error = %v, want %q", got.LastError, "daemon restarted")
	}

	// Verify running workflow is now failed.
	got, err = store.Get(ctx, wRunning.ID)
	if err != nil {
		t.Fatalf("get running: %v", err)
	}
	if got.Status != models.WorkflowStatusFailed {
		t.Errorf("running workflow status = %q, want failed", got.Status)
	}

	// Verify completed workflow is untouched.
	got, err = store.Get(ctx, wCompleted.ID)
	if err != nil {
		t.Fatalf("get completed: %v", err)
	}
	if got.Status != models.WorkflowStatusCompleted {
		t.Errorf("completed workflow status = %q, want completed", got.Status)
	}

	// No running workflows remain.
	runningList, err := store.ListByStatus(ctx, string(models.WorkflowStatusRunning))
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(runningList) != 0 {
		t.Errorf("running list should be empty, got %d", len(runningList))
	}
}
