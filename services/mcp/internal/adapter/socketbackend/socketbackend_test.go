package socketbackend

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/kit/daemoncaps"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeDaemon is a minimal DaemonService implementation served over a Unix
// socket so we can exercise the socket-backed Backend without a real bossd.
type fakeDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler

	sessions   []*pb.Session
	refreshReq *pb.RefreshAgentAccountRequest
	repoPRsReq *pb.ListRepoPRsRequest
	// createOutputs are emitted as SetupOutput events before the terminal
	// SessionCreated event, exercising the stream-draining behaviour.
	createOutputs []string
	created       *pb.Session
	// attachExisting is echoed on the terminal SessionCreated frame so the
	// backend adapter can be exercised for both the genuine-create and
	// attach-to-existing paths.
	attachExisting bool
	// mergeDetail is echoed on MergeSessionResponse.detail — the daemon's note
	// about a merge-strategy substitution.
	mergeDetail string
	// mergeReq records the last MergeSessionRequest, so the head pin can be
	// asserted on the wire.
	mergeReq              *pb.MergeSessionRequest
	legacyNoHeadPin       bool
	omitHeadPinCapability bool
	capabilityError       error
	capabilityProbeCalls  int
	// mintReq records the last MintHostedProofViewLinkRequest; mintErr, when
	// set, is the relay's refusal.
	mintReq         *pb.MintHostedProofViewLinkRequest
	mintErr         error
	setupChecksReq  *pb.ListSetupChecksRequest
	setupChecksResp *pb.ListSetupChecksResponse
	// omitPolicyRefCapability models a daemon that predates trial policy refs.
	omitPolicyRefCapability bool
	createCalls             int
}

func (f *fakeDaemon) MintHostedProofViewLink(_ context.Context, req *connect.Request[pb.MintHostedProofViewLinkRequest]) (*connect.Response[pb.MintHostedProofViewLinkResponse], error) {
	f.mintReq = req.Msg
	if f.mintErr != nil {
		return nil, f.mintErr
	}
	return connect.NewResponse(&pb.MintHostedProofViewLinkResponse{ViewUrl: "https://acme.proofs.test/v1.1.sig/tok/", ExpiresAt: timestamppb.New(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC))}), nil
}

func (f *fakeDaemon) ListSetupChecks(_ context.Context, req *connect.Request[pb.ListSetupChecksRequest]) (*connect.Response[pb.ListSetupChecksResponse], error) {
	f.setupChecksReq = req.Msg
	if f.setupChecksResp != nil {
		return connect.NewResponse(f.setupChecksResp), nil
	}
	return connect.NewResponse(&pb.ListSetupChecksResponse{}), nil
}

func (f *fakeDaemon) MergeSession(_ context.Context, req *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	f.mergeReq = req.Msg
	if f.legacyNoHeadPin {
		f.mergeReq.ExpectedHeadSha = ""
	}
	return connect.NewResponse(&pb.MergeSessionResponse{
		Session: &pb.Session{Id: req.Msg.GetId()},
		Detail:  f.mergeDetail,
	}), nil
}

func (f *fakeDaemon) ListSessions(_ context.Context, _ *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: f.sessions}), nil
}

func (f *fakeDaemon) ListRepoPRs(_ context.Context, req *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	f.repoPRsReq = req.Msg
	return connect.NewResponse(&pb.ListRepoPRsResponse{PullRequests: []*pb.PRSummary{{Number: 7, SessionId: "s1"}}}), nil
}

func (f *fakeDaemon) RefreshAgentAccount(_ context.Context, req *connect.Request[pb.RefreshAgentAccountRequest]) (*connect.Response[pb.RefreshAgentAccountResponse], error) {
	f.refreshReq = req.Msg
	return connect.NewResponse(&pb.RefreshAgentAccountResponse{
		Account:      &pb.AgentAccount{Id: req.Msg.GetId(), Label: "refreshed"},
		LiveSmokeRan: req.Msg.GetTestAfterSave(),
		Detail:       "credential test passed",
	}), nil
}

func (f *fakeDaemon) CreateSession(_ context.Context, _ *connect.Request[pb.CreateSessionRequest], stream *connect.ServerStream[pb.CreateSessionResponse]) error {
	f.createCalls++
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

// serveAuthedFakeDaemonAt starts the fake daemon gated by the socketauth server
// interceptor for the given token, mirroring how bossd's Listen() now requires a
// Bearer token on every DaemonService RPC.
func serveAuthedFakeDaemonAt(t *testing.T, fake bossanovav1connect.DaemonServiceHandler, socketPath, token string) {
	t.Helper()

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	_, handler := bossanovav1connect.NewDaemonServiceHandler(fake, connect.WithInterceptors(socketauth.NewServerInterceptor(token)))
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketauth.TokenPath(socketPath))
	})
}

// TestNewAttachesSocketAuthToken proves the MCP socket backend authenticates
// against an auth-gated daemon — i.e. New() reads the co-located token and
// attaches the client interceptor, exactly like the boss CLI's local client.
// Without this the whole `boss mcp` local tool surface would be rejected with
// CodeUnauthenticated once bossd requires the token.
func TestNewAttachesSocketAuthToken(t *testing.T) {
	t.Parallel()

	socketPath := shortSocketPath(t)
	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	serveAuthedFakeDaemonAt(t, &fakeDaemon{sessions: []*pb.Session{{Id: "sess-1"}}}, socketPath, token)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := backend.ListSessions(context.Background(), &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("authed ListSessions failed (token not attached?): %v", err)
	}
	if len(got) != 1 || got[0].GetId() != "sess-1" {
		t.Fatalf("unexpected sessions: %+v", got)
	}
}

// TestAuthedDaemonRejectsWhenTokenMissing proves the gate is real: with no
// co-located token, New() attaches no credential and the auth-gated daemon
// rejects the RPC with CodeUnauthenticated (so the passing test above is not a
// false positive from an ungated server).
func TestAuthedDaemonRejectsWhenTokenMissing(t *testing.T) {
	t.Parallel()

	socketPath := shortSocketPath(t)
	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	serveAuthedFakeDaemonAt(t, &fakeDaemon{}, socketPath, token)

	if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := backend.ListSessions(context.Background(), &pb.ListSessionsRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
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

// TestMergeSessionDetail proves the local socket path carries the
// daemon's merge-strategy note through to the MCP layer instead of discarding
// it, which is what this backend used to do.
func TestMergeSessionDetail(t *testing.T) {
	t.Parallel()

	const detail = "requested rebase was refused; merged with squash instead"
	fake := &fakeDaemon{mergeDetail: detail}
	socketPath := serveFakeDaemon(t, fake)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	session, got, err := backend.MergeSession(context.Background(), "sess-7", "")
	if err != nil {
		t.Fatalf("MergeSession: %v", err)
	}
	if session.GetId() != "sess-7" {
		t.Fatalf("got session %q, want sess-7", session.GetId())
	}
	if got != detail {
		t.Fatalf("detail = %q, want %q (the daemon's note must not be dropped)", got, detail)
	}
}

// TestMergeSessionPin pins BOS-1381: merge_session's match_head reaches the
// daemon as MergeSessionRequest.expected_head_sha. The name stays short on
// purpose: t.TempDir embeds it in the Unix socket path, and macOS caps that
// path at 104 bytes (a longer name fails bind with "invalid argument").
func TestMergeSessionPin(t *testing.T) {
	t.Parallel()

	const pin = "0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b"
	fake := &fakeDaemon{}
	backend, err := New(serveFakeDaemon(t, fake))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := backend.MergeSession(context.Background(), "sess-8", pin); err != nil {
		t.Fatalf("MergeSession: %v", err)
	}
	if got := fake.mergeReq.GetExpectedHeadSha(); got != pin {
		t.Fatalf("expected_head_sha = %q, want %q", got, pin)
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

	resp, err := backend.RefreshAgentAccount(context.Background(), &pb.RefreshAgentAccountRequest{
		Id:            "acct-1",
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	})
	if err != nil {
		t.Fatalf("RefreshAgentAccount: %v", err)
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

func (f *fakeDaemon) GetDaemonCapabilities(context.Context, *connect.Request[pb.GetDaemonCapabilitiesRequest]) (*connect.Response[pb.GetDaemonCapabilitiesResponse], error) {
	f.capabilityProbeCalls++
	if f.legacyNoHeadPin {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("unknown method"))
	}
	if f.capabilityError != nil {
		return nil, f.capabilityError
	}
	capabilities := []string{daemoncaps.MergeExpectedHeadSHA}
	if f.omitHeadPinCapability {
		capabilities = nil
	}
	if !f.omitPolicyRefCapability {
		capabilities = append(capabilities, daemoncaps.CreateSessionPolicyRef)
	}
	return connect.NewResponse(&pb.GetDaemonCapabilitiesResponse{Capabilities: capabilities}), nil
}

func TestMergePinRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *fakeDaemon
		code connect.Code
	}{
		{"legacy", &fakeDaemon{legacyNoHeadPin: true}, connect.CodeUnimplemented},
		{"missing", &fakeDaemon{omitHeadPinCapability: true}, connect.CodeUnimplemented},
		{"unavailable", &fakeDaemon{capabilityError: connect.NewError(connect.CodeUnavailable, errors.New("offline"))}, connect.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socketPath := shortSocketPath(t)
			serveFakeDaemonAt(t, tc.fake, socketPath)
			backend, err := New(socketPath)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = backend.MergeSession(context.Background(), "sess-8", strings.Repeat("a", 40))
			if connect.CodeOf(err) != tc.code {
				t.Errorf("code=%v, want %v (err=%v)", connect.CodeOf(err), tc.code, err)
			}
			if tc.fake.mergeReq != nil {
				t.Fatalf("merge attempted: %v", tc.fake.mergeReq)
			}
		})
	}
}

func TestMergeLegacyUnpinned(t *testing.T) {
	fake := &fakeDaemon{legacyNoHeadPin: true}
	backend, err := New(serveFakeDaemon(t, fake))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.MergeSession(context.Background(), "sess-8", ""); err != nil {
		t.Fatal(err)
	}
	if fake.mergeReq == nil {
		t.Fatal("unpinned merge was not attempted")
	}
	if fake.capabilityProbeCalls != 0 {
		t.Fatalf("unpinned probes=%d, want zero", fake.capabilityProbeCalls)
	}
}

// TestMintViewLink pins that the local backend maps the tool's
// MintProofViewLink to bossd's MintHostedProofViewLink and back, and passes
// the relay's refusal through with its code. The name stays short because the
// fake daemon's socket path embeds it and Unix socket paths are length-capped.
func TestMintViewLink(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemon{}
	backend, err := New(serveFakeDaemon(t, fake))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const head = "0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b0a1b"
	resp, err := backend.MintProofViewLink(context.Background(), &pb.MintProofViewLinkRequest{RepoOriginUrl: "https://github.com/acme/widgets", PrNumber: 7, HeadSha: head})
	if err != nil {
		t.Fatalf("MintProofViewLink: %v", err)
	}
	if got := fake.mintReq; got.GetRepoOriginUrl() != "https://github.com/acme/widgets" || got.GetPrNumber() != 7 || got.GetHeadSha() != head {
		t.Fatalf("relay request = %v", got)
	}
	if resp.GetViewUrl() != "https://acme.proofs.test/v1.1.sig/tok/" || !resp.GetExpiresAt().AsTime().Equal(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("response = %v", resp)
	}

	fake.mintErr = connect.NewError(connect.CodeFailedPrecondition, errors.New("sign in with boss login"))
	if _, err := backend.MintProofViewLink(context.Background(), &pb.MintProofViewLinkRequest{RepoOriginUrl: "https://github.com/acme/widgets", PrNumber: 7}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("refusal = %v, want FailedPrecondition", err)
	}
}

// TestListRepoPRsForwardsQuery proves the local backend sends the tool's PR
// list query on ListRepoPRsRequest and returns the daemon's rows.
func TestListRepoPRsForwardsQuery(t *testing.T) {
	t.Parallel()
	fake := &fakeDaemon{}
	backend, err := New(serveFakeDaemon(t, fake))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	query := &pb.PRListQuery{
		Author: "alice", BaseBranch: "main", Order: pb.PRListOrder_PR_LIST_ORDER_UPDATED_DESC,
		Limit: 3, OwnerFilter: pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_ALL,
	}
	prs, err := backend.ListRepoPRs(context.Background(), &pb.ListRepoPRsRequest{RepoId: "r1", Query: query})
	if err != nil {
		t.Fatalf("ListRepoPRs: %v", err)
	}
	if len(prs) != 1 || prs[0].GetSessionId() != "s1" {
		t.Fatalf("pull requests = %+v, want the daemon's row", prs)
	}
	got := fake.repoPRsReq.GetQuery()
	if fake.repoPRsReq.GetRepoId() != "r1" || got.GetAuthor() != "alice" || got.GetBaseBranch() != "main" ||
		got.GetOrder() != query.GetOrder() || got.GetLimit() != 3 || got.GetOwnerFilter() != query.GetOwnerFilter() {
		t.Fatalf("ListRepoPRsRequest = %+v, want r1 with query %+v", fake.repoPRsReq, query)
	}
}

func TestListSetupChecksRoundTrips(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemon{
		setupChecksResp: &pb.ListSetupChecksResponse{
			RepoId: "repo-1",
			Checks: []*pb.SetupCheck{{
				Id:     "gh-auth",
				Area:   pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
				Status: pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
				Title:  "GitHub CLI authenticated",
			}},
		},
	}
	backend, err := New(serveFakeDaemon(t, fake))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := backend.ListSetupChecks(context.Background(), "repo-1", "/tmp/work")
	if err != nil {
		t.Fatalf("ListSetupChecks: %v", err)
	}
	if fake.setupChecksReq.GetRepoId() != "repo-1" || fake.setupChecksReq.GetWorkspacePath() != "/tmp/work" {
		t.Fatalf("request not forwarded: %+v", fake.setupChecksReq)
	}
	if got.GetRepoId() != "repo-1" || len(got.GetChecks()) != 1 || got.GetChecks()[0].GetId() != "gh-auth" {
		t.Fatalf("unexpected response: %+v", got)
	}
}

func TestCreatePolicyRefRefusedBeforeCreate(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *fakeDaemon
		code connect.Code
	}{
		{"legacy", &fakeDaemon{legacyNoHeadPin: true}, connect.CodeUnimplemented},
		{"missing", &fakeDaemon{omitPolicyRefCapability: true}, connect.CodeUnimplemented},
		{"unavailable", &fakeDaemon{capabilityError: connect.NewError(connect.CodeUnavailable, errors.New("offline"))}, connect.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socketPath := shortSocketPath(t)
			serveFakeDaemonAt(t, tc.fake, socketPath)
			backend, err := New(socketPath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.CreateSession(context.Background(), &pb.CreateSessionRequest{IsVerifyOnly: true, PolicyRef: "config/trial"})
			if connect.CodeOf(err) != tc.code {
				t.Errorf("code=%v, want %v (err=%v)", connect.CodeOf(err), tc.code, err)
			}
			if tc.fake.createCalls != 0 {
				t.Fatalf("session created %d times on a daemon that cannot carry policy_ref", tc.fake.createCalls)
			}
		})
	}
}

func TestCreateProbesOnlyForPolicyRef(t *testing.T) {
	serve := func(fake *fakeDaemon) string {
		socketPath := shortSocketPath(t)
		serveFakeDaemonAt(t, fake, socketPath)
		return socketPath
	}
	fake := &fakeDaemon{created: &pb.Session{Id: "sess-1", PolicyRef: "config/trial"}}
	backend, err := New(serve(fake))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CreateSession(context.Background(), &pb.CreateSessionRequest{IsVerifyOnly: true, PolicyRef: "config/trial"}); err != nil {
		t.Fatalf("trial create on a capable daemon: %v", err)
	}
	if fake.capabilityProbeCalls != 1 || fake.createCalls != 1 {
		t.Fatalf("probes=%d creates=%d, want 1 and 1", fake.capabilityProbeCalls, fake.createCalls)
	}

	legacy := &fakeDaemon{legacyNoHeadPin: true, created: &pb.Session{Id: "sess-2"}}
	backend, err = New(serve(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CreateSession(context.Background(), &pb.CreateSessionRequest{}); err != nil {
		t.Fatalf("ordinary create on a legacy daemon: %v", err)
	}
	if legacy.capabilityProbeCalls != 0 || legacy.createCalls != 1 {
		t.Fatalf("probes=%d creates=%d, want 0 and 1", legacy.capabilityProbeCalls, legacy.createCalls)
	}
}
