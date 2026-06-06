package skillinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBossRepairSkillWatchModePollingContract(t *testing.T) {
	skill := readEmbeddedBossRepairSkill(t)
	watchMode := markdownSection(t, skill, "## Watch Mode")

	assertNotContains(t, skill, "gh pr checks --watch")
	assertContains(t, watchMode, "gh pr checks --json bucket")
	assertContains(t, watchMode, "node scripts/review-feedback-probe.js")
	assertContains(t, watchMode, "if checks are pending")
	assertContains(t, watchMode, "seconds, then poll checks, review threads, and mergeability again.")
	assertContains(t, watchMode, "Do not wait on checks without probing reviews and mergeability first.")
	assertContains(t, watchMode, "all fixed or declined review threads are resolved")
	assertContains(t, watchMode, "repair_status=clean")
	assertContains(t, watchMode, "mergeable is not `CONFLICTING`")
	assertContains(t, watchMode, "captured immediately before that pass")
	assertContains(t, watchMode, "refresh this baseline after every pushed repair")
}

func TestBossRepairSkillReviewRepliesUseGitHubReplyEndpoint(t *testing.T) {
	skill := readEmbeddedBossRepairSkill(t)

	assertNotContains(t, skill, "in_reply_to_id")
	assertContains(t, skill, "gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -f body=\"Fixed:")
	assertContains(t, skill, "gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -f body=\"Not fixing:")
	assertContains(t, skill, "gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -f body=\"Could you clarify")
}

func TestBossRepairEmbeddedSkillCopiesStayIdentical(t *testing.T) {
	serviceSkill := readEmbeddedBossRepairSkill(t)

	repoRoot := findRepoRoot(t)
	pluginPath := filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills", "boss-repair", "SKILL.md")
	pluginSkillBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("read plugin boss-repair skill: %v", err)
	}

	if serviceSkill != string(pluginSkillBytes) {
		t.Fatalf("boss-repair skill copies differ between services/boss and bossd-plugin-claude")
	}
}

func readEmbeddedBossRepairSkill(t *testing.T) string {
	t.Helper()

	skillBytes, err := SkillsFS.ReadFile("skills/boss-repair/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded boss-repair skill: %v", err)
	}
	return string(skillBytes)
}

func markdownSection(t *testing.T, markdown, heading string) string {
	t.Helper()

	start := strings.Index(markdown, heading)
	if start == -1 {
		t.Fatalf("heading %q not found", heading)
	}

	rest := markdown[start+len(heading):]
	next := strings.Index(rest, "\n## ")
	if next == -1 {
		return rest
	}
	return rest[:next]
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected content to contain %q", needle)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if strings.Contains(haystack, needle) {
		t.Fatalf("expected content not to contain %q", needle)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.work not found above %s", dir)
		}
		dir = parent
	}
}
