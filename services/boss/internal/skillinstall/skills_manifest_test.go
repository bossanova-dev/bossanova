package skillinstall

import (
	"io/fs"
	"path/filepath"
	"regexp"
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
