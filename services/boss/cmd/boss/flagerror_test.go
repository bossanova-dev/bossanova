package main

import (
	"testing"

	"github.com/spf13/pflag"
)

// TestRootInstallsFlagErrorHook proves the real root command carries the hook,
// so every subcommand inherits it rather than the tests proving only a
// synthetic tree.
func TestRootInstallsFlagErrorHook(t *testing.T) {
	root := rootCmd()
	notes, _, err := root.Find([]string{"notes", "add"})
	if err != nil {
		t.Fatalf("find notes add: %v", err)
	}
	if notes.FlagErrorFunc() == nil {
		t.Fatal("notes add resolved no FlagErrorFunc")
	}
	names := []string{}
	notes.Flags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
	found := false
	for _, n := range names {
		if n == "tag" {
			found = true
		}
	}
	if !found {
		t.Errorf("notes add should register --tag; got %v", names)
	}
}
