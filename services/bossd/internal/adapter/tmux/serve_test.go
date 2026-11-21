package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"testing"
)

func TestServeOverridesInheritedRemainOnExit(t *testing.T) {
	factory := &mockCommandFactory{}
	client := NewClient(WithCommandFactory(factory.factory))
	if err := client.NewSession(context.Background(), NewSessionOpts{Name: "boss-repo-session-serve", WorkDir: t.TempDir(), Command: []string{"true"}, ExitOnCompletion: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{"tmux", "set-option", "-t", "boss-repo-session-serve", "remain-on-exit", "off"}
	if !slices.ContainsFunc(factory.calls, func(got []string) bool { return slices.Equal(got, want) }) {
		t.Fatalf("serve pane inherits server retain option: %v", factory.calls)
	}
}

func TestServeRetentionFailureDoesNotLeavePane(t *testing.T) {
	for _, alive := range []bool{true, false} {
		t.Run(fmt.Sprint(alive), func(t *testing.T) {
			factory := &mockCommandFactory{}
			killed := false
			client := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if slices.Equal(args, []string{"set-option", "-t", "serve", "remain-on-exit", "off"}) {
					return exec.CommandContext(ctx, "false")
				}
				if len(args) > 0 && args[0] == "has-session" && !alive {
					return exec.CommandContext(ctx, "sh", "-c", "echo \"can't find session\" >&2; exit 1")
				}
				if len(args) > 0 && args[0] == "kill-session" {
					killed = true
				}
				return factory.factory(ctx, name, args...)
			}))
			err := client.NewSession(context.Background(), NewSessionOpts{Name: "serve", WorkDir: t.TempDir(), Command: []string{"true"}, ExitOnCompletion: true})
			if (err != nil) != alive || killed != alive {
				t.Fatalf("alive=%v error=%v killed=%v", alive, err, killed)
			}
		})
	}
}

func TestServeChecksPaneAfterDisablingRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		probe     string
		isMissing bool
		wantError bool
		wantKill  bool
	}{
		{name: "retained dead pane", probe: "echo 1", wantKill: true},
		{name: "live pane", probe: "echo 0"},
		{name: "probe failed", probe: "false", wantError: true, wantKill: true},
		{name: "exited during probe", probe: "false", isMissing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := &mockCommandFactory{}
			killed := false
			client := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if slices.Equal(args, []string{"display-message", "-p", "-t", "serve", "#{pane_dead}"}) {
					return exec.CommandContext(ctx, "sh", "-c", tc.probe)
				}
				if len(args) > 0 && args[0] == "has-session" && tc.isMissing {
					return exec.CommandContext(ctx, "sh", "-c", "echo \"can't find session\" >&2; exit 1")
				}
				if len(args) > 0 && args[0] == "kill-session" {
					killed = true
				}
				return factory.factory(ctx, name, args...)
			}))
			err := client.NewSession(t.Context(), NewSessionOpts{Name: "serve", WorkDir: t.TempDir(), Command: []string{"true"}, ExitOnCompletion: true})
			if (err != nil) != tc.wantError || killed != tc.wantKill {
				t.Fatalf("error=%v killed=%v, want error=%v kill=%v", err, killed, tc.wantError, tc.wantKill)
			}
		})
	}
}
