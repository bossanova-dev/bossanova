package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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

func TestBuildInteractiveCommandReturnsSlashCommandPrefix(t *testing.T) {
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:      "agent-1",
		LogPath:        filepath.Join(t.TempDir(), "claude.log"),
		InitialCommand: "boss-finalize",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if resp.GetCommandPrefix() != "/" {
		t.Fatalf("command prefix = %q, want /", resp.GetCommandPrefix())
	}
}

func TestBuildInteractiveCommandAppendsSystemPrompt(t *testing.T) {
	srv := &Server{}

	// With the field set, the claude argv must carry --append-system-prompt.
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:          "agent-1",
		LogPath:            filepath.Join(t.TempDir(), "claude.log"),
		AppendSystemPrompt: "autonomous cron run",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	joined := strings.Join(resp.GetArgv(), "\x00")
	if !strings.Contains(joined, "--append-system-prompt\x00autonomous cron run") {
		t.Fatalf("argv = %v, want --append-system-prompt with the directive", resp.GetArgv())
	}
	// The declaration is what bossd trusts instead of assuming delivery, so it
	// must track the flag exactly. Assert both together: the pair, never the
	// declaration alone, is what makes drift detectable.
	assertDeclarationMatchesArgv(t, resp)

	// Without it, the flag must be absent (non-cron chats are unaffected).
	respNone, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "agent-2",
		LogPath:   filepath.Join(t.TempDir(), "claude.log"),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if strings.Contains(strings.Join(respNone.GetArgv(), " "), "--append-system-prompt") {
		t.Fatalf("argv = %v, want no --append-system-prompt when field empty", respNone.GetArgv())
	}
	assertDeclarationMatchesArgv(t, respNone)
}

// assertDeclarationMatchesArgv is the anti-drift pin: append_system_prompt_support
// claims IN_ARGV exactly when --append-system-prompt is on the command line, and
// never otherwise. A declaration that outran the flag would make bossd stay
// silent about a suffix that never reached the agent — the precise failure this
// field exists to make impossible.
func assertDeclarationMatchesArgv(t *testing.T, resp *bossanovav1.BuildInteractiveCommandResponse) {
	t.Helper()
	onArgv := slices.Contains(resp.GetArgv(), "--append-system-prompt")
	claimed := resp.GetAppendSystemPromptSupport() == bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV
	if onArgv != claimed {
		t.Fatalf("declaration %v disagrees with argv %v (flag present = %v)",
			resp.GetAppendSystemPromptSupport(), resp.GetArgv(), onArgv)
	}
}

func TestBuildInteractiveCommand_AppendsModel(t *testing.T) {
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
		LogPath:   filepath.Join(t.TempDir(), "claude.log"),
		Model:     "sonnet",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.GetArgv(), "\x00")
	if !strings.Contains(joined, "--model\x00sonnet") {
		t.Fatalf("argv %v missing --model sonnet", resp.GetArgv())
	}
}

func TestBuildInteractiveCommand_NoModelWhenEmpty(t *testing.T) {
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
		LogPath:   filepath.Join(t.TempDir(), "claude.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resp.GetArgv() {
		if a == "--model" {
			t.Fatal("argv should not contain --model when model empty")
		}
	}
}

func TestBuildInteractiveCommand_AppendsMcpConfig(t *testing.T) {
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:            "sid",
		LogPath:              filepath.Join(t.TempDir(), "claude.log"),
		ManagedMcpConfigPath: "/data/bossanova/mcp-configs/sid.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.GetArgv(), "\x00")
	if !strings.Contains(joined, "--mcp-config\x00/data/bossanova/mcp-configs/sid.json") {
		t.Fatalf("argv %v missing --mcp-config <path>", resp.GetArgv())
	}
}

// argvHas reports whether argv contains an exact token.
func argvHas(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func TestBuildInteractiveCommand_AppendsStrictManagedMcpConfig(t *testing.T) {
	srv := &Server{}
	// StrictManagedMcpConfig true with a non-empty config path → --strict-mcp-config is
	// appended so the curated config is the whole MCP surface.
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:              "sid",
		LogPath:                filepath.Join(t.TempDir(), "claude.log"),
		ManagedMcpConfigPath:   "/data/bossanova/mcp-configs/sid.json",
		StrictManagedMcpConfig: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !argvHas(resp.GetArgv(), "--strict-mcp-config") {
		t.Fatalf("argv %v missing --strict-mcp-config when strict + config set", resp.GetArgv())
	}

	// StrictManagedMcpConfig false → never append --strict-mcp-config (interactive path).
	respOff, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:              "sid",
		LogPath:                filepath.Join(t.TempDir(), "claude.log"),
		ManagedMcpConfigPath:   "/data/bossanova/mcp-configs/sid.json",
		StrictManagedMcpConfig: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if argvHas(respOff.GetArgv(), "--strict-mcp-config") {
		t.Fatalf("argv %v must not contain --strict-mcp-config when strict false", respOff.GetArgv())
	}

	// StrictManagedMcpConfig true but empty config path must abort the launch. Omitting
	// both flags would fall back to user/project MCP servers and violate strict isolation.
	_, err = srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:              "sid",
		LogPath:                filepath.Join(t.TempDir(), "claude.log"),
		StrictManagedMcpConfig: true,
	})
	if err == nil {
		t.Fatal("BuildInteractiveCommand strict mode without config path returned nil error")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("BuildInteractiveCommand strict mode without config path code = %v, want %v", got, codes.FailedPrecondition)
	}
}

func TestBuildInteractiveCommand_NoMcpConfigWhenEmpty(t *testing.T) {
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
		LogPath:   filepath.Join(t.TempDir(), "claude.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resp.GetArgv() {
		if a == "--mcp-config" {
			t.Fatalf("argv should not contain --mcp-config when path empty: %v", resp.GetArgv())
		}
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

func TestServer_RemoveAgentRunHook(t *testing.T) {
	worktree := t.TempDir()
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}
	// A session-keyed finalize entry that must survive the removal, so the
	// post-removal Stop array is non-nil and the survival assertion is real
	// (not a vacuous pass on an empty/absent Stop list).
	if _, err := srv.ConfigureFinalizeHook(context.Background(), &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir: worktree, SessionId: "sess-1", AgentSessionId: "", HookToken: "tok-final", HookPort: 4000,
	}); err != nil {
		t.Fatalf("configure finalize: %v", err)
	}
	if _, err := srv.ConfigureFinalizeHook(context.Background(), &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir: worktree, SessionId: "sess-1", AgentSessionId: "run-1", HookToken: "tok-1", HookPort: 4000,
	}); err != nil {
		t.Fatalf("configure run: %v", err)
	}
	if _, err := srv.RemoveAgentRunHook(context.Background(), &bossanovav1.RemoveAgentRunHookRequest{
		WorkDir: worktree, AgentSessionId: "run-1",
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	settings := readSettings(t, worktree) // shared helper from hookconfig_test.go (same package)
	stop, ok := settings["hooks"].(map[string]any)["Stop"].([]any)
	if !ok {
		t.Fatal("Stop array missing after removal; the finalize entry should remain")
	}
	var foundFinalize bool
	for _, raw := range stop {
		switch raw.(map[string]any)["matcher"] {
		case runHookMatcherPrefix + "run-1":
			t.Fatal("run-1 hook should be gone after RemoveAgentRunHook")
		case "bossd-finalize":
			foundFinalize = true
		}
	}
	if !foundFinalize {
		t.Error("bossd-finalize entry must survive RemoveAgentRunHook")
	}
}

func TestServer_RemoveAgentRunHook_ValidationError(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}
	_, err := srv.RemoveAgentRunHook(context.Background(), &bossanovav1.RemoveAgentRunHookRequest{
		WorkDir: "", AgentSessionId: "run-1",
	})
	if err == nil {
		t.Fatal("expected an error for empty WorkDir")
	}
	if got := status.Code(err); got != codes.Internal {
		t.Errorf("status code = %v, want %v", got, codes.Internal)
	}
}

// TestServer_BuildInteractiveCommand pins the bare-argv contract. The
// previous shell-wrapping shape (`bash -c "claude … | tee log"`) made
// claude's stdout a pipe rather than a tmux PTY, which modern claude
// auto-detects as non-interactive and bails on with "Input must be
// provided either through stdin or as a prompt argument when using
// --print". The caller (services/bossd/internal/session/tmux_chat.go)
// now captures pane output via `tmux pipe-pane` instead, so this RPC
// must return the plain process argv with no tee, no bash wrapper, no
// `set -o pipefail`.
func TestServer_BuildInteractiveCommand(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}

	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc-123", Resume: false, LogPath: "/data/logs/abc-123.log",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	want := []string{"claude", "--session-id", "abc-123"}
	if !reflect.DeepEqual(resp.Argv, want) {
		t.Fatalf("Argv: got %v, want %v", resp.Argv, want)
	}
	if resp.ReadyMarker != "❯" {
		t.Fatalf("ReadyMarker: got %q, want %q", resp.ReadyMarker, "❯")
	}
	if resp.CommandPrefix != "/" {
		t.Fatalf("CommandPrefix: got %q, want /", resp.CommandPrefix)
	}
	for _, a := range resp.Argv {
		if a == "bash" || a == "sh" || strings.Contains(a, "tee ") || strings.Contains(a, "| ") {
			t.Errorf("Argv must not be shell-wrapped (pipe-tee breaks TTY detection): %v", resp.Argv)
		}
	}
}

func TestServer_BuildInteractiveCommand_Resume(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}

	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "rid", Resume: true, LogPath: filepath.Join(t.TempDir(), "x.log"),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	want := []string{"claude", "--resume", "rid"}
	if !reflect.DeepEqual(resp.Argv, want) {
		t.Fatalf("Argv: got %v, want %v", resp.Argv, want)
	}
	if resp.ReadyMarker != "❯" {
		t.Fatalf("ReadyMarker: got %q, want %q", resp.ReadyMarker, "❯")
	}
	if resp.CommandPrefix != "/" {
		t.Fatalf("CommandPrefix: got %q, want /", resp.CommandPrefix)
	}
}

func TestServer_BuildInteractiveCommand_WrapsInLoginShellWhenConfigured(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_login_shell", "/bin/bash")
	srv := newServer(nil, zerolog.Nop(), runnerOptsFromEnv()...)

	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc-123", Resume: false,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	want := []string{
		"/bin/bash",
		"-l",
		"-c",
		`if [ -r "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi; exec "$@"`,
		"bash",
		"claude",
		"--session-id",
		"abc-123",
	}
	if !reflect.DeepEqual(resp.Argv, want) {
		t.Fatalf("wrapped bash argv:\n got=%#v\nwant=%#v", resp.Argv, want)
	}
}

func TestServer_BuildInteractiveCommand_DangerouslySkipPermissions(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}
	srv.runner.dangerouslySkipPermissions = true

	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "xyz", Resume: false,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	want := []string{"claude", "--session-id", "xyz", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(resp.Argv, want) {
		t.Fatalf("Argv: got %v, want %v", resp.Argv, want)
	}
}

func TestServer_ResolveInteractiveSessionID(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop())}

	tests := []struct {
		name               string
		requestedSessionID string
		wantFound          bool
		wantSessionID      string
		wantReason         string
	}{
		{
			name:       "empty",
			wantReason: "requested_session_id empty",
		},
		{
			name:               "requested",
			requestedSessionID: "abc-123",
			wantFound:          true,
			wantSessionID:      "abc-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := srv.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
				RequestedSessionId: tt.requestedSessionID,
			})
			if err != nil {
				t.Fatalf("ResolveInteractiveSessionID: %v", err)
			}
			if resp.Found != tt.wantFound {
				t.Fatalf("Found: got %v, want %v", resp.Found, tt.wantFound)
			}
			if resp.SessionId != tt.wantSessionID {
				t.Fatalf("SessionId: got %q, want %q", resp.SessionId, tt.wantSessionID)
			}
			if resp.Reason != tt.wantReason {
				t.Fatalf("Reason: got %q, want %q", resp.Reason, tt.wantReason)
			}
		})
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
	if resp.BlocksInput {
		t.Error("expected blocks_input=false for plain text")
	}
}

// TestHasQuestionPromptSplitsNotifyFromBlocking is the claude leg of the
// BOS-600 delivery-gate proof. The daemon refuses to deliver on blocks_input,
// so what has to be true HERE is that the two fields are different predicates:
// a conversational question must set has_prompt (the human is still worth
// notifying) while leaving blocks_input false (the composer is live and the
// message is the answer). The daemon's own tests prove it gates on the field;
// this proves the field carries the right verdict for real Claude panes.
func TestHasQuestionPromptSplitsNotifyFromBlocking(t *testing.T) {
	modal, err := os.ReadFile(filepath.Join("testdata", "panes", "limit_decision_modal.txt"))
	if err != nil {
		t.Fatalf("read modal fixture: %v (services/bossd/internal/tmux copies this file; do not delete it)", err)
	}
	// services/bossd/internal/tmux keeps a byte copy of this capture
	// (testdata/panes/claude_question_modal.txt) and proves "this pane is
	// refused with no keystroke sent" against it, while this test proves the
	// real grammar sets blocks_input on it. That composition holds only while
	// both sides read the same bytes, and the module boundary forbids reading
	// across it — so each side hashes its own copy against its own literal.
	// That is a tripwire, not a proof: nothing compares the two files, so
	// re-pinning both literals would let them diverge green. What it does buy is
	// that divergence cannot happen QUIETLY — edit one copy and that side reddens
	// with the other file named in the failure.
	const modalFixtureDigest = "121503714e4e93e248124b8542828eed52656f727af770592c445f8f34778d29"
	if got := fmt.Sprintf("%x", sha256.Sum256(modal)); got != modalFixtureDigest {
		t.Fatalf("fixture digest = %s, want %s; services/bossd/internal/tmux/testdata/panes/claude_question_modal.txt "+
			"must stay byte-identical and asserts against the same digest", got, modalFixtureDigest)
	}

	tests := []struct {
		name         string
		pane         []byte
		wantPrompt   bool
		wantBlocking bool
	}{
		{
			name:         "conversational question with a live composer",
			pane:         []byte("⏺ I've updated the client. Want me to run the tests now?\n\n❯ \n  claude-opus-4 · ~/code/bossanova · ready\n"),
			wantPrompt:   true,
			wantBlocking: false,
		},
		{
			// The real capture the tmux gate uses as its claude fixture: a
			// weekly-limit decision menu whose ❯ leads an option row, not a
			// composer. Refusing here is the entire point of BOS-600.
			name:         "captured limit-decision modal",
			pane:         modal,
			wantPrompt:   true,
			wantBlocking: true,
		},
	}
	s := newServer(nil, zerolog.Nop())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := s.HasQuestionPrompt(context.Background(),
				&bossanovav1.HasQuestionPromptRequest{PaneContent: tt.pane})
			if err != nil {
				t.Fatalf("HasQuestionPrompt: %v", err)
			}
			if resp.GetHasPrompt() != tt.wantPrompt {
				t.Errorf("has_prompt = %v, want %v", resp.GetHasPrompt(), tt.wantPrompt)
			}
			if resp.GetBlocksInput() != tt.wantBlocking {
				t.Errorf("blocks_input = %v, want %v; the daemon would %s this pane",
					resp.GetBlocksInput(), tt.wantBlocking,
					map[bool]string{true: "refuse delivery into", false: "type into"}[resp.GetBlocksInput()])
			}
		})
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

func TestListIgnoredDirtyFilesIncludesScheduledTasksLock(t *testing.T) {
	srv := &Server{}
	resp, err := srv.ListIgnoredDirtyFiles(context.Background(), &bossanovav1.ListIgnoredDirtyFilesRequest{})
	if err != nil {
		t.Fatalf("ListIgnoredDirtyFiles: %v", err)
	}
	got := map[string]bool{}
	for _, path := range resp.GetPaths() {
		got[path] = true
	}
	for _, want := range []string{".claude/settings.local.json", ".claude/scheduled_tasks.lock"} {
		if !got[want] {
			t.Fatalf("ignored paths missing %q; got %v", want, resp.GetPaths())
		}
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

func TestBuildInteractiveCommand_FallsBackToPluginModel(t *testing.T) {
	srv := &Server{runner: NewRunner(zerolog.Nop(), WithModel("opus[1m]"))}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
		LogPath:   filepath.Join(t.TempDir(), "claude.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.GetArgv(), "\x00")
	if !strings.Contains(joined, "--model\x00opus[1m]") {
		t.Fatalf("argv %v missing plugin default --model opus[1m]", resp.GetArgv())
	}
}

func TestBuildInteractiveCommand_RequestModelBeatsPluginModel(t *testing.T) {
	srv := &Server{runner: NewRunner(zerolog.Nop(), WithModel("opus[1m]"))}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
		Model:     "sonnet",
		LogPath:   filepath.Join(t.TempDir(), "claude.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.GetArgv(), "\x00")
	if !strings.Contains(joined, "--model\x00sonnet") {
		t.Fatalf("argv %v want per-request --model sonnet to win", resp.GetArgv())
	}
	if strings.Contains(joined, "opus[1m]") {
		t.Fatalf("argv %v plugin default must not also appear", resp.GetArgv())
	}
}

// GetInfo must declare the model setting, otherwise the daemon never renders a
// field for it and BOSS_PLUGIN_model is unreachable from settings.
func TestGetInfoDeclaresModelSetting(t *testing.T) {
	srv := &Server{}
	resp, err := srv.GetInfo(context.Background(), &bossanovav1.AgentRunnerServiceGetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.GetInfo().GetUserSettings() {
		if s.GetKey() == "model" {
			if s.GetType() != bossanovav1.UserSettingType_USER_SETTING_TYPE_STRING {
				t.Fatalf("model setting type = %v, want STRING", s.GetType())
			}
			return
		}
	}
	t.Fatalf("GetInfo user settings %v missing a 'model' key", resp.GetInfo().GetUserSettings())
}
