package client

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// fakeDaemonRPC is a hand-rolled DaemonServiceClient used to drive LocalClient's
// thin RPC wrappers without a live socket. It embeds the interface so any method
// not overridden below panics if a test reaches it (a signal the test touched an
// uncovered path). When err is non-nil the overridden methods return it, which
// exercises each wrapper's `if err != nil { return nil, err }` propagation;
// otherwise they return a canned response so the success-path field extraction
// can be asserted.
type fakeDaemonRPC struct {
	bossanovav1connect.DaemonServiceClient
	err error

	// Captured requests for the wrappers that shape optional fields before
	// dispatching (so the request-mutation branches can be asserted).
	lastTrackerReq *pb.ListTrackerIssuesRequest
	lastRecordReq  *pb.RecordChatRequest
	lastGetReq     *pb.GetSessionRequest
	lastListReq    *pb.ListSessionsRequest

	// Notes (BOS-553): captured so tests can assert LocalClient ignores the
	// repoID argument on Get/Update/Delete — the daemon request carries only
	// the id.
	lastNoteGetReq    *pb.GetNoteRequest
	lastNoteUpdateReq *pb.UpdateNoteRequest
	lastNoteDeleteReq *pb.DeleteNoteRequest
}

var _ bossanovav1connect.DaemonServiceClient = (*fakeDaemonRPC)(nil)

const fakeSessionID = "sess-1"

// fakeMergeDetail is the MergeSessionResponse.detail the fake daemon echoes, so
// TestLocalClientMergeSessionReturnsDetail can pin that BOS-816 second return
// actually comes off the wire rather than being synthesized client-side.
const fakeMergeDetail = "merge strategy squash substituted for rebase"

func sessionResp[T any](f *fakeDaemonRPC, build func() *T) (*connect.Response[T], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(build()), nil
}

func (f *fakeDaemonRPC) GetSession(_ context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	f.lastGetReq = req.Msg
	return sessionResp(f, func() *pb.GetSessionResponse {
		return &pb.GetSessionResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) ListSessions(_ context.Context, req *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	f.lastListReq = req.Msg
	return sessionResp(f, func() *pb.ListSessionsResponse {
		return &pb.ListSessionsResponse{Sessions: []*pb.Session{{Id: fakeSessionID}}}
	})
}

func (f *fakeDaemonRPC) StopSession(_ context.Context, _ *connect.Request[pb.StopSessionRequest]) (*connect.Response[pb.StopSessionResponse], error) {
	return sessionResp(f, func() *pb.StopSessionResponse {
		return &pb.StopSessionResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) PauseSession(_ context.Context, _ *connect.Request[pb.PauseSessionRequest]) (*connect.Response[pb.PauseSessionResponse], error) {
	return sessionResp(f, func() *pb.PauseSessionResponse {
		return &pb.PauseSessionResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) ResumeSession(_ context.Context, _ *connect.Request[pb.ResumeSessionRequest]) (*connect.Response[pb.ResumeSessionResponse], error) {
	return sessionResp(f, func() *pb.ResumeSessionResponse {
		return &pb.ResumeSessionResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) RetrySession(_ context.Context, _ *connect.Request[pb.RetrySessionRequest]) (*connect.Response[pb.RetrySessionResponse], error) {
	return sessionResp(f, func() *pb.RetrySessionResponse {
		return &pb.RetrySessionResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) CloseSession(_ context.Context, _ *connect.Request[pb.CloseSessionRequest]) (*connect.Response[pb.CloseSessionResponse], error) {
	return sessionResp(f, func() *pb.CloseSessionResponse {
		return &pb.CloseSessionResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) MergeSession(_ context.Context, _ *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	return sessionResp(f, func() *pb.MergeSessionResponse {
		return &pb.MergeSessionResponse{Session: &pb.Session{Id: fakeSessionID}, Detail: fakeMergeDetail}
	})
}

func (f *fakeDaemonRPC) UpdateSession(_ context.Context, _ *connect.Request[pb.UpdateSessionRequest]) (*connect.Response[pb.UpdateSessionResponse], error) {
	return sessionResp(f, func() *pb.UpdateSessionResponse {
		return &pb.UpdateSessionResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) LinkSessionPR(_ context.Context, _ *connect.Request[pb.LinkSessionPRRequest]) (*connect.Response[pb.LinkSessionPRResponse], error) {
	return sessionResp(f, func() *pb.LinkSessionPRResponse {
		return &pb.LinkSessionPRResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) RefreshSessionPR(_ context.Context, _ *connect.Request[pb.RefreshSessionPRRequest]) (*connect.Response[pb.RefreshSessionPRResponse], error) {
	return sessionResp(f, func() *pb.RefreshSessionPRResponse {
		return &pb.RefreshSessionPRResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) ArchiveSession(_ context.Context, _ *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	return sessionResp(f, func() *pb.ArchiveSessionResponse {
		return &pb.ArchiveSessionResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) ResurrectSession(_ context.Context, _ *connect.Request[pb.ResurrectSessionRequest]) (*connect.ServerStreamForClient[pb.ResurrectSessionResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	// A real *connect.ServerStreamForClient cannot be constructed outside the
	// connect package, so the success path here is only reachable through the
	// full round-trip tests; the wrapper table below exercises the error leg.
	return nil, errors.New("streaming resurrect is exercised via the connect round-trip tests")
}

func (f *fakeDaemonRPC) EmptyTrash(_ context.Context, _ *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error) {
	return sessionResp(f, func() *pb.EmptyTrashResponse {
		return &pb.EmptyTrashResponse{DeletedCount: 7}
	})
}

func (f *fakeDaemonRPC) ListTrackerIssues(_ context.Context, req *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error) {
	f.lastTrackerReq = req.Msg
	return sessionResp(f, func() *pb.ListTrackerIssuesResponse {
		return &pb.ListTrackerIssuesResponse{Issues: []*pb.TrackerIssue{{ExternalId: "issue-1"}}}
	})
}

func (f *fakeDaemonRPC) RecordChat(_ context.Context, req *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	f.lastRecordReq = req.Msg
	return sessionResp(f, func() *pb.RecordChatResponse {
		return &pb.RecordChatResponse{Chat: &pb.ClaudeChat{AgentSessionId: "agent-1"}}
	})
}

var errRPC = errors.New("rpc boom")

// TestLocalClientSessionWrappers drives every LocalClient wrapper that extracts
// a *pb.Session from the daemon response. Each must return the session on the
// success path (killing the `if err != nil` negation, which would otherwise
// return a nil session) and propagate the error verbatim on the failure path.
func TestLocalClientSessionWrappers(t *testing.T) {
	ctx := context.Background()
	wrappers := []struct {
		name string
		call func(*LocalClient) (*pb.Session, error)
	}{
		{"GetSession", func(c *LocalClient) (*pb.Session, error) {
			return c.GetSession(ctx, "id", SessionReadOptions{})
		}},
		{"StopSession", func(c *LocalClient) (*pb.Session, error) { return c.StopSession(ctx, "id") }},
		{"PauseSession", func(c *LocalClient) (*pb.Session, error) { return c.PauseSession(ctx, "id") }},
		{"ResumeSession", func(c *LocalClient) (*pb.Session, error) { return c.ResumeSession(ctx, "id") }},
		{"RetrySession", func(c *LocalClient) (*pb.Session, error) { return c.RetrySession(ctx, "id") }},
		{"CloseSession", func(c *LocalClient) (*pb.Session, error) { return c.CloseSession(ctx, "id") }},
		{"MergeSession", func(c *LocalClient) (*pb.Session, error) {
			sess, _, err := c.MergeSession(ctx, "id")
			return sess, err
		}},
		{"UpdateSession", func(c *LocalClient) (*pb.Session, error) {
			return c.UpdateSession(ctx, &pb.UpdateSessionRequest{Id: "id"})
		}},
		{"LinkSessionPR", func(c *LocalClient) (*pb.Session, error) { return c.LinkSessionPR(ctx, "id", "1") }},
		{"RefreshSessionPR", func(c *LocalClient) (*pb.Session, error) {
			id := "id"
			return c.RefreshSessionPR(ctx, &pb.RefreshSessionPRRequest{Id: &id})
		}},
		{"ArchiveSession", func(c *LocalClient) (*pb.Session, error) { return c.ArchiveSession(ctx, "id") }},
	}

	for _, w := range wrappers {
		t.Run(w.name+"/success", func(t *testing.T) {
			c := &LocalClient{rpc: &fakeDaemonRPC{}}
			got, err := w.call(c)
			if err != nil {
				t.Fatalf("%s success: unexpected err %v", w.name, err)
			}
			if got == nil || got.Id != fakeSessionID {
				t.Fatalf("%s success: got %v, want session %q", w.name, got, fakeSessionID)
			}
		})

		t.Run(w.name+"/error", func(t *testing.T) {
			c := &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
			got, err := w.call(c)
			if !errors.Is(err, errRPC) {
				t.Fatalf("%s error: got err %v, want %v", w.name, err, errRPC)
			}
			if got != nil {
				t.Fatalf("%s error: got session %v, want nil", w.name, got)
			}
		})
	}
}

// TestLocalClientListSessions covers the slice-returning wrapper separately
// since its success value is a []*pb.Session rather than a single session.
func TestLocalClientListSessions(t *testing.T) {
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		c := &LocalClient{rpc: &fakeDaemonRPC{}}
		got, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, SessionReadOptions{})
		if err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if len(got) != 1 || got[0].Id != fakeSessionID {
			t.Fatalf("got %v, want one session %q", got, fakeSessionID)
		}
	})

	t.Run("error", func(t *testing.T) {
		c := &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
		got, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, SessionReadOptions{})
		if !errors.Is(err, errRPC) {
			t.Fatalf("got err %v, want %v", err, errRPC)
		}
		if got != nil {
			t.Fatalf("got %v, want nil sessions", got)
		}
	})
}

// TestLocalClientGetSessionEndpointOptIn pins the BOS-473 request shaping on
// GetSession: the zero-value option leaves the machine-local endpoint fields
// unset (a plain read), while opting in sets IncludeLocalHttpEndpoints and
// stamps the platform network-namespace identity so the daemon can gate on it.
func TestLocalClientGetSessionEndpointOptIn(t *testing.T) {
	ctx := context.Background()

	t.Run("default leaves endpoint fields unset", func(t *testing.T) {
		fake := &fakeDaemonRPC{}
		c := &LocalClient{rpc: fake}
		if _, err := c.GetSession(ctx, "id", SessionReadOptions{}); err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if fake.lastGetReq.GetIncludeLocalHttpEndpoints() {
			t.Fatalf("IncludeLocalHttpEndpoints = true, want false for a plain read")
		}
		if ns := fake.lastGetReq.GetClientNetworkNamespace(); ns != "" {
			t.Fatalf("ClientNetworkNamespace = %q, want empty for a plain read", ns)
		}
	})

	t.Run("opt-in sets flag and namespace identity", func(t *testing.T) {
		fake := &fakeDaemonRPC{}
		c := &LocalClient{rpc: fake}
		if _, err := c.GetSession(ctx, "id", SessionReadOptions{IncludeLocalHTTPEndpoints: true}); err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if !fake.lastGetReq.GetIncludeLocalHttpEndpoints() {
			t.Fatalf("IncludeLocalHttpEndpoints = false, want true when opted in")
		}
		if got, want := fake.lastGetReq.GetClientNetworkNamespace(), networkNamespaceIdentity(); got != want {
			t.Fatalf("ClientNetworkNamespace = %q, want platform identity %q", got, want)
		}
	})
}

// TestLocalClientListSessionsEndpointOptIn mirrors the GetSession opt-in pins
// for the list read path.
func TestLocalClientListSessionsEndpointOptIn(t *testing.T) {
	ctx := context.Background()

	t.Run("default leaves endpoint fields unset", func(t *testing.T) {
		fake := &fakeDaemonRPC{}
		c := &LocalClient{rpc: fake}
		if _, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, SessionReadOptions{}); err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if fake.lastListReq.GetIncludeLocalHttpEndpoints() {
			t.Fatalf("IncludeLocalHttpEndpoints = true, want false for a plain read")
		}
		if ns := fake.lastListReq.GetClientNetworkNamespace(); ns != "" {
			t.Fatalf("ClientNetworkNamespace = %q, want empty for a plain read", ns)
		}
	})

	t.Run("opt-in sets flag and namespace identity", func(t *testing.T) {
		fake := &fakeDaemonRPC{}
		c := &LocalClient{rpc: fake}
		if _, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, SessionReadOptions{IncludeLocalHTTPEndpoints: true}); err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if !fake.lastListReq.GetIncludeLocalHttpEndpoints() {
			t.Fatalf("IncludeLocalHttpEndpoints = false, want true when opted in")
		}
		if got, want := fake.lastListReq.GetClientNetworkNamespace(), networkNamespaceIdentity(); got != want {
			t.Fatalf("ClientNetworkNamespace = %q, want platform identity %q", got, want)
		}
	})
}

// TestLocalClientListSessionsOptInDoesNotMutateCallerRequest pins the BOS-473
// per-call isolation: opting into endpoint hydration must not mutate the
// caller-owned request, so the same request object reused for a later default
// read stays a plain read (no leaked opt-in flag or namespace identity).
func TestLocalClientListSessionsOptInDoesNotMutateCallerRequest(t *testing.T) {
	ctx := context.Background()
	fake := &fakeDaemonRPC{}
	c := &LocalClient{rpc: fake}

	req := &pb.ListSessionsRequest{IncludeArchived: true}
	if _, err := c.ListSessions(ctx, req, SessionReadOptions{IncludeLocalHTTPEndpoints: true}); err != nil {
		t.Fatalf("opt-in ListSessions: unexpected err %v", err)
	}
	// The wire request the daemon saw must carry the opt-in...
	if !fake.lastListReq.GetIncludeLocalHttpEndpoints() {
		t.Fatal("wire request IncludeLocalHttpEndpoints = false, want true when opted in")
	}
	// ...but the caller's own request object must be untouched.
	if req.GetIncludeLocalHttpEndpoints() {
		t.Fatal("caller request was mutated: IncludeLocalHttpEndpoints = true, want false")
	}
	if ns := req.GetClientNetworkNamespace(); ns != "" {
		t.Fatalf("caller request was mutated: ClientNetworkNamespace = %q, want empty", ns)
	}

	// Reusing the same request for a default read must be a plain read.
	if _, err := c.ListSessions(ctx, req, SessionReadOptions{}); err != nil {
		t.Fatalf("reused default ListSessions: unexpected err %v", err)
	}
	if fake.lastListReq.GetIncludeLocalHttpEndpoints() {
		t.Fatal("reused default read leaked IncludeLocalHttpEndpoints = true, want false")
	}
	if ns := fake.lastListReq.GetClientNetworkNamespace(); ns != "" {
		t.Fatalf("reused default read leaked ClientNetworkNamespace = %q, want empty", ns)
	}
}

// TestLocalClientEmptyTrash pins the int32 count extraction: the success path
// returns the daemon's DeletedCount and the error path returns the zero value
// alongside the error (so the `if err != nil` negation can't pass a stale 0 off
// as success).
func TestLocalClientEmptyTrash(t *testing.T) {
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		c := &LocalClient{rpc: &fakeDaemonRPC{}}
		got, err := c.EmptyTrash(ctx, &pb.EmptyTrashRequest{})
		if err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if got != 7 {
			t.Fatalf("DeletedCount = %d, want 7", got)
		}
	})

	t.Run("error", func(t *testing.T) {
		c := &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
		got, err := c.EmptyTrash(ctx, &pb.EmptyTrashRequest{})
		if !errors.Is(err, errRPC) {
			t.Fatalf("got err %v, want %v", err, errRPC)
		}
		if got != 0 {
			t.Fatalf("DeletedCount = %d, want 0 on error", got)
		}
	})
}

// TestLocalClientListTrackerIssues covers both the optional-source request
// shaping (`if source != ""`) and the error propagation. A non-empty source
// must be threaded into the request as a pointer; an empty source must leave it
// nil.
func TestLocalClientListTrackerIssues(t *testing.T) {
	ctx := context.Background()

	t.Run("source set is forwarded", func(t *testing.T) {
		fake := &fakeDaemonRPC{}
		c := &LocalClient{rpc: fake}
		got, err := c.ListTrackerIssues(ctx, "repo", "q", "github")
		if err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if len(got) != 1 || got[0].ExternalId != "issue-1" {
			t.Fatalf("got %v, want one issue", got)
		}
		if fake.lastTrackerReq.Source == nil || fake.lastTrackerReq.GetSource() != "github" {
			t.Fatalf("Source = %v, want pointer to %q", fake.lastTrackerReq.Source, "github")
		}
	})

	t.Run("empty source stays nil", func(t *testing.T) {
		fake := &fakeDaemonRPC{}
		c := &LocalClient{rpc: fake}
		if _, err := c.ListTrackerIssues(ctx, "repo", "q", ""); err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if fake.lastTrackerReq.Source != nil {
			t.Fatalf("Source = %v, want nil for empty source", fake.lastTrackerReq.Source)
		}
	})

	t.Run("error", func(t *testing.T) {
		c := &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
		got, err := c.ListTrackerIssues(ctx, "repo", "q", "github")
		if !errors.Is(err, errRPC) {
			t.Fatalf("got err %v, want %v", err, errRPC)
		}
		if got != nil {
			t.Fatalf("got %v, want nil issues", got)
		}
	})
}

// TestLocalClientRecordChat covers the optional agent-name request shaping
// (`if agentName != ""`) plus success extraction and error propagation.
func TestLocalClientRecordChat(t *testing.T) {
	ctx := context.Background()

	t.Run("agent name set is forwarded", func(t *testing.T) {
		fake := &fakeDaemonRPC{}
		c := &LocalClient{rpc: fake}
		got, err := c.RecordChat(ctx, "sess", "agent", "title", "claude", true)
		if err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if got == nil || got.AgentSessionId != "agent-1" {
			t.Fatalf("got %v, want chat agent-1", got)
		}
		if fake.lastRecordReq.AgentName == nil || fake.lastRecordReq.GetAgentName() != "claude" {
			t.Fatalf("AgentName = %v, want pointer to %q", fake.lastRecordReq.AgentName, "claude")
		}
		if !fake.lastRecordReq.Resume {
			t.Fatalf("Resume = false, want true forwarded")
		}
	})

	t.Run("empty agent name stays nil", func(t *testing.T) {
		fake := &fakeDaemonRPC{}
		c := &LocalClient{rpc: fake}
		if _, err := c.RecordChat(ctx, "sess", "agent", "title", "", false); err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if fake.lastRecordReq.AgentName != nil {
			t.Fatalf("AgentName = %v, want nil for empty name", fake.lastRecordReq.AgentName)
		}
	})

	t.Run("error", func(t *testing.T) {
		c := &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
		got, err := c.RecordChat(ctx, "sess", "agent", "title", "claude", false)
		if !errors.Is(err, errRPC) {
			t.Fatalf("got err %v, want %v", err, errRPC)
		}
		if got != nil {
			t.Fatalf("got %v, want nil chat", got)
		}
	})
}

// TestLocalClientMergeSessionReturnsDetail pins BOS-816: the widened
// MergeSession returns MergeSessionResponse.detail alongside the session, so the
// CLI can print a merge-strategy substitution note. The wrapper table above only
// covers the session return.
func TestLocalClientMergeSessionReturnsDetail(t *testing.T) {
	c := &LocalClient{rpc: &fakeDaemonRPC{}}
	sess, detail, err := c.MergeSession(context.Background(), "id")
	if err != nil {
		t.Fatalf("MergeSession: %v", err)
	}
	if sess == nil || sess.GetId() != fakeSessionID {
		t.Fatalf("session = %v, want %q", sess, fakeSessionID)
	}
	if detail != fakeMergeDetail {
		t.Fatalf("detail = %q, want %q", detail, fakeMergeDetail)
	}
}

// TestLocalClient_ListSessionsWithReadFailuresNeverPartial pins the local half
// of the BOS-1151 seam: a LocalClient reads one daemon, which either answers or
// fails outright, so the sibling read must return the same sessions ListSessions
// does and an empty failure list. A LocalClient that invented failures would put
// a "this list is incomplete" notice on a list that is complete.
func TestLocalClient_ListSessionsWithReadFailuresNeverPartial(t *testing.T) {
	t.Parallel()
	fake := &fakeDaemonRPC{}
	c := &LocalClient{rpc: fake}

	sessions, failures, err := c.ListSessionsWithReadFailures(context.Background(), &pb.ListSessionsRequest{}, SessionReadOptions{})
	if err != nil {
		t.Fatalf("ListSessionsWithReadFailures: %v", err)
	}
	if len(sessions) != 1 || sessions[0].GetId() != fakeSessionID {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
	if len(failures) != 0 {
		t.Fatalf("a local read is never partial, got %d failures", len(failures))
	}

	// A real failure is still an error, not a partial read.
	errClient := &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
	if _, _, err := errClient.ListSessionsWithReadFailures(context.Background(), &pb.ListSessionsRequest{}, SessionReadOptions{}); err == nil {
		t.Fatal("a failed local read must return an error, not an empty partial result")
	}
}
