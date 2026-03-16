package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bosso/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// mockDaemonHandler implements DaemonServiceHandler for proxy tests.
type mockDaemonHandler struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler

	sessions []*pb.Session
	// attachEvents are sent during AttachSession streaming.
	attachEvents []*pb.AttachSessionResponse
}

func (m *mockDaemonHandler) ListSessions(_ context.Context, _ *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	return connect.NewResponse(&pb.ListSessionsResponse{
		Sessions: m.sessions,
	}), nil
}

func (m *mockDaemonHandler) GetSession(_ context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			return connect.NewResponse(&pb.GetSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found"))
}

func (m *mockDaemonHandler) AttachSession(_ context.Context, _ *connect.Request[pb.AttachSessionRequest], stream *connect.ServerStream[pb.AttachSessionResponse]) error {
	for _, ev := range m.attachEvents {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

func (m *mockDaemonHandler) StopSession(_ context.Context, req *connect.Request[pb.StopSessionRequest]) (*connect.Response[pb.StopSessionResponse], error) {
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			s.State = pb.SessionState_SESSION_STATE_CLOSED
			return connect.NewResponse(&pb.StopSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found"))
}

func (m *mockDaemonHandler) PauseSession(_ context.Context, req *connect.Request[pb.PauseSessionRequest]) (*connect.Response[pb.PauseSessionResponse], error) {
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			s.State = pb.SessionState_SESSION_STATE_BLOCKED
			return connect.NewResponse(&pb.PauseSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found"))
}

func (m *mockDaemonHandler) ResumeSession(_ context.Context, req *connect.Request[pb.ResumeSessionRequest]) (*connect.Response[pb.ResumeSessionResponse], error) {
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			s.State = pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
			return connect.NewResponse(&pb.ResumeSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found"))
}

// proxyTestEnv extends testEnv with a mock daemon.
type proxyTestEnv struct {
	*testEnv
	mockDaemon *mockDaemonHandler
	daemonURL  string
	userJWT    string
}

// setupProxyTestEnv creates a full test environment with mock daemon, registered
// daemon in the orchestrator, and a session registry entry.
func setupProxyTestEnv(t *testing.T) *proxyTestEnv {
	t.Helper()

	env := setupTestEnv(t)

	mock := &mockDaemonHandler{
		sessions: []*pb.Session{
			{
				Id:     "session-1",
				RepoId: "repo-1",
				Title:  "Test Session",
				State:  pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			},
		},
	}

	// Start mock daemon server.
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(mock)
	mux.Handle(path, handler)
	daemonServer := httptest.NewServer(mux)
	t.Cleanup(daemonServer.Close)

	// Register a user and daemon via the orchestrator.
	_, userJWT := env.createTestUser(t)

	endpoint := daemonServer.URL
	regReq := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-proxy",
		Hostname: "proxy-host",
		Endpoint: &endpoint,
	})
	regReq.Header().Set("Authorization", "Bearer "+userJWT)
	regResp, err := env.client.RegisterDaemon(context.Background(), regReq)
	if err != nil {
		t.Fatalf("RegisterDaemon: %v", err)
	}

	// Mark daemon online via heartbeat.
	hbReq := connect.NewRequest(&pb.HeartbeatRequest{
		DaemonId:       "daemon-proxy",
		Timestamp:      timestamppb.Now(),
		ActiveSessions: 1,
	})
	hbReq.Header().Set("Authorization", "Bearer "+regResp.Msg.SessionToken)
	if _, err := env.client.Heartbeat(context.Background(), hbReq); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	// Register session in the orchestrator's session registry.
	if _, err := env.sessions.Create(context.Background(), db.CreateSessionEntryParams{
		SessionID: "session-1",
		DaemonID:  "daemon-proxy",
		Title:     "Test Session",
		State:     int(pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN),
	}); err != nil {
		t.Fatalf("Create session entry: %v", err)
	}

	return &proxyTestEnv{
		testEnv:    env,
		mockDaemon: mock,
		daemonURL:  daemonServer.URL,
		userJWT:    userJWT,
	}
}

func TestProxyGetSession(t *testing.T) {
	env := setupProxyTestEnv(t)

	req := connect.NewRequest(&pb.ProxyGetSessionRequest{Id: "session-1"})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	resp, err := env.client.ProxyGetSession(context.Background(), req)
	if err != nil {
		t.Fatalf("ProxyGetSession: %v", err)
	}

	if resp.Msg.Session.Id != "session-1" {
		t.Errorf("session id = %q, want %q", resp.Msg.Session.Id, "session-1")
	}
	if resp.Msg.Session.Title != "Test Session" {
		t.Errorf("title = %q, want %q", resp.Msg.Session.Title, "Test Session")
	}
}

func TestProxyGetSessionNotFound(t *testing.T) {
	env := setupProxyTestEnv(t)

	req := connect.NewRequest(&pb.ProxyGetSessionRequest{Id: "nonexistent"})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	_, err := env.client.ProxyGetSession(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestProxyGetSessionRequiresAuth(t *testing.T) {
	env := setupProxyTestEnv(t)

	req := connect.NewRequest(&pb.ProxyGetSessionRequest{Id: "session-1"})
	// No auth.

	_, err := env.client.ProxyGetSession(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestProxyGetSessionOwnershipCheck(t *testing.T) {
	env := setupProxyTestEnv(t)

	// Create a different user.
	otherSub := "auth0|other-user"
	_, err := env.users.Create(context.Background(), db.CreateUserParams{
		Sub:   otherSub,
		Email: "other@example.com",
		Name:  "Other User",
	})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	otherJWT := env.signJWT(otherSub, "other@example.com")

	req := connect.NewRequest(&pb.ProxyGetSessionRequest{Id: "session-1"})
	req.Header().Set("Authorization", "Bearer "+otherJWT)

	_, err = env.client.ProxyGetSession(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}

func TestProxyListSessions(t *testing.T) {
	env := setupProxyTestEnv(t)

	daemonID := "daemon-proxy"
	req := connect.NewRequest(&pb.ProxyListSessionsRequest{
		DaemonId: &daemonID,
	})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	resp, err := env.client.ProxyListSessions(context.Background(), req)
	if err != nil {
		t.Fatalf("ProxyListSessions: %v", err)
	}

	if len(resp.Msg.Sessions) != 1 {
		t.Fatalf("sessions count = %d, want 1", len(resp.Msg.Sessions))
	}
	if resp.Msg.Sessions[0].Id != "session-1" {
		t.Errorf("session id = %q, want %q", resp.Msg.Sessions[0].Id, "session-1")
	}
}

func TestProxyListSessionsAllDaemons(t *testing.T) {
	env := setupProxyTestEnv(t)

	// No daemon_id — should query all online daemons.
	req := connect.NewRequest(&pb.ProxyListSessionsRequest{})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	resp, err := env.client.ProxyListSessions(context.Background(), req)
	if err != nil {
		t.Fatalf("ProxyListSessions: %v", err)
	}

	if len(resp.Msg.Sessions) != 1 {
		t.Fatalf("sessions count = %d, want 1", len(resp.Msg.Sessions))
	}
}

func TestProxyListSessionsOwnershipCheck(t *testing.T) {
	env := setupProxyTestEnv(t)

	// Create a different user.
	otherSub := "auth0|other-list"
	_, err := env.users.Create(context.Background(), db.CreateUserParams{
		Sub:   otherSub,
		Email: "other-list@example.com",
		Name:  "Other List",
	})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	otherJWT := env.signJWT(otherSub, "other-list@example.com")

	daemonID := "daemon-proxy"
	req := connect.NewRequest(&pb.ProxyListSessionsRequest{
		DaemonId: &daemonID,
	})
	req.Header().Set("Authorization", "Bearer "+otherJWT)

	_, err = env.client.ProxyListSessions(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}

func TestProxyAttachSession(t *testing.T) {
	env := setupProxyTestEnv(t)

	// Set up streaming events on the mock daemon.
	env.mockDaemon.attachEvents = []*pb.AttachSessionResponse{
		{Event: &pb.AttachSessionResponse_OutputLine{
			OutputLine: &pb.OutputLine{Text: "hello from daemon", Timestamp: timestamppb.Now()},
		}},
		{Event: &pb.AttachSessionResponse_StateChange{
			StateChange: &pb.StateChange{
				PreviousState: pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
				NewState:      pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
			},
		}},
		{Event: &pb.AttachSessionResponse_SessionEnded{
			SessionEnded: &pb.SessionEnded{
				FinalState: pb.SessionState_SESSION_STATE_MERGED,
			},
		}},
	}

	req := connect.NewRequest(&pb.ProxyAttachSessionRequest{Id: "session-1"})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	stream, err := env.client.ProxyAttachSession(context.Background(), req)
	if err != nil {
		t.Fatalf("ProxyAttachSession: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// Collect events.
	var events []*pb.ProxyAttachSessionResponse
	for stream.Receive() {
		events = append(events, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("events count = %d, want 3", len(events))
	}

	// Check first event is output line.
	if ol := events[0].GetOutputLine(); ol == nil {
		t.Error("event 0: expected OutputLine")
	} else if ol.Text != "hello from daemon" {
		t.Errorf("output text = %q, want %q", ol.Text, "hello from daemon")
	}

	// Check second event is state change.
	if sc := events[1].GetStateChange(); sc == nil {
		t.Error("event 1: expected StateChange")
	} else if sc.NewState != pb.SessionState_SESSION_STATE_AWAITING_CHECKS {
		t.Errorf("new state = %v, want AWAITING_CHECKS", sc.NewState)
	}

	// Check third event is session ended.
	if se := events[2].GetSessionEnded(); se == nil {
		t.Error("event 2: expected SessionEnded")
	} else if se.FinalState != pb.SessionState_SESSION_STATE_MERGED {
		t.Errorf("final state = %v, want MERGED", se.FinalState)
	}
}

// --- TransferSession tests ---

// transferTestEnv extends proxyTestEnv with a second daemon.
type transferTestEnv struct {
	*proxyTestEnv
	targetDaemonToken string
}

func setupTransferTestEnv(t *testing.T) *transferTestEnv {
	t.Helper()

	env := setupProxyTestEnv(t)

	// Create a second mock daemon server for the target.
	targetMock := &mockDaemonHandler{
		sessions: []*pb.Session{}, // target starts with no sessions
	}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(targetMock)
	mux.Handle(path, handler)
	targetServer := httptest.NewServer(mux)
	t.Cleanup(targetServer.Close)

	// Register the target daemon.
	endpoint := targetServer.URL
	regReq := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-target",
		Hostname: "target-host",
		Endpoint: &endpoint,
	})
	regReq.Header().Set("Authorization", "Bearer "+env.userJWT)
	regResp, err := env.client.RegisterDaemon(context.Background(), regReq)
	if err != nil {
		t.Fatalf("RegisterDaemon target: %v", err)
	}

	// Mark target daemon online.
	hbReq := connect.NewRequest(&pb.HeartbeatRequest{
		DaemonId:       "daemon-target",
		Timestamp:      timestamppb.Now(),
		ActiveSessions: 0,
	})
	hbReq.Header().Set("Authorization", "Bearer "+regResp.Msg.SessionToken)
	if _, err := env.client.Heartbeat(context.Background(), hbReq); err != nil {
		t.Fatalf("Heartbeat target: %v", err)
	}

	return &transferTestEnv{
		proxyTestEnv:      env,
		targetDaemonToken: regResp.Msg.SessionToken,
	}
}

func TestTransferSession(t *testing.T) {
	env := setupTransferTestEnv(t)

	req := connect.NewRequest(&pb.TransferSessionRequest{
		SessionId:      "session-1",
		SourceDaemonId: "daemon-proxy",
		TargetDaemonId: "daemon-target",
	})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	resp, err := env.client.TransferSession(context.Background(), req)
	if err != nil {
		t.Fatalf("TransferSession: %v", err)
	}

	if resp.Msg.TargetDaemonId != "daemon-target" {
		t.Errorf("target_daemon_id = %q, want %q", resp.Msg.TargetDaemonId, "daemon-target")
	}
	if resp.Msg.Session == nil {
		t.Fatal("session is nil")
	}
	if resp.Msg.Session.Id != "session-1" {
		t.Errorf("session id = %q, want %q", resp.Msg.Session.Id, "session-1")
	}

	// Verify registry was updated.
	entry, err := env.sessions.Get(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("Get session entry: %v", err)
	}
	if entry.DaemonID != "daemon-target" {
		t.Errorf("daemon_id = %q, want %q", entry.DaemonID, "daemon-target")
	}
}

func TestTransferSessionRequiresAuth(t *testing.T) {
	env := setupTransferTestEnv(t)

	req := connect.NewRequest(&pb.TransferSessionRequest{
		SessionId:      "session-1",
		SourceDaemonId: "daemon-proxy",
		TargetDaemonId: "daemon-target",
	})
	// No auth header.

	_, err := env.client.TransferSession(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestTransferSessionOwnershipCheck(t *testing.T) {
	env := setupTransferTestEnv(t)

	// Create a different user.
	otherSub := "auth0|other-transfer"
	_, err := env.users.Create(context.Background(), db.CreateUserParams{
		Sub:   otherSub,
		Email: "other-transfer@example.com",
		Name:  "Other Transfer",
	})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	otherJWT := env.signJWT(otherSub, "other-transfer@example.com")

	req := connect.NewRequest(&pb.TransferSessionRequest{
		SessionId:      "session-1",
		SourceDaemonId: "daemon-proxy",
		TargetDaemonId: "daemon-target",
	})
	req.Header().Set("Authorization", "Bearer "+otherJWT)

	_, err = env.client.TransferSession(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}

func TestTransferSessionSameDaemon(t *testing.T) {
	env := setupTransferTestEnv(t)

	req := connect.NewRequest(&pb.TransferSessionRequest{
		SessionId:      "session-1",
		SourceDaemonId: "daemon-proxy",
		TargetDaemonId: "daemon-proxy", // same!
	})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	_, err := env.client.TransferSession(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestTransferSessionNotFound(t *testing.T) {
	env := setupTransferTestEnv(t)

	req := connect.NewRequest(&pb.TransferSessionRequest{
		SessionId:      "nonexistent",
		SourceDaemonId: "daemon-proxy",
		TargetDaemonId: "daemon-target",
	})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	_, err := env.client.TransferSession(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestTransferSessionWrongSource(t *testing.T) {
	env := setupTransferTestEnv(t)

	// Session belongs to daemon-proxy, not daemon-target.
	req := connect.NewRequest(&pb.TransferSessionRequest{
		SessionId:      "session-1",
		SourceDaemonId: "daemon-target", // wrong source
		TargetDaemonId: "daemon-proxy",
	})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	_, err := env.client.TransferSession(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestTransferSessionCreatesAuditLog(t *testing.T) {
	env := setupTransferTestEnv(t)

	req := connect.NewRequest(&pb.TransferSessionRequest{
		SessionId:      "session-1",
		SourceDaemonId: "daemon-proxy",
		TargetDaemonId: "daemon-target",
	})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	_, err := env.client.TransferSession(context.Background(), req)
	if err != nil {
		t.Fatalf("TransferSession: %v", err)
	}

	action := "session.transfer"
	entries, err := env.audit.List(context.Background(), db.AuditListOpts{
		Action: &action,
	})
	if err != nil {
		t.Fatalf("List audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	if entries[0].Resource != "session:session-1" {
		t.Errorf("resource = %q, want %q", entries[0].Resource, "session:session-1")
	}
}

// --- Proxy handler tests (continued) ---

func TestProxyStopSession(t *testing.T) {
	env := setupProxyTestEnv(t)

	req := connect.NewRequest(&pb.ProxyStopSessionRequest{Id: "session-1"})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	resp, err := env.client.ProxyStopSession(context.Background(), req)
	if err != nil {
		t.Fatalf("ProxyStopSession: %v", err)
	}

	if resp.Msg.Session.State != pb.SessionState_SESSION_STATE_CLOSED {
		t.Errorf("state = %v, want CLOSED", resp.Msg.Session.State)
	}
}

func TestProxyPauseSession(t *testing.T) {
	env := setupProxyTestEnv(t)

	req := connect.NewRequest(&pb.ProxyPauseSessionRequest{Id: "session-1"})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	resp, err := env.client.ProxyPauseSession(context.Background(), req)
	if err != nil {
		t.Fatalf("ProxyPauseSession: %v", err)
	}

	if resp.Msg.Session.State != pb.SessionState_SESSION_STATE_BLOCKED {
		t.Errorf("state = %v, want BLOCKED", resp.Msg.Session.State)
	}
}

func TestProxyResumeSession(t *testing.T) {
	env := setupProxyTestEnv(t)

	req := connect.NewRequest(&pb.ProxyResumeSessionRequest{Id: "session-1"})
	req.Header().Set("Authorization", "Bearer "+env.userJWT)

	resp, err := env.client.ProxyResumeSession(context.Background(), req)
	if err != nil {
		t.Fatalf("ProxyResumeSession: %v", err)
	}

	if resp.Msg.Session.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Errorf("state = %v, want IMPLEMENTING_PLAN", resp.Msg.Session.State)
	}
}
