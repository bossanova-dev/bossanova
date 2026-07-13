// Package skillparity holds the tree-aware skill-payload parity test (BOS-344
// Task 5). The boss CLI embeds its skill tree from
// services/boss/internal/skillinstall/skills, and the claude plugin embeds an
// identical mirror from plugins/bossd-plugin-claude/skilldata/skills. Both are
// populated by `make copy-skills` and committed. If they drift — a file added to
// one tree, or the same file edited in only one — the boss CLI and the daemon's
// claude plugin ship different skills, which is a silent correctness bug.
//
// This test asserts the two trees are identical: same set of relative paths AND
// identical content hashes (tree-aware, not a flat byte concat). It reads both
// trees from disk as Bazel `data` deps (see BUILD.bazel) so it runs under
// `bazel test` (runfiles, rundir=".") and plain `go test`/make alike — unlike
// the runtime-embed manifest tests in skillinstall, which read out-of-module
// source and stay `manual`.
package skillparity

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

// repoRoot resolves the repository root from this test file's location. Under
// `go test` runtime.Caller returns an absolute path; under `bazel test` it
// returns the repo-relative source path and rundir="." sets cwd to the runfiles
// root, so the same relative walk resolves against the staged `data` trees.
// This file lives at services/boss/internal/skillparity/parity_test.go, so the
// repo root is four directories up.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate repo root")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "..", "..")
}

// manifest walks root and returns rel-path -> sha256(content), skipping
// directories and the empty .gitkeep placeholder (present in both trees to keep
// the embed target non-empty; it carries no skill payload).
func manifest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if filepath.Base(rel) == ".gitkeep" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSkillPayloadParity fails loudly if the boss embed tree and the claude
// plugin mirror tree differ in path set or content — the tree-aware guard the
// copy-skills sync relies on.
func TestSkillPayloadParity(t *testing.T) {
	root := repoRoot(t)
	bossRoot := filepath.Join(root, "services", "boss", "internal", "skillinstall", "skills")
	mirrorRoot := filepath.Join(root, "plugins", "bossd-plugin-claude", "skilldata", "skills")

	boss := manifest(t, bossRoot)
	mirror := manifest(t, mirrorRoot)

	if len(boss) == 0 {
		t.Fatalf("boss skill tree at %s is empty — data dep missing?", bossRoot)
	}

	// Path-set parity.
	for _, p := range sortedKeys(boss) {
		if _, ok := mirror[p]; !ok {
			t.Errorf("path %q present in boss embed but missing from claude plugin mirror", p)
		}
	}
	for _, p := range sortedKeys(mirror) {
		if _, ok := boss[p]; !ok {
			t.Errorf("path %q present in claude plugin mirror but missing from boss embed", p)
		}
	}

	// Content-hash parity for the shared paths.
	for _, p := range sortedKeys(boss) {
		mh, ok := mirror[p]
		if !ok {
			continue
		}
		if boss[p] != mh {
			t.Errorf("content mismatch for %q: boss=%s mirror=%s (run `make copy-skills`)", p, boss[p][:12], mh[:12])
		}
	}
}
