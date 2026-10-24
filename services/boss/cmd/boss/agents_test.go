package main

import (
	"slices"
	"testing"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/spf13/cobra"
)

// TestRunAgentsCommandIsRegistered proves the command reaches `boss --help`
// with a --json flag, rather than only being reachable through renderAgents.
func TestRunAgentsCommandIsRegistered(t *testing.T) {
	root := rootCmd()
	found, _, err := root.Find([]string{"agents"})
	if err != nil {
		t.Fatalf("root.Find(agents): %v", err)
	}
	if found == nil || found.Name() != "agents" {
		t.Fatalf("`boss agents` is not registered on the root command (found %v)", found)
	}
	if found.Hidden {
		t.Error("`boss agents` is hidden; it must appear in `boss --help`")
	}
	// Assert the concrete group, not merely a non-empty one: a typo'd GroupID
	// would satisfy `!= ""` while silently filing the command away from
	// `boss plugin list`, which is the whole point of the placement.
	plugin, _, err := root.Find([]string{"plugin"})
	if err != nil {
		t.Fatalf("root.Find(plugin): %v", err)
	}
	if found.GroupID != plugin.GroupID {
		t.Errorf("`boss agents` is in group %q, want %q (alongside `boss plugin list`)",
			found.GroupID, plugin.GroupID)
	}
	if !slices.ContainsFunc(root.Groups(), func(g *cobra.Group) bool { return g.ID == found.GroupID }) {
		t.Errorf("`boss agents` GroupID %q is not a registered group", found.GroupID)
	}
	if found.Flags().Lookup(cliutil.JSONFlagName) == nil {
		t.Error("`boss agents` does not register --json")
	}
}
