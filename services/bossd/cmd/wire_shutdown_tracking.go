package main

import (
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/safego"
)

func (w *daemonWiring) wireShutdownTracking() {
	// shutdownWG tracks daemon goroutines so we can wait for them to exit cleanly.
	// Subsystems that manage their own goroutines (poller, dispatcher, orchestrator,
	// display poller, tmux poller) expose a Done() channel; goroutines spawned
	// directly below use add/done via trackedGo below.
	//
	// It is a shutdownTracker rather than a bare sync.WaitGroup because a
	// WaitGroup has no notion of who incremented it, so a join that timed out
	// could only report that it gave up and never on what — ten unattributable
	// seconds on every forced exit. Each registration carries a static name, and
	// the forced-exit branch prints the ones still outstanding. run() holds the
	// only value of the type; see its doc comment for why that is the mechanism
	// keeping registration on the startup path.

	// archiveWG joins the auto-archive workers (BOS-923). They are deliberately
	// NOT on shutdownWG: shutdownWG's Wait is what proves the poller,
	// dispatcher, reconcile sweep and task orchestrator have stopped, and those
	// four are the archive producers — an archive launched by a producer's last
	// tick has to be joinable AFTER that Wait returns, not refused by it.
	//
	// A standing sentinel holds archiveWG's counter at one for the daemon's
	// whole life, released by closeArchiveTracking under the same mutex that
	// flips archiveTrackClosed. sync.WaitGroup forbids only an Add that lifts
	// the counter up FROM ZERO concurrently with Wait, so while the sentinel is
	// held no Add can be that Add, and after it is released no Add happens at
	// all. That matters because archives register from goroutines the daemon
	// does not own: Server.MergeSession's synchronous post-merge refresh
	// reaches the display poller's archive path on a gRPC handler goroutine,
	// which is not tracked anywhere. "The producing goroutine is itself already
	// tracked" is NOT true of every archive launch point and must not be
	// relied on.
	//
	// archiveOutstanding names the sessions currently registered, so the drain's
	// timeout branch can say which rows may be left unarchived instead of only
	// that some might be. It is a separate structure from the sentinel/mutex
	// pair above and changes none of that mechanism: the identity is already in
	// trackArchiveDone's hand, and this only stops it being thrown away.
	var (
		archiveWG          sync.WaitGroup
		archiveTrackMu     sync.Mutex
		archiveTrackClosed bool
		archiveOutstanding outstandingSet
	)
	archiveWG.Add(1)
	// closeArchiveTracking releases the sentinel and refuses any further
	// archive registration. Idempotent. Only ever called by drainArchiveWorkers
	// below, immediately before the join.
	closeArchiveTracking := func() {
		archiveTrackMu.Lock()
		defer archiveTrackMu.Unlock()
		if archiveTrackClosed {
			return
		}
		archiveTrackClosed = true
		archiveWG.Done()
	}

	// drainArchiveWorkers closes registration and then joins the archives
	// already registered, under a hard 10-second bound. Idempotent, and
	// deferred immediately below so it covers EVERY return from run, not just
	// the shutdown tail that calls it explicitly. That matters most for the
	// error return from srv.Shutdown missing its 5s deadline: without the defer
	// that path walks straight past the join into the deferred database.Close
	// and loses the archived_at write this whole mechanism exists to protect —
	// and it is the path a slow MergeSession handler, the archive producer the
	// tracker was built around, is most likely to take. Defers run LIFO and
	// database.Close is registered far above this, so this always runs first.
	// Reaching it before the daemon is up is harmless: the counter holds only
	// the sentinel, so releasing it makes the Wait return at once.
	archiveBudget := archiveDrainBudget
	if w.opts.archiveDrainBudget > 0 {
		archiveBudget = w.opts.archiveDrainBudget
	}
	w.goroutineBudget = shutdownGoroutineBudget
	if w.opts.shutdownGoroutineBudget > 0 {
		w.goroutineBudget = w.opts.shutdownGoroutineBudget
	}
	var archiveDrainOnce sync.Once
	w.drainArchiveWorkers = func() {
		archiveDrainOnce.Do(func() {
			closeArchiveTracking()
			waitForTrackedArchives(log.Logger, archiveWG.Wait, archiveOutstanding.snapshot, archiveBudget)
		})
	}
	w.deferCleanup(func() { w.drainArchiveWorkers() })

	// trackedGo spawns fn via safego.Go and registers it with shutdownWG under
	// name. The name is what the forced-exit branch prints when this goroutine
	// is one of the ones that did not stop, so it should be the vocabulary the
	// surrounding log lines already use for the subsystem. An empty name panics.
	w.trackedGo = func(name string, fn func()) {
		w.shutdownWG.add(name)
		safego.Go(log.Logger, func() {
			defer w.shutdownWG.done(name)
			fn()
		})
	}

	// trackDone registers a subsystem's Done() channel with shutdownWG under
	// name. Every call is on this startup path, before any wait, which is what
	// keeps its Add off zero.
	w.trackDone = func(name string, done <-chan struct{}) {
		w.shutdownWG.add(name)
		go func() {
			defer w.shutdownWG.done(name)
			<-done
		}()
	}

	if w.opts.onShutdownTrackerReady != nil {
		w.opts.onShutdownTrackerReady(w.trackDone)
	}

	// trackArchiveDone is the archive-worker tracker handed to the four archive
	// launch points below (BOS-923). Unlike trackDone it is called from
	// goroutines the daemon does not own — including a gRPC handler goroutine,
	// via MergeSession's synchronous post-merge refresh — at any moment,
	// shutdown included. The sentinel plus this mutex are what make that safe.
	//
	// Registration is refused only after the producers have stopped and the
	// archive drain has begun. A refused archive still runs, it is just no
	// longer joined — the pre-BOS-923 behaviour, and the session it abandons is
	// named in the log so the row can be reconciled by hand.
	w.trackArchiveDone = func(sessionID string, done <-chan struct{}) {
		archiveTrackMu.Lock()
		if archiveTrackClosed {
			archiveTrackMu.Unlock()
			log.Warn().
				Str("session", sessionID).
				Msg("archive started after shutdown closed archive tracking; not joined, archived_at may not be written")
			return
		}
		archiveWG.Add(1)
		archiveOutstanding.add(sessionID)
		archiveTrackMu.Unlock()
		go func() {
			defer archiveWG.Done()
			defer archiveOutstanding.clear(sessionID)
			<-done
		}()
	}
	// Startup symlink resolution is bounded, but macOS may leave a resolver
	// blocked on a TCC prompt after its diagnostic deadline. Include every
	// worker handle in daemon shutdown coordination instead of losing those
	// lifecycle signals when startup continues.
	for i, done := range w.startupDiagnosticWorkerDone {
		w.trackDone(fmt.Sprintf("symlink-resolution-worker-%d", i), done)
	}

}
