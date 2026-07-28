package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	dbpkg "github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/sessionports"
)

// fakeEndpointReader is a canned SessionEndpointReader. It records how it was
// reached and always returns eps, so a test asserts purely on whether the
// server's gates let a read reach the reader at all.
type fakeEndpointReader struct {
	eps        []sessionports.Endpoint
	getCalls   int
	batchCalls int
}

func (f *fakeEndpointReader) Get(sessionports.SessionSnapshot) []sessionports.Endpoint {
	f.getCalls++
	return f.eps
}

func (f *fakeEndpointReader) GetBatch(snaps []sessionports.SessionSnapshot) map[string][]sessionports.Endpoint {
	f.batchCalls++
	out := make(map[string][]sessionports.Endpoint, len(snaps))
	for _, s := range snaps {
		out[s.ID] = f.eps
	}
	return out
}

func cannedEndpoints() []sessionports.Endpoint {
	return []sessionports.Endpoint{{Port: 8080, URL: "http://127.0.0.1:8080"}}
}

// openGate / closedGate are deterministic namespace-gate stand-ins so these
// tests do not depend on the host platform's /proc/self/ns/net.
func openGate(string) bool   { return true }
func closedGate(string) bool { return false }

// seedEndpointSession creates a repo + session and returns the server and
// session id. The server is wired with the given reader and gate.
func seedEndpointSession(t *testing.T, reader SessionEndpointReader, gate func(string) bool) (*Server, string) {
	t.Helper()
	sqlDB := setupServerTestDB(t)
	repos := dbpkg.NewRepoStore(sqlDB)
	sessions := dbpkg.NewSessionStore(sqlDB)
	s := New(Config{
		Repos:                 repos,
		Sessions:              sessions,
		EndpointReader:        reader,
		EndpointNamespaceGate: gate,
	})
	ctx := context.Background()
	repo, err := repos.Create(ctx, dbpkg.CreateRepoParams{
		DisplayName:       "r",
		LocalPath:         "/tmp/r",
		OriginURL:         "https://github.com/acme/r.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, dbpkg.CreateSessionParams{
		RepoID:     repo.ID,
		Title:      "session",
		BranchName: "feature",
		BaseBranch: "main",
		AgentName:  "codex",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s, sess.ID
}

func getSessionEndpoints(t *testing.T, s *Server, id string, includeEndpoints bool) []*pb.HttpEndpoint {
	t.Helper()
	resp, err := s.GetSession(context.Background(), connect.NewRequest(&pb.GetSessionRequest{
		Id:                        id,
		IncludeLocalHttpEndpoints: includeEndpoints,
		ClientNetworkNamespace:    "net:[test]",
	}))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return resp.Msg.GetSession().GetHttpEndpoints()
}

// TestSessionToProto_NeverCarriesEndpoints is the mechanical boundary pin: the
// generic conversion used by every mutation response, daemon snapshot, and
// reverse-stream delta must never populate http_endpoints, even from a fully
// populated session row. Hydration is the handlers' job, gated and last.
func TestSessionToProto_NeverCarriesEndpoints(t *testing.T) {
	tmux := "tmux-x"
	p := SessionToProto(&models.Session{
		ID:              "sess-1",
		WorktreePath:    "/tmp/wt/sess-1",
		TmuxSessionName: &tmux,
	})
	if len(p.GetHttpEndpoints()) != 0 {
		t.Fatalf("SessionToProto carried %d endpoints, want 0", len(p.GetHttpEndpoints()))
	}
}

// TestGetSession_DefaultReadNoEndpoints proves a plain local read (opt-in false)
// never hydrates endpoints even though a reader is wired and its gate is open.
func TestGetSession_DefaultReadNoEndpoints(t *testing.T) {
	reader := &fakeEndpointReader{eps: cannedEndpoints()}
	s, id := seedEndpointSession(t, reader, openGate)

	if eps := getSessionEndpoints(t, s, id, false); len(eps) != 0 {
		t.Fatalf("default read returned %d endpoints, want 0", len(eps))
	}
	if reader.getCalls != 0 {
		t.Fatalf("reader was consulted %d times on a default read, want 0", reader.getCalls)
	}
}

// TestGetSession_OptInHydratesWhenGateOpen proves an opted-in read with an open
// namespace gate surfaces the reader's endpoints.
func TestGetSession_OptInHydratesWhenGateOpen(t *testing.T) {
	reader := &fakeEndpointReader{eps: cannedEndpoints()}
	s, id := seedEndpointSession(t, reader, openGate)

	eps := getSessionEndpoints(t, s, id, true)
	if len(eps) != 1 || eps[0].GetPort() != 8080 || eps[0].GetUrl() != "http://127.0.0.1:8080" {
		t.Fatalf("opted-in read endpoints = %+v, want the canned endpoint", eps)
	}
}

// TestGetSession_NamespaceMismatchFailsClosed proves that even an opted-in read
// gets no endpoints when the namespace gate denies (the Linux fail-closed path,
// exercised deterministically via the injected gate).
func TestGetSession_NamespaceMismatchFailsClosed(t *testing.T) {
	reader := &fakeEndpointReader{eps: cannedEndpoints()}
	s, id := seedEndpointSession(t, reader, closedGate)

	if eps := getSessionEndpoints(t, s, id, true); len(eps) != 0 {
		t.Fatalf("namespace-mismatch read returned %d endpoints, want 0 (fail closed)", len(eps))
	}
	if reader.getCalls != 0 {
		t.Fatalf("reader was consulted %d times behind a closed gate, want 0", reader.getCalls)
	}
}

// TestGetSession_NilReaderNoEndpoints proves the nil-safe path: a daemon with no
// endpoint reader wired never hydrates, even for an opted-in read.
func TestGetSession_NilReaderNoEndpoints(t *testing.T) {
	s, id := seedEndpointSession(t, nil, openGate)
	if eps := getSessionEndpoints(t, s, id, true); len(eps) != 0 {
		t.Fatalf("nil-reader read returned %d endpoints, want 0", len(eps))
	}
}

func listSessionEndpoints(t *testing.T, s *Server, includeEndpoints bool) []*pb.Session {
	t.Helper()
	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{
		IncludeLocalHttpEndpoints: includeEndpoints,
		ClientNetworkNamespace:    "net:[test]",
	}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	return resp.Msg.GetSessions()
}

// TestListSessions_DefaultReadNoEndpoints mirrors the GetSession default-read pin
// for the batch path.
func TestListSessions_DefaultReadNoEndpoints(t *testing.T) {
	reader := &fakeEndpointReader{eps: cannedEndpoints()}
	s, _ := seedEndpointSession(t, reader, openGate)

	for _, sess := range listSessionEndpoints(t, s, false) {
		if len(sess.GetHttpEndpoints()) != 0 {
			t.Fatalf("default list carried endpoints for %s, want 0", sess.GetId())
		}
	}
	if reader.batchCalls != 0 {
		t.Fatalf("reader was batch-consulted %d times on a default list, want 0", reader.batchCalls)
	}
}

// TestListSessions_OptInHydratesWhenGateOpen proves the batch path hydrates every
// listed session behind an open gate.
func TestListSessions_OptInHydratesWhenGateOpen(t *testing.T) {
	reader := &fakeEndpointReader{eps: cannedEndpoints()}
	s, id := seedEndpointSession(t, reader, openGate)

	sessions := listSessionEndpoints(t, s, true)
	var found bool
	for _, sess := range sessions {
		if sess.GetId() != id {
			continue
		}
		found = true
		if len(sess.GetHttpEndpoints()) != 1 || sess.GetHttpEndpoints()[0].GetPort() != 8080 {
			t.Fatalf("opted-in list endpoints = %+v, want the canned endpoint", sess.GetHttpEndpoints())
		}
	}
	if !found {
		t.Fatalf("session %s not present in list", id)
	}
}

// TestListSessions_NamespaceMismatchFailsClosed mirrors the GetSession fail-closed
// pin for the batch path.
func TestListSessions_NamespaceMismatchFailsClosed(t *testing.T) {
	reader := &fakeEndpointReader{eps: cannedEndpoints()}
	s, _ := seedEndpointSession(t, reader, closedGate)

	for _, sess := range listSessionEndpoints(t, s, true) {
		if len(sess.GetHttpEndpoints()) != 0 {
			t.Fatalf("mismatch list carried endpoints for %s, want 0", sess.GetId())
		}
	}
	if reader.batchCalls != 0 {
		t.Fatalf("reader was batch-consulted %d times behind a closed gate, want 0", reader.batchCalls)
	}
}

// TestSessionEndpointSnapshot pins the tmux-name assembly: the session's own
// tmux name plus each chat's, skipping nil/empty, with the worktree path as the
// identity anchor.
func TestSessionEndpointSnapshot(t *testing.T) {
	sessTmux := "sess-tmux"
	chatTmux := "chat-tmux"
	var empty string
	sess := &models.Session{ID: "s1", WorktreePath: "/wt/s1", TmuxSessionName: &sessTmux}
	chats := []*models.AgentChat{
		{TmuxSessionName: &chatTmux},
		{TmuxSessionName: nil},
		{TmuxSessionName: &empty},
		nil,
	}
	snap := sessionEndpointSnapshot(sess, chats)
	if snap.ID != "s1" || snap.WorktreePath != "/wt/s1" {
		t.Fatalf("snapshot identity = %+v, want id s1 / worktree /wt/s1", snap)
	}
	if len(snap.TmuxNames) != 2 || snap.TmuxNames[0] != "sess-tmux" || snap.TmuxNames[1] != "chat-tmux" {
		t.Fatalf("snapshot tmux names = %v, want [sess-tmux chat-tmux]", snap.TmuxNames)
	}
}

// TestEndpointsToProto covers the wire mapping and the defensive port clamp.
func TestEndpointsToProto(t *testing.T) {
	if got := endpointsToProto(nil); got != nil {
		t.Fatalf("nil endpoints -> %v, want nil", got)
	}
	out := endpointsToProto([]sessionports.Endpoint{
		{Port: 8080, URL: "http://127.0.0.1:8080"},
		{Port: 70000, URL: "http://127.0.0.1:70000"}, // out of range, dropped
		{Port: -1, URL: "bad"},                       // out of range, dropped
	})
	if len(out) != 1 || out[0].GetPort() != 8080 {
		t.Fatalf("endpointsToProto = %+v, want only the in-range 8080 endpoint", out)
	}
}
