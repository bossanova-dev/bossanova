package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/spf13/cobra"

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

// TestFixTerminalHasDiagnosticsGroup guards the skill extractor contract: it
// hard-errors on a non-hidden command with no GroupID
// (lib/bossalib/clidoc/clidoc.go).
func TestFixTerminalHasDiagnosticsGroup(t *testing.T) {
	root := rootCmd()
	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "fix-terminal" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("fix-terminal is not registered on the root command")
	}
	if found.GroupID != "diagnostics" {
		t.Errorf("fix-terminal GroupID = %q, want %q", found.GroupID, "diagnostics")
	}
	if found.Hidden {
		t.Error("fix-terminal is hidden; it is a user-facing escape hatch")
	}
	if found.Short == "" {
		t.Error("fix-terminal has no Short description")
	}
}

// TestFixTerminalBypassesStartupSkillInstaller drives the command through the
// real root so the PersistentPreRunE actually runs. Without the bypass the
// startup skill installer would print a [Y/n] prompt into the terminal being
// repaired and block in fmt.Scanln — hanging the one command a user reaches for
// while their terminal is spewing mouse reports. See BOS-650.
func TestFixTerminalBypassesStartupSkillInstaller(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
	skillInstallReadAnswer = func() string {
		t.Error("fix-terminal must not trigger the startup skill prompt")
		return ""
	}
	// An interactive tty with nothing installed yet: the pre-run installer would
	// prompt (fatal above) if it were not bypassed.
	skillInstallIsTerminal = func() bool { return true }

	root := rootCmd()
	var out bytes.Buffer
	root.SetArgs([]string{"fix-terminal"})
	root.SetOut(&out)
	root.SetErr(io.Discard)

	if err := root.Execute(); err != nil {
		t.Fatalf("boss fix-terminal: %v", err)
	}
	if got := out.String(); got != termreset.AbnormalExitReset {
		t.Errorf("boss fix-terminal wrote %q, want exactly termreset.AbnormalExitReset (%q)", got, termreset.AbnormalExitReset)
	}
}
