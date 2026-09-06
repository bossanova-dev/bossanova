package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// --- reconcile-specific mock VCS provider ---

type reconcileMockProvider struct {
	mu sync.Mutex

	openPRs   map[string][]vcs.PRSummary // keyed by originURL
	closedPRs map[string][]vcs.PRSummary
	openErr   map[string]error
	closedErr map[string]error

	prStatus    map[int]*vcs.PRStatus // keyed by PR number
	prStatusErr map[int]error

	listOpenCalls   []string
	listClosedCalls []string
	openDelay       time.Duration
	openInFlight    int
	maxOpenInFlight int
	// openHook, when set, runs inside ListOpenPRs after the call is recorded
	// and with the mutex released. It lets a test hold a listing open until
	// every concurrent caller has provably reached the provider.
	openHook func()
}

func TestNeedsPRAssociation(t *testing.T) {
	now := time.Now()
	prNumber := 123
	base := func() *models.Session {
		return &models.Session{
			BranchName: "feature/ready",
			State:      machine.ImplementingPlan,
		}
	}

	tests := []struct {
		name string
		sess *models.Session
		want bool
	}{
		{name: "nil", sess: nil, want: false},
		{
			name: "archived",
			sess: func() *models.Session {
				sess := base()
				sess.ArchivedAt = &now
				return sess
			}(),
			want: false,
		},
		{
			name: "PR backed",
			sess: func() *models.Session {
				sess := base()
				sess.PRNumber = &prNumber
				return sess
			}(),
			want: false,
		},
		{
			name: "blank branch",
			sess: func() *models.Session {
				sess := base()
				sess.BranchName = ""
				return sess
			}(),
			want: false,
		},
		{
			name: "creating worktree",
			sess: func() *models.Session {
				sess := base()
				sess.State = machine.CreatingWorktree
				return sess
			}(),
			want: false,
		},
		{
			name: "starting agent",
			sess: func() *models.Session {
				sess := base()
				sess.State = machine.StartingAgent
				return sess
			}(),
			want: false,
		},
		{name: "ready", sess: base(), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeedsPRAssociation(tt.sess); got != tt.want {
				t.Fatalf("NeedsPRAssociation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func newReconcileMockProvider() *reconcileMockProvider {
	return &reconcileMockProvider{
		openPRs:     make(map[string][]vcs.PRSummary),
		closedPRs:   make(map[string][]vcs.PRSummary),
		openErr:     make(map[string]error),
		closedErr:   make(map[string]error),
		prStatus:    make(map[int]*vcs.PRStatus),
		prStatusErr: make(map[int]error),
	}
}

func (m *reconcileMockProvider) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	m.mu.Lock()
	m.listOpenCalls = append(m.listOpenCalls, repoPath)
	m.openInFlight++
	if m.openInFlight > m.maxOpenInFlight {
		m.maxOpenInFlight = m.openInFlight
	}
	delay := m.openDelay
	hook := m.openHook
	err := m.openErr[repoPath]
	prs := m.openPRs[repoPath]
	m.mu.Unlock()

	if hook != nil {
		hook()
	}

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			m.mu.Lock()
			m.openInFlight--
			m.mu.Unlock()
			return nil, ctx.Err()
		}
	}

	m.mu.Lock()
	m.openInFlight--
	m.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return prs, nil
}

func (m *reconcileMockProvider) ListClosedPRs(_ context.Context, repoPath string) ([]vcs.PRSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listClosedCalls = append(m.listClosedCalls, repoPath)
	if err := m.closedErr[repoPath]; err != nil {
		return nil, err
	}
	return m.closedPRs[repoPath], nil
}

// Unused Provider methods — satisfy interface.
func (m *reconcileMockProvider) SearchPRsByTitleTag(context.Context, string, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (m *reconcileMockProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return nil, nil
}
func (m *reconcileMockProvider) GetPRStatus(_ context.Context, _ string, prID int) (*vcs.PRStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.prStatusErr[prID]; err != nil {
		return nil, err
	}
	return m.prStatus[prID], nil
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

// --- reconcile-specific mock branch resolver ---

type reconcileMockBranchResolver struct {
	branches map[string]string // worktreePath -> live branch
	errs     map[string]error  // worktreePath -> error
	calls    []string
}

func newReconcileMockBranchResolver() *reconcileMockBranchResolver {
	return &reconcileMockBranchResolver{
		branches: make(map[string]string),
		errs:     make(map[string]error),
	}
}

func (m *reconcileMockBranchResolver) CurrentBranch(_ context.Context, worktreePath string) (string, error) {
	m.calls = append(m.calls, worktreePath)
	if err := m.errs[worktreePath]; err != nil {
		return "", err
	}
	return m.branches[worktreePath], nil
}

// --- reconcile-specific mock session store ---

type reconcileMockSessionStore struct {
	sessions  map[string]*models.Session
	updateErr map[string]error // session ID → error
	// updateParams records every Update call in order, keyed by session ID, so
	// tests can assert that fields land in the *same* store write rather than
	// in a follow-up round-trip.
	updateParams map[string][]db.UpdateSessionParams
}

func newReconcileMockSessionStore() *reconcileMockSessionStore {
	return &reconcileMockSessionStore{
		sessions:     make(map[string]*models.Session),
		updateErr:    make(map[string]error),
		updateParams: make(map[string][]db.UpdateSessionParams),
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
		if s.ArchivedAt == nil && (repoID == "" || s.RepoID == repoID) {
			result = append(result, s)
		}
	}
	return result, nil
}

func (m *reconcileMockSessionStore) ListActiveWithRepo(_ context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		if s.ArchivedAt == nil && (repoID == "" || s.RepoID == repoID) {
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

func (m *reconcileMockSessionStore) ListByRepoAndPR(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		if s.RepoID == repoID && s.PRNumber != nil && *s.PRNumber == prNumber && s.ArchivedAt == nil {
			result = append(result, &db.SessionWithRepo{Session: s})
		}
	}
	return result, nil
}

func (m *reconcileMockSessionStore) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	var result []*models.Session
	for _, s := range m.sessions {
		if s.ArchivedAt != nil {
			result = append(result, s)
		}
	}
	return result, nil
}

// ListTmuxSessionNames satisfies db.SessionStore (BOS-846). No test in this
// package drives the orphaned-tmux reaper, so an empty whitelist is correct.
func (m *reconcileMockSessionStore) ListTmuxSessionNames(_ context.Context) ([]string, error) {
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
	m.updateParams[id] = append(m.updateParams[id], params)
	if params.State != nil {
		s.State = machine.State(*params.State)
	}
	if params.PRNumber != nil {
		s.PRNumber = *params.PRNumber
	}
	if params.PRURL != nil {
		s.PRURL = *params.PRURL
	}
	if params.BranchName != nil {
		s.BranchName = *params.BranchName
	}
	if params.Title != nil {
		s.Title = *params.Title
	}
	if params.BlockedReason != nil {
		s.BlockedReason = *params.BlockedReason
	}
	return s, nil
}

func (m *reconcileMockSessionStore) Archive(_ context.Context, _ string) error { return nil }
func (m *reconcileMockSessionStore) ResurrectToState(_ context.Context, _ string, _ int) (bool, error) {
	return false, nil
}
func (m *reconcileMockSessionStore) RollbackFailedResurrect(_ context.Context, _ string, _ time.Time, _, _ int) (bool, error) {
	return false, nil
}
func (m *reconcileMockSessionStore) Delete(_ context.Context, _ string) error { return nil }
func (m *reconcileMockSessionStore) AdvanceOrphanedSessions(_ context.Context) (int64, error) {
	return 0, nil
}
func (m *reconcileMockSessionStore) UpdateStateConditional(_ context.Context, _ string, _, _ int) (bool, error) {
	return false, nil
}
func (m *reconcileMockSessionStore) ListByState(_ context.Context, _ int) ([]*models.Session, error) {
	return nil, nil
}
func (m *reconcileMockSessionStore) ListByStates(_ context.Context, _ []int) ([]*models.Session, error) {
	return nil, nil
}
func (m *reconcileMockSessionStore) UpdateStateConditionalFrom(_ context.Context, _ string, _ int, _ []int) (bool, error) {
	return false, nil
}
func (m *reconcileMockSessionStore) UpdateRepairDiagnostics(_ context.Context, _ db.UpdateRepairDiagnosticsParams) error {
	return nil
}

func (m *reconcileMockSessionStore) UpdateRepairBlocked(_ context.Context, _ string, _ time.Time, _ string) error {
	return nil
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

func TestPRAssociationResolver_ReconcileSessionsScopesToSuppliedSessions(t *testing.T) {
	ctx := context.Background()
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo-one",
	}
	repos.repos["repo-2"] = &models.Repo{
		ID:        "repo-2",
		OriginURL: "https://github.com/owner/repo-two",
	}

	visible := &models.Session{
		ID:         "sess-visible",
		RepoID:     "repo-1",
		BranchName: "feature-visible",
		State:      machine.AwaitingChecks,
	}
	unlisted := &models.Session{
		ID:         "sess-unlisted",
		RepoID:     "repo-2",
		BranchName: "feature-unlisted",
		State:      machine.AwaitingChecks,
	}
	sessions.addSession(visible)
	sessions.addSession(unlisted)

	provider.openPRs["https://github.com/owner/repo-one"] = []vcs.PRSummary{{
		Number:     11,
		HeadBranch: "feature-visible",
		State:      vcs.PRStateOpen,
	}}
	provider.openPRs["https://github.com/owner/repo-two"] = []vcs.PRSummary{{
		Number:     22,
		HeadBranch: "feature-unlisted",
		State:      vcs.PRStateOpen,
	}}

	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	updated, err := resolver.ReconcileSessions(ctx, []*models.Session{visible})
	if err != nil {
		t.Fatalf("ReconcileSessions: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}
	if visible.PRNumber == nil || *visible.PRNumber != 11 {
		t.Fatalf("visible PRNumber = %v, want 11", visible.PRNumber)
	}
	if unlisted.PRNumber != nil {
		t.Fatalf("unlisted PRNumber = %v, want nil", *unlisted.PRNumber)
	}
	if got, want := strings.Join(provider.listOpenCalls, ","), "https://github.com/owner/repo-one"; got != want {
		t.Fatalf("ListOpenPRs calls = %q, want %q", got, want)
	}
}

func TestPRAssociationResolver_ReconcileSessionsSkipsArchived(t *testing.T) {
	ctx := context.Background()
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}

	archivedAt := time.Now()
	archived := &models.Session{
		ID:         "sess-archived",
		RepoID:     "repo-1",
		BranchName: "feature-archived",
		State:      machine.AwaitingChecks,
		ArchivedAt: &archivedAt,
	}
	sessions.addSession(archived)
	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{{
		Number:     33,
		HeadBranch: "feature-archived",
		State:      vcs.PRStateOpen,
	}}

	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	updated, err := resolver.ReconcileSessions(ctx, []*models.Session{archived})
	if err != nil {
		t.Fatalf("ReconcileSessions: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated = %d, want 0", updated)
	}
	if archived.PRNumber != nil {
		t.Fatalf("archived PRNumber = %v, want nil", *archived.PRNumber)
	}
	if len(provider.listOpenCalls) != 0 {
		t.Fatalf("ListOpenPRs calls = %v, want none", provider.listOpenCalls)
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

// TestReconcilePRAssociations_AdvancesWedgedStateOnAdopt pins the BOS-697
// un-wedge: when reconcile adopts a PR the agent opened itself, a session still
// sitting in PushingBranch / OpeningDraftPR is advanced to AwaitingChecks in the
// same store write that sets PRNumber. Without it the row keeps a state that
// permits no PR lifecycle event, so it silently drops every check, review and
// merge event that follows. ImplementingPlan is deliberately left alone — the
// agent is still working and PlanComplete routes it via HasPR on its own.
func TestReconcilePRAssociations_AdvancesWedgedStateOnAdopt(t *testing.T) {
	cases := []struct {
		name      string
		initial   machine.State
		wantState machine.State
		wantWrite bool
	}{
		{"pushing_branch", machine.PushingBranch, machine.AwaitingChecks, true},
		{"opening_draft_pr", machine.OpeningDraftPR, machine.AwaitingChecks, true},
		{"implementing_plan", machine.ImplementingPlan, machine.ImplementingPlan, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
				State:      tc.initial,
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

			got := sessions.sessions["sess-1"]
			if got.State != tc.wantState {
				t.Fatalf("state = %s, want %s", got.State, tc.wantState)
			}

			writes := sessions.updateParams["sess-1"]
			if len(writes) != 1 {
				t.Fatalf("store writes = %d, want exactly 1 (state must ride the PR-association write)", len(writes))
			}
			if writes[0].PRNumber == nil {
				t.Fatal("the single write must carry PRNumber")
			}
			if tc.wantWrite && writes[0].State == nil {
				t.Fatal("the single write must also carry State")
			}
			if !tc.wantWrite && writes[0].State != nil {
				t.Fatalf("state must not be written for %s, got %d", tc.initial, *writes[0].State)
			}
		})
	}
}

// TestReconcile_ArchivesMergedButUnarchivedSessions pins the BOS-697 self-heal
// sweep. Rows that reached Merged while the archive hook was unreachable (or on
// a daemon that restarted before the poller got there) sit merged-but-unarchived
// forever: nothing revisits a terminal row. The sweep archives them on the next
// reconcile tick, gated on the repo's ShouldArchiveSessionsAfterMerge flag.
func TestReconcile_ArchivesMergedButUnarchivedSessions(t *testing.T) {
	archivedAt := time.Now()
	cases := []struct {
		name        string
		state       machine.State
		archiveFlag bool
		archived    bool
		wantArchive bool
	}{
		{"merged on flag-on repo", machine.Merged, true, false, true},
		{"merged on flag-off repo", machine.Merged, false, false, false},
		{"already archived", machine.Merged, true, true, false},
		{"not merged", machine.AwaitingChecks, true, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newReconcileMockSessionStore()
			repos := newMockRepoStore()
			provider := newReconcileMockProvider()

			repos.repos["repo-1"] = &models.Repo{
				ID:                              "repo-1",
				OriginURL:                       "https://github.com/owner/repo",
				ShouldArchiveSessionsAfterMerge: tc.archiveFlag,
			}
			prNumber := 42
			sess := &models.Session{
				ID:         "sess-1",
				RepoID:     "repo-1",
				BranchName: "feature-x",
				State:      tc.state,
				PRNumber:   &prNumber,
			}
			if tc.archived {
				sess.ArchivedAt = &archivedAt
			}
			sessions.addSession(sess)

			arch := newFakeArchiver()
			resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
				WithArchiver(arch, nil)

			if _, err := resolver.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if tc.archived {
				// ListActive already filters archived rows, so reaching the
				// sweep through Reconcile would never exercise its own
				// ArchivedAt guard — the sub-case would pin the store, not the
				// guard. Hand the row straight to the sweep so deleting that
				// guard goes red here.
				resolver.archiveMergedButUnarchived(context.Background(), []*models.Session{sess})
			}

			if tc.wantArchive {
				select {
				case id := <-arch.calls:
					if id != "sess-1" {
						t.Fatalf("archived %q, want sess-1", id)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("merged-but-unarchived session was not archived by the sweep")
				}
				return
			}
			select {
			case id := <-arch.calls:
				t.Fatalf("session %q archived when it should not have been", id)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// TestReconcile_SweepSkipsCandidatesArchivedAfterTheSnapshot pins the read-side
// half of BOS-924. The snapshot the sweep is handed comes from the top of the
// tick and the archive it launches is detached and slow, so a row can be
// archived by another path in between. Archiving on the stale copy would delete
// a worktree twice and re-run the whole archive path against a row already gone.
//
// The stale copy is what the sweep sees; the store holds the row as it now is.
// Delete the re-read guard and this goes red, because the stale copy still
// matches the predicate.
func TestReconcile_SweepSkipsCandidatesArchivedAfterTheSnapshot(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:                              "repo-1",
		OriginURL:                       "https://github.com/owner/repo",
		ShouldArchiveSessionsAfterMerge: true,
	}
	prNumber := 42
	archivedAt := time.Now()
	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-x",
		State:      machine.Merged,
		PRNumber:   &prNumber,
		ArchivedAt: &archivedAt, // the row as it is NOW
	})
	stale := &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-x",
		State:      machine.Merged,
		PRNumber:   &prNumber,
		// ArchivedAt nil: the row as it looked when the snapshot was taken.
	}

	arch := newFakeArchiver()
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithArchiver(arch, nil)

	resolver.archiveMergedButUnarchived(context.Background(), []*models.Session{stale})

	select {
	case id := <-arch.calls:
		t.Fatalf("session %q archived from a stale snapshot; the row was already archived", id)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestReconcile_SweepSkipsCandidatesResurrectedAfterTheSnapshot is the case the
// guard exists for. A user resurrects an archived merged session; the sweep is
// still holding the pre-resurrect snapshot, which matches its predicate exactly.
// Dispatching on it would archive the session back out from under the agent that
// was just started for it and delete the worktree the resurrect recreated.
//
// The store holds the resurrected row (live, ImplementingPlan); the snapshot is
// the archived-then-merged copy. Nothing here sleeps or advances a clock: the
// two shapes disagree, which is the whole point.
func TestReconcile_SweepSkipsCandidatesResurrectedAfterTheSnapshot(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:                              "repo-1",
		OriginURL:                       "https://github.com/owner/repo",
		ShouldArchiveSessionsAfterMerge: true,
	}
	prNumber := 42
	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-x",
		State:      machine.ImplementingPlan, // resurrected: live, no longer Merged
		PRNumber:   &prNumber,
	})
	stale := &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-x",
		State:      machine.Merged,
		PRNumber:   &prNumber,
	}

	arch := newFakeArchiver()
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithArchiver(arch, nil)

	resolver.archiveMergedButUnarchived(context.Background(), []*models.Session{stale})

	select {
	case id := <-arch.calls:
		t.Fatalf("session %q archived from a pre-resurrect snapshot; the resurrect would be undone", id)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestReconcileSessions_DoesNotRunTheMergedSweep pins where the sweep lives.
// It hangs off Reconcile, the full scanner, NOT the shared ReconcileSessions:
// the other ReconcileSessions caller is Server.ListSessions, which passes rows
// pre-filtered by NeedsPRAssociation (PRNumber == nil), a shape a merged row can
// never have — so running it there is dead work on a hot path. Moving the call
// back onto ReconcileSessions would otherwise be a silent change.
func TestReconcileSessions_DoesNotRunTheMergedSweep(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:                              "repo-1",
		OriginURL:                       "https://github.com/owner/repo",
		ShouldArchiveSessionsAfterMerge: true,
	}
	prNumber := 42
	merged := &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-x",
		State:      machine.Merged,
		PRNumber:   &prNumber,
	}
	sessions.addSession(merged)

	arch := newFakeArchiver()
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithArchiver(arch, nil)

	if _, err := resolver.ReconcileSessions(context.Background(), []*models.Session{merged}); err != nil {
		t.Fatalf("ReconcileSessions: %v", err)
	}

	select {
	case id := <-arch.calls:
		t.Fatalf("session %q archived from ReconcileSessions; the sweep belongs on Reconcile", id)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestReconcile_MergedSweepIsNoOpWithoutArchiver confirms the sweep is a
// documented no-op — not a panic — when no archiver is wired.
func TestReconcile_MergedSweepIsNoOpWithoutArchiver(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:                              "repo-1",
		OriginURL:                       "https://github.com/owner/repo",
		ShouldArchiveSessionsAfterMerge: true,
	}
	prNumber := 42
	sessions.addSession(&models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-x",
		State:      machine.Merged,
		PRNumber:   &prNumber,
	})

	n, err := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if n != 0 {
		t.Fatalf("reconciled = %d, want 0", n)
	}
	// Reading ShouldArchiveSessionsAfterMerge requires a repo lookup, and
	// nothing else in this fixture touches the repo store (the session already
	// has a PR, so PR association skips it). Zero lookups is therefore direct
	// evidence the sweep returned before doing any work; the assertion goes red
	// if BOTH nil-archiver guards — the resolver's and the shared helper's — are
	// removed (verified by deleting them).
	if got := repos.getCalls.Load(); got != 0 {
		t.Fatalf("repo lookups = %d, want 0 (sweep must not run without an archiver)", got)
	}
}

// TestReconcilePRAssociations_NotifiesOnRename verifies the update notifier
// fires once with the renamed session when reconcile attaches a PR and renames
// the session to the PR title. Without this hook the rename never reaches the
// cloud/web (reconcile writes through the store, bypassing the UpdateSession
// RPC that would emit the event).
func TestReconcilePRAssociations_NotifiesOnRename(t *testing.T) {
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
		Title:      "Bossanova auto-implement",
		BranchName: "feature-x",
		State:      machine.AwaitingChecks,
	})

	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 42, HeadBranch: "feature-x", State: vcs.PRStateOpen, Title: "[BOS-79] Per-cron model selection"},
	}

	var notified []*models.Session
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithUpdateNotifier(func(_ context.Context, s *models.Session) {
			notified = append(notified, s)
		})

	if _, err := resolver.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notified) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(notified))
	}
	if notified[0].ID != "sess-1" {
		t.Fatalf("notified session = %q, want sess-1", notified[0].ID)
	}
	if notified[0].Title != "[BOS-79] Per-cron model selection" {
		t.Fatalf("notified title = %q, want renamed PR title", notified[0].Title)
	}
}

func TestReconcilePRAssociations_MatchesLiveWorktreeBranchWhenStoredIsStale(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()
	branches := newReconcileMockBranchResolver()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/owner/repo"}

	sessions.addSession(&models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "WonderCanvas auto-implement",
		BranchName:   "cron-wondercanvas-auto-implement-1781780400", // stale
		WorktreePath: "/wt/sess-1",
		State:        machine.AwaitingChecks,
	})

	// The agent switched the worktree to its own branch and opened PR #354 there.
	branches.branches["/wt/sess-1"] = "dave/won-1208-foo"
	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 354, Title: "[WON-1208] Fix the thing", HeadBranch: "dave/won-1208-foo", State: vcs.PRStateOpen},
	}

	n, err := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithBranchResolver(branches).
		Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated = %d, want 1", n)
	}

	got := sessions.sessions["sess-1"]
	if got.PRNumber == nil || *got.PRNumber != 354 {
		t.Fatalf("PRNumber = %v, want 354", got.PRNumber)
	}
	if got.PRURL == nil || *got.PRURL != "https://github.com/owner/repo/pull/354" {
		t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/354", got.PRURL)
	}
	if got.BranchName != "dave/won-1208-foo" {
		t.Fatalf("BranchName = %q, want corrected live branch", got.BranchName)
	}
	if got.Title != "[WON-1208] Fix the thing" {
		t.Fatalf("Title = %q, want PR title", got.Title)
	}
}

func TestReconcilePRAssociations_StoredBranchMatchSkipsLiveResolution(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()
	branches := newReconcileMockBranchResolver()
	// Force an error if the live branch is ever consulted.
	branches.errs["/wt/sess-1"] = errors.New("CurrentBranch must not be called when stored branch matches")

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/owner/repo"}
	sessions.addSession(&models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "My session",
		BranchName:   "dave/won-1208-foo",
		WorktreePath: "/wt/sess-1",
		State:        machine.AwaitingChecks,
	})
	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 354, Title: "[WON-1208] Fix the thing", HeadBranch: "dave/won-1208-foo", State: vcs.PRStateOpen},
	}

	n, err := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithBranchResolver(branches).
		Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated = %d, want 1", n)
	}
	if got := sessions.sessions["sess-1"]; got.BranchName != "dave/won-1208-foo" {
		t.Fatalf("BranchName = %q, want unchanged", got.BranchName)
	}
	if len(branches.calls) != 0 {
		t.Fatalf("CurrentBranch calls = %v, want none", branches.calls)
	}
}

func TestReconcilePRAssociations_LiveBranchUnavailableFallsBackGracefully(t *testing.T) {
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()
	branches := newReconcileMockBranchResolver()
	branches.errs["/wt/sess-1"] = errors.New("detached HEAD") // resolver fails

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/owner/repo"}
	sessions.addSession(&models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		BranchName:   "cron-stale",
		WorktreePath: "/wt/sess-1",
		State:        machine.AwaitingChecks,
	})
	// PR exists but only on a branch we can't discover (resolver errored).
	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 354, HeadBranch: "dave/won-1208-foo", State: vcs.PRStateOpen},
	}

	n, err := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithBranchResolver(branches).
		Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err) // must NOT error
	}
	if n != 0 {
		t.Fatalf("updated = %d, want 0 (graceful fallback)", n)
	}
}

func TestPRAssociationResolver_ReusesOpenPRCacheWithinTTL(t *testing.T) {
	ctx := context.Background()
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
	sessions.addSession(&models.Session{
		ID:         "sess-2",
		RepoID:     "repo-1",
		BranchName: "feature-y",
		State:      machine.AwaitingChecks,
	})

	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 42, HeadBranch: "feature-x", State: vcs.PRStateOpen},
	}

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	resolver.SetTTLForTest(time.Minute)
	resolver.SetNowForTest(func() time.Time { return now })

	updated, err := resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 updated, got %d", updated)
	}
	if len(provider.listOpenCalls) != 1 {
		t.Fatalf("expected 1 ListOpenPRs call, got %d", len(provider.listOpenCalls))
	}
	if len(provider.listClosedCalls) != 0 {
		t.Fatalf("expected no ListClosedPRs calls, got %d", len(provider.listClosedCalls))
	}

	updated, err = resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updated, got %d", updated)
	}
	if len(provider.listOpenCalls) != 1 {
		t.Fatalf("expected cached open PRs to be reused, got %d ListOpenPRs calls", len(provider.listOpenCalls))
	}
	if len(provider.listClosedCalls) != 0 {
		t.Fatalf("expected no ListClosedPRs calls, got %d", len(provider.listClosedCalls))
	}
}

func TestPRAssociationResolver_DoesNotHoldCacheLockDuringProviderCalls(t *testing.T) {
	ctx := context.Background()
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()
	provider.openDelay = 50 * time.Millisecond

	first := &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "feature-x",
		State:      machine.AwaitingChecks,
	}
	second := &models.Session{
		ID:         "sess-2",
		RepoID:     "repo-2",
		BranchName: "feature-y",
		State:      machine.AwaitingChecks,
	}
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo-1",
	}
	repos.repos["repo-2"] = &models.Repo{
		ID:        "repo-2",
		OriginURL: "https://github.com/owner/repo-2",
	}

	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	start := make(chan struct{})
	errs := make(chan error, 2)

	var wg sync.WaitGroup
	for _, sess := range []*models.Session{first, second} {
		wg.Add(1)
		go func(sess *models.Session) {
			defer wg.Done()
			<-start
			_, err := resolver.ReconcileSessions(ctx, []*models.Session{sess})
			errs <- err
		}(sess)
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("ReconcileSessions: %v", err)
		}
	}
	if provider.maxOpenInFlight < 2 {
		t.Fatalf("max concurrent ListOpenPRs calls = %d, want at least 2", provider.maxOpenInFlight)
	}
}

func TestPRAssociationResolver_RefreshesOpenPRCacheAfterTTL(t *testing.T) {
	ctx := context.Background()
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

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	resolver.SetTTLForTest(time.Minute)
	resolver.SetNowForTest(func() time.Time { return now })

	updated, err := resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updated, got %d", updated)
	}
	if len(provider.listOpenCalls) != 1 {
		t.Fatalf("expected 1 ListOpenPRs call, got %d", len(provider.listOpenCalls))
	}

	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 43, HeadBranch: "feature-y", State: vcs.PRStateOpen},
	}
	now = now.Add(time.Minute + time.Second)

	updated, err = resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 updated, got %d", updated)
	}
	if len(provider.listOpenCalls) != 2 {
		t.Fatalf("expected open PR cache refresh, got %d ListOpenPRs calls", len(provider.listOpenCalls))
	}
	if len(provider.listClosedCalls) != 0 {
		t.Fatalf("expected no ListClosedPRs calls, got %d", len(provider.listClosedCalls))
	}

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != 43 {
		t.Fatalf("expected PRNumber=43, got %v", sess.PRNumber)
	}
}

func TestPRAssociationResolver_DoesNotMatchClosedPR(t *testing.T) {
	ctx := context.Background()
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
		BranchName: "feature-closed",
		State:      machine.AwaitingChecks,
	})

	provider.closedPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 99, HeadBranch: "feature-closed", State: vcs.PRStateClosed},
	}

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	resolver.SetTTLForTest(time.Minute)
	resolver.SetNowForTest(func() time.Time { return now })

	updated, err := resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updated, got %d", updated)
	}
	if sessions.sessions["sess-1"].PRNumber != nil {
		t.Fatalf("expected PRNumber=nil for closed-only match, got %v", sessions.sessions["sess-1"].PRNumber)
	}
	if len(provider.listOpenCalls) != 1 {
		t.Fatalf("expected 1 ListOpenPRs call, got %d", len(provider.listOpenCalls))
	}
	if len(provider.listClosedCalls) != 0 {
		t.Fatalf("expected no ListClosedPRs calls, got %d", len(provider.listClosedCalls))
	}
}

func TestReconcilePRAssociationsClearsDraftPRBlockedReasonWhenPRAttached(t *testing.T) {
	ctx := context.Background()
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repo := &models.Repo{
		ID:        "repo-1",
		OriginURL: "git@github.com:owner/repo.git",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open missing PR",
		BranchName:    "open-missing-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.openPRs[repo.OriginURL] = []vcs.PRSummary{
		{
			Number:     42,
			Title:      "Open missing PR",
			HeadBranch: "open-missing-pr",
			State:      vcs.PRStateOpen,
		},
	}

	updated, err := ReconcilePRAssociations(ctx, sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("ReconcilePRAssociations returned error: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}

	got := sessions.sessions[session.ID]
	if got.PRNumber == nil || *got.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", got.PRNumber)
	}
	if got.PRURL == nil || *got.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/42", got.PRURL)
	}
	if got.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *got.BlockedReason)
	}
}

func TestReconcilePRAssociations_DoesNotMatchClosedPR(t *testing.T) {
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
	if n != 0 {
		t.Fatalf("expected 0 updated, got %d", n)
	}

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber != nil {
		t.Fatalf("expected PRNumber=nil for closed-only match, got %v", sess.PRNumber)
	}
	if len(provider.listOpenCalls) != 1 {
		t.Fatalf("expected 1 ListOpenPRs call, got %d", len(provider.listOpenCalls))
	}
	if len(provider.listClosedCalls) != 0 {
		t.Fatalf("expected no ListClosedPRs calls, got %d", len(provider.listClosedCalls))
	}
}

func TestReconcilePRAssociations_DuplicateOpenBranchesKeepFirstResult(t *testing.T) {
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

	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 50, HeadBranch: "feature-z", State: vcs.PRStateOpen},
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
	if sess.PRNumber == nil || *sess.PRNumber != 50 {
		t.Fatalf("expected PRNumber=50 (first open PR), got %v", sess.PRNumber)
	}
	if len(provider.listOpenCalls) != 1 {
		t.Fatalf("expected 1 ListOpenPRs call, got %d", len(provider.listOpenCalls))
	}
	if len(provider.listClosedCalls) != 0 {
		t.Fatalf("expected no ListClosedPRs calls, got %d", len(provider.listClosedCalls))
	}
}

func TestReconcilePRAssociations_APIErrorContinues(t *testing.T) {
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

	provider.openErr["https://github.com/owner/repo1"] = errors.New("API rate limit")
	provider.openPRs["https://github.com/owner/repo2"] = []vcs.PRSummary{
		{Number: 22, HeadBranch: "branch-b", State: vcs.PRStateOpen},
	}

	n, err := ReconcilePRAssociations(context.Background(), sessions, repos, provider, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 updated, got %d", n)
	}
	if sessions.sessions["sess-1"].PRNumber != nil {
		t.Fatalf("expected sess-1 PRNumber to remain nil, got %v", sessions.sessions["sess-1"].PRNumber)
	}
	if sessions.sessions["sess-2"].PRNumber == nil || *sessions.sessions["sess-2"].PRNumber != 22 {
		t.Fatalf("expected sess-2 PRNumber=22, got %v", sessions.sessions["sess-2"].PRNumber)
	}
	if len(provider.listOpenCalls) != 2 {
		t.Fatalf("expected 2 ListOpenPRs calls, got %d", len(provider.listOpenCalls))
	}
	if len(provider.listClosedCalls) != 0 {
		t.Fatalf("expected no ListClosedPRs calls, got %d", len(provider.listClosedCalls))
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

// TestPRsForRepo_ReusesDefaultCacheWithinTTL verifies that a second prsForRepo
// call within the default TTL window reuses the cached PR list and does not
// call ListOpenPRs a second time. The expected TTL is pinned via the package
// constant defaultPRAssociationCacheTTL so any change to that constant is
// immediately reflected here.
func TestPRsForRepo_ReusesDefaultCacheWithinTTL(t *testing.T) {
	ctx := context.Background()
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}

	sessions.addSession(&models.Session{
		ID:         "sess-a",
		RepoID:     "repo-1",
		BranchName: "feature-a",
		State:      machine.AwaitingChecks,
	})
	sessions.addSession(&models.Session{
		ID:         "sess-b",
		RepoID:     "repo-1",
		BranchName: "feature-b",
		State:      machine.AwaitingChecks,
	})

	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 77, HeadBranch: "feature-a", State: vcs.PRStateOpen},
	}

	// Use a fixed clock so the TTL boundary is deterministic. Advance time
	// to just before the TTL expires — the cache should still be warm.
	t0 := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	current := t0
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	resolver.SetNowForTest(func() time.Time { return current })

	// First reconcile populates the cache.
	updated, err := resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if updated != 1 {
		t.Fatalf("first Reconcile: updated = %d, want 1", updated)
	}
	if len(provider.listOpenCalls) != 1 {
		t.Fatalf("first Reconcile: ListOpenPRs calls = %d, want 1", len(provider.listOpenCalls))
	}

	// Advance time to just inside the TTL window.
	current = t0.Add(defaultPRAssociationCacheTTL - time.Second)

	// Second reconcile must reuse the cache — ListOpenPRs must not be called again.
	updated, err = resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("second Reconcile (within TTL): %v", err)
	}
	if updated != 0 {
		t.Fatalf("second Reconcile (within TTL): updated = %d, want 0 (already associated)", updated)
	}
	if len(provider.listOpenCalls) != 1 {
		t.Fatalf("second Reconcile (within TTL=%s): ListOpenPRs calls = %d, want 1 (cache hit)",
			defaultPRAssociationCacheTTL, len(provider.listOpenCalls))
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

// --- negative cache for failed PR listings (see defaultPRAssociationFailureTTL) ---

// newFailingListingFixture builds one repo with sessionCount PR-less sessions
// on it and a provider whose ListOpenPRs fails, which is the shape that used to
// cost one provider call per session on every pass.
func newFailingListingFixture(t *testing.T, sessionCount int) (
	*reconcileMockSessionStore, *mockRepoStore, *reconcileMockProvider,
) {
	t.Helper()

	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "https://github.com/owner/repo",
	}
	for i := 0; i < sessionCount; i++ {
		sessions.addSession(&models.Session{
			ID:         fmt.Sprintf("sess-%d", i),
			RepoID:     "repo-1",
			BranchName: fmt.Sprintf("feature-%d", i),
			State:      machine.AwaitingChecks,
		})
	}
	provider.openErr["https://github.com/owner/repo"] = errors.New("gh pr list: signal: killed")

	return sessions, repos, provider
}

// TestPRsForRepo_NegativeCacheCollapsesFailuresWithinAPass pins the fix for the
// amplification: N sessions on a repo whose listing fails must cost ONE
// provider call, not N. It also pins the log-volume half — the repeats are
// demoted to Debug so a failing repo produces one Warn, not one per session.
func TestPRsForRepo_NegativeCacheCollapsesFailuresWithinAPass(t *testing.T) {
	const sessionCount = 4

	ctx := context.Background()
	sessions, repos, provider := newFailingListingFixture(t, sessionCount)

	var logs bytes.Buffer
	logger := zerolog.New(&logs).Level(zerolog.DebugLevel)

	resolver := NewPRAssociationResolver(sessions, repos, provider, logger)

	updated, err := resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated = %d, want 0 (every listing failed)", updated)
	}

	if got := len(provider.listOpenCalls); got != 1 {
		t.Fatalf("ListOpenPRs calls = %d, want 1 for %d sessions on one failing repo", got, sessionCount)
	}

	warns, debugs := countReconcileFindPRLines(t, logs.String())
	if warns != 1 {
		t.Errorf("Warn lines = %d, want exactly 1 per repo per failure window", warns)
	}
	if debugs != sessionCount-1 {
		t.Errorf("Debug lines = %d, want %d (the cached repeats)", debugs, sessionCount-1)
	}
}

// countReconcileFindPRLines counts the "reconcile: find PR for session" lines
// in captured zerolog JSON output, split by level.
func countReconcileFindPRLines(t *testing.T, out string) (warns, debugs int) {
	t.Helper()

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		if entry.Message != "reconcile: find PR for session" {
			continue
		}
		switch entry.Level {
		case "warn":
			warns++
		case "debug":
			debugs++
		default:
			t.Fatalf("unexpected level %q on a find-PR line", entry.Level)
		}
	}
	return warns, debugs
}

// TestPRsForRepo_NegativeCacheExpiresAfterFailureTTL verifies the remembered
// failure is short-lived: a pass inside the window reuses it, and the first
// pass after it expires calls the provider again. The window is read from
// defaultPRAssociationFailureTTL so a change to that constant lands here.
func TestPRsForRepo_NegativeCacheExpiresAfterFailureTTL(t *testing.T) {
	ctx := context.Background()
	sessions, repos, provider := newFailingListingFixture(t, 2)

	t0 := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)
	current := t0
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	resolver.SetNowForTest(func() time.Time { return current })

	if _, err := resolver.Reconcile(ctx); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if got := len(provider.listOpenCalls); got != 1 {
		t.Fatalf("first pass: ListOpenPRs calls = %d, want 1", got)
	}

	// Just inside the failure window: the remembered failure is reused.
	current = t0.Add(defaultPRAssociationFailureTTL - time.Second)
	if _, err := resolver.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if got := len(provider.listOpenCalls); got != 1 {
		t.Fatalf("inside failure TTL (%s): ListOpenPRs calls = %d, want 1 (cache hit)",
			defaultPRAssociationFailureTTL, got)
	}

	// Past the window: the provider is retried.
	current = t0.Add(defaultPRAssociationFailureTTL + time.Second)
	if _, err := resolver.Reconcile(ctx); err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}
	if got := len(provider.listOpenCalls); got != 2 {
		t.Fatalf("past failure TTL (%s): ListOpenPRs calls = %d, want 2 (retried)",
			defaultPRAssociationFailureTTL, got)
	}
}

// TestPRsForRepo_NegativeCacheDoesNotOutliveRecovery verifies a repo that
// recovers is associated on the first pass after the window, rather than being
// held back by the remembered failure.
func TestPRsForRepo_NegativeCacheDoesNotOutliveRecovery(t *testing.T) {
	ctx := context.Background()
	sessions, repos, provider := newFailingListingFixture(t, 1)

	t0 := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)
	current := t0
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())
	resolver.SetNowForTest(func() time.Time { return current })

	if updated, err := resolver.Reconcile(ctx); err != nil || updated != 0 {
		t.Fatalf("failing pass: updated = %d, err = %v; want 0, nil", updated, err)
	}

	// The provider recovers, and the window lapses.
	provider.mu.Lock()
	delete(provider.openErr, "https://github.com/owner/repo")
	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 42, HeadBranch: "feature-0", State: vcs.PRStateOpen, Title: "recovered"},
	}
	provider.mu.Unlock()
	current = t0.Add(defaultPRAssociationFailureTTL + time.Second)

	updated, err := resolver.Reconcile(ctx)
	if err != nil {
		t.Fatalf("recovered pass: %v", err)
	}
	if updated != 1 {
		t.Fatalf("recovered pass: updated = %d, want 1", updated)
	}
	sess, err := sessions.Get(ctx, "sess-0")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.PRNumber == nil || *sess.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", sess.PRNumber)
	}
}

// TestPRsForRepo_DoesNotRememberFailureFromCallerTimeout pins the deliberate
// exception in rememberListingFailure: a listing killed by the CALLER's expired
// budget says nothing about the repo, so it must not be cached. Otherwise one
// exhausted 10s ListSessions budget would suppress the 60s periodic sweep's
// attempt for the whole failure window.
//
// This reproduces the reported incident directly — a `gh pr list` still running
// when the caller's deadline lands — and then proves the SAME resolver retries.
func TestPRsForRepo_DoesNotRememberFailureFromCallerTimeout(t *testing.T) {
	sessions, repos, provider := newFailingListingFixture(t, 2)

	// The listing succeeds eventually, but not before the caller's budget ends.
	provider.mu.Lock()
	delete(provider.openErr, "https://github.com/owner/repo")
	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 7, HeadBranch: "feature-0", State: vcs.PRStateOpen, Title: "slow listing"},
	}
	provider.openDelay = 250 * time.Millisecond
	provider.mu.Unlock()

	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())

	// 100ms, not 20ms. Both assertions below depend on the pass actually
	// REACHING provider.ListOpenPRs before the budget expires; under scheduling
	// delay on a loaded CI the top-of-loop ctx.Err() check can win first,
	// recording 0 calls and failing a test that is not about scheduling. 100ms
	// is comfortably above that noise and still far below the 250ms listing
	// delay, so the call is still interrupted mid-flight, which is the state
	// under test.
	tightCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := resolver.Reconcile(tightCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pass under an expiring budget: err = %v, want context.DeadlineExceeded", err)
	}
	if got := len(provider.listOpenCalls); got != 1 {
		t.Fatalf("timed-out pass: ListOpenPRs calls = %d, want 1", got)
	}

	// The provider is healthy again. Nothing about wall-clock time has changed —
	// the failure window is 15s and this test runs in milliseconds — so if the
	// timeout had been remembered, this pass would be served from the negative
	// cache and make no second call.
	provider.mu.Lock()
	provider.openDelay = 0
	provider.mu.Unlock()

	updated, err := resolver.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("retry pass: %v", err)
	}
	if got := len(provider.listOpenCalls); got != 2 {
		t.Fatalf("retry pass: ListOpenPRs calls = %d, want 2 (a caller timeout must not be cached)", got)
	}
	if updated != 1 {
		t.Fatalf("retry pass: updated = %d, want 1", updated)
	}
}

// TestReconcileSessions_StopsWhenCallerBudgetExpires pins the other half of the
// reported log spam: on a dead context the pass stops outright instead of
// walking every remaining session and logging a Warn for each one — those
// warnings blamed PR discovery for what is really a local repo read failing on
// an already-expired context.
func TestReconcileSessions_StopsWhenCallerBudgetExpires(t *testing.T) {
	sessions, repos, provider := newFailingListingFixture(t, 5)

	active, err := sessions.ListActive(context.Background(), "")
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 5 {
		t.Fatalf("fixture: active = %d, want 5", len(active))
	}

	var logs bytes.Buffer
	resolver := NewPRAssociationResolver(sessions, repos, provider,
		zerolog.New(&logs).Level(zerolog.DebugLevel))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	updated, err := resolver.ReconcileSessions(ctx, active)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if updated != 0 {
		t.Fatalf("updated = %d, want 0", updated)
	}

	// Nothing was attempted: not the local repo read, not the provider.
	if got := repos.getCalls.Load(); got != 0 {
		t.Errorf("repo Get calls = %d, want 0 (pass must stop before any work)", got)
	}
	if got := len(provider.listOpenCalls); got != 0 {
		t.Errorf("ListOpenPRs calls = %d, want 0", got)
	}

	warns, debugs := countReconcileFindPRLines(t, logs.String())
	if warns != 0 || debugs != 0 {
		t.Errorf("find-PR log lines = %d warn / %d debug, want 0/0", warns, debugs)
	}
}

// TestPRsForRepo_ConcurrentMissesShareOneProviderCall pins the coordination the
// negative cache alone cannot provide. main.go shares one resolver between
// ListSessions and the periodic reconciler, so two passes can both read a miss
// before either writes — and a FAILING repo then costs one gh call per
// concurrent pass, which is the per-call cost this whole mechanism exists to
// remove.
//
// # Why the assertion is taken WHILE the flight is open
//
// The obvious shape — launch N goroutines, join them, then assert one call —
// is racy, and it flaked in CI. A goroutine can signal "started" and then be
// descheduled before it reaches the flight; if the flight has completed and
// left the singleflight map by the time it arrives, it correctly opens a NEW
// flight and makes a second provider call. That is singleflight behaving as
// designed, not the defect under test, so an assertion taken after everyone
// has finished is measuring scheduling luck.
//
// The invariant that is actually true, and testable without timing luck, is:
// WHILE one flight is open, no number of additional callers may produce a
// second provider call. So the winner is pinned inside the provider, the count
// is captured before anything is released, and only then is the flight let go.
// Late arrivals after the release are irrelevant to the invariant and no longer
// break the test.
//
// The guard is still non-vacuous: the fake records each call BEFORE it blocks,
// so with the singleflight removed all `callers` goroutines append and the
// captured count is `callers`, not 1.
func TestPRsForRepo_ConcurrentMissesShareOneProviderCall(t *testing.T) {
	sessions, repos, provider := newFailingListingFixture(t, 2)

	const callers = 8

	// entered is signalled by every provider entry; release pins them there.
	entered := make(chan struct{}, callers)
	release := make(chan struct{})
	provider.mu.Lock()
	provider.openHook = func() {
		entered <- struct{}{}
		<-release
	}
	provider.mu.Unlock()

	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())

	arrived := make(chan struct{}, callers)

	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			arrived <- struct{}{}
			_, errs[i] = resolver.prsForRepo(context.Background(), "repo-1", "https://github.com/owner/repo")
		}()
	}

	// The flight is provably open once the winner is inside the provider.
	<-entered
	for range callers {
		<-arrived
	}
	// The load-bearing barrier, and it must establish that callers are INSIDE
	// singleflight.Do — not merely close to it. Arrival on the channel above
	// happens just before the call, so on a loaded scheduler every signal can be
	// drained while followers are still descheduled; a fixed settle would then
	// expire with only the winner having reached the provider, leaving
	// `during == 1` and passing VACUOUSLY, since that is equally true with the
	// singleflight removed. singleflight parks its waiters on an unexported
	// WaitGroup, so a stack dump is the available signal — the same reasoning,
	// and the same helper, as TestPrepareFailover_ConcurrentProbesCoalesce.
	waitForCallersInFlight(t, callers)

	provider.mu.Lock()
	during := len(provider.listOpenCalls)
	provider.mu.Unlock()

	close(release)
	wg.Wait()

	if during != 1 {
		t.Fatalf("ListOpenPRs calls while one flight was open = %d, want 1: %d concurrent misses must collapse onto one flight", during, callers)
	}
	// Every caller must still receive the failure — collapsing the call must not
	// silently succeed for the joiners.
	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d got nil error; the shared flight failed", i)
		}
	}
}

// TestPRsForRepo_LateFailureDoesNotEvictAFreshSuccess is the ordering interlock.
// A success is valid for its own TTL no matter what a later call observed;
// replacing it with a cached error would suppress every valid PR association
// for the failure window on a repo that is demonstrably reachable.
func TestPRsForRepo_LateFailureDoesNotEvictAFreshSuccess(t *testing.T) {
	sessions, repos, provider := newFailingListingFixture(t, 2)

	provider.mu.Lock()
	delete(provider.openErr, "https://github.com/owner/repo")
	provider.openPRs["https://github.com/owner/repo"] = []vcs.PRSummary{
		{Number: 7, HeadBranch: "feature-0", State: vcs.PRStateOpen, Title: "good listing"},
	}
	provider.mu.Unlock()

	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop())

	// Install a fresh, unexpired success.
	if _, err := resolver.prsForRepo(context.Background(), "repo-1", "https://github.com/owner/repo"); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	// A slower call that started earlier now resolves as a failure and tries to
	// record it. Reached directly, because reproducing the interleaving through
	// the public path is exactly the race the flight now prevents.
	resolver.rememberListingFailure(
		context.Background(),
		"repo-1",
		errors.New("list open PRs for repo \"repo-1\": gh: connection reset"),
	)

	// The cached success must survive, and must still be served without a call.
	before := len(provider.listOpenCalls)
	prs, err := resolver.prsForRepo(context.Background(), "repo-1", "https://github.com/owner/repo")
	if err != nil {
		t.Fatalf("a late failure evicted a fresh success: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 7 {
		t.Fatalf("prs = %+v, want the cached successful listing", prs)
	}
	if got := len(provider.listOpenCalls); got != before {
		t.Fatalf("ListOpenPRs calls = %d, want %d: the success should still be cached", got, before)
	}
}
