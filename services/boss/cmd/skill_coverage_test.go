package main

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/recurser/bossalib/skilldata"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestSkillCoversAllCommands verifies that every non-deprecated, non-help
// command in the boss CLI is documented in the boss skill file.
func TestSkillCoversAllCommands(t *testing.T) {
	skill := readSkillContent(t)
	root := rootCmd()

	var walk func(cmd *cobra.Command, path string)
	walk = func(cmd *cobra.Command, path string) {
		if cmd.Deprecated != "" || cmd.Name() == "help" || cmd.Name() == "completion" {
			return
		}

		// Check command is documented. Use backtick prefix to avoid substring
		// false positives (e.g. "boss ls" matching inside "boss trash ls").
		// The command may be followed by args ("`boss show <session-id>`") or
		// be a bare group command ("`boss repo`"), so check for "`<path>`" or "`<path> ".
		// Group commands (no Run/RunE) that only exist to hold subcommands are
		// not required to have their own section — their subcommands suffice.
		isGroup := cmd.RunE == nil && cmd.Run == nil && cmd.HasSubCommands()
		if !isGroup {
			fenced := fmt.Sprintf("`%s`", path)
			prefixed := fmt.Sprintf("`%s ", path)
			if !strings.Contains(skill, fenced) && !strings.Contains(skill, prefixed) {
				t.Errorf("command %q not found in skill (looked for %s or %s)", path, fenced, prefixed)
			}
		}

		// Check all non-hidden flags are documented.
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			flagNeedle := fmt.Sprintf("--%s", f.Name)
			if !strings.Contains(skill, flagNeedle) {
				t.Errorf("flag --%s on command %q not found in skill", f.Name, path)
			}
		})

		for _, child := range cmd.Commands() {
			walk(child, path+" "+child.Name())
		}
	}

	// Check root persistent flags.
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		flagNeedle := fmt.Sprintf("--%s", f.Name)
		if !strings.Contains(skill, flagNeedle) {
			t.Errorf("persistent flag --%s not found in skill", f.Name)
		}
	})

	// Walk all subcommands (skip root itself — it's the TUI entry point).
	for _, child := range root.Commands() {
		walk(child, "boss "+child.Name())
	}
}

func readSkillContent(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(skilldata.SkillsFS, "skills/boss/SKILL.md")
	if err != nil {
		t.Fatalf("read skill file from embedded FS: %v", err)
	}
	return string(data)
}
