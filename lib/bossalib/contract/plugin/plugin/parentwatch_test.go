package plugin

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.uber.org/goleak"
)

// watchTestInterval is short enough that every scenario resolves in
// milliseconds. No test here sleeps waiting for a real orphan.
const watchTestInterval = time.Millisecond

// fakeParent is an injectable os.Getppid replacement whose answer the test
// can change mid-flight, and which records how many times it was sampled.
type fakeParent struct {
	mu     sync.Mutex
	pid    int
	sample int
}

func newFakeParent(pid int) *fakeParent { return &fakeParent{pid: pid} }

func (f *fakeParent) get() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sample++
	return f.pid
}

func (f *fakeParent) set(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pid = pid
}

func (f *fakeParent) samples() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sample
}

// exitRecorder captures the injected exit action: how often it fired and with
// which code. Closing fired on the first call lets a test wait on the branch
// instead of sleeping for it.
type exitRecorder struct {
	mu    sync.Mutex
	calls []int
	fired chan struct{}
	once  sync.Once
}

func newExitRecorder() *exitRecorder {
	return &exitRecorder{fired: make(chan struct{})}
}

func (e *exitRecorder) exit(code int) {
	e.mu.Lock()
	e.calls = append(e.calls, code)
	e.mu.Unlock()
	e.once.Do(func() { close(e.fired) })
}

func (e *exitRecorder) codes() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int(nil), e.calls...)
}

// envFunc builds a lookupEnv stub. Passing present=false models the variable
// being absent from the environment entirely.
func envFunc(value string, present bool) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key != ParentPIDEnvVar {
			return "", false
		}
		return value, present
	}
}

// waitForSamples blocks until the parent-PID source has been polled at least
// n times, so a test can prove the watcher kept running without firing rather
// than merely sleeping past it.
func waitForSamples(t *testing.T, f *fakeParent, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.samples() >= n {
			return
		}
		time.Sleep(watchTestInterval)
	}
	t.Fatalf("parent PID sampled %d times, want at least %d", f.samples(), n)
}

// TestParentWatchExitsWhenParentBecomesInit is the primary firing case: the
// parent dies and the child is reparented to PID 1.
func TestParentWatchExitsWhenParentBecomesInit(t *testing.T) {
	defer goleak.VerifyNone(t)

	parent := newFakeParent(4242)
	rec := newExitRecorder()
	stop := startParentWatch(zerolog.Nop(), envFunc("4242", true), parent.get, watchTestInterval, rec.exit)
	defer stop()

	parent.set(1)

	select {
	case <-rec.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not exit within 2s of the parent being reparented to PID 1")
	}
	if codes := rec.codes(); len(codes) != 1 || codes[0] != 0 {
		t.Fatalf("exit calls = %v, want exactly [0]", codes)
	}
}

// TestParentWatchExitsWhenParentBecomesAnotherPID pins that the trigger is
// "no longer the expected parent", not "reparented to 1" specifically.
func TestParentWatchExitsWhenParentBecomesAnotherPID(t *testing.T) {
	defer goleak.VerifyNone(t)

	parent := newFakeParent(4242)
	rec := newExitRecorder()
	stop := startParentWatch(zerolog.Nop(), envFunc("4242", true), parent.get, watchTestInterval, rec.exit)
	defer stop()

	parent.set(9999)

	select {
	case <-rec.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not exit within 2s of the parent PID changing")
	}
	if codes := rec.codes(); len(codes) != 1 || codes[0] != 0 {
		t.Fatalf("exit calls = %v, want exactly [0]", codes)
	}
}

// TestParentWatchNeverExitsWhileParentMatches is the "healthy daemon" case:
// the destructive action must not fire while nothing has changed.
func TestParentWatchNeverExitsWhileParentMatches(t *testing.T) {
	defer goleak.VerifyNone(t)

	parent := newFakeParent(4242)
	rec := newExitRecorder()
	stop := startParentWatch(zerolog.Nop(), envFunc("4242", true), parent.get, watchTestInterval, rec.exit)

	waitForSamples(t, parent, 50)
	stop()

	if codes := rec.codes(); len(codes) != 0 {
		t.Fatalf("exit calls = %v, want none while the parent still matches", codes)
	}
}

// TestParentWatchExitsOnceNotOncePerPoll pins that a parent that stays gone
// produces a single exit — the watcher returns after firing.
func TestParentWatchExitsOnceNotOncePerPoll(t *testing.T) {
	defer goleak.VerifyNone(t)

	parent := newFakeParent(4242)
	rec := newExitRecorder()
	stop := startParentWatch(zerolog.Nop(), envFunc("4242", true), parent.get, watchTestInterval, rec.exit)
	defer stop()

	parent.set(1)

	select {
	case <-rec.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire")
	}

	// The parent stays gone. Give the watcher many more poll intervals than it
	// would need to fire again if it had not returned.
	samplesAtFire := parent.samples()
	time.Sleep(100 * watchTestInterval)
	if codes := rec.codes(); len(codes) != 1 {
		t.Fatalf("exit calls = %v, want exactly one", codes)
	}
	if got := parent.samples(); got > samplesAtFire+1 {
		t.Fatalf("parent sampled %d more times after firing; watcher did not stop", got-samplesAtFire)
	}
}

// TestParentWatchDoesNotArmWithoutIdentity is the R7 fail-open control: a
// plugin launched by anything other than a stamping bossd keeps running,
// however its parent PID moves.
func TestParentWatchDoesNotArmWithoutIdentity(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, tc := range []struct {
		name    string
		value   string
		present bool
	}{
		{name: "absent", value: "", present: false},
		{name: "empty", value: "", present: true},
		{name: "blank", value: "   ", present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := newFakeParent(4242)
			rec := newExitRecorder()
			stop := startParentWatch(zerolog.Nop(), envFunc(tc.value, tc.present), parent.get, watchTestInterval, rec.exit)

			parent.set(1)
			time.Sleep(50 * watchTestInterval)
			stop()

			if codes := rec.codes(); len(codes) != 0 {
				t.Fatalf("exit calls = %v, want none when no identity is stamped", codes)
			}
			if got := parent.samples(); got != 0 {
				t.Fatalf("parent sampled %d times; nothing should have been armed", got)
			}
		})
	}
}

// TestParentWatchDoesNotArmOnUnparseableIdentity is the same fail-open
// outcome for a malformed stamp, plus the single log line that keeps the
// host-side bug from being silent.
func TestParentWatchDoesNotArmOnUnparseableIdentity(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, value := range []string{"not-a-pid", "12x", "0", "-5"} {
		t.Run(value, func(t *testing.T) {
			var buf bytes.Buffer
			logger := zerolog.New(&buf)

			parent := newFakeParent(4242)
			rec := newExitRecorder()
			stop := startParentWatch(logger, envFunc(value, true), parent.get, watchTestInterval, rec.exit)

			parent.set(1)
			time.Sleep(50 * watchTestInterval)
			stop()

			if codes := rec.codes(); len(codes) != 0 {
				t.Fatalf("exit calls = %v, want none for unparseable identity %q", codes, value)
			}
			if got := parent.samples(); got != 0 {
				t.Fatalf("parent sampled %d times; nothing should have been armed", got)
			}
			if !strings.Contains(buf.String(), "not armed") {
				t.Fatalf("expected a log line explaining the refusal, got %q", buf.String())
			}
		})
	}
}

// TestParentWatchDoesNotArmOnShadowedIdentity is the catastrophic-false-
// positive guard. A stamp that does not describe this process's actual parent
// at startup — stale, foreign, or shadowed by the host's own environment — is
// UNKNOWN, never "parent dead". Without this guard every plugin would exit on
// its first poll.
func TestParentWatchDoesNotArmOnShadowedIdentity(t *testing.T) {
	defer goleak.VerifyNone(t)

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	// Stamp says 4242; this process's actual parent is 7777.
	parent := newFakeParent(7777)
	rec := newExitRecorder()
	stop := startParentWatch(logger, envFunc("4242", true), parent.get, watchTestInterval, rec.exit)

	time.Sleep(50 * watchTestInterval)
	parent.set(1)
	time.Sleep(50 * watchTestInterval)
	stop()

	if codes := rec.codes(); len(codes) != 0 {
		t.Fatalf("exit calls = %v, want none for a stamp that is not this process's parent", codes)
	}
	// Exactly one sample: the arm-time check itself, and nothing after it.
	if got := parent.samples(); got != 1 {
		t.Fatalf("parent sampled %d times, want 1 (the arm-time guard only)", got)
	}
	if !strings.Contains(buf.String(), "not armed") {
		t.Fatalf("expected a log line explaining the refusal, got %q", buf.String())
	}
}

// TestParentWatchArmsWhenIdentityNamesTheLiveParent is the ordinary case: the
// stamp matches os.Getppid at startup, so the watcher arms.
func TestParentWatchArmsWhenIdentityNamesTheLiveParent(t *testing.T) {
	defer goleak.VerifyNone(t)

	parent := newFakeParent(os.Getppid())
	rec := newExitRecorder()
	stop := startParentWatch(
		zerolog.Nop(),
		envFunc(strconv.Itoa(os.Getppid()), true),
		parent.get,
		watchTestInterval,
		rec.exit,
	)

	waitForSamples(t, parent, 5)
	stop()

	if codes := rec.codes(); len(codes) != 0 {
		t.Fatalf("exit calls = %v, want none while the real parent is alive", codes)
	}
}

// TestParentWatchStartUsesProcessDefaults pins that the production entry point
// is wired to the real environment and does not arm in a test binary, whose
// parent identity is never stamped.
func TestParentWatchStartUsesProcessDefaults(t *testing.T) {
	defer goleak.VerifyNone(t)

	t.Setenv(ParentPIDEnvVar, "")
	stop := StartParentWatch(zerolog.Nop())
	stop()
}

// TestParentWatchArmingIsObservable pins the positive signal the orphan
// integration tests read. Arming is otherwise invisible from outside the
// process, which let a control that only names arming in its premise pass
// against a build where the stamp was shadowed and nothing ever armed.
func TestParentWatchArmingIsObservable(t *testing.T) {
	defer goleak.VerifyNone(t)

	t.Run("armed says so", func(t *testing.T) {
		var buf bytes.Buffer
		parent := newFakeParent(4242)
		rec := newExitRecorder()
		stop := startParentWatch(zerolog.New(&buf), envFunc("4242", true), parent.get, watchTestInterval, rec.exit)
		stop()

		if !strings.Contains(buf.String(), ParentWatchArmedMsg) {
			t.Fatalf("armed watchdog logged %q, want it to contain %q", buf.String(), ParentWatchArmedMsg)
		}
	})

	// Every fail-open branch must stay quiet about arming, or the marker
	// stops distinguishing the two states and the controls go vacuous again.
	for _, tc := range []struct {
		name    string
		value   string
		present bool
	}{
		{name: "identity absent", present: false},
		{name: "identity unparseable", value: "not-a-pid", present: true},
		{name: "identity shadowed", value: "999999", present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			parent := newFakeParent(4242)
			rec := newExitRecorder()
			stop := startParentWatch(zerolog.New(&buf), envFunc(tc.value, tc.present), parent.get, watchTestInterval, rec.exit)
			stop()

			if strings.Contains(buf.String(), ParentWatchArmedMsg) {
				t.Fatalf("%s logged the armed marker but did not arm: %s", tc.name, buf.String())
			}
		})
	}
}
