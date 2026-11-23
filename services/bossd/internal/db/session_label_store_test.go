package db

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

func TestSQLiteSessionLabelStore_CRUDAndAssignments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := setupTestDB(t)
	labels := NewSessionLabelStore(database)
	repos := NewRepoStore(database)
	sessions := NewSessionStore(database)

	repo, err := repos.Create(ctx, domain.CreateRepoParams{
		OriginURL:   "https://github.com/acme/app.git",
		LocalPath:   "/tmp/acme-app",
		DisplayName: "acme/app",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, domain.CreateSessionParams{
		RepoID:     repo.ID,
		Title:      "Fix login bug",
		BranchName: "fix-login",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Create with preset color name and with hex color.
	bug, err := labels.Create(ctx, domain.CreateSessionLabelParams{
		Name:        "bug",
		Color:       "red",
		Description: "Something is broken",
	})
	if err != nil {
		t.Fatalf("create bug label: %v", err)
	}
	if bug.Color != "#eb5757" {
		t.Fatalf("bug.Color = %q, want #eb5757", bug.Color)
	}
	urgent, err := labels.Create(ctx, domain.CreateSessionLabelParams{
		Name:  "Urgent",
		Color: "#F2994A",
	})
	if err != nil {
		t.Fatalf("create urgent label: %v", err)
	}
	if urgent.Color != "#f2994a" {
		t.Fatalf("urgent.Color = %q, want #f2994a", urgent.Color)
	}

	// Duplicate name (case-insensitive) fails with ErrLabelAlreadyExists.
	if _, err := labels.Create(ctx, domain.CreateSessionLabelParams{Name: "BUG"}); !errors.Is(err, ErrLabelAlreadyExists) {
		t.Fatalf("duplicate create err = %v, want ErrLabelAlreadyExists", err)
	}

	// Assign both labels to session.
	assigned, err := labels.SetForSession(ctx, sess.ID, []string{urgent.ID, bug.ID})
	if err != nil {
		t.Fatalf("SetForSession: %v", err)
	}
	if len(assigned) != 2 || assigned[0].Name != "bug" || assigned[1].Name != "Urgent" {
		t.Fatalf("assigned = %+v, want [bug, Urgent]", assigned)
	}

	// List returns session counts.
	all, err := labels.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 || all[0].SessionCount != 1 || all[1].SessionCount != 1 {
		t.Fatalf("List counts = %+v, want 1 each", all)
	}

	// Batch lookup returns labels keyed by session ID.
	bySession, err := labels.ListForSessions(ctx, []string{sess.ID})
	if err != nil {
		t.Fatalf("ListForSessions: %v", err)
	}
	if len(bySession[sess.ID]) != 2 {
		t.Fatalf("ListForSessions count = %d, want 2", len(bySession[sess.ID]))
	}

	// Update label name and color.
	newName := "defect"
	newColor := "purple"
	updated, err := labels.Update(ctx, bug.ID, domain.UpdateSessionLabelParams{
		Name:  &newName,
		Color: &newColor,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "defect" || updated.Color != "#bb87fc" {
		t.Fatalf("updated = %+v", updated)
	}

	// Delete returns affected session IDs and cascades assignments.
	affected, err := labels.Delete(ctx, bug.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(affected) != 1 || affected[0] != sess.ID {
		t.Fatalf("affected = %v, want [%s]", affected, sess.ID)
	}
	remaining, err := labels.ListForSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("ListForSession after delete: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != urgent.ID {
		t.Fatalf("remaining = %+v, want [Urgent]", remaining)
	}

	// Default color works when empty.
	plain, err := labels.Create(ctx, domain.CreateSessionLabelParams{Name: "triage"})
	if err != nil {
		t.Fatalf("create plain: %v", err)
	}
	if plain.Color != models.DefaultLabelColor {
		t.Fatalf("plain.Color = %q, want %q", plain.Color, models.DefaultLabelColor)
	}
}
