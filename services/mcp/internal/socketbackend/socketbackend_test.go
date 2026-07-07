package socketbackend

import (
	"context"
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
)

// fakeDaemon is a minimal DaemonService implementation served over a Unix
// socket so we can exercise the socket-backed Backend without a real bossd.
type fakeDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler

	sessions   []*pb.Session
	refreshReq *pb.RefreshAccountRequest
	// createOutputs are emitted as SetupOutput events before the terminal
	// SessionCreated event, exercising the stream-draining behaviour.
	createOutputs []string
	created       *pb.Session
	// attachExisting is echoed on the terminal SessionCreated frame so the
	// backend adapter can be exercised for both the genuine-create and
	// attach-to-existing paths.
	attachExisting bool
}

func (f *fakeDaemon) ListSessions(_ context.Context, _ *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: f.sessions}), nil
}

func (f *fakeDaemon) RefreshAccount(_ context.Context, req *connect.Request[pb.RefreshAccountRequest]) (*connect.Response[pb.RefreshAccountResponse], error) {
	f.refreshReq = req.Msg
	return connect.NewResponse(&pb.RefreshAccountResponse{
		Account:      &pb.Account{Id: req.Msg.GetId(), Label: "refreshed"},
		LiveSmokeRan: req.Msg.GetTestAfterSave(),
		Detail:       "credential test passed",
	}), nil
}

func (f *fakeDaemon) CreateSession(_ context.Context, _ *connect.Request[pb.CreateSessionRequest], stream *connect.ServerStream[pb.CreateSessionResponse]) error {
	for _, line := range f.createOutputs {
		if err := stream.Send(&pb.CreateSessionResponse{
			Event: &pb.CreateSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: line}},
		}); err != nil {
			return err
		}
	}
	return stream.Send(&pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SessionCreated{SessionCreated: &pb.SessionCreated{
			Session:          f.created,
			AttachedExisting: f.attachExisting,
		}},
	})
}

// serveFakeDaemon starts the fake daemon on a fresh temp-dir Unix socket and
// returns its path.
func serveFakeDaemon(t *testing.T, fake bossanovav1connect.DaemonServiceHandler) string {
	t.Helper()

	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	serveFakeDaemonAt(t, fake, socketPath)
	return socketPath
}

// serveFakeDaemonAt starts the fake daemon bound to a caller-supplied socket
// path, letting a test reserve the path first (e.g. to simulate a restart
// window) and bind the listener later.
func serveFakeDaemonAt(t *testing.T, fake bossanovav1connect.DaemonServiceHandler, socketPath string) {
	t.Helper()

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	_, handler := bossanovav1connect.NewDaemonServiceHandler(fake)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(socketPath)
	})
}

func TestListSessionsRoundTrips(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemon{sessions: []*pb.Session{
		{Id: "sess-1", Title: "first"},
		{Id: "sess-2", Title: "second"},
	}}
	socketPath := serveFakeDaemon(t, fake)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := backend.ListSessions(context.Background(), &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	if got[0].GetId() != "sess-1" || got[1].GetId() != "sess-2" {
		t.Fatalf("unexpected sessions: %+v", got)
	}
}

func TestCreateSessionDrainsStream(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemon{
		createOutputs: []string{"cloning...", "installing deps...", "ready"},
		created:       &pb.Session{Id: "sess-new", Title: "drained"},
	}
	socketPath := serveFakeDaemon(t, fake)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := backend.CreateSession(context.Background(), &pb.CreateSessionRequest{RepoId: "repo-1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if res.Session.GetId() != "sess-new" {
		t.Fatalf("got session %q, want sess-new (stream must drain to the terminal Session)", res.Session.GetId())
	}
	if res.AttachedExisting {
		t.Fatalf("genuine create must report AttachedExisting=false")
	}
}

func TestRefreshAccountRoundTrips(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemon{}
	socketPath := serveFakeDaemon(t, fake)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := backend.RefreshAccount(context.Background(), &pb.RefreshAccountRequest{
		Id:            "acct-1",
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	})
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if fake.refreshReq.GetId() != "acct-1" || string(fake.refreshReq.GetCredential()) != "new-token" || !fake.refreshReq.GetTestAfterSave() {
		t.Fatalf("request not forwarded: %+v", fake.refreshReq)
	}
	if resp.GetAccount().GetId() != "acct-1" || !resp.GetLiveSmokeRan() {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestCreateSessionAttached proves the adapter surfaces the attached_existing
// flag from the terminal SessionCreated frame: when the daemon attached to a
// session that already owned the target branch/PR/tracker, the caller learns the
// supplied prompt was not run. (Short test name keeps the macOS unix-socket path
// under its 104-byte limit.)
func TestCreateSessionAttached(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemon{
		created:        &pb.Session{Id: "sess-existing", Title: "attached"},
		attachExisting: true,
	}
	socketPath := serveFakeDaemon(t, fake)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := backend.CreateSession(context.Background(), &pb.CreateSessionRequest{RepoId: "repo-1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if res.Session.GetId() != "sess-existing" {
		t.Fatalf("got session %q, want sess-existing", res.Session.GetId())
	}
	if !res.AttachedExisting {
		t.Fatalf("attach-to-existing must report AttachedExisting=true")
	}
}

func TestGetCronJobCallsRPC(t *testing.T) {
	t.Parallel()

	socketPath := serveFakeDaemon(t, &fakeDaemonCron{cronJob: &pb.CronJob{Id: "cron-1", Name: "nightly"}})

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job, err := backend.GetCronJob(context.Background(), "cron-1")
	if err != nil {
		t.Fatalf("GetCronJob: %v", err)
	}
	if job.GetId() != "cron-1" {
		t.Fatalf("got cron %q, want cron-1", job.GetId())
	}
}

type fakeDaemonCron struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	cronJob *pb.CronJob
}

func (f *fakeDaemonCron) GetCronJob(_ context.Context, req *connect.Request[pb.GetCronJobRequest]) (*connect.Response[pb.GetCronJobResponse], error) {
	if req.Msg.GetId() != "cron-1" {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	return connect.NewResponse(&pb.GetCronJobResponse{CronJob: f.cronJob}), nil
}

// shortSocketPath returns a Unix socket path under a short temp directory.
// t.TempDir() embeds the (long) test name, which can push the socket path past
// the platform's ~104-char sun_path limit and surface EINVAL instead of the
// ENOENT/ECONNREFUSED we mean to exercise.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bs")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// TestDialRetryBridgesRestartWindow proves that a transient absence of the
// socket (as during a bossd restart) is bridged by the dial-layer retry: the
// listener binds AFTER the RPC begins, yet the call still succeeds without the
// caller retrying.
func TestDialRetryBridgesRestartWindow(t *testing.T) {
	t.Parallel()

	// Reserve a socket path but do not serve it yet.
	socketPath := shortSocketPath(t)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Issue the RPC from a background goroutine so the test goroutine can bind
	// the listener mid-flight. Only the test goroutine touches *testing.T
	// (serveFakeDaemonAt calls t.Fatalf / t.Cleanup, which must not run off the
	// test goroutine); the RPC goroutine only reports its result over a channel.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type rpcResult struct {
		sessions []*pb.Session
		err      error
	}
	resultCh := make(chan rpcResult, 1)
	go func() {
		got, err := backend.ListSessions(ctx, &pb.ListSessionsRequest{})
		resultCh <- rpcResult{sessions: got, err: err}
	}()

	// Bind the fake daemon after a short delay, comfortably shorter than the
	// full retry window (maxDialAttempts * dialRetryDelay), so the in-flight
	// dial retry bridges the gap.
	time.Sleep(300 * time.Millisecond)
	serveFakeDaemonAt(t, &fakeDaemon{sessions: []*pb.Session{{Id: "sess-1"}}}, socketPath)

	res := <-resultCh
	if res.err != nil {
		t.Fatalf("ListSessions during restart window: %v", res.err)
	}
	if len(res.sessions) != 1 || res.sessions[0].GetId() != "sess-1" {
		t.Fatalf("unexpected sessions: %+v", res.sessions)
	}
}

// TestDialRetryBoundedWithClearError proves the retry is bounded and the final
// error names the socket path and is actionable.
func TestDialRetryBoundedWithClearError(t *testing.T) {
	t.Parallel()

	// A path that is never served — dials keep failing with ENOENT.
	socketPath := shortSocketPath(t)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	_, err = backend.ListSessions(context.Background(), &pb.ListSessionsRequest{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error dialing an unserved socket, got nil")
	}
	// The retry window is (maxDialAttempts-1) backoffs of dialRetryDelay (~750ms).
	// Bound the assertion at 2x the full window: far tighter than a bare "did it
	// ever return" ceiling (so a regression widening the window is caught), yet
	// with ample slack to stay non-flaky under -race on a loaded CI runner.
	maxWindow := 2 * maxDialAttempts * dialRetryDelay // 2s, vs the ~750ms real window
	if elapsed > maxWindow {
		t.Fatalf("dial retry not bounded: took %v (window %v)", elapsed, maxWindow)
	}
	msg := err.Error()
	if !strings.Contains(msg, socketPath) {
		t.Fatalf("error should name the socket path %q, got: %v", socketPath, msg)
	}
	if !strings.Contains(msg, "daemon may be restarting") {
		t.Fatalf("error should be actionable (mention restart), got: %v", msg)
	}
}

// TestDialRetryHonorsContextCancellation proves a cancelled/expired request
// context aborts the backoff promptly rather than waiting out the full window.
func TestDialRetryHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	socketPath := shortSocketPath(t)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	fullWindow := maxDialAttempts * dialRetryDelay // 1s

	start := time.Now()
	_, err = backend.ListSessions(ctx, &pb.ListSessionsRequest{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when context is cancelled, got nil")
	}
	// Should return well before exhausting the full retry window.
	if elapsed >= fullWindow {
		t.Fatalf("context cancellation not honored promptly: took %v (full window %v)", elapsed, fullWindow)
	}
}
