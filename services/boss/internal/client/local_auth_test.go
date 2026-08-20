package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/socketauth"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// authTestDaemon is a minimal DaemonService that answers ListRepos (the RPC
// LocalClient.Ping uses) with an empty success response. Everything else is
// Unimplemented.
type authTestDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
}

func (authTestDaemon) ListRepos(context.Context, *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	return connect.NewResponse(&pb.ListReposResponse{}), nil
}

// startAuthDaemon serves authTestDaemon on a Unix socket, gated by the
// socketauth server interceptor for the token co-located with socketPath.
func startAuthDaemon(t *testing.T) string {
	t.Helper()
	// Use /tmp directly — t.TempDir() can exceed the 104-char macOS Unix socket limit.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("boss-client-auth-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(socketPath)

	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(
		authTestDaemon{},
		connect.WithInterceptors(socketauth.NewServerInterceptor(token)),
	)
	mux.Handle(path, handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketauth.TokenPath(socketPath))
	})
	return socketPath
}

func TestNewLocal_AttachesTokenWhenPresent(t *testing.T) {
	socketPath := startAuthDaemon(t)

	c := NewLocal(socketPath)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("authenticated Ping failed: %v", err)
	}
}

func TestNewLocal_MissingTokenGivesClearError(t *testing.T) {
	socketPath := startAuthDaemon(t)

	// Remove the co-located token so the client attaches no credential and the
	// daemon rejects it — the missing-token path a stale/old boss would hit.
	if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	c := NewLocal(socketPath)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded without a token, want a clear error")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("err = %v, want a clear daemon-rejected error mentioning 'daemon'", err)
	}
}

// TestNewLocal_MissingTokenClearErrorOnNonPingRPC proves the friendly mapping is
// applied to every unary RPC (via errMapInterceptor), not only Ping — the TUI/CLI
// first contact the daemon through ListRepos/ListSessions, not Ping.
func TestNewLocal_MissingTokenClearErrorOnNonPingRPC(t *testing.T) {
	socketPath := startAuthDaemon(t)
	if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	c := NewLocal(socketPath)
	_, err := c.ListRepos(context.Background())
	if err == nil {
		t.Fatal("ListRepos succeeded without a token, want a clear error")
	}
	if !strings.Contains(err.Error(), "daemon rejected this client") {
		t.Fatalf("err = %v, want the mapped daemon-rejected error", err)
	}
}

// TestNewLocal_MissingTokenClearErrorOnStream proves the friendly mapping also
// covers streaming RPCs (surfaced via the stream's Err()), not just unary — a
// stale-token client opening a stream must get the actionable message too.
func TestNewLocal_MissingTokenClearErrorOnStream(t *testing.T) {
	socketPath := startAuthDaemon(t)
	if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	c := NewLocal(socketPath)
	stream, err := c.AttachSession(context.Background(), "does-not-exist")
	if err == nil {
		// Stream-open auth surfaces on the first Receive/Err.
		stream.Receive()
		err = stream.Err()
		_ = stream.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "daemon rejected this client") {
		t.Fatalf("err = %v, want the mapped daemon-rejected error on the stream path", err)
	}
}

// TestNewLocalWithToken_ValidTokenReturnsClient guards the constructor's basic
// shape: a non-nil *LocalClient with socketPath set to exactly what was passed
// in (not one resolved/rewritten from a co-located token file, since a
// forwarded socket has none).
func TestNewLocalWithToken_ValidTokenReturnsClient(t *testing.T) {
	c := NewLocalWithToken("/tmp/does-not-need-to-exist.sock", "some-token")
	if c == nil {
		t.Fatal("NewLocalWithToken returned nil")
	}
	if c.socketPath != "/tmp/does-not-need-to-exist.sock" {
		t.Fatalf("socketPath = %q, want %q", c.socketPath, "/tmp/does-not-need-to-exist.sock")
	}
}

// TestNewLocalWithToken_EmptyTokenStillBuildsClient proves an empty token
// behaves like a missing co-located token file: the client is still built
// (never nil, never panics) and only fails later at RPC time.
func TestNewLocalWithToken_EmptyTokenStillBuildsClient(t *testing.T) {
	c := NewLocalWithToken("/tmp/does-not-need-to-exist.sock", "")
	if c == nil {
		t.Fatal("NewLocalWithToken(\"\") returned nil")
	}
}

// TestNewLocal_RegressionBothTokenPresenceCases is the refactor's regression
// guard: NewLocal must keep returning a non-nil client whether or not a
// bossd.token file sits beside the socket, exactly as before the
// newLocalClient extraction.
func TestNewLocal_RegressionBothTokenPresenceCases(t *testing.T) {
	t.Run("token present", func(t *testing.T) {
		socketPath := startAuthDaemon(t) // startAuthDaemon calls LoadOrCreateToken
		c := NewLocal(socketPath)
		if c == nil {
			t.Fatal("NewLocal returned nil with a valid co-located token")
		}
	})

	t.Run("token absent", func(t *testing.T) {
		socketPath := startAuthDaemon(t)
		if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil {
			t.Fatalf("remove token: %v", err)
		}
		c := NewLocal(socketPath)
		if c == nil {
			t.Fatal("NewLocal returned nil with no co-located token")
		}
	})
}

// recordingAuthDaemon is like authTestDaemon but records the Authorization
// header seen on the inbound ListRepos call, so tests can assert exactly what
// NewLocalWithToken attached (or didn't) without depending on server-side
// token validation.
type recordingAuthDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	mu     sync.Mutex
	header string
	seen   bool
}

func (d *recordingAuthDaemon) ListRepos(ctx context.Context, req *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	d.mu.Lock()
	d.header = req.Header().Get(socketauth.AuthHeader)
	d.seen = true
	d.mu.Unlock()
	return connect.NewResponse(&pb.ListReposResponse{}), nil
}

func (d *recordingAuthDaemon) snapshot() (header string, seen bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.header, d.seen
}

// startRecordingDaemon serves recordingAuthDaemon on a Unix socket with NO
// auth interceptor of its own — it exists purely to observe what the client
// sends, not to enforce anything server-side.
func startRecordingDaemon(t *testing.T) (socketPath string, daemon *recordingAuthDaemon) {
	t.Helper()
	socketPath = filepath.Join("/tmp", fmt.Sprintf("boss-client-authrec-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(socketPath)

	daemon = &recordingAuthDaemon{}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(daemon)
	mux.Handle(path, handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(socketPath)
	})
	return socketPath, daemon
}

// TestNewLocalWithToken_AttachesExplicitTokenAsAuthorizationHeader is the
// behavioural proof required by BOS-713: the token passed explicitly to
// NewLocalWithToken (not one read from beside the socket, since a forwarded
// socket has none) is what actually reaches the wire as the Authorization
// header, in the "Bearer <token>" shape socketauth.NewClientInterceptor
// produces.
func TestNewLocalWithToken_AttachesExplicitTokenAsAuthorizationHeader(t *testing.T) {
	socketPath, daemon := startRecordingDaemon(t)

	c := NewLocalWithToken(socketPath, "explicit-remote-token")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	header, seen := daemon.snapshot()
	if !seen {
		t.Fatal("daemon never observed the ListRepos call")
	}
	want := "Bearer explicit-remote-token"
	if header != want {
		t.Fatalf("Authorization header = %q, want %q", header, want)
	}
}

// TestNewLocalWithToken_EmptyTokenSendsNoAuthorizationHeader is the negative
// half of the behavioural proof: an empty token must not synthesize a
// "Bearer " header (which validHeader would reject anyway, but the client
// should not even attach the auth interceptor when there's nothing to send).
func TestNewLocalWithToken_EmptyTokenSendsNoAuthorizationHeader(t *testing.T) {
	socketPath, daemon := startRecordingDaemon(t)

	c := NewLocalWithToken(socketPath, "")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	header, seen := daemon.snapshot()
	if !seen {
		t.Fatal("daemon never observed the ListRepos call")
	}
	if header != "" {
		t.Fatalf("Authorization header = %q, want empty (no auth interceptor attached)", header)
	}
}

// TestNewLocalWithToken_TokenNeverAppearsInRPCError proves the explicit token
// is never leaked into an error string: a failed RPC against a daemon that
// actually enforces auth (and rejects this token) must still produce an error
// message that never contains the raw token value.
func TestNewLocalWithToken_TokenNeverAppearsInRPCError(t *testing.T) {
	socketPath := startAuthDaemon(t) // enforces the real co-located token, so ours is wrong

	const secretToken = "super-secret-explicit-token-should-not-leak"
	c := NewLocalWithToken(socketPath, secretToken)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded with a token the daemon never issued, want an error")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error leaked the explicit token: %v", err)
	}
}

// --- GetAuthState (BOS-944) ---

// authStateDaemon answers GetAuthState with a canned response or a canned
// error. Everything else stays Unimplemented, so the test exercises exactly
// the one procedure.
type authStateDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	resp *pb.GetAuthStateResponse
	err  error
}

func (d *authStateDaemon) GetAuthState(context.Context, *connect.Request[pb.GetAuthStateRequest]) (*connect.Response[pb.GetAuthStateResponse], error) {
	if d.err != nil {
		return nil, d.err
	}
	return connect.NewResponse(d.resp), nil
}

// startAuthStateDaemon serves the supplied handler over a token-gated Unix
// socket, the same shape a real bossd presents, and returns the socket path.
func startAuthStateDaemon(t *testing.T, daemon *authStateDaemon) string {
	t.Helper()
	socketPath := filepath.Join("/tmp", fmt.Sprintf("boss-client-authstate-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(socketPath)

	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(
		daemon,
		connect.WithInterceptors(socketauth.NewServerInterceptor(token)),
	)
	mux.Handle(path, handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketauth.TokenPath(socketPath))
	})
	return socketPath
}

// TestLocalClient_GetAuthStateRoundTrips proves every field survives the wire
// unchanged. The doctor renders these verbatim, so a dropped field is a wrong
// diagnosis rather than a missing one.
func TestLocalClient_GetAuthStateRoundTrips(t *testing.T) {
	failingSince := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	registeredAt := time.Date(2026, 8, 19, 9, 15, 0, 0, time.UTC)
	socketPath := startAuthStateDaemon(t, &authStateDaemon{resp: &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		NeedsLogin:         true,
		ReloginReason:      "refresh_outcome_unknown",
		LastRegisteredAt:   timestamppb.New(registeredAt),
		UpstreamConnected:  false,
		AuthFailingSince:   timestamppb.New(failingSince),
	}})

	resp, err := NewLocal(socketPath).GetAuthState(context.Background())
	if err != nil {
		t.Fatalf("GetAuthState() error = %v", err)
	}
	if !resp.GetUpstreamConfigured() || !resp.GetNeedsLogin() {
		t.Errorf("upstream_configured=%t needs_login=%t, want true/true", resp.GetUpstreamConfigured(), resp.GetNeedsLogin())
	}
	if got := resp.GetReloginReason(); got != "refresh_outcome_unknown" {
		t.Errorf("relogin_reason = %q, want %q", got, "refresh_outcome_unknown")
	}
	if resp.GetUpstreamConnected() {
		t.Error("upstream_connected = true, want false")
	}
	if got := resp.GetLastRegisteredAt().AsTime(); !got.Equal(registeredAt) {
		t.Errorf("last_registered_at = %v, want %v", got, registeredAt)
	}
	if got := resp.GetAuthFailingSince().AsTime(); !got.Equal(failingSince) {
		t.Errorf("auth_failing_since = %v, want %v", got, failingSince)
	}
}

// A local-only daemon answers with upstream_configured=false and no error.
func TestLocalClient_GetAuthStateUnconfiguredDaemon(t *testing.T) {
	socketPath := startAuthStateDaemon(t, &authStateDaemon{resp: &pb.GetAuthStateResponse{}})

	resp, err := NewLocal(socketPath).GetAuthState(context.Background())
	if err != nil {
		t.Fatalf("GetAuthState() error = %v", err)
	}
	if resp.GetUpstreamConfigured() {
		t.Error("upstream_configured = true, want false")
	}
	if resp.GetAuthFailingSince() != nil {
		t.Errorf("auth_failing_since = %v, want unset", resp.GetAuthFailingSince())
	}
}

// A daemon predating this RPC answers CodeUnimplemented. The code must survive
// the client's error mapping intact, because `boss daemon doctor` classifies on
// exactly that code to say "upgrade the daemon" rather than "auth is broken".
func TestLocalClient_GetAuthStateOlderDaemonSurfacesUnimplemented(t *testing.T) {
	// The canned error is exactly what the embedded Unimplemented handler
	// would return, which is what an older daemon binary answers. It is stated
	// explicitly rather than left to the embedding so the assertion below
	// cannot start passing for some other reason.
	socketPath := startAuthStateDaemon(t, &authStateDaemon{
		err: connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented")),
	})

	_, err := NewLocal(socketPath).GetAuthState(context.Background())
	if err == nil {
		t.Fatal("GetAuthState() error = nil, want CodeUnimplemented")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("connect.CodeOf(err) = %v, want %v", got, connect.CodeUnimplemented)
	}
}
