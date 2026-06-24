package skillgen

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/bossalib/clidoc"
)

func newTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "boss"}
	ls := &cobra.Command{Use: "ls", Short: "List sessions", GroupID: "session", Run: func(*cobra.Command, []string) {}}
	ls.Flags().String("repo", "", "Filter by repo ID")
	root.AddCommand(ls)

	repo := &cobra.Command{Use: "repo", Short: "Manage repos", GroupID: "repo"} // group cmd, no Run
	repoAdd := &cobra.Command{Use: "add", Short: "Register a repo", Run: func(*cobra.Command, []string) {}}
	repo.AddCommand(repoAdd) // inherits repo group
	root.AddCommand(repo)
	return root
}

func TestExtractGroupsAndInheritance(t *testing.T) {
	groups, err := Extract(newTestRoot())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got := map[string][]string{}
	for _, g := range groups {
		for _, c := range g.Commands {
			got[g.ID] = append(got[g.ID], c.Path)
		}
	}
	if want := []string{"boss ls"}; !equal(got["session"], want) {
		t.Errorf("session commands = %v, want %v", got["session"], want)
	}
	if want := []string{"boss repo", "boss repo add"}; !equal(got["repo"], want) {
		t.Errorf("repo commands = %v, want %v", got["repo"], want)
	}
}

func TestExtractMergesRegistryProseAndFlags(t *testing.T) {
	groups, err := Extract(newTestRoot())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	var ls *clidoc.CommandDoc
	for _, g := range groups {
		for i := range g.Commands {
			if g.Commands[i].Path == "boss ls" {
				ls = &g.Commands[i]
			}
		}
	}
	if ls == nil {
		t.Fatal("boss ls not extracted")
	}
	if ls.Synopsis != "List sessions" {
		t.Errorf("synopsis = %q, want %q", ls.Synopsis, "List sessions")
	}
	if ls.UsageLine != "boss ls [flags]" {
		t.Errorf("usage line = %q, want %q", ls.UsageLine, "boss ls [flags]")
	}
	if len(ls.Flags) != 1 || ls.Flags[0].Name != "repo" {
		t.Errorf("flags = %+v, want single --repo", ls.Flags)
	}
}

func TestExtractUsageLinePreservesArguments(t *testing.T) {
	root := &cobra.Command{Use: "boss"}
	show := &cobra.Command{Use: "show <session-id>", Short: "Show a session", GroupID: "session", Run: func(*cobra.Command, []string) {}}
	root.AddCommand(show)

	groups, err := Extract(root)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	var got string
	for _, g := range groups {
		for _, cmd := range g.Commands {
			if cmd.Path == "boss show" {
				got = cmd.UsageLine
			}
		}
	}
	if got != "boss show <session-id>" {
		t.Fatalf("usage line = %q, want %q", got, "boss show <session-id>")
	}
}

func TestExtractOmitsNoiseDefaults(t *testing.T) {
	root := &cobra.Command{Use: "boss"}
	c := &cobra.Command{Use: "c", Short: "c", GroupID: "other", Run: func(*cobra.Command, []string) {}}
	c.Flags().Bool("flag-bool", false, "a bool")     // DefValue "false" -> omit
	c.Flags().StringSlice("flag-slice", nil, "list") // DefValue "[]" -> omit
	c.Flags().Int("flag-int", 5, "an int")           // DefValue "5" -> keep
	root.AddCommand(c)

	groups, err := Extract(root)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	defaults := map[string]string{}
	for _, g := range groups {
		for _, cmd := range g.Commands {
			for _, f := range cmd.Flags {
				defaults[f.Name] = f.Default
			}
		}
	}
	if defaults["flag-bool"] != "" {
		t.Errorf("bool default should be omitted, got %q", defaults["flag-bool"])
	}
	if defaults["flag-slice"] != "" {
		t.Errorf("empty-slice default should be omitted, got %q", defaults["flag-slice"])
	}
	if defaults["flag-int"] != "5" {
		t.Errorf("int default should be kept, got %q", defaults["flag-int"])
	}
}

func TestExtractHardErrorsOnUngroupedCommand(t *testing.T) {
	root := &cobra.Command{Use: "boss"}
	root.AddCommand(&cobra.Command{Use: "orphan", Short: "no group", Run: func(*cobra.Command, []string) {}})
	_, err := Extract(root)
	if err == nil {
		t.Fatal("expected hard error for ungrouped command, got nil")
	}
	if !strings.Contains(err.Error(), "boss orphan") {
		t.Errorf("error should name the offender; got: %v", err)
	}
}

func TestExtractHardErrorsOnUnknownGroup(t *testing.T) {
	root := &cobra.Command{Use: "boss"}
	root.AddCommand(&cobra.Command{Use: "weird", Short: "bad group", GroupID: "nonexistent", Run: func(*cobra.Command, []string) {}})
	_, err := Extract(root)
	if err == nil {
		t.Fatal("expected hard error for unknown group, got nil")
	}
	if !strings.Contains(err.Error(), "boss weird") || !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should name the offender and unknown group; got: %v", err)
	}
}

func TestExtractSkipsHiddenAndDeprecated(t *testing.T) {
	root := &cobra.Command{Use: "boss"}
	root.AddCommand(&cobra.Command{Use: "hid", Short: "hidden", GroupID: "other", Hidden: true, Run: func(*cobra.Command, []string) {}})
	root.AddCommand(&cobra.Command{Use: "dep", Short: "deprecated", GroupID: "other", Deprecated: "gone", Run: func(*cobra.Command, []string) {}})
	groups, err := Extract(root)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, g := range groups {
		for _, c := range g.Commands {
			if c.Path == "boss hid" || c.Path == "boss dep" {
				t.Errorf("hidden/deprecated command leaked: %s", c.Path)
			}
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
