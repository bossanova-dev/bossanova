package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeClaude returns an agentruntime.CommandFactory that runs /bin/sh -c "$script"
// instead of the real claude binary. Used by the server tests to exercise the
// subprocess plumbing without requiring claude to be installed.
func fakeClaude(t *testing.T, script string) agentruntime.CommandFactory {
	t.Helper()
	return func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if slices.Contains(args, "--help") {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s\n' '--strict-mcp-config --setting-sources'")
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

func TestBuildArgvIncludesModelFromOptions(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true,
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
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true,
		SessionID: "x", ProvidedSessionID: true,
		Options: map[string]string{"model": ""},
	})
	for _, a := range got {
		if a == "--model" {
			t.Fatalf("buildArgv = %v, want no --model for empty option", got)
		}
	}
}

// Trusted repos retain native MCP configuration: Claude Code discovers servers its own
// native way (project .mcp.json gated by enabledMcpjsonServers, plus the user
// and local scopes) and the repo declares them. Emitting --strict-mcp-config
// would suppress exactly that, so neither flag may reach argv — including when
// a stale caller still sets the retired options, which is why this passes them.
func TestBuildArgvTrustedNeverEmitsMcpFlags(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true, Options: map[string]string{
		"managed_mcp_config_path":      "/data/bossanova/mcp-configs/sid.json",
		"is_strict_managed_mcp_config": "true",
	}})
	for _, a := range got {
		if a == "--mcp-config" || a == "--strict-mcp-config" {
			t.Fatalf("buildArgv = %v, want no %s", got, a)
		}
	}
}

func TestBuildArgvIncludesResume(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	resume := "abc"
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true,
		Resume: &resume, SessionID: "x", ProvidedSessionID: true,
	})
	want := []string{
		"claude", "--print", "--verbose", "--output-format", "stream-json",
		"--resume", "abc", "--session-id", "x", "--effort", defaultClaudeEffort,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgv = %v, want %v", got, want)
	}
}

func TestClaudeBuildArgv_WrapsInLoginShell(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_login_shell", "/bin/bash")
	r := NewRunner(zerolog.Nop(), runnerOptsFromEnv()...)
	argv := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true})
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
		"--effort",
		defaultClaudeEffort,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("wrapped bash argv:\n got=%#v\nwant=%#v", argv, want)
	}
}

func TestBuildArgvIncludesDangerouslySkipPermissionsWhenSet(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithDangerouslySkipPermissions(true))
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true, SessionID: "x", ProvidedSessionID: true})
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
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true, SessionID: "x", ProvidedSessionID: false})
	for _, a := range got {
		if a == "--session-id" {
			t.Errorf("--session-id should not appear when ProvidedSessionID is false: %v", got)
		}
	}
}

func TestBuildArgvFallsBackToPluginModel(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithModel("opus[1m]"))
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true, Options: map[string]string{"model": ""}})
	joined := strings.Join(got, "\x00")
	if !strings.Contains(joined, "--model\x00opus[1m]") {
		t.Fatalf("buildArgv = %v, want plugin default --model opus[1m]", got)
	}
}

func TestBuildArgvRequestModelBeatsPluginModel(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithModel("opus[1m]"))
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true, Options: map[string]string{"model": "sonnet"}})
	joined := strings.Join(got, "\x00")
	if !strings.Contains(joined, "--model\x00sonnet") {
		t.Fatalf("buildArgv = %v, want per-request --model sonnet to win", got)
	}
	if strings.Contains(joined, "opus[1m]") {
		t.Fatalf("buildArgv = %v, plugin default must not also appear", got)
	}
}

func TestBuildArgvEffortPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		defaultVal string
		requestVal string
		want       string
	}{
		{name: "request wins", defaultVal: "high", requestVal: "medium", want: "medium"},
		{name: "plugin fallback", defaultVal: "high", requestVal: "", want: "high"},
		{name: "both empty", defaultVal: "", requestVal: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRunner(zerolog.Nop(), WithEffort(tt.defaultVal))
			got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true, Options: map[string]string{"effort": tt.requestVal}})
			joined := strings.Join(got, "\x00")
			if tt.want == "" {
				if strings.Contains(joined, "--effort") {
					t.Fatalf("buildArgv = %v, want no --effort", got)
				}
				return
			}
			if !strings.Contains(joined, "--effort\x00"+tt.want) {
				t.Fatalf("buildArgv = %v, want --effort %s", got, tt.want)
			}
			if tt.defaultVal != "" && tt.requestVal != "" && strings.Contains(joined, "\x00"+tt.defaultVal+"\x00") {
				t.Fatalf("buildArgv = %v, plugin default must not also appear", got)
			}
		})
	}
}

func TestBuildArgvUsesBuiltInEffortDefault(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true, Options: map[string]string{"effort": ""}})
	if !strings.Contains(strings.Join(got, "\x00"), "--effort\x00"+defaultClaudeEffort) {
		t.Fatalf("buildArgv = %v, want built-in --effort %s", got, defaultClaudeEffort)
	}
}

// A bracketed context-window variant must reach argv byte-for-byte. It is passed
// through loginshell.Wrap as a positional arg, so no shell ever globs it; this
// pins that and guards against a future sanitiser stripping the suffix.
func TestBuildArgvPreservesBracketedModelVariant(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithLoginShell("/opt/homebrew/bin/fish"), WithModel("opus[1m]"))
	got := r.buildArgv(agentruntime.BuildArgvInput{ShouldTrustProjectConfig: true})
	var found bool
	for i, a := range got {
		if a == "--model" {
			if i+1 >= len(got) || got[i+1] != "opus[1m]" {
				t.Fatalf("buildArgv = %v, want the model arg to be exactly opus[1m]", got)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("buildArgv = %v, want --model present", got)
	}
}

func TestBuildArgvUntrustedDisablesProjectConfiguration(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	got := strings.Join(r.buildArgv(agentruntime.BuildArgvInput{}), "\x00")
	for _, want := range []string{"--strict-mcp-config", "--setting-sources\x00user"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv %q missing %q", got, want)
		}
	}
}

func TestProjectIsolationSupportFailsClosed(t *testing.T) {
	for _, missing := range []string{"--strict-mcp-config", "--setting-sources"} {
		t.Run(missing, func(t *testing.T) {
			supported := "--setting-sources"
			if missing == supported {
				supported = "--strict-mcp-config"
			}
			factory := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' "+supported)
			}
			r := NewRunner(zerolog.Nop(), WithCommandFactory(factory))
			_, err := r.StartWithProjectConfigTrust(context.Background(), t.TempDir(), "plan", nil, "sid", "", "", "", nil, false)
			if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "boss repo update <id> --trusted") {
				t.Fatalf("missing %s: %v", missing, err)
			}
			srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
			_, err = srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{ProjectTrust: bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE})
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("interactive missing %s: %v", missing, err)
			}
		})
	}
}

func TestStartRunPropagatesProjectConfigurationTrust(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%v", trusted), func(t *testing.T) {
			var launchArgs []string
			helpCalls := 0
			factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				if slices.Contains(args, "--help") {
					helpCalls++
					return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' '--strict-mcp-config --setting-sources'")
				}
				launchArgs = slices.Clone(args)
				return exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null")
			}
			srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
			dir := t.TempDir()
			resp, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: dir, Plan: "plan", SessionId: "sid", LogPath: filepath.Join(dir, "log"), ShouldTrustProjectConfig: trusted})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := srv.runner.Stop(resp.SessionId); err != nil {
					t.Errorf("stop test run: %v", err)
				}
			})
			wantHelpCalls := 1
			if trusted {
				wantHelpCalls = 0
			}
			if helpCalls != wantHelpCalls {
				t.Fatalf("help probes = %d, want %d", helpCalls, wantHelpCalls)
			}
			for _, flag := range []string{"--strict-mcp-config", "--setting-sources"} {
				if got := slices.Contains(launchArgs, flag); got == trusted {
					t.Fatalf("trusted %v: argv %v", trusted, launchArgs)
				}
			}
		})
	}
}
