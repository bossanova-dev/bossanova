package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// findAddSubcommand returns the `add` subcommand of the account command group.
func findAddSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	account := accountCmd()
	for _, c := range account.Commands() {
		if c.Name() == "add" {
			return c
		}
	}
	t.Fatalf("account command has no `add` subcommand")
	return nil
}

func TestAccountAddCommandShape(t *testing.T) {
	add := findAddSubcommand(t)

	// New interactive-flow flags are present.
	for _, name := range []string{"token-stdin", "timeout"} {
		if add.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag on `account add`", name)
		}
	}
	// The BOS-160 non-interactive flags are preserved.
	for _, name := range []string{"provider", "label", "email", "priority", "token", "credential-file"} {
		if add.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag to be preserved on `account add`", name)
		}
	}

	// --timeout defaults to 10m.
	if f := add.Flags().Lookup("timeout"); f != nil && f.DefValue != "10m0s" {
		t.Errorf("expected --timeout default 10m0s, got %q", f.DefValue)
	}

	// Args accepts at most one positional provider.
	if add.Args == nil {
		t.Fatalf("expected Args validator on `account add`")
	}
	if err := add.Args(add, []string{"claude"}); err != nil {
		t.Errorf("expected one positional arg to be accepted, got %v", err)
	}
	if err := add.Args(add, []string{"claude", "extra"}); err == nil {
		t.Errorf("expected two positional args to be rejected")
	}
}
