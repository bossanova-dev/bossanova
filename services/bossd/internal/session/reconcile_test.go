package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// --- reconcile-specific mock VCS provider ---

type reconcileMockProvider struct {
	openPRs   map[string][]vcs.PRSummary // keyed by originURL
	closedPRs map[string][]vcs.PRSummary
	openErr   map[string]error
	closedErr map[string]error

	listOpenCalls   []string
	listClosedCalls []string
}

func newReconcileMockProvider() *reconcileMockProvider {
	return &reconcileMockProvider{
		openPRs:   make(map[string][]vcs.PRSummary),
		closedPRs: make(map[string][]vcs.PRSummary),
		openErr:   make(map[string]error),
		closedErr: make(map[string]error),
	}
}

func (m *reconcileMockProvider) ListOpenPRs(_ context.Context, repoPath string) ([]vcs.PRSummary, error) {
	m.listOpenCalls = append(m.listOpenCalls, repoPath)
	if err := m.openErr[repoPath]; err != nil {
		return nil, err
	}
	return m.openPRs[repoPath], nil
}

func (m *reconcileMockProvider) ListClosedPRs(_ context.Context, repoPath string) ([]vcs.PRSummary, error) {
	m.listClosedCalls = append(m.listClosedCalls, repoPath)
	if err := m.closedErr[repoPath]; err != nil {
		return nil, err
	}
	return m.closedPRs[repoPath], nil
}

// Unused Provider methods — satisfy interface.
func (m *reconcileMockProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return nil, nil
}
func (m *reconcileMockProvider) GetPRStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (m *reconcileMockProvider) GetCheckResults(context.Context, string, int) ([]vcs.CheckResult, error) {
	return nil, nil
}
func (m *reconcileMockProvider) GetFailedCheckLogs(context.Context, string, string) (string, error) {
	return "", nil
}
func (m *reconcileMockProvider) MarkReadyForReview(context.Context, string, int) error { return nil }
func (m *reconcileMockProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	return nil, nil
}
func (m *reconcileMockProvider) MergePR(context.Context, string, int, string) error { return nil }
func (m *reconcileMockProvider) UpdatePRTitle(context.Context, string, int, string) error {
	return nil
}
func (m *reconcileMockProvider) GetPRMergeCommit(context.Context, string, int) (string, error) {
	return "", nil
}
func (m *reconcileMockProvider) GetAllowedMergeStrategies(context.Context, string) ([]string, error) {
	return []string{"merge", "squash", "rebase"}, nil
}

// --- reconcile-specific mock session store ---

type reconcileMockSessionStore struct {
	sessions  map[string]*models.Session
	updateErr map[string]error // session ID → error
}

func newReconcileMockSessionStore() *reconcileMockSessionStore {
	return &reconcileMockSessionStore{
		sessions:  make(map[string]*models.Session),
		updateErr: make(map[string]error),
	}
}

func (m *reconcileMockSessionStore) addSession(s *models.Session) {
	m.sessions[s.ID] = s
}

func (m *reconcileMockSessionStore) Create(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *reconcileMockSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	return s, nil
}

func (m *reconcileMockSessionStore) List(_ context.Context, _ string) ([]*models.Session, error) {
	var result []*models.Session
	for _, s := range m.sessions {
		result = append(result, s)
	}
	return result, nil
}

func (m *reconcileMockSessionStore) ListActive(_ context.Context, repoID string) ([]*models.Session, error) {
	var result []*models.Session
	for _, s := range m.sessions {
		if s.RepoID == repoID {
			result = append(result, s)
		}
	}
	return result, nil
}

func (m *reconcileMockSessionStore) ListActiveWithRepo(_ context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		if repoID == "" || s.RepoID == repoID {
			result = append(result, &db.SessionWithRepo{Session: s})
		}
	}
	return result, nil
}

func (m *reconcileMockSessionStore) ListWithRepo(_ context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		if repoID == "" || s.RepoID == repoID {
			result = append(result, &db.SessionWithRepo{Session: s})
		}
	}
	return result, nil
}

func (m *reconcileMockSessionStore) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}

func (m *reconcileMockSessionStore) Update(_ context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	if err := m.updateErr[id]; err != nil {
		return nil, err
	}
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if params.PRNumber != nil {
		s.PRNumber = *params.PRNumber
	}
	if params.PRURL != nil {
		s.PRURL = *params.PRURL
	}
	return s, nil
}

func (m *reconcileMockSessionStore) Archive(_ context.Context, _ string) error   { return nil }
func (m *reconcileMockSessionStore) Resurrect(_ context.Context, _ string) error { return nil }
func (m *reconcileMockSessionStore) Delete(_ context.Context, _ string) error    { return nil }
func (m *reconcileMockSessionStore) AdvanceOrphanedSessions(_ context.Context) (int64, error) {
	return 0, nil
}

// --- Tests ---

func TestReconcilePRAssociations_NoRepos(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
}

func TestReconcilePRAssociations_NoOrphanedSessions(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}

	// Session already has a PR number — not orphaned.
	prNum := 10
	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-branch",
		PRNumber:   &prNum,
		State:      machine.AwaitingChecks,
	})

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 updated, got %d", n)
	}
	// No API calls should have been made.
	if len(provider.listOpenCalls) != 0 {
		t.Fatalf("expected no ListOpenPRs calls, got %d", len(provider.listOpenCalls))
	}
}

func TestReconcilePRAssociations_MatchOpenPR(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}

	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-x",
		State:      machine.AwaitingChecks,
	})

	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 42, HeadBranch: "feature-x", State: vcs.PRStateOpen},
	}

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 updated, got %d", n)
	}

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != 42 {
		t.Fatalf("expected PRNumber=42, got %v", sess.PRNumber)
	}
	if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("expected PR URL, got %v", sess.PRURL)
	}
}

func TestReconcilePRAssociations_MatchClosedPR(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}

	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-y",
		State:      machine.AwaitingChecks,
	})

	provider.closedPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 99, HeadBranch: "feature-y", State: vcs.PRStateClosed},
	}

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 updated, got %d", n)
	}

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != 99 {
		t.Fatalf("expected PRNumber=99, got %v", sess.PRNumber)
	}
}

func TestReconcilePRAssociations_OpenPRTakesPrecedenceOverClosed(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}

	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-z",
		State:      machine.AwaitingChecks,
	})

	// Same branch has both a closed PR and an open PR.
	provider.closedPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 50, HeadBranch: "feature-z", State: vcs.PRStateClosed},
	}
	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 51, HeadBranch: "feature-z", State: vcs.PRStateOpen},
	}

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 updated, got %d", n)
	}

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != 51 {
		t.Fatalf("expected PRNumber=51 (open PR), got %v", sess.PRNumber)
	}
}

func TestReconcilePRAssociations_APIErrorSkipsRepo(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo1",
	}
	repos.repos["repo-2"] = &models.Repo{
		ID:        "repo-2",
		OriginURL: "https://github.com/owner/repo2",
	}

	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "branch-a",
		State:      machine.AwaitingChecks,
	})
	sessions.addSession(&models.Session{
		ID:         "sess-2",
		RepoID:     "repo-2",
		BranchName: "branch-b",
		State:      machine.AwaitingChecks,
	})

	// repo-1 fails with an API error.
	provider.openErr["https://github.com/owner/repo1"] = errors.New("API rate limit")

	// repo-2 succeeds.
	provider.openPRs["https://github.com/owner/repo2"] = []vcs.PRSummary{
		{Number: 7, HeadBranch: "branch-b", State: vcs.PRStateOpen},
	}

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 updated (repo-2 only), got %d", n)
	}
}

func TestReconcilePRAssociations_UpdateErrorContinues(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}

	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "branch-a",
		State:      machine.AwaitingChecks,
	})
	sessions.addSession(&models.Session{
		ID:         "sess-2",
		RepoID:     "repo-1",
		BranchName: "branch-b",
		State:      machine.AwaitingChecks,
	})

	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 10, HeadBranch: "branch-a", State: vcs.PRStateOpen},
		{Number: 11, HeadBranch: "branch-b", State: vcs.PRStateOpen},
	}

	// sess-1 update fails, but sess-2 should still succeed.
	sessions.updateErr["sess-1"] = errors.New("db locked")

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 updated (sess-2 only), got %d", n)
	}

	if sessions.sessions["sess-2"].PRNumber == nil || *sessions.sessions["sess-2"].PRNumber != 11 {
		t.Fatalf("expected sess-2 PRNumber=11")
	}
}

func TestReconcilePRAssociations_EmptyBranchNameSkipped(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}

	// Session with empty branch name — should not be considered orphaned.
	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "",
		State:      machine.CreatingWorktree,
	})

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
	if len(provider.listOpenCalls) != 0 {
		t.Fatalf("expected no API calls, got %d", len(provider.listOpenCalls))
	}
}

func TestConstructPRURL(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		prNumber  int
		want      string
	}{
		{"https", "https://github.com/owner/repo", 42, "https://github.com/owner/repo/pull/42"},
		{"https with .git", "https://github.com/owner/repo.git", 42, "https://github.com/owner/repo/pull/42"},
		{"ssh", "git@github.com:owner/repo.git", 42, "https://github.com/owner/repo/pull/42"},
		{"invalid", "invalid", 42, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := constructPRURL(tt.originURL, tt.prNumber)
			if got != tt.want {
				t.Errorf("constructPRURL(%q, %d) = %q, want %q", tt.originURL, tt.prNumber, got, tt.want)
			}
		})
	}
}
