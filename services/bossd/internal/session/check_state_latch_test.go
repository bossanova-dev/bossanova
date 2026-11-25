package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

var _ db.CheckStateLatchStore = (*mockSessionStore)(nil)

func (m *mockSessionStore) SetCheckStateIfUnchanged(
	_ context.Context,
	id string,
	expectedState machine.CheckState,
	expectedHeadSHA *string,
	expectedAt *time.Time,
	newState machine.CheckState,
	newHeadSHA string,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false, fmt.Errorf("session %s not found", id)
	}
	if !sameCheckStateLatch(s, expectedState, expectedHeadSHA, expectedAt) {
		return false, nil
	}
	checkState := int(newState)
	headCopy := newHeadSHA
	headPtr := &headCopy
	nowStr := sqlutil.TimeNow()
	nowPtr := &nowStr
	params := db.UpdateSessionParams{
		LastCheckState:        &checkState,
		LastCheckStateHeadSHA: &headPtr,
		LastCheckStateAt:      &nowPtr,
	}
	if m.updateHook != nil {
		if err := m.updateHook(id, params); err != nil {
			return false, err
		}
	}
	m.updates = append(m.updates, mockSessionUpdate{id: id, params: params})
	s.LastCheckState = newState
	s.LastCheckStateHeadSHA = &headCopy
	s.LastCheckStateAt = sqlutil.ParseOptionalTime(&nowStr)
	return true, nil
}

func TestDisplayPollerLatchesCheckState(t *testing.T) {
	verifyDisplayPollerLatchesCheckState(t)
}

func TestDisplayPollerLatchesCheckState_Sessions_AC235401(t *testing.T) {
	verifyDisplayPollerLatchesCheckState(t)
}

func verifyDisplayPollerLatchesCheckState(t *testing.T) {
	t.Helper()
	success := vcs.CheckConclusionSuccess
	failure := vcs.CheckConclusionFailure
	cancelled := vcs.CheckConclusionCancelled
	skipped := vcs.CheckConclusionSkipped

	prevAt := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	oldHead := "old111"
	curHead := "abc123"

	tests := []struct {
		name         string
		state        machine.State
		draft        bool
		headSHA      string
		initState    machine.CheckState
		initHead     *string
		initAt       *time.Time
		checks       []vcs.CheckResult
		policy       RequiredChecksSource
		wantUpdates  int
		wantState    machine.CheckState
		wantHead     *string
		wantAtNonNil bool
		wantSameAt   bool
	}{
		{
			name:         "green_ready_for_review",
			state:        machine.ReadyForReview,
			draft:        false,
			headSHA:      curHead,
			checks:       []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
			wantUpdates:  1,
			wantState:    machine.CheckStatePassed,
			wantHead:     &curHead,
			wantAtNonNil: true,
		},
		{
			name:         "new_head_after_green",
			state:        machine.GreenDraft,
			draft:        true,
			headSHA:      curHead,
			initState:    machine.CheckStatePassed,
			initHead:     &oldHead,
			initAt:       &prevAt,
			checks:       []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
			wantUpdates:  1,
			wantState:    machine.CheckStatePassed,
			wantHead:     &curHead,
			wantAtNonNil: true,
		},
		{
			name:      "cancelled_no_write",
			state:     machine.GreenDraft,
			draft:     true,
			headSHA:   curHead,
			initState: machine.CheckStatePassed,
			initHead:  &curHead,
			initAt:    &prevAt,
			checks: []vcs.CheckResult{
				{Name: "build", Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &cancelled},
				{Name: "test", Workflow: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &success},
			},
			wantUpdates:  0,
			wantState:    machine.CheckStatePassed,
			wantHead:     &curHead,
			wantAtNonNil: true,
			wantSameAt:   true,
		},
		{
			name:         "failing",
			state:        machine.ReadyForReview,
			draft:        false,
			headSHA:      curHead,
			checks:       []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &failure}},
			wantUpdates:  1,
			wantState:    machine.CheckStateFailed,
			wantHead:     &curHead,
			wantAtNonNil: true,
		},
		{
			name:         "pending",
			state:        machine.ReadyForReview,
			draft:        false,
			headSHA:      curHead,
			checks:       []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusInProgress}},
			wantUpdates:  1,
			wantState:    machine.CheckStatePending,
			wantHead:     &curHead,
			wantAtNonNil: true,
		},
		{
			name:        "unknown_no_write",
			state:       machine.ReadyForReview,
			draft:       false,
			headSHA:     curHead,
			checks:      nil,
			wantUpdates: 0,
			wantState:   machine.CheckStateUnspecified,
		},
		{
			name:         "unchanged_no_write",
			state:        machine.ReadyForReview,
			draft:        false,
			headSHA:      curHead,
			initState:    machine.CheckStatePassed,
			initHead:     &curHead,
			initAt:       &prevAt,
			checks:       []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
			wantUpdates:  0,
			wantState:    machine.CheckStatePassed,
			wantHead:     &curHead,
			wantAtNonNil: true,
			wantSameAt:   true,
		},
		{
			name:        "empty_head_no_write",
			state:       machine.ReadyForReview,
			draft:       false,
			headSHA:     "",
			checks:      []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
			wantUpdates: 0,
			wantState:   machine.CheckStateUnspecified,
		},
		{
			name:         "skipped_only_latches_unspecified",
			state:        machine.GreenDraft,
			draft:        true,
			headSHA:      curHead,
			initState:    machine.CheckStatePassed,
			initHead:     &oldHead,
			initAt:       &prevAt,
			checks:       []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &skipped}},
			wantUpdates:  1,
			wantState:    machine.CheckStateUnspecified,
			wantHead:     &curHead,
			wantAtNonNil: true,
		},
		{
			name:        "required_check_missing_no_write",
			state:       machine.GreenDraft,
			draft:       true,
			headSHA:     curHead,
			checks:      []vcs.CheckResult{{Name: "lint", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
			policy:      pollerPolicyByPath{"/repos/widgets": {Names: []string{"go-test"}}},
			wantUpdates: 0,
			wantState:   machine.CheckStateUnspecified,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			tracker := status.NewDisplayTracker()

			prNum := 42
			repo := &models.Repo{ID: "repo-1", LocalPath: "/repos/widgets", OriginURL: "owner/widgets"}
			repos.repos[repo.ID] = repo

			var initHeadCopy *string
			if tc.initHead != nil {
				h := *tc.initHead
				initHeadCopy = &h
			}
			var initAtCopy *time.Time
			if tc.initAt != nil {
				at := *tc.initAt
				initAtCopy = &at
			}
			sess := &models.Session{
				ID:                    "sess-1",
				RepoID:                repo.ID,
				PRNumber:              &prNum,
				State:                 tc.state,
				LastCheckState:        tc.initState,
				LastCheckStateHeadSHA: initHeadCopy,
				LastCheckStateAt:      initAtCopy,
			}
			sessions.sessions[sess.ID] = sess

			vp.nextPRStatus = &vcs.PRStatus{
				State:            vcs.PRStateOpen,
				Draft:            tc.draft,
				Mergeable:        boolPtr(true),
				MergeStateStatus: vcs.MergeStateStatusClean,
				HeadSHA:          tc.headSHA,
				BaseBranch:       "main",
			}
			vp.nextCheckResults = tc.checks

			poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, zerolog.Nop())
			if tc.policy != nil {
				poller.SetRequiredChecks(tc.policy)
			}

			if ok := poller.pollSession(context.Background(), repo, sess, prNum); !ok {
				t.Fatalf("pollSession returned false, want true")
			}

			if len(sessions.updates) != tc.wantUpdates {
				t.Fatalf("Update calls = %d, want %d", len(sessions.updates), tc.wantUpdates)
			}
			got := sessions.sessions[sess.ID]
			if got.LastCheckState != tc.wantState {
				t.Errorf("LastCheckState = %v, want %v", got.LastCheckState, tc.wantState)
			}
			if tc.wantHead == nil {
				if got.LastCheckStateHeadSHA != nil {
					t.Errorf("LastCheckStateHeadSHA = %q, want nil", *got.LastCheckStateHeadSHA)
				}
			} else if got.LastCheckStateHeadSHA == nil || *got.LastCheckStateHeadSHA != *tc.wantHead {
				t.Errorf("LastCheckStateHeadSHA = %v, want %q", got.LastCheckStateHeadSHA, *tc.wantHead)
			}
			if tc.wantAtNonNil && got.LastCheckStateAt == nil {
				t.Errorf("LastCheckStateAt = nil, want non-nil")
			}
			if !tc.wantAtNonNil && got.LastCheckStateAt != nil {
				t.Errorf("LastCheckStateAt = %v, want nil", got.LastCheckStateAt)
			}
			if tc.wantSameAt && (got.LastCheckStateAt == nil || !got.LastCheckStateAt.Equal(prevAt)) {
				t.Errorf("LastCheckStateAt = %v, want unchanged %v", got.LastCheckStateAt, prevAt)
			}
		})
	}
}

func TestDisplayPollerLatchUpdateErrorDoesNotFailPoll(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()

	prNum := 42
	repo := &models.Repo{ID: "repo-1", LocalPath: "/repos/widgets", OriginURL: "owner/widgets"}
	repos.repos[repo.ID] = repo
	sess := &models.Session{
		ID:       "sess-1",
		RepoID:   repo.ID,
		PRNumber: &prNum,
		State:    machine.ReadyForReview,
	}
	sessions.sessions[sess.ID] = sess
	sessions.updateHook = func(_ string, _ db.UpdateSessionParams) error {
		return errors.New("db busy")
	}

	vp.nextPRStatus = &vcs.PRStatus{
		State:            vcs.PRStateOpen,
		Draft:            false,
		Mergeable:        boolPtr(true),
		MergeStateStatus: vcs.MergeStateStatusClean,
		HeadSHA:          "abc123",
		BaseBranch:       "main",
	}
	vp.nextCheckResults = []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &success}}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, zerolog.Nop())
	if ok := poller.pollSession(context.Background(), repo, sess, prNum); !ok {
		t.Fatalf("pollSession returned false on latch update error, want true")
	}
}

type recomputerFunc func(ctx context.Context, sessionID string) error

func (f recomputerFunc) Recompute(ctx context.Context, sessionID string) error {
	return f(ctx, sessionID)
}

func TestDisplayPollerLatchesCheckStateBeforeTrackerSet(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	for _, tc := range []struct {
		name  string
		state machine.State
		draft bool
	}{
		{name: "open_pr", state: machine.ReadyForReview, draft: false},
		{name: "draft_pr", state: machine.GreenDraft, draft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			tracker := status.NewDisplayTracker()

			prNum := 42
			repo := &models.Repo{ID: "repo-1", LocalPath: "/repos/widgets", OriginURL: "owner/widgets"}
			repos.repos[repo.ID] = repo
			sess := &models.Session{
				ID:       "sess-1",
				RepoID:   repo.ID,
				PRNumber: &prNum,
				State:    tc.state,
			}
			sessions.sessions[sess.ID] = sess

			vp.nextPRStatus = &vcs.PRStatus{
				State:            vcs.PRStateOpen,
				Draft:            tc.draft,
				Mergeable:        boolPtr(true),
				MergeStateStatus: vcs.MergeStateStatusClean,
				HeadSHA:          "abc123",
				BaseBranch:       "main",
			}
			vp.nextCheckResults = []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &success}}

			var observedState machine.CheckState
			var observedHead *string
			var observedAt *time.Time
			recomputeCalls := 0
			tracker.SetRecomputer(recomputerFunc(func(ctx context.Context, sessionID string) error {
				recomputeCalls++
				got, err := sessions.Get(ctx, sessionID)
				if err != nil {
					return err
				}
				observedState = got.LastCheckState
				observedHead = got.LastCheckStateHeadSHA
				observedAt = got.LastCheckStateAt
				return nil
			}))

			poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, zerolog.Nop())
			if ok := poller.pollSession(context.Background(), repo, sess, prNum); !ok {
				t.Fatalf("pollSession returned false, want true")
			}
			if recomputeCalls != 1 {
				t.Fatalf("Recompute calls = %d, want 1", recomputeCalls)
			}
			if observedState != machine.CheckStatePassed {
				t.Errorf("LastCheckState during tracker.Set Recompute = %v, want %v", observedState, machine.CheckStatePassed)
			}
			if observedHead == nil || *observedHead != "abc123" {
				t.Errorf("LastCheckStateHeadSHA during tracker.Set Recompute = %v, want abc123", observedHead)
			}
			if observedAt == nil {
				t.Errorf("LastCheckStateAt during tracker.Set Recompute = nil, want non-nil")
			}
		})
	}
}

type hookCheckProvider struct {
	*mockVCSProvider
	onGetCheckResults func()
}

func (p *hookCheckProvider) GetCheckResults(ctx context.Context, repo string, pr int) ([]vcs.CheckResult, error) {
	if p.onGetCheckResults != nil {
		p.onGetCheckResults()
	}
	return p.mockVCSProvider.GetCheckResults(ctx, repo, pr)
}

func TestDisplayPollerLatchDoesNotOverwriteConcurrentUpdate(t *testing.T) {
	failure := vcs.CheckConclusionFailure
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 11, 20, 12, 1, 0, 0, time.UTC)
	head1 := "head-1"
	head2 := "head-2"

	for _, tc := range []struct {
		name            string
		initState       machine.CheckState
		initHead        *string
		initAt          *time.Time
		concurrentState machine.CheckState
		concurrentHead  string
		polledHead      string
		polledChecks    []vcs.CheckResult
	}{
		{
			name:            "concurrent_dispatcher_verdict_same_head",
			initState:       machine.CheckStatePending,
			initHead:        &head1,
			initAt:          &t0,
			concurrentState: machine.CheckStatePassed,
			concurrentHead:  head1,
			polledHead:      head1,
			polledChecks:    []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &failure}},
		},
		{
			name:            "concurrent_webhook_newer_head",
			initState:       machine.CheckStateUnspecified,
			initHead:        nil,
			initAt:          nil,
			concurrentState: machine.CheckStatePassed,
			concurrentHead:  head2,
			polledHead:      head1,
			polledChecks:    []vcs.CheckResult{{Name: "test", Status: vcs.CheckStatusCompleted, Conclusion: &failure}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			baseVP := newMockVCSProvider()
			tracker := status.NewDisplayTracker()

			prNum := 42
			repo := &models.Repo{ID: "repo-1", LocalPath: "/repos/widgets", OriginURL: "owner/widgets"}
			repos.repos[repo.ID] = repo

			copyStrPtr := func(p *string) *string {
				if p == nil {
					return nil
				}
				v := *p
				return &v
			}
			copyTimePtr := func(p *time.Time) *time.Time {
				if p == nil {
					return nil
				}
				v := *p
				return &v
			}

			stored := &models.Session{
				ID:                    "sess-1",
				RepoID:                repo.ID,
				PRNumber:              &prNum,
				State:                 machine.ReadyForReview,
				LastCheckState:        tc.initState,
				LastCheckStateHeadSHA: copyStrPtr(tc.initHead),
				LastCheckStateAt:      copyTimePtr(tc.initAt),
			}
			sessions.sessions[stored.ID] = stored

			// Separate snapshot as SQLite ListActive returns per query.
			listSnapshot := &models.Session{
				ID:                    stored.ID,
				RepoID:                stored.RepoID,
				PRNumber:              &prNum,
				State:                 stored.State,
				LastCheckState:        tc.initState,
				LastCheckStateHeadSHA: copyStrPtr(tc.initHead),
				LastCheckStateAt:      copyTimePtr(tc.initAt),
			}

			baseVP.nextPRStatus = &vcs.PRStatus{
				State:            vcs.PRStateOpen,
				Draft:            false,
				Mergeable:        boolPtr(true),
				MergeStateStatus: vcs.MergeStateStatusClean,
				HeadSHA:          tc.polledHead,
				BaseBranch:       "main",
			}
			baseVP.nextCheckResults = tc.polledChecks

			vp := &hookCheckProvider{
				mockVCSProvider: baseVP,
				onGetCheckResults: func() {
					// Simulate Dispatcher or concurrent RefreshPR writing a newer verdict
					// while pollSession is in flight.
					state := int(tc.concurrentState)
					head := tc.concurrentHead
					headPtr := &head
					at := t1.UTC().Format("2006-01-02T15:04:05.000Z")
					atPtr := &at
					if _, err := sessions.Update(context.Background(), stored.ID, db.UpdateSessionParams{
						LastCheckState:        &state,
						LastCheckStateHeadSHA: &headPtr,
						LastCheckStateAt:      &atPtr,
					}); err != nil {
						t.Errorf("concurrent update: %v", err)
					}
				},
			}

			poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, zerolog.Nop())
			if ok := poller.pollSession(context.Background(), repo, listSnapshot, prNum); !ok {
				t.Fatalf("pollSession returned false, want true")
			}

			got := sessions.sessions[stored.ID]
			if got.LastCheckState != tc.concurrentState {
				t.Errorf("LastCheckState = %v, want preserved %v", got.LastCheckState, tc.concurrentState)
			}
			if got.LastCheckStateHeadSHA == nil || *got.LastCheckStateHeadSHA != tc.concurrentHead {
				t.Errorf("LastCheckStateHeadSHA = %v, want preserved %q", got.LastCheckStateHeadSHA, tc.concurrentHead)
			}
			if got.LastCheckStateAt == nil || !got.LastCheckStateAt.Equal(t1) {
				t.Errorf("LastCheckStateAt = %v, want preserved %v", got.LastCheckStateAt, t1)
			}
		})
	}
}
