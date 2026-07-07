package skillinstall

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestEmbeddedSkillManifestExcludesBossProof pins the exact set of skills the boss
// binary ships via the embedded skillinstall payload. BOS-271 consolidated the four
// published cores (boss-epic/implement/plan/review) onto this single-source home and
// dropped boss-proof from the publish set — boss-proof stays a repo-local dev skill
// under .claude/skills, never embedded. This test fails loudly if boss-proof (or any
// other unexpected skill) is re-added to, or an expected core drops out of, the embed.
func TestEmbeddedSkillManifestExcludesBossProof(t *testing.T) {
	entries, err := fs.ReadDir(SkillsFS, "skills")
	if err != nil {
		t.Fatalf("read embedded skills dir: %v", err)
	}

	var got []string
	for _, e := range entries {
		if e.IsDir() {
			got = append(got, e.Name())
		}
	}
	sort.Strings(got)

	want := []string{
		"boss",
		"boss-epic",
		"boss-finalize",
		"boss-implement",
		"boss-plan",
		"boss-repair",
		"boss-review",
		"boss-verify",
	}

	if len(got) != len(want) {
		t.Fatalf("embedded skill set = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("embedded skill set = %v; want %v", got, want)
		}
	}

	for _, name := range got {
		if name == "boss-proof" {
			t.Fatalf("boss-proof must not be embedded (dropped from the publish set in BOS-271)")
		}
	}
}

// knownCores enumerates the published/dev core skill names. It is the core-name set,
// NOT an extension-name enumeration, so it stays stable as extensions are added or
// renamed. boss-proof is included even though it is not embedded (BOS-271 dropped it
// from the publish set) because it is the parent core of the boss-proof-* extensions,
// so its prefix is needed to recognize them.
var knownCores = map[string]bool{
	"boss":           true,
	"boss-epic":      true,
	"boss-finalize":  true,
	"boss-implement": true,
	"boss-plan":      true,
	"boss-proof":     true,
	"boss-repair":    true,
	"boss-review":    true,
	"boss-verify":    true,
}

// isExtensionDirName reports whether a skill directory name is a boss-<core>-<suffix>
// extension (e.g. boss-review-golang, boss-plan-draft) rather than a core. A name is an
// extension iff it is not itself a known core AND it carries a known core name plus a
// trailing "-<suffix>" segment. The "not itself a core" guard keeps a two-segment core
// like boss-plan (which prefix-matches the bare "boss" core) from being misclassified.
func isExtensionDirName(name string) bool {
	if knownCores[name] {
		return false
	}
	for core := range knownCores {
		if strings.HasPrefix(name, core+"-") {
			return true
		}
	}
	return false
}

// TestIsExtensionDirName pins the classifier's true AND false branches directly, so the
// BOS-288 hard gate cannot silently regress. TestSkillPayloadsExcludeExtensions only ever
// exercises isExtensionDirName against the embedded/mirror dirs, which today hold cores
// only — the true branch is never taken there, so a helper that regressed to always-false
// would still pass that gate green while shipping an extension. This table exercises both.
func TestIsExtensionDirName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Cores are never extensions (knownCores guard), including two-segment cores that
		// prefix-collide with the bare "boss" core, and boss-proof (a core not embedded).
		{"boss", false},
		{"boss-plan", false},
		{"boss-review", false},
		{"boss-implement", false},
		{"boss-proof", false},
		// boss-<core>-<suffix> names are extensions.
		{"boss-plan-draft", true},
		{"boss-review-golang", true},
		{"boss-review-thermonuclear", true},
		{"boss-proof-web", true},
		{"boss-implement-superpowers", true},
		// Non-boss / unrelated names are not extensions.
		{"golang-pro", false},
		{"bossnew", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isExtensionDirName(tc.name); got != tc.want {
			t.Errorf("isExtensionDirName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSkillPayloadsExcludeExtensions guards the BOS-288 disjoint-set invariant: the
// repo-local boss-<skill>-* extensions are discovered and dispatched repo-local from a
// worktree's .claude/skills and must NEVER be shipped in the embedded payload or the
// claude plugin mirror. Publishing/installing an extension would defeat the model
// documented in docs/skills/extension-contract.md. This is the hard gate that keeps the
// publish allowlist honest as extensions are added.
func TestSkillPayloadsExcludeExtensions(t *testing.T) {
	// 1. Embedded payload (SkillsFS).
	embedded, err := fs.ReadDir(SkillsFS, "skills")
	if err != nil {
		t.Fatalf("read embedded skills dir: %v", err)
	}
	for _, e := range embedded {
		if e.IsDir() && isExtensionDirName(e.Name()) {
			t.Errorf("embedded payload ships extension %q; extensions must stay repo-local (BOS-288)", e.Name())
		}
	}

	// 2. Claude plugin mirror (git-tracked on disk, a different module).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate repo root")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	mirrorDir := filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills")
	mirror, err := os.ReadDir(mirrorDir)
	if err != nil {
		t.Fatalf("read plugin mirror %s: %v", mirrorDir, err)
	}
	for _, e := range mirror {
		if e.IsDir() && isExtensionDirName(e.Name()) {
			t.Errorf("plugin mirror ships extension %q; extensions must stay repo-local (BOS-288)", e.Name())
		}
	}
}

func TestEmbeddedSkillMetadataUsesPublishedBossNames(t *testing.T) {
	entries, err := fs.ReadDir(SkillsFS, "skills")
	if err != nil {
		t.Fatalf("read embedded skills dir: %v", err)
	}

	frontmatterName := regexp.MustCompile(`(?m)^name:\s*([a-z0-9-]+)\s*$`)
	legacyHeading := regexp.MustCompile(`(?m)^#\s+BS\s+`)
	legacyDisplayName := regexp.MustCompile(`(?m)^\s*display_name:\s*['"]?BS\s+`)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		skillPath := filepath.ToSlash(filepath.Join("skills", name, "SKILL.md"))
		content, err := fs.ReadFile(SkillsFS, skillPath)
		if err != nil {
			t.Fatalf("read %s: %v", skillPath, err)
		}

		matches := frontmatterName.FindStringSubmatch(string(content))
		if matches == nil {
			t.Fatalf("%s: missing frontmatter name", skillPath)
		}
		if matches[1] != name {
			t.Fatalf("%s: frontmatter name = %q; want directory name %q", skillPath, matches[1], name)
		}
		if legacyHeading.Match(content) {
			t.Fatalf("%s: H1 must use boss-* naming, not legacy BS branding", skillPath)
		}

		agentPath := filepath.ToSlash(filepath.Join("skills", name, "agents", "openai.yaml"))
		agentContent, err := fs.ReadFile(SkillsFS, agentPath)
		if err != nil {
			if strings.Contains(err.Error(), "file does not exist") {
				continue
			}
			t.Fatalf("read %s: %v", agentPath, err)
		}
		if legacyDisplayName.Match(agentContent) {
			t.Fatalf("%s: display_name must use boss-* naming, not legacy BS branding", agentPath)
		}
	}
}
