package views

import (
	"strings"
	"testing"
	"time"
)

// TestAccountRegisterReauthUnderHostNamesTheReauthRemedy is the TUI half of the
// remedy split. Under `boss --host <dest>` the codex flow refuses before
// Exec.Start, and that refusal is the only thing the operator carries away —
// esc pops straight back to the accounts list. Told `boss account add codex`,
// an operator repairing a bound account registers a SECOND one on the daemon
// host and leaves the broken binding exactly as it was, which is the state
// reauthentication exists to fix. So the reauth path names its own command,
// with the id, matching the CLI's refusal in services/boss/cmd (BOS-1142).
//
// Driven through beginReauth rather than by calling codexRemoteRefusal directly,
// because the bug was that the refusal site could not see reauthAccountID at
// all; a direct call would assert the helper and skip the wiring.
//
// Not parallel: hostDestination is a package global.
func TestAccountRegisterReauthUnderHostNamesTheReauthRemedy(t *testing.T) {
	withHostDestination(t, "deploy@build-box.invalid")

	client := &regAcctClient{}
	ex := &regExec{proc: newRegScriptedProc([]string{"should never be read"}, nil)}
	m := newRegisterModel(t, client, ex)
	m.homeDir = func() (string, error) { return t.TempDir(), nil }

	m, _ = m.beginReauth("acct-codex-9")

	var flowErr error
	select {
	case flowErr = <-m.donec:
	case <-time.After(3 * time.Second):
		t.Fatal("the reauth flow never returned under --host")
	}
	upd, _ := m.Update(flowDoneMsg{err: flowErr})
	m = upd.(AccountRegisterModel)

	if got := ex.launchedName(); got != "" {
		t.Fatalf("codex was spawned locally under --host (launched %q); the refusal "+
			"must still come before Exec.Start", got)
	}
	if m.state != registerStateError || m.err == nil {
		t.Fatalf("expected the refusal on the error screen; state=%d err=%v", m.state, m.err)
	}

	msg := m.err.Error()
	if !strings.Contains(msg, "boss account reauth acct-codex-9") {
		t.Errorf("refusal %q must carry the command that repairs THIS account", msg)
	}
	if strings.Contains(msg, "boss account add") {
		t.Errorf("refusal %q sends the operator to add, which registers a second "+
			"account and leaves the failed binding untouched", msg)
	}
	if !strings.Contains(msg, "deploy@build-box.invalid") {
		t.Errorf("refusal %q must still name the machine to run it on", msg)
	}
	if client.addCount() != 0 {
		t.Fatalf("a refused reauth must store nothing; adds=%d", client.addCount())
	}

	select {
	case <-m.flowDone:
	case <-time.After(3 * time.Second):
		t.Fatal("flow goroutine leaked after a policy refusal")
	}
}
