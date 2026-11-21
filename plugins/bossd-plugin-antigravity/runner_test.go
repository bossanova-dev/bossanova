package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestStartAgentRunReturnsConversationIDAndDeliversPlanOnStdin(t *testing.T) {
	dir := t.TempDir()
	script := `printf '%s\n' "$@" > "$BOSS_TEST_OUT/argv"; echo 'agy: starting' >&2; echo '` + initLine(conversationA) + `'; cat > "$BOSS_TEST_OUT/stdin"`
	t.Setenv("BOSS_TEST_OUT", dir)
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(script)))
	plan := "line one\nline \"two\" with 'quotes'"

	resp, err := s.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: t.TempDir(), Plan: plan, SessionId: "boss-key", LogPath: filepath.Join(dir, "run.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSessionId() != conversationA {
		t.Fatalf("session id = %q, want the conversation id %q", resp.GetSessionId(), conversationA)
	}
	waitRun(t, s, conversationA)
	exit, err := s.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: conversationA})
	if err != nil || !exit.GetIsComplete() || exit.GetExitError() != "" {
		t.Fatalf("exit status by conversation id = %v, %v; want a clean completed run", exit, err)
	}

	stdin, err := os.ReadFile(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	var turn struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(stdin, &turn); err != nil || turn.Event != "user" || turn.Message.Content != plan {
		t.Fatalf("stdin = %q (%v); want one stream-json user turn carrying the plan", stdin, err)
	}
	argv, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "line one") {
		t.Fatalf("plan leaked onto argv: %q", argv)
	}
}

func TestStartAgentRunFailsWhenAgyDoesNotResume(t *testing.T) {
	script := `echo 'warning: conversation "` + conversationA + `" not found' >&2; echo '` + initLine(conversationB) + `'; exec sleep 60`
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(script)))
	resume := conversationA

	_, err := s.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: t.TempDir(), Plan: "continue", ResumeId: &resume, SessionId: "boss-key", LogPath: filepath.Join(t.TempDir(), "run.log"),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition: a resume that lost its history must not run the plan", err)
	}
	waitRun(t, s, "boss-key")
}

func TestStartAgentRunWithoutInitReturnsProcessKey(t *testing.T) {
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`echo 'no init here'; exit 2`)))

	resp, err := s.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: t.TempDir(), SessionId: "boss-key", LogPath: filepath.Join(t.TempDir(), "run.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSessionId() != "boss-key" {
		t.Fatalf("session id = %q, want the process key", resp.GetSessionId())
	}
	waitRun(t, s, "boss-key")
	exit, err := s.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: "boss-key"})
	if err != nil || !exit.GetIsComplete() || exit.GetExitError() == "" {
		t.Fatalf("exit status = %v, %v; want the failed exit reported", exit, err)
	}
}

func TestStopRunByConversationID(t *testing.T) {
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`echo '`+initLine(conversationA)+`'; exec sleep 60`)))
	if _, err := s.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: t.TempDir(), Plan: "work", LogPath: filepath.Join(t.TempDir(), "run.log"),
	}); err != nil {
		t.Fatal(err)
	}
	running, _ := s.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: conversationA})
	if !running.GetRunning() {
		t.Fatal("run is not reported running under its conversation id")
	}
	if _, err := s.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{SessionId: conversationA}); err != nil {
		t.Fatal(err)
	}
	running, _ = s.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: conversationA})
	if running.GetRunning() {
		t.Fatal("run still running after StopRun")
	}
}

func TestHeadlessArgv(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realResolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	resume := conversationA
	tests := []struct {
		name    string
		opts    []Option
		in      agentruntime.BuildArgvInput
		want    [][]string
		without []string
	}{
		{
			name:    "fresh run uses plugin defaults",
			opts:    []Option{WithModel("gemini-4-neon-medium")},
			in:      agentruntime.BuildArgvInput{WorkDir: link},
			want:    [][]string{{"--input-format", "stream-json"}, {"--output-format", "stream-json"}, {"--model", "gemini-4-neon-medium"}},
			without: []string{"--conversation", "--dangerously-skip-permissions", "--mode", "--effort", "-p"},
		},
		{
			name: "request model and effort win, resume and permission settings apply",
			opts: []Option{WithModel("fallback"), WithMode("accept-edits"), WithDangerouslySkipPermissions(true)},
			in: agentruntime.BuildArgvInput{WorkDir: link, Resume: &resume,
				Options: map[string]string{"model": "claude-opus-5-5-high", "effort": "xhigh"}},
			want: [][]string{{"--conversation", conversationA}, {"--model", "claude-opus-5-5-high"}, {"--effort", "xhigh"},
				{"--dangerously-skip-permissions"}, {"--mode", "accept-edits"}},
			without: []string{"fallback"},
		},
		{
			name:    "an effort agy rejects is dropped",
			in:      agentruntime.BuildArgvInput{WorkDir: link, Options: map[string]string{"effort": "ultra"}},
			without: []string{"--effort", "ultra"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv := NewRunner(zerolog.Nop(), tt.opts...).buildArgv(tt.in)
			if argv[0] != "agy" {
				t.Fatalf("argv[0] = %q, want agy", argv[0])
			}
			for _, pair := range tt.want {
				if !containsSeq(argv, pair) {
					t.Errorf("argv %q lacks %q", argv, pair)
				}
			}
			for _, absent := range tt.without {
				if slices.Contains(argv, absent) {
					t.Errorf("argv %q unexpectedly has %q", argv, absent)
				}
			}
			assertTrustsBoth(t, argv, link, realResolved)
		})
	}
}

func assertTrustsBoth(t *testing.T, argv []string, dirs ...string) {
	t.Helper()
	i := slices.Index(argv, "--settings")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv %q has no --settings", argv)
	}
	var settings struct {
		TrustedWorkspaces []string `json:"trustedWorkspaces"`
	}
	if err := json.Unmarshal([]byte(argv[i+1]), &settings); err != nil {
		t.Fatalf("--settings %q is not JSON: %v", argv[i+1], err)
	}
	for _, dir := range dirs {
		if !slices.Contains(settings.TrustedWorkspaces, dir) {
			t.Errorf("trustedWorkspaces %q lacks %q", settings.TrustedWorkspaces, dir)
		}
	}
}

func containsSeq(argv, seq []string) bool {
	for i := range argv {
		if i+len(seq) <= len(argv) && slices.Equal(argv[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestRunnerOptsFromEnv(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_dangerously_skip_permissions", "true")
	t.Setenv("BOSS_PLUGIN_mode", "plan")
	t.Setenv("BOSS_PLUGIN_model", "gemini-3.8-flash-low")
	t.Setenv("BOSS_PLUGIN_login_shell", "/bin/sh")

	argv := NewRunner(zerolog.Nop(), runnerOptsFromEnv(zerolog.Nop())...).buildArgv(agentruntime.BuildArgvInput{WorkDir: t.TempDir()})

	if argv[0] != "/bin/sh" {
		t.Errorf("argv %q not wrapped in the login shell", argv)
	}
	for _, pair := range [][]string{{"--dangerously-skip-permissions"}, {"--mode", "plan"}, {"--model", "gemini-3.8-flash-low"}} {
		if !containsSeq(argv, pair) {
			t.Errorf("argv %q lacks %q", argv, pair)
		}
	}
}

func TestRunnerOptsFromEnvIgnoresUnknownMode(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_mode", "yolo")
	argv := NewRunner(zerolog.Nop(), runnerOptsFromEnv(zerolog.Nop())...).buildArgv(agentruntime.BuildArgvInput{WorkDir: t.TempDir()})
	if slices.Contains(argv, "--mode") {
		t.Fatalf("argv %q passes an unknown mode", argv)
	}
}
