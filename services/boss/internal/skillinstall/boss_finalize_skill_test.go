package skillinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBossFinalizeSkillAssertsZeroMergeCommitsBeforePush pins the mechanical gate that turns
// the linear-history invariant from prose into a checked precondition: the final push (and
// therefore the un-draft that follows it) is guarded by a zero merge-commit assertion, so a
// branch poisoned by an earlier base merge can never be finalized on a rebase-merge repo.
func TestBossFinalizeSkillAssertsZeroMergeCommitsBeforePush(t *testing.T) {
	skill := readEmbeddedBossFinalizeSkill(t)

	preflight := "git rev-list --merges --count \"origin/$BASE_BRANCH\"..HEAD"
	assertContains(t, skill, preflight)

	// The preflight lives in the push step, ahead of the force-push itself.
	push := sectionBetween(t, skill, "### Step 6: Push to Remote", "### Step 6b")
	assertContains(t, push, preflight)
	if strings.Index(push, preflight) > strings.Index(push, "git push --force-with-lease") {
		t.Fatalf("merge-commit preflight must precede the push in Step 6")
	}

	// Blocking requirements 2 and 7 carry the invariant, and the conflict-repair path
	// (Step 6d) rebases + asserts rather than merging the base in.
	requirements := sectionBetween(t, skill, "## ⛔ BLOCKING REQUIREMENTS", "## Workflow Steps")
	assertContains(t, requirements, "rebase")
	assertContains(t, requirements, "never merge the base")
	assertContains(t, requirements, "merge commit")

	conflicts := sectionBetween(t, skill, "### Step 6d: Check for Merge Conflicts", "### Step 7")
	assertContains(t, conflicts, preflight)

	// Checklist + failure table keep the invariant visible where agents look last.
	checklist := sectionBetween(t, skill, "## Checklist", "## Common Failures")
	assertContains(t, checklist, "--merges --count")
	failures := markdownSection(t, skill, "## Common Failures")
	assertContains(t, failures, "--merges --count")
}

// TestBossFinalizeSkillCleanlinessChecksCannotBeFabricated pins BOS-1330. A command-rewriting shell
// hook can replace `git status` output with a summary that drops the "up to date with origin" line
// and prints nothing (or `ok`) for a dirty tree, so neither Step 6's push-destination check nor Step 7's
// clean-state assertion may rest on it: the first compares object ids from command substitutions,
// the second asks the shape-validating worktree-state.mjs verdict vendored into this core.
func TestBossFinalizeSkillCleanlinessChecksCannotBeFabricated(t *testing.T) {
	skill := readEmbeddedBossFinalizeSkill(t)

	push := sectionBetween(t, skill, "### Step 6: Push to Remote", "### Step 6b")
	assertNotContains(t, push, "git status  # Verify")
	assertContains(t, push, `LOCAL_HEAD="$(git rev-parse HEAD)" || exit 1`)
	assertContains(t, push, `PUSHED_HEAD="$(git rev-parse '@{push}')" || exit 1`)
	assertContains(t, push, `test -n "$LOCAL_HEAD" && test "$LOCAL_HEAD" = "$PUSHED_HEAD"`)
	assertContains(t, push, "git push --force-with-lease || {")
	if strings.Index(push, "git push --force-with-lease") > strings.Index(push, "LOCAL_HEAD=") {
		t.Fatalf("the push-destination comparison must follow the push it verifies")
	}

	cleanup := sectionBetween(t, skill, "### Step 7: Clean Up and Verify", "### Step 8")
	assertNotContains(t, cleanup, "git status            # Confirm clean state")
	assertContains(t, cleanup, `node "$BOSS_FINALIZE_TOOLBOX/worktree-state.mjs"`)
	assertContains(t, cleanup, "`unknown` (or no verdict line) is never clean")

	if _, err := SkillsFS.ReadFile("skills/boss-finalize/toolbox/worktree-state.mjs"); err != nil {
		t.Fatalf("worktree-state.mjs must be vendored into boss-finalize: %v", err)
	}
}

func TestBossFinalizeEmbeddedSkillCopiesStayIdentical(t *testing.T) {
	serviceSkill := readEmbeddedBossFinalizeSkill(t)

	repoRoot := findRepoRoot(t)
	pluginPath := filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills", "boss-finalize", "SKILL.md")
	pluginSkillBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("read plugin boss-finalize skill: %v", err)
	}

	if serviceSkill != string(pluginSkillBytes) {
		t.Fatalf("boss-finalize skill copies differ between services/boss and bossd-plugin-claude")
	}
}

func readEmbeddedBossFinalizeSkill(t *testing.T) string {
	t.Helper()

	skillBytes, err := SkillsFS.ReadFile("skills/boss-finalize/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded boss-finalize skill: %v", err)
	}
	return string(skillBytes)
}
