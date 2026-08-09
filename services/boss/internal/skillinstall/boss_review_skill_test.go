package skillinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBossReviewConfirmationSurfaceIncludesVerifiedFindings(t *testing.T) {
	skill := readEmbeddedBossReviewSkill(t)
	phase6 := sectionBetween(t, skill, "## Phase 6 — Fix must-fix", "## Phase 7 — Report")

	assertContains(t, phase6, "cited files of every `verified` must-fix item")
	assertContains(t, phase6, "the cited files still form the confirming surface; do not skip")

	methodologyBytes, err := SkillsFS.ReadFile("skills/boss-review/references/core-methodology.md")
	if err != nil {
		t.Fatalf("read boss-review methodology: %v", err)
	}
	methodology := string(methodologyBytes)
	assertContains(t, methodology, "cited files of every verified finding")
}

func TestBossReviewEmbeddedSkillCopiesStayIdentical(t *testing.T) {
	repoRoot := findRepoRoot(t)
	for _, rel := range []string{
		"SKILL.md",
		filepath.Join("references", "core-methodology.md"),
	} {
		t.Run(rel, func(t *testing.T) {
			embedded, err := SkillsFS.ReadFile("skills/boss-review/" + filepath.ToSlash(rel))
			if err != nil {
				t.Fatalf("read embedded boss-review %s: %v", rel, err)
			}
			mirror, err := os.ReadFile(filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills", "boss-review", rel))
			if err != nil {
				t.Fatalf("read plugin boss-review %s: %v", rel, err)
			}
			if string(embedded) != string(mirror) {
				t.Errorf("boss-review %s differs between services/boss and bossd-plugin-claude; run `make copy-skills`", rel)
			}
		})
	}
}

func readEmbeddedBossReviewSkill(t *testing.T) string {
	t.Helper()

	skillBytes, err := SkillsFS.ReadFile("skills/boss-review/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded boss-review skill: %v", err)
	}
	return string(skillBytes)
}
