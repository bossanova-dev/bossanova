package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/rs/zerolog"
)

// mockSessionLister implements SessionLister for testing.
type mockSessionLister struct {
	sessions []*pb.Session
	err      error
}

func (m *mockSessionLister) ListSessions(ctx context.Context) ([]*pb.Session, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.sessions, nil
}

// mockHandler implements OrchestratorServiceHandler for testing.
type mockHandler struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	registerCalls  atomic.Int32
	heartbeatCalls atomic.Int32
	syncCalls      atomic.Int32

	registerFn  func(context.Context, *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error)
	heartbeatFn func(context.Context, *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error)
	syncFn      func(context.Context, *connect.Request[pb.SyncSessionsRequest]) (*connect.Response[pb.SyncSessionsResponse], error)
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

func (m *mockHandler) SyncSessions(ctx context.Context, req *connect.Request[pb.SyncSessionsRequest]) (*connect.Response[pb.SyncSessionsResponse], error) {
	if m.syncFn != nil {
		return m.syncFn(ctx, req)
	}
	m.syncCalls.Add(1)
	return connect.NewResponse(&pb.SyncSessionsResponse{}), nil
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
	lister := &mockSessionLister{sessions: []*pb.Session{}}
	mgr := newManagerWithClient(cfg, bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL), logger, lister)

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

func TestConfigFromEnvReturnsNilWhenExplicitlyEmpty(t *testing.T) {
	// Explicit empty string is the opt-out path into local-only mode.
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")
	t.Setenv("BOSSD_DAEMON_ID", "")
	t.Setenv("BOSSD_USER_JWT", "")

	cfg := ConfigFromEnv()
	if cfg != nil {
		t.Fatal("expected nil config when BOSSD_ORCHESTRATOR_URL is explicitly empty")
	}
}

func TestConfigFromEnvUsesProductionDefaultWhenUnset(t *testing.T) {
	// t.Setenv always sets the var; unsetting requires os.Unsetenv and a
	// Cleanup to restore any prior value.
	prev, hadPrev := os.LookupEnv("BOSSD_ORCHESTRATOR_URL")
	if err := os.Unsetenv("BOSSD_ORCHESTRATOR_URL"); err != nil {
		t.Fatalf("unset env: %v", err)
	}
	t.Cleanup(func() {
		var err error
		if hadPrev {
			err = os.Setenv("BOSSD_ORCHESTRATOR_URL", prev)
		} else {
			err = os.Unsetenv("BOSSD_ORCHESTRATOR_URL")
		}
		if err != nil {
			t.Errorf("restore env: %v", err)
		}
	})
	// Short-circuit the keychain read path — otherwise macOS pops the
	// "allow access to Bossanova keychain" prompt on every test run. A
	// non-empty BOSSD_USER_JWT skips loadTokenFromKeychain entirely.
	t.Setenv("BOSSD_USER_JWT", "test-jwt")

	cfg := ConfigFromEnv()
	if cfg == nil {
		t.Fatal("expected non-nil config when BOSSD_ORCHESTRATOR_URL is unset (should use default)")
	}
	if cfg.OrchestratorURL != "https://orchestrator.bossanova.dev" {
		t.Fatalf("expected production default URL, got %q", cfg.OrchestratorURL)
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

func TestConfigFromEnvDefaultsDaemonIDToHostname(t *testing.T) {
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "https://api.example.com")
	t.Setenv("BOSSD_DAEMON_ID", "")
	t.Setenv("BOSSD_USER_JWT", "my-jwt")

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	if hostname == "" {
		t.Skip("hostname unavailable on this machine")
	}

	cfg := ConfigFromEnv()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.DaemonID != hostname {
		t.Fatalf("expected daemon ID to default to hostname %q, got %q", hostname, cfg.DaemonID)
	}
	if cfg.Hostname != hostname {
		t.Fatalf("expected hostname %q, got %q", hostname, cfg.Hostname)
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

func TestSyncLoop_SendsSessionSnapshots(t *testing.T) {
	handler := &mockHandler{}
	var capturedSessions []*pb.Session
	handler.syncFn = func(_ context.Context, req *connect.Request[pb.SyncSessionsRequest]) (*connect.Response[pb.SyncSessionsResponse], error) {
		handler.syncCalls.Add(1)
		capturedSessions = req.Msg.Sessions
		return connect.NewResponse(&pb.SyncSessionsResponse{}), nil
	}

	// Create mock session lister with test sessions
	testSessions := []*pb.Session{
		{Id: "sess-1", RepoId: "repo-1", Title: "Test Session 1"},
		{Id: "sess-2", RepoId: "repo-1", Title: "Test Session 2"},
	}
	lister := &mockSessionLister{sessions: testSessions}

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
	mgr := newManagerWithClient(cfg, bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL), logger, lister)

	err := mgr.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Stop()

	// Directly call syncSessions to test without waiting for ticker
	if err := mgr.syncSessions(); err != nil {
		t.Fatalf("syncSessions: %v", err)
	}

	if handler.syncCalls.Load() != 1 {
		t.Fatalf("expected 1 sync call, got %d", handler.syncCalls.Load())
	}
	if len(capturedSessions) != 2 {
		t.Fatalf("expected 2 sessions in sync request, got %d", len(capturedSessions))
	}
	if capturedSessions[0].Id != "sess-1" {
		t.Fatalf("expected first session ID 'sess-1', got %q", capturedSessions[0].Id)
	}
}

func TestSyncLoop_SkipsWhenDisconnected(t *testing.T) {
	handler := &mockHandler{}
	lister := &mockSessionLister{sessions: []*pb.Session{{Id: "sess-1"}}}

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
	mgr := newManagerWithClient(cfg, bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL), logger, lister)

	// Don't call Connect() - manager is not connected
	if mgr.IsConnected() {
		t.Fatal("manager should not be connected before Connect()")
	}

	// The syncLoop checks IsConnected() before calling syncSessions(),
	// so when disconnected, sync is skipped. We verify IsConnected() is false.
	// We can't easily test the loop logic without timers, but we verify the
	// guard condition that syncLoop uses.
	if mgr.IsConnected() {
		t.Fatal("expected manager to be disconnected")
	}
}

func TestNotifyLogin_ConnectsToUpstream(t *testing.T) {
	handler := &mockHandler{}
	mgr := setupTest(t, handler)

	// NotifyLogin should register and start loops (no prior Connect needed).
	err := mgr.NotifyLogin(context.Background(), []string{"repo-1"})
	if err != nil {
		t.Fatalf("NotifyLogin: %v", err)
	}
	defer mgr.Stop()

	if !mgr.IsConnected() {
		t.Fatal("expected IsConnected to be true after NotifyLogin")
	}
	if handler.registerCalls.Load() != 1 {
		t.Fatalf("expected 1 register call, got %d", handler.registerCalls.Load())
	}
}

func TestNotifyLogout_DisconnectsFromUpstream(t *testing.T) {
	handler := &mockHandler{}
	mgr := setupTest(t, handler)

	err := mgr.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if !mgr.IsConnected() {
		t.Fatal("expected IsConnected before NotifyLogout")
	}

	mgr.NotifyLogout()

	if mgr.IsConnected() {
		t.Fatal("expected IsConnected to be false after NotifyLogout")
	}
}

func TestNotifyLogin_ReconnectsWhenAlreadyConnected(t *testing.T) {
	handler := &mockHandler{}
	mgr := setupTest(t, handler)

	// First connect.
	err := mgr.Connect(context.Background(), []string{"repo-1"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if handler.registerCalls.Load() != 1 {
		t.Fatalf("expected 1 register call after Connect, got %d", handler.registerCalls.Load())
	}

	// NotifyLogin should stop existing loops, re-register, and restart.
	err = mgr.NotifyLogin(context.Background(), []string{"repo-1", "repo-2"})
	if err != nil {
		t.Fatalf("NotifyLogin: %v", err)
	}
	defer mgr.Stop()

	if !mgr.IsConnected() {
		t.Fatal("expected IsConnected after NotifyLogin reconnect")
	}
	if handler.registerCalls.Load() != 2 {
		t.Fatalf("expected 2 register calls (initial + reconnect), got %d", handler.registerCalls.Load())
	}
}

func TestHeartbeat_ReportsActiveSessionCount(t *testing.T) {
	handler := &mockHandler{}
	var capturedActiveCount int32
	handler.heartbeatFn = func(_ context.Context, req *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
		handler.heartbeatCalls.Add(1)
		capturedActiveCount = req.Msg.ActiveSessions
		return connect.NewResponse(&pb.HeartbeatResponse{}), nil
	}

	// Create mock session lister with 3 sessions
	testSessions := []*pb.Session{
		{Id: "sess-1"},
		{Id: "sess-2"},
		{Id: "sess-3"},
	}
	lister := &mockSessionLister{sessions: testSessions}

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
	mgr := newManagerWithClient(cfg, bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL), logger, lister)

	err := mgr.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Stop()

	// Send heartbeat
	if err := mgr.sendHeartbeat(); err != nil {
		t.Fatalf("sendHeartbeat: %v", err)
	}

	if capturedActiveCount != 3 {
		t.Fatalf("expected ActiveSessions=3, got %d", capturedActiveCount)
	}
}
