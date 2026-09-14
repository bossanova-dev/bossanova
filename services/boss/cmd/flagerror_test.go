package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"tags", "tag", 1},
		{"states", "state", 1},
		{"allowed-model", "allowed-models", 1},
		{"tag", "tag", 0},
		{"TAGS", "tag", 1},
		{"", "tag", 3},
		{"tag", "", 3},
		{"wildly-unrelated", "tag", 15},
	}
	for _, tc := range cases {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// flagErrorTestCmd builds a small tree: a root with a persistent flag and one
// child with local flags, so inherited flags are covered too.
func flagErrorTestCmd() *cobra.Command {
	root := &cobra.Command{Use: "root"}
	root.PersistentFlags().String("remote", "", "")
	child := &cobra.Command{Use: "child", RunE: func(*cobra.Command, []string) error { return nil }}
	child.Flags().StringArray("tag", nil, "")
	child.Flags().String("hidden-flag", "", "")
	_ = child.Flags().MarkHidden("hidden-flag")
	child.Flags().String("legacy", "", "")
	_ = child.Flags().MarkDeprecated("legacy", "use --tag")
	root.AddCommand(child)
	installFlagErrorHook(root)
	// Force the merge cobra does before parsing, so InheritedFlags is populated.
	_ = child.ParseFlags(nil)
	return child
}

func TestNearestFlagName(t *testing.T) {
	child := flagErrorTestCmd()

	if got, ok := nearestFlagName(child, "tags"); !ok || got != "tag" {
		t.Errorf("nearestFlagName(tags) = %q, %v; want tag, true", got, ok)
	}
	// An inherited persistent flag is a candidate too.
	if got, ok := nearestFlagName(child, "remotes"); !ok || got != "remote" {
		t.Errorf("nearestFlagName(remotes) = %q, %v; want remote, true", got, ok)
	}
	// Nothing within the bound: no suggestion rather than a misleading one.
	if got, ok := nearestFlagName(child, "wildly-unrelated-flag-name"); ok {
		t.Errorf("nearestFlagName of a distant name suggested %q; want no suggestion", got)
	}
	// A hidden flag is never suggested — `--help` does not list it, so a caller
	// pointed at it would be looking for something undocumented.
	if got, ok := nearestFlagName(child, "hidden-flat"); ok && got == "hidden-flag" {
		t.Errorf("a hidden flag must not be suggested, got %q", got)
	}
	// Nor is a deprecated one.
	if got, ok := nearestFlagName(child, "legacz"); ok && got == "legacy" {
		t.Errorf("a deprecated flag must not be suggested, got %q", got)
	}
	if got, ok := nearestFlagName(nil, "tag"); ok {
		t.Errorf("a nil command must yield no suggestion, got %q", got)
	}
	if got, ok := nearestFlagName(child, ""); ok {
		t.Errorf("an empty name must yield no suggestion, got %q", got)
	}
}

func TestFlagErrorHookClassifiesAndSuggests(t *testing.T) {
	child := flagErrorTestCmd()

	err := child.ParseFlags([]string{"--tags", "x"})
	if err == nil {
		t.Fatal("expected pflag to reject --tags")
	}
	hooked := flagErrorHook(child, err)
	if errorCodeFor(hooked) != codeInvalidArgument {
		t.Errorf("code = %q, want %q", errorCodeFor(hooked), codeInvalidArgument)
	}
	if !strings.Contains(hooked.Error(), "unknown flag: --tags") {
		t.Errorf("the original rejection must be preserved, got %q", hooked.Error())
	}
	if !strings.Contains(hooked.Error(), "did you mean --tag?") {
		t.Errorf("expected a suggestion, got %q", hooked.Error())
	}
	// The pflag error stays reachable through the wrap chain.
	var notExist *pflag.NotExistError
	if !errors.As(hooked, &notExist) {
		t.Error("the typed pflag error must survive wrapping")
	}
}

func TestFlagErrorHookLeavesHelpAlone(t *testing.T) {
	child := flagErrorTestCmd()
	if got := flagErrorHook(child, pflag.ErrHelp); !errors.Is(got, pflag.ErrHelp) {
		t.Errorf("ErrHelp must pass through unchanged, got %v", got)
	}
	if errorCodeFor(flagErrorHook(child, pflag.ErrHelp)) == codeInvalidArgument {
		t.Error("help is not an invalid argument")
	}
	if got := flagErrorHook(child, nil); got != nil {
		t.Errorf("a nil error must stay nil, got %v", got)
	}
}

// TestFlagErrorHookClassifiesNonSuggestibleRejections proves classification is
// not conditional on a near-miss being found: a value error and a shorthand
// rejection carry no suggestion but still classify.
func TestFlagErrorHookClassifiesNonSuggestibleRejections(t *testing.T) {
	child := flagErrorTestCmd()
	for _, args := range [][]string{
		{"--wildly-unrelated-flag-name", "x"},
		{"-Z"},
	} {
		err := child.ParseFlags(args)
		if err == nil {
			t.Fatalf("expected pflag to reject %v", args)
		}
		hooked := flagErrorHook(child, err)
		if errorCodeFor(hooked) != codeInvalidArgument {
			t.Errorf("%v: code = %q, want %q", args, errorCodeFor(hooked), codeInvalidArgument)
		}
		if strings.Contains(hooked.Error(), "did you mean") {
			t.Errorf("%v: no suggestion should be offered, got %q", args, hooked.Error())
		}
	}
}

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
	names := registeredFlagNames(notes)
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
