package db

import (
	"context"
	"slices"
	"testing"
)

// TestListTmuxSessionNames covers the whole-database read the orphaned-tmux
// reaper (BOS-846) needs. Every other SessionStore list is repo-scoped, so
// there was no way to ask "which pane names does any session row claim".
func TestListTmuxSessionNames(t *testing.T) {
	database := setupTestDB(t)
	repoStore := NewRepoStore(database)
	sessionStore := NewSessionStore(database)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	// A row that never had a pane name: stays NULL.
	_ = createTestSession(t, sessionStore, repo.ID)

	named := createTestSession(t, sessionStore, repo.ID)
	name := "boss-abcdef12-34567890"
	namePtr := &name
	if _, err := sessionStore.Update(ctx, named.ID, UpdateSessionParams{TmuxSessionName: &namePtr}); err != nil {
		t.Fatalf("set tmux session name: %v", err)
	}

	// An empty-string name is as meaningless as NULL and must not enter the
	// whitelist — an empty entry there would match nothing but still costs a
	// map slot, and a caller treating "" as a real name could whitelist a
	// malformed pane.
	blanked := createTestSession(t, sessionStore, repo.ID)
	blank := ""
	blankPtr := &blank
	if _, err := sessionStore.Update(ctx, blanked.ID, UpdateSessionParams{TmuxSessionName: &blankPtr}); err != nil {
		t.Fatalf("set empty tmux session name: %v", err)
	}

	got, err := sessionStore.ListTmuxSessionNames(ctx)
	if err != nil {
		t.Fatalf("ListTmuxSessionNames() error = %v", err)
	}
	if len(got) != 1 || got[0] != name {
		t.Fatalf("ListTmuxSessionNames() = %q, want exactly [%q]", got, name)
	}
}

// TestListTmuxSessionNames_SpansRepos proves the read is global. A repo-scoped
// answer would leave every other repo's live panes outside the whitelist and
// therefore reapable.
func TestListTmuxSessionNames_SpansRepos(t *testing.T) {
	database := setupTestDB(t)
	repoStore := NewRepoStore(database)
	sessionStore := NewSessionStore(database)
	ctx := context.Background()

	repoA := createTestRepo(t, repoStore)
	repoB, err := repoStore.Create(ctx, CreateRepoParams{
		DisplayName:       "second-repo",
		LocalPath:         "/tmp/second-repo",
		OriginURL:         "https://github.com/test/second.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create second repo: %v", err)
	}

	for i, repoID := range []string{repoA.ID, repoB.ID} {
		sess := createTestSession(t, sessionStore, repoID)
		name := []string{"boss-aaaaaaaa-11111111", "boss-bbbbbbbb-22222222"}[i]
		namePtr := &name
		if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{TmuxSessionName: &namePtr}); err != nil {
			t.Fatalf("set tmux session name: %v", err)
		}
	}

	got, err := sessionStore.ListTmuxSessionNames(ctx)
	if err != nil {
		t.Fatalf("ListTmuxSessionNames() error = %v", err)
	}
	slices.Sort(got)
	want := []string{"boss-aaaaaaaa-11111111", "boss-bbbbbbbb-22222222"}
	if !slices.Equal(got, want) {
		t.Fatalf("ListTmuxSessionNames() = %q, want %q", got, want)
	}
}

// TestListTmuxSessionNames_IncludesArchived pins that archiving a session does
// not drop its pane name from the whitelist. BOS-428 routes `--detach` runs
// through durable tmux precisely so they outlive the daemon; an
// archived-but-still-running unattended run is the most likely false positive
// this read exists to prevent.
func TestListTmuxSessionNames_IncludesArchived(t *testing.T) {
	database := setupTestDB(t)
	repoStore := NewRepoStore(database)
	sessionStore := NewSessionStore(database)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess := createTestSession(t, sessionStore, repo.ID)
	name := "boss-cccccccc-33333333"
	namePtr := &name
	if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{TmuxSessionName: &namePtr}); err != nil {
		t.Fatalf("set tmux session name: %v", err)
	}
	if err := sessionStore.Archive(ctx, sess.ID); err != nil {
		t.Fatalf("archive session: %v", err)
	}

	got, err := sessionStore.ListTmuxSessionNames(ctx)
	if err != nil {
		t.Fatalf("ListTmuxSessionNames() error = %v", err)
	}
	if len(got) != 1 || got[0] != name {
		t.Fatalf("ListTmuxSessionNames() = %q, want the archived session's name %q", got, name)
	}
}
