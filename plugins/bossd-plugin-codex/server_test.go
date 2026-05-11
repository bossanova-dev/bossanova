package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// TestBuildInteractiveCommandUsesLogTeeArgv verifies the interactive argv
// is wrapped in LogTeeArgv (bash -c '... 2>&1 | tee <log>'), that
// fresh sessions invoke `codex` directly without `resume`, and that resume
// sessions append `resume <UUID>` as a positional subcommand. The shell
// must be bash (not sh) because LogTeeArgv uses `set -o pipefail`, a
// bash extension that fails on dash/ash.
func TestBuildInteractiveCommandUsesLogTeeArgv(t *testing.T) {
	s := newTestServer(t)
	logPath := "/tmp/codex.log"

	// Fresh.
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "abc", Resume: false, LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if len(resp.Argv) < 3 || resp.Argv[0] != "bash" || resp.Argv[1] != "-c" {
		t.Fatalf("Argv expected bash -c <script>, got %v", resp.Argv)
	}
	script := resp.Argv[2]
	if !strings.Contains(script, "codex") {
		t.Errorf("script does not invoke codex: %q", script)
	}
	if strings.Contains(script, "resume") {
		t.Errorf("fresh interactive command should not contain 'resume': %q", script)
	}
	if !strings.Contains(script, "tee") || !strings.Contains(script, logPath) {
		t.Errorf("script does not tee to log path: %q", script)
	}

	// Resume.
	respR, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: "uuid-9", Resume: true, LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand(resume): %v", err)
	}
	if !strings.Contains(respR.Argv[2], "resume uuid-9") {
		t.Errorf("resume script missing 'resume uuid-9' positional: %q", respR.Argv[2])
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
		SessionId: "abc", Resume: false, LogPath: "/tmp/x.log",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	script := resp.Argv[2]
	for _, want := range []string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request", "--model", "gpt-5"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q: %s", want, script)
		}
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
		SessionId: "abc", Resume: false, LogPath: "/tmp/x.log",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	script := resp.Argv[2]
	if !strings.Contains(script, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("script missing --dangerously-bypass-approvals-and-sandbox: %s", script)
	}
	if strings.Contains(script, "--sandbox") {
		t.Errorf("script should drop --sandbox when bypass is on: %s", script)
	}
	if strings.Contains(script, "--ask-for-approval") {
		t.Errorf("script should drop --ask-for-approval when bypass is on: %s", script)
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

// TestExitStatusUnknownSessionIsComplete asserts ExitStatus for a session
// the runner doesn't know about reports IsComplete=true with an empty
// ExitError. The poll-fallback loop relies on this so a stale session ID
// (e.g. daemon restart bookkeeping) doesn't pin the daemon in IsComplete=false
// forever.
func TestExitStatusUnknownSessionIsComplete(t *testing.T) {
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

// TestLastTurnIsUserMissingTranscriptCollapsesToFalse asserts that a
// session ID with no rollout file on disk reports IsUser=false (no error).
// The daemon treats false as "don't suppress the question state", so
// errors-as-false is the correct, conservative collapse.
func TestLastTurnIsUserMissingTranscriptCollapsesToFalse(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

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

func TestResolveInteractiveSessionIDRPCUsesRolloutMetadata(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
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
