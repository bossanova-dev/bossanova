package testharness_test

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/testharness"
)

// TestE2E_SessionDeleted_RemoveSession verifies that RemoveSession invokes
// the Server's OnSessionDeleted hook so cmd/main.go can publish a
// SessionDelta_KIND_DELETED on the reverse stream — without it, bosso's
// in-memory Registry would keep showing the session in the web UI.
func TestE2E_SessionDeleted_RemoveSession(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoID,
		Title:  "Remove me",
		Plan:   "Plan for remove",
	})

	if got := h.DeletedSessionIDs(); len(got) != 0 {
		t.Fatalf("expected no deletions before RemoveSession, got %+v", got)
	}

	if _, err := h.Client.RemoveSession(ctx, connect.NewRequest(&pb.RemoveSessionRequest{Id: sess.Id})); err != nil {
		t.Fatalf("remove session: %v", err)
	}

	got := h.DeletedSessionIDs()
	if len(got) != 1 || got[0] != sess.Id {
		t.Fatalf("expected OnSessionDeleted([%q]), got %+v", sess.Id, got)
	}
}

// TestE2E_SessionDeleted_EmptyTrash verifies that EmptyTrash invokes
// OnSessionDeleted once per archived session it removes from the DB.
func TestE2E_SessionDeleted_EmptyTrash(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	mk := func(title string) string {
		sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
			RepoId: repoID,
			Title:  title,
			Plan:   "Plan for " + title,
		})
		return sess.Id
	}
	a := mk("trash-a")
	b := mk("trash-b")

	for _, id := range []string{a, b} {
		if _, err := h.Client.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: id})); err != nil {
			t.Fatalf("archive %s: %v", id, err)
		}
	}

	// EmptyTrash without OlderThan deletes everything archived.
	resp, err := h.Client.EmptyTrash(ctx, connect.NewRequest(&pb.EmptyTrashRequest{}))
	if err != nil {
		t.Fatalf("empty trash: %v", err)
	}
	if resp.Msg.DeletedCount != 2 {
		t.Fatalf("expected DeletedCount=2, got %d", resp.Msg.DeletedCount)
	}

	got := h.DeletedSessionIDs()
	if len(got) != 2 {
		t.Fatalf("expected 2 OnSessionDeleted callbacks, got %d: %+v", len(got), got)
	}
	if !slices.Contains(got, a) || !slices.Contains(got, b) {
		t.Fatalf("expected callbacks for %q and %q, got %+v", a, b, got)
	}
}

// TestE2E_SessionDeleted_CreateSessionFailure verifies that when the
// CreateSession lifecycle fails after the session row has already been
// inserted, the cleanup path invokes OnSessionDeleted for the orphaned
// row. Without this, bosso would carry a phantom "stopped" session in
// its Registry until daemon reconnect.
func TestE2E_SessionDeleted_CreateSessionFailure(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	// Force the worktree create to fail with ErrBranchExists. This
	// short-circuits StartSession before the worktree is created, so
	// CreateSession's cleanup path runs s.sessions.Delete on the
	// orphaned row — which should now also fire OnSessionDeleted.
	h.Git.CreateFunc = func(_ context.Context, _ gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
		return nil, gitpkg.ErrBranchExists
	}

	stream, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoID,
		Title:  "Will fail",
		Plan:   "Should fail",
	}))
	if err != nil {
		t.Fatalf("open create stream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup
	for stream.Receive() {
		// drain — no SessionCreated should arrive on the failure path.
	}
	if stream.Err() == nil {
		t.Fatal("expected stream error from forced branch collision")
	}

	got := h.DeletedSessionIDs()
	if len(got) != 1 {
		t.Fatalf("expected exactly one OnSessionDeleted callback, got %d: %+v", len(got), got)
	}
	// The session ID is generated server-side and never returned on the
	// failure path, so we just assert the callback fired with a non-empty
	// id — and that ListSessions agrees the row is gone.
	if got[0] == "" {
		t.Fatal("expected non-empty session id in callback")
	}
	listResp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{RepoId: &repoID}))
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(listResp.Msg.Sessions) != 0 {
		t.Fatalf("expected no sessions after failure cleanup, got %d", len(listResp.Msg.Sessions))
	}
}
