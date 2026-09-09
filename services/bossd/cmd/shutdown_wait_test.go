package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// testBudget is short enough that a timing-out case costs milliseconds rather
// than the ten real seconds production waits, and long enough that a loaded CI
// machine still lets an immediately-returning wait win the race.
const testBudget = 250 * time.Millisecond

func newCapturingLogger() (zerolog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return zerolog.New(buf), buf
}

// blockingWait returns a wait func that never returns until the test ends.
// Every test that uses it needs that release, or the goroutine
// waitForTrackedGoroutines spawned outlives the test.
func blockingWait(t *testing.T) func() {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func() { <-release }
}

// trackBlocked registers name on tracker behind a goroutine that blocks until
// the returned release runs, standing in for a subsystem that does not honour
// cancellation. It mirrors what run()'s trackedGo closure does, which is what
// makes these tests exercise the registration path rather than a hand-built
// name list. Released in t.Cleanup as well, so a timing-out case leaves nothing
// running.
func trackBlocked(t *testing.T, tracker *shutdownTracker, name string) func() {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(release) }) }
	t.Cleanup(stop)

	tracker.add(name)
	go func() {
		defer tracker.done(name)
		<-release
	}()
	return stop
}

func assertLogged(t *testing.T, buf *bytes.Buffer, want string) {
	t.Helper()
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("expected log to contain %q, got: %s", want, buf.String())
	}
}

func assertNotLogged(t *testing.T, buf *bytes.Buffer, unwanted string) {
	t.Helper()
	if strings.Contains(buf.String(), unwanted) {
		t.Fatalf("expected log NOT to contain %q, got: %s", unwanted, buf.String())
	}
}

// TestWaitForTrackedGoroutines_Drained is the clean path: every tracked
// goroutine exited inside the budget. The negative half is the point — an
// implementation that logged both lines, or that logged the forced-exit line
// unconditionally, would pass a "contains the clean line" assertion alone. It
// also pins R3: the clean line gains no names field, so an operator's existing
// grep for a clean shutdown still matches byte for byte.
func TestWaitForTrackedGoroutines_Drained(t *testing.T) {
	logger, buf := newCapturingLogger()

	var tracker shutdownTracker
	tracker.add("poller")
	tracker.done("poller")

	if drained := waitForTrackedGoroutines(logger, tracker.wait, tracker.stillRunning, testBudget); !drained {
		t.Fatal("expected a wait that returns immediately to report drained")
	}

	assertLogged(t, buf, cleanGoroutineExitMsg)
	assertNotLogged(t, buf, forcedGoroutineExitMsg)
	// The JSON key form, not the bare word: the clean message itself contains
	// "goroutines", so asserting the bare token would be vacuous here.
	assertNotLogged(t, buf, `"`+outstandingGoroutinesField+`":`)
	assertNotLogged(t, buf, "poller")
}

// TestWaitForTrackedGoroutines_TimesOut is the branch that had no coverage at
// all before the helper was extracted: a goroutine that never honours
// cancellation, and the bounded wait giving up on it.
func TestWaitForTrackedGoroutines_TimesOut(t *testing.T) {
	logger, buf := newCapturingLogger()

	start := time.Now()
	drained := waitForTrackedGoroutines(logger, blockingWait(t), func() []string { return nil }, testBudget)
	elapsed := time.Since(start)

	if drained {
		t.Fatal("expected a wait that never returns to report not drained")
	}
	assertLogged(t, buf, forcedGoroutineExitMsg)
	assertNotLogged(t, buf, cleanGoroutineExitMsg)

	// The budget is a real parameter, not a decoration over the hard-coded
	// production constant: if it were ignored this would have taken 10s.
	if elapsed >= shutdownGoroutineBudget {
		t.Fatalf("budget parameter was ignored: waited %s with a %s budget", elapsed, testBudget)
	}
}

// TestWaitForTrackedGoroutines_NamesOnlyTheOutstandingGoroutine is the whole
// point of the ticket, and its negative half is what keeps it honest: naming
// everything ever registered would be no more attributable than naming nothing.
//
// It pins mis-pairing at the tracker level only: the identity in the line is the
// one the blocked registration supplied, so an implementation that reported some
// other registered name turns this red. It does NOT reach run()'s call sites --
// the names here are the test's own -- so swapping two names across their
// registration sites in main.go stays green. Nothing on this branch pins a
// specific call site to a specific name.
func TestWaitForTrackedGoroutines_NamesOnlyTheOutstandingGoroutine(t *testing.T) {
	logger, buf := newCapturingLogger()

	var tracker shutdownTracker
	tracker.add("display-poller")
	tracker.done("display-poller")
	trackBlocked(t, &tracker, "tmux-reaper")

	if waitForTrackedGoroutines(logger, tracker.wait, tracker.stillRunning, testBudget) {
		t.Fatal("expected a blocked registration to keep the join from draining")
	}

	assertLogged(t, buf, forcedGoroutineExitMsg)
	assertLogged(t, buf, "tmux-reaper")
	assertNotLogged(t, buf, "display-poller")
}

// TestWaitForTrackedGoroutines_NamesEveryOutstandingGoroutine pins the field as
// a list rather than a first match. An implementation that reported only one
// name would still make the delta incident attributable to *something*, and
// would still be wrong about how much was stuck.
func TestWaitForTrackedGoroutines_NamesEveryOutstandingGoroutine(t *testing.T) {
	logger, buf := newCapturingLogger()

	var tracker shutdownTracker
	stuck := []string{"broadcast-delivery-worker", "grpc-server", "poller"}
	for _, name := range stuck {
		trackBlocked(t, &tracker, name)
	}

	if waitForTrackedGoroutines(logger, tracker.wait, tracker.stillRunning, testBudget) {
		t.Fatal("expected three blocked registrations to keep the join from draining")
	}

	for _, name := range stuck {
		assertLogged(t, buf, name)
	}
}

// TestWaitForTrackedGoroutines_LateCompletionDoesNotRewriteTheRecord: a
// goroutine that finishes after the join gave up must not retroactively alter
// what the log said the daemon abandoned. The record is evidence for a later
// incident read; a name that could vanish from it after the fact would be worse
// than no name at all.
func TestWaitForTrackedGoroutines_LateCompletionDoesNotRewriteTheRecord(t *testing.T) {
	logger, buf := newCapturingLogger()

	var tracker shutdownTracker
	release := trackBlocked(t, &tracker, "callback-delivery-worker")

	if waitForTrackedGoroutines(logger, tracker.wait, tracker.stillRunning, testBudget) {
		t.Fatal("expected a blocked registration to keep the join from draining")
	}
	logged := buf.String()

	release()
	tracker.wait()

	if tracker.stillRunning() != nil && len(tracker.stillRunning()) != 0 {
		t.Fatalf("expected the tracker to be empty after the late completion, got %v", tracker.stillRunning())
	}
	if buf.String() != logged {
		t.Fatalf("the log changed after the wait returned:\nbefore: %s\nafter:  %s", logged, buf.String())
	}
	assertLogged(t, buf, "callback-delivery-worker")
}

// TestShutdownTrackerRejectsEmptyName is the gate R2 asks for. The name
// parameter alone is not one: trackedGo("", fn) compiles, so without a runtime
// rejection a future call site could silently rejoin the anonymous set and
// every existing test would stay green.
func TestShutdownTrackerRejectsEmptyName(t *testing.T) {
	var tracker shutdownTracker

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected an empty registration name to panic")
		}
		msg, ok := recovered.(string)
		if !ok {
			t.Fatalf("expected a string panic value, got %T: %v", recovered, recovered)
		}
		if !strings.Contains(msg, "empty name") {
			t.Fatalf("panic message does not say what is wrong: %q", msg)
		}
		// The site is what makes the panic actionable — an empty name has, by
		// construction, no other identity to report.
		if !strings.Contains(msg, "shutdown_wait_test.go:") {
			t.Fatalf("panic message does not name the registration site: %q", msg)
		}
	}()

	tracker.add("")
}

// TestShutdownTrackerNamesSurviveDuplicateRegistrations: two goroutines may
// legitimately share a name, and the first completion must not erase the
// second's entry. A plain set would under-report the timeout here.
func TestShutdownTrackerNamesSurviveDuplicateRegistrations(t *testing.T) {
	var tracker shutdownTracker

	tracker.add("symlink-resolution-worker-0")
	tracker.add("symlink-resolution-worker-0")
	tracker.done("symlink-resolution-worker-0")

	if got := tracker.stillRunning(); len(got) != 1 || got[0] != "symlink-resolution-worker-0" {
		t.Fatalf("expected the second registration to still be outstanding, got %v", got)
	}

	tracker.done("symlink-resolution-worker-0")
	if got := tracker.stillRunning(); len(got) != 0 {
		t.Fatalf("expected an empty tracker, got %v", got)
	}
}

// TestShutdownTrackerConcurrentCompletionDoesNotRace exercises the shape the
// daemon actually has at shutdown: many tracked goroutines completing while the
// bounded wait reads the name set. Meaningful under -race (make test-race);
// under a plain run it is still a smoke test that the join terminates.
func TestShutdownTrackerConcurrentCompletionDoesNotRace(t *testing.T) {
	logger, _ := newCapturingLogger()

	var tracker shutdownTracker
	const workers = 32
	for i := 0; i < workers; i++ {
		name := "worker"
		if i%2 == 0 {
			name = "other-worker"
		}
		tracker.add(name)
		go func(name string) {
			time.Sleep(time.Millisecond)
			tracker.done(name)
		}(name)
	}

	// Readers racing the completions above, the way the timeout branch does.
	var readers sync.WaitGroup
	stopReading := make(chan struct{})
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReading:
					return
				default:
					_ = tracker.stillRunning()
				}
			}
		}()
	}

	drained := waitForTrackedGoroutines(logger, tracker.wait, tracker.stillRunning, 5*time.Second)
	close(stopReading)
	readers.Wait()

	if !drained {
		t.Fatal("expected every worker to complete inside a 5s budget")
	}
	if got := tracker.stillRunning(); len(got) != 0 {
		t.Fatalf("expected an empty tracker after all workers completed, got %v", got)
	}
}

// TestWaitForTrackedArchives_Drained: a join that completes stays silent, the
// way it always has. The archive drain has no clean-path line, and adding one
// would put a new record in every healthy shutdown for no diagnostic gain.
func TestWaitForTrackedArchives_Drained(t *testing.T) {
	logger, buf := newCapturingLogger()

	var outstanding outstandingSet
	outstanding.add("sess-done")
	outstanding.clear("sess-done")

	if drained := waitForTrackedArchives(logger, func() {}, outstanding.snapshot, testBudget); !drained {
		t.Fatal("expected a join that returns immediately to report drained")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected a drained archive join to log nothing, got: %s", buf.String())
	}
}

// TestWaitForTrackedArchives_NamesOnlyTheOutstandingSession is the defect this
// leg fixes: the timeout already held every sessionID and printed none of them.
// The negative half matters as much as the positive one — naming a session that
// finished would send an operator to reconcile a row that is already correct.
func TestWaitForTrackedArchives_NamesOnlyTheOutstandingSession(t *testing.T) {
	logger, buf := newCapturingLogger()

	var outstanding outstandingSet
	outstanding.add("sess-finished")
	outstanding.clear("sess-finished")
	outstanding.add("sess-stuck")

	if waitForTrackedArchives(logger, blockingWait(t), outstanding.snapshot, testBudget) {
		t.Fatal("expected an outstanding archive to keep the join from draining")
	}

	assertLogged(t, buf, forcedArchiveExitMsg)
	assertLogged(t, buf, "sess-stuck")
	assertNotLogged(t, buf, "sess-finished")
}

// readDaemonLog returns the rotated bossd log a daemon under test wrote. The
// caller must have isolated XDG_STATE_HOME, or this reads the developer's real
// log and any assertion over it is meaningless.
//
// It exists because the assertion these tests actually need is about the log
// line an operator will grep during an incident, not about a value returned to
// a hook — a daemon that computed the right names and logged the wrong line
// would still leave the next forced exit unattributable.
func readDaemonLog(t *testing.T, stateHome string) string {
	t.Helper()
	path := filepath.Join(stateHome, "bossanova", "logs", "bossd.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read daemon log %s: %v", path, err)
	}
	return string(data)
}

// TestForcedExitMessagesNameTheirBudgets is the enforcement the two budget
// constants previously had only in prose ("named verbatim in ..., so the two
// move together"). A comment does not move anything: budget is now an injected
// parameter, so raising shutdownGoroutineBudget to 15s while
// forcedGoroutineExitMsg still says "within 10s" would ship an operator-facing
// line that is simply false, with every other test on this branch green,
// because they all reference the message symbolically.
//
// Contains rather than equality because the number is embedded in a sentence,
// and Duration.String() renders the two production values as exactly "10s".
// Note that "10s" does not contain "1s", so shortening the budget fails this
// too -- the assertion is not satisfiable by a substring accident.
func TestForcedExitMessagesNameTheirBudgets(t *testing.T) {
	for _, tt := range []struct {
		name   string
		msg    string
		budget time.Duration
	}{
		{"goroutine join", forcedGoroutineExitMsg, shutdownGoroutineBudget},
		{"archive join", forcedArchiveExitMsg, archiveDrainBudget},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(tt.msg, tt.budget.String()) {
				t.Fatalf("the %s forced-exit line does not name its own budget: message %q, budget %s", tt.name, tt.msg, tt.budget)
			}
		})
	}
}
