package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newTestServer returns a Server wired to a nil host (the host client is
// only used by paths the agent runner doesn't exercise in unit tests) and
// a default Runner. Helper kept colocated with the tests so each test reads
// like a one-liner.
func newTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	return newServer(nil, zerolog.Nop(), opts...)
}

func TestBuildInteractiveCommandReturnsDollarCommandPrefix(t *testing.T) {
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:      "agent-1",
		LogPath:        filepath.Join(t.TempDir(), "codex.log"),
		InitialCommand: "boss-finalize",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if resp.GetCommandPrefix() != "$" {
		t.Fatalf("command prefix = %q, want $", resp.GetCommandPrefix())
	}
}

// TestBuildInteractiveCommandIgnoresAppendSystemPrompt pins that the boss
// session-context suffix is a deliberate no-op for codex: codex has no
// append-system-prompt surface today (see openai/codex#11588), so the daemon
// may offer the field but the codex argv must be byte-for-byte identical with
// and without it. When codex grows a real append flag, this test should flip
// to assert the suffix is wired in.
func TestBuildInteractiveCommandIgnoresAppendSystemPrompt(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	worktree := t.TempDir()
	srv := &Server{}

	base := &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "agent-1",
		WorktreePath: worktree,
	}
	withPrompt := &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:          "agent-1",
		WorktreePath:       worktree,
		AppendSystemPrompt: "You are running inside a bossanova-managed chat. ...",
	}

	respBase, err := srv.BuildInteractiveCommand(context.Background(), base)
	if err != nil {
		t.Fatalf("BuildInteractiveCommand (base): %v", err)
	}
	respWith, err := srv.BuildInteractiveCommand(context.Background(), withPrompt)
	if err != nil {
		t.Fatalf("BuildInteractiveCommand (with prompt): %v", err)
	}

	if strings.Join(respWith.GetArgv(), "\x00") != strings.Join(respBase.GetArgv(), "\x00") {
		t.Fatalf("AppendSystemPrompt must not alter codex argv: base=%v with=%v", respBase.GetArgv(), respWith.GetArgv())
	}
	if strings.Contains(strings.Join(respWith.GetArgv(), " "), "append-system-prompt") {
		t.Fatalf("codex argv must not carry append-system-prompt: %v", respWith.GetArgv())
	}
}

func TestBuildInteractiveCommand_WiresMcpViaConfigOverride(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	worktree := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "s1.json")
	cfg := `{"mcpServers":{"boss":{"command":"/trusted/mcp","args":["--socket","/run/bossd.sock"]}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}

	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:     "s1",
		McpConfigPath: cfgPath,
		WorktreePath:  worktree,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	// Codex has no --mcp-config; the plugin translates the config file into
	// repeatable `-c mcp_servers.boss.*` overrides (keeps wiring out of the
	// worktree). Assert both command and args overrides are present.
	joined := strings.Join(resp.GetArgv(), "\x00")
	if !strings.Contains(joined, "-c\x00mcp_servers.boss.command=\"/trusted/mcp\"") {
		t.Fatalf("argv missing codex mcp command override: %v", resp.GetArgv())
	}
	if !strings.Contains(joined, "-c\x00mcp_servers.boss.args=[\"--socket\",\"/run/bossd.sock\"]") {
		t.Fatalf("argv missing codex mcp args override: %v", resp.GetArgv())
	}
}

// TestBuildInteractiveCommand_CuratedLinearIgnored pins that the curated cron
// config's HTTP "bossanova-linear" server never becomes a codex `-c
// mcp_servers.*` override: codex has no --strict-mcp-config, codexMcpOverrideArgs
// reads ONLY the stdio "boss" server, and a command-less HTTP entry is inert.
// Codex behavior is therefore byte-identical whether or not Linear is present.
func TestBuildInteractiveCommand_CuratedLinearIgnored(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	worktree := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "s1.json")
	// The curated (cron) config shape: boss stdio + bossanova-linear HTTP.
	cfg := `{"mcpServers":{"boss":{"command":"/trusted/mcp","args":["--socket","/run/bossd.sock"]},"bossanova-linear":{"type":"http","url":"https://mcp.linear.app/mcp","headers":{"Authorization":"Bearer ${LINEAR_API_KEY}"}}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}

	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:     "s1",
		McpConfigPath: cfgPath,
		WorktreePath:  worktree,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	joined := strings.Join(resp.GetArgv(), "\x00")
	// boss override still present (unchanged).
	if !strings.Contains(joined, "-c\x00mcp_servers.boss.command=\"/trusted/mcp\"") {
		t.Fatalf("argv missing codex mcp boss override: %v", resp.GetArgv())
	}
	// Linear HTTP entry must NEVER surface as an override or leak the key, and
	// codex must never emit --strict-mcp-config (it has no such flag).
	if strings.Contains(joined, "bossanova-linear") || strings.Contains(joined, "mcp.linear.app") ||
		strings.Contains(joined, "LINEAR_API_KEY") || strings.Contains(joined, "--strict-mcp-config") {
		t.Fatalf("codex argv must not reference the Linear HTTP server or strict flag: %v", resp.GetArgv())
	}
}

func TestBuildInteractiveCommand_McpOverridePrecedesResumeSubcommand(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	worktree := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "s1.json")
	cfg := `{"mcpServers":{"boss":{"command":"/trusted/mcp","args":["--socket","/run/bossd.sock"]}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}

	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:     "agent-1",
		Resume:        true,
		McpConfigPath: cfgPath,
		WorktreePath:  worktree,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	// `-c` is a codex global option and MUST precede the `resume` subcommand;
	// pin the ordering so a future reorder cannot silently push it after resume.
	argv := resp.GetArgv()
	cIdx, resumeIdx := -1, -1
	for i, a := range argv {
		if a == "-c" && cIdx == -1 {
			cIdx = i
		}
		if a == "resume" {
			resumeIdx = i
		}
	}
	if cIdx == -1 || resumeIdx == -1 {
		t.Fatalf("expected both a -c override and a resume subcommand: %v", argv)
	}
	if cIdx > resumeIdx {
		t.Fatalf("-c override (idx %d) must precede resume subcommand (idx %d): %v", cIdx, resumeIdx, argv)
	}
}

func TestBuildInteractiveCommand_NoMcpOverrideWhenEmpty(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "s1",
		WorktreePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if strings.Contains(strings.Join(resp.GetArgv(), " "), "mcp_servers.boss") {
		t.Fatalf("codex argv must not carry mcp override when path empty: %v", resp.GetArgv())
	}
}

func TestBuildInteractiveCommand_NoMcpOverrideWhenConfigUnreadable(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:     "s1",
		McpConfigPath: filepath.Join(t.TempDir(), "does-not-exist.json"),
		WorktreePath:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand must not fail on unreadable mcp config: %v", err)
	}
	if strings.Contains(strings.Join(resp.GetArgv(), " "), "mcp_servers.boss") {
		t.Fatalf("codex argv must not carry mcp override when config unreadable: %v", resp.GetArgv())
	}
}

// TestBuildInteractiveCommand_NoTuiNotificationOverrides pins BOS-487's
// empirical finding: codex must NOT be launched with `tui.notifications` /
// `tui.notification_method` overrides.
//
// The plan wanted `-c 'tui.notifications=["approval-requested"]' -c
// tui.notification_method="osc9"` so an OSC 9 escape in the pipe-pane raw log
// could feed BOS-485's question-signal store. Measured against codex-cli
// 0.145.0: `approval-requested` is not a codex notification kind (the string is
// absent from the binary), and because `tui.notifications` is an allow-list,
// naming it emits nothing AND suppresses the one kind that does fire. That kind
// is `agent-turn-complete`, whose payload is the last assistant message — the
// semantic opposite of "a question is pending", so it must never reach the
// question-signal store.
//
// Flip this test only alongside evidence that codex emits an approval-time
// notification. Details:
// docs/solutions/logic-errors/spike-codex-osc9-notification-is-turn-complete-only.md
func TestBuildInteractiveCommand_NoTuiNotificationOverrides(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "s1",
		WorktreePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	argv := strings.Join(resp.GetArgv(), " ")
	for _, banned := range []string{
		"tui.notifications",
		"tui.notification_method",
		"tui.notification_condition",
	} {
		if strings.Contains(argv, banned) {
			t.Fatalf("codex argv must not carry %q (BOS-487: OSC 9 only signals "+
				"turn-complete, never an approval): %v", banned, resp.GetArgv())
		}
	}
}

func TestBuildInteractiveCommandTrustsWorktreeBeforeReturningArgv(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	worktree := t.TempDir()
	srv := &Server{}

	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "agent-1",
		WorktreePath: worktree,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if len(resp.Argv) == 0 {
		t.Fatal("Argv is empty")
	}

	config, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	wantHeader := `[projects."` + tomlBasicStringEscape(worktree) + `"]`
	if !strings.Contains(string(config), wantHeader+"\ntrust_level = \"trusted\"") {
		t.Fatalf("codex config missing trusted worktree block:\n%s", config)
	}
}

func TestTrustCodexWorktreeSerializesConcurrentConfigUpdates(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	const worktreeCount = 24
	worktrees := make([]string, worktreeCount)
	for i := range worktrees {
		worktrees[i] = filepath.Join(t.TempDir(), "repo")
		if err := os.MkdirAll(worktrees[i], 0o755); err != nil {
			t.Fatalf("create worktree: %v", err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, worktreeCount)
	for _, worktree := range worktrees {
		worktree := worktree
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- trustCodexWorktree(worktree)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("trustCodexWorktree: %v", err)
		}
	}

	config, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	for _, worktree := range worktrees {
		absPath, err := filepath.Abs(worktree)
		if err != nil {
			t.Fatalf("resolve worktree path: %v", err)
		}
		wantHeader := `[projects."` + tomlBasicStringEscape(absPath) + `"]`
		if !strings.Contains(string(config), wantHeader+"\ntrust_level = \"trusted\"") {
			t.Fatalf("codex config missing trusted worktree block for %s:\n%s", worktree, config)
		}
	}
}

// TestConfigureFinalizeHookReturnsUnsupported asserts that codex declines
// finalize-via-hook (no in-CLI Stop-hook surface). The daemon falls back
// to ExitStatus polling for codex sessions.
func TestConfigureFinalizeHookReturnsUnsupported(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.ConfigureFinalizeHook(context.Background(), &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir: t.TempDir(), SessionId: "s1", HookToken: "tkn", HookPort: 12345,
	})
	if err != nil {
		t.Fatalf("ConfigureFinalizeHook: %v", err)
	}
	if resp.IsSupported {
		t.Error("IsSupported = true, want false for codex")
	}
}

func TestRemoveAgentRunHookReturnsUnsupported(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.RemoveAgentRunHook(context.Background(), &bossanovav1.RemoveAgentRunHookRequest{
		WorkDir: t.TempDir(), AgentSessionId: "agent-1",
	})
	if err != nil {
		t.Fatalf("RemoveAgentRunHook: %v", err)
	}
	if resp.IsSupported {
		t.Error("IsSupported = true, want false for codex")
	}
}

func TestAgentRunnerServiceDescIncludesRemoveAgentRunHook(t *testing.T) {
	for _, method := range agentRunnerServiceDesc.Methods {
		if method.MethodName == "RemoveAgentRunHook" {
			return
		}
	}
	t.Fatal("agentRunnerServiceDesc missing RemoveAgentRunHook")
}

// TestGetInfoIncludesCodexUserSettings verifies the three codex-specific
// user settings (sandbox / approval / model) are advertised so the
// settings UI can surface them without a separate codex-aware path.
//
// sandbox and approval are advertised as ENUM with AllowedValues so the
// TUI cycle picker constrains user input to values codex actually
// accepts; model is STRING because codex permits arbitrary model
// identifiers.
func TestGetInfoIncludesCodexUserSettings(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.GetInfo(context.Background(), &bossanovav1.AgentRunnerServiceGetInfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if resp.Info == nil {
		t.Fatal("Info nil")
	}
	if resp.Info.Name != "codex" {
		t.Errorf("Info.Name = %q, want codex", resp.Info.Name)
	}

	byKey := map[string]*bossanovav1.UserSetting{}
	for _, us := range resp.Info.UserSettings {
		byKey[us.Key] = us
	}

	for _, key := range []string{"sandbox", "approval", "model", "dangerously_bypass_approvals_and_sandbox"} {
		if byKey[key] == nil {
			t.Errorf("user setting %q missing from GetInfo", key)
		}
	}

	// dangerously_bypass_approvals_and_sandbox: BOOL, defaults to "false".
	bypass := byKey["dangerously_bypass_approvals_and_sandbox"]
	if bypass != nil {
		if bypass.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_BOOL {
			t.Errorf("bypass.Type = %v, want USER_SETTING_TYPE_BOOL", bypass.Type)
		}
		if bypass.DefaultValue != "false" {
			t.Errorf("bypass.DefaultValue = %q, want \"false\"", bypass.DefaultValue)
		}
	}

	// sandbox: ENUM, "" first, then the codex-accepted modes.
	sandbox := byKey["sandbox"]
	if sandbox != nil {
		if sandbox.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM {
			t.Errorf("sandbox.Type = %v, want USER_SETTING_TYPE_ENUM", sandbox.Type)
		}
		wantSandbox := []string{"", "read-only", "workspace-write", "danger-full-access"}
		if !equalStrings(sandbox.AllowedValues, wantSandbox) {
			t.Errorf("sandbox.AllowedValues = %v, want %v", sandbox.AllowedValues, wantSandbox)
		}
	}

	// approval: ENUM, "" first, then the codex-accepted policies.
	approval := byKey["approval"]
	if approval != nil {
		if approval.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM {
			t.Errorf("approval.Type = %v, want USER_SETTING_TYPE_ENUM", approval.Type)
		}
		wantApproval := []string{"", "untrusted", "on-failure", "on-request", "never"}
		if !equalStrings(approval.AllowedValues, wantApproval) {
			t.Errorf("approval.AllowedValues = %v, want %v", approval.AllowedValues, wantApproval)
		}
	}

	// model: STRING (codex accepts arbitrary identifiers, no fixed set).
	model := byKey["model"]
	if model != nil {
		if model.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_STRING {
			t.Errorf("model.Type = %v, want USER_SETTING_TYPE_STRING", model.Type)
		}
		if len(model.AllowedValues) != 0 {
			t.Errorf("model.AllowedValues = %v, want empty (string-typed setting)", model.AllowedValues)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBuildInteractiveCommandKeepsTTY verifies the interactive argv runs codex
// directly in the tmux pane. TmuxChat mirrors output with pipe-pane, so wrapping
// the command in a tee pipeline would make stdout non-TTY and codex exits
// before the ready marker appears.
func TestBuildInteractiveCommandKeepsTTY(t *testing.T) {
	s := newTestServer(t)
	logPath := filepath.Join(t.TempDir(), "codex.log")

	// Fresh.
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc", Resume: false, LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if len(resp.Argv) == 0 {
		t.Fatalf("Argv empty")
	}
	if resp.Argv[0] != "codex" {
		t.Fatalf("Argv expected codex directly, got %v", resp.Argv)
	}
	if contains(resp.Argv, "bash") || contains(resp.Argv, "tee") || contains(resp.Argv, logPath) {
		t.Fatalf("Argv should not tee or wrap codex output, got %v", resp.Argv)
	}
	if contains(resp.Argv, "resume") {
		t.Errorf("fresh interactive command should not contain resume: %v", resp.Argv)
	}
	if resp.ReadyMarker != "›" {
		t.Fatalf("ReadyMarker = %q, want Codex prompt marker %q", resp.ReadyMarker, "›")
	}
	if resp.CommandPrefix != "$" {
		t.Fatalf("CommandPrefix = %q, want $", resp.CommandPrefix)
	}
	if resp.ConsumesInitialInput {
		t.Fatal("ConsumesInitialInput = true with no startup input")
	}

	// Resume.
	respR, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "uuid-9", Resume: true, LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand(resume): %v", err)
	}
	want := []string{"codex", "resume", "uuid-9"}
	if !equalStrings(respR.Argv, want) {
		t.Errorf("resume argv = %v, want %v", respR.Argv, want)
	}
	if respR.ReadyMarker != "›" {
		t.Fatalf("resume ReadyMarker = %q, want Codex prompt marker %q", respR.ReadyMarker, "›")
	}
	if respR.CommandPrefix != "$" {
		t.Fatalf("resume CommandPrefix = %q, want $", respR.CommandPrefix)
	}
	if respR.ConsumesInitialInput {
		t.Fatal("resume ConsumesInitialInput = true with no startup input")
	}
}

func TestBuildInteractiveCommandEmbedsStartupCommand(t *testing.T) {
	s := newTestServer(t)

	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:      "abc",
		InitialCommand: "boss-repair",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if got, want := resp.Argv[len(resp.Argv)-1], "$boss-repair"; got != want {
		t.Fatalf("fresh command argv last = %q, want %q; argv=%v", got, want, resp.Argv)
	}
	if !resp.ConsumesInitialInput {
		t.Fatal("ConsumesInitialInput = false, want true")
	}
	if resp.ReadyMarker != "›" {
		t.Fatalf("ReadyMarker = %q, want Codex prompt marker %q", resp.ReadyMarker, "›")
	}
	if resp.CommandPrefix != "$" {
		t.Fatalf("CommandPrefix = %q, want $", resp.CommandPrefix)
	}
}

func TestBuildInteractiveCommandConvertsSlashStartupCommand(t *testing.T) {
	s := newTestServer(t)

	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:      "abc",
		InitialCommand: "/bs-sweep-mutation",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if got, want := resp.Argv[len(resp.Argv)-1], "$bs-sweep-mutation"; got != want {
		t.Fatalf("fresh command argv last = %q, want %q; argv=%v", got, want, resp.Argv)
	}
	if !resp.ConsumesInitialInput {
		t.Fatal("ConsumesInitialInput = false, want true")
	}
}

func TestBuildInteractiveCommandEmbedsStartupPromptOnResume(t *testing.T) {
	s := newTestServer(t)

	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:     "uuid-9",
		Resume:        true,
		InitialPrompt: "repair this PR",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand(resume): %v", err)
	}
	if len(resp.Argv) < 4 {
		t.Fatalf("resume argv too short: %v", resp.Argv)
	}
	if got, want := resp.Argv[:3], []string{"codex", "resume", "uuid-9"}; !equalStrings(got, want) {
		t.Fatalf("resume argv prefix = %v, want %v", got, want)
	}
	if got, want := resp.Argv[len(resp.Argv)-1], "repair this PR"; got != want {
		t.Fatalf("resume argv last = %q, want %q; argv=%v", got, want, resp.Argv)
	}
	if !resp.ConsumesInitialInput {
		t.Fatal("ConsumesInitialInput = false, want true")
	}
}

// TestBuildInteractiveCommandIncludesRunnerOptions verifies that
// sandbox/approval/model toggles flow through to the interactive command,
// matching the headless argv from runner.buildArgv.
func TestBuildInteractiveCommandIncludesRunnerOptions(t *testing.T) {
	s := newTestServer(t,
		WithSandbox("workspace-write"),
		WithApproval("on-request"),
		WithModel("gpt-5"),
	)
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc", Resume: false, LogPath: filepath.Join(t.TempDir(), "x.log"),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	for _, want := range []string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request", "--model", "gpt-5"} {
		if !contains(resp.Argv, want) {
			t.Errorf("argv missing %q: %v", want, resp.Argv)
		}
	}
}

// TestCodexBuildInteractiveCommand_RequestModelWins proves a per-request model
// overrides the plugin-global BOSS_PLUGIN_model env default.
func TestCodexBuildInteractiveCommand_RequestModelWins(t *testing.T) {
	s := newTestServer(t, WithModel("codex-default"))
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid", Model: "gpt-5-codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.Argv, "\x00")
	if !strings.Contains(joined, "--model\x00gpt-5-codex") {
		t.Fatalf("argv %v should use request model over env default", resp.Argv)
	}
	if strings.Contains(joined, "codex-default") {
		t.Fatalf("argv %v should not contain the env default when request model set", resp.Argv)
	}
}

// TestCodexBuildInteractiveCommand_EnvFallback proves an empty request model
// falls back to the plugin-global env default.
func TestCodexBuildInteractiveCommand_EnvFallback(t *testing.T) {
	s := newTestServer(t, WithModel("codex-default"))
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.Argv, "\x00")
	if !strings.Contains(joined, "--model\x00codex-default") {
		t.Fatalf("argv %v should fall back to env default model", resp.Argv)
	}
}

// TestCodexBuildArgv_RequestModelWins proves the headless path prefers the
// per-request model (carried via Options) over the env default.
func TestCodexBuildArgv_RequestModelWins(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithModel("codex-default"))
	got := r.buildArgv(agentruntime.BuildArgvInput{
		Options: map[string]string{"model": "gpt-5-codex"},
	})
	joined := strings.Join(got, "\x00")
	if !strings.Contains(joined, "--model\x00gpt-5-codex") {
		t.Fatalf("buildArgv %v should use request model over env default", got)
	}
}

// TestBuildInteractiveCommandIncludesDangerouslyBypass asserts the
// dangerously-bypass toggle flows through to the interactive command, and
// that when set it suppresses --sandbox / --ask-for-approval (codex rejects
// them combined). The interactive surface must match the headless argv.
func TestBuildInteractiveCommandIncludesDangerouslyBypass(t *testing.T) {
	s := newTestServer(t,
		WithSandbox("workspace-write"),
		WithApproval("on-request"),
		WithDangerouslyBypassApprovalsAndSandbox(true),
	)
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc", Resume: false, LogPath: filepath.Join(t.TempDir(), "x.log"),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if !contains(resp.Argv, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("argv missing --dangerously-bypass-approvals-and-sandbox: %v", resp.Argv)
	}
	if contains(resp.Argv, "--sandbox") {
		t.Errorf("argv should drop --sandbox when bypass is on: %v", resp.Argv)
	}
	if contains(resp.Argv, "--ask-for-approval") {
		t.Errorf("argv should drop --ask-for-approval when bypass is on: %v", resp.Argv)
	}
}

func TestBuildInteractiveCommandWrapsInLoginShellWhenConfigured(t *testing.T) {
	s := &Server{runner: &Runner{loginShell: "/opt/homebrew/bin/fish"}}
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:      "abc",
		InitialCommand: "boss-repair",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	wantArgv := []string{
		"/opt/homebrew/bin/fish",
		"-l",
		"-i",
		"-c",
		"exec $argv",
		"codex",
		"$boss-repair",
	}
	if !equalStrings(resp.Argv, wantArgv) {
		t.Fatalf("wrapped fish argv:\n got=%#v\nwant=%#v", resp.Argv, wantArgv)
	}
	if resp.ReadyMarker != "›" {
		t.Fatalf("ReadyMarker = %q, want Codex prompt marker %q", resp.ReadyMarker, "›")
	}
	if resp.CommandPrefix != "$" {
		t.Fatalf("CommandPrefix = %q, want $", resp.CommandPrefix)
	}
	if !resp.ConsumesInitialInput {
		t.Fatal("ConsumesInitialInput = false, want true")
	}
}

func TestBuildInteractiveCommandDoesNotWrapWhenLoginShellEmpty(t *testing.T) {
	s := &Server{runner: &Runner{}}
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if resp.Argv[0] != "codex" {
		t.Fatalf("no login shell -> direct codex argv, got %v", resp.Argv)
	}
}

// TestStopRunMissingSessionReturnsNotFound asserts the server maps an
// unknown-session Stop to codes.NotFound. Daemon callers rely on this
// status to distinguish "session never started" from a real internal
// error so they can suppress retries on already-stopped sessions.
func TestStopRunMissingSessionReturnsNotFound(t *testing.T) {
	s := newTestServer(t)
	_, err := s.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{
		SessionId: "session-that-never-started",
	})
	if err == nil {
		t.Fatal("StopRun for unknown session: want error, got nil")
	}
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("StopRun status code = %v, want NotFound", got)
	}
}

// TestIsRunningReportsRunnerState asserts the IsRunning RPC reflects
// the runner's state; for a server with no started runs, every session
// ID reports false. (The complementary "true while running" path is
// covered by main_test.go's TestRunnerEndToEndWithFakeCodex which polls
// IsRunning until the fake exits.)
func TestIsRunningReportsRunnerState(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{
		SessionId: "anything",
	})
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if resp.Running {
		t.Error("IsRunning = true for unknown session, want false")
	}
}

// TestExitStatusUnknownHeadlessRunnerSessionIsComplete documents the
// headless StartRun polling contract. Interactive tmux chats are launched via
// BuildInteractiveCommand and must not use this runner status as proof of chat
// completion.
func TestExitStatusUnknownHeadlessRunnerSessionIsComplete(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{
		SessionId: "session-the-runner-never-knew",
	})
	if err != nil {
		t.Fatalf("ExitStatus: %v", err)
	}
	if !resp.IsComplete {
		t.Error("IsComplete = false for unknown session, want true")
	}
	if resp.ExitError != "" {
		t.Errorf("ExitError = %q for unknown session, want empty", resp.ExitError)
	}
}

// TestHasQuestionPromptDelegatesToDetector exercises the RPC boundary
// against both pane shapes (approval menu / idle pane). The detector
// itself is unit-tested in question_test.go; this confirms the server
// passes PaneContent through unchanged.
func TestHasQuestionPromptDelegatesToDetector(t *testing.T) {
	s := newTestServer(t)

	// Real codex 0.129.0 footer triggers detection.
	resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{
		PaneContent: []byte("  1. Yes\n  2. No\n\nPress enter to confirm or esc to cancel\n"),
	})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(menu): %v", err)
	}
	if !resp.HasPrompt {
		t.Error("HasPrompt = false for approval menu, want true")
	}

	// Idle pane (no menu, no footer) does not trigger.
	respIdle, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{
		PaneContent: []byte("just some output\n› placeholder for input\n"),
	})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(idle): %v", err)
	}
	if respIdle.HasPrompt {
		t.Error("HasPrompt = true for idle pane, want false")
	}
}

// TestBlocksInputIgnoresScrolledOffApproval is the regression BOS-600's delivery
// gate depends on. blocks_input is what the daemon refuses a send on, and the
// pane it hands us is captured WITH scrollback — so the question cannot be "does
// an approval footer appear anywhere in this buffer?" An approval the user
// answered long ago is still in the buffer; answering yes to that would refuse
// every subsequent delivery to a chat that is sitting idle at its composer.
//
// The fixture is the real captured approval pane (testdata/panes/question.txt),
// scrolled up by appending plain output rows — the same thing the TUI does when
// the agent keeps going after an approval is answered. Nothing about the
// approval text is edited, so this cannot pass by weakening the grammar.
func TestBlocksInputIgnoresScrolledOffApproval(t *testing.T) {
	s := newTestServer(t)

	live, err := os.ReadFile("testdata/panes/question.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: live})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(live): %v", err)
	}
	if !resp.BlocksInput {
		t.Fatal("BlocksInput = false while the approval menu is on screen; the delivery gate would type into it")
	}

	scrolled := append(bytes.Clone(live), []byte(strings.Repeat("  wrote /tmp/codex-approval-test.txt\n", 40))...)
	respScrolled, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: scrolled})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(scrolled): %v", err)
	}
	if respScrolled.BlocksInput {
		t.Error("BlocksInput = true for an approval that has scrolled out of view; delivery to this chat would be refused forever")
	}
}

// TestBlocksInputSurvivesPanePadding is the other half of the tail bound, and
// the half that fails open. `tmux capture-pane` emits every row of the pane, so
// a capture carries as many trailing blank rows as the conversation is short —
// and codex draws its menu under the last output line, not at the pane bottom.
// A window counted from the raw end of the buffer is therefore spent on padding
// before it reaches the menu: the real 60-row fixture below has its approval
// footer on row 35, so four more blank rows would have hidden it. Hiding it
// means blocks_input goes false and the daemon types the payload plus Enter
// into a live approval menu — precisely the incident this branch exists to
// stop, reintroduced by the fix for the opposite one.
//
// The fixture text is again untouched; only the padding grows.
func TestBlocksInputSurvivesPanePadding(t *testing.T) {
	s := newTestServer(t)

	live, err := os.ReadFile("testdata/panes/question.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	for _, blankRows := range []int{4, 12, 40, 200} {
		padded := append(bytes.Clone(live), []byte(strings.Repeat("\n", blankRows))...)
		resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: padded})
		if err != nil {
			t.Fatalf("HasQuestionPrompt(padded %d): %v", blankRows, err)
		}
		if !resp.BlocksInput {
			t.Errorf("BlocksInput = false with %d blank pane rows below an on-screen approval menu; the delivery gate would type into it", blankRows)
		}
	}
}

// TestBlocksInputMatchesTallRequestUserInputCard pins the one modal whose
// grammar spans more than its footer: the request_user_input picker is matched
// from a "Question N/M (K unanswered)" header down to its footer, so a card
// taller than the modal window would lose the header to the slice and read as
// "no modal" while filling the screen. The card is matched against the whole
// active pane once its footer proves it reaches the bottom; a card whose footer
// has scrolled away is stale and must still be ignored.
func TestBlocksInputMatchesTallRequestUserInputCard(t *testing.T) {
	s := newTestServer(t)

	card := "  Question 1/2 (2 unanswered)\n" +
		strings.Repeat("  ▌ some long question body line\n", 40) +
		"  tab to add notes | enter to submit answer | esc to interrupt\n"

	onScreen := card + strings.Repeat("\n", 8)
	resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(onScreen)})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(on-screen card): %v", err)
	}
	if !resp.BlocksInput {
		t.Error("BlocksInput = false for a request_user_input card taller than the modal window; the delivery gate would type into it")
	}

	scrolled := card + strings.Repeat("  wrote /tmp/codex-answer.txt\n", 40)
	respScrolled, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(scrolled)})
	if err != nil {
		t.Fatalf("HasQuestionPrompt(scrolled card): %v", err)
	}
	if respScrolled.BlocksInput {
		t.Error("BlocksInput = true for a request_user_input card that has scrolled out of view; delivery to this chat would be refused forever")
	}
}

// TestTallCardEvidenceStaysScopedToTheWindow guards the two ways the tall-card
// path — the one branch allowed to read above the modal window — can be talked
// out of its answer by scrollback. Both directions are failures the window
// bound exists to prevent, so widening the header search must not drag the rest
// of the decision up with it.
func TestTallCardEvidenceStaysScopedToTheWindow(t *testing.T) {
	s := newTestServer(t)

	body := strings.Repeat("  ▌ some long question body line\n", 40)
	footer := "  tab to add notes | enter to submit answer | esc to interrupt\n"
	padding := strings.Repeat("\n", 6)

	tests := []struct {
		name string
		pane string
		want bool
		why  string
	}{
		{
			name: "stale working spinner above the card",
			// A spinner from an earlier turn stays in the buffer until
			// "• Session Complete", which a long chat may never print. Vetoing
			// pane-wide on it would switch the tall card's only defence off.
			pane: "• Working (7s • esc to interrupt)\n" +
				"  Question 1/2 (2 unanswered)\n" + body + footer + padding,
			want: true,
			why:  "a spinner from an earlier turn must not unblock a card that is on screen now",
		},
		{
			name: "working spinner inside the window below the footer",
			// The other half of the veto's job: a spinner drawn under the footer
			// is codex saying the picker is finished and the turn has resumed,
			// so the composer is free even though the card is still on screen.
			pane: "  Question 1/2 (2 unanswered)\n" + body + footer +
				"  wrote /tmp/codex-answer.txt\n" +
				"• Working (7s • esc to interrupt)\n" + padding,
			want: false,
			why:  "a spinner under the footer means the picker is done; blocking here refuses a ready pane",
		},
		{
			name: "card body quoting a header does not relocate the card",
			// Card bodies are free-form agent prose and routinely quote earlier
			// terminal output. A quoted header must not pose as the start of the
			// bottom-most card, or a live picker reads as unblocked.
			pane: "  Question 1/1 (1 unanswered)\n" + body +
				"  ▌ should I keep the state where Question 3/3 (0 unanswered) is shown?\n" +
				body + footer + padding,
			want: true,
			why:  "a header quoted inside a card body must not be mistaken for a real card header",
		},
		{
			name: "header rendered but its footer not yet drawn",
			// A capture can land between the header row and the footer row of a
			// re-render. The previous frame's footer is still on screen, so the
			// picker still owns the composer; treating the footerless newest
			// header as "no card" would fail open on that frame.
			pane: "  Question 1/2 (2 unanswered)\n" + body + footer +
				"  Question 2/2 (1 unanswered)\n" +
				strings.Repeat("  ▌ some long question body line\n", 3) + padding,
			want: true,
			why:  "a half-drawn re-render must not read as an idle composer",
		},
		{
			name: "answered bottom card under an unanswered older one",
			// The bottom card owns the composer; it is fully answered, so the
			// picker is done. An older card's counter must not vouch for it, or
			// delivery is refused for the rest of the session.
			pane: "  Question 1/3 (2 unanswered)\n" + body + footer +
				strings.Repeat("  wrote /tmp/codex-answer.txt\n", 50) +
				"  Question 3/3 (0 unanswered)\n" + body + footer + padding,
			want: false,
			why:  "an older card's unanswered counter must not keep a finished picker blocking",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(tc.pane)})
			if err != nil {
				t.Fatalf("HasQuestionPrompt: %v", err)
			}
			if resp.BlocksInput != tc.want {
				t.Errorf("BlocksInput = %v, want %v: %s", resp.BlocksInput, tc.want, tc.why)
			}
		})
	}
}

// TestLastTurnIsUserMissingTranscriptCollapsesToFalse asserts that a
// session ID with no rollout file on disk reports IsUser=false (no error).
// The daemon treats false as "don't suppress the question state", so
// errors-as-false is the correct, conservative collapse.
func TestLastTurnIsUserMissingTranscriptCollapsesToFalse(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment

	s := newTestServer(t)
	resp, err := s.LastTurnIsUser(context.Background(), &bossanovav1.LastTurnIsUserRequest{
		WorkDir:        "/anywhere",
		AgentSessionId: "uuid-without-transcript",
	})
	if err != nil {
		t.Fatalf("LastTurnIsUser: %v", err)
	}
	if resp.IsUser {
		t.Error("IsUser = true for missing transcript, want false")
	}
}

// TestTranscriptExistsRPCMatchesDiskState asserts the RPC delegates to
// transcriptExists correctly: missing → false, present → true.
func TestTranscriptExistsRPCMatchesDiskState(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment

	s := newTestServer(t)

	// Missing.
	resp, err := s.TranscriptExists(context.Background(), &bossanovav1.TranscriptExistsRequest{
		WorkDir:        "/anywhere",
		AgentSessionId: "no-such-uuid",
	})
	if err != nil {
		t.Fatalf("TranscriptExists(missing): %v", err)
	}
	if resp.Exists {
		t.Error("Exists = true for missing rollout, want false")
	}

	// Present.
	uuid := "rpc-test-uuid"
	dst := shardedRolloutPath(filepath.Join(tmpHome, codexSessionsDir), uuid)
	copyFixture(t, "testdata/transcripts/sample.jsonl", dst)
	respHave, err := s.TranscriptExists(context.Background(), &bossanovav1.TranscriptExistsRequest{
		WorkDir:        "/anywhere",
		AgentSessionId: uuid,
	})
	if err != nil {
		t.Fatalf("TranscriptExists(present): %v", err)
	}
	if !respHave.Exists {
		t.Error("Exists = false for non-empty rollout, want true")
	}
}

func TestResolveInteractiveSessionIDRPCPrefersPaneFDResolution(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	// Two sibling codex chats in the SAME worktree launched in the same
	// window. The time-window scan is ambiguous (two codex-tui rollouts), but
	// fd resolution binds THIS chat's pane to its OWN rollout.
	launchedAfter := time.Now().Add(-2 * time.Second)
	own := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", launchedAfter.Add(time.Second))
	_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", launchedAfter.Add(time.Second))

	s := newTestServer(t)
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{4242: {4242, 5252}},
		openFiles:   map[int][]string{5252: {own}},
	}
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
		PanePid:       4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if !resp.Found {
		t.Fatal("Found = false, want true")
	}
	if resp.SessionId != uuidA {
		t.Errorf("SessionId = %q, want fd-resolved %q", resp.SessionId, uuidA)
	}
	if resp.Ambiguous {
		t.Error("Ambiguous = true, want false (fd resolution is unambiguous)")
	}
}

func TestResolveInteractiveSessionIDRPCFallsBackWhenFDInspectionUnavailable(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	launchedAfter := time.Now().Add(-2 * time.Second)
	path := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", launchedAfter.Add(time.Second))

	s := newTestServer(t)
	// The process tree can't be enumerated at all (empty descendants ⇒ `ps`
	// failed / pane gone ⇒ fd inspection unavailable on this host). ONLY then do
	// we fall back to the time-window scan, which resolves the single match.
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{},
		openFiles:   map[int][]string{},
	}
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
		PanePid:       4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if !resp.Found || resp.SessionId != uuidA {
		t.Fatalf("Found=%v SessionId=%q, want true/%q via time-window fallback", resp.Found, resp.SessionId, uuidA)
	}
	if resp.TranscriptPath != path {
		t.Errorf("TranscriptPath = %q, want %q", resp.TranscriptPath, path)
	}
}

// TestResolveInteractiveSessionIDRPCWaitsForOwnFDWhenTreeVisible is the direct
// BOS-290 regression for the eager-fallback hole: when a chat's pane process
// tree IS visible but its own codex hasn't opened its rollout yet, the plugin
// must report Found=false so the daemon keeps polling for THIS chat's own fd —
// it must NOT fall through to the time-window scan and bind a sibling chat's
// already-written rollout (which the -2s window slack would otherwise surface as
// the single unambiguous match).
func TestResolveInteractiveSessionIDRPCWaitsForOwnFDWhenTreeVisible(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	// A sibling chat's rollout already exists in this worktree and sits inside
	// the target chat's time-window (the collision the eager fallback caused).
	launchedAfter := time.Now().Add(-2 * time.Second)
	_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", launchedAfter.Add(time.Second))

	s := newTestServer(t)
	// The target chat's pane tree is visible (ps enumerated it) but its codex
	// holds no rollout open yet.
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{4242: {4242, 5252}},
		openFiles:   map[int][]string{5252: {}},
	}
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
		PanePid:       4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if resp.Found {
		t.Fatalf("Found=true SessionId=%q — must NOT bind the sibling's rollout; keep polling for the chat's own fd (BOS-290)", resp.SessionId)
	}
}

func TestResolveInteractiveSessionIDRPCUsesRolloutMetadata(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment
	workDir := t.TempDir()
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	path := writeSessionMetaRollout(t,
		filepath.Join(tmpHome, codexSessionsDir),
		"rpc-session",
		workDir,
		"codex-tui",
		launchedAfter.Add(time.Second),
	)

	s := newTestServer(t)
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if !resp.Found {
		t.Fatal("Found = false, want true")
	}
	if resp.SessionId != "rpc-session" {
		t.Errorf("SessionId = %q, want rpc-session", resp.SessionId)
	}
	if resp.TranscriptPath != path {
		t.Errorf("TranscriptPath = %q, want %q", resp.TranscriptPath, path)
	}
	if resp.Ambiguous {
		t.Error("Ambiguous = true, want false")
	}
	if resp.Reason != "" {
		t.Errorf("Reason = %q, want empty", resp.Reason)
	}
}

// TestGetChatTitleSupportedEvenWithoutTranscript asserts GetChatTitle
// always reports Supported=true (the codex plugin can ALWAYS attempt
// title extraction) and returns an empty Title when the transcript is
// missing. The daemon uses Supported to decide whether to fall back to
// a generic placeholder; an empty Title with Supported=true tells it
// "we tried, transcript not ready yet".
func TestGetChatTitleSupportedEvenWithoutTranscript(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment

	s := newTestServer(t)
	resp, err := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir:   "/anywhere",
		SessionId: "uuid-without-transcript",
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if !resp.Supported {
		t.Error("Supported = false, want true (codex plugin always supports title extraction)")
	}
	if resp.Title != "" {
		t.Errorf("Title = %q, want empty for missing transcript", resp.Title)
	}
	if resp.Explicit {
		t.Error("Explicit = true, want false when there is no session index thread name to claim authority for")
	}
}

// TestGetChatTitle_FromSessionIndexThreadNameIsExplicit locks the authority
// of a non-empty session index thread_name: it is the only representation
// codex gives a native `/rename`, so bossd must be allowed to apply it over an
// already-populated chat title (BOS-611).
func TestGetChatTitle_FromSessionIndexThreadNameIsExplicit(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	sessionID := "019f3a91-a6bb-7492-9173-976fa6354cc0"
	sessionsRoot := filepath.Join(codexHome, "sessions")
	writeSessionMetaRollout(t, sessionsRoot, sessionID, t.TempDir(), "codex-tui", time.Now())

	index := `{"id":"older","thread_name":"Ignore me","updated_at":"2026-07-07T03:10:00Z"}` + "\n" +
		`{"id":"` + sessionID + `","thread_name":"Rename Test","updated_at":"2026-07-07T03:14:20Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(index), 0o600); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	s := newTestServer(t)
	resp, err := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir: "/anywhere", SessionId: sessionID,
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if resp.Title != "Rename Test" {
		t.Errorf("Title = %q, want session index thread name", resp.Title)
	}
	if !resp.Explicit {
		t.Error("Explicit = false, want true for a non-empty session index thread name")
	}
}

// TestGetChatTitle_SessionIndexLatestSameIDWins locks the "latest wins"
// scan: if the index ever carries more than one entry for a session id, the
// last non-empty thread_name is the one surfaced.
func TestGetChatTitle_SessionIndexLatestSameIDWins(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	sessionID := "019f3a91-a6bb-7492-9173-976fa6354cc0"
	sessionsRoot := filepath.Join(codexHome, "sessions")
	writeSessionMetaRollout(t, sessionsRoot, sessionID, t.TempDir(), "codex-tui", time.Now())

	index := `{"id":"` + sessionID + `","thread_name":"First Name","updated_at":"2026-07-07T03:10:00Z"}` + "\n" +
		`{"id":"` + sessionID + `","thread_name":"Latest Name","updated_at":"2026-07-07T03:14:20Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(index), 0o600); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	s := newTestServer(t)
	resp, err := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir: "/anywhere", SessionId: sessionID,
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if resp.Title != "Latest Name" {
		t.Errorf("Title = %q, want latest thread name", resp.Title)
	}
	if !resp.Explicit {
		t.Error("Explicit = false, want true for latest thread name")
	}
}

// TestGetChatTitle_EmptyThreadNameFallsBackToRollout verifies that an
// explicit-but-empty thread_name does not produce an explicit title: it must
// fall back to the non-explicit rollout first-message so bossd never treats a
// cleared name as a curated one.
func TestGetChatTitle_EmptyThreadNameFallsBackToRollout(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")

	sessionID := "019f3a91-a6bb-7492-9173-976fa6354cc0"
	dst := shardedRolloutPath(filepath.Join(tmpHome, codexSessionsDir), sessionID)
	copyFixture(t, "testdata/transcripts/sample.jsonl", dst)

	index := `{"id":"` + sessionID + `","thread_name":"","updated_at":"2026-07-07T03:14:20Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(tmpHome, ".codex", "session_index.jsonl"), []byte(index), 0o600); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	s := newTestServer(t)
	resp, err := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{
		WorkDir: "/anywhere", SessionId: sessionID,
	})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if resp.Title != "say hello and exit" {
		t.Errorf("Title = %q, want rollout first message", resp.Title)
	}
	if resp.Explicit {
		t.Error("Explicit = true, want false when thread_name is empty")
	}
}

// TestListIgnoredDirtyFilesReturnsEmptySlice asserts the codex plugin's
// ignoredDirtyFiles is empty (no .claude/settings.local.json equivalent),
// and that the response carries an empty (non-nil) slice — the daemon
// type-asserts on Paths length.
func TestListIgnoredDirtyFilesReturnsEmptySlice(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.ListIgnoredDirtyFiles(context.Background(), &bossanovav1.ListIgnoredDirtyFilesRequest{
		WorkDir: "/anywhere",
	})
	if err != nil {
		t.Fatalf("ListIgnoredDirtyFiles: %v", err)
	}
	if len(resp.Paths) != 0 {
		t.Errorf("Paths = %v, want empty", resp.Paths)
	}
}
