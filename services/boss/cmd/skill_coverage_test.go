package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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

// readSkillContent reads the canonical boss SKILL.md from the repository's
// .claude/skills/ source tree, walking up from this test file's location to
// locate the repo root. This avoids depending on `make copy-skills` having
// populated the embedded FS before `go test` compiles the test binary.
func readSkillContent(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		candidate := filepath.Join(dir, ".claude", "skills", "boss", "SKILL.md")
		if _, err := os.Stat(candidate); err == nil {
			data, err := os.ReadFile(candidate)
			if err != nil {
				t.Fatalf("read %s: %v", candidate, err)
			}
			return string(data)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate .claude/skills/boss/SKILL.md walking up from %s", filepath.Dir(thisFile))
		}
		dir = parent
	}
}
