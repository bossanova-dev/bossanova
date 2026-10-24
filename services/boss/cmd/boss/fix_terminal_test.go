package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/termreset"
)

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
	g := newSkillsGroup()
	setupStartupGroup(t, g)
	setStartupAgents(g, map[string]bool{"claude": true, "codex": true})
	g.StartupReadAnswer = func() string {
		t.Error("fix-terminal must not trigger the startup skill prompt")
		return ""
	}
	// An interactive tty with nothing installed yet: the pre-run installer would
	// prompt (fatal above) if it were not bypassed.
	g.StartupIsTerminal = func() bool { return true }

	root := rootCmdWithSkills(g)
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
