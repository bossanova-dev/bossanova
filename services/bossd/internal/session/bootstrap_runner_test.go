package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type fakeStarter struct {
	started  chan struct{}
	release  chan struct{}
	err      error
	sawCtx   atomic.Value // context.Context observed inside StartSession
	writeTo  bool         // when true, write a line to opts.SetupOutput
	panicNow bool         // when true, panic instead of returning
}

func (f *fakeStarter) StartSession(ctx context.Context, _ string, opts StartSessionOpts) error {
	f.sawCtx.Store(ctx)
	if f.writeTo && opts.SetupOutput != nil {
		_, _ = io.WriteString(opts.SetupOutput, "bootstrapping\n")
	}
	close(f.started)
	if f.panicNow {
		panic("boom inside StartSession")
	}
	<-f.release
	return f.err
}

// A PANIC inside StartSession must settle as a failure, not vanish.
//
// safego.Go recovers it rather than crashing the daemon, so the goroutine
// unwinds through the runner's defers with no return value. If the hooks were
// called inline after StartSession instead, a panic would skip both: the
// session row and its worktree would be stranded, the per-target lock would be
// held for the daemon's whole lifetime (and the stranded-bootstrap reaper
// could never win it back), and Err() would read nil — so the RPC would tell
// the client the session started.
func TestBootstrapPanicSettlesAsAFailureAndReleasesTheTarget(t *testing.T) {
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{}), panicNow: true}
	runner := NewBootstrapRunner(context.Background(), starter, zerolog.Nop())

	var mu sync.Mutex
	var order []string
	bs := runner.Start("sess-panic", StartSessionOpts{}, BootstrapHooks{
		OnFailure: func(context.Context, string, error) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "cleanup")
		},
		ReleaseTarget: func() {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "release")
		},
	})

	select {
	case <-bs.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a panicking bootstrap never settled: Done stayed open, so every subscriber is wedged")
	}
	if !errors.Is(bs.Err(), ErrBootstrapPanicked) {
		t.Fatalf("Err() = %v, want ErrBootstrapPanicked; a recovered panic reported as success is a false SessionCreated", bs.Err())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "cleanup" || order[1] != "release" {
		t.Fatalf("hook order = %v, want [cleanup release] even on the panic path", order)
	}
}

// The daemon context IS honoured — the bootstrap is daemon-scoped, not
// immortal, so Shutdown still cancels it.
func TestBootstrapDescendsFromTheDaemonContext(t *testing.T) {
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{})}
	daemonCtx, cancelDaemon := context.WithCancel(context.Background())
	defer cancelDaemon()
	runner := NewBootstrapRunner(daemonCtx, starter, zerolog.Nop())

	bs := runner.Start("sess-1", StartSessionOpts{}, BootstrapHooks{})
	<-starter.started
	cancelDaemon()

	observed, _ := starter.sawCtx.Load().(context.Context)
	select {
	case <-observed.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the bootstrap context did not follow the daemon context")
	}
	close(starter.release)
	<-bs.Done()
}

func TestBootstrapReleasesTheTargetLockExactlyOnceOnSuccess(t *testing.T) {
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{})}
	runner := NewBootstrapRunner(context.Background(), starter, zerolog.Nop())

	var releases atomic.Int32
	bs := runner.Start("sess-1", StartSessionOpts{}, BootstrapHooks{
		ReleaseTarget: func() { releases.Add(1) },
	})
	<-starter.started
	if got := releases.Load(); got != 0 {
		t.Fatalf("target lock released %d times before the bootstrap finished; want 0", got)
	}
	close(starter.release)
	<-bs.Done()
	if got := releases.Load(); got != 1 {
		t.Fatalf("target lock released %d times; want exactly 1", got)
	}
}

func TestBootstrapRunsOnFailureOnlyOnErrorAndReleasesAfterIt(t *testing.T) {
	wantErr := errors.New("worktree add failed")
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{}), err: wantErr}
	runner := NewBootstrapRunner(context.Background(), starter, zerolog.Nop())

	var mu sync.Mutex
	var order []string
	bs := runner.Start("sess-1", StartSessionOpts{}, BootstrapHooks{
		ReleaseTarget: func() {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "release")
		},
		OnFailure: func(ctx context.Context, id string, err error) {
			if id != "sess-1" || !errors.Is(err, wantErr) {
				t.Errorf("OnFailure got (%q, %v); want (sess-1, %v)", id, err, wantErr)
			}
			// The cleanup must not inherit a cancelled context: a create
			// abandoned mid-bootstrap still owes its artifact reclaim.
			select {
			case <-ctx.Done():
				t.Error("OnFailure received an already-cancelled context")
			default:
			}
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "cleanup")
		},
	})
	<-starter.started
	close(starter.release)
	<-bs.Done()

	if !errors.Is(bs.Err(), wantErr) {
		t.Fatalf("Err() = %v; want %v", bs.Err(), wantErr)
	}
	// Cleanup BEFORE release: the target lock is what makes an overlapping
	// create for the same target wait for THIS create's cleanup to finish
	// rather than be refused as a duplicate of a row about to be deleted.
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "cleanup" || order[1] != "release" {
		t.Fatalf("hook order = %v; want [cleanup release]", order)
	}
}

func TestBootstrapDoesNotRunOnFailureWhenTheBootstrapSucceeds(t *testing.T) {
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{})}
	runner := NewBootstrapRunner(context.Background(), starter, zerolog.Nop())

	var failures atomic.Int32
	bs := runner.Start("sess-1", StartSessionOpts{}, BootstrapHooks{
		OnFailure: func(context.Context, string, error) { failures.Add(1) },
	})
	<-starter.started
	close(starter.release)
	<-bs.Done()

	if bs.Err() != nil {
		t.Fatalf("Err() = %v; want nil", bs.Err())
	}
	if got := failures.Load(); got != 0 {
		t.Fatalf("OnFailure ran %d times on a successful bootstrap; want 0", got)
	}
}

func TestBootstrapSubscribersSeeSetupOutput(t *testing.T) {
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{}), writeTo: true}
	runner := NewBootstrapRunner(context.Background(), starter, zerolog.Nop())

	bs := runner.Start("sess-1", StartSessionOpts{}, BootstrapHooks{})
	lines, cancel := bs.Subscribe()
	defer cancel()
	<-starter.started

	select {
	case got := <-lines:
		if got != "bootstrapping" {
			t.Fatalf("got %q, want %q", got, "bootstrapping")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never saw the setup line")
	}
	close(starter.release)
	<-bs.Done()
}

// A caller-supplied SetupOutput must be overwritten, not honoured: routing
// bootstrap output anywhere a caller could stall reintroduces the coupling
// this whole change removes.
func TestBootstrapOverwritesACallerSuppliedSetupOutput(t *testing.T) {
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{}), writeTo: true}
	runner := NewBootstrapRunner(context.Background(), starter, zerolog.Nop())

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	bs := runner.Start("sess-1", StartSessionOpts{SetupOutput: pw}, BootstrapHooks{})
	<-starter.started
	close(starter.release)

	// Nothing ever reads pr; if the caller's writer had been honoured, the
	// starter's write would have blocked and Done would never close.
	select {
	case <-bs.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the caller-supplied SetupOutput was honoured and blocked the bootstrap")
	}
}
