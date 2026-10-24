package account

import (
	"errors"
	"fmt"
	"os"

	"github.com/recurser/boss/internal/accountflow"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// registrationOp identifies WHICH registration verb a guard is refusing, so the
// remedy it prints repairs the operator's actual situation (BOS-1142).
//
// The two verbs diverge sharply once a guard fires. `boss account add` mints a
// NEW account; `boss account reauth <id>` replaces the credential on an EXISTING
// one. An operator who follows an add remedy out of a reauth refusal therefore
// registers a second account and leaves the failed bound id exactly as it was —
// precisely the state reauthentication exists to repair. The add remedy's
// --token-stdin escape hatch is wrong there for a second, independent reason:
// reauth runs the codex device flow (accountflow.RunCodexReauth), and codex
// rejects --token-stdin outright — see runAccountAddCodex.
//
// It carries the values rather than a verb string because both guards need the
// same two facts and a bare bool pair at three call sites is what let the reauth
// path inherit the add wording in the first place.
type registrationOp struct {
	// reauth marks the verb. Kept separate from accountID so a degenerate empty
	// id (`boss account reauth ""`) still gets reauth wording rather than
	// silently falling back to the add remedy.
	reauth bool
	// accountID is the account a reauth would repair. Empty for add.
	accountID string
	// pastedToken marks the one add shape with no local subprocess in it:
	// `boss account add claude --token-stdin` reads a token that already exists.
	// Meaningless for reauth, which has no pasted-token shape.
	pastedToken bool
}

// addRegistration describes a `boss account add`, pastedToken marking the
// claude --token-stdin shape.
func addRegistration(pastedToken bool) registrationOp {
	return registrationOp{pastedToken: pastedToken}
}

// reauthRegistration describes a `boss account reauth <id>`.
func reauthRegistration(accountID string) registrationOp {
	return registrationOp{reauth: true, accountID: accountID}
}

// reauthCommand is the remedy a reauth refusal has to carry away with it. The id
// is part of the command, not decoration: `boss account reauth` without one is
// not a command the operator can run.
func (o registrationOp) reauthCommand() string {
	id := o.accountID
	if id == "" {
		id = "<account-id>"
	}
	return "boss account reauth " + id
}

// guardInteractiveStdin fails fast when an interactive registration flow would
// block on a terminal that is not attached. --token-stdin (claude add) is the one
// non-interactive escape hatch and skips the guard. Returning cleanly here —
// before any client dial or subprocess — is what a headless agent or cron hits.
func guardInteractiveStdin(op registrationOp) error {
	if op.pastedToken {
		return nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	if op.reauth {
		// No --token-stdin suggestion: reauth has no pasted-token shape at all.
		return fmt.Errorf("boss account reauth is interactive; run `%s` in a terminal on the daemon host", op.reauthCommand())
	}
	return errors.New("boss account add is interactive; run it in a terminal (or pass --token-stdin for claude)")
}

// requireLocalRegistration refuses account registration that would have to run a
// LOCAL subprocess — the claude setup-token walkthrough, the codex device flow —
// against a daemon on another machine. It is the *subprocess* that is local, not
// the credential: `claude setup-token` and `codex login` resolve their binary
// against this process's PATH and write into scratch dirs here, so under --host
// they 127 on a machine that need not have the agent CLIs at all.
//
// pastedToken marks the one shape with no subprocess in it: `boss account add
// claude --token-stdin` reads a token that already exists and stores it through
// the client, which under --host is the tunnelled one — so the credential lands
// on the remote daemon, which is exactly where the operator asked for it. A
// Claude setup token is account-scoped rather than machine-scoped, so there is
// nothing incoherent about minting it elsewhere and registering it there. That
// path is permitted under --host, and the TUI performs the same operation when
// claude is chosen in a --host session (BOS-847).
//
// --remote stays refused for every shape: there is no remote *machine* behind a
// cloud orchestrator to have run the CLI on, so allowing a paste there is a
// separate design question rather than the same one.
//
// Reauth (`boss account reauth <id>`) is refused for the same reason and gets
// its own remedy: see registrationOp for why the add wording is not merely
// imprecise there but actively harmful.
//
// It runs before any client dial or subprocess.
func (g *group) requireLocalRegistration(cmd *cobra.Command, op registrationOp) error {
	if g.remoteURL(cmd) != "" {
		if op.reauth {
			return fmt.Errorf("boss account reauth re-runs the provider login on the local daemon and cannot target a remote (--remote) daemon; run `%s` on the daemon host instead", op.reauthCommand())
		}
		return errors.New("boss account add registers credentials on the local daemon and cannot target a remote (--remote) daemon")
	}
	host := g.hostDestination(cmd)
	if host != "" && op.reauth {
		// Deliberately NOT the add remedy: `ssh <host> boss account add ...`
		// creates a second account and leaves the failed bound id untouched, and
		// there is no --token-stdin shape to fall back to. Name the id, because
		// the operator reached this from a specific broken binding.
		return fmt.Errorf("boss account reauth runs the agent CLI on this machine and cannot target a remote (--host %s) daemon; run `ssh %s %s` on that host instead", host, host, op.reauthCommand())
	}
	if host != "" && !op.pastedToken {
		// The claude escape hatch has to carry --host back with it. Suggesting the
		// bare `boss account add claude --token-stdin` reads as a working remedy
		// but drops the destination, so an operator who copies it registers the
		// pasted credential on their *local* daemon — the wrong-machine outcome
		// this guard exists to prevent (BOS-847).
		// The elision is inside a shell command in a code span, not prose: ellipsis: literal-dots ok
		return fmt.Errorf("boss account add runs the agent CLI on this machine and cannot target a remote (--host %s) daemon; run `ssh %s boss account add ...` on that host instead, or register claude with `boss --host %s account add claude --token-stdin`", host, host, host)
	}
	return nil
}

// runAccountAddClaude drives the interactive `boss account add claude`
// registration: the claude setup-token walkthrough, or a pasted/piped token
// with --token-stdin. The TTY guard runs before any RPC or subprocess.
func (g *group) runAccountAddClaude(cmd *cobra.Command) error {
	// Read --token-stdin first: it is what decides whether this invocation has a
	// local subprocess in it, and therefore whether the --host guard applies.
	tokenStdin, _ := cmd.Flags().GetBool("token-stdin")
	op := addRegistration(tokenStdin)
	if err := g.requireLocalRegistration(cmd, op); err != nil {
		return err
	}
	if err := guardInteractiveStdin(op); err != nil {
		return err
	}
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	label, _ := cmd.Flags().GetString("label")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	priority, _ := cmd.Flags().GetInt32("priority")
	return accountflow.RunClaudeAdd(cmd.Context(), accountflow.ClaudeOptions{
		Exec:      accountflow.NewOSExec(),
		Prompter:  accountflow.NewIOPrompter(cmd.InOrStdin(), cmd.OutOrStdout()),
		Client:    c,
		Timeout:   timeout,
		PasteMode: tokenStdin,
		// --token-stdin is both "paste the token" and "stdin is spent", so the CLI
		// sets both fields and behaves exactly as it did before they were split.
		StdinUnavailable: tokenStdin,
		Label:            label,
		Priority:         priority,
	})
}

// runAccountAddCodex drives the interactive `boss account add codex` device
// flow. Codex has no token-stdin path (it needs an interactive browser
// round-trip), so --token-stdin is rejected up front.
func (g *group) runAccountAddCodex(cmd *cobra.Command) error {
	// Codex has no pasted-token shape at all, so the guard always applies.
	op := addRegistration(false)
	if err := g.requireLocalRegistration(cmd, op); err != nil {
		return err
	}
	if tokenStdin, _ := cmd.Flags().GetBool("token-stdin"); tokenStdin {
		return errors.New("--token-stdin is not supported for codex; codex registration uses an interactive device flow")
	}
	if err := guardInteractiveStdin(op); err != nil {
		return err
	}
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	label, _ := cmd.Flags().GetString("label")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	priority, _ := cmd.Flags().GetInt32("priority")
	return accountflow.RunCodexAdd(cmd.Context(), accountflow.CodexOptions{
		Exec:     accountflow.NewOSExec(),
		Prompter: accountflow.NewIOPrompter(cmd.InOrStdin(), cmd.OutOrStdout()),
		Client:   c,
		Timeout:  timeout,
		Label:    label,
		Priority: priority,
	})
}

// runAccountReauth drives `boss account reauth <id>`: re-run the provider's
// interactive device login and hand the fresh credential to the EXISTING
// account rather than registering a second one (BOS-1142).
//
// Thin by design — it resolves the client and the flags, and the flow itself
// is a pure function over the narrow AgentAccountClient seam so it stays testable
// without a daemon.
func (g *group) runAccountReauth(cmd *cobra.Command, id string) error {
	// The device login needs an interactive TTY and writes a credential to the
	// local machine, so the same local-registration and stdin guards the codex
	// add path uses apply here too — but declared as a REAUTH, so each refusal
	// prints the remedy that repairs this account instead of the add remedy,
	// which would register a second one and leave the failed binding as it is.
	op := reauthRegistration(id)
	if err := g.requireLocalRegistration(cmd, op); err != nil {
		return err
	}
	if err := guardInteractiveStdin(op); err != nil {
		return err
	}
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	timeout, _ := cmd.Flags().GetDuration("timeout")
	return accountflow.RunCodexReauth(cmd.Context(), accountflow.CodexOptions{
		Exec:     accountflow.NewOSExec(),
		Prompter: accountflow.NewIOPrompter(cmd.InOrStdin(), cmd.OutOrStdout()),
		Client:   c,
		Timeout:  timeout,
	}, id)
}
