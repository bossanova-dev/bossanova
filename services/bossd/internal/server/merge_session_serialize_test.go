package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

// serializeSessionStore is a map-backed db.SessionStore for the merge
// serialization tests: it resolves distinct sessions by ID (unlike
// lifecycleSessionStoreFake, which returns one fixed row). Only Get is
// exercised; every other method nil-panics loudly via the embedded interface.
type serializeSessionStore struct {
	db.SessionStore
	sessions map[string]*models.Session
}

func (s *serializeSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	sess, ok := s.sessions[id]
	if !ok {
		return nil, errors.New("session not found: " + id)
	}
	return sess, nil
}

// serializeRepoStore is a map-backed db.RepoStore resolving distinct repos by ID.
type serializeRepoStore struct {
	db.RepoStore
	repos map[string]*models.Repo
}

func (r *serializeRepoStore) Get(_ context.Context, id string) (*models.Repo, error) {
	repo, ok := r.repos[id]
	if !ok {
		return nil, errors.New("repo not found: " + id)
	}
	return repo, nil
}

// blockingMergeProvider is a vcs.Provider whose live pre-merge gate always
// reports green, and whose MergePR blocks on a release channel while recording
// the observed maximum number of concurrently in-flight merges. It is the
// instrument the serialization tests use to prove per-repo mutual exclusion.
type blockingMergeProvider struct {
	// entered receives once for every MergePR that reaches its critical section.
	entered chan struct{}
	// release gates each MergePR's return; a receive from it lets one merge finish.
	release chan struct{}

	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	mergeCalls  atomic.Int32
}

func newBlockingMergeProvider() *blockingMergeProvider {
	return &blockingMergeProvider{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
}

func (p *blockingMergeProvider) MergePR(_ context.Context, _ string, _ int, _ string) error {
	p.mergeCalls.Add(1)
	n := p.inFlight.Add(1)
	for {
		max := p.maxInFlight.Load()
		if n <= max || p.maxInFlight.CompareAndSwap(max, n) {
			break
		}
	}
	p.entered <- struct{}{}
	<-p.release
	p.inFlight.Add(-1)
	// Short-circuit after the critical section so MergeSession returns without
	// touching VerifyOnBase / SyncBaseBranch (worktrees is nil in this harness).
	return errors.New("merge short-circuited in test")
}

// --- green live-gate reads + safe zero values for the rest of vcs.Provider ---

func (p *blockingMergeProvider) GetPRStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	return &vcs.PRStatus{
		State:            vcs.PRStateOpen,
		Mergeable:        boolPtr(true),
		MergeStateStatus: vcs.MergeStateStatusClean,
	}, nil
}

func (p *blockingMergeProvider) GetCheckResults(context.Context, string, int) ([]vcs.CheckResult, error) {
	return []vcs.CheckResult{{
		Status:     vcs.CheckStatusCompleted,
		Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
	}}, nil
}

func (p *blockingMergeProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	return nil, nil
}

func (p *blockingMergeProvider) GetAllowedMergeStrategies(context.Context, string) ([]string, error) {
	return []string{"merge"}, nil
}

func (p *blockingMergeProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return &vcs.PRInfo{}, nil
}
func (p *blockingMergeProvider) GetFailedCheckLogs(context.Context, string, string) (string, error) {
	return "", nil
}
func (p *blockingMergeProvider) MarkReadyForReview(context.Context, string, int) error { return nil }
func (p *blockingMergeProvider) ListOpenPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (p *blockingMergeProvider) ListClosedPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (p *blockingMergeProvider) SearchPRsByTitleTag(context.Context, string, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (p *blockingMergeProvider) UpdatePRTitle(context.Context, string, int, string) error { return nil }
func (p *blockingMergeProvider) GetPRMergeCommit(context.Context, string, int) (string, error) {
	return "", vcs.ErrPRNotMerged
}

func serializeSession(id, repoID string, pr int) *models.Session {
	prNum := pr
	return &models.Session{
		ID:         id,
		RepoID:     repoID,
		PRNumber:   &prNum,
		BaseBranch: "main",
		BranchName: "feature-" + id,
	}
}

func serializeRepo(id string) *models.Repo {
	return &models.Repo{
		ID:                id,
		OriginURL:         "https://github.com/acme/" + id,
		DefaultBaseBranch: "main",
		LocalPath:         "/x/" + id,
	}
}

func serializeMergeServer(t *testing.T, prov vcs.Provider, sessions map[string]*models.Session, repos map[string]*models.Repo) *Server {
	t.Helper()
	tracker := status.NewDisplayTracker()
	for id := range sessions {
		// Seed an entry so the tracker looks realistic going into the merge;
		// MergeSession's deferred SetMerging clear never dereferences the
		// (nil) prRefresher regardless, because that call is guarded by the
		// surviving `s.prRefresher != nil` check.
		tracker.Set(id, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	}
	return &Server{
		sessions:       &serializeSessionStore{sessions: sessions},
		repos:          &serializeRepoStore{repos: repos},
		provider:       prov,
		displayTracker: tracker,
		logger:         zerolog.Nop(),
	}
}

// TestMergeSession_SerializesPerRepo proves two concurrent MergeSession calls
// for the SAME repo never overlap their provider-merge critical section: the
// observed max in-flight MergePR is exactly 1.
func TestMergeSession_SerializesPerRepo(t *testing.T) {
	prov := newBlockingMergeProvider()
	sessions := map[string]*models.Session{
		"s1": serializeSession("s1", "r1", 1),
		"s2": serializeSession("s2", "r1", 2),
	}
	repos := map[string]*models.Repo{"r1": serializeRepo("r1")}
	srv := serializeMergeServer(t, prov, sessions, repos)

	var wg sync.WaitGroup
	for _, id := range []string{"s1", "s2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, _ = srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: id}))
		}(id)
	}

	// First merge reaches its critical section; release it, then the second
	// may acquire the gate and enter. If the gate did not serialize, both would
	// enter concurrently and maxInFlight would reach 2.
	waitEntered(t, prov)
	prov.release <- struct{}{}
	waitEntered(t, prov)
	prov.release <- struct{}{}
	wg.Wait()

	if got := prov.maxInFlight.Load(); got != 1 {
		t.Fatalf("max in-flight MergePR for same repo = %d, want 1", got)
	}
	if got := prov.mergeCalls.Load(); got != 2 {
		t.Fatalf("MergePR call count = %d, want 2", got)
	}
}

// TestMergeSession_ParallelAcrossRepos proves the gate is per-repo, not global:
// two MergeSession calls for DIFFERENT repos run their merge critical sections
// concurrently (observed max in-flight == 2).
func TestMergeSession_ParallelAcrossRepos(t *testing.T) {
	prov := newBlockingMergeProvider()
	sessions := map[string]*models.Session{
		"s1": serializeSession("s1", "r1", 1),
		"s2": serializeSession("s2", "r2", 2),
	}
	repos := map[string]*models.Repo{
		"r1": serializeRepo("r1"),
		"r2": serializeRepo("r2"),
	}
	srv := serializeMergeServer(t, prov, sessions, repos)

	var wg sync.WaitGroup
	for _, id := range []string{"s1", "s2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, _ = srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: id}))
		}(id)
	}

	// Both merges must reach their critical section before either is released.
	waitEntered(t, prov)
	waitEntered(t, prov)
	prov.release <- struct{}{}
	prov.release <- struct{}{}
	wg.Wait()

	if got := prov.maxInFlight.Load(); got != 2 {
		t.Fatalf("max in-flight MergePR across repos = %d, want 2", got)
	}
}

// TestMergeSession_CancelledWhileQueued proves a queued merge whose context is
// cancelled while it waits on the gate returns CodeCanceled and never calls
// MergePR for that request.
func TestMergeSession_CancelledWhileQueued(t *testing.T) {
	prov := newBlockingMergeProvider()
	sessions := map[string]*models.Session{
		"s1": serializeSession("s1", "r1", 1),
		"s2": serializeSession("s2", "r1", 2),
	}
	repos := map[string]*models.Repo{"r1": serializeRepo("r1")}
	srv := serializeMergeServer(t, prov, sessions, repos)

	// s1 acquires the gate and parks inside MergePR, holding the repo gate.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	}()
	waitEntered(t, prov)

	// s2 queues behind s1; cancelling its context while it waits on the full
	// gate must make it bail without ever calling MergePR.
	ctx2, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := srv.MergeSession(ctx2, connect.NewRequest(&pb.MergeSessionRequest{Id: "s2"}))
		errCh <- err
	}()
	cancel()

	select {
	case err := <-errCh:
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Fatalf("cancelled queued merge code = %v, want Canceled (err=%v)", connect.CodeOf(err), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelled merge to return")
	}

	// Let s1 finish and confirm only s1 ever reached MergePR.
	prov.release <- struct{}{}
	wg.Wait()
	if got := prov.mergeCalls.Load(); got != 1 {
		t.Fatalf("MergePR call count = %d, want 1 (cancelled s2 must not merge)", got)
	}
}

// TestMergeSession_DeadlineWhileQueued proves a queued merge whose context
// deadline expires while it waits on the gate returns CodeDeadlineExceeded (not
// CodeCanceled) and never calls MergePR for that request — preserving correct
// timeout semantics for the web path's mergeCommandDeadline.
func TestMergeSession_DeadlineWhileQueued(t *testing.T) {
	prov := newBlockingMergeProvider()
	sessions := map[string]*models.Session{
		"s1": serializeSession("s1", "r1", 1),
		"s2": serializeSession("s2", "r1", 2),
	}
	repos := map[string]*models.Repo{"r1": serializeRepo("r1")}
	srv := serializeMergeServer(t, prov, sessions, repos)

	// s1 acquires the gate and parks inside MergePR, holding the repo gate.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	}()
	waitEntered(t, prov)

	// s2 queues behind s1 with a short deadline; when it expires while waiting
	// on the full gate, it must bail as DeadlineExceeded without calling MergePR.
	ctx2, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := srv.MergeSession(ctx2, connect.NewRequest(&pb.MergeSessionRequest{Id: "s2"}))
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
			t.Fatalf("queued merge deadline code = %v, want DeadlineExceeded (err=%v)", connect.CodeOf(err), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for deadlined merge to return")
	}

	// Let s1 finish and confirm only s1 ever reached MergePR.
	prov.release <- struct{}{}
	wg.Wait()
	if got := prov.mergeCalls.Load(); got != 1 {
		t.Fatalf("MergePR call count = %d, want 1 (deadlined s2 must not merge)", got)
	}
}

// TestMergeSession_AlreadyCancelledDoesNotAcquire proves the gate honors an
// already-cancelled context even when it is FREE: a caller that already gave up
// never reaches MergePR. This exercises acquireRepoMerge's up-front ctx.Err()
// guard (the free-gate case the holder-blocked cancellation tests can't reach,
// since there only ctx.Done is ever ready). The companion post-acquire re-check
// — for the window where the holder releases at the same instant the caller's
// ctx fires — is not deterministically reproducible and so is covered by
// inspection, not a dedicated test.
func TestMergeSession_AlreadyCancelledDoesNotAcquire(t *testing.T) {
	prov := newBlockingMergeProvider()
	sessions := map[string]*models.Session{"s1": serializeSession("s1", "r1", 1)}
	repos := map[string]*models.Repo{"r1": serializeRepo("r1")}
	srv := serializeMergeServer(t, prov, sessions, repos)

	// Gate is free (no other merge holds it); the context is cancelled up front.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := srv.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("pre-cancelled merge code = %v, want Canceled (err=%v)", connect.CodeOf(err), err)
	}
	if got := prov.mergeCalls.Load(); got != 0 {
		t.Fatalf("MergePR call count = %d, want 0 (pre-cancelled merge must not merge)", got)
	}
}

func waitEntered(t *testing.T, p *blockingMergeProvider) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a merge to enter its critical section")
	}
}
