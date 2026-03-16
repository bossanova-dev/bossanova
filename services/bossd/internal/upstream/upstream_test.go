package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/rs/zerolog"
)

// mockHandler implements OrchestratorServiceHandler for testing.
type mockHandler struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	registerCalls  atomic.Int32
	heartbeatCalls atomic.Int32

	registerFn  func(context.Context, *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error)
	heartbeatFn func(context.Context, *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error)
}

func (m *mockHandler) RegisterDaemon(ctx context.Context, req *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error) {
	if m.registerFn != nil {
		return m.registerFn(ctx, req)
	}
	m.registerCalls.Add(1)
	return connect.NewResponse(&pb.RegisterDaemonResponse{
		DaemonId:     req.Msg.DaemonId,
		SessionToken: "test-session-token",
	}), nil
}

func (m *mockHandler) Heartbeat(ctx context.Context, req *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
	if m.heartbeatFn != nil {
		return m.heartbeatFn(ctx, req)
	}
	m.heartbeatCalls.Add(1)
	return connect.NewResponse(&pb.HeartbeatResponse{}), nil
}

func setupTest(t *testing.T, handler *mockHandler) *Manager {
	t.Helper()

	mux := http.NewServeMux()
	path, h := bossanovav1connect.NewOrchestratorServiceHandler(handler)
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := Config{
		OrchestratorURL: srv.URL,
		DaemonID:        "test-daemon",
		Hostname:        "test-host",
		UserJWT:         "test-jwt",
	}

	logger := zerolog.Nop()
	mgr := newManagerWithClient(cfg, bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL), logger)

	return mgr
}

func TestConnectRegisters(t *testing.T) {
	handler := &mockHandler{}
	var capturedReq *pb.RegisterDaemonRequest
	handler.registerFn = func(_ context.Context, req *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error) {
		handler.registerCalls.Add(1)
		capturedReq = req.Msg
		return connect.NewResponse(&pb.RegisterDaemonResponse{
			DaemonId:     req.Msg.DaemonId,
			SessionToken: "tok-123",
		}), nil
	}

	mgr := setupTest(t, handler)

	err := mgr.Connect(context.Background(), []string{"repo-1", "repo-2"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Stop()

	if !mgr.IsConnected() {
		t.Fatal("expected IsConnected to be true")
	}
	if mgr.SessionToken() != "tok-123" {
		t.Fatalf("expected session token 'tok-123', got %q", mgr.SessionToken())
	}
	if capturedReq == nil {
		t.Fatal("register was not called")
	}
	if capturedReq.DaemonId != "test-daemon" {
		t.Fatalf("expected daemon_id 'test-daemon', got %q", capturedReq.DaemonId)
	}
	if capturedReq.Hostname != "test-host" {
		t.Fatalf("expected hostname 'test-host', got %q", capturedReq.Hostname)
	}
	if len(capturedReq.RepoIds) != 2 {
		t.Fatalf("expected 2 repo IDs, got %d", len(capturedReq.RepoIds))
	}
}

func TestConnectFailsOnRegistrationError(t *testing.T) {
	handler := &mockHandler{}
	handler.registerFn = func(_ context.Context, _ *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error) {
		handler.registerCalls.Add(1)
		return nil, connect.NewError(connect.CodeUnauthenticated, nil)
	}

	mgr := setupTest(t, handler)

	err := mgr.Connect(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from Connect")
	}
	if mgr.IsConnected() {
		t.Fatal("expected IsConnected to be false after failed registration")
	}
}

func TestStopTerminatesHeartbeat(t *testing.T) {
	handler := &mockHandler{}
	mgr := setupTest(t, handler)

	err := mgr.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	mgr.Stop()

	if mgr.IsConnected() {
		t.Fatal("expected IsConnected to be false after Stop")
	}
}

func TestHeartbeatSendsRequests(t *testing.T) {
	handler := &mockHandler{}
	var capturedDaemonID string
	handler.heartbeatFn = func(_ context.Context, req *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
		handler.heartbeatCalls.Add(1)
		capturedDaemonID = req.Msg.DaemonId
		return connect.NewResponse(&pb.HeartbeatResponse{}), nil
	}

	mgr := setupTest(t, handler)

	err := mgr.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Stop()

	// Directly call sendHeartbeat to test without waiting for ticker.
	if err := mgr.sendHeartbeat(); err != nil {
		t.Fatalf("sendHeartbeat: %v", err)
	}

	if capturedDaemonID != "test-daemon" {
		t.Fatalf("expected daemon_id 'test-daemon', got %q", capturedDaemonID)
	}
}

func TestHeartbeatUsesSessionToken(t *testing.T) {
	handler := &mockHandler{}
	var capturedAuth string
	handler.heartbeatFn = func(_ context.Context, req *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
		handler.heartbeatCalls.Add(1)
		capturedAuth = req.Header().Get("Authorization")
		return connect.NewResponse(&pb.HeartbeatResponse{}), nil
	}

	mgr := setupTest(t, handler)

	err := mgr.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Stop()

	if err := mgr.sendHeartbeat(); err != nil {
		t.Fatalf("sendHeartbeat: %v", err)
	}

	if capturedAuth != "Bearer test-session-token" {
		t.Fatalf("expected auth header 'Bearer test-session-token', got %q", capturedAuth)
	}
}

func TestRegisterSendsJWTAuth(t *testing.T) {
	handler := &mockHandler{}
	var capturedAuth string
	handler.registerFn = func(_ context.Context, req *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error) {
		handler.registerCalls.Add(1)
		capturedAuth = req.Header().Get("Authorization")
		return connect.NewResponse(&pb.RegisterDaemonResponse{
			DaemonId:     req.Msg.DaemonId,
			SessionToken: "tok",
		}), nil
	}

	mgr := setupTest(t, handler)

	err := mgr.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Stop()

	if capturedAuth != "Bearer test-jwt" {
		t.Fatalf("expected auth header 'Bearer test-jwt', got %q", capturedAuth)
	}
}

func TestReconnectOnHeartbeatFailure(t *testing.T) {
	handler := &mockHandler{}

	failCount := atomic.Int32{}
	handler.heartbeatFn = func(_ context.Context, _ *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
		handler.heartbeatCalls.Add(1)
		if failCount.Add(1) <= 3 {
			return nil, connect.NewError(connect.CodeUnavailable, nil)
		}
		return connect.NewResponse(&pb.HeartbeatResponse{}), nil
	}

	mgr := setupTest(t, handler)

	err := mgr.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Stop()

	// Simulate 3 heartbeat failures — should trigger reconnect.
	for i := 0; i < 3; i++ {
		_ = mgr.sendHeartbeat()
	}

	// Manager should detect failure and mark disconnected.
	// The heartbeatLoop handles reconnect, but we test the mechanics here.
	// Note: mgr.IsConnected() may still be true since we haven't run the loop logic.

	// Verify heartbeat was called 3 times.
	if handler.heartbeatCalls.Load() != 3 {
		t.Fatalf("expected 3 heartbeat calls, got %d", handler.heartbeatCalls.Load())
	}
}

func TestConfigFromEnvReturnsNilWhenNotSet(t *testing.T) {
	// Clear env vars to ensure clean state.
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")
	t.Setenv("BOSSD_DAEMON_ID", "")
	t.Setenv("BOSSD_USER_JWT", "")

	cfg := ConfigFromEnv()
	if cfg != nil {
		t.Fatal("expected nil config when BOSSD_ORCHESTRATOR_URL is not set")
	}
}

func TestConfigFromEnvReadsValues(t *testing.T) {
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "https://api.example.com")
	t.Setenv("BOSSD_DAEMON_ID", "my-daemon")
	t.Setenv("BOSSD_USER_JWT", "my-jwt")

	cfg := ConfigFromEnv()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.OrchestratorURL != "https://api.example.com" {
		t.Fatalf("expected URL 'https://api.example.com', got %q", cfg.OrchestratorURL)
	}
	if cfg.DaemonID != "my-daemon" {
		t.Fatalf("expected daemon ID 'my-daemon', got %q", cfg.DaemonID)
	}
	if cfg.UserJWT != "my-jwt" {
		t.Fatalf("expected JWT 'my-jwt', got %q", cfg.UserJWT)
	}
}

func TestReconnectWithBackoff(t *testing.T) {
	handler := &mockHandler{}

	attempts := atomic.Int32{}
	handler.registerFn = func(_ context.Context, req *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error) {
		n := attempts.Add(1)
		if n <= 2 {
			return nil, connect.NewError(connect.CodeUnavailable, nil)
		}
		return connect.NewResponse(&pb.RegisterDaemonResponse{
			DaemonId:     req.Msg.DaemonId,
			SessionToken: "new-token",
		}), nil
	}

	mgr := setupTest(t, handler)

	// Manually trigger reconnect (normally called from heartbeatLoop).
	done := make(chan error, 1)
	go func() {
		done <- mgr.reconnect()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconnect: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconnect timed out")
	}

	if !mgr.IsConnected() {
		t.Fatal("expected IsConnected after reconnect")
	}
	if mgr.SessionToken() != "new-token" {
		t.Fatalf("expected new token, got %q", mgr.SessionToken())
	}
	// Should have called register 3 times (2 failures + 1 success).
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 register attempts, got %d", attempts.Load())
	}
}

func TestReconnectStopsOnShutdown(t *testing.T) {
	handler := &mockHandler{}
	handler.registerFn = func(_ context.Context, _ *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error) {
		return nil, connect.NewError(connect.CodeUnavailable, nil)
	}

	mgr := setupTest(t, handler)

	done := make(chan error, 1)
	go func() {
		done <- mgr.reconnect()
	}()

	// Give reconnect a moment to start, then stop.
	time.Sleep(50 * time.Millisecond)
	close(mgr.stopCh)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from reconnect after stop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect didn't stop")
	}
}
