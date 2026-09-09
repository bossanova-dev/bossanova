package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// shutdownGoroutineBudget is the hard upper bound the daemon gives its tracked
// goroutines to exit once the shutdown sequence reaches the join. It is named
// verbatim in forcedGoroutineExitMsg below, and that correspondence is enforced
// by TestForcedExitMessagesNameTheirBudgets rather than by this sentence:
// changing either the constant or the message alone turns that test red.
const shutdownGoroutineBudget = 10 * time.Second

// cleanGoroutineExitMsg and forcedGoroutineExitMsg are the two operator-facing
// outcomes of the join. Both are grepped by hand in incident triage, so they
// are constants rather than inline literals: the wording is the interface.
const (
	cleanGoroutineExitMsg  = "all daemon goroutines exited cleanly"
	forcedGoroutineExitMsg = "forced exit: daemon goroutines did not stop within 10s"
)

// outstandingGoroutinesField is the structured field carrying the names of the
// goroutines a timed-out join gave up on. Deliberately the same shape as the
// draft-PR join's Strs("sessions", abandoned) a few lines above it in the
// shutdown tail, which is the one wait in this sequence that already named its
// subjects.
const outstandingGoroutinesField = "goroutines"

// archiveDrainBudget is the hard upper bound on the auto-archive join that runs
// after the goroutine join above (BOS-923). Named verbatim in
// forcedArchiveExitMsg, and that correspondence is enforced by
// TestForcedExitMessagesNameTheirBudgets rather than by this sentence.
const archiveDrainBudget = 10 * time.Second

// forcedArchiveExitMsg is the archive join's timeout line, and
// outstandingArchiveSessionsField carries the sessions it walked away from.
// There is deliberately no clean-path counterpart: a drained archive join has
// always been silent, and this change is about attributing the timeout, not
// about adding lines to a healthy shutdown.
const (
	forcedArchiveExitMsg            = "forced exit: auto-archive workers did not finish within 10s; a session may be left unarchived"
	outstandingArchiveSessionsField = "sessions"
)

// outstandingSet is a mutex-guarded multiset of the identities a bounded
// shutdown wait is still waiting on. A multiset rather than a set because two
// registrations may legitimately share a name (the indexed symlink-resolution
// workers are the near miss), and a set would let the first completion erase
// the second one's entry and under-report the timeout.
type outstandingSet struct {
	mu    sync.Mutex
	names map[string]int
}

func (s *outstandingSet) add(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names == nil {
		s.names = make(map[string]int)
	}
	s.names[name]++
}

func (s *outstandingSet) clear(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names[name] <= 1 {
		delete(s.names, name)
		return
	}
	s.names[name]--
}

// snapshot returns the names still outstanding, sorted so a log line is stable
// and diffable across shutdowns. The copy is what makes it safe to read while
// registrations are still completing.
func (s *outstandingSet) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.names))
	for name := range s.names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// shutdownTracker is the identity-carrying join run() uses for its daemon
// goroutines. It wraps a sync.WaitGroup — which has no notion of who
// incremented it, and so can only report that a Wait gave up, never on what —
// and records a name per registration so the forced-exit branch can name the
// subsystems that did not stop.
//
// Registration is startup-only. That is held by construction everywhere but
// one seam: the type and every one of its methods are unexported inside
// package main, which is a binary and cannot be imported by anything, and
// run() constructs the only value, keeps it in a local, and never lets it
// escape. The exception is runOpts.onShutdownTrackerReady, which is handed the
// run()-local trackDone closure so a test can inject a registration that never
// completes; for that one caller the startup-only rule is convention, stated
// on the field. Nothing in production sets it.
//
// That is load-bearing, not tidiness. trackDone carries no sentinel because
// every Add happens on the startup path before any wait, which is what keeps
// its counter off zero — and a WaitGroup Add that lifts the counter from zero
// concurrently with Wait panics the daemon. Its sibling trackArchiveDone holds
// a lifetime sentinel precisely because it IS reached from goroutines the
// daemon does not own. Making this type convenient to call from a handler
// goroutine reintroduces exactly the panic that sentinel exists to prevent.
type shutdownTracker struct {
	wg          sync.WaitGroup
	outstanding outstandingSet
}

// add registers one goroutine under name and lifts the WaitGroup counter.
//
// An empty name panics. The name parameter alone is not the gate — trackedGo("",
// fn) compiles — so without this a future call site could silently rejoin the
// anonymous set this whole type exists to abolish, and the forced-exit line
// would go back to naming nothing while every test still passed. Every name in
// the daemon is a compile-time constant chosen at the registration site, so an
// empty one is a programming error that the first boot surfaces, deterministically
// and loudly, rather than an operating condition to degrade through.
func (t *shutdownTracker) add(name string) {
	if name == "" {
		panic("shutdownTracker: registration with an empty name at " + registrationSite())
	}
	t.outstanding.add(name)
	t.wg.Add(1)
}

// done clears name and drops the WaitGroup counter. Ordering matters: the
// counter drops last, so a wait that returns can never observe a name still
// outstanding for a goroutine it has already joined.
func (t *shutdownTracker) done(name string) {
	t.outstanding.clear(name)
	t.wg.Done()
}

func (t *shutdownTracker) wait() { t.wg.Wait() }

// stillRunning names the registrations that have not completed. Callers read it
// only after a bounded wait has given up; on the drained path there is nothing
// to report and R3 forbids adding a field to that line.
func (t *shutdownTracker) stillRunning() []string { return t.outstanding.snapshot() }

// thisFile is the absolute path of this source file, captured once at init.
// registrationSite's frame filter compares against it rather than against a
// literal "shutdown_wait.go" suffix: a literal keeps compiling through a rename
// or a split of this file and silently starts reporting a frame inside the
// tracker itself as the registration site, which is the one thing the panic
// exists not to say. runtime.Caller(0) here reports the file this literal is
// written in, so the filter follows the code.
var thisFile = func() string {
	_, file, _, _ := runtime.Caller(0)
	return file
}()

// registrationSite names the call that registered without a name, so the panic
// points at the site to fix rather than at this file. It reports the nearest
// frames outside this file: the trackedGo / trackDone closure body and the run()
// line that called it.
func registrationSite() string {
	pcs := make([]uintptr, 16)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var sites []string
	for len(sites) < 2 {
		frame, more := frames.Next()
		if frame.File != "" && frame.File != thisFile {
			sites = append(sites, fmt.Sprintf("%s:%d", filepath.Base(frame.File), frame.Line))
		}
		if !more {
			break
		}
	}
	if len(sites) == 0 {
		return "an unknown site"
	}
	return strings.Join(sites, " <- ")
}

// waitForTrackedGoroutines races the daemon's tracked-goroutine join against a
// budget and reports whether it drained.
//
// It is a package-level function over a plain func value, extracted from run()
// for the same reason proxy_drain.go and plugin_gate.go were: run() is far too
// long and side-effect-heavy for a test to drive, so a branch that lives only
// inside it is a branch nothing can reach. The forced-exit branch below had no
// coverage at all before this extraction, because reaching it meant standing up
// a real daemon AND wedging one of its goroutines for the full ten seconds.
// With the wait and the budget injected, both branches are one unit test each.
//
// budget is a parameter so tests need not wait ten real seconds; production
// passes shutdownGoroutineBudget, which is the duration forcedGoroutineExitMsg
// names.
//
// outstanding is read only once the budget has expired, and the names it
// returns are logged as a structured field on that branch alone. The drained
// line keeps its exact previous wording and gains no fields: an operator
// grepping for a clean shutdown must not have to learn a new shape, and there
// is nothing outstanding to report on that path anyway.
func waitForTrackedGoroutines(logger zerolog.Logger, wait func(), outstanding func() []string, budget time.Duration) bool {
	if drainedWithin(wait, budget) {
		logger.Info().Msg(cleanGoroutineExitMsg)
		return true
	}
	// Snapshotted here, after the budget expired, so a goroutine that finishes
	// while the line is being written cannot retroactively change what the
	// record says the daemon gave up on.
	logger.Warn().
		Strs(outstandingGoroutinesField, outstanding()).
		Msg(forcedGoroutineExitMsg)
	return false
}

// drainedWithin runs wait on its own goroutine and reports whether it returned
// inside budget. The goroutine is deliberately abandoned when it does not: this
// is the shutdown tail, the daemon is about to exit, and the whole point of the
// bound is that something in there is not stopping.
func drainedWithin(wait func(), budget time.Duration) bool {
	waitCh := make(chan struct{})
	go func() {
		wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		return true
	case <-time.After(budget):
		return false
	}
}

// waitForTrackedArchives races the auto-archive join against a budget and
// reports whether it drained, naming the sessions still outstanding when it
// gives up.
//
// Same shape as waitForTrackedGoroutines and, like it, extracted so the timeout
// branch is reachable by a test rather than only by a real ten-second wedge.
// The defect it fixes is strictly cheaper than that one's: trackArchiveDone
// already receives the sessionID, already names it on the refusal path, and was
// simply discarding it here — so an operator was told a session may be left
// unarchived without being told which row to reconcile.
//
// The archive tracker's own sentinel and mutex are untouched by this. That path
// legitimately registers from goroutines the daemon does not own, and its
// safety comes from holding the WaitGroup counter off zero for the daemon's
// whole life; nothing here changes when or how that happens.
func waitForTrackedArchives(logger zerolog.Logger, wait func(), outstanding func() []string, budget time.Duration) bool {
	if drainedWithin(wait, budget) {
		return true
	}
	logger.Warn().
		Strs(outstandingArchiveSessionsField, outstanding()).
		Msg(forcedArchiveExitMsg)
	return false
}
