package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"

	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/status"
)

// mergeLocalWorktrees is a minimal WorktreeManager for the local-merge (no-PR)
// MergeSession path. Only MergeLocalBranch is exercised on that path; the
// embedded nil WorktreeManager makes any other call panic, which never happens
// here. onMerge fires inside MergeLocalBranch to observe display-tracker state
// at the exact moment the blocking merge runs.
type mergeLocalWorktrees struct {
	gitpkg.WorktreeManager
	err     error
	onMerge func()
}

func (m *mergeLocalWorktrees) MergeLocalBranch(context.Context, string, string, string, string) error {
	if m.onMerge != nil {
		m.onMerge()
	}
	return m.err
}

// greenMergeProvider returns a provider whose live PR reads pass the pre-merge
// gate (open, mergeable, clean, one passed check) so MergeSession reaches the
// blocking provider merge. onMerge/mergeErr are threaded onto the merge itself.
func greenMergeProvider(onMerge func(), mergeErr error) *mergeGateProvider {
	return &mergeGateProvider{
		prStatus: &vcs.PRStatus{
			State:            vcs.PRStateOpen,
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
		checks: []vcs.CheckResult{{
			Status:     vcs.CheckStatusCompleted,
			Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
		}},
		onMerge:  onMerge,
		mergeErr: mergeErr,
	}
}

// prRefresherSpy records RefreshPR calls so the merge-only re-poll path can be
// asserted without a real display poller.
type prRefresherSpy struct {
	calls  int
	origin string
	prNum  int
	err    error
}

func (s *prRefresherSpy) RefreshPR(_ context.Context, origin string, prNum int) error {
	s.calls++
	s.origin = origin
	s.prNum = prNum
	return s.err
}

func mergeDisplayRepo() *models.Repo {
	return &models.Repo{
		ID:                "r1",
		OriginURL:         "https://github.com/acme/repo",
		DefaultBaseBranch: "main",
		LocalPath:         "/x",
	}
}

// TestMergeSessionSetsMergingBeforeProviderMergeAndClearsOnError pins the
// error path: MergeSession sets DisplayMerging BEFORE the blocking provider
// merge (so the synchronous recompute streams "merging" to clients first) and
// the defer clears it when the merge returns an error.
func TestMergeSessionSetsMergingBeforeProviderMergeAndClearsOnError(t *testing.T) {
	tracker := status.NewDisplayTracker()
	var mergingDuringMerge bool
	prov := greenMergeProvider(func() {
		if e := tracker.Get("s1"); e != nil {
			mergingDuringMerge = e.Merging
		}
	}, errors.New("merge short-circuited in test"))

	srv := &Server{
		sessions:       &lifecycleSessionStoreFake{session: blockedFixLoopSession()},
		repos:          &archiveRepoStoreFake{repo: mergeDisplayRepo()},
		provider:       prov,
		displayTracker: tracker,
		logger:         zerolog.Nop(),
	}

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err == nil {
		t.Fatal("expected the injected merge error to propagate")
	}
	if !prov.mergeCalled {
		t.Fatal("expected execution to reach the blocking provider merge")
	}
	if !mergingDuringMerge {
		t.Fatal("DisplayMerging was not set before the blocking merge; clients would not see 'merging'")
	}
	// The defer must clear the merging-only entry on the error return.
	if e := tracker.Get("s1"); e != nil && e.Merging {
		t.Fatalf("expected Merging cleared on error return, got %+v", e)
	}
}

// TestMergeSessionRefreshesPRAfterMergeOnlyClear pins the merge-only repair
// path: when no polled PR-status entry existed before the merge (e.g. the
// post-restart window), SetMerging creates a merge-only entry that the deferred
// clear drops. Without a re-poll, recompute would downgrade the persisted PR
// label to "stopped"; MergeSession instead calls RefreshPR with the session's
// PR so the computer restores the real label. Exercised on the failed-merge
// return, where the session stays active and the downgrade would otherwise persist.
func TestMergeSessionRefreshesPRAfterMergeOnlyClear(t *testing.T) {
	tracker := status.NewDisplayTracker()
	spy := &prRefresherSpy{}
	prov := greenMergeProvider(nil, errors.New("merge short-circuited in test"))

	srv := &Server{
		sessions:       &lifecycleSessionStoreFake{session: blockedFixLoopSession()},
		repos:          &archiveRepoStoreFake{repo: mergeDisplayRepo()},
		provider:       prov,
		displayTracker: tracker,
		prRefresher:    spy,
		logger:         zerolog.Nop(),
	}

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err == nil {
		t.Fatal("expected the injected merge error to propagate")
	}
	if spy.calls != 1 {
		t.Fatalf("expected RefreshPR called once on merge-only clear, got %d", spy.calls)
	}
	if spy.origin != "https://github.com/acme/repo" || spy.prNum != 42 {
		t.Fatalf("RefreshPR called with wrong args: origin=%q pr=%d", spy.origin, spy.prNum)
	}
}

// TestMergeSessionSkipsPRRefreshWhenTrackerEntryExisted pins the complement: a
// prior polled PR status means SetMerging updates an existing entry rather than
// creating a merge-only one, so clearing Merging preserves the real status and
// no re-poll is needed. RefreshPR must not fire (and must not incur a needless
// GitHub round-trip) on the common path.
func TestMergeSessionSkipsPRRefreshWhenTrackerEntryExisted(t *testing.T) {
	tracker := status.NewDisplayTracker()
	// A prior poll populated a real PR status for this session.
	tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	spy := &prRefresherSpy{}
	prov := greenMergeProvider(nil, errors.New("merge short-circuited in test"))

	srv := &Server{
		sessions:       &lifecycleSessionStoreFake{session: blockedFixLoopSession()},
		repos:          &archiveRepoStoreFake{repo: mergeDisplayRepo()},
		provider:       prov,
		displayTracker: tracker,
		prRefresher:    spy,
		logger:         zerolog.Nop(),
	}

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err == nil {
		t.Fatal("expected the injected merge error to propagate")
	}
	if spy.calls != 0 {
		t.Fatalf("expected no RefreshPR when a polled entry already existed, got %d", spy.calls)
	}
}

// TestMergeSessionSetsMergingBeforeLocalMergeAndClearsOnSuccess pins the
// success path via the local-branch merge (no PR): DisplayMerging is set before
// the blocking local merge and the defer clears it on the successful return.
func TestMergeSessionSetsMergingBeforeLocalMergeAndClearsOnSuccess(t *testing.T) {
	tracker := status.NewDisplayTracker()
	var mergingDuringMerge bool
	wt := &mergeLocalWorktrees{onMerge: func() {
		if e := tracker.Get("s1"); e != nil {
			mergingDuringMerge = e.Merging
		}
	}}

	// No PRNumber -> local merge path (skips the live gate and provider merge).
	localSession := &models.Session{
		ID:         "s1",
		RepoID:     "r1",
		BaseBranch: "main",
		BranchName: "feature",
	}

	srv := &Server{
		sessions:       &lifecycleSessionStoreFake{session: localSession},
		repos:          &archiveRepoStoreFake{repo: mergeDisplayRepo()},
		worktrees:      wt,
		displayTracker: tracker,
		logger:         zerolog.Nop(),
	}

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err != nil {
		t.Fatalf("expected a successful local merge, got %v", err)
	}
	if !mergingDuringMerge {
		t.Fatal("DisplayMerging was not set before the blocking local merge")
	}
	// The defer must clear the merging-only entry on the successful return.
	if e := tracker.Get("s1"); e != nil && e.Merging {
		t.Fatalf("expected Merging cleared on successful return, got %+v", e)
	}
}
