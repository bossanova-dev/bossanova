package diagnostics

import (
	"bytes"
	"testing"

	"github.com/recurser/boss/internal/termreset"
)

// TestFixTerminalWritesAbnormalExitReset pins the command's entire output to
// the abnormal-exit reset sequence — the wide set, not the narrow mouse-only
// one — with no banner and no trailing newline. `ssh host boss fix-terminal`
// must emit control bytes and nothing else, or it prints noise into the very
// terminal it is repairing.
func TestFixTerminalWritesAbnormalExitReset(t *testing.T) {
	cmd := fixTerminalCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("fix-terminal returned error: %v", err)
	}
	if got := out.String(); got != termreset.AbnormalExitReset {
		t.Errorf("fix-terminal wrote %q, want exactly termreset.AbnormalExitReset (%q)", got, termreset.AbnormalExitReset)
	}
	if errOut.Len() != 0 {
		t.Errorf("fix-terminal wrote %q to stderr, want nothing", errOut.String())
	}
}

// TestFixTerminalRejectsArgs keeps the command from silently ignoring a
// mistyped invocation.
func TestFixTerminalRejectsArgs(t *testing.T) {
	cmd := fixTerminalCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"unexpected"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("fix-terminal accepted a positional argument; want an error")
	}
}
