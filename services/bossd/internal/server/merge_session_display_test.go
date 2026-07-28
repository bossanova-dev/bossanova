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

// prRefresherSpy records post-merge re-poll calls so MergeSession's deferred
// refresh can be asserted without a real display poller. It implements only
// RefreshPRWithoutWebhookCredit — the sole method on the server's PRRefresher
// interface — so a MergeSession that reverted to the webhook-crediting
// RefreshPR would not compile against it.
type prRefresherSpy struct {
	calls  int
	origin string
	prNum  int
	err    error
}

func (s *prRefresherSpy) RefreshPRWithoutWebhookCredit(_ context.Context, origin string, prNum int) error {
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

// TestMergeSessionRefreshesPRAfterMergeOnlyClear pins one instance of the
// unconditional post-merge refresh: when no polled PR-status entry existed
// before the merge (e.g. the post-restart window), SetMerging creates a
// merge-only entry that the deferred clear drops. Without a re-poll, recompute
// would downgrade the persisted PR label to "stopped"; MergeSession's
// unconditional refresh restores the real label regardless. Exercised on the
// failed-merge return, where the session stays active and the downgrade would
// otherwise persist until the next webhook or poll tick.
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
		t.Fatalf("expected the post-merge refresh called once on merge-only clear, got %d", spy.calls)
	}
	if spy.origin != "https://github.com/acme/repo" || spy.prNum != 42 {
		t.Fatalf("refresh called with wrong args: origin=%q pr=%d", spy.origin, spy.prNum)
	}
}

// TestMergeSessionRefreshesPRWhenTrackerEntryExisted pins the common path: a
// prior poll already populated a real PR status (e.g. "✓ passing") before the
// merge landed. That label goes stale the instant the merge completes — a merge
// boss just performed must not need a webhook or the next display-poller tick
// (2 minutes by default) to reflect its own action — so the defer must re-poll
// unconditionally rather
// than skip the refresh just because SetMerging updated an existing entry
// instead of creating a merge-only one. Skipping here (the pre-BOS-534
// behaviour) meant every ordinary merge left the tracker showing the pre-merge
// status until the next webhook or poll tick.
func TestMergeSessionRefreshesPRWhenTrackerEntryExisted(t *testing.T) {
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
	if spy.calls != 1 {
		t.Fatalf("expected the post-merge refresh called once even though a polled entry already existed, got %d", spy.calls)
	}
	if spy.origin != "https://github.com/acme/repo" || spy.prNum != 42 {
		t.Fatalf("refresh called with wrong args: origin=%q pr=%d", spy.origin, spy.prNum)
	}
}

// TestMergeSessionRefreshesPRAfterSuccessfulMerge pins the headline case
// BOS-534 fixes: an actual successful PR merge, not one of the error
// short-circuits the other refresh tests use. As in the common path above, a
// prior poll already populated the tracker with a real PR status, so the
// pre-BOS-534 `!hadEntry` guard would have skipped the refresh here — the
// exact path every ordinary successful merge takes. The refresh runs
// synchronously inside the defer so MergeSessionResponse cannot race it,
// restoring the true "✓ merged" label instead of leaving the stale
// pre-merge one until the next webhook or poll tick.
func TestMergeSessionRefreshesPRAfterSuccessfulMerge(t *testing.T) {
	tracker := status.NewDisplayTracker()
	tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	spy := &prRefresherSpy{}
	prov := greenMergeProvider(nil, nil)
	// Non-empty mergeCommitSHA lets mergepolicy.VerifyOnBase's GetPRMergeCommit
	// read succeed instead of falling back to vcs.ErrPRNotMerged.
	prov.mergeCommitSHA = "abc123"

	srv := &Server{
		sessions:       &lifecycleSessionStoreFake{session: blockedFixLoopSession()},
		repos:          &archiveRepoStoreFake{repo: mergeDisplayRepo()},
		provider:       prov,
		worktrees:      &mergePolicyWorktrees{isAncestor: true},
		displayTracker: tracker,
		prRefresher:    spy,
		logger:         zerolog.Nop(),
	}

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err != nil {
		t.Fatalf("expected a successful merge, got %v", err)
	}
	if !prov.mergeCalled {
		t.Fatal("expected execution to reach the actual provider merge")
	}
	if spy.calls != 1 {
		t.Fatalf("expected the post-merge refresh called once after a successful merge, got %d", spy.calls)
	}
	if spy.origin != "https://github.com/acme/repo" || spy.prNum != 42 {
		t.Fatalf("refresh called with wrong args: origin=%q pr=%d", spy.origin, spy.prNum)
	}
}

// TestMergeSessionRefreshSkippedWhenPRRefresherNil proves MergeSession does
// not panic (a nil-interface method call) when s.prRefresher is nil but a
// tracker entry and a PR number both exist — the exact combination that
// reaches the refresh call's guard. Servers built without a PRRefresher
// configured (e.g. some test doubles, or a deployment that hasn't wired one)
// must still merge safely; the surviving `s.prRefresher != nil` check is what
// makes that true now that the `hadEntry` guard is gone.
func TestMergeSessionRefreshSkippedWhenPRRefresherNil(t *testing.T) {
	tracker := status.NewDisplayTracker()
	tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	prov := greenMergeProvider(nil, errors.New("merge short-circuited in test"))

	srv := &Server{
		sessions:       &lifecycleSessionStoreFake{session: blockedFixLoopSession()},
		repos:          &archiveRepoStoreFake{repo: mergeDisplayRepo()},
		provider:       prov,
		displayTracker: tracker,
		// prRefresher intentionally left nil.
		logger: zerolog.Nop(),
	}

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err == nil {
		t.Fatal("expected the injected merge error to propagate")
	}
	// The defer still ran to completion: reaching the (skipped) refresh means
	// the Merging flag was cleared first. Without this the test would only fail
	// on an outright panic. The entry must still exist — a seeded polled entry
	// surviving the clear is part of what is being asserted, so a nil-tolerant
	// check here would pass on a dropped entry too.
	entry := tracker.Get("s1")
	if entry == nil {
		t.Fatal("expected the seeded polled tracker entry to survive the deferred clear")
	}
	if entry.Merging {
		t.Fatal("expected the deferred SetMerging clear to run even with a nil prRefresher")
	}
}

// mergeLateGetFailStore serves the seeded session for the first Get and fails
// every Get after it. MergeSession calls Get exactly twice — once up front and
// once for the closing re-fetch — so this reproduces "the merge landed, then
// the final re-fetch failed", the path that leaves the handler's sess variable
// nil. It embeds lifecycleSessionStoreFake rather than the bare db.SessionStore
// interface so the Get/Update/Archive a future MergeSession is most likely to
// reach fall through to real fake behaviour instead of a nil-interface panic —
// which would look exactly like the nil-deref bug this test exists to pin. The
// rest of db.SessionStore still nil-panics through that fake's own bare embed.
type mergeLateGetFailStore struct {
	*lifecycleSessionStoreFake
	calls int
}

func (f *mergeLateGetFailStore) Get(_ context.Context, _ string) (*models.Session, error) {
	f.calls++
	if f.calls > 1 {
		return nil, errors.New("session row vanished before re-fetch")
	}
	return f.session, nil
}

// TestMergeSessionSurvivesSessionGetFailureAfterMerge pins the nil-deref the
// unconditional refresh would otherwise expose. MergeSession reassigns sess
// from the closing re-fetch, and that Get returns (nil, err) on failure — so a
// defer reading sess.PRNumber panics on a merge that already landed. The old
// `!hadEntry` guard hid this by short-circuiting before the deref on every
// ordinary merge; now that the refresh is unconditional the handler must
// instead capture the PR number up front. Asserts a clean CodeInternal error
// (a panic here would fail the test outright) and that the refresh still fired
// with the PR number the merge actually operated on.
func TestMergeSessionSurvivesSessionGetFailureAfterMerge(t *testing.T) {
	tracker := status.NewDisplayTracker()
	tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	spy := &prRefresherSpy{}
	prov := greenMergeProvider(nil, nil)
	prov.mergeCommitSHA = "abc123"

	srv := &Server{
		sessions:       &mergeLateGetFailStore{lifecycleSessionStoreFake: &lifecycleSessionStoreFake{session: blockedFixLoopSession()}},
		repos:          &archiveRepoStoreFake{repo: mergeDisplayRepo()},
		provider:       prov,
		worktrees:      &mergePolicyWorktrees{isAncestor: true},
		displayTracker: tracker,
		prRefresher:    spy,
		logger:         zerolog.Nop(),
	}

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err == nil {
		t.Fatal("expected the failed re-fetch to surface as an error")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("expected CodeInternal from the failed re-fetch, got %v", got)
	}
	if !prov.mergeCalled {
		t.Fatal("expected execution to reach the actual provider merge")
	}
	if spy.calls != 1 {
		t.Fatalf("expected the post-merge refresh called once despite the failed re-fetch, got %d", spy.calls)
	}
	if spy.prNum != 42 {
		t.Fatalf("expected the captured pre-merge PR number, got %d", spy.prNum)
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
