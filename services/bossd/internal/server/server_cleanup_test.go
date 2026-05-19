package server

import (
	"context"
	"testing"

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

	if worktrees.emptyTrashCalls != 0 {
		t.Fatalf("EmptyTrash calls = %d, want 0", worktrees.emptyTrashCalls)
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

	if worktrees.emptyTrashCalls != 1 {
		t.Fatalf("EmptyTrash calls = %d, want 1", worktrees.emptyTrashCalls)
	}
	if got, want := worktrees.repoPath, "/repo"; got != want {
		t.Fatalf("EmptyTrash repo path = %q, want %q", got, want)
	}
	if got, want := worktrees.branches[0], "owned-branch"; got != want {
		t.Fatalf("EmptyTrash branch = %q, want %q", got, want)
	}
	if !sessions.deleted["sess-1"] {
		t.Fatal("session was not deleted")
	}
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
func (s *cleanupSessionStore) ListActive(context.Context, string) ([]*models.Session, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListActiveWithRepo(context.Context, string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListWithRepo(context.Context, string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (s *cleanupSessionStore) ListArchived(context.Context, string) ([]*models.Session, error) {
	panic("not used")
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
func (s *cleanupSessionStore) Resurrect(context.Context, string) error {
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

type cleanupWorktreeManager struct {
	emptyTrashCalls int
	repoPath        string
	branches        []string
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
func (m *cleanupWorktreeManager) EmptyTrash(_ context.Context, repoPath string, branches []string) error {
	m.emptyTrashCalls++
	m.repoPath = repoPath
	m.branches = append([]string(nil), branches...)
	return nil
}
func (m *cleanupWorktreeManager) EmptyCommit(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) Push(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) Status(context.Context, string) (string, error) {
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
func (m *cleanupWorktreeManager) EnsureBaseBranchReadyForSync(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) SyncBaseBranch(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) IsAncestor(context.Context, string, string, string) (bool, error) {
	panic("not used")
}
func (m *cleanupWorktreeManager) FetchBase(context.Context, string, string) error {
	panic("not used")
}
func (m *cleanupWorktreeManager) MergeLocalBranch(context.Context, string, string, string, string) error {
	panic("not used")
}
