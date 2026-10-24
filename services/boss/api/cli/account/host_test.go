package account

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRequireLocalRegistration(t *testing.T) {
	g := testRegistrationGroup(t)
	t.Run("rejects remote", func(t *testing.T) {
		err := g.requireLocalRegistration(remoteTestCommand(t), addRegistration(false))
		if err == nil || !strings.Contains(err.Error(), "--remote") {
			t.Fatalf("error = %v, want a message naming --remote", err)
		}
	})

	// A pasted token has no local subprocess in it, but --remote is refused for
	// every shape anyway: there is no remote *machine* behind a cloud
	// orchestrator that the operator could have minted the token on, so allowing
	// it there is a different design question than the --host one (BOS-847).
	t.Run("rejects remote even for a pasted token", func(t *testing.T) {
		err := g.requireLocalRegistration(remoteTestCommand(t), addRegistration(true))
		if err == nil || !strings.Contains(err.Error(), "--remote") {
			t.Fatalf("error = %v, want a message naming --remote", err)
		}
	})

	t.Run("rejects host", func(t *testing.T) {
		err := g.requireLocalRegistration(hostTestCommand(t), addRegistration(false))
		if err == nil {
			t.Fatal("expected boss account add against --host to be refused")
		}
		if !strings.Contains(err.Error(), "--host") {
			t.Fatalf("error %q must name --host", err.Error())
		}
		if !strings.Contains(err.Error(), "this machine") {
			t.Fatalf("error %q must explain the agent CLI runs on this machine", err.Error())
		}
		// The refusal is only useful if it carries the way out with it.
		if !strings.Contains(err.Error(), "--token-stdin") {
			t.Fatalf("error %q must point at the shape that does work under --host", err.Error())
		}
		// ...and the way out has to keep the destination. A bare
		// `boss account add claude --token-stdin` is a command that runs, so an
		// operator copies it — straight onto their local daemon, which is the
		// wrong-machine outcome this guard exists to prevent (BOS-847).
		if !strings.Contains(err.Error(), "boss --host user@example.test account add claude --token-stdin") {
			t.Fatalf("error %q must suggest the escape hatch with --host still on it", err.Error())
		}
	})

	// The whole point of the guard is the local subprocess, and
	// `boss account add claude --token-stdin` has none: it reads a token that
	// already exists and stores it through the tunnelled client, so the
	// credential lands on the remote daemon the operator asked for (BOS-847).
	t.Run("allows host for a pasted token", func(t *testing.T) {
		if err := g.requireLocalRegistration(hostTestCommand(t), addRegistration(true)); err != nil {
			t.Fatalf("claude --token-stdin under --host: %v", err)
		}
	})

	t.Run("allows local", func(t *testing.T) {
		if err := g.requireLocalRegistration(&cobra.Command{Use: "boss"}, addRegistration(false)); err != nil {
			t.Fatalf("plain local command: %v", err)
		}
	})
}

// TestAccountAddHandlersApplyTheHostGuardBeforeDialling pins the guard at the
// two call sites rather than only on the helper, because which value each
// handler passes for pastedToken IS the policy: codex has no pasted-token shape,
// so it must pass a constant false and stay refused under --host even when the
// operator supplies --token-stdin (BOS-847).
//
// Both cases return before newClient, so nothing here opens an ssh connection.
func TestAccountAddHandlersApplyTheHostGuardBeforeDialling(t *testing.T) {
	g := testRegistrationGroup(t)
	t.Run("claude without a pasted token is refused", func(t *testing.T) {
		cmd := hostTestCommand(t)
		// Registered explicitly so the refusal comes from the flag being false
		// rather than from GetBool failing on a flag that was never declared.
		cmd.Flags().Bool("token-stdin", false, "")
		err := g.runAccountAddClaude(cmd)
		if err == nil || !strings.Contains(err.Error(), "--host") {
			t.Fatalf("error = %v, want the --host refusal", err)
		}
	})

	t.Run("codex is refused even with --token-stdin", func(t *testing.T) {
		cmd := hostTestCommand(t)
		cmd.Flags().Bool("token-stdin", true, "")
		err := g.runAccountAddCodex(cmd)
		if err == nil || !strings.Contains(err.Error(), "--host") {
			t.Fatalf("error = %v, want the --host refusal rather than the codex "+
				"--token-stdin message; codex must not opt into the pasted-token exemption", err)
		}
	})
}

func testRegistrationGroup(t *testing.T) *group {
	t.Helper()
	return &group{
		newClient: func(*cobra.Command) (Client, error) {
			t.Fatal("registration guard must refuse before resolving the client")
			return nil, nil
		},
		remoteURL: func(cmd *cobra.Command) string {
			value, _ := cmd.Flags().GetString("remote")
			return value
		},
		hostDestination: func(cmd *cobra.Command) string {
			value, _ := cmd.Flags().GetString("host")
			return value
		},
	}
}

func remoteTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "boss"}
	cmd.Flags().String("remote", "https://orchestrator.example.test", "")
	return cmd
}

func hostTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "boss"}
	cmd.Flags().String("host", "user@example.test", "")
	return cmd
}
