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

func TestPollTasksRPCTimeoutIs90s(t *testing.T) {
	t.Parallel()
	// PollTasks needs a looser ceiling than the default: a single poll
	// recursively shells out to gh per PR/issue, and 30s was insufficient
	// for repos with 20+ open dependabot PRs. Must remain strictly below
	// the orchestrator's 2-minute poll interval to avoid overlapping polls.
	if pollTasksRPCTimeout != 90*time.Second {
		t.Fatalf("pollTasksRPCTimeout = %v, want 90s", pollTasksRPCTimeout)
	}
	if pollTasksRPCTimeout >= 2*time.Minute {
		t.Fatalf("pollTasksRPCTimeout (%v) must stay below the 2m orchestrator poll interval", pollTasksRPCTimeout)
	}
}

// claudeInitProbeTimeoutMirror mirrors claudeInitProbeTimeout from
// plugins/bossd-plugin-claude/mcpsurface.go. It is DUPLICATED rather than
// imported because plugin binaries are separate modules that must not be
// depended on from the host (and must not depend on this package either — see
// CLAUDE.md, "module boundaries"); the plugin side keeps its own mirror of
// defaultPluginRPCTimeout honest in TestClaudeInitProbeTimeoutStaysBelowHostCeiling.
const claudeInitProbeTimeoutMirror = 20 * time.Second

// pluginProbeTimeoutMargin is the headroom the plugin's own deadline must leave
// beneath this package's ceiling, for gRPC transport and response encoding
// after the probe gives up. Mirrored plugin-side as hostProbeTimeoutMargin.
const pluginProbeTimeoutMargin = 10 * time.Second

// TestDefaultPluginRPCTimeoutClearsPluginProbeCeiling is the mirror-side half of
// the DescribeMCPSurface in-band-probe_error guard. Lowering
// defaultPluginRPCTimeout without lowering the claude plugin's own probe
// deadline would make the HOST deadline fire first on a slow claude cold start,
// so the RPC would fail with DeadlineExceeded instead of returning the
// probe_error + empty servers answer BOS-867 requires. Raising the plugin side
// alone is caught by the test named above, in the plugin's own package.
func TestDefaultPluginRPCTimeoutClearsPluginProbeCeiling(t *testing.T) {
	if defaultPluginRPCTimeout < claudeInitProbeTimeoutMirror+pluginProbeTimeoutMargin {
		t.Fatalf(
			"defaultPluginRPCTimeout = %s, but the claude plugin's claudeInitProbeTimeout is %s "+
				"(plugins/bossd-plugin-claude/mcpsurface.go) and must fire first with %s of margin. "+
				"At or below that sum the host deadline wins and DescribeMCPSurface returns a gRPC "+
				"DeadlineExceeded instead of the in-band probe_error contract. Raise this ceiling, or "+
				"lower claudeInitProbeTimeout there and update claudeInitProbeTimeoutMirror here.",
			defaultPluginRPCTimeout, claudeInitProbeTimeoutMirror, pluginProbeTimeoutMargin,
		)
	}
}
