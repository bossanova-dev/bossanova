package main

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"github.com/recurser/boss/internal/termreset"
	"github.com/spf13/cobra"
)

type fakeDaemonCommands struct{ calls []string }

func (g *fakeDaemonCommands) Command() *cobra.Command { return &cobra.Command{Use: "daemon"} }
func (g *fakeDaemonCommands) WarnIfDaemonBinaryStale(cmd *cobra.Command) {
	g.calls = append(g.calls, "stale")
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "stale warning")
}
func (g *fakeDaemonCommands) WarnIfBossBinaryBehindCheckout(cmd *cobra.Command) {
	g.calls = append(g.calls, "revision")
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "revision warning")
}
func (g *fakeDaemonCommands) WarnIfDaemonUnsupervised(cmd *cobra.Command) {
	g.calls = append(g.calls, "supervision")
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "supervision warning")
}
func TestRootCommandRunsDaemonWarningsBeforeDiagnosticBypass(t *testing.T) {
	g := &fakeDaemonCommands{}
	root := rootCmdWithGroups(newSkillsGroup(), g)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"fix-terminal"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.calls, []string{"stale", "revision", "supervision"}) {
		t.Fatalf("warning order = %v", g.calls)
	}
	if stdout.String() != termreset.AbnormalExitReset {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "stale warning\nrevision warning\nsupervision warning\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
