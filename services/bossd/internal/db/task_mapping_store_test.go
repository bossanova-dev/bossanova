package db

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/models"
)

func TestTaskMappingStore_CreateAndGetByExternalID(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewTaskMappingStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	m, err := store.Create(ctx, CreateTaskMappingParams{
		ExternalID: "dependabot:pr:https://github.com/foo/bar:42",
		PluginName: "dependabot",
		RepoID:     repo.ID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.ID == "" {
		t.Error("id should not be empty")
	}
	if m.ExternalID != "dependabot:pr:https://github.com/foo/bar:42" {
		t.Errorf("external_id = %q, want %q", m.ExternalID, "dependabot:pr:https://github.com/foo/bar:42")
	}
	if m.PluginName != "dependabot" {
		t.Errorf("plugin_name = %q, want %q", m.PluginName, "dependabot")
	}
	if m.Status != models.TaskMappingStatusPending {
		t.Errorf("status = %v, want Pending", m.Status)
	}

	// GetByExternalID round-trip.
	got, err := store.GetByExternalID(ctx, m.ExternalID)
	if err != nil {
		t.Fatalf("get by external id: %v", err)
	}
	if got.ID != m.ID {
		t.Errorf("id = %q, want %q", got.ID, m.ID)
	}
	if got.RepoID != repo.ID {
		t.Errorf("repo_id = %q, want %q", got.RepoID, repo.ID)
	}
}

func TestTaskMappingStore_Delete(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewTaskMappingStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	m, err := store.Create(ctx, CreateTaskMappingParams{
		ExternalID: "dependabot:pr:https://github.com/foo/bar:99",
		PluginName: "dependabot",
		RepoID:     repo.ID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Delete the mapping.
	if err := store.Delete(ctx, m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// GetByExternalID should now return not-found.
	got, err := store.GetByExternalID(ctx, m.ExternalID)
	if err == nil && got != nil {
		t.Error("expected not-found after delete, but got a result")
	}

	// Deleting a non-existent ID should return an error.
	if err := store.Delete(ctx, "nonexistent-id"); err == nil {
		t.Error("expected error when deleting non-existent mapping")
	}
}

func TestTaskMappingStore_DuplicateExternalID(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewTaskMappingStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	params := CreateTaskMappingParams{
		ExternalID: "dependabot:pr:https://github.com/foo/bar:42",
		PluginName: "dependabot",
		RepoID:     repo.ID,
	}
	if _, err := store.Create(ctx, params); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := store.Create(ctx, params)
	if err == nil {
		t.Error("expected error for duplicate external_id")
	}
}

func TestTaskMappingStore_UpdateStatusAndSessionID(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewTaskMappingStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Fix dependabot PR",
		WorktreePath: "/tmp/wt/fix-dep",
		BranchName:   "dependabot/npm_and_yarn/lodash-4.17.21",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	m, err := store.Create(ctx, CreateTaskMappingParams{
		ExternalID: "dependabot:pr:https://github.com/foo/bar:42",
		PluginName: "dependabot",
		RepoID:     repo.ID,
	})
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Update status and session_id.
	newStatus := models.TaskMappingStatusInProgress
	sessionID := sess.ID
	sessionPtr := &sessionID
	updated, err := store.Update(ctx, m.ID, UpdateTaskMappingParams{
		Status:    &newStatus,
		SessionID: &sessionPtr,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != models.TaskMappingStatusInProgress {
		t.Errorf("status = %v, want InProgress", updated.Status)
	}
	if updated.SessionID == nil || *updated.SessionID != sess.ID {
		t.Errorf("session_id = %v, want %q", updated.SessionID, sess.ID)
	}

	// Verify via GetBySessionID.
	got, err := store.GetBySessionID(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get by session id: %v", err)
	}
	if got.ID != m.ID {
		t.Errorf("id = %q, want %q", got.ID, m.ID)
	}
}

func TestTaskMappingStore_ListPending(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewTaskMappingStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	// Create two mappings: one with pending update, one without.
	m1, err := store.Create(ctx, CreateTaskMappingParams{
		ExternalID: "ext-1",
		PluginName: "dependabot",
		RepoID:     repo.ID,
	})
	if err != nil {
		t.Fatalf("create m1: %v", err)
	}
	if _, err := store.Create(ctx, CreateTaskMappingParams{
		ExternalID: "ext-2",
		PluginName: "dependabot",
		RepoID:     repo.ID,
	}); err != nil {
		t.Fatalf("create m2: %v", err)
	}

	// Set pending_update_status on m1 only.
	pendingStatus := models.TaskMappingStatusCompleted
	pendingStatusPtr := &pendingStatus
	if _, err := store.Update(ctx, m1.ID, UpdateTaskMappingParams{
		PendingUpdateStatus: &pendingStatusPtr,
	}); err != nil {
		t.Fatalf("set pending status: %v", err)
	}

	// ListPending should return only m1.
	pending, err := store.ListPending(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(pending))
	}
	if pending[0].ID != m1.ID {
		t.Errorf("pending[0].ID = %q, want %q", pending[0].ID, m1.ID)
	}
}

func TestTaskMappingStore_UpdatePendingFieldsAndClear(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewTaskMappingStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	m, err := store.Create(ctx, CreateTaskMappingParams{
		ExternalID: "ext-pending",
		PluginName: "dependabot",
		RepoID:     repo.ID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Set pending update fields.
	pendingStatus := models.TaskMappingStatusCompleted
	pendingStatusPtr := &pendingStatus
	details := "merged successfully"
	detailsPtr := &details
	updated, err := store.Update(ctx, m.ID, UpdateTaskMappingParams{
		PendingUpdateStatus:  &pendingStatusPtr,
		PendingUpdateDetails: &detailsPtr,
	})
	if err != nil {
		t.Fatalf("set pending: %v", err)
	}
	if updated.PendingUpdateStatus == nil || *updated.PendingUpdateStatus != models.TaskMappingStatusCompleted {
		t.Errorf("pending_update_status = %v, want Completed", updated.PendingUpdateStatus)
	}
	if updated.PendingUpdateDetails == nil || *updated.PendingUpdateDetails != "merged successfully" {
		t.Errorf("pending_update_details = %v, want %q", updated.PendingUpdateDetails, "merged successfully")
	}

	// Clear pending fields by setting to nil (double pointer: *nil = set NULL).
	var nilStatus *models.TaskMappingStatus
	var nilDetails *string
	cleared, err := store.Update(ctx, m.ID, UpdateTaskMappingParams{
		PendingUpdateStatus:  &nilStatus,
		PendingUpdateDetails: &nilDetails,
	})
	if err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	if cleared.PendingUpdateStatus != nil {
		t.Errorf("pending_update_status should be nil after clear, got %v", *cleared.PendingUpdateStatus)
	}
	if cleared.PendingUpdateDetails != nil {
		t.Errorf("pending_update_details should be nil after clear, got %q", *cleared.PendingUpdateDetails)
	}

	// ListPending should return nothing.
	pending, err := store.ListPending(ctx)
	if err != nil {
		t.Fatalf("list pending after clear: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending count = %d, want 0", len(pending))
	}
}
