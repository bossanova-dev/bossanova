package main

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
)

// fakeClaude returns an agentruntime.CommandFactory that runs /bin/sh -c "$script"
// instead of the real claude binary. Used by the server tests to exercise the
// subprocess plumbing without requiring claude to be installed.
func fakeClaude(t *testing.T, script string) agentruntime.CommandFactory {
	t.Helper()
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

func TestBuildArgvIncludesModelFromOptions(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	got := r.buildArgv(agentruntime.BuildArgvInput{
		SessionID: "x", ProvidedSessionID: true,
		Options: map[string]string{"model": "sonnet"},
	})
	joined := strings.Join(got, "\x00")
	if !strings.Contains(joined, "--model\x00sonnet") {
		t.Errorf("buildArgv = %v, want --model sonnet", got)
	}
}

func TestBuildArgvNoModelWhenEmptyOption(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	got := r.buildArgv(agentruntime.BuildArgvInput{
		SessionID: "x", ProvidedSessionID: true,
		Options: map[string]string{"model": ""},
	})
	for _, a := range got {
		if a == "--model" {
			t.Fatalf("buildArgv = %v, want no --model for empty option", got)
		}
	}
}

func TestBuildArgvIncludesResume(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	resume := "abc"
	got := r.buildArgv(agentruntime.BuildArgvInput{
		Resume: &resume, SessionID: "x", ProvidedSessionID: true,
	})
	want := []string{
		"claude", "--print", "--verbose", "--output-format", "stream-json",
		"--resume", "abc", "--session-id", "x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgv = %v, want %v", got, want)
	}
}

func TestClaudeBuildArgv_WrapsInLoginShell(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_login_shell", "/bin/bash")
	r := NewRunner(zerolog.Nop(), runnerOptsFromEnv()...)
	argv := r.buildArgv(agentruntime.BuildArgvInput{})
	want := []string{
		"/bin/bash",
		"-l",
		"-c",
		`if [ -r "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi; exec "$@"`,
		"bash",
		"claude",
		"--print",
		"--verbose",
		"--output-format",
		"stream-json",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("wrapped bash argv:\n got=%#v\nwant=%#v", argv, want)
	}
}

func TestBuildArgvIncludesDangerouslySkipPermissionsWhenSet(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithDangerouslySkipPermissions(true))
	got := r.buildArgv(agentruntime.BuildArgvInput{SessionID: "x", ProvidedSessionID: true})
	found := false
	for _, a := range got {
		if a == "--dangerously-skip-permissions" {
			found = true
		}
	}
	if !found {
		t.Errorf("--dangerously-skip-permissions missing from %v", got)
	}
}

func TestBuildArgvOmitsSessionIDWhenNotProvided(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	got := r.buildArgv(agentruntime.BuildArgvInput{SessionID: "x", ProvidedSessionID: false})
	for _, a := range got {
		if a == "--session-id" {
			t.Errorf("--session-id should not appear when ProvidedSessionID is false: %v", got)
		}
	}
}
