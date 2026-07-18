package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/cmd/skillgen"
)

// skillPath is the canonical embedded skill source (source of truth), relative
// to the services/boss module root (where `make gen-skill` runs `go run .`).
const skillPath = "internal/skillinstall/skills/boss/SKILL.md"

func newGenSkillCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:    "gen-skill",
		Short:  "Regenerate the /boss skill command reference from the CLI",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGenSkill(rootCmd(), skillPath, check, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "Exit non-zero if the committed skill is stale; do not write")
	return cmd
}

func runGenSkill(root *cobra.Command, path string, check bool, out io.Writer) error {
	body, err := skillgen.GenerateBody(root)
	if err != nil {
		return err
	}
	// #nosec G304 -- path is the skill-doc target of the hidden dev-only `gen-skill` CLI; it is a non-secret repo markdown the developer names, and reading the named file is the command's purpose. owner=@recurser review-by=2026-09-16 issue=BOS-414
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read skill: %w", err)
	}
	updated, err := skillgen.Inject(string(existing), body)
	if err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	if check {
		if updated != string(existing) {
			return fmt.Errorf("%s is stale — run `make gen-skill`", path)
		}
		fmt.Fprintln(out, "skill reference up to date")
		return nil
	}
	if updated == string(existing) {
		return nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write skill: %w", err)
	}
	fmt.Fprintln(out, "regenerated "+path)
	return nil
}
