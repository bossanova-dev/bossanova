package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/session"
)

// removeWorktreeServer wires RemoveSession/RemoveRepo with a real Lifecycle so
// the archive runs through Lifecycle.ArchiveSession and the given worktree
// manager. The server's own manager is a separate fake that records the
// branch reap.
func removeWorktreeServer(sessions *cleanupSessionStore, repos *cleanupRepoStore, archiver gitpkg.WorktreeManager) (*Server, *cleanupWorktreeManager, *int) {
	reaper := &cleanupWorktreeManager{}
	deletedEvents := 0
	return &Server{
		repos:            repos,
		sessions:         sessions,
		worktrees:        reaper,
		lifecycle:        session.NewLifecycle(sessions, repos, nil, nil, archiver, nil, nil, nil, zerolog.Nop()),
		onSessionDeleted: func(context.Context, string) { deletedEvents++ },
	}, reaper, &deletedEvents
}

// bazelBuiltWorktree creates <base>/<repo>/<name> with a Bazel output base the
// real manager recognises as that worktree's, and returns both paths.
func bazelBuiltWorktree(t *testing.T, base, repo, name string) (worktree, outputBase string) {
	t.Helper()
	worktree = filepath.Join(base, repo, name)
	outputBase = filepath.Join(t.TempDir(), "_bazel_tester", "0123abcd")
	execroot := filepath.Join(outputBase, "execroot", "_main")
	for _, dir := range []string{worktree, filepath.Join(execroot, "bazel-out")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	marker := filepath.Join(worktree, "MODULE.bazel")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.Symlink(marker, filepath.Join(execroot, "MODULE.bazel")); err != nil {
		t.Fatalf("link execroot marker: %v", err)
	}
	if err := os.Symlink(filepath.Join(execroot, "bazel-out"), filepath.Join(worktree, "bazel-out")); err != nil {
		t.Fatalf("link bazel-out: %v", err)
	}
	return worktree, outputBase
}

func assertAbsent(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s %s still exists (lstat err = %v)", what, path, err)
	}
}

// TestRemoveSessionArchivesLiveWorktree_Sessions_AC222801 pins BOS-2228: removing a live session
// archives its worktree through Manager.Archive (directory and Bazel output
// base both reaped) before the row is deleted, instead of deleting the row and
// leaving the directory behind with nothing naming it.
func TestRemoveSessionArchivesLiveWorktree_Sessions_AC222801(t *testing.T) {
	base := t.TempDir()
	worktree, outputBase := bazelBuiltWorktree(t, base, "repo", "feature")
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: "repo-1", BranchName: "feature", WorktreePath: worktree},
	}}
	repos := &cleanupRepoStore{repos: map[string]*models.Repo{
		"repo-1": {ID: "repo-1", LocalPath: "/repo", DisplayName: "repo", WorktreeBaseDir: base},
	}}
	s, reaper, deletedEvents := removeWorktreeServer(sessions, repos, gitpkg.NewManager(zerolog.Nop()))

	if _, err := s.RemoveSession(context.Background(), connect.NewRequest(&pb.RemoveSessionRequest{Id: "sess-1"})); err != nil {
		t.Fatalf("RemoveSession: %v", err)
	}
	assertAbsent(t, worktree, "worktree")
	assertAbsent(t, outputBase, "Bazel output base")
	if sessions.sessions["sess-1"].ArchivedAt == nil {
		t.Fatal("row was deleted without being archived first")
	}
	if !sessions.deleted["sess-1"] || sessions.deleteCalls != 1 || *deletedEvents != 1 {
		t.Fatalf("deleted=%v deleteCalls=%d deletedEvents=%d, want the row deleted once", sessions.deleted, sessions.deleteCalls, *deletedEvents)
	}
	if reaper.reapLocalBranchesCalls != 1 {
		t.Fatalf("ReapLocalBranches calls = %d, want 1 after the worktree is gone", reaper.reapLocalBranchesCalls)
	}
}

func TestRemoveSessionKeepsRowWhenArchiveFails_Sessions_AC222801(t *testing.T) {
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: "repo-1", BranchName: "feature", WorktreePath: "/wt/repo/feature"},
	}}
	repos := &cleanupRepoStore{repos: map[string]*models.Repo{"repo-1": {ID: "repo-1", LocalPath: "/repo"}}}
	archiver := &cleanupWorktreeManager{archiveErr: map[string]error{"/wt/repo/feature": errors.New("disk busy")}}
	s, reaper, deletedEvents := removeWorktreeServer(sessions, repos, archiver)

	_, err := s.RemoveSession(context.Background(), connect.NewRequest(&pb.RemoveSessionRequest{Id: "sess-1"}))
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err=%v)", got, err)
	}
	if sessions.deleteCalls != 0 || *deletedEvents != 0 || sessions.sessions["sess-1"].ArchivedAt != nil {
		t.Fatalf("deleteCalls=%d deletedEvents=%d archivedAt=%v, want the live row kept", sessions.deleteCalls, *deletedEvents, sessions.sessions["sess-1"].ArchivedAt)
	}
	if reaper.reapLocalBranchesCalls != 0 {
		t.Fatal("reaped the branch of a session whose worktree is still checked out")
	}
}

func TestRemoveSessionOfArchivedSessionSkipsArchive(t *testing.T) {
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: "repo-1", BranchName: "feature", WorktreePath: "/wt/repo/feature", ArchivedAt: cleanupPtr(time.Now())},
	}}
	repos := &cleanupRepoStore{repos: map[string]*models.Repo{"repo-1": {ID: "repo-1", LocalPath: "/repo"}}}
	archiver := &cleanupWorktreeManager{}
	s, reaper, deletedEvents := removeWorktreeServer(sessions, repos, archiver)

	if _, err := s.RemoveSession(context.Background(), connect.NewRequest(&pb.RemoveSessionRequest{Id: "sess-1"})); err != nil {
		t.Fatalf("RemoveSession: %v", err)
	}
	if len(archiver.archived) != 0 {
		t.Fatalf("archived %q again, want an archived session removed as before", archiver.archived)
	}
	if sessions.deleteCalls != 1 || *deletedEvents != 1 || reaper.reapLocalBranchesCalls != 1 {
		t.Fatalf("deleteCalls=%d deletedEvents=%d reaps=%d, want 1/1/1", sessions.deleteCalls, *deletedEvents, reaper.reapLocalBranchesCalls)
	}
}

// TestRemoveSessionReportsArchiveDeletedRowOnce covers an archive that
// hard-deletes the row itself (a zero-output session's finalize): the remove
// succeeds, fires one delete event and does not delete a second time.
func TestRemoveSessionReportsArchiveDeletedRowOnce(t *testing.T) {
	sessions := &cleanupSessionStore{
		sessions: map[string]*models.Session{
			"sess-1": {ID: "sess-1", RepoID: "repo-1", WorktreePath: "/wt/repo/feature"},
		},
		archiveDeletes: true,
	}
	repos := &cleanupRepoStore{repos: map[string]*models.Repo{"repo-1": {ID: "repo-1", LocalPath: "/repo"}}}
	s, _, deletedEvents := removeWorktreeServer(sessions, repos, &cleanupWorktreeManager{})

	if _, err := s.RemoveSession(context.Background(), connect.NewRequest(&pb.RemoveSessionRequest{Id: "sess-1"})); err != nil {
		t.Fatalf("RemoveSession: %v", err)
	}
	if sessions.deleteCalls != 1 || *deletedEvents != 1 {
		t.Fatalf("deleteCalls=%d deletedEvents=%d, want the row reported removed exactly once", sessions.deleteCalls, *deletedEvents)
	}
}

// TestRemoveRepoArchivesEveryLiveWorktree_Repos_AC222801 pins BOS-2228: deleting a repo
// deletes its session rows, so every unarchived session's worktree is archived
// first; already-archived sessions are left alone.
func TestRemoveRepoArchivesEveryLiveWorktree_Repos_AC222801(t *testing.T) {
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{
		"live-1":   {ID: "live-1", RepoID: "repo-1", WorktreePath: "/wt/repo/one"},
		"live-2":   {ID: "live-2", RepoID: "repo-1", WorktreePath: "/wt/repo/two"},
		"archived": {ID: "archived", RepoID: "repo-1", WorktreePath: "/wt/repo/old", ArchivedAt: cleanupPtr(time.Now())},
		"other":    {ID: "other", RepoID: "repo-2", WorktreePath: "/wt/other/x"},
	}}
	repos := &cleanupRepoStore{repos: map[string]*models.Repo{"repo-1": {ID: "repo-1", LocalPath: "/repo"}}}
	archiver := &cleanupWorktreeManager{}
	s, _, _ := removeWorktreeServer(sessions, repos, archiver)

	if _, err := s.RemoveRepo(context.Background(), connect.NewRequest(&pb.RemoveRepoRequest{Id: "repo-1"})); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if got, want := len(archiver.archived), 2; got != want {
		t.Fatalf("archived %q, want both live worktrees only", archiver.archived)
	}
	for _, id := range []string{"live-1", "live-2"} {
		if sessions.sessions[id].ArchivedAt == nil {
			t.Fatalf("session %s was not archived before the repo was deleted", id)
		}
	}
	if !sameStrings(repos.deleted, []string{"repo-1"}) {
		t.Fatalf("deleted repos = %q, want repo-1", repos.deleted)
	}
}

func TestRemoveRepoDeletesNothingWhenAnArchiveFails_Repos_AC222801(t *testing.T) {
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{
		"ok":     {ID: "ok", RepoID: "repo-1", WorktreePath: "/wt/repo/ok"},
		"stuck":  {ID: "stuck", RepoID: "repo-1", WorktreePath: "/wt/repo/stuck"},
		"trash1": {ID: "trash1", RepoID: "repo-1", WorktreePath: "/wt/repo/t", ArchivedAt: cleanupPtr(time.Now())},
	}}
	repos := &cleanupRepoStore{repos: map[string]*models.Repo{"repo-1": {ID: "repo-1", LocalPath: "/repo"}}}
	archiver := &cleanupWorktreeManager{archiveErr: map[string]error{"/wt/repo/stuck": errors.New("disk busy")}}
	s, _, _ := removeWorktreeServer(sessions, repos, archiver)

	_, err := s.RemoveRepo(context.Background(), connect.NewRequest(&pb.RemoveRepoRequest{Id: "repo-1"}))
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", got, err)
	}
	if len(repos.deleted) != 0 || sessions.deleteCalls != 0 {
		t.Fatalf("deleted repos=%q session deletes=%d, want nothing deleted", repos.deleted, sessions.deleteCalls)
	}
	if sessions.sessions["stuck"].ArchivedAt != nil {
		t.Fatal("the session whose archive failed was marked archived")
	}
}
