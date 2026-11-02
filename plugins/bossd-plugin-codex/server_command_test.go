package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

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

// Boss does not own MCP configuration. Codex resolves servers from
// $CODEX_HOME/config.toml plus the repo's own .codex/config.toml, and the repo
// declares them — so the argv must carry neither a `-c mcp_servers.*` override
// nor a synthetic CODEX_HOME pinned ahead of the binary. It was confirmed
// failing against the pre-change server.go (which pinned
// `env CODEX_HOME=<managed>.codex-home codex`) before that branch was deleted;
// the fields it set are now retired from the proto, so this stands as a
// reintroduction guard.
func TestBuildInteractiveCommand_NoManagedMcpWiring(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	srv := &Server{}
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "agent-1",
		LogPath:      filepath.Join(t.TempDir(), "codex.log"),
		WorktreePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	argv := resp.GetArgv()
	for i, a := range argv {
		if strings.HasPrefix(a, "CODEX_HOME=") {
			t.Fatalf("argv pins an isolated CODEX_HOME at %d: %v", i, argv)
		}
		if strings.HasPrefix(a, "mcp_servers.") {
			t.Fatalf("argv carries a managed MCP override at %d: %v", i, argv)
		}
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
		ProjectTrust: bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
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

// TestBuildInteractiveCommandLeavesUntrustedWorktreeUntrusted pins BOS-1562:
// a request with no trust decision never writes project trust into the user's
// Codex config, so a repo's .codex/config.toml stays behind Codex's own trust
// prompt until the user accepts it.
func TestBuildInteractiveCommandLeavesUntrustedWorktreeUntrusted(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	srv := &Server{}

	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "agent-1",
		WorktreePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if len(resp.Argv) == 0 {
		t.Fatal("Argv is empty")
	}
	if _, err := os.Stat(filepath.Join(codexHome, "config.toml")); !os.IsNotExist(err) {
		config, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
		t.Fatalf("codex config written for an untrusted repo (stat err %v):\n%s", err, config)
	}
}

// TestBuildInteractiveCommandRevokesTrustForUntrustedRepo pins that a
// worktree pre-trusted while its repo was trusted loses that trust once bossd
// sends PROJECT_TRUST_DECISION_REVOKE (boss repo update --trusted=false): the
// entry becomes an explicit "untrusted", so Codex neither loads the repo's
// .codex/config.toml nor prompts for it.
func TestBuildInteractiveCommandRevokesTrustForUntrustedRepo(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	worktree := t.TempDir()
	srv := &Server{}
	req := &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "agent-1",
		WorktreePath: worktree,
		ProjectTrust: bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
	}
	if _, err := srv.BuildInteractiveCommand(context.Background(), req); err != nil {
		t.Fatalf("trusted BuildInteractiveCommand: %v", err)
	}
	trustLine := `[projects."` + tomlBasicStringEscape(worktree) + `"]` + "\ntrust_level = \"trusted\""
	config, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil || !strings.Contains(string(config), trustLine) {
		t.Fatalf("precondition: worktree not trusted (err %v):\n%s", err, config)
	}

	req.ProjectTrust = bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE
	if _, err := srv.BuildInteractiveCommand(context.Background(), req); err != nil {
		t.Fatalf("untrusted BuildInteractiveCommand: %v", err)
	}
	config, err = os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	if strings.Contains(string(config), `trust_level = "trusted"`) || !strings.Contains(string(config), `trust_level = "untrusted"`) {
		t.Fatalf("worktree not explicitly Codex-untrusted after trust was revoked:\n%s", config)
	}
}

// TestBuildInteractiveCommandWithoutTrustDecisionKeepsTrust pins that a
// request carrying no trust decision (PROJECT_TRUST_DECISION_UNSPECIFIED)
// leaves an existing trust entry alone. bossd sends exactly that for metadata-only requests — a launch
// preview and input rendering for a live pane — of a TRUSTED repo, so
// treating it as a revoke would strip that repo's Codex trust.
func TestBuildInteractiveCommandWithoutTrustDecisionKeepsTrust(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	worktree := t.TempDir()
	srv := &Server{}
	if _, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "agent-1",
		WorktreePath: worktree,
		ProjectTrust: bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
	}); err != nil {
		t.Fatalf("trusted BuildInteractiveCommand: %v", err)
	}
	trustLine := `[projects."` + tomlBasicStringEscape(worktree) + `"]` + "\ntrust_level = \"trusted\""

	if _, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:     "agent-1",
		Resume:        true,
		WorktreePath:  worktree,
		InitialPrompt: "next message",
	}); err != nil {
		t.Fatalf("metadata BuildInteractiveCommand: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	if !strings.Contains(string(config), trustLine) {
		t.Fatalf("trusted worktree lost its Codex trust on a request with no trust decision:\n%s", config)
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
			errs <- trustCodexWorktree(worktree, nil)
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
	want := []string{"codex", "resume", "uuid-9", "-c", "model_reasoning_effort=" + defaultCodexEffort, "-c", "tui.disable_paste_burst=true"}
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

func TestCodexBuildInteractiveCommand_EffortPrecedence(t *testing.T) {
	s := newTestServer(t, WithEffort("high"))
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
		Effort:    "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.Argv, "\x00")
	if !strings.Contains(joined, "-c\x00model_reasoning_effort=medium") {
		t.Fatalf("argv %v should use request effort over env default", resp.Argv)
	}
	if strings.Contains(joined, "model_reasoning_effort=high") {
		t.Fatalf("argv %v should not contain env default when request effort set", resp.Argv)
	}

	resp, err = s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(resp.Argv, "\x00")
	if !strings.Contains(joined, "-c\x00model_reasoning_effort=high") {
		t.Fatalf("argv %v should fall back to env default effort", resp.Argv)
	}

	s = newTestServer(t)
	resp, err = s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "sid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(resp.Argv, "\x00"), "-c\x00model_reasoning_effort="+defaultCodexEffort) {
		t.Fatalf("argv %v want built-in default effort %s", resp.Argv, defaultCodexEffort)
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

// A live-pane send types its text and presses Enter at once; with codex's paste
// burst on, that Enter becomes a literal newline and the message never submits.
func TestBuildInteractiveCommandDisablesPasteBurst(t *testing.T) {
	for name, req := range map[string]*bossanovav1.BuildInteractiveCommandRequest{
		"fresh":       {SessionId: "abc"},
		"resume":      {SessionId: "uuid-9", Resume: true},
		"no runner":   {SessionId: "abc"},
		"with prompt": {SessionId: "abc", InitialPrompt: "hello"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{runner: &Runner{}}
			if name == "no runner" {
				s = &Server{}
			}
			resp, err := s.BuildInteractiveCommand(context.Background(), req)
			if err != nil {
				t.Fatalf("BuildInteractiveCommand: %v", err)
			}
			joined := strings.Join(resp.Argv, "\x00")
			if !strings.Contains(joined, "-c\x00tui.disable_paste_burst=true") {
				t.Fatalf("argv lacks the paste-burst override: %v", resp.Argv)
			}
			if sep := strings.Index(joined, "\x00--\x00"); sep >= 0 && strings.Index(joined, "tui.disable_paste_burst") > sep {
				t.Fatalf("paste-burst override lands after `--`, where codex reads it as prompt: %v", resp.Argv)
			}
		})
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
		"-c",
		"tui.disable_paste_burst=true",
		"--",
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

// The rendered first input travels through bossd's private file, not argv:
// the plugin writes it 0600 and argv reads it after `--` at exec time.
func TestBuildInteractiveCommandWritesStartupInputToFile(t *testing.T) {
	for name, req := range map[string]*bossanovav1.BuildInteractiveCommandRequest{
		"command": {SessionId: "abc", InitialCommand: "/boss-repair 42"},
		"prompt":  {SessionId: "abc", InitialPrompt: "exec"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{runner: &Runner{loginShell: "/opt/homebrew/bin/fish"}}
			path := filepath.Join(t.TempDir(), "abc.initial-input")
			req.InitialInputPath = path
			resp, err := s.BuildInteractiveCommand(context.Background(), req)
			if err != nil {
				t.Fatalf("BuildInteractiveCommand: %v", err)
			}
			if !resp.GetConsumesInitialInput() {
				t.Fatal("ConsumesInitialInput = false, want true")
			}
			want := codexInitialInput(req)
			for _, a := range resp.GetArgv() {
				if a == want {
					t.Fatalf("rendered input %q must not be inlined: %#v", want, resp.GetArgv())
				}
			}
			if !contains(resp.GetArgv(), path) {
				t.Fatalf("argv must reference the input file: %#v", resp.GetArgv())
			}
			if got := resp.GetArgv()[len(resp.GetArgv())-1]; got != "--" {
				t.Fatalf("argv must end with -- so the file read lands after it: %#v", resp.GetArgv())
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("input file not written: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Fatalf("input file mode = %o, want 600", perm)
			}
			if b, _ := os.ReadFile(path); string(b) != want {
				t.Fatalf("input file = %q, want rendered %q", b, want)
			}
		})
	}
}

// Inline delivery (no login shell, or no file) still separates the input with
// `--`, so a prompt naming a subcommand ("exec", "review") stays a prompt.
func TestBuildInteractiveCommandSeparatesInlineInputWithDoubleDash(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc", InitialPrompt: "review",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	argv := resp.GetArgv()
	if got := argv[len(argv)-2:]; !equalStrings(got, []string{"--", "review"}) {
		t.Fatalf("argv tail = %v, want [-- review]; argv=%v", got, argv)
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

// Codex confirms a consumed first input through its rollout (bossd reads a user
// message timestamped at or after launch), so it declares can_confirm_delivery
// exactly when argv carries that input.
func TestBuildInteractiveCommandDeclaresDeliveryConfirmation(t *testing.T) {
	s := newTestServer(t)
	cases := map[string]struct {
		req  *bossanovav1.BuildInteractiveCommandRequest
		want bool
	}{
		"prompt":       {req: &bossanovav1.BuildInteractiveCommandRequest{SessionId: "a", InitialPrompt: "do the work"}, want: true},
		"command":      {req: &bossanovav1.BuildInteractiveCommandRequest{SessionId: "a", InitialCommand: "boss-repair"}, want: true},
		"resume input": {req: &bossanovav1.BuildInteractiveCommandRequest{SessionId: "a", Resume: true, InitialPrompt: "continue"}, want: true},
		"no input":     {req: &bossanovav1.BuildInteractiveCommandRequest{SessionId: "a"}, want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := s.BuildInteractiveCommand(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("BuildInteractiveCommand: %v", err)
			}
			if got := resp.GetCanConfirmDelivery(); got != tc.want {
				t.Fatalf("CanConfirmDelivery = %v, want %v", got, tc.want)
			}
			if got := resp.GetCanConfirmDeliveryViaTranscript(); got != tc.want {
				t.Fatalf("transcript confirmation = %v, want %v", got, tc.want)
			}
			if resp.GetCanConfirmDelivery() && !resp.GetConsumesInitialInput() {
				t.Fatal("cannot confirm an input argv does not consume")
			}
		})
	}
}
