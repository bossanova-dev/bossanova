package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/spf13/cobra"
)

// hostTestCommand mirrors remoteTestCommand for the --host transport.
func hostTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	return hostTestCommandWith(t, "user@example.test", "")
}

func hostTestCommandWith(t *testing.T, host, hostSocket string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "boss"}
	cmd.Flags().String("host", host, "")
	cmd.Flags().String("host-socket", hostSocket, "")
	cmd.Flags().String("remote", "", "")
	return cmd
}

func TestHostFlagAccessors(t *testing.T) {
	t.Run("reads both flags", func(t *testing.T) {
		cmd := hostTestCommandWith(t, "alias", "/run/bossd.sock")
		if got := hostDestination(cmd); got != "alias" {
			t.Fatalf("hostDestination = %q, want %q", got, "alias")
		}
	})

	t.Run("nil safe and absent-flag safe", func(t *testing.T) {
		if got := hostDestination(nil); got != "" {
			t.Fatalf("hostDestination(nil) = %q, want empty", got)
		}
	})
}

func TestRequireSingleTransport(t *testing.T) {
	t.Run("rejects host with remote naming both flags", func(t *testing.T) {
		cmd := hostTestCommandWith(t, "user@example.test", "")
		if err := cmd.Flags().Set("remote", "https://orchestrator.example.test"); err != nil {
			t.Fatalf("set remote: %v", err)
		}
		err := requireSingleTransport(cmd)
		if err == nil {
			t.Fatal("expected --host with --remote to be rejected")
		}
		for _, want := range []string{"--host", "--remote"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q must name %s", err.Error(), want)
			}
		}
	})

	t.Run("allows each flag alone", func(t *testing.T) {
		if err := requireSingleTransport(hostTestCommand(t)); err != nil {
			t.Fatalf("--host alone: %v", err)
		}
		if err := requireSingleTransport(remoteTestCommand(t)); err != nil {
			t.Fatalf("--remote alone: %v", err)
		}
		if err := requireSingleTransport(&cobra.Command{Use: "boss"}); err != nil {
			t.Fatalf("plain local command: %v", err)
		}
	})
}

// TestRootRejectsHostWithRemote pins that the exclusion is wired into the root's
// PersistentPreRunE, so it fires for every subcommand before any dial.
func TestRootRejectsHostWithRemote(t *testing.T) {
	root := rootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--host", "user@example.test", "--remote", "https://orchestrator.example.test", "ls"})

	err := root.Execute()
	if err == nil {
		// Elides the middle of a command line, not prose: ellipsis: literal-dots ok
		t.Fatal("expected boss --host ... --remote ... to fail")
	}
	for _, want := range []string{"--host", "--remote"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %s", err.Error(), want)
		}
	}
}

// TestRootHelpFlagStillWorks guards the -h shorthand: cobra auto-registers -h
// for --help and evaluates it before PersistentPreRunE, so neither the new
// flags nor the new exclusion check may break `boss -h`.
func TestRootHelpFlagStillWorks(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			root := rootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{arg})

			if err := root.Execute(); err != nil {
				t.Fatalf("boss %s: %v", arg, err)
			}
			rendered := out.String()
			if !strings.Contains(rendered, "Usage:") {
				t.Fatalf("boss %s printed no usage: %q", arg, rendered)
			}
			if !strings.Contains(rendered, "--host") {
				t.Fatalf("boss %s should document --host: %q", arg, rendered)
			}
		})
	}
}

// TestIsTailInvocation pins the argv pre-scan that decides whether the file
// logger is attached. Each value-taking root flag must be skipped, or
// `boss --host <dest> tail` would write to the file it is about to read.
func TestIsTailInvocation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare tail", []string{"tail"}, true},
		{"bare cost", []string{"cost"}, true},
		{"remote value skipped", []string{"--remote", "https://x", "tail"}, true},
		{"host value skipped", []string{"--host", "user@example.test", "tail"}, true},
		{"host value skipped for cost", []string{"--host", "user@example.test", "cost"}, true},
		{"host socket value skipped", []string{"--host-socket", "/run/bossd.sock", "tail"}, true},
		{"equals form", []string{"--host=user@example.test", "tail"}, true},
		{"host value is not the subcommand", []string{"--host", "tail"}, false},
		{"other subcommand", []string{"--host", "user@example.test", "ls"}, false},
		{"no args", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTailInvocation(tc.args); got != tc.want {
				t.Fatalf("isTailInvocation(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestNewClientHostBranchNeverStartsALocalDaemon(t *testing.T) {
	oldNewHostClient := newHostClient
	oldEnsureRunning := daemonEnsureRunning
	t.Cleanup(func() { newHostClient = oldNewHostClient; daemonEnsureRunning = oldEnsureRunning })
	t.Setenv("BOSS_SOCKET", "")
	daemonEnsureRunning = func(string) error { t.Fatal("local daemon started for --host"); return nil }
	want := daemonrpc.NewLocal("/unused")
	newHostClient = func(cmd *cobra.Command, host string) (*daemonrpc.LocalClient, error) {
		if host != "user@example.test" {
			t.Fatalf("host = %q", host)
		}
		return want, nil
	}
	got, err := newDaemonClient(hostTestCommand(t))
	if err != nil || got != want {
		t.Fatalf("newClient = %v, %v; want host client", got, err)
	}
}

// TestHostStartupErrorReachesTheDaemonWait: unlike --remote, a --host failure
// must route into waitForDaemon so the reconnect screen gets its chance.
func TestHostStartupErrorReachesTheDaemonWait(t *testing.T) {
	startupErr := errors.New("dial unix: connection refused")
	waitCalled := false

	_, gotErr := handleClientStartupError(hostTestCommand(t), startupErr, func() (startupClient, error) {
		waitCalled = true
		return nil, startupErr
	})
	if !waitCalled {
		t.Fatal("a --host startup error must reach the daemon wait, not bypass it")
	}
	if !errors.Is(gotErr, startupErr) {
		t.Fatalf("error = %v, want %v", gotErr, startupErr)
	}
}

// hostDaemonSubcommand builds a `boss <path...>` tree carrying --host on the
// root, so the subtree guard can be asserted on the real CommandPath without
// executing anything.
func hostDaemonSubcommand(t *testing.T, host string, path ...string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "boss"}
	root.Flags().String("host", host, "")
	root.Flags().String("host-socket", "", "")
	root.Flags().String("remote", "", "")
	cmd := root
	for _, name := range path {
		child := &cobra.Command{Use: name}
		cmd.AddCommand(child)
		cmd = child
	}
	return cmd
}

// TestRequireLocalDaemonTarget: the `boss daemon` subtree installs, starts,
// stops, and uninstalls *this* machine's bossd through local subprocesses, so
// under --host it would act on the wrong daemon entirely. It must be refused,
// and only that subtree.
func TestRequireLocalDaemonTarget(t *testing.T) {
	t.Run("rejects every daemon subcommand under host", func(t *testing.T) {
		for _, sub := range []string{"install", "doctor", "uninstall", "status", "start", "stop", "restart", "rotate-token"} {
			t.Run(sub, func(t *testing.T) {
				err := requireLocalDaemonTarget(hostDaemonSubcommand(t, "user@example.test", "daemon", sub))
				if err == nil {
					// Elides the middle of a command line, not prose: ellipsis: literal-dots ok
					t.Fatalf("boss --host ... daemon %s must be refused", sub)
				}
				for _, want := range []string{"--host", "user@example.test", "ssh user@example.test boss daemon"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q must contain %q", err.Error(), want)
					}
				}
			})
		}
	})

	t.Run("rejects the bare daemon command", func(t *testing.T) {
		if err := requireLocalDaemonTarget(hostDaemonSubcommand(t, "alias", "daemon")); err == nil {
			t.Fatal("boss --host alias daemon must be refused")
		}
	})

	t.Run("allows the daemon subtree without host", func(t *testing.T) {
		for _, sub := range []string{"status", "stop", "restart"} {
			if err := requireLocalDaemonTarget(hostDaemonSubcommand(t, "", "daemon", sub)); err != nil {
				t.Fatalf("local boss daemon %s: %v", sub, err)
			}
		}
	})

	t.Run("allows other commands under host", func(t *testing.T) {
		// Only the daemon subtree is local-only; --host exists so everything
		// else reaches the remote daemon, and a guard that caught them all
		// would defeat the flag.
		for _, path := range [][]string{{"ls"}, {"session", "new"}, {"repo", "ls"}} {
			cmd := hostDaemonSubcommand(t, "user@example.test", path...)
			if err := requireLocalDaemonTarget(cmd); err != nil {
				// Elides the middle of a command line, not prose: ellipsis: literal-dots ok
				t.Fatalf("boss --host ... %s: %v", cmd.CommandPath(), err)
			}
		}
		// A command whose name merely starts with "daemon" is not the subtree.
		if err := requireLocalDaemonTarget(hostDaemonSubcommand(t, "user@example.test", "daemonize")); err != nil {
			t.Fatalf("boss daemonize is not the daemon subtree: %v", err)
		}
	})

	t.Run("nil safe", func(t *testing.T) {
		if err := requireLocalDaemonTarget(nil); err != nil {
			t.Fatalf("nil command: %v", err)
		}
	})
}

// TestRootRejectsDaemonSubtreeAgainstHost pins that the guard is wired into the
// root's PersistentPreRunE, so it fires before RunE — that is, before any local
// launchd/systemd subprocess runs.
func TestRootRejectsDaemonSubtreeAgainstHost(t *testing.T) {
	for _, sub := range []string{"status", "stop", "uninstall"} {
		t.Run(sub, func(t *testing.T) {
			root := rootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"--host", "user@example.test", "daemon", sub})

			err := root.Execute()
			if err == nil {
				// Elides the middle of a command line, not prose: ellipsis: literal-dots ok
				t.Fatalf("boss --host ... daemon %s must fail", sub)
			}
			if !strings.Contains(err.Error(), "--host user@example.test") {
				t.Fatalf("error %q must name the destination", err.Error())
			}
		})
	}
}
