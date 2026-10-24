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
	started        chan struct{}
	release        chan struct{}
	err            error
	sawCtx         atomic.Value // context.Context observed inside StartSession
	writeTo        bool         // when true, write a line to opts.SetupOutput
	panicNow       bool         // when true, panic instead of returning
	resurrectCalls atomic.Int32
	setupErr       error
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

func TestBootstrapLookupTracksSettlement(t *testing.T) {
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{})}
	runner := NewBootstrapRunner(t.Context(), starter, zerolog.Nop())
	bs := runner.Start("session", StartSessionOpts{}, BootstrapHooks{})
	defer func() {
		select {
		case <-starter.release:
		default:
			close(starter.release)
		}
		<-bs.Done()
	}()
	<-starter.started
	if got, ok := runner.Lookup("session"); !ok || got != bs {
		t.Fatal("in-flight bootstrap missing")
	}
	close(starter.release)
	<-bs.Done()
	if _, ok := runner.Lookup("session"); ok {
		t.Fatal("settled bootstrap retained")
	}
}

func TestBootstrapRemovalPreservesReplacement(t *testing.T) {
	starter := &fakeStarter{started: make(chan struct{}), release: make(chan struct{})}
	runner := NewBootstrapRunner(t.Context(), starter, zerolog.Nop())
	bs := runner.Start("session", StartSessionOpts{}, BootstrapHooks{})
	defer func() {
		select {
		case <-starter.release:
		default:
			close(starter.release)
		}
		<-bs.Done()
	}()
	<-starter.started
	replacement := &Bootstrap{sessionID: "session"}
	runner.mu.Lock()
	runner.active["session"] = replacement
	runner.mu.Unlock()
	close(starter.release)
	<-bs.Done()
	if got, ok := runner.Lookup("session"); !ok || got != replacement {
		t.Fatal("old bootstrap removed its replacement")
	}
}

func (f *fakeStarter) ResurrectSession(ctx context.Context, id string, opts ResurrectSessionOpts) (*ResurrectSessionResult, error) {
	f.resurrectCalls.Add(1)
	err := f.StartSession(ctx, id, StartSessionOpts{SetupOutput: opts.SetupOutput})
	return &ResurrectSessionResult{SetupErr: f.setupErr}, err
}

func TestResurrectBootstrapJoinContextAndSettlement(t *testing.T) {
	daemonCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &fakeStarter{started: make(chan struct{}), release: make(chan struct{}), writeTo: true, setupErr: errors.New("nonfatal setup")}
	r := NewBootstrapRunner(daemonCtx, f, zerolog.Nop())
	var settled atomic.Int32
	bs, err := r.StartResurrect("session", ResurrectSessionOpts{}, ResurrectHooks{OnSettled: func(ctx context.Context, id string, result *ResurrectSessionResult, err error) {
		if ctx.Err() != nil || id != "session" || result.SetupErr != f.setupErr || err != nil {
			t.Errorf("unexpected settlement: %s %v %v", id, result, err)
		}
		settled.Add(1)
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	joined, err := r.StartResurrect("session", ResurrectSessionOpts{}, ResurrectHooks{})
	if err != nil || joined != bs {
		t.Fatalf("join = %p, %v; want %p", joined, err, bs)
	}
	lines, unsubscribe := bs.Subscribe()
	defer unsubscribe()
	if line := <-lines; line != "bootstrapping" {
		t.Fatalf("replay = %q", line)
	}
	cancel()
	observed, _ := f.sawCtx.Load().(context.Context)
	select {
	case <-observed.Done():
	case <-time.After(time.Second):
		t.Fatal("daemon cancellation not inherited")
	}
	close(f.release)
	<-bs.Done()
	if f.resurrectCalls.Load() != 1 || settled.Load() != 1 || bs.ResurrectResult().SetupErr != f.setupErr {
		t.Fatal("join or settlement lost result")
	}
	if _, ok := r.Lookup("session"); ok {
		t.Fatal("settled bootstrap retained")
	}
}

func TestResurrectBootstrapConflictsWithCreate(t *testing.T) {
	f := &fakeStarter{started: make(chan struct{}), release: make(chan struct{})}
	r := NewBootstrapRunner(t.Context(), f, zerolog.Nop())
	bs := r.Start("session", StartSessionOpts{}, BootstrapHooks{})
	defer func() { close(f.release); <-bs.Done() }()
	<-f.started
	if _, err := r.StartResurrect("session", ResurrectSessionOpts{}, ResurrectHooks{}); !errors.Is(err, ErrBootstrapInFlight) {
		t.Fatalf("conflict = %v", err)
	}
	if f.resurrectCalls.Load() != 0 {
		t.Fatal("conflicting resurrect ran")
	}
}

func TestResurrectBootstrapPanicSettles(t *testing.T) {
	f := &fakeStarter{started: make(chan struct{}), panicNow: true}
	r := NewBootstrapRunner(t.Context(), f, zerolog.Nop())
	var settled atomic.Int32
	bs, err := r.StartResurrect("session", ResurrectSessionOpts{}, ResurrectHooks{OnSettled: func(_ context.Context, _ string, _ *ResurrectSessionResult, err error) {
		if !errors.Is(err, ErrBootstrapPanicked) {
			t.Errorf("settlement error = %v", err)
		}
		settled.Add(1)
	}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-bs.Done():
	case <-time.After(time.Second):
		t.Fatal("panic never settled")
	}
	if !errors.Is(bs.Err(), ErrBootstrapPanicked) || settled.Load() != 1 {
		t.Fatal("panic reported success")
	}
	if _, ok := r.Lookup("session"); ok {
		t.Fatal("panic retained registry entry")
	}
}
