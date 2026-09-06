package main

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestAccountReauthGuardsPointAtReauthNotAdd pins the remedy each refusal
// prints, not just the fact that it refuses. `boss account reauth <id>` reuses
// the guards the codex add path uses, and the add remedies are actively harmful
// here: `ssh <host> boss account add codex` registers a SECOND account and
// leaves the broken binding exactly as it was — the state reauthentication
// exists to repair — and `--token-stdin` is a shape codex rejects outright
// (runAccountAddCodex refuses it), so suggesting it sends the operator to a
// command that cannot work (BOS-1142).
//
// Both cases return before newClient, so nothing here opens an ssh connection.
func TestAccountReauthGuardsPointAtReauthNotAdd(t *testing.T) {
	// assertReauthRemedy is the shared contract: name the repair command with
	// the id the operator actually gave, and never hand back an add remedy.
	assertReauthRemedy := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("a remote/non-interactive reauth must stay refused")
		}
		if !strings.Contains(err.Error(), "boss account reauth acct-9") {
			t.Errorf("error %q must carry the reauth remedy, with the id", err.Error())
		}
		if strings.Contains(err.Error(), "account add") {
			t.Errorf("error %q sends the operator to add, which creates a second "+
				"account and leaves the failed binding untouched", err.Error())
		}
		if strings.Contains(err.Error(), "--token-stdin") {
			t.Errorf("error %q suggests a flag codex does not accept", err.Error())
		}
	}

	t.Run("host refusal names the reauth command on that host", func(t *testing.T) {
		err := runAccountReauth(hostTestCommand(t), "acct-9")
		assertReauthRemedy(t, err)
		if !strings.Contains(err.Error(), "--host") {
			t.Errorf("error %q must still say why it refused", err.Error())
		}
		// The remedy is only copy-pasteable if it keeps the destination.
		if !strings.Contains(err.Error(), "ssh user@example.test boss account reauth acct-9") {
			t.Errorf("error %q must suggest the repair on the daemon host", err.Error())
		}
	})

	t.Run("non-interactive refusal names the reauth command", func(t *testing.T) {
		// term.IsTerminal reads the real fd, so the pipe has to be real too;
		// otherwise this passes or fails on how `go test` happened to be invoked.
		devnull, openErr := os.Open(os.DevNull)
		if openErr != nil {
			t.Fatalf("open %s: %v", os.DevNull, openErr)
		}
		t.Cleanup(func() { _ = devnull.Close() })
		stdin := os.Stdin
		os.Stdin = devnull
		t.Cleanup(func() { os.Stdin = stdin })

		err := runAccountReauth(&cobra.Command{Use: "boss"}, "acct-9")
		assertReauthRemedy(t, err)
	})
}
