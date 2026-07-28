package skillinstall

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// TestBossRepairSkillPinsLinearHistoryInvariant pins the linear-history contract: a repair
// pass syncs with the base by rebasing, never by merging the base into the branch (a merge
// commit structurally breaks a rebase-merge repo and deadlocks the PR), asserts a zero
// merge-commit preflight before pushing, and documents the linearize recovery for a branch
// that already carries one.
func TestBossRepairSkillPinsLinearHistoryInvariant(t *testing.T) {
	skill := readEmbeddedBossRepairSkill(t)
	invariant := markdownSection(t, skill, "## Linear-History Invariant")

	// Base sync is a rebase, and merging the base in is forbidden with the reason stated.
	assertContains(t, invariant, "git rebase \"origin/$BASE_BRANCH\"")
	assertContains(t, invariant, "Never merge the base branch into the session branch")
	assertContains(t, invariant, "FORBIDDEN")
	assertContains(t, invariant, "rebase-merge")
	assertContains(t, invariant, "git pull --rebase")

	// The mechanical preflight, stated as a command that must evaluate to zero.
	assertContains(t, invariant, "git rev-list --merges --count \"origin/$BASE_BRANCH\"..HEAD")

	// Poisoned-branch diagnosis + linearize recovery.
	assertContains(t, invariant, "git rebase --onto \"origin/$BASE_BRANCH\" \"$(git merge-base \"origin/$BASE_BRANCH\" HEAD)\"")
	assertContains(t, invariant, "git push --force-with-lease")
	assertContains(t, invariant, "--rebase-merges")

	// The conflict strategy and its push step must carry the invariant too.
	assertContains(t, skill, "[Linear-History Invariant](#linear-history-invariant)")
	strategyA := sectionBetween(t, skill, "#### Strategy A: Merge Conflicts", "#### Strategy B: Failing Checks")
	assertContains(t, strategyA, "git rev-list --merges --count \"origin/$BASE_BRANCH\"..HEAD")
	assertContains(t, strategyA, "git push --force-with-lease")
}

// TestBossRepairSkillResidualContract pins the residuals-vs-true-stops model. A condition the pass
// legitimately could not resolve is a RESIDUAL: it is reported and the pass still exits zero. Only
// a TRUE STOP — the worker unable to run at all — exits non-zero. Retry, cooldown, and attempt caps
// belong to the daemon-owned repair loop, so the worker must never simulate them by failing on
// purpose. Two edge cases used to instruct the opposite ("Exit with failure status"), which
// conflated "I finished and something remains" with "I crashed".
//
// The absence of that directive is asserted over the WHOLE skill, not just the new section: a gate
// scoped to the section alone would go green while the contradiction survived a few hundred lines
// below, which is precisely the defect this pins. For the same reason the gate also covers the four
// other places that stated the old binary success/failure contract in different words — Guidelines
// "Fail Fast", the Integration bullet, Watch Mode's short-of-green terminations, and the completion
// checklist. A negative assertion on one literal spelling cannot catch a synonym, so each of those
// windows is pinned positively on the reconciled wording.
//
// Windows come from sectionBetween rather than markdownSection because sectionBetween fatals on an
// ambiguous or non-line-anchored start heading, whereas markdownSection is a plain strings.Index
// that a prose cross-reference could silently drag the window's left edge onto.
func TestBossRepairSkillResidualContract(t *testing.T) {
	skill := readEmbeddedBossRepairSkill(t)
	residuals := sectionBetween(t, skill, "## Residuals vs true stops", "## Edge Cases and Error Handling")

	// Both terms are defined, each with its exit code, and a residual is explicitly not silent.
	assertContains(t, residuals, "**Residual**")
	assertContains(t, residuals, "is **not an error**")
	assertContains(t, residuals, "**exit zero**")
	assertContains(t, residuals, "never a silent one")
	assertContains(t, residuals, "**True stop**")
	assertContains(t, residuals, "**exit non-zero**")

	// The daemon-owned loop, not the worker, owns retry/cooldown/attempt caps.
	assertContains(t, residuals, "Retry, cooldown, and attempt caps belong to the **daemon-owned repair loop**")
	assertContains(t, residuals, "Never simulate backoff by failing on purpose")

	// The reason failing is wrong must stay the TRUE one. A non-zero exit is recorded as a failed
	// attempt, which escalates the backoff and the consecutive-failure count; it does not discard
	// the loop's accounting, and it does not "apply the cooldown" the branch needs. Pinned because
	// the inverted rationale reads just as fluently and would send a reader back to failing on
	// purpose for exactly the reason this section removes.
	assertContains(t, residuals, "recorded as a **failed attempt**")
	assertContains(t, residuals, "charges the branch for one it did not earn")

	// The in-snippet `exit 1` guards are step aborts, not a third exit-status outcome.
	assertContains(t, residuals, "abort that step")

	// Daemon retry-ownership carries the same Watch Mode carve-out every other statement of the
	// contract carries: a hand-run watch owns its loop, and no daemon loop sits behind it. Step 11
	// of Watch Mode links into this section, so without the carve-out it routes a watch reader to
	// prose telling it retries are not its job.
	assertContains(t, residuals, "Watch Mode is the exception")

	// The two rewritten edge cases report a residual and exit zero, keeping their diagnostics.
	complexConflicts := sectionBetween(t, skill, "### Complex Conflicts", "### Cascading Failures")
	assertContains(t, complexConflicts, "gh pr comment --body \"Automatic repair detected complex merge conflicts")
	assertContains(t, complexConflicts, "**residual**")
	assertContains(t, complexConflicts, "**exit zero**")

	missingContext := sectionBetween(t, skill, "### Missing Context", "## Guidelines")
	assertContains(t, missingContext, "Add a PR comment requesting clarification")
	assertContains(t, missingContext, "Do NOT make assumptions")
	assertContains(t, missingContext, "**residual**")
	assertContains(t, missingContext, "**exit zero**")

	// The rest of the skill must not restate the old binary success/failure contract. These three
	// windows each used to instruct — or strongly imply — a non-zero exit for a residual, which is
	// the same defect one section removed and three others quietly kept. Pinned per window so a
	// regression names the section it crept back into.
	guidelines := sectionBetween(t, skill, "## Guidelines", "## Anti-Patterns")
	assertContains(t, guidelines, "report the residual and exit zero")
	assertNotContains(t, guidelines, "Fail Fast")

	integration := sectionBetween(t, skill, "## Integration with Repair Plugin", "## Watch Mode")
	assertContains(t, integration, "Exit zero once a pass completes")
	assertContains(t, integration, "only on a true stop")
	assertNotContains(t, integration, "success/failure status")

	// Watch mode owns its own loop, so its two short-of-green terminations (no-progress, the 5-pass
	// bound) are the places most likely to reintroduce a failure exit for a normal outcome.
	watchMode := sectionBetween(t, skill, "## Watch Mode", "## Checklist")
	assertContains(t, watchMode, "Ending short of green is a residual, not a true stop")
	assertContains(t, watchMode, "**exit zero**")
	assertNotContains(t, watchMode, "exit success only when")

	// Every terminal state of the watch loop carries an exit instruction and a reason token — the
	// green one included, which is the state most easily left implicit once step 8 became a
	// definition rather than a directive.
	assertContains(t, watchMode, "Once all four hold, stop and exit zero.")

	// The escalation terminal reuses `blocked` rather than minting a token of its own. Callers that
	// classify this line accept a closed vocabulary, and a token outside it degrades to a retry —
	// which would send a case the skill just declared needs a human back around the loop.
	assertContains(t, watchMode, "`max-attempts` / `blocked`")
	assertNotContains(t, watchMode, "`escalated`")

	// Strategy C's deliberately-unresolved clarification thread is the most common residual on the
	// main line. It was never *contradicted* by the old contract, only omitted from it, so neither
	// the whole-file negative nor the four synonym windows would notice its designation being
	// dropped again.
	strategyC := sectionBetween(t, skill, "#### Strategy C: Review Feedback", "### Phase 3: Verify and Monitor")
	assertContains(t, strategyC, "A thread left open this way is a **residual**")

	// The completion checklist asks whether the residual was actually reported, so a pass cannot be
	// walked to "done" with an unreported one.
	checklist := sectionBetween(t, skill, "## Checklist", "## Success Criteria")
	assertContains(t, checklist, "Residuals reported in the summary")

	// A residual is visible in the round's output rather than implied by its absence. The window
	// ends at the residuals section so this cannot be satisfied by the definition prose above.
	summary := sectionBetween(t, skill, "## Repair Summary", "## Residuals vs true stops")
	assertContains(t, summary, "**Residuals**:")

	// The contradicted directive is gone from the whole skill, not merely from these sections.
	assertNotContains(t, skill, "Exit with failure status")
}

// TestBossRepairSkillStaleSHAContract pins the stale-SHA cancellation rule in Phase 2. A round reads
// PR state over several seconds; if a push lands mid-round, the check failures it read describe a
// commit that is no longer the head. Acting on them "fixes" a failure that no longer exists, burns an
// attempt against the driving loop's cap, and can push a confusing commit. The rule states three
// ORDERED obligations, and each is pinned separately because dropping any one of them silently
// restores the defect:
//
//  1. Capture the head SHA at the top of the round, BEFORE any PR state is read. Captured later — say
//     after the review probe — the baseline already includes a mid-round push, so the comparison in
//     (3) can never fire and the gate would pass on a rule that never triggers.
//  2. Handle review feedback before CI failures. This is a real ordering change, not a stylistic one:
//     thread content is stable, check runs are volatile, so reading the stable half first confines the
//     freshness problem to the half that actually has one. Pinned because the ordering reads as
//     arbitrary to anyone editing the section and is the first thing a reorganisation would drop.
//  3. Re-read the head SHA immediately before acting on a check failure and skip when it moved.
//
// The skip must be framed as CANCELLATION: a superseded round is a normal outcome reported as a
// residual with a zero exit, deferring to the existing residual model rather than restating it. A
// non-zero exit here would be recorded as a failed attempt against a branch that was never broken —
// the same defect the residual contract removes elsewhere — so the exit code and the cross-reference
// are both pinned, and the cross-reference is checked to RESOLVE (its target heading really exists),
// since a dangling anchor renders as ordinary text and takes the reader nowhere.
//
// The own-push carve-out is pinned hardest. A round that reads a failure, fixes it, and pushes moves
// the head BY ITS OWN HAND. Worded loosely — "if the head has moved, the round is stale" — every
// successful round would report itself stale and cancel its own work, which is strictly worse than
// having no rule at all. The wording must therefore say both that the test happens before acting and
// that a round's own commits never trip it.
//
// The window is scoped to the Phase 2 preamble via sectionBetween, not a whole-file search: a
// document-wide match would go green with the rule written under an unrelated heading, where the
// worker executing Phase 2 would never read it. sectionBetween (not markdownSection) is required
// because markdownSection only splits on top-level `## ` headings and cannot scope to a `### ` one;
// it also fatals on an ambiguous or non-line-anchored marker, and rejects a same-or-higher-level
// heading sneaking into the window, so the window cannot widen silently into Strategy A/B/C.
func TestBossRepairSkillStaleSHAContract(t *testing.T) {
	skill := readEmbeddedBossRepairSkill(t)
	phase2 := sectionBetween(t, skill, "### Phase 2: Execute Repair Strategy", "#### Strategy A: Merge Conflicts")

	// (1) The baseline is captured first, and the prose names what "first" means — before any of the
	// three state reads a round performs — so the capture cannot drift below one of them.
	assertContains(t, phase2, "capture the head SHA before reading PR state")
	assertContains(t, phase2, "before reading review threads, check runs, or mergeability")

	// The baseline is the PR head, not the local worktree tip. `git rev-parse HEAD` only moves when
	// this round itself commits — the one case the own-push carve-out below excludes — so a rule
	// written against it could ONLY ever fire on the round's own work and would be vacuous against
	// the foreign push it exists to catch. The rejection of the local tip is pinned alongside the
	// positive form so the cheaper-looking command cannot be swapped back in.
	assertContains(t, phase2, "gh pr view --json headRefOid -q .headRefOid")
	assertContains(t, phase2, "The local worktree tip is **not** the value to compare")
	assertContains(t, phase2, "`git rev-parse HEAD` only moves")

	// Placement, not just wording: Phase 1.1 reads PR state (`gh pr view`), so a capture that only
	// appears under Phase 2 is captured AFTER the CI view is already formed and the comparison in (3)
	// can never fire. Pin the command in the Phase 1 window too, and pin the Phase 2 prose that says
	// where it lives so the two cannot drift apart.
	assertContains(t, phase2, "ahead of `gh pr view`")
	phase1 := sectionBetween(t, skill, "### Phase 1: Assess Current State", "### Phase 2: Execute Repair Strategy")
	assertContains(t, phase1, "gh pr view --json headRefOid -q .headRefOid")
	assertContains(t, phase1, "Record the PR head SHA **first**")

	// Presence in Phase 1 is not enough — ORDER is the whole point. Assert positionally that the
	// capture precedes the bare `gh pr view` that reads PR state, so moving the line down inside the
	// same fence turns the gate red instead of leaving it green on a document that captures the
	// baseline after the read it is supposed to precede.
	captureAt := strings.Index(phase1, "gh pr view --json headRefOid -q .headRefOid")
	readAt := strings.Index(phase1, "\ngh pr view  ")
	if captureAt < 0 || readAt < 0 || captureAt > readAt {
		t.Fatalf("Phase 1.1 must capture the PR head SHA before the `gh pr view` state read (capture at %d, read at %d)", captureAt, readAt)
	}

	// Phase 2 restates the capture; it must not present it as a second runnable step, or the worker
	// re-baselines after Phase 1's read (and possibly after its own feedback push) and the
	// comparison can never fire.
	assertContains(t, phase2, "**Do not run it again here** as the round's routine baseline")
	assertContains(t, phase2, "The missing-baseline recovery below is the one exception")

	// A bare shell variable does not survive between an agent's separate tool calls, and an unset
	// variable compares UNEQUAL — which would cancel every round. The fail-safe direction (missing
	// baseline => proceed, never cancel) is what keeps the rule from being worse than no rule.
	assertContains(t, phase2, "shell state does not survive")
	assertContains(t, phase2, "do not cancel and do not proceed on what you already")
	assertContains(t, phase2, "re-capture the PR head and re-read the check runs against it")
	assertContains(t, phase2, "A missing baseline is never on its own a reason to cancel")

	// The window between deciding to act and pushing is small but not empty: without a final check a
	// foreign push landing inside it is still clobbered by the fix the round already built.
	assertContains(t, phase2, "Check the head once more immediately before you push a CI fix")
	assertContains(t, phase2, "rather than force-pushing over the newer commit")

	// A pre-push cancellation leaves a committed, unpushed fix. Without a stated disposition the
	// worker either pushes it anyway (defeating the cancellation) or trips Phase 3's clean-tree check
	// and Watch Mode's no-progress comparison with a dangling commit.
	assertContains(t, phase2, "Keep the commit you already made")
	assertContains(t, phase2, "name it in the residual as built but unpushed")

	// (2) Feedback precedes CI, with the stable-versus-volatile rationale that makes the order
	// non-arbitrary. Without the rationale a later editor reads the ordering as incidental.
	assertContains(t, phase2, "Handle review feedback before CI failures")
	assertContains(t, phase2, "Thread content is stable")
	assertContains(t, phase2, "Check runs are volatile")

	// (3) The re-check and the skip. "superseded" is the word the residual line reports, so it is
	// pinned as the term of art rather than left to a synonym.
	assertContains(t, phase2, "Re-read the head SHA before acting on a CI failure")
	assertContains(t, phase2, "skip the failure when it has moved")
	assertContains(t, phase2, "superseded commit")
	assertContains(t, phase2, "do not fix it and do not push a commit for it")

	// Cancellation, not error: reported as a residual, zero exit, deferring to the residual model.
	assertContains(t, phase2, "**cancellation, not error**")
	assertContains(t, phase2, "A superseded round is a normal outcome")
	assertContains(t, phase2, "Report the superseded CI view as a residual and **exit zero**")
	assertContains(t, phase2, "must never exit non-zero")
	assertContains(t, phase2, "[Residuals vs true stops](#residuals-vs-true-stops)")

	// Scope: the skip drops the CI half of the round, not the whole pass. Without this a round that
	// also owes conflict or review work would end the pass on the stale CI signal alone.
	assertContains(t, phase2, "Skip only the CI half of the round")

	// Watch Mode owns its own bounded loop and must re-poll rather than exit — the same carve-out the
	// residual model already names. A rule that told a watch run to "exit zero" would abandon up to
	// four remaining passes.
	assertContains(t, phase2, "In Watch Mode the cancellation is not an exit at all")
	assertContains(t, phase2, "return to the poll loop")
	assertContains(t, phase2, "[Watch Mode](#watch-mode)")

	// The cross-reference must RESOLVE. The rule deliberately does not restate the residual model, so
	// if the target section were renamed or removed the reader would be left with a dead link and no
	// statement of the model anywhere in reach.
	sectionBetween(t, skill, "## Residuals vs true stops", "## Edge Cases and Error Handling")

	// The own-push carve-out. Both halves are required: the test happens BEFORE acting, and a round's
	// own commits are excluded. Either half alone still permits the reading in which a round that
	// fixes and pushes cancels itself.
	assertContains(t, phase2, "A round's own push never marks that round stale")
	assertContains(t, phase2, "only against commits this round did not author")
	assertContains(t, phase2, "is _expected_ to differ from `ROUND_HEAD`")
	assertContains(t, phase2, "re-baseline `ROUND_HEAD` to the commit you just pushed")
	assertContains(t, phase2, "Only a head that moved before this round acted")

	// Re-baselining alone would move the SHA without refreshing the stale data it guards: obligation
	// (2) puts a review-feedback push FIRST, so the check runs read before that push describe a
	// commit the round itself superseded, yet the re-baselined comparison would pass. The re-read is
	// therefore pinned to the same sentence as the re-baseline.
	assertContains(t, phase2, "**and re-read the check runs**")
	assertContains(t, phase2, "a CI view read before any push, your own included")

	// Authorship is not derivable from a SHA, and nothing in the document tells a worker to inspect
	// it. The prose must resolve the clause into the mechanism that actually implements it rather
	// than leaving an untestable obligation standing.
	assertContains(t, phase2, "You never have to inspect authorship")
	assertContains(t, phase2, "any remaining difference from `ROUND_HEAD` is by construction")

	// The re-read cannot conjure runs that GitHub has not created yet. Without this the worker, told
	// to re-read after its own push, would get pending/empty and fall back on the pre-push results —
	// re-arming the stale-view defect with a freshly re-baselined SHA certifying it as current. It
	// also keeps the re-read from being read as a licence to loop past default mode's one-pass
	// contract in Phase 3.
	assertContains(t, phase2, "Check runs for a commit you just pushed usually do not exist yet")
	assertContains(t, phase2, "not a second repair pass and not the Phase 3 poll")
	assertContains(t, phase2, "there is no fresh CI view to act on")

	// Watch Mode carries its own `$BEFORE` baseline captured with `git rev-parse HEAD` — the very
	// command this rule rejects. Naming the two baselines and their different questions is what stops
	// a watch worker from substituting one for the other and silently restoring the vacuous form.
	assertContains(t, phase2, "`$BEFORE` in")
	assertContains(t, phase2, "They are not interchangeable")
}

// sectionBetween returns the slice of markdown from start (inclusive) up to end (exclusive).
// markdownSection only splits on top-level `## ` headings, so this is the precise form used
// when an assertion must stay inside one subsection.
//
// Both markers must match at the START of a line and start with `#`, and start must be unique.
// Without that anchoring an inline prose mention of a heading (e.g. "see Step 6d: …") could move
// the window's left edge earlier and silently widen it across most of the document, making every
// `assertContains` on the result pass vacuously — a gate that stops gating without failing.
func sectionBetween(t *testing.T, markdown, start, end string) string {
	t.Helper()

	section, err := markdownSectionBetween(markdown, start, end)
	if err != nil {
		t.Fatalf("section %q..%q: %v", start, end, err)
	}
	return section
}

// markdownSectionBetween is the pure form of sectionBetween: it returns an error rather than
// failing a test, so its five rejection paths can be exercised directly by TestSectionBetween.
// Every prose gate in this file is only as strong as this window, and a window that widens
// silently turns each `assertContains` on it into a no-op — so the logic that prevents that is
// itself covered rather than trusted.
func markdownSectionBetween(markdown, start, end string) (string, error) {
	for _, marker := range []string{start, end} {
		if !strings.HasPrefix(marker, "#") {
			return "", fmt.Errorf("section marker %q must be a markdown heading prefix", marker)
		}
	}

	starts := lineAnchoredIndexes(markdown, start, 0)
	if len(starts) == 0 {
		return "", fmt.Errorf("start heading %q not found at the start of any line", start)
	}
	if len(starts) > 1 {
		return "", fmt.Errorf("start heading %q is ambiguous (%d line-anchored matches); the section window would be unsound", start, len(starts))
	}
	begin := starts[0]
	// Skip the start heading's own line so a shared prefix (e.g. "### Step 6" as the start and
	// "### Step 6b" as the end) cannot terminate the window on the heading that opened it.
	nl := strings.Index(markdown[begin:], "\n")
	if nl == -1 {
		return "", fmt.Errorf("start heading %q has no body", start)
	}
	ends := lineAnchoredIndexes(markdown, end, begin+nl+1)
	if len(ends) == 0 {
		return "", fmt.Errorf("end heading %q not found at the start of any line after %q", end, start)
	}
	section := markdown[begin:ends[0]]

	// Guard the RIGHT edge too. Anchoring `start` only stops the window growing leftward; a new
	// heading inserted between the two markers grows it rightward just as silently, because
	// `end` is still found — just further away. Reject any heading at or above the start's own
	// level inside the window: crossing one means the window no longer covers one section, and
	// every assertion on it may be satisfied by prose the caller never meant to include.
	//
	// Fenced blocks are skipped: these skills are full of shell snippets whose `# comment` lines
	// begin a line with `#` and would otherwise read as an H1 mid-section.
	//
	// The fence state is seeded by scanning from the top of the document, NOT assumed closed at
	// the window's left edge. One live window anchors on `## Repair Summary`, which is template
	// text inside a fence rather than a real heading — seeding `false` there inverts the state,
	// so the template's CLOSING fence reads as an opening one and every line after it is skipped
	// as fenced. That silently disables the guard on exactly the window it was added to protect.
	depth := len(start) - len(strings.TrimLeft(start, "#"))
	var fence byte
	for _, line := range strings.Split(markdown[:begin], "\n") {
		fence = stepFence(fence, line)
	}
	for _, line := range strings.Split(section, "\n")[1:] {
		if next := stepFence(fence, line); next != fence {
			fence = next
			continue
		}
		if fence != 0 || !strings.HasPrefix(line, "#") {
			continue
		}
		level := len(line) - len(strings.TrimLeft(line, "#"))
		// A real ATX heading separates its hashes from the text; `#!/bin/sh` and `#comment` do not.
		if level > depth || !strings.HasPrefix(line[level:], " ") {
			continue
		}
		return "", fmt.Errorf("heading %q sits between %q and %q; the section window would silently span more than one section", line, start, end)
	}
	return section, nil
}

// stepFence advances the fenced-code state across one line, returning the new state: 0 outside a
// fence, otherwise the character that opened the current one.
//
// The opening character is tracked rather than a bare in/out flag because a fence is closed only
// by a fence of the SAME character. Toggling on either marker makes a `~~~` line inside a
// backtick block read as a close, which flips the state for the rest of the document and turns
// genuinely-fenced `#` lines back into headings — a false reject in a helper every prose gate in
// this package shares.
func stepFence(fence byte, line string) byte {
	var marker byte
	switch trimmed := strings.TrimSpace(line); {
	case strings.HasPrefix(trimmed, "```"):
		marker = '`'
	case strings.HasPrefix(trimmed, "~~~"):
		marker = '~'
	default:
		return fence
	}
	if fence == 0 {
		return marker
	}
	if fence == marker {
		return 0
	}
	return fence
}

// lineAnchoredIndexes returns every offset at or after `from` where `marker` begins a line.
func lineAnchoredIndexes(markdown, marker string, from int) []int {
	var found []int
	for i := from; i < len(markdown); {
		rel := strings.Index(markdown[i:], marker)
		if rel == -1 {
			break
		}
		at := i + rel
		if at == 0 || markdown[at-1] == '\n' {
			found = append(found, at)
		}
		i = at + 1
	}
	return found
}

func TestSectionBetween(t *testing.T) {
	const doc = "## Alpha\nalpha body\n\n### Alpha Sub\nsub body\n\n## Beta\nbeta body\n\n## Gamma\ngamma body\n"

	tests := []struct {
		name       string
		markdown   string
		start, end string
		want       string
		wantErr    string
	}{
		{
			name: "window stops at the end heading", markdown: doc,
			start: "## Alpha", end: "## Beta",
			want: "## Alpha\nalpha body\n\n### Alpha Sub\nsub body\n\n",
		},
		{
			name: "deeper headings inside the window are allowed", markdown: doc,
			start: "### Alpha Sub", end: "## Beta",
			want: "### Alpha Sub\nsub body\n\n",
		},
		{
			name: "a same-level heading inside the window is rejected", markdown: doc,
			start: "## Alpha", end: "## Gamma",
			wantErr: "silently span more than one section",
		},
		{
			name: "an ambiguous start heading is rejected", markdown: "## Dup\na\n\n## Dup\nb\n\n## End\n",
			start: "## Dup", end: "## End",
			wantErr: "is ambiguous",
		},
		{
			name: "a non-heading marker is rejected", markdown: doc,
			start: "Alpha", end: "## Beta",
			wantErr: "must be a markdown heading prefix",
		},
		{
			name: "a start matched only mid-line is not found", markdown: "intro see ## Alpha here\n## Beta\n",
			start: "## Alpha", end: "## Beta",
			wantErr: "not found at the start of any line",
		},
		{
			name: "an end that only precedes the start is not found", markdown: "## Beta\nbeta\n\n## Alpha\nalpha\n",
			start: "## Alpha", end: "## Beta",
			wantErr: "not found at the start of any line after",
		},
		{
			name:     "a shell comment inside a fence is not a heading",
			markdown: "## Alpha\n\n```bash\n# Resolve conflicts, then:\ngit push\n```\n\n## Beta\nbeta\n",
			start:    "## Alpha", end: "## Beta",
			want: "## Alpha\n\n```bash\n# Resolve conflicts, then:\ngit push\n```\n\n",
		},
		{
			name:     "a shebang outside a fence is not a heading",
			markdown: "## Alpha\n#!/bin/sh\nbody\n\n## Beta\nbeta\n",
			start:    "## Alpha", end: "## Beta",
			want: "## Alpha\n#!/bin/sh\nbody\n\n",
		},
		{
			// The live `## Repair Summary` window has exactly this shape. Seeding the fence state
			// from the window's left edge instead of the document's top inverts it here, and the
			// guard silently stops guarding.
			name: "a start marker inside a fence still detects a later heading",
			markdown: "## Real Heading\n\n```\n## Alpha\ntemplate body\n```\n\n" +
				"## Sneaked In\nprose\n\n## Beta\nbeta\n",
			start: "## Alpha", end: "## Beta",
			wantErr: "silently span more than one section",
		},
		{
			name:     "a start marker inside a fence accepts a clean window",
			markdown: "## Real Heading\n\n```\n## Alpha\ntemplate body\n```\n\n## Beta\nbeta\n",
			start:    "## Alpha", end: "## Beta",
			want: "## Alpha\ntemplate body\n```\n\n",
		},
		{
			name:     "the start heading's own line is not treated as an intruder",
			markdown: "## Alpha\nbody\n\n## Beta\nbeta\n",
			start:    "## Alpha", end: "## Beta",
			want: "## Alpha\nbody\n\n",
		},
		{
			// A tilde line inside a backtick fence must not close it; treating it as a close
			// flips the state and turns the still-fenced `## Inner` into a spurious intruder.
			name:     "a tilde line does not close a backtick fence",
			markdown: "## Alpha\n\n```\n~~~\n## Inner\n```\n\n## Beta\nbeta\n",
			start:    "## Alpha", end: "## Beta",
			want: "## Alpha\n\n```\n~~~\n## Inner\n```\n\n",
		},
		{
			name:     "a tilde fence still hides a heading",
			markdown: "## Alpha\n\n~~~\n## Inner\n~~~\n\n## Beta\nbeta\n",
			start:    "## Alpha", end: "## Beta",
			want: "## Alpha\n\n~~~\n## Inner\n~~~\n\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := markdownSectionBetween(tc.markdown, tc.start, tc.end)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got section %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("section mismatch:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestBossRepairEmbeddedSkillCopiesStayIdentical pins the whole boss-repair skill directory —
// not just SKILL.md — as byte-identical between the tree embedded into services/boss and the
// bossd-plugin-claude mirror refreshed by `make copy-skills`. Every file under the directory
// ships to users, so a file that drifts in one tree ships a skill whose script differs from
// the one the plugin installs.
//
// This is defence-in-depth, not the primary mirror gate. Whole-tree parity across ALL skills
// already lives in services/boss/internal/skillparity (TestSkillPayloadParity, BOS-344 Task 5):
// it walks both trees from disk and compares the full path set plus sha256 content, in both
// directions, and it runs under `bazel test` as well as `go test`. This test is deliberately
// narrower — boss-repair only — and its value is a skill-scoped failure message. It is NOT
// closing a coverage gap; scripts/review-feedback-probe.js is already hashed in both directions
// by TestSkillPayloadParity. What it does close is this test's own former inconsistency: its
// name and failure message promised to pin the skill copies while it compared a single file.
//
// Two things NOT to over-read here. Reading through SkillsFS rather than from disk buys nothing
// today: under `go test` the `//go:embed all:skills` directive materialises SkillsFS from the
// same checkout skillparity reads, so the two are identical by construction. It would only
// diverge under bazel, where embedsrcs is a hand-maintained list — and this target is tagged
// `manual`, so no build invokes it there. Conversely, `manual` does not mean uncovered: the native
// ledger pass (scripts/bazel/ledger.json) runs `go test ./internal/skillinstall` under
// `make test-boss`, so this gate does execute in CI.
func TestBossRepairEmbeddedSkillCopiesStayIdentical(t *testing.T) {
	const embeddedRoot = "skills/boss-repair"

	embeddedFS, err := fs.Sub(SkillsFS, embeddedRoot)
	if err != nil {
		t.Fatalf("sub-tree %q of the embedded skills FS: %v", embeddedRoot, err)
	}
	embedded := collectTree(t, embeddedFS, "embedded services/boss tree "+embeddedRoot)

	repoRoot := findRepoRoot(t)
	mirrorRoot := filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills", "boss-repair")
	mirror := collectTree(t, os.DirFS(mirrorRoot), "bossd-plugin-claude mirror "+mirrorRoot)

	// A walk that yields nothing makes every comparison below pass vacuously — the exact failure
	// mode this gate exists to close. A root that does not exist at all is already loud (WalkDir
	// hands the stat error to the callback, which returns it), but a root that exists and is
	// simply not the tree we mean — a renamed skill directory, a mirror emptied by a bad sync, a
	// `data` dep staged without its payload — is silent. Pin the files known to be tracked in
	// both trees so that case fails too.
	if len(embedded) == 0 {
		t.Fatalf("embedded boss-repair tree %q collected no files; the walk is mis-rooted", embeddedRoot)
	}
	if len(mirror) == 0 {
		t.Fatalf("mirror boss-repair tree %q collected no files; the walk is mis-rooted", mirrorRoot)
	}
	for _, want := range []string{"SKILL.md", "scripts/review-feedback-probe.js"} {
		if _, ok := embedded[want]; !ok {
			t.Fatalf("embedded boss-repair tree is missing %q; collected %v", want, sortedPaths(embedded))
		}
		if _, ok := mirror[want]; !ok {
			t.Fatalf("mirror boss-repair tree is missing %q; collected %v", want, sortedPaths(mirror))
		}
	}

	for _, rel := range sortedPaths(embedded) {
		mirrorBytes, ok := mirror[rel]
		if !ok {
			t.Errorf("boss-repair %s is embedded in services/boss but missing from the bossd-plugin-claude mirror; re-run `make copy-skills`", rel)
			continue
		}
		if !bytes.Equal(embedded[rel], mirrorBytes) {
			t.Errorf("boss-repair %s differs between services/boss and bossd-plugin-claude; re-run `make copy-skills`", rel)
		}
	}

	// The reverse direction a one-way loop misses: a file left behind in the mirror by a rename
	// or deletion on the embedded side.
	for _, rel := range sortedPaths(mirror) {
		if _, ok := embedded[rel]; !ok {
			t.Errorf("boss-repair %s exists in the bossd-plugin-claude mirror but not in the embedded services/boss tree; re-run `make copy-skills` to prune it (rsync --delete), or add it under services/boss/internal/skillinstall/%s if the removal was accidental", rel, embeddedRoot)
		}
	}
}

// collectTree walks fsys and returns every regular file's bytes keyed by its path relative to
// the FS root, with label naming the tree in any failure.
//
// Both trees are handed in as an fs.FS — fs.Sub for the embed, os.DirFS for the mirror — so
// io/fs supplies keys that are already root-relative and, per its path rules, always
// slash-separated. That is what makes an embed.FS path and an OS path directly comparable here:
// no prefix trimming, no filepath.Rel, and no filepath.ToSlash normalisation, on any platform.
func collectTree(t *testing.T, fsys fs.FS, label string) map[string][]byte {
	t.Helper()

	files := make(map[string][]byte)
	err := fs.WalkDir(fsys, ".", func(walked string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, walked)
		if err != nil {
			return err
		}
		files[walked] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", label, err)
	}
	return files
}

// sortedPaths returns the collected relative paths in a stable order so failure output does not
// vary run-to-run with Go's randomised map iteration.
func sortedPaths(files map[string][]byte) []string {
	return slices.Sorted(maps.Keys(files))
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

// assertContains and assertNotContains report with t.Errorf, not t.Fatalf. The gates in this file
// make dozens of mutually independent prose assertions, so aborting on the first mismatch would
// tell an author who reworded one paragraph about one broken pin per run — and these fail in
// batches, because a single edit usually moves several pinned sentences at once.
func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		t.Errorf("expected content to contain %q", needle)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if strings.Contains(haystack, needle) {
		t.Errorf("expected content not to contain %q", needle)
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
