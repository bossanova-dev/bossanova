package detach

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// errBoom is an ordinary flight failure, used to prove errors come back verbatim.
var errBoom = errors.New("boom")

type ctxKey string

const probeKey ctxKey = "probe"

func TestFlightRecoversAPanicIntoTheFlightError(t *testing.T) {
	t.Parallel()
	// The whole point of the package: without the in-body recover, singleflight
	// re-raises this on a bare goroutine and the TEST BINARY dies rather than
	// failing. Reaching the assertion at all is half the evidence.
	_, err := Flight(context.Background(), &Group{}, zerolog.Nop(), "k", time.Second,
		func(context.Context) (int, error) { panic("kaboom in the body") })
	if err == nil {
		t.Fatal("a panicking flight body returned no error; the panic was swallowed rather than converted")
	}
	if got := err.Error(); got != "detached flight panicked: kaboom in the body" {
		t.Errorf("error = %q, want the panic value carried verbatim so the caller can still classify and log it", got)
	}
}

func TestFlightRefusesAnEmptyKeyWithoutRunningTheBody(t *testing.T) {
	t.Parallel()
	var ran atomic.Bool
	_, err := Flight(context.Background(), &Group{}, zerolog.Nop(), "", time.Second,
		func(context.Context) (int, error) { ran.Store(true); return 1, nil })
	if !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("error = %v, want ErrEmptyKey — an empty key coalesces every flight of the group into one", err)
	}
	if ran.Load() {
		t.Error("the body ran on an empty key; the refusal must happen before any flight is seeded")
	}
}

func TestFlightRefusesAnAlreadyDeadCallerWithoutSeedingAFlight(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
		want error
	}{
		{
			name: "cancelled",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: context.Canceled,
		},
		{
			name: "deadline exceeded",
			ctx: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran atomic.Bool
			_, err := Flight(tc.ctx(), &Group{}, zerolog.Nop(), "k", time.Second,
				func(context.Context) (int, error) { ran.Store(true); return 1, nil })
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v returned verbatim so the caller can split Canceled from DeadlineExceeded", err, tc.want)
			}
			if ran.Load() {
				t.Error("a caller that had already given up started detached work anyway")
			}
		})
	}
}

// attachCounter returns a WithAttachHook option and the channel it reports
// attachments on. It is what replaced the "sleep 20ms and hope the joiner got
// there" shape the coalescing tests in this file used to carry: on a loaded
// runner a joiner that missed the window let the leader finish, singleflight
// delete the key, and the joiner start a SECOND flight — failing an assertion
// the code under test never violated (BOS-951).
//
// The send is non-blocking, and deliberately so. This hook runs synchronously
// inside Flight on the caller's own goroutine, so a blocking send on a full
// channel would not fail the test — it would hang the flight with no
// diagnostic until the package timeout killed the binary. The default arm
// makes an undersized capacity a named failure instead.
//
// That arm runs on a flight caller's goroutine, and nothing here waits for a
// hook invocation. It is not alone in touching t from such a goroutine — the
// callers in TestFlightCoalescesOneKeyAndGivesTheJoinerTheLeadersTypedResult
// and TestFlightLeaderAbandoningDoesNotKillTheFlight both call t.Errorf there
// too — but each of those is JOINED before its test returns (wg.Wait,
// <-joinerDone), which is what makes them safe. This arm has no such edge, so
// what keeps it clear of "Log in goroutine after test has completed" is only
// that capacity exceeds the caller count and it is therefore unreachable.
// Raise the caller count without raising the capacity and that panic becomes
// live.
func attachCounter(t *testing.T) (FlightOption, chan string) {
	t.Helper()
	// A constant rather than a parameter because unparam flags a parameter
	// every caller passes the same value for. 4 against a bound of TWO: one
	// signal per caller, and every coalescing test in this file runs exactly
	// one leader and one joiner. A test with more callers than this must raise
	// it — the default arm below says so at the moment it matters.
	const capacity = 4
	attached := make(chan string, capacity)
	return WithAttachHook(func(key string) {
		select {
		case attached <- key:
		default:
			t.Errorf("the attach channel overflowed its capacity of %d; key %q was dropped — "+
				"raise the capacity, a blocking send here would have hung the flight instead",
				cap(attached), key)
		}
	}), attached
}

// waitAttached blocks until want callers have ATTACHED to the flight.
//
// A COUNT, never an ordering: DoChan starts the flight body on a new goroutine,
// so a caller's attach signal and the body's own progress are unordered. Each
// caller signals exactly once — pinned by
// TestFlightAttachHookFiresOncePerAttachedCaller — which is what makes counting
// order-independent.
func waitAttached(t *testing.T, attached <-chan string, want int) {
	t.Helper()
	for i := 0; i < want; i++ {
		select {
		case <-attached:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d callers attached to the flight; the missing caller would have "+
				"started a flight of its own", i, want)
		}
	}
}

func TestFlightCoalescesOneKeyAndGivesTheJoinerTheLeadersTypedResult(t *testing.T) {
	t.Parallel()
	var g Group
	var bodies atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	attach, attached := attachCounter(t)
	// Guarded for the failure path: waitAttached's t.Fatalf runs deferred
	// functions but skips the close below, and these bodies park on a bare
	// <-release with nothing else to wake them.
	releaseAll := sync.OnceFunc(func() { close(release) })
	defer releaseAll()

	results := make(chan string, 2)
	var wg sync.WaitGroup
	// Both callers run the identical body, so which of them becomes leader is
	// singleflight's choice rather than the test's.
	launch := func() {
		defer wg.Done()
		got, err := Flight(context.Background(), &g, zerolog.Nop(), "same", time.Second,
			func(context.Context) (string, error) {
				bodies.Add(1)
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release
				return "leader result", nil
			}, attach)
		if err != nil {
			t.Errorf("flight: %v", err)
		}
		results <- got
	}

	wg.Add(1)
	go launch()

	<-entered // the leader is inside the body, so the next call must join it
	wg.Add(1)
	go launch()

	// Both callers have ATTACHED — observed, not slept for. Releasing earlier
	// would let singleflight delete the key before the joiner arrived, and the
	// joiner would then run a second body and fail the assertion below against
	// correct code.
	waitAttached(t, attached, 2)
	releaseAll()
	wg.Wait()
	close(results)

	if got := bodies.Load(); got != 1 {
		t.Fatalf("body ran %d times for one key, want 1 — the flights did not coalesce", got)
	}
	for got := range results {
		if got != "leader result" {
			t.Errorf("caller received %q, want the leader's typed result", got)
		}
	}
}

func TestFlightDistinctKeysDoNotCoalesce(t *testing.T) {
	t.Parallel()
	var g Group
	var bodies atomic.Int32
	body := func(context.Context) (int, error) { bodies.Add(1); return 7, nil }

	for _, key := range []string{"a", "b"} {
		if _, err := Flight(context.Background(), &g, zerolog.Nop(), key, time.Second, body); err != nil {
			t.Fatalf("flight %q: %v", key, err)
		}
	}
	if got := bodies.Load(); got != 2 {
		t.Fatalf("body ran %d times for two distinct keys, want 2", got)
	}
}

func TestFlightJoinerExpiresOnItsOwnContextWhileTheLeaderRunsOn(t *testing.T) {
	t.Parallel()
	var g Group
	release := make(chan struct{})
	finished := make(chan struct{})
	entered := make(chan struct{})

	go func() {
		_, err := Flight(context.Background(), &g, zerolog.Nop(), "k", time.Second,
			func(context.Context) (int, error) {
				close(entered)
				<-release
				return 42, nil
			})
		if err != nil {
			t.Errorf("leader: %v", err)
		}
		close(finished)
	}()

	<-entered
	joinerCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Flight(joinerCtx, &g, zerolog.Nop(), "k", time.Second,
		func(context.Context) (int, error) { return 0, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("joiner error = %v, want DeadlineExceeded — a joiner must be bounded by its OWN context", err)
	}

	select {
	case <-finished:
		t.Fatal("the leader finished when the joiner expired; a joiner's departure must not end the flight")
	default:
	}
	close(release)
	<-finished
}

func TestFlightLeaderAbandoningDoesNotKillTheFlight(t *testing.T) {
	t.Parallel()
	var g Group
	entered := make(chan struct{})
	release := make(chan struct{})
	body := func(ctx context.Context) (int, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return 99, nil
	}

	attach, attached := attachCounter(t)
	// Guarded for the failure path: waitAttached's t.Fatalf runs deferred
	// functions but skips the close below, and these bodies park on a bare
	// <-release with nothing else to wake them.
	releaseAll := sync.OnceFunc(func() { close(release) })
	defer releaseAll()

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := Flight(leaderCtx, &g, zerolog.Nop(), "k", time.Second, body, attach)
		leaderDone <- err
	}()
	<-entered

	joinerDone := make(chan int, 1)
	go func() {
		got, err := Flight(context.Background(), &g, zerolog.Nop(), "k", time.Second, body, attach)
		if err != nil {
			t.Errorf("joiner: %v", err)
		}
		joinerDone <- got
	}()
	// The joiner must be ATTACHED before the leader hangs up, or this asserts
	// nothing: an unattached joiner runs its own flight, which the leader's
	// departure could never have killed.
	waitAttached(t, attached, 2)

	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want Canceled verbatim", err)
	}

	releaseAll()
	if got := <-joinerDone; got != 99 {
		t.Errorf("joiner result = %d, want 99 — the leader's departure cancelled the detached flight", got)
	}
}

func TestFlightRunsTheBodyToCompletionWhenEveryCallerAbandons(t *testing.T) {
	t.Parallel()
	var g Group
	entered := make(chan struct{})
	completed := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = Flight(ctx, &g, zerolog.Nop(), "k", time.Second, func(bodyCtx context.Context) (int, error) {
			close(entered)
			time.Sleep(50 * time.Millisecond)
			completed <- bodyCtx.Err()
			return 1, nil
		})
	}()
	<-entered
	cancel()

	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("the body's context was %v when every caller had left; the flight must not inherit their cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the body never completed after every caller abandoned it")
	}
}

func TestFlightBodyContextCarriesTheBudgetNotTheCallersDeadline(t *testing.T) {
	t.Parallel()
	const budget = 40 * time.Millisecond
	callerCtx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	var deadline time.Time
	var hadDeadline bool
	if _, err := Flight(callerCtx, &Group{}, zerolog.Nop(), "k", budget,
		func(bodyCtx context.Context) (int, error) {
			deadline, hadDeadline = bodyCtx.Deadline()
			return 1, nil
		}); err != nil {
		t.Fatalf("flight: %v", err)
	}
	if !hadDeadline {
		t.Fatal("the body's context carried no deadline; the budget is the flight's only bound")
	}
	if remaining := time.Until(deadline); remaining > budget || remaining < budget/2 {
		t.Errorf("body deadline is %s away, want ~%s — the body inherited the caller's clock instead of the budget", remaining, budget)
	}
}

func TestFlightBodyKeepsCallerValuesButNotCallerCancellation(t *testing.T) {
	t.Parallel()
	callerCtx, cancel := context.WithCancel(context.WithValue(context.Background(), probeKey, "carried"))
	entered := make(chan struct{})
	type observed struct {
		value  any
		ctxErr error
	}
	seen := make(chan observed, 1)

	go func() {
		_, _ = Flight(callerCtx, &Group{}, zerolog.Nop(), "k", time.Second,
			func(bodyCtx context.Context) (int, error) {
				close(entered)
				time.Sleep(40 * time.Millisecond)
				seen <- observed{value: bodyCtx.Value(probeKey), ctxErr: bodyCtx.Err()}
				return 1, nil
			})
	}()
	<-entered
	cancel()

	got := <-seen
	if got.value != "carried" {
		t.Errorf("body context value = %v, want the caller's request-scoped value to survive WithoutCancel", got.value)
	}
	if got.ctxErr != nil {
		t.Errorf("body context error = %v, want nil — the caller's cancellation must not reach the flight", got.ctxErr)
	}
}

func TestFlightReturnsABodyErrorVerbatim(t *testing.T) {
	t.Parallel()
	got, err := Flight(context.Background(), &Group{}, zerolog.Nop(), "k", time.Second,
		func(context.Context) (string, error) { return "", errBoom })
	if !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want errBoom matched by errors.Is — wrapping here reclassifies every caller sentinel", err)
	}
	if errors.Is(err, ErrResultType) {
		t.Error("a failing body produced a result-type error; the error must be returned before the unwrap")
	}
	if got != "" {
		t.Errorf("result = %q, want the zero value alongside an error", got)
	}
}

func TestFlightReportsADynamicTypeMismatchAsErrResultType(t *testing.T) {
	t.Parallel()
	// Reachable because singleflight carries results as `any`: two callers join
	// the same group and key expecting different T, and the joiner receives the
	// leader's value. The generic parameter does not remove the assertion.
	var g Group
	entered := make(chan struct{})
	release := make(chan struct{})

	attach, attached := attachCounter(t)
	// Guarded for the failure path: waitAttached's t.Fatalf runs deferred
	// functions but skips the close below, and these bodies park on a bare
	// <-release with nothing else to wake them.
	releaseAll := sync.OnceFunc(func() { close(release) })
	defer releaseAll()

	go func() {
		_, err := Flight(context.Background(), &g, zerolog.Nop(), "k", time.Second,
			func(context.Context) (string, error) {
				close(entered)
				<-release
				return "a string", nil
			}, attach)
		if err != nil {
			t.Errorf("leader: %v", err)
		}
	}()
	<-entered

	joined := make(chan error, 1)
	go func() {
		_, err := Flight(context.Background(), &g, zerolog.Nop(), "k", time.Second,
			func(context.Context) (int, error) { return 0, nil }, attach)
		joined <- err
	}()
	// The int caller must have JOINED the string leader's flight; a caller that
	// started its own would get a valid int back and never see ErrResultType,
	// so the assertion below would pass against a broken unwrap.
	waitAttached(t, attached, 2)
	releaseAll()

	if err := <-joined; !errors.Is(err, ErrResultType) {
		t.Fatalf("error = %v, want ErrResultType for a result whose dynamic type is not T", err)
	}
}

func TestCleanupBudgetIsFiveSeconds(t *testing.T) {
	t.Parallel()
	// Pinned because the whole second half of BOS-952 is "this value exists
	// once"; a silent change here would silently retune three former call sites.
	if CleanupBudget != 5*time.Second {
		t.Fatalf("CleanupBudget = %s, want 5s", CleanupBudget)
	}
}

func TestCleanupDetachesFromTheCallerAndBoundsWithTheBudget(t *testing.T) {
	t.Parallel()
	const budget = 40 * time.Millisecond
	callerCtx, cancel := context.WithCancel(context.WithValue(context.Background(), probeKey, "carried"))
	cancel() // the commonest way to reach a cleanup is the context above it dying

	var (
		ran      bool
		ctxErr   error
		value    any
		deadline time.Time
		ok       bool
	)
	Cleanup(callerCtx, budget, func(cleanupCtx context.Context) {
		ran = true
		ctxErr = cleanupCtx.Err()
		value = cleanupCtx.Value(probeKey)
		deadline, ok = cleanupCtx.Deadline()
	})

	if !ran {
		t.Fatal("Cleanup did not run fn for an already-cancelled caller; every converted call site depends on it running")
	}
	if ctxErr != nil {
		t.Errorf("fn's context error = %v, want nil — a cleanup issued on a dead context is refused and the resource leaks", ctxErr)
	}
	if value != "carried" {
		t.Errorf("fn's context value = %v, want the caller's request-scoped values preserved", value)
	}
	if !ok {
		t.Fatal("fn's context carried no deadline; a detached cleanup must not be able to outlive the process")
	}
	if remaining := time.Until(deadline); remaining > budget || remaining < budget/2 {
		t.Errorf("fn's deadline is %s away, want ~%s", remaining, budget)
	}
}

func TestCleanupCancelsTheDerivedContextWhenFnReturns(t *testing.T) {
	t.Parallel()
	var captured context.Context
	Cleanup(context.Background(), time.Hour, func(cleanupCtx context.Context) { captured = cleanupCtx })
	if err := captured.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("derived context error after return = %v, want Canceled — the timeout would otherwise leak until it fires", err)
	}
}

func TestCleanupCancelsTheDerivedContextWhenFnPanics(t *testing.T) {
	t.Parallel()
	var captured context.Context
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("Cleanup swallowed a panic from fn; it recovers nothing and must let it through")
			}
		}()
		Cleanup(context.Background(), time.Hour, func(cleanupCtx context.Context) {
			captured = cleanupCtx
			panic("cleanup exploded")
		})
	}()
	if err := captured.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("derived context error after a panicking fn = %v, want Canceled", err)
	}
}

// TestFlightAttachHookFiresOncePerAttachedCaller pins the seam's arity: the
// hook is per CALLER, not per flight. Every other test in this file now counts
// attach signals to know when a joiner has arrived, so a hook that fired once
// per flight would hang them and one that fired twice per caller would let them
// proceed early — which makes this arity load-bearing rather than incidental.
func TestFlightAttachHookFiresOncePerAttachedCaller(t *testing.T) {
	t.Parallel()
	var g Group
	release := make(chan struct{})
	// OnceFunc rather than a bare defer: the happy path closes release itself,
	// and this exists for the failure path, where a t.Fatalf below would
	// otherwise leave both launch goroutines blocked on <-release for the rest
	// of the run. t.Fatalf runs deferred functions, so the escape is real.
	releaseAll := sync.OnceFunc(func() { close(release) })
	defer releaseAll()
	entered := make(chan struct{}, 1)
	attach, attached := attachCounter(t)

	var wg sync.WaitGroup
	launch := func() {
		defer wg.Done()
		got, err := Flight(context.Background(), &g, zerolog.Nop(), "same", time.Second,
			func(context.Context) (int, error) {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release
				return 7, nil
			}, attach)
		if err != nil {
			t.Errorf("flight: %v", err)
		}
		if got != 7 {
			t.Errorf("flight result = %d, want 7", got)
		}
	}

	wg.Add(1)
	go launch()
	<-entered // the leader is inside the body, so the next call must join it
	wg.Add(1)
	go launch()

	// Drained here rather than through waitAttached so the key can be asserted
	// as well as the count.
	for i := 0; i < 2; i++ {
		select {
		case got := <-attached:
			if got != "same" {
				t.Errorf("attach hook saw key %q, want %q", got, "same")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 callers signalled attachment; the joiner never attached", i)
		}
	}

	releaseAll()
	wg.Wait()
	// Two received above and none left over is the "exactly once per caller"
	// half: a per-flight hook would have produced one and hung the loop.
	if n := len(attached); n != 0 {
		t.Errorf("attach hook fired %d extra times, want exactly one signal per caller", n)
	}
}

func TestFlightAttachHookDoesNotFireOnTheEmptyKeyRefusal(t *testing.T) {
	t.Parallel()
	var fired atomic.Int32
	_, err := Flight(context.Background(), &Group{}, zerolog.Nop(), "", time.Second,
		func(context.Context) (int, error) { return 1, nil },
		WithAttachHook(func(string) { fired.Add(1) }))
	if !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("err = %v, want ErrEmptyKey", err)
	}
	// The refusal returns before DoChan, so there is nothing to have attached
	// TO. A hook that fired here would report an attachment that never happened.
	if got := fired.Load(); got != 0 {
		t.Errorf("attach hook fired %d times on the empty-key refusal, want 0", got)
	}
}

func TestFlightAttachHookDoesNotFireOnTheAlreadyDeadCallerFence(t *testing.T) {
	t.Parallel()
	var fired atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Flight(ctx, &Group{}, zerolog.Nop(), "k", time.Second,
		func(context.Context) (int, error) { return 1, nil },
		WithAttachHook(func(string) { fired.Add(1) }))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Same reason as the empty-key case: the fence returns before DoChan, so no
	// flight was joined.
	if got := fired.Load(); got != 0 {
		t.Errorf("attach hook fired %d times on the dead-caller fence, want 0", got)
	}
}

// TestFlightWithNoOptionsIsUnchanged is the compatibility half: the option is
// variadic precisely so every existing call site keeps compiling and behaving
// as it did, and a nil hook must not be invoked.
func TestFlightWithNoOptionsIsUnchanged(t *testing.T) {
	t.Parallel()
	got, err := Flight(context.Background(), &Group{}, zerolog.Nop(), "k", time.Second,
		func(context.Context) (string, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("flight: %v", err)
	}
	if got != "ok" {
		t.Errorf("flight result = %q, want %q", got, "ok")
	}

	// WithAttachHook(nil) is the same nil-check path a production Server takes
	// when its test-only hook is unset.
	got, err = Flight(context.Background(), &Group{}, zerolog.Nop(), "k", time.Second,
		func(context.Context) (string, error) { return "ok", nil }, WithAttachHook(nil))
	if err != nil {
		t.Fatalf("flight with a nil hook: %v", err)
	}
	if got != "ok" {
		t.Errorf("flight result = %q, want %q", got, "ok")
	}
}
