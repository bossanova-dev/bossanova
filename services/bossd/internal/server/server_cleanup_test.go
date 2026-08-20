package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
)

func TestCleanupFailedCreateSessionDoesNotTrashBranchWithoutWorktree(t *testing.T) {
	ctx := context.Background()
	sessions := &cleanupSessionStore{
		sessions: map[string]*models.Session{
			"sess-1": {
				ID:         "sess-1",
				RepoID:     "repo-1",
				BranchName: "user-branch",
			},
		},
	}
	worktrees := &cleanupWorktreeManager{}
	s := &Server{
		repos: &cleanupRepoStore{
			repos: map[string]*models.Repo{
				"repo-1": {ID: "repo-1", LocalPath: "/repo"},
			},
		},
		sessions:  sessions,
		worktrees: worktrees,
	}

	s.cleanupFailedCreateSession(ctx, "sess-1")

	if worktrees.reapLocalBranchesCalls != 0 {
		t.Fatalf("ReapLocalBranches calls = %d, want 0", worktrees.reapLocalBranchesCalls)
	}
	// The leftover worktree directory must still be purged (without deleting the
	// branch) so a stale dir can't wedge the branch on the next attempt.
	if worktrees.purgeCalls != 1 {
		t.Fatalf("PurgeWorktree calls = %d, want 1", worktrees.purgeCalls)
	}
	if got, want := worktrees.purgeBranch, "user-branch"; got != want {
		t.Fatalf("PurgeWorktree branch = %q, want %q", got, want)
	}
	if !sessions.deleted["sess-1"] {
		t.Fatal("session was not deleted")
	}
}

func TestCleanupFailedCreateSessionTrashesBranchWithWorktree(t *testing.T) {
	ctx := context.Background()
	sessions := &cleanupSessionStore{
		sessions: map[string]*models.Session{
			"sess-1": {
				ID:           "sess-1",
				RepoID:       "repo-1",
				BranchName:   "owned-branch",
				WorktreePath: "/worktrees/repo/owned-branch",
			},
		},
	}
	worktrees := &cleanupWorktreeManager{}
	s := &Server{
		repos: &cleanupRepoStore{
			repos: map[string]*models.Repo{
				"repo-1": {ID: "repo-1", LocalPath: "/repo"},
			},
		},
		sessions:  sessions,
		worktrees: worktrees,
	}

	s.cleanupFailedCreateSession(ctx, "sess-1")

	if worktrees.reapLocalBranchesCalls != 1 {
		t.Fatalf("ReapLocalBranches calls = %d, want 1", worktrees.reapLocalBranchesCalls)
	}
	if got, want := worktrees.repoPath, "/repo"; got != want {
		t.Fatalf("ReapLocalBranches repo path = %q, want %q", got, want)
	}
	if got, want := worktrees.branches[0], "owned-branch"; got != want {
		t.Fatalf("ReapLocalBranches branch = %q, want %q", got, want)
	}
	// The directory is also purged on the worktree-created path.
	if worktrees.purgeCalls != 1 {
		t.Fatalf("PurgeWorktree calls = %d, want 1", worktrees.purgeCalls)
	}
	if !sessions.deleted["sess-1"] {
		t.Fatal("session was not deleted")
	}
}

func TestEmptyTrashDispatchesReapLocalBranchesOncePerRepo(t *testing.T) {
	ctx := context.Background()
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{
		"one": {ID: "one", RepoID: "repo-1", BranchName: "shared", ArchivedAt: cleanupPtr(time.Now())},
		"two": {ID: "two", RepoID: "repo-1", BranchName: "shared", ArchivedAt: cleanupPtr(time.Now())},
	}}
	worktrees := &cleanupWorktreeManager{}
	s := &Server{
		repos: &cleanupRepoStore{repos: map[string]*models.Repo{
			"repo-1": {ID: "repo-1", LocalPath: "/repo"},
		}},
		sessions:  sessions,
		worktrees: worktrees,
	}

	resp, err := s.EmptyTrash(ctx, connect.NewRequest(&pb.EmptyTrashRequest{}))
	if err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}
	if got, want := resp.Msg.DeletedCount, int32(2); got != want {
		t.Fatalf("DeletedCount = %d, want %d", got, want)
	}
	if got, want := worktrees.reapLocalBranchesCalls, 1; got != want {
		t.Fatalf("ReapLocalBranches calls = %d, want %d", got, want)
	}
	if got, want := worktrees.branches, []string{"shared"}; !sameStrings(got, want) {
		t.Fatalf("ReapLocalBranches branches = %q, want %q", got, want)
	}
}

func TestRemoveSessionDispatchesReapLocalBranches(t *testing.T) {
	ctx := context.Background()
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: "repo-1", BranchName: "session-branch"},
	}}
	worktrees := &cleanupWorktreeManager{}
	s := &Server{
		repos: &cleanupRepoStore{repos: map[string]*models.Repo{
			"repo-1": {ID: "repo-1", LocalPath: "/repo"},
		}},
		sessions:  sessions,
		worktrees: worktrees,
	}

	if _, err := s.RemoveSession(ctx, connect.NewRequest(&pb.RemoveSessionRequest{Id: "sess-1"})); err != nil {
		t.Fatalf("RemoveSession: %v", err)
	}
	if got, want := worktrees.reapLocalBranchesCalls, 1; got != want {
		t.Fatalf("ReapLocalBranches calls = %d, want %d", got, want)
	}
	if got, want := worktrees.branches, []string{"session-branch"}; !sameStrings(got, want) {
		t.Fatalf("ReapLocalBranches branches = %q, want %q", got, want)
	}
}

func cleanupPtr[T any](value T) *T { return &value }

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

type cleanupRepoStore struct {
	repos map[string]*models.Repo
}

func (s *cleanupRepoStore) Create(context.Context, db.CreateRepoParams) (*models.Repo, error) {
	panic("not used")
}
func (s *cleanupRepoStore) Get(_ context.Context, id string) (*models.Repo, error) {
	return s.repos[id], nil
}
func (s *cleanupRepoStore) GetByPath(context.Context, string) (*models.Repo, error) {
	panic("not used")
}
func (s *cleanupRepoStore) GetByOrigin(context.Context, string) (*models.Repo, error) {
	panic("not used")
}
func (s *cleanupRepoStore) List(context.Context) ([]*models.Repo, error) {
	panic("not used")
}
func (s *cleanupRepoStore) Update(context.Context, string, db.UpdateRepoParams) (*models.Repo, error) {
	panic("not used")
}
func (s *cleanupRepoStore) Delete(context.Context, string) error {
	panic("not used")
}

type cleanupSessionStore struct {
	sessions map[string]*models.Session
	deleted  map[string]bool
}

func (s *cleanupSessionStore) Create(context.Context, db.CreateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (s *cleanupSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	return s.sessions[id], nil
}
func (s *cleanupSessionStore) List(context.Context, string) ([]*models.Session, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListByState(context.Context, int) ([]*models.Session, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListByStates(context.Context, []int) ([]*models.Session, error) {
	panic("not used")
}
func (s *cleanupSessionStore) UpdateStateConditionalFrom(context.Context, string, int, []int) (bool, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListActive(context.Context, string) ([]*models.Session, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListActiveWithRepo(context.Context, string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListWithRepo(context.Context, string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListByRepoAndPR(context.Context, string, int) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListArchived(context.Context, string) ([]*models.Session, error) {
	archived := make([]*models.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if sess.ArchivedAt != nil {
			archived = append(archived, sess)
		}
	}
	return archived, nil
}

// ListTmuxSessionNames satisfies db.SessionStore (BOS-846). No test in this
// package drives the orphaned-tmux reaper, so an empty whitelist is correct.
func (s *cleanupSessionStore) ListTmuxSessionNames(_ context.Context) ([]string, error) {
	return nil, nil
}
func (s *cleanupSessionStore) Update(context.Context, string, db.UpdateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (s *cleanupSessionStore) UpdateStateConditional(context.Context, string, int, int) (bool, error) {
	panic("not used")
}
func (s *cleanupSessionStore) Archive(context.Context, string) error {
	panic("not used")
}
func (s *cleanupSessionStore) ResurrectToState(context.Context, string, int) (bool, error) {
	panic("not used")
}
func (s *cleanupSessionStore) RollbackFailedResurrect(context.Context, string, time.Time, int, int) (bool, error) {
	panic("not used")
}
func (s *cleanupSessionStore) Delete(_ context.Context, id string) error {
	if s.deleted == nil {
		s.deleted = map[string]bool{}
	}
	s.deleted[id] = true
	return nil
}
func (s *cleanupSessionStore) AdvanceOrphanedSessions(context.Context) (int64, error) {
	panic("not used")
}
func (s *cleanupSessionStore) UpdateRepairDiagnostics(context.Context, db.UpdateRepairDiagnosticsParams) error {
	panic("not used")
}

func (s *cleanupSessionStore) UpdateRepairBlocked(context.Context, string, time.Time, string) error {
	panic("not used")
}

type cleanupWorktreeManager struct {
	reapLocalBranchesCalls int
	repoPath               string
	branches               []string

	purgeCalls   int
	purgeBranch  string
	purgeRepoDir string
	// purgeErr stands in for a purge that never ran (a contended clone), the one
	// case the real manager reports back.
	purgeErr error
}

func (m *cleanupWorktreeManager) Create(context.Context, gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) CreateFromExistingBranch(context.Context, gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) Archive(context.Context, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) Resurrect(context.Context, gitpkg.ResurrectOpts) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) ReapLocalBranches(_ context.Context, repoPath string, branches []string) error {
	m.reapLocalBranchesCalls++
	m.repoPath = repoPath
	m.branches = append([]string(nil), branches...)
	return nil
}
func (m *cleanupWorktreeManager) PurgeWorktree(_ context.Context, repoPath, _, _, branch string) error {
	m.purgeCalls++
	m.purgeRepoDir = repoPath
	m.purgeBranch = branch
	return m.purgeErr
}
func (m *cleanupWorktreeManager) EmptyCommit(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) VerifyCurrentBranch(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) Push(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) PushWithLease(context.Context, string, string, string) (string, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) InjectPRNumbers(context.Context, string, string, int, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) VerifyPushedBranchAheadOfBase(context.Context, string, string, string, gitpkg.VerifyPushedBranchAheadOfBaseOpts) (*gitpkg.BranchVerification, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) Status(context.Context, string) (string, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) CommitSubjects(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (m *cleanupWorktreeManager) HasDiffAgainstBase(context.Context, string, string) (bool, error) {
	return true, nil
}

func (m *cleanupWorktreeManager) LatestCommitSubject(context.Context, string) (string, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) BranchDebugSnapshot(context.Context, string, string, string) (*gitpkg.BranchDebugSnapshot, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) Clone(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) DetectOriginURL(context.Context, string) (string, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) IsGitRepo(context.Context, string) bool {
	panic("not used")
}
func (m *cleanupWorktreeManager) DetectDefaultBranch(context.Context, string) (string, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) SyncBaseBranch(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) RetryDeferredBaseSyncs(context.Context) {
	panic("not used")
}
func (m *cleanupWorktreeManager) IsAncestor(context.Context, string, string, string) (bool, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) CountMergeCommits(context.Context, string, string, string) (int, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) DeleteLocalBranch(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) BranchSafeToDelete(context.Context, string, string, string) (bool, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) FetchBase(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) MergeLocalBranch(context.Context, string, string, string, string) error {
	panic("not used")
}

func (m *cleanupWorktreeManager) CountBehindBase(context.Context, string, string, string) (int, error) {
	panic("not used")
}

func (m *cleanupWorktreeManager) RebaseOntoBaseAndPush(context.Context, string, string, string) (*gitpkg.RebaseResult, error) {
	panic("not used")
}
