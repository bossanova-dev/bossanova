package hostclient

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// TestMain runs goleak at package level. Per-test goleak.VerifyNone races
// against t.Parallel siblings' goroutines and produces flaky failures; a
// single package-wide check after all tests finish is the idiomatic fix.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// newBufconnDirectClient spins up an in-memory gRPC server whose UnknownServiceHandler
// blocks every RPC (unary and streaming) until its context is cancelled. This lets
// timeout tests observe cancellation without depending on a real daemon. The handler
// covers unknown services because the bossalib module only generates connect-rpc
// stubs — not standard grpc server stubs — so there is nothing to register.
func newBufconnDirectClient(t *testing.T, opts ...ClientOption) (*DirectClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		<-stream.Context().Done()
		return stream.Context().Err()
	}))
	go func() { _ = srv.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return NewDirectClient(conn, opts...), func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
}

func newBufconnEagerClient(t *testing.T, opts ...ClientOption) (*EagerClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		<-stream.Context().Done()
		return stream.Context().Err()
	}))
	go func() { _ = srv.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	c := newEagerClientFromDialer(func() (*grpc.ClientConn, error) { return conn, nil }, zerolog.Nop(), opts...)
	<-c.ready
	if c.err != nil {
		t.Fatalf("eager dial: %v", c.err)
	}
	return c, func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
}

func TestDirectClientAppliesDefaultTimeout(t *testing.T) {
	t.Parallel()
	c, cleanup := newBufconnDirectClient(t, WithTimeout(150*time.Millisecond))
	defer cleanup()

	start := time.Now()
	_, err := c.ListSessions(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected fast timeout, elapsed %v", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("expected timeout to approach 150ms, elapsed %v", elapsed)
	}
}

func TestDirectClientHonorsCallerDeadline(t *testing.T) {
	t.Parallel()
	// Default client timeout is 30s; the caller's 100ms deadline must still win.
	c, cleanup := newBufconnDirectClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.ListSessions(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > time.Second {
		t.Fatalf("caller deadline ignored; elapsed %v", elapsed)
	}
}

func TestNewDirectClientDefaultsTimeout(t *testing.T) {
	t.Parallel()
	c := NewDirectClient(nil)
	if c.defaultRPCTimeout != DefaultRPCTimeout {
		t.Fatalf("default timeout = %v, want %v", c.defaultRPCTimeout, DefaultRPCTimeout)
	}
}

func TestWithTimeoutOverridesDefault(t *testing.T) {
	t.Parallel()
	c := NewDirectClient(nil, WithTimeout(5*time.Second))
	if c.defaultRPCTimeout != 5*time.Second {
		t.Fatalf("override ignored: got %v", c.defaultRPCTimeout)
	}
}

func TestWithStartChatRunTimeoutOverridesStartChatRunOnly(t *testing.T) {
	t.Parallel()
	c := NewDirectClient(nil, WithTimeout(5*time.Second), WithStartChatRunTimeout(6*time.Second))
	if c.defaultRPCTimeout != 5*time.Second {
		t.Fatalf("default RPC timeout = %v, want 5s", c.defaultRPCTimeout)
	}
	if c.startChatRunTimeout != 6*time.Second {
		t.Fatalf("StartChatRun timeout = %v, want 6s", c.startChatRunTimeout)
	}
}

func TestResolveOptionsDefault(t *testing.T) {
	t.Parallel()
	if got := resolveOptions(nil).rpcTimeout; got != DefaultRPCTimeout {
		t.Fatalf("default = %v, want %v", got, DefaultRPCTimeout)
	}
	if got := resolveOptions([]ClientOption{WithTimeout(2 * time.Second)}).rpcTimeout; got != 2*time.Second {
		t.Fatalf("override = %v, want 2s", got)
	}
	if got := resolveOptions(nil).startChatRunTimeout; got != StartChatRunRPCTimeout {
		t.Fatalf("default StartChatRun timeout = %v, want %v", got, StartChatRunRPCTimeout)
	}
	for _, d := range []time.Duration{0, -time.Second} {
		if got := resolveOptions([]ClientOption{WithStartChatRunTimeout(d)}).startChatRunTimeout; got != StartChatRunRPCTimeout {
			t.Fatalf("WithStartChatRunTimeout(%v) resolved to %v, want default %v", d, got, StartChatRunRPCTimeout)
		}
	}
}

func TestDialWithTimeoutReturnsConn(t *testing.T) {
	t.Parallel()
	wantConn := &grpc.ClientConn{}
	dial := func() (*grpc.ClientConn, error) { return wantConn, nil }

	got, err := dialWithTimeout(dial, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantConn {
		t.Fatalf("got wrong conn")
	}
}

func TestDialWithTimeoutFiresOnStuckDial(t *testing.T) {
	t.Parallel()
	// Covers the "broker never dispatches" path. The blocker channel is closed
	// at the end so the background goroutine exits before goleak.VerifyNone runs.
	blocker := make(chan struct{})
	defer close(blocker)

	dial := func() (*grpc.ClientConn, error) {
		<-blocker
		return nil, errors.New("unblocked-with-error")
	}

	start := time.Now()
	conn, err := dialWithTimeout(dial, 80*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if conn != nil {
		t.Fatal("expected nil conn on timeout")
	}
	if elapsed > 500*time.Millisecond || elapsed < 50*time.Millisecond {
		t.Fatalf("timeout not within expected range, elapsed %v", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout-out message, got %v", err)
	}
}

func TestNewEagerClientSurfacesDialError(t *testing.T) {
	t.Parallel()
	dial := func() (*grpc.ClientConn, error) {
		return nil, errors.New("broker refused")
	}

	c := newEagerClientFromDialer(dial, zerolog.Nop())
	<-c.ready

	if c.err == nil {
		t.Fatal("expected dial error")
	}
	if !strings.Contains(c.err.Error(), "broker refused") {
		t.Fatalf("expected dial-error surface, got %v", c.err)
	}

	// Any subsequent RPC must return that same error instead of blocking.
	_, err := c.ListSessions(context.Background())
	if err == nil {
		t.Fatal("expected RPC error after failed dial")
	}
}

func TestNewEagerClientWithBrokerIDReturnsNonNil(t *testing.T) {
	t.Parallel()
	// blocker is closed in the defer so the background goroutine exits before
	// goleak.VerifyNone runs.
	blocker := make(chan struct{})
	defer close(blocker)

	dial := func() (*grpc.ClientConn, error) {
		<-blocker
		return nil, errors.New("unblocked-with-error")
	}

	// newEagerClientFromDialer is the testable core; NewEagerClientWithBrokerID
	// delegates to it with broker.Dial(brokerID). We verify here that a non-1
	// broker ID (simulated via dialer) produces a non-nil *EagerClient.
	c := newEagerClientFromDialer(dial, zerolog.Nop())
	if c == nil {
		t.Fatal("expected non-nil EagerClient")
	}
}

func TestBrokerDialTimeoutDefault(t *testing.T) {
	t.Parallel()
	// Pins the production default. If this intentionally changes, the
	// Flight Leg 3 handoff's 10s assertion should be updated to match.
	if brokerDialTimeout != 10*time.Second {
		t.Fatalf("brokerDialTimeout = %v, want 10s", brokerDialTimeout)
	}
}

func TestTimeoutErrorIsDeadlineExceeded(t *testing.T) {
	t.Parallel()
	c, cleanup := newBufconnDirectClient(t, WithTimeout(80*time.Millisecond))
	defer cleanup()

	_, err := c.ListSessions(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	// gRPC typically wraps the deadline as a status code (DeadlineExceeded) rather
	// than the raw context error, so also accept the string representation.
	if !errors.Is(err, context.DeadlineExceeded) &&
		!strings.Contains(err.Error(), "DeadlineExceeded") &&
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

// TestStartChatRunIgnoresClientDefaultTimeout proves the exemption: StartChatRun
// is bounded by StartChatRunRPCTimeout, not by whatever default the client was
// constructed with. An 80ms client default would cut the call off almost
// immediately if StartChatRun still funnelled through the shared clamp.
func TestStartChatRunIgnoresClientDefaultTimeout(t *testing.T) {
	t.Parallel()
	c, cleanup := newBufconnDirectClient(t, WithTimeout(80*time.Millisecond))
	defer cleanup()

	// The caller's deadline is what actually ends this test: waiting out the
	// real 90s ceiling is not a thing a unit test may do.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("StartChatRun returned after %v: the client's 80ms configured default is still clamping it. "+
			"StartChatRun must be bounded by StartChatRunRPCTimeout instead.", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected the caller's 250ms deadline to end the call, elapsed %v", elapsed)
	}
}

// TestStartChatRunHonorsShorterCallerDeadline proves the exemption is a ceiling
// and not a floor: context.WithTimeout never extends a parent, so a caller that
// supplies a tighter deadline still wins.
func TestStartChatRunHonorsShorterCallerDeadline(t *testing.T) {
	t.Parallel()
	c, cleanup := newBufconnDirectClient(t, WithStartChatRunTimeout(600*time.Second))
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Lower bound so the test cannot pass vacuously: a connection that failed
	// instantly would also produce a non-nil err with elapsed ~0, proving
	// nothing about deadline propagation.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("StartChatRun returned after %v, well before the caller's 100ms deadline: "+
			"the call failed for some reason other than the deadline, so this test proved nothing", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected the caller's 100ms deadline to win over StartChatRunRPCTimeout, elapsed %v", elapsed)
	}
}

func TestStartChatRunUsesConfiguredCeiling(t *testing.T) {
	t.Parallel()
	c, cleanup := newBufconnDirectClient(t, WithStartChatRunTimeout(160*time.Millisecond))
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("StartChatRun returned after %v, before the configured 160ms ceiling could be observed", elapsed)
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("StartChatRun returned after %v; configured ceiling was not applied before the caller's 500ms deadline", elapsed)
	}
}

func TestEagerStartChatRunForwardsConfiguredCeiling(t *testing.T) {
	t.Parallel()
	c, cleanup := newBufconnEagerClient(t, WithStartChatRunTimeout(160*time.Millisecond))
	defer cleanup()

	if c.inner.startChatRunTimeout != 160*time.Millisecond {
		t.Fatalf("eager inner StartChatRun timeout = %v, want 160ms", c.inner.startChatRunTimeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("StartChatRun returned after %v, before the configured 160ms ceiling could be observed", elapsed)
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("StartChatRun returned after %v; eager client did not forward the configured ceiling", elapsed)
	}
}

func TestStartChatRunRPCTimeoutDefault(t *testing.T) {
	t.Parallel()
	if StartChatRunRPCTimeout != 90*time.Second {
		t.Fatalf("StartChatRunRPCTimeout = %v, want 90s", StartChatRunRPCTimeout)
	}
	// Sanity: the exemption has to actually be looser than what it exempts from.
	if StartChatRunRPCTimeout <= DefaultRPCTimeout {
		t.Fatalf("StartChatRunRPCTimeout (%v) must exceed DefaultRPCTimeout (%v), "+
			"otherwise exempting StartChatRun from the shared clamp buys nothing",
			StartChatRunRPCTimeout, DefaultRPCTimeout)
	}
}
