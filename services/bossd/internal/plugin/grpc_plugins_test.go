package plugin

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// newBlockingGRPCConn spins up an in-memory gRPC server whose
// UnknownServiceHandler blocks until ctx is cancelled, so timeout tests can
// observe cancellation without a real plugin subprocess. The bossanovav1
// package generates only connect-rpc stubs (not standard grpc), so there is
// no standard server-side handler to register; UnknownServiceHandler matches
// any method path including the plugin service paths.
func newBlockingGRPCConn(t *testing.T) (*grpc.ClientConn, func()) {
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
	return conn, func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
}

func TestInvokePluginUnaryAppliesTimeoutCeiling(t *testing.T) {
	t.Parallel()
	conn, cleanup := newBlockingGRPCConn(t)
	defer cleanup()

	// Use the *WithTimeout variant with a short bound to exercise the timeout
	// path in under a second. This proves the ceiling is actually applied —
	// invokePluginUnary itself uses the production 30s default.
	req := &bossanovav1.WorkflowServiceGetInfoRequest{}
	resp := &bossanovav1.WorkflowServiceGetInfoResponse{}
	start := time.Now()
	err := invokePluginUnaryWithTimeout(context.Background(), conn, 100*time.Millisecond, "/bossanova.v1.WorkflowService/GetInfo", req, resp)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout not applied, elapsed %v", elapsed)
	}
	if !strings.Contains(err.Error(), "DeadlineExceeded") &&
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected deadline-exceeded, got %v", err)
	}
}

func TestInvokePluginUnaryHonorsCallerDeadline(t *testing.T) {
	t.Parallel()
	conn, cleanup := newBlockingGRPCConn(t)
	defer cleanup()

	// Default timeout is 30s; caller's 100ms ctx must still win.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req := &bossanovav1.WorkflowServiceGetInfoRequest{}
	resp := &bossanovav1.WorkflowServiceGetInfoResponse{}
	start := time.Now()
	err := invokePluginUnary(ctx, conn, "/bossanova.v1.WorkflowService/GetInfo", req, resp)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > time.Second {
		t.Fatalf("caller deadline ignored; elapsed %v", elapsed)
	}
}

func TestDefaultPluginRPCTimeoutIs30s(t *testing.T) {
	t.Parallel()
	// Pins the production default. Flight Leg 3 handoff calls out the 30s
	// choice for human review against streaming RPCs — changing this value
	// deliberately should update that review context.
	if defaultPluginRPCTimeout != 30*time.Second {
		t.Fatalf("defaultPluginRPCTimeout = %v, want 30s", defaultPluginRPCTimeout)
	}
}
