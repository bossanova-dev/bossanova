package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/cmd/skillgen"
)

// The drift gate (TestSkillMatchesGenerated) compares committed bytes against
// skillgen.Generate — it never exercises the filesystem layer that gets from one
// to the other. These tests cover that layer directly, and in particular
// pruneReferences, the only destructive code the generator owns: it os.RemoveAll's
// every entry of references/ the bundle does not name, so a regression there
// deletes real files while the byte comparison happily passes on the survivors.

// newSkillFixture writes a minimal marker-bearing SKILL.md into a temp dir and
// returns its path plus the bundle rendered from the live CLI tree.
func newSkillFixture(t *testing.T) (string, skillgen.Bundle) {
	t.Helper()
	bundle, err := skillgen.Generate(rootCmd())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(bundle.References) == 0 {
		t.Fatalf("Generate produced no references — the fixture would be vacuous")
	}
	path := filepath.Join(t.TempDir(), "SKILL.md")
	body := "# fixture\n\n" + skillgen.BeginMarker + "\n" + skillgen.EndMarker + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path, bundle
}

func refDirOf(path string) string {
	return filepath.Join(filepath.Dir(path), referencesDir)
}

func TestRunGenSkillWritesReferencesThenIsIdempotent(t *testing.T) {
	path, bundle := newSkillFixture(t)

	var out bytes.Buffer
	if err := runGenSkill(rootCmd(), path, false, &out); err != nil {
		t.Fatalf("runGenSkill: %v", err)
	}
	if !strings.Contains(out.String(), "regenerated") {
		t.Errorf("first run printed %q; want a regenerated notice", out.String())
	}

	skill, err := os.ReadFile(path) // #nosec G304 -- test-owned temp path; owner=@recurser review-by=2027-02-02 issue=BOS-637
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if !strings.Contains(string(skill), "| `references/session.md`") {
		t.Errorf("SKILL.md did not receive the routing index:\n%s", skill)
	}
	for key, want := range bundle.References {
		got, err := os.ReadFile(filepath.Join(filepath.Dir(path), key)) // #nosec G304 -- test-owned temp path; owner=@recurser review-by=2027-02-02 issue=BOS-637
		if err != nil {
			t.Errorf("read %s: %v", key, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s: written bytes differ from the bundle", key)
		}
	}

	// A second run must be a silent no-op: `make gen-skill` runs on every build,
	// so a generator that rewrote identical bytes would dirty the worktree.
	out.Reset()
	if err := runGenSkill(rootCmd(), path, false, &out); err != nil {
		t.Fatalf("second runGenSkill: %v", err)
	}
	if out.String() != "" {
		t.Errorf("second run printed %q; want no output (nothing changed)", out.String())
	}
}

func TestRunGenSkillPrunesOnlyUnnamedEntries(t *testing.T) {
	path, bundle := newSkillFixture(t)
	if err := runGenSkill(rootCmd(), path, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("runGenSkill: %v", err)
	}
	refDir := refDirOf(path)

	orphan := filepath.Join(refDir, "retired-group.md")
	if err := os.WriteFile(orphan, []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	orphanDir := filepath.Join(refDir, "nested")
	if err := os.MkdirAll(orphanDir, 0o750); err != nil {
		t.Fatalf("mkdir orphan dir: %v", err)
	}

	var out bytes.Buffer
	if err := runGenSkill(rootCmd(), path, false, &out); err != nil {
		t.Fatalf("prune run: %v", err)
	}
	if out.String() == "" {
		t.Errorf("prune run printed nothing; a prune is a change and must be reported")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan reference survived the prune (stat err = %v)", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan directory survived the prune (stat err = %v)", err)
	}
	// Everything the bundle names must still be there, byte-for-byte: a prune
	// that took the whole directory with it would otherwise look identical.
	for key, want := range bundle.References {
		got, err := os.ReadFile(filepath.Join(filepath.Dir(path), key)) // #nosec G304 -- test-owned temp path; owner=@recurser review-by=2027-02-02 issue=BOS-637
		if err != nil {
			t.Errorf("read %s after prune: %v", key, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s: bytes changed during the prune run", key)
		}
	}
}

func TestRunGenSkillCheckNamesTheDriftedReferenceAndDoesNotWrite(t *testing.T) {
	path, bundle := newSkillFixture(t)
	if err := runGenSkill(rootCmd(), path, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("runGenSkill: %v", err)
	}
	refDir := refDirOf(path)

	if err := runGenSkill(rootCmd(), path, true, &bytes.Buffer{}); err != nil {
		t.Fatalf("--check on a clean tree: %v", err)
	}

	// A hand-edited reference must be named by --check even though SKILL.md's
	// index is perfectly current.
	edited := filepath.Join(refDir, "session.md")
	before, err := os.ReadFile(edited) // #nosec G304 -- test-owned temp path; owner=@recurser review-by=2027-02-02 issue=BOS-637
	if err != nil {
		t.Fatalf("read session.md: %v", err)
	}
	if err := os.WriteFile(edited, append(before, []byte("\nhand edit\n")...), 0o600); err != nil {
		t.Fatalf("hand-edit session.md: %v", err)
	}
	skillBefore, err := os.ReadFile(path) // #nosec G304 -- test-owned temp path; owner=@recurser review-by=2027-02-02 issue=BOS-637
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}

	err = runGenSkill(rootCmd(), path, true, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("--check returned nil on a hand-edited reference")
	}
	if !strings.Contains(err.Error(), "session.md") {
		t.Errorf("--check error = %q; want it to name session.md", err)
	}
	// Check mode must never repair what it reports.
	after, err := os.ReadFile(edited) // #nosec G304 -- test-owned temp path; owner=@recurser review-by=2027-02-02 issue=BOS-637
	if err != nil {
		t.Fatalf("re-read session.md: %v", err)
	}
	if string(after) == string(before) {
		t.Errorf("--check rewrote the hand-edited reference")
	}
	skillAfter, err := os.ReadFile(path) // #nosec G304 -- test-owned temp path; owner=@recurser review-by=2027-02-02 issue=BOS-637
	if err != nil {
		t.Fatalf("re-read skill: %v", err)
	}
	if string(skillAfter) != string(skillBefore) {
		t.Errorf("--check rewrote SKILL.md")
	}

	// And a reference the bundle names but that is missing entirely.
	if err := os.WriteFile(edited, before, 0o600); err != nil {
		t.Fatalf("restore session.md: %v", err)
	}
	if err := os.Remove(filepath.Join(refDir, "chat.md")); err != nil {
		t.Fatalf("remove chat.md: %v", err)
	}
	err = runGenSkill(rootCmd(), path, true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "chat.md") {
		t.Errorf("--check on a missing reference = %v; want an error naming chat.md", err)
	}

	// A missing references/ directory is reported, not a panic or a false pass.
	if err := os.RemoveAll(refDir); err != nil {
		t.Fatalf("remove refDir: %v", err)
	}
	stale, err := staleReference(refDir, bundle.References)
	if err != nil {
		t.Fatalf("staleReference on a missing dir: %v", err)
	}
	if stale != refDir {
		t.Errorf("staleReference(missing dir) = %q; want %q", stale, refDir)
	}
	if err := runGenSkill(rootCmd(), path, true, &bytes.Buffer{}); err == nil {
		t.Errorf("--check returned nil with references/ deleted")
	}
}
