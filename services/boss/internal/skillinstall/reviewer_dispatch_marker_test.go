package skillinstall

import (
	"strings"
	"testing"
)

// reviewerDispatchMarker is the fixed token line that must lead every reviewer
// worker prompt. Run-cost telemetry counts reviewer subagents by matching it at
// the head of a dispatched prompt (see
// lib/bossalib/agenttelemetry.ReviewerDispatchMarker), so a brief that drops it
// silently undercounts the run rather than failing anything at review time.
// Keep this literal byte-identical to the Go constant.
const reviewerDispatchMarker = "[bs-reviewer-dispatch]"

func readEmbeddedSkillFile(t *testing.T, path string) string {
	t.Helper()

	b, err := SkillsFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestReviewDispatchBriefsCarryReviewerMarker pins the marker into every brief
// that starts a reviewer subagent. The assertions are scoped to the section that
// owns each dispatch, so a marker that survives only in an unrelated part of the
// file does not keep this test green.
func TestReviewDispatchBriefsCarryReviewerMarker(t *testing.T) {
	review := readEmbeddedSkillFile(t, "skills/boss-review/SKILL.md")

	// The normative rule: every tier, every phase, envelope or prose.
	assertContains(t, markdownSection(t, review, "## Operating rules"), reviewerDispatchMarker)

	// Phase 1 Tier 2 — the fenced lens reviewer template. The marker must be the
	// first line of the prompt body, directly under the dispatch descriptor.
	phase1 := sectionBetween(t, review, "## Phase 1 — Specialist lens passes (additive, conditional, parallel subagents)", "## Phase R — Review rounds (discovered; 3-tier fallback contract)")
	assertContains(t, phase1, "Subagent (general-purpose), AWAITED, read-only:\n  "+reviewerDispatchMarker)

	// Phase R Tier 3 — the inline whole-diff rubric prompt.
	phaseR := sectionBetween(t, review, "## Phase R — Review rounds (discovered; 3-tier fallback contract)", "## Phase D — Default rounds (opportunistic; additive, never a tier)")
	assertContains(t, phaseR, reviewerDispatchMarker+"\n\nYou are a code reviewer for one boss-review round.")

	// Phase D — the default-round worker brief.
	phaseD := sectionBetween(t, review, "## Phase D — Default rounds (opportunistic; additive, never a tier)", "## Phase 5 — Categorize")
	assertContains(t, phaseD, reviewerDispatchMarker)

	// boss-build Step 6 dispatches the review subagent that runs the whole stack.
	build := readEmbeddedSkillFile(t, "skills/boss-build/SKILL.md")
	assertContains(t, markdownSection(t, build, "## Step 6: Whole-branch review (dispatch the review pass)"), reviewerDispatchMarker)

	stack := readEmbeddedSkillFile(t, "skills/boss-build/references/review-stack.md")
	assertContains(t, stack, reviewerDispatchMarker)
}

// TestFinalizeStepTwelvePrintsTerminalState pins the Step 12 instruction that
// produces the terminal-state token telemetry extracts. There is no fenced
// output template to pin, so the pin is the governing sentence plus the token
// set, plus the line-leading requirement extraction actually enforces. Pinning
// the sentence alone would let the instruction and the parser drift apart while
// this test stayed green.
func TestFinalizeStepTwelvePrintsTerminalState(t *testing.T) {
	finalize := readEmbeddedSkillFile(t, "skills/boss-build/references/finalize-and-stop.md")
	stop := markdownSection(t, finalize, "## Step 12: Stop cleanly")

	assertContains(t, stop, "Output the terminal state (`REVIEW_READY` / `PARTIAL` / `BLOCKED` / `NO_CHANGE`) as the first token")

	// Pin the parser's contract, not just the sentence carrying it. terminalStateInText
	// matches only a line-leading token, so an instruction that says "print the state"
	// without saying "lead the line with it" is satisfiable by a print that extracts to "".
	assertContains(t, stop, "line-leading")

	for _, token := range []string{"REVIEW_READY", "PARTIAL", "BLOCKED", "NO_CHANGE"} {
		if !strings.Contains(stop, token) {
			t.Errorf("Step 12 no longer names terminal state %q; run-cost extraction pins this token set", token)
		}
	}
}
