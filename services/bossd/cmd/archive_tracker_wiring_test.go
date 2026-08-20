package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/recurser/bossalib/config"
)

// TestArchiveTrackerSeamsWired pins the BOS-923 fix to real startup wiring
// rather than to a setter call in a unit test. Four separate paths auto-archive
// a merged session — the PR-merged webhook, the display poller's terminal
// reconcile, the merged-but-unarchived sweep, and the dependabot task
// orchestrator — and each one launches the archive on a detached
// context.Background(). An archiver wired without a tracker leaves those
// goroutines outside shutdown coordination again, which has no shape at runtime: the daemon
// boots, archives work, and only a shutdown landing mid-archive shows the defect
// (a session whose worktree is gone but whose archived_at was never written).
// Asserting `live` here is what makes that regression a red test.
//
// The second half then drives a handle of the test's own through the very
// tracker those four sites received, and asserts run() will not return while it
// is open. That is a statement about the tracker, not about a literal archive:
// no merge is driven through the booted daemon here, so this half would still
// pass if archiveSessionAfterMergeIfEnabled stopped calling track. What proves
// the handle IS the archive lives in the session package's
// TestDispatcherArchive_TrackedHandleRepresentsTheArchive and its siblings; the
// two halves together are the coverage. goleak catches a tracker goroutine that
// outlives the daemon.
//
// Deliberately mirrors TestTransientResumeSeamsWired's shape (temp HOME, no
// plugins, stopSig/ready channels) so the seam tests fail the same way.
func TestArchiveTrackerSeamsWired(t *testing.T) {
	// Same lumberjack exemption as TestRun_GracefulShutdown_NoGoroutineLeak:
	// its mill goroutine has no public stop hook and dies with the process.
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
		goleak.IgnoreAnyFunction("gopkg.in/natefinch/lumberjack%2ev2.(*Logger).millRun"),
	)

	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(baseDir, "settings.json"))
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	dbPath := filepath.Join(baseDir, "bossd.db")
	socketPath := filepath.Join(baseDir, "bossd.sock")

	var hookFired atomic.Int32
	var seamsLive atomic.Int32
	trackerCh := make(chan func(string, <-chan struct{}), 1)

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     dbPath,
			socketPath: socketPath,
			plugins:    []config.PluginConfig{},
			onReady:    func() { close(ready) },
			onArchiveTrackerSeamsWired: func(live bool, track func(string, <-chan struct{})) {
				hookFired.Store(1)
				if live {
					seamsLive.Store(1)
				}
				select {
				case trackerCh <- track:
				default:
				}
			},
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state within 15s")
	}

	if hookFired.Load() != 1 {
		t.Fatal("onArchiveTrackerSeamsWired never fired")
	}
	if seamsLive.Load() != 1 {
		t.Fatal("at least one archive-after-merge archiver was wired without a shutdown tracker")
	}

	var track func(string, <-chan struct{})
	select {
	case track = <-trackerCh:
	default:
		t.Fatal("archive worker tracker was not captured")
	}
	if track == nil {
		t.Fatal("archive worker tracker is nil")
	}

	// Stand in for an archive still running when the signal lands. Registered
	// before SIGTERM, and from this test's own untracked goroutine — which is
	// exactly what MergeSession's post-merge refresh does in production. The
	// tracker is safe against that because run() holds a standing sentinel on
	// archiveWG and gates registration behind a mutex, not because producers
	// happen to be tracked.
	archiveInFlight := make(chan struct{})
	track("sess-in-flight", archiveInFlight)

	stopSig <- syscall.SIGTERM

	// run() must NOT return while the archive handle is open. A bounded
	// negative check rather than a sleep: the assertion is that shutdown is
	// still blocked at this instant, well inside the daemon's own 10s cap, so
	// nothing here can provoke the forced-exit path.
	select {
	case err := <-done:
		t.Fatalf("run returned while an archive was still in flight (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}

	close(archiveInFlight)

	// Bounded well under the daemon's own 10s forced-exit cap, which is how
	// this asserts the absence of the "forced exit: auto-archive workers did
	// not finish within 10s" path — the cap this handle is held by: that path
	// also returns nil, so only the timing separates a completed join from an
	// abandoned one. Note this half is a duration-bounded pair (still blocked
	// at 500ms, returned within 5s of the close), not a causal one: Go offers
	// no way from out here to prove the return was *caused* by the close.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of the archive completing (forced-exit path?)")
	}

	// An archive that starts after shutdown closed tracking must be refused,
	// not registered: a bare archiveWG.Add here would lift the counter from
	// zero concurrently with a Wait and panic the daemon. Refusal means no
	// watcher goroutine is spawned either, which goleak above checks — a
	// registration would leak one on this never-closed handle.
	lateArchive := make(chan struct{})
	track("sess-too-late", lateArchive)
}
