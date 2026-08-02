package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/cmd/skillgen"
)

// skillPath is the canonical embedded skill source (source of truth), relative
// to the services/boss module root (where `make gen-skill` runs `go run .`).
const skillPath = "internal/skillinstall/skills/boss/SKILL.md"

// referencesDir is the generator-owned subdirectory of the skill holding one
// per-command-group reference file. Everything in it is generated, so any entry
// the bundle does not name is stale by definition.
const referencesDir = "references"

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
	bundle, err := skillgen.Generate(root)
	if err != nil {
		return err
	}
	// #nosec G304 -- hidden dev-only gen-skill command reading a repo SKILL.md; path kept as a test seam, non-secret.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read skill: %w", err)
	}
	updated, err := skillgen.Inject(string(existing), bundle.Index)
	if err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	refDir := filepath.Join(filepath.Dir(path), referencesDir)

	if check {
		stale, err := staleReference(refDir, bundle.References)
		if err != nil {
			return err
		}
		if updated != string(existing) {
			return fmt.Errorf("%s is stale — run `make gen-skill`", path)
		}
		// Name the drifted reference rather than SKILL.md: the index can be
		// perfectly current while a per-group file is edited, missing or stale.
		if stale != "" {
			return fmt.Errorf("%s is stale — run `make gen-skill`", stale)
		}
		fmt.Fprintln(out, "skill reference up to date")
		return nil
	}

	// Order matters: the references must exist (and stale ones be gone) before
	// SKILL.md starts pointing at them, or a failure mid-run ships an index
	// whose rows name files that are not there.
	wrote, err := writeReferences(refDir, bundle.References)
	if err != nil {
		return err
	}
	pruned, err := pruneReferences(refDir, bundle.References)
	if err != nil {
		return err
	}
	if updated != string(existing) {
		if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
			return fmt.Errorf("write skill: %w", err)
		}
	} else if !wrote && !pruned {
		return nil
	}
	fmt.Fprintln(out, "regenerated "+path)
	return nil
}

// writeReferences writes every bundle reference into refDir, reporting whether
// any file's bytes actually changed.
func writeReferences(refDir string, refs map[string]string) (bool, error) {
	if err := os.MkdirAll(refDir, 0o750); err != nil {
		return false, fmt.Errorf("create %s: %w", refDir, err)
	}
	changed := false
	for _, key := range sortedRefKeys(refs) {
		dest := filepath.Join(refDir, filepath.Base(key))
		// #nosec G304 -- generator-owned reference file under the repo skill payload; non-secret.
		// owner=@recurser review-by=2027-01-18 issue=BOS-637
		if prev, err := os.ReadFile(dest); err == nil && string(prev) == refs[key] {
			continue
		}
		if err := os.WriteFile(dest, []byte(refs[key]), 0o600); err != nil {
			return false, fmt.Errorf("write %s: %w", dest, err)
		}
		changed = true
	}
	return changed, nil
}

// pruneReferences removes every entry of refDir the bundle does not name —
// file or directory, .md or not. The directory is generator-owned, so anything
// unexpected is stale; without this a deleted command group leaves an orphan
// reference no gate would notice.
func pruneReferences(refDir string, refs map[string]string) (bool, error) {
	entries, err := os.ReadDir(refDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", refDir, err)
	}
	want := refBaseNames(refs)
	pruned := false
	for _, e := range entries {
		if want[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(refDir, e.Name())); err != nil {
			return false, fmt.Errorf("prune %s: %w", filepath.Join(refDir, e.Name()), err)
		}
		pruned = true
	}
	return pruned, nil
}

// staleReference returns the path of the first reference file whose presence or
// bytes differ from the bundle — an extra entry, a hand-edited one, or one the
// bundle names that is missing — and "" when refDir matches the bundle exactly.
// os.ReadDir sorts its entries, so the reported path is deterministic.
func staleReference(refDir string, refs map[string]string) (string, error) {
	entries, err := os.ReadDir(refDir)
	if err != nil {
		if os.IsNotExist(err) {
			if len(refs) > 0 {
				return refDir, nil
			}
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", refDir, err)
	}
	byName := map[string]string{}
	for key, content := range refs {
		byName[filepath.Base(key)] = content
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.Name()] = true
		path := filepath.Join(refDir, e.Name())
		want, ok := byName[e.Name()]
		if !ok || e.IsDir() {
			return path, nil
		}
		// #nosec G304 -- generator-owned reference file under the repo skill payload; non-secret.
		// owner=@recurser review-by=2027-01-18 issue=BOS-637
		got, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		if string(got) != want {
			return path, nil
		}
	}
	for _, key := range sortedRefKeys(refs) {
		if name := filepath.Base(key); !present[name] {
			return filepath.Join(refDir, name), nil
		}
	}
	return "", nil
}

func refBaseNames(refs map[string]string) map[string]bool {
	out := make(map[string]bool, len(refs))
	for key := range refs {
		out[filepath.Base(key)] = true
	}
	return out
}

func sortedRefKeys(refs map[string]string) []string {
	out := make([]string, 0, len(refs))
	for key := range refs {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
