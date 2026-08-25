package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/socketauth"
)

// wedgedDaemon models the failure BOS-723 exists to bound: a bossd that accepts
// the Unix-socket connection and is answered by the HTTP server, but never
// writes a unary response. ListRepos (the RPC LocalClient.Ping issues) blocks
// until the *client* gives up and cancels the request.
//
// AttachSession is deliberately NOT wedged: it emits one frame immediately and
// a second one after streamGap, which is longer than the (shrunk)
// defaultRPCDeadline. That is the falsifying case for ever setting
// http.Client.Timeout on the shared client — a wall-clock timeout there would
// truncate this stream.
type wedgedDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	streamGap time.Duration
	// release is closed at test cleanup, so the wedged handler cannot outlive
	// the test. It is deliberately NOT the request context: Connect propagates
	// the client's deadline to the server as a timeout header, so a handler that
	// woke on <-ctx.Done() would answer at the very instant the client gives up
	// and the two errors would race — some runs would surface the server's code
	// instead of the client's bound. Blocking on release keeps the daemon silent
	// and the assertion deterministic.
	release chan struct{}
	// respondWith, when non-nil, makes the unary handler answer IMMEDIATELY
	// with that error instead of going quiet. It models the opposite daemon: a
	// perfectly responsive one that happens to fail. See startAnsweringDaemon.
	respondWith error
}

func (d wedgedDaemon) ListRepos(context.Context, *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	if d.respondWith != nil {
		return nil, d.respondWith
	}
	<-d.release
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("wedged daemon never answered"))
}

// ResurrectSession models the BOS-984 shape: a repo setup script that runs
// longer than the client's unary deadline, streaming progress while it works.
// It is deliberately NOT wedged — the daemon here is perfectly healthy, just
// slow, which is exactly the case the old unary RPC misreported as
// deadline_exceeded.
func (d wedgedDaemon) ResurrectSession(ctx context.Context, req *connect.Request[pb.ResurrectSessionRequest], stream *connect.ServerStream[pb.ResurrectSessionResponse]) error {
	if err := stream.Send(&pb.ResurrectSessionResponse{Event: &pb.ResurrectSessionResponse_SetupOutput{
		SetupOutput: &pb.SetupScriptOutput{Text: "installing dependencies"},
	}}); err != nil {
		return err
	}
	select {
	case <-time.After(d.streamGap):
	case <-ctx.Done():
		return ctx.Err()
	}
	return stream.Send(&pb.ResurrectSessionResponse{Event: &pb.ResurrectSessionResponse_SessionResurrected{
		SessionResurrected: &pb.SessionResurrected{Session: &pb.Session{Id: req.Msg.GetId()}},
	}})
}

func (d wedgedDaemon) AttachSession(ctx context.Context, _ *connect.Request[pb.AttachSessionRequest], stream *connect.ServerStream[pb.AttachSessionResponse]) error {
	send := func(text string) error {
		return stream.Send(&pb.AttachSessionResponse{
			Event: &pb.AttachSessionResponse_OutputLine{OutputLine: &pb.OutputLine{Text: text}},
		})
	}
	if err := send("first"); err != nil {
		return err
	}
	select {
	case <-time.After(d.streamGap):
	case <-ctx.Done():
		return ctx.Err()
	}
	return send("second")
}

// startWedgedDaemon serves wedgedDaemon on a Unix socket behind the socketauth
// server interceptor, and returns the socket path plus the co-located token so
// callers can exercise both NewLocal and NewLocalWithToken (the --host path).
//
// It mirrors startAuthDaemon's documented /tmp workaround: t.TempDir() can
// exceed the 104-char macOS Unix socket path limit.
func startWedgedDaemon(t *testing.T, streamGap time.Duration) (socketPath, token string) {
	t.Helper()
	return serveTestDaemon(t, streamGap, nil)
}

// startAnsweringDaemon serves a daemon that is NOT wedged: its unary handler
// answers immediately with respondWith. It exists for the one scene the wedged
// variant cannot express — a responsive daemon returning a
// CodeDeadlineExceeded of its own, from some inner handler deadline. That must
// not be reported as "the daemon did not answer", and it is the only case that
// distinguishes deciding the wedged wording on our own context from deciding it
// on the returned error's code.
func startAnsweringDaemon(t *testing.T, respondWith error) (socketPath, token string) {
	t.Helper()
	return serveTestDaemon(t, time.Second, respondWith)
}

func serveTestDaemon(t *testing.T, streamGap time.Duration, respondWith error) (socketPath, token string) {
	t.Helper()
	socketPath = filepath.Join("/tmp", fmt.Sprintf("boss-client-wedged-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(socketPath)

	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	release := make(chan struct{})
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(
		wedgedDaemon{streamGap: streamGap, release: release, respondWith: respondWith},
		connect.WithInterceptors(socketauth.NewServerInterceptor(token)),
	)
	mux.Handle(path, handler)
	// No WriteTimeout here: the point of these tests is that the *client* bounds
	// the call, so the server must never be the thing that ends it.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		close(release)
		_ = srv.Close()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketauth.TokenPath(socketPath))
	})
	return socketPath, token
}

// shrinkDefaultRPCDeadline swaps the package-level default for a test-sized one
// and restores it afterwards. Tests that call it must not use t.Parallel() —
// they mutate shared package state.
func shrinkDefaultRPCDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	prev := defaultRPCDeadline
	defaultRPCDeadline = d
	t.Cleanup(func() { defaultRPCDeadline = prev })
}

// rpcWatchdogBudget is the wall-clock ceiling every bounded-RPC test allows.
// It sits far above the deadlines those tests shrink defaultRPCDeadline to, so
// it only ever trips on a genuine regression, never on a slow machine.
const rpcWatchdogBudget = 5 * time.Second

// callWithinBudget runs one RPC on its own goroutine and fails the test if it
// has not returned inside rpcWatchdogBudget. Without this, a regression that
// drops the unary bound would hang until the package-wide 10-minute test
// timeout panic instead of naming what broke.
func callWithinBudget(t *testing.T, call func() error) (elapsed time.Duration, err error) {
	t.Helper()
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1) // buffered: the goroutine must never block if we give up
	start := time.Now()
	go func() { done <- result{err: call(), elapsed: time.Since(start)} }()

	select {
	case r := <-done:
		return r.elapsed, r.err
	case <-time.After(rpcWatchdogBudget):
		t.Fatalf("RPC did not return within %v — the unary deadline bound is not being applied", rpcWatchdogBudget)
		return 0, nil
	}
}

// assertWedgedDaemonError checks the actionable wording the ticket requires:
// the error must name the daemon, name the socket path, and point the user at
// a recovery command.
func assertWedgedDaemonError(t *testing.T, err error, socketPath string) {
	t.Helper()
	if err == nil {
		t.Fatal("unary RPC against a wedged daemon returned nil error, want a bounded failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "daemon") {
		t.Errorf("err = %v, want a message naming the daemon", err)
	}
	if !strings.Contains(msg, socketPath) {
		t.Errorf("err = %v, want a message naming the socket path %q", err, socketPath)
	}
	if !strings.Contains(msg, "boss daemon restart") && !strings.Contains(msg, "boss daemon status") {
		t.Errorf("err = %v, want a message mentioning `boss daemon status` or `boss daemon restart`", err)
	}
}

// TestLocalClient_UnaryRPCBoundedAgainstWedgedDaemon is the core regression: a
// daemon that accepts the connection and never answers used to hang boss
// forever. The call must now fail inside the bound.
func TestLocalClient_UnaryRPCBoundedAgainstWedgedDaemon(t *testing.T) {
	shrinkDefaultRPCDeadline(t, 300*time.Millisecond)
	socketPath, _ := startWedgedDaemon(t, time.Second)

	c := NewLocal(socketPath)
	elapsed, err := callWithinBudget(t, func() error {
		return c.Ping(context.Background())
	})

	assertWedgedDaemonError(t, err, socketPath)
	if elapsed < defaultRPCDeadline {
		t.Errorf("Ping returned after %v, before the %v bound — something other than the deadline ended it", elapsed, defaultRPCDeadline)
	}
}

// TestLocalClientWithToken_UnaryRPCBoundedAgainstWedgedDaemon covers the
// --host ssh-forwarded-socket path, which builds its client through
// NewLocalWithToken and so must inherit the same bound and the same wording.
func TestLocalClientWithToken_UnaryRPCBoundedAgainstWedgedDaemon(t *testing.T) {
	shrinkDefaultRPCDeadline(t, 300*time.Millisecond)
	socketPath, token := startWedgedDaemon(t, time.Second)

	c := NewLocalWithToken(socketPath, token)
	elapsed, err := callWithinBudget(t, func() error {
		_, err := c.ListRepos(context.Background())
		return err
	})

	assertWedgedDaemonError(t, err, socketPath)
	if elapsed < defaultRPCDeadline {
		t.Errorf("ListRepos returned after %v, before the %v bound — something other than the deadline ended it", elapsed, defaultRPCDeadline)
	}
}

// TestLocalClient_CallerDeadlineHonouredVerbatim proves the interceptor never
// extends nor shortens a deadline the caller already set.
//
// The two directions are not equally interesting. context.WithTimeout can never
// push a deadline out past its parent's, so the "not extended" half holds even
// without the interceptor's caller-wins check — it is kept as documentation, not
// as a guard. The "not shortened" half is the one that actually falsifies:
// without the check, a caller asking for longer than the default would be cut
// off at the default.
func TestLocalClient_CallerDeadlineHonouredVerbatim(t *testing.T) {
	t.Run("shorter than the default is not extended", func(t *testing.T) {
		shrinkDefaultRPCDeadline(t, 10*time.Second)
		socketPath, _ := startWedgedDaemon(t, time.Second)

		c := NewLocal(socketPath)
		// Start the stopwatch BEFORE the deadline clock so the two share an
		// origin. callWithinBudget takes its own start a moment later, and any
		// pause in between — a GC assist, a deschedule on a loaded machine —
		// makes that reading under-report the real wait by however long the
		// pause was. Measured against a lower bound equal to the WHOLE budget,
		// with no tolerance, that is a false failure waiting to happen: it hit
		// CI at 95.257356ms against this very 100ms deadline. Timers never fire
		// early, so measured from here the bound cannot be undershot unless the
		// call genuinely returned before the caller's deadline.
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_, err := callWithinBudget(t, func() error { return c.Ping(ctx) })
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("Ping with a 100ms deadline against a wedged daemon returned nil error")
		}
		if elapsed >= time.Second {
			t.Errorf("Ping took %v, want ~100ms: the caller's deadline was extended toward the %v default", elapsed, defaultRPCDeadline)
		}
		if elapsed < 100*time.Millisecond {
			t.Errorf("Ping returned after %v, before the caller's own 100ms deadline", elapsed)
		}
	})

	t.Run("longer than the default is not shortened", func(t *testing.T) {
		shrinkDefaultRPCDeadline(t, 200*time.Millisecond)
		socketPath, _ := startWedgedDaemon(t, time.Second)

		c := NewLocal(socketPath)
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer cancel()

		elapsed, err := callWithinBudget(t, func() error { return c.Ping(ctx) })
		if err == nil {
			t.Fatal("Ping with an 800ms deadline against a wedged daemon returned nil error")
		}
		if elapsed < 700*time.Millisecond {
			t.Fatalf("Ping returned after %v, want ~800ms: the caller's deadline was cut short at the %v default", elapsed, defaultRPCDeadline)
		}
	})
}

// TestLocalClient_CallerDeadlineDoesNotGetTheWedgedDaemonWording pins the other
// half of "a caller-supplied deadline always wins": it must not be *reported* as
// this client's bound either.
//
// Several callers set their own, much shorter deadline — `boss repair doctor`
// allows 30s (services/boss/cmd/repair_doctor.go), the MCP commands 2s. If the
// wedged-daemon wording keyed on the deadline code alone, every one of those
// expiries would tell the user to run `boss daemon restart` on a daemon that had
// simply not finished a job inside the caller's own budget. Only an expiry
// deadlineInterceptor imposed itself is evidence the daemon is not answering.
func TestLocalClient_CallerDeadlineDoesNotGetTheWedgedDaemonWording(t *testing.T) {
	shrinkDefaultRPCDeadline(t, 10*time.Second)
	socketPath, _ := startWedgedDaemon(t, time.Second)

	c := NewLocal(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := callWithinBudget(t, func() error { return c.Ping(ctx) })
	if err == nil {
		t.Fatal("Ping with a 100ms caller deadline returned nil error")
	}
	msg := err.Error()
	if strings.Contains(msg, "boss daemon restart") || strings.Contains(msg, "did not answer in time") {
		t.Errorf("err = %v, want the plain transport error: the caller's OWN deadline expired, so blaming the daemon and telling the user to restart it is wrong", err)
	}
}

// TestLocalClient_ServerOwnDeadlineIsNotReportedAsAWedgedDaemon is the
// falsifying test for deciding "this expiry is mine" on the interceptor's own
// context rather than on the returned error's code.
//
// The daemon here answers instantly — it is not wedged at all — but answers
// with a CodeDeadlineExceeded raised by some inner handler deadline of its own.
// A code-based check would tag that as this client's bound and tell the user to
// run `boss daemon restart` on a daemon that replied in milliseconds. Deciding
// on ctx.Err() cannot: our context has not expired, so no marker is attached
// and the caller sees the daemon's own error.
func TestLocalClient_ServerOwnDeadlineIsNotReportedAsAWedgedDaemon(t *testing.T) {
	shrinkDefaultRPCDeadline(t, 10*time.Second)
	socketPath, _ := startAnsweringDaemon(t, connect.NewError(connect.CodeDeadlineExceeded, errors.New("inner handler deadline")))

	c := NewLocal(socketPath)
	elapsed, err := callWithinBudget(t, func() error { return c.Ping(context.Background()) })
	if err == nil {
		t.Fatal("Ping against a daemon returning CodeDeadlineExceeded returned nil error")
	}
	if elapsed > time.Second {
		t.Fatalf("Ping took %v: the daemon answers immediately, so nothing here should have waited on the %v bound", elapsed, defaultRPCDeadline)
	}
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Errorf("connect.CodeOf(%v) = %v, want the daemon's own CodeDeadlineExceeded passed through", err, connect.CodeOf(err))
	}
	msg := err.Error()
	if strings.Contains(msg, "did not answer in time") || strings.Contains(msg, "boss daemon restart") {
		t.Errorf("err = %v, want the daemon's own error: it answered in milliseconds, so telling the user to restart it is a misdiagnosis", err)
	}
}

// TestLocalClient_BoundedErrorUnwrapsToDeadlineExceeded proves the marker the
// wedged-daemon wording keys on stays transparent: callers doing their own
// errors.Is / connect.CodeOf classification must still see a deadline expiry,
// not an opaque wrapper.
func TestLocalClient_BoundedErrorUnwrapsToDeadlineExceeded(t *testing.T) {
	shrinkDefaultRPCDeadline(t, 300*time.Millisecond)
	socketPath, _ := startWedgedDaemon(t, time.Second)

	c := NewLocal(socketPath)
	_, err := callWithinBudget(t, func() error { return c.Ping(context.Background()) })
	if err == nil {
		t.Fatal("Ping against a wedged daemon returned nil error")
	}
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Errorf("connect.CodeOf(%v) = %v, want CodeDeadlineExceeded: the bound marker is not transparent to connect's classifier", err, connect.CodeOf(err))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false: the bound marker is not transparent to the stdlib classifier", err)
	}
}

// TestNewLocalClient_DialContextHonoursItsContext exercises the transport's
// dialer directly, against a socket that really is listening.
//
// It has to go at the dialer directly, because the end-to-end route cannot
// falsify this: net/http checks the request context before it ever dials, so a
// pre-cancelled Ping fails identically whether or not the dialer honours its
// context. Calling DialContext ourselves is what makes the old
// ctx-discarding net.Dial observable — it would return a live connection here.
func TestNewLocalClient_DialContextHonoursItsContext(t *testing.T) {
	socketPath, _ := startWedgedDaemon(t, time.Second)

	c := NewLocal(socketPath)
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("transport has no DialContext; the Unix-socket dialer is not installed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The address arguments are ignored by the closure — it always dials socketPath.
	conn, err := transport.DialContext(ctx, "tcp", "ignored:0")
	if conn != nil {
		_ = conn.Close()
		t.Error("dial connected despite an already-cancelled context; the dialer is discarding it")
	}
	if err == nil {
		t.Fatal("dial with a cancelled context returned nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
}

// TestLocalClient_PreCancelledContextFailsFast is the end-to-end companion: a
// caller who cancels before the RPC gets context.Canceled back through the
// whole client stack, and the error mapper does not rewrite it into the
// wedged-daemon wording (which would misdiagnose the caller's own cancellation).
func TestLocalClient_PreCancelledContextFailsFast(t *testing.T) {
	socketPath, _ := startWedgedDaemon(t, time.Second)

	c := NewLocal(socketPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := callWithinBudget(t, func() error { return c.Ping(ctx) })
	if err == nil {
		t.Fatal("Ping with a pre-cancelled context returned nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	if strings.Contains(err.Error(), "boss daemon restart") {
		t.Errorf("err = %v, want a plain cancellation, not the wedged-daemon message", err)
	}
}

// TestLocalClient_StreamingSurvivesUnaryDeadline is the falsifying test for
// http.Client.Timeout. AttachSession's second frame arrives well after
// defaultRPCDeadline has elapsed; a wall-clock timeout on the shared
// *http.Client would truncate the stream before it lands.
func TestLocalClient_StreamingSurvivesUnaryDeadline(t *testing.T) {
	shrinkDefaultRPCDeadline(t, 200*time.Millisecond)
	socketPath, _ := startWedgedDaemon(t, 600*time.Millisecond)

	c := NewLocal(socketPath)
	stream, err := c.AttachSession(context.Background(), "any-session")
	if err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var texts []string
	for stream.Receive() {
		texts = append(texts, stream.Msg().OutputLine.GetText())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream.Err() = %v, want the stream to run past the unary deadline cleanly", err)
	}
	want := []string{"first", "second"}
	if len(texts) != len(want) || texts[0] != want[0] || texts[1] != want[1] {
		t.Fatalf("received %v, want %v — the stream was truncated by a unary-scoped bound", texts, want)
	}
}

// TestNewLocalClient_HTTPClientTimeoutIsZero guards against a later
// "simplification" that sets http.Client.Timeout: it covers the whole response
// body read, so it would kill CreateSession and AttachSession, the only two
// streaming RPCs on this client.
func TestNewLocalClient_HTTPClientTimeoutIsZero(t *testing.T) {
	c := newLocalClient("/tmp/does-not-need-to-exist.sock", "some-token")
	if c.httpClient == nil {
		t.Fatal("newLocalClient did not retain its *http.Client")
	}
	if c.httpClient.Timeout != 0 {
		t.Fatalf("http.Client.Timeout = %v, want 0: a wall-clock timeout truncates CreateSession/AttachSession", c.httpClient.Timeout)
	}
}

// TestSlowProcedures_KeysAreGeneratedProcedureConstants pins every member of
// the slow-RPC set to a real generated procedure constant, so a renamed RPC
// breaks the build here rather than silently losing its longer bound.
//
// Note what this test can and cannot do. The pinned set is built from the same
// generated constants as the production set, so at runtime it only detects a
// member being added or removed — the shape assertion below (every member is a
// DaemonService procedure path) is the part that is independent of the
// production map. Neither half can catch the set being *incomplete*: review
// caught RecordChat/WakeChat/ArchiveSession/ListAccounts
// missing while this test was green. Membership is a judgement call about the
// handler's work, so when adding a slow unary RPC, add it to slowProcedures
// first and here second.
func TestSlowProcedures_KeysAreGeneratedProcedureConstants(t *testing.T) {
	allowed := map[string]bool{
		bossanovav1connect.DaemonServiceCloneAndRegisterRepoProcedure: true,
		bossanovav1connect.DaemonServiceMergeSessionProcedure:         true,
		bossanovav1connect.DaemonServiceRefreshSessionPRProcedure:     true,
		bossanovav1connect.DaemonServiceListTrackerIssuesProcedure:    true,
		bossanovav1connect.DaemonServiceListRepoPRsProcedure:          true,
		bossanovav1connect.DaemonServiceListAccountsProcedure:         true,
		bossanovav1connect.DaemonServiceRepairDoctorProcedure:         true,
		bossanovav1connect.DaemonServiceTestAccountProcedure:          true,
		bossanovav1connect.DaemonServiceAddAccountProcedure:           true,
		bossanovav1connect.DaemonServiceRefreshAccountProcedure:       true,
		bossanovav1connect.DaemonServiceRunCronJobNowProcedure:        true,
		bossanovav1connect.DaemonServiceSendChatMessageProcedure:      true,
		bossanovav1connect.DaemonServiceSwitchSessionAccountProcedure: true,
		bossanovav1connect.DaemonServiceRecordChatProcedure:           true,
		bossanovav1connect.DaemonServiceWakeChatProcedure:             true,
		bossanovav1connect.DaemonServiceArchiveSessionProcedure:       true,
		bossanovav1connect.DaemonServiceEmptyTrashProcedure:           true,
		bossanovav1connect.DaemonServiceRemoveSessionProcedure:        true,
	}
	if len(slowProcedures) != len(allowed) {
		t.Errorf("slowProcedures has %d entries, the pinned set has %d", len(slowProcedures), len(allowed))
	}
	for procedure := range slowProcedures {
		if !allowed[procedure] {
			t.Errorf("slowProcedures member %q is not a pinned bossanovav1connect.DaemonService*Procedure constant", procedure)
		}
		// Independent of the pinned set: a hand-typed literal, or a constant
		// borrowed from another service, cannot look like a DaemonService
		// procedure path by accident.
		if !strings.HasPrefix(procedure, "/bossanova.v1.DaemonService/") {
			t.Errorf("slowProcedures member %q is not a /bossanova.v1.DaemonService/ procedure path", procedure)
		}
		if got := deadlineFor(procedure); got != slowRPCDeadline {
			t.Errorf("deadlineFor(%q) = %v, want slowRPCDeadline (%v)", procedure, got, slowRPCDeadline)
		}
	}
}

// TestDeadlineFor_ReadsTheBoundsLive is the guard for the hazard that made the
// slow table a set rather than a procedure→duration map: a map of durations is
// filled once at package-var initialisation, so reassigning slowRPCDeadline
// afterwards (rpcdeadline.go's e2e override, or a test) would leave stale
// copies and silently not apply — which is exactly how RecordChat stayed at
// 120s in an e2e build that had asked for a 3s bound.
func TestDeadlineFor_ReadsTheBoundsLive(t *testing.T) {
	prevSlow, prevDefault := slowRPCDeadline, defaultRPCDeadline
	t.Cleanup(func() { slowRPCDeadline, defaultRPCDeadline = prevSlow, prevDefault })

	slowRPCDeadline = 7 * time.Second
	defaultRPCDeadline = 2 * time.Second

	if got := deadlineFor(bossanovav1connect.DaemonServiceRecordChatProcedure); got != 7*time.Second {
		t.Errorf("deadlineFor(RecordChat) = %v after reassigning slowRPCDeadline, want 7s", got)
	}
	if got := deadlineFor("/bossanova.v1.DaemonService/ListSessions"); got != 2*time.Second {
		t.Errorf("deadlineFor(ListSessions) = %v after reassigning defaultRPCDeadline, want 2s", got)
	}
}

// TestSlowProcedures_CoverTheAttachPath is the specific regression the
// review found: RecordChat is the RPC the attach view blocks on, and its daemon
// handler reaches spawnChatTmux (plugin gRPC BuildInteractive, keyring
// credential materialisation, a tmux spawn). Capping it at the ordinary default
// would report a merely-cold daemon as wedged on the exact screen this ticket
// exists to improve, so it must carry the slow bound.
func TestSlowProcedures_CoverTheAttachPath(t *testing.T) {
	for _, procedure := range []string{
		bossanovav1connect.DaemonServiceRecordChatProcedure,
		bossanovav1connect.DaemonServiceWakeChatProcedure,
		// ResurrectSession is deliberately absent (BOS-984): it is
		// server-streaming now, and this unary-only interceptor never bounds it.
		bossanovav1connect.DaemonServiceArchiveSessionProcedure,
	} {
		if got := deadlineFor(procedure); got != slowRPCDeadline {
			t.Errorf("deadlineFor(%q) = %v, want slowRPCDeadline (%v): this handler spawns tmux or moves a worktree and cannot answer within the default", procedure, got, slowRPCDeadline)
		}
	}
}

// TestDeadlineFor covers both halves of the lookup: a table member gets the
// slow bound, and anything absent — including a real but ordinary procedure —
// falls back to the default.
func TestDeadlineFor(t *testing.T) {
	tests := []struct {
		name      string
		procedure string
		want      time.Duration
	}{
		{"table member gets the slow bound", bossanovav1connect.DaemonServiceMergeSessionProcedure, slowRPCDeadline},
		{"ordinary procedure gets the default", "/bossanova.v1.DaemonService/ListSessions", defaultRPCDeadline},
		{"unknown procedure gets the default", "/bossanova.v1.DaemonService/NoSuchRPC", defaultRPCDeadline},
		{"empty procedure gets the default", "", defaultRPCDeadline},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := deadlineFor(tc.procedure); got != tc.want {
				t.Fatalf("deadlineFor(%q) = %v, want %v", tc.procedure, got, tc.want)
			}
		})
	}
}

// TestLocalClient_ResurrectOutlivesTheUnaryDeadline is the client half of the
// BOS-984 proof. The daemon takes streamGap to settle the resurrect — five
// times the shrunk unary bound — and the call must still succeed.
//
// This is what could not pass before the conversion. ResurrectSession was a
// member of slowProcedures, so deadlineInterceptor.WrapUnary imposed
// slowRPCDeadline on it and a setup script that ran past that bound cancelled
// the request from the client side, whatever the daemon was doing. The
// interceptor's streaming wrappers are pass-throughs, so converting the RPC is
// what removes the bound — not raising it.
//
// Cannot use t.Parallel(): shrinkDefaultRPCDeadline mutates package state.
func TestLocalClient_ResurrectOutlivesTheUnaryDeadline(t *testing.T) {
	shrinkDefaultRPCDeadline(t, 200*time.Millisecond)
	prevSlow := slowRPCDeadline
	slowRPCDeadline = slowDeadlineRatio * defaultRPCDeadline
	t.Cleanup(func() { slowRPCDeadline = prevSlow })

	const setupDuration = time.Second
	socketPath, _ := startWedgedDaemon(t, setupDuration)
	c := NewLocal(socketPath)

	type outcome struct {
		lines   []string
		session *pb.Session
		err     error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		var got outcome
		stream, err := c.ResurrectSession(context.Background(), "sess-1")
		if err != nil {
			got.err = err
			done <- got
			return
		}
		defer func() { _ = stream.Close() }()
		for stream.Receive() {
			msg := stream.Msg()
			if so := msg.GetSetupOutput(); so != nil {
				got.lines = append(got.lines, so.GetText())
			}
			if r := msg.GetSessionResurrected(); r != nil {
				got.session = r.GetSession()
			}
		}
		got.err = stream.Err()
		done <- got
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(rpcWatchdogBudget):
		t.Fatalf("resurrect did not return within %v", rpcWatchdogBudget)
	}
	elapsed := time.Since(start)

	if got.err != nil {
		t.Fatalf("resurrect failed after %v against a healthy but slow daemon "+
			"(unary bound was %v, slow bound %v): %v",
			elapsed, defaultRPCDeadline, slowRPCDeadline, got.err)
	}
	if elapsed <= slowRPCDeadline {
		t.Fatalf("resurrect returned in %v, inside the %v slow-unary bound — "+
			"the slow daemon did not actually delay, so this proves nothing",
			elapsed, slowRPCDeadline)
	}
	if got.session.GetId() != "sess-1" {
		t.Fatalf("terminal frame session = %v, want sess-1", got.session)
	}
	if len(got.lines) == 0 {
		t.Fatal("no setup-output frames; the restore was silent for its whole duration")
	}
}

// TestSlowProcedures_ExcludesStreamingResurrect pins the cleanup half of
// BOS-984. deadlineInterceptor is unary-only, so leaving a now-streaming
// procedure in slowProcedures is dead configuration that reads as an applied
// bound to the next person who looks.
func TestSlowProcedures_ExcludesStreamingResurrect(t *testing.T) {
	t.Parallel()

	if _, ok := slowProcedures[bossanovav1connect.DaemonServiceResurrectSessionProcedure]; ok {
		t.Fatal("ResurrectSession is server-streaming; the unary-only deadline interceptor " +
			"never applies slowProcedures to it, so an entry here is misleading dead config")
	}
}
