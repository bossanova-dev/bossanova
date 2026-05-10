package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestGetInfo(t *testing.T) {
	s := newServer(nil, zerolog.Nop())
	resp, err := s.GetInfo(context.Background(), &bossanovav1.AgentRunnerServiceGetInfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if resp.Info == nil {
		t.Fatal("Info nil")
	}
	if resp.Info.Name != "claude" {
		t.Errorf("Info.Name = %q, want claude", resp.Info.Name)
	}
	if len(resp.Info.Capabilities) == 0 {
		t.Error("Info.Capabilities empty")
	}
}

func TestGetInfoIncludesDangerouslySkipPermissionsSetting(t *testing.T) {
	s := newServer(nil, zerolog.Nop())
	resp, err := s.GetInfo(context.Background(), &bossanovav1.AgentRunnerServiceGetInfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	var found bool
	for _, us := range resp.Info.UserSettings {
		if us.Key == "dangerously_skip_permissions" {
			found = true
			if us.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_BOOL {
				t.Errorf("type = %v, want USER_SETTING_TYPE_BOOL", us.Type)
			}
			if us.DefaultValue != "false" {
				t.Errorf("default = %q, want %q", us.DefaultValue, "false")
			}
		}
	}
	if !found {
		t.Error("dangerously_skip_permissions setting missing from GetInfo")
	}
}

func TestServer_StartRun_SubprocessSurvivesHandlerContextCancel(t *testing.T) {
	// Regression: bossd's host_service uses context.Background() when calling
	// StartRun, but gRPC servers still create a per-call context for the
	// handler. If the handler hands that ctx to the runner, the spawned
	// claude subprocess gets SIGTERM the instant the handler returns and
	// the gRPC framework cancels the per-call ctx — every repair attempt
	// then fails with "signal: terminated" within milliseconds.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "sleep 5")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	handlerCtx, cancelHandler := context.WithCancel(context.Background())
	startResp, err := srv.StartRun(handlerCtx, &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "sid-survive", LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Simulate gRPC tearing down the per-call ctx after the handler returns.
	cancelHandler()

	// Give cancellation propagation a moment, then assert the subprocess
	// outlived the RPC. Poll briefly to absorb scheduling jitter.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		runningResp, err := srv.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: startResp.SessionId})
		if err != nil {
			t.Fatalf("IsRunning: %v", err)
		}
		if !runningResp.Running {
			t.Fatal("subprocess died after handler ctx cancel — RPC ctx must not gate subprocess lifetime")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Clean up.
	if _, err := srv.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{SessionId: startResp.SessionId}); err != nil {
		t.Fatalf("StopRun cleanup: %v", err)
	}
}

func TestServer_StopRun_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "sleep 5")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	startResp, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "sid-stop", LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	runningResp, err := srv.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: startResp.SessionId})
	if err != nil || !runningResp.Running {
		t.Fatalf("IsRunning before stop: running=%v err=%v", runningResp.Running, err)
	}

	if _, err := srv.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{SessionId: startResp.SessionId}); err != nil {
		t.Fatalf("StopRun: %v", err)
	}

	runningResp, err = srv.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: startResp.SessionId})
	if err != nil || runningResp.Running {
		t.Fatalf("IsRunning after stop: running=%v err=%v", runningResp.Running, err)
	}
}

func TestServer_ExitStatus(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "exit 7")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	startResp, _ := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "sid-exit", LogPath: logPath,
	})

	deadline := time.Now().Add(2 * time.Second)
	var exit *bossanovav1.AgentExitStatusResponse
	for time.Now().Before(deadline) {
		var err error
		exit, err = srv.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: startResp.SessionId})
		if err == nil && exit.IsComplete {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exit == nil || !exit.IsComplete {
		t.Fatal("never observed exit")
	}
	if exit.ExitError == "" {
		t.Error("expected non-empty ExitError for exit-7")
	}
}

func TestServer_ConfigureFinalizeHook(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}

	resp, err := srv.ConfigureFinalizeHook(context.Background(), &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir: dir, SessionId: "s1", HookToken: "tkn", HookPort: 12345,
	})
	if err != nil {
		t.Fatalf("ConfigureFinalizeHook: %v", err)
	}
	if !resp.IsSupported {
		t.Error("IsSupported = false, want true for claude")
	}
	hookFile := filepath.Join(dir, ".claude", "settings.local.json")
	if _, err := os.Stat(hookFile); err != nil {
		t.Errorf("hook file not written: %v", err)
	}
	data, _ := os.ReadFile(hookFile)
	if !strings.Contains(string(data), "tkn") {
		t.Errorf("hook file does not contain token: %q", data)
	}
	// Session-keyed branch must POST to /hooks/finalize/{sessionID}.
	if !strings.Contains(string(data), "/hooks/finalize/s1") {
		t.Errorf("hook file does not contain session-keyed URL: %q", data)
	}
}

// TestServer_ConfigureFinalizeHook_RunScoped exercises the
// agent_session_id branch: a non-empty AgentSessionId installs a
// run-scoped Stop-hook entry that POSTs to
// /hooks/agent-run-complete/{agent_session_id}, with a
// "bossd-agent-run-{agent_session_id}" matcher so it can coexist
// alongside the cron's session-keyed entry without overwriting it.
func TestServer_ConfigureFinalizeHook_RunScoped(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}

	resp, err := srv.ConfigureFinalizeHook(context.Background(), &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir:        dir,
		SessionId:      "s2",
		AgentSessionId: "agent-run-xyz",
		HookToken:      "tkn-run",
		HookPort:       54321,
	})
	if err != nil {
		t.Fatalf("ConfigureFinalizeHook (run-scoped): %v", err)
	}
	if !resp.IsSupported {
		t.Error("IsSupported = false, want true for claude")
	}
	hookFile := filepath.Join(dir, ".claude", "settings.local.json")
	data, err := os.ReadFile(hookFile)
	if err != nil {
		t.Fatalf("read hook file: %v", err)
	}
	if !strings.Contains(string(data), "bossd-agent-run-agent-run-xyz") {
		t.Errorf("hook file missing run-keyed matcher: %q", data)
	}
	if !strings.Contains(string(data), "/hooks/agent-run-complete/agent-run-xyz") {
		t.Errorf("hook file missing run-keyed URL: %q", data)
	}
	if !strings.Contains(string(data), "tkn-run") {
		t.Errorf("hook file missing run token: %q", data)
	}
}

func TestServer_BuildInteractiveCommand(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}

	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc-123", Resume: false, LogPath: "/data/logs/abc-123.log",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if len(resp.Argv) < 3 || resp.Argv[0] != "bash" || resp.Argv[1] != "-c" {
		t.Fatalf("Argv expected bash -c <script>, got %v", resp.Argv)
	}
	script := resp.Argv[2]
	if !strings.Contains(script, "--session-id abc-123") {
		t.Errorf("script does not pass --session-id: %q", script)
	}
	if !strings.Contains(script, "tee") || !strings.Contains(script, "/data/logs/abc-123.log") {
		t.Errorf("script does not tee to log path: %q", script)
	}
}

func TestServer_BuildInteractiveCommand_Resume(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}

	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "rid", Resume: true, LogPath: "/tmp/x.log",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if !strings.Contains(resp.Argv[2], "--resume rid") {
		t.Errorf("resume flag missing: %q", resp.Argv[2])
	}
}

func TestHasQuestionPromptDelegatesToStatusdetect(t *testing.T) {
	s := newServer(nil, zerolog.Nop())
	pane := []byte("question?\n❯ pick one\n  option a\n  option b\n")
	resp, err := s.HasQuestionPrompt(context.Background(),
		&bossanovav1.HasQuestionPromptRequest{PaneContent: pane})
	if err != nil {
		t.Fatalf("HasQuestionPrompt: %v", err)
	}
	if !resp.HasPrompt {
		t.Errorf("expected has_prompt=true for ❯ + indented options + ?")
	}
}

func TestHasQuestionPromptReturnsFalseForPlainText(t *testing.T) {
	s := newServer(nil, zerolog.Nop())
	resp, err := s.HasQuestionPrompt(context.Background(),
		&bossanovav1.HasQuestionPromptRequest{PaneContent: []byte("just typing some prose")})
	if err != nil {
		t.Fatalf("HasQuestionPrompt: %v", err)
	}
	if resp.HasPrompt {
		t.Error("expected has_prompt=false for plain text")
	}
}

func TestServer_ListIgnoredDirtyFiles(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}
	resp, err := srv.ListIgnoredDirtyFiles(context.Background(), &bossanovav1.ListIgnoredDirtyFilesRequest{
		WorkDir: "/anywhere",
	})
	if err != nil {
		t.Fatalf("ListIgnoredDirtyFiles: %v", err)
	}
	want := ".claude/settings.local.json"
	found := false
	for _, p := range resp.Paths {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Errorf("Paths missing %q: got %v", want, resp.Paths)
	}
}

// startGRPCTestServer spins up a real grpc.Server (in-memory bufconn) with the
// production agentRunnerServiceDesc registered against srv. Mirrors how
// bossd dials the claude plugin in production: the gRPC framework creates a
// per-RPC ctx for each handler and cancels it as soon as the handler returns.
// In-process unit tests of the *Server struct cannot exercise that lifecycle —
// only a real gRPC server can.
func startGRPCTestServer(t *testing.T, runner *Runner) (*grpc.ClientConn, func()) {
	t.Helper()

	srv := &Server{logger: zerolog.Nop(), runner: runner}
	lis := bufconn.Listen(1 << 16)
	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&agentRunnerServiceDesc, srv)

	serveDone := make(chan struct{})
	go func() {
		_ = grpcServer.Serve(lis)
		close(serveDone)
	}()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		_ = lis.Close()
		grpcServer.Stop()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		grpcServer.GracefulStop()
		<-serveDone
		_ = lis.Close()
	}
	return conn, cleanup
}

// grpcInvoke wraps conn.Invoke for a single AgentRunnerService RPC. Each call
// gets a fresh background ctx so the test's outer ctx cannot accidentally
// short-circuit the call before it reaches the handler.
func grpcInvoke(t *testing.T, conn *grpc.ClientConn, method string, in, out any) error {
	t.Helper()
	return conn.Invoke(context.Background(), "/bossanova.v1.AgentRunnerService/"+method, in, out)
}

// TestGRPCRoundTrip_StartRun_SubprocessSurvivesAfterRPCReturns is the
// production-fidelity regression test for the SIGTERM bug: when StartRun
// passed the gRPC handler's per-call ctx to runner.Start, the spawned
// subprocess died the instant the RPC response went on the wire. The
// in-process *Server unit test catches this only because we manually cancel
// a stand-in ctx; this test catches it without any manual cancellation,
// because the gRPC framework itself does the cancelling.
//
// If anyone re-introduces the bug (passing ctx instead of context.Background()
// in StartRun), the IsRunning probe below will flip to false within
// milliseconds and the test will fail.
func TestGRPCRoundTrip_StartRun_SubprocessSurvivesAfterRPCReturns(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "sleep 5")))
	conn, cleanup := startGRPCTestServer(t, r)
	defer cleanup()

	startResp := &bossanovav1.StartAgentRunResponse{}
	if err := grpcInvoke(t, conn, "StartRun", &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "sid-grpc-survive", LogPath: logPath,
	}, startResp); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if startResp.SessionId == "" {
		t.Fatal("StartRun returned empty SessionId")
	}

	// Poll for ~500ms — production saw the SIGTERM hit in <1s, so any
	// regression will surface inside this window.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		runResp := &bossanovav1.IsAgentRunningResponse{}
		if err := grpcInvoke(t, conn, "IsRunning",
			&bossanovav1.IsAgentRunningRequest{SessionId: startResp.SessionId},
			runResp); err != nil {
			t.Fatalf("IsRunning: %v", err)
		}
		if !runResp.Running {
			t.Fatal("subprocess died after gRPC StartRun returned — gRPC per-call ctx must not gate subprocess lifetime")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Cleanup so the test doesn't leak a sleeping subprocess.
	stopResp := &bossanovav1.StopAgentRunResponse{}
	if err := grpcInvoke(t, conn, "StopRun",
		&bossanovav1.StopAgentRunRequest{SessionId: startResp.SessionId}, stopResp); err != nil {
		t.Fatalf("StopRun cleanup: %v", err)
	}
}

// TestGRPCRoundTrip_ConcurrentSessions_AllSurvive verifies that the per-RPC
// ctx-detachment fix works under concurrency. Three independent StartRun
// RPCs fire in parallel; every spawned subprocess must outlive its
// originating RPC. A regression where ctx is reattached for any subset of
// sessions (e.g. the first one wins, the rest get SIGTERMed) would leave at
// least one IsRunning probe returning false.
func TestGRPCRoundTrip_ConcurrentSessions_AllSurvive(t *testing.T) {
	dir := t.TempDir()
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "sleep 5")))
	conn, cleanup := startGRPCTestServer(t, r)
	defer cleanup()

	const n = 3
	sessionIDs := []string{"sid-c-1", "sid-c-2", "sid-c-3"}

	var startWG sync.WaitGroup
	startErrs := make([]error, n)
	startWG.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer startWG.Done()
			startErrs[i] = grpcInvoke(t, conn, "StartRun", &bossanovav1.StartAgentRunRequest{
				WorkDir:   dir,
				SessionId: sessionIDs[i],
				LogPath:   filepath.Join(dir, sessionIDs[i]+".log"),
			}, &bossanovav1.StartAgentRunResponse{})
		}(i)
	}
	startWG.Wait()
	for i, err := range startErrs {
		if err != nil {
			t.Fatalf("StartRun[%d]: %v", i, err)
		}
	}

	// Every subprocess must still be alive after every RPC has returned.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, sid := range sessionIDs {
			runResp := &bossanovav1.IsAgentRunningResponse{}
			if err := grpcInvoke(t, conn, "IsRunning",
				&bossanovav1.IsAgentRunningRequest{SessionId: sid}, runResp); err != nil {
				t.Fatalf("IsRunning[%s]: %v", sid, err)
			}
			if !runResp.Running {
				t.Fatalf("session %s died after concurrent StartRun returned", sid)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Cleanup all sessions.
	for _, sid := range sessionIDs {
		_ = grpcInvoke(t, conn, "StopRun",
			&bossanovav1.StopAgentRunRequest{SessionId: sid},
			&bossanovav1.StopAgentRunResponse{})
	}
}

// TestGRPCRoundTrip_StopRun_TerminatesSubprocess verifies the explicit
// teardown path is independent of the (now-removed) ctx-driven path. After
// the bug fix, the subprocess is no longer tied to the StartRun RPC ctx, so
// the only remaining way to kill a healthy subprocess is StopRun. If a
// future refactor breaks the runner's per-process cancel/Stop wiring, this
// test will hang then fail rather than silently leaking processes.
func TestGRPCRoundTrip_StopRun_TerminatesSubprocess(t *testing.T) {
	dir := t.TempDir()
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "sleep 30")))
	conn, cleanup := startGRPCTestServer(t, r)
	defer cleanup()

	startResp := &bossanovav1.StartAgentRunResponse{}
	if err := grpcInvoke(t, conn, "StartRun", &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "sid-stop-grpc", LogPath: filepath.Join(dir, "agent.log"),
	}, startResp); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Confirm it really is running before we ask for shutdown.
	runResp := &bossanovav1.IsAgentRunningResponse{}
	if err := grpcInvoke(t, conn, "IsRunning",
		&bossanovav1.IsAgentRunningRequest{SessionId: startResp.SessionId}, runResp); err != nil {
		t.Fatalf("IsRunning before stop: %v", err)
	}
	if !runResp.Running {
		t.Fatal("subprocess not running before StopRun — fakeClaude exited early?")
	}

	if err := grpcInvoke(t, conn, "StopRun",
		&bossanovav1.StopAgentRunRequest{SessionId: startResp.SessionId},
		&bossanovav1.StopAgentRunResponse{}); err != nil {
		t.Fatalf("StopRun: %v", err)
	}

	// IsRunning must flip to false within the runner's WaitDelay (10s) +
	// some slack. Poll up to 12s — well under any reasonable test timeout.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if err := grpcInvoke(t, conn, "IsRunning",
			&bossanovav1.IsAgentRunningRequest{SessionId: startResp.SessionId}, runResp); err != nil {
			t.Fatalf("IsRunning after stop: %v", err)
		}
		if !runResp.Running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("subprocess still running 12s after StopRun — runner.Stop wiring may be broken")
}

// TestGRPCRoundTrip_ExitStatus_ReportsNaturalExit verifies that when a
// subprocess exits on its own (without StopRun being called), ExitStatus
// reports IsComplete=true with the actual exit error. This is the path the
// repair plugin's WaitAgentRun depends on to decide whether to fire
// SESSION_EVENT_FIX_COMPLETE — if the path regresses, repair will appear to
// hang or silently re-attempt the same commit forever.
func TestGRPCRoundTrip_ExitStatus_ReportsNaturalExit(t *testing.T) {
	dir := t.TempDir()
	r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeClaude(t, "exit 7")))
	conn, cleanup := startGRPCTestServer(t, r)
	defer cleanup()

	startResp := &bossanovav1.StartAgentRunResponse{}
	if err := grpcInvoke(t, conn, "StartRun", &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "sid-exit-grpc", LogPath: filepath.Join(dir, "agent.log"),
	}, startResp); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	exit := &bossanovav1.AgentExitStatusResponse{}
	for time.Now().Before(deadline) {
		exit = &bossanovav1.AgentExitStatusResponse{}
		if err := grpcInvoke(t, conn, "ExitStatus",
			&bossanovav1.AgentExitStatusRequest{SessionId: startResp.SessionId}, exit); err != nil {
			t.Fatalf("ExitStatus: %v", err)
		}
		if exit.IsComplete {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !exit.IsComplete {
		t.Fatal("ExitStatus never reported IsComplete after subprocess exited 7")
	}
	if exit.ExitError == "" {
		t.Error("ExitStatus.ExitError empty for `exit 7` — natural-exit error path may have regressed")
	}
}
