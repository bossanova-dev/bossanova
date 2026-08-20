package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/status"
)

// handleRecorder is the ArchiveWorkerTracker stand-in these tests wire in place
// of main.go's trackArchiveDone. It records every handle it is handed, and the
// session that handle archives, on buffered channels so an assertion can count
// them without racing the archive goroutine.
type handleRecorder struct {
	handles  chan (<-chan struct{})
	sessions chan string
}

func newHandleRecorder() *handleRecorder {
	return &handleRecorder{
		handles:  make(chan (<-chan struct{}), 8),
		sessions: make(chan string, 8),
	}
}

func (h *handleRecorder) track(sessionID string, done <-chan struct{}) {
	h.sessions <- sessionID
	h.handles <- done
}

// nextSession returns the session ID handed alongside the next handle. The ID
// is what lets the daemon name a session whose archive it could not join.
func (h *handleRecorder) nextSession(t *testing.T) string {
	t.Helper()
	select {
	case id := <-h.sessions:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("no session ID was handed to the tracker")
		return ""
	}
}

// next returns the next tracked handle, failing if none arrives. The archive is
// launched from the caller's goroutine, so a handle that never shows up means
// the launch point dropped it — the BOS-923 defect itself.
func (h *handleRecorder) next(t *testing.T) <-chan struct{} {
	t.Helper()
	select {
	case done := <-h.handles:
		return done
	case <-time.After(2 * time.Second):
		t.Fatal("archive worker handle was never handed to the tracker")
		return nil
	}
}

// blockingArchiver holds ArchiveSession open until release is closed, so a test
// can observe the tracked handle while the archive is genuinely mid-flight.
// Without this, a handle that closed at launch time would be indistinguishable
// from one that represents the archive.
type blockingArchiver struct {
	entered chan string
	release chan struct{}
}

func newBlockingArchiver() *blockingArchiver {
	return &blockingArchiver{entered: make(chan string, 8), release: make(chan struct{})}
}

func (b *blockingArchiver) ArchiveSession(_ context.Context, id string) error {
	b.entered <- id
	<-b.release
	return nil
}

// assertOpen fails if done is already closed. A short deadline rather than a
// sleep: the point is that the handle is still open at this instant. The window
// is deliberately small — a handle that closed at launch is already closed by
// the time we look, so widening it buys nothing and only adds wall clock to
// every run, including CI's -race pass.
func assertOpen(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s: tracked handle closed while the archive was still blocked", what)
	case <-time.After(20 * time.Millisecond):
	}
}

func assertCloses(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: tracked handle never closed after the archive completed", what)
	}
}

// mergedDispatcherFixture builds the dispatcher shape every archive-after-merge
// test needs: one session on a flag-on repo, ready to receive a PRMerged event.
func mergedDispatcherFixture(t *testing.T) (*mockSessionStore, *mockRepoStore, *mockVCSProvider) {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", ShouldArchiveSessionsAfterMerge: true}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}
	return sessions, repos, vp
}

func runMergedEvent(ctx context.Context, d *Dispatcher) {
	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)
	d.Run(ctx, ch)
}

// TestDispatcherArchive_TracksWorkerHandle pins the BOS-923 fix at the webhook
// launch point: the completion channel safego.Go returns must reach the tracker
// instead of being discarded, and it must close once the archive is done.
func TestDispatcherArchive_TracksWorkerHandle(t *testing.T) {
	ctx := context.Background()
	sessions, repos, vp := mergedDispatcherFixture(t)

	arch := newFakeArchiver()
	rec := newHandleRecorder()
	d := NewDispatcher(sessions, repos, vp, zerolog.Nop())
	d.SetArchiver(arch, rec.track)

	runMergedEvent(ctx, d)

	select {
	case id := <-arch.calls:
		if id != "sess-1" {
			t.Fatalf("archived %q, want sess-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session was not archived after merge")
	}

	if id := rec.nextSession(t); id != "sess-1" {
		t.Errorf("tracker got session %q, want sess-1", id)
	}
	assertCloses(t, rec.next(t), "dispatcher")

	// Exactly one handle: one archive, one join. A second would mean the
	// tracker is being fed per-event rather than per-goroutine.
	select {
	case <-rec.handles:
		t.Fatal("more than one archive worker handle was tracked for a single merge")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestDispatcherArchive_TrackedHandleRepresentsTheArchive is what proves the
// handle is the archive rather than the launch. While the archiver is blocked
// the handle must stay open; it closes only once the archive returns. A handle
// that closed at launch would satisfy the test above but join nothing.
func TestDispatcherArchive_TrackedHandleRepresentsTheArchive(t *testing.T) {
	ctx := context.Background()
	sessions, repos, vp := mergedDispatcherFixture(t)

	arch := newBlockingArchiver()
	rec := newHandleRecorder()
	d := NewDispatcher(sessions, repos, vp, zerolog.Nop())
	d.SetArchiver(arch, rec.track)

	runMergedEvent(ctx, d)

	select {
	case <-arch.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("archiver was never entered")
	}

	done := rec.next(t)
	assertOpen(t, done, "dispatcher")
	close(arch.release)
	assertCloses(t, done, "dispatcher")
}

// TestDispatcherArchive_NilTrackerStillArchives keeps the seam nil-safe: a
// caller with no shutdown coordination (every test, and any future embedder)
// must get the old fire-and-forget behavior, not a panic.
func TestDispatcherArchive_NilTrackerStillArchives(t *testing.T) {
	ctx := context.Background()
	sessions, repos, vp := mergedDispatcherFixture(t)

	arch := newFakeArchiver()
	d := NewDispatcher(sessions, repos, vp, zerolog.Nop())
	d.SetArchiver(arch, nil)

	runMergedEvent(ctx, d)

	select {
	case id := <-arch.calls:
		if id != "sess-1" {
			t.Fatalf("archived %q, want sess-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session was not archived with a nil tracker")
	}
	if d.HasArchiveTracker() {
		t.Error("HasArchiveTracker() = true after SetArchiver(arch, nil)")
	}
}

// TestDispatcherArchive_JoinsAShutdownWaitGroup exercises the seam against the
// real consumer shape — main.go's trackArchiveDone closure over archiveWG —
// rather than a recorder. It is the unit-level statement of the whole ticket: the
// daemon's shutdown wait must not return while an archive is mid-flight.
//
// The standing wg.Add(1) below models main.go's sentinel: it holds the counter
// off zero so the tracker's nested Add can never be the Add that lifts it from
// zero concurrently with Wait. It is NOT a claim that the producing goroutine
// is tracked — MergeSession's post-merge refresh proves it need not be.
func TestDispatcherArchive_JoinsAShutdownWaitGroup(t *testing.T) {
	ctx := context.Background()
	sessions, repos, vp := mergedDispatcherFixture(t)

	var archiveWG sync.WaitGroup
	archiveWG.Add(1) // the sentinel main.go holds for the daemon's lifetime
	trackArchiveDone := func(_ string, done <-chan struct{}) {
		archiveWG.Add(1)
		go func() {
			defer archiveWG.Done()
			<-done
		}()
	}

	arch := newBlockingArchiver()
	d := NewDispatcher(sessions, repos, vp, zerolog.Nop())
	d.SetArchiver(arch, trackArchiveDone)

	runMergedEvent(ctx, d)

	select {
	case <-arch.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("archiver was never entered")
	}
	archiveWG.Done() // sentinel released, as main.go does before its Wait

	waited := make(chan struct{})
	go func() {
		archiveWG.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("shutdown wait returned while the archive was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(arch.release)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown wait never returned after the archive completed")
	}
}

// TestDisplayPollerArchive_TracksWorkerHandle covers the second launch point:
// the poller's terminal reconcile, which is the path a daemon that missed the
// merge webhook takes (and the one MergeSession's post-merge refresh drives).
func TestDisplayPollerArchive_TracksWorkerHandle(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()

	repos.repos["repo-1"] = &models.Repo{
		ID:                              "repo-1",
		OriginURL:                       "owner/repo",
		ShouldArchiveSessionsAfterMerge: true,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: intPtr(42),
		State:    machine.PushingBranch,
	}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "sha-resolved"}

	arch := newBlockingArchiver()
	rec := newHandleRecorder()
	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, zerolog.Nop())
	poller.SetArchiver(arch, rec.track)

	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	select {
	case <-arch.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("archiver was never entered")
	}

	done := rec.next(t)
	assertOpen(t, done, "display poller")
	close(arch.release)
	assertCloses(t, done, "display poller")
}

// TestReconcileSweepArchive_TracksOneHandlePerSession pins the multiplicity the
// sweep alone has: it loops over every merged-but-unarchived session, so the
// tracker must receive one handle per session in a single tick, not one per
// tick. A launch point that tracked only the last one would still look joined.
func TestReconcileSweepArchive_TracksOneHandlePerSession(t *testing.T) {
	ctx := context.Background()
	sessions := newReconcileMockSessionStore()
	repos := newMockRepoStore()
	provider := newReconcileMockProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:                              "repo-1",
		OriginURL:                       "https://github.com/owner/repo",
		ShouldArchiveSessionsAfterMerge: true,
	}

	const want = 3
	merged := make([]*models.Session, 0, want)
	for _, id := range []string{"sess-1", "sess-2", "sess-3"} {
		prNumber := 42
		sess := &models.Session{
			ID:         id,
			RepoID:     "repo-1",
			BranchName: "feature-" + id,
			State:      machine.Merged,
			PRNumber:   &prNumber,
		}
		merged = append(merged, sess)
		// The sweep re-reads each candidate before dispatching (BOS-924), so
		// the rows have to exist in the store, not just in the snapshot.
		sessions.addSession(sess)
	}

	arch := newFakeArchiver()
	rec := newHandleRecorder()
	resolver := NewPRAssociationResolver(sessions, repos, provider, zerolog.Nop()).
		WithArchiver(arch, rec.track)

	resolver.archiveMergedButUnarchived(ctx, merged)

	seen := make(map[string]bool, want)
	for i := 0; i < want; i++ {
		// Read the ID with the handle: N handles for one session repeated
		// would satisfy a bare count while still leaving two sessions
		// untracked.
		seen[rec.nextSession(t)] = true
		assertCloses(t, rec.next(t), "reconcile sweep")
	}
	for _, id := range []string{"sess-1", "sess-2", "sess-3"} {
		if !seen[id] {
			t.Errorf("no archive worker handle was tracked for %s", id)
		}
	}
	select {
	case <-rec.handles:
		t.Fatalf("more than %d archive worker handles tracked for %d merged sessions", want, want)
	case <-time.After(20 * time.Millisecond):
	}
}
