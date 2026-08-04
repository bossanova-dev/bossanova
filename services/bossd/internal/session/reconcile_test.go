package session

import (
	"context"
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
	err := m.openErr[repoPath]
	prs := m.openPRs[repoPath]
	m.mu.Unlock()

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

func (m *reconcileMockSessionStore) Archive(_ context.Context, _ string) error   { return nil }
func (m *reconcileMockSessionStore) Resurrect(_ context.Context, _ string) error { return nil }
func (m *reconcileMockSessionStore) Delete(_ context.Context, _ string) error    { return nil }
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
				WithArchiver(arch)

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
		WithArchiver(arch)

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
