package skillinstall

import (
	"io/fs"
	"testing"
)

// TestBossBuildStep7UsesVerifiedPushProcedure keeps the normal PR-gate push on
// the same durable path as the below-floor route. A one-shot push can fail and
// still let later terminal cleanup write BLOCKED, stranding completed commits
// in the worktree.
func TestBossBuildStep7UsesVerifiedPushProcedure(t *testing.T) {
	var canonical string
	for label, payload := range shippedPayloads(t) {
		data, err := fs.ReadFile(payload, "skills/boss-build/SKILL.md")
		if err != nil {
			t.Fatalf("%s: read boss-build skill: %v", label, err)
		}

		skill := string(data)
		if canonical == "" {
			canonical = skill
		} else if skill != canonical {
			t.Errorf("%s: boss-build skill differs from embedded payload", label)
		}

		step7 := sectionBetween(t, skill, "## Step 7: PR gate (create/reuse)", "## Steps 8-12:")
		assertContains(t, step7, "retry/rebase/rescue procedure")
		assertContains(t, step7, "references/review-stack.md")
		assertContains(t, step7, "Continue only when it sets `PUSHED=yes`")
		assertContains(t, step7, "`PUSHED=rescue` or `PUSHED=no` means the session")
		assertContains(t, step7, "Stop cleanly** `BLOCKED`")
		assertNotContains(t, step7, "git push -u origin \"$SESSION_BRANCH\"")
	}
}
