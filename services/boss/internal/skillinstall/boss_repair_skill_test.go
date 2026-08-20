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
	assertContains(t, skill, "gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -F body=@\"$REPLY_BODY\"")
	assertContains(t, skill, "Reply bodies are Markdown and must never be shell-interpolated.")
	assertContains(t, skill, "Use the agent's file-editing tool to write the complete reply text")
	assertContains(t, skill, "First run the path-creation block by itself")
	assertContains(t, skill, "printf '%s\\n' \"$REPLY_BODY\"")
	assertContains(t, skill, "REPLY_BODY=\"/the/path/printed/above\"")
	assertNotContains(t, skill, "cat >\"$REPLY_BODY\"")
}

func TestBossRepairSkillReviewRepliesPreserveAPIFailureStatus(t *testing.T) {
	skill := readEmbeddedBossRepairSkill(t)
	strategyC := sectionBetween(t, skill, "#### Strategy C: Review Feedback", "### Phase 3: Verify and Monitor")

	const replyCommand = "gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -F body=@\"$REPLY_BODY\""
	if got := strings.Count(strategyC, replyCommand); got != 3 {
		t.Fatalf("reply command count = %d, want 3", got)
	}
	if got := strings.Count(strategyC, "GH_STATUS=$?"); got != 3 {
		t.Fatalf("saved GitHub failure status count = %d, want 3", got)
	}
	if got := strings.Count(strategyC, "exit \"$GH_STATUS\""); got != 3 {
		t.Fatalf("preserved GitHub failure exit count = %d, want 3", got)
	}
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

	// A residual is visible in the round's output rather than implied by its absence. The window ends
	// at the section that FOLLOWS the summary template so this cannot be satisfied by the definition
	// prose below it. That end marker is `## Terminal outcomes`, not `## Residuals vs true stops`:
	// the terminal-outcome model was inserted between the two, and sectionBetween rejects a window
	// spanning a same-level heading — correctly, since the wider window would let the assertion be
	// satisfied by the outcome list rather than by the template that must carry the line.
	summary := sectionBetween(t, skill, "## Repair Summary", "## Terminal outcomes")
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

// TestBossRepairSkillPhase2InlineBranchContract pins where a Phase 2 strategy runs. Dispatch is the
// expected branch, and running the strategy inline is a written-down, sanctioned path — not an
// improvisation a round has to invent unaided when the subagent tool is present in the tool list but
// prohibited by a higher-priority instruction. That case is the common one and the original prose
// covered neither it nor the tool-absent one: inline appeared only as a dispatch-failure fallback.
//
// The window is scoped with sectionBetween for the reason TestBossRepairSkillStaleSHAContract above
// scopes its own — a document-wide match would go green with the branch written under some unrelated
// heading, where the worker executing Phase 2 would never read it.
//
// The negative half matters as much as the positive. The improvement note behind this rule asserted
// that cron runs are forbidden to dispatch subagents; that is false — boss-plan dispatches an awaited
// `general-purpose` subagent under `BOSS_CRON=true`, itself gate-pinned in
// scripts/bs-plan-skill.test.mjs — so the fix must not trade one false statement for another.
func TestBossRepairSkillPhase2InlineBranchContract(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			phase2 := sectionBetween(t, skill, "### Phase 2: Execute Repair Strategy", "#### Strategy A: Merge Conflicts")

			// The inline branch, and the word that makes it sanctioned rather than degraded.
			assertContains(t, phase2, "run the strategy inline (sanctioned)")
			assertContains(t, phase2, "Inline is the **sanctioned** path")

			// Both conditions the choice turns on. Availability alone misses a tool that is listed
			// but prohibited, which is precisely the case that sent rounds down an undocumented path.
			assertContains(t, phase2, "subagent tool permitted AND available")
			assertContains(t, phase2, "instruction prohibits calling it")

			// Dispatch stays the expected branch, so a session that CAN dispatch does not read the
			// inline branch as licence to take the weaker single-voice route.
			assertContains(t, phase2, "(the expected path)")

			// The dispatch branch still names the subagent type. Rewriting the branch prose is
			// exactly when this token goes missing, and an unnamed type leaves the branch called
			// "expected" the least specified of the three.
			assertContains(t, phase2, "subagent_type: general-purpose")

			// No claim that an unattended run may not dispatch. Lowercased so a capitalised
			// restatement cannot slip past.
			lowered := strings.ToLower(phase2)
			for _, forbidden := range []string{
				"may not dispatch",
				"must not dispatch",
				"cannot dispatch",
				"forbidden from dispatching",
				"forbidden to dispatch",
				"cron runs are forbidden",
			} {
				assertNotContains(t, lowered, forbidden)
			}
		})
	}
}

// TestBossRepairSkillTerminalOutcomeContract pins the closed set of pass outcomes. Before this
// section existed the skill described how to repair things but never named what a pass ARRIVES at,
// so a round that found an already-green, thread-clean, mergeable PR had no route through Phase 1.2
// (which presupposes a problem), no name for what it reached, and therefore read as an
// under-performed run — which is exactly the pressure that produces manufactured work.
//
// Each outcome is pinned WITH its exit instruction, because a name without an exit code leaves the
// worker to infer one, and the inference that "nothing happened" means "the run failed" is the
// defect. The scope-creep prohibition is pinned as three concrete prohibited acts rather than a
// general exhortation: "do not do unnecessary work" is unfalsifiable at the point of temptation,
// whereas "do not re-run gates this pass had no reason to invoke" names the act.
func TestBossRepairSkillTerminalOutcomeContract(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			outcomes := sectionBetween(t, skill, "## Terminal outcomes", "## Residuals vs true stops")

			// Every outcome is named, and each carries its own exit instruction.
			assertContains(t, outcomes, "**repaired** — a fix was committed and pushed. Exit zero.")
			assertContains(t, outcomes, "**nothing to repair**")
			assertContains(t, outcomes, "**parked no-op**")
			assertContains(t, outcomes, "**residual**")
			assertContains(t, outcomes, "**true stop** — the worker could not run at all. Exit non-zero")

			// "nothing to repair" is legitimate, not a failed pass, and its exit is spelled out.
			assertContains(t, outcomes, "a **legitimate terminal outcome, distinct from a failed pass**")
			assertContains(t, outcomes, "report **nothing to repair** with `**Residuals**: none`, and exit zero")

			// The prohibition, with the three concrete acts it forbids.
			assertContains(t, outcomes, "Manufacturing work in this state is scope creep and is **forbidden**")
			assertContains(t, outcomes, "do not re-run gates this pass had no reason to invoke")
			assertContains(t, outcomes, "do not re-read already-resolved threads")
			assertContains(t, outcomes, "do not make an unrelated improvement while you are here")

			// The parked no-op and residual outcomes both exit zero, and the residual defers to the
			// section that defines the model rather than restating it.
			assertContains(t, outcomes, "The parked thread or threads are the residual. Exit zero.")
			assertContains(t, outcomes, "Report it and exit zero, per")
			assertContains(t, outcomes, "[Residuals vs true stops](#residuals-vs-true-stops)")

			// These are outcome names mapped onto the EXISTING reason vocabulary, never new tokens.
			assertContains(t, outcomes, "**outcome names**, not new reason tokens")
			assertContains(t, outcomes, "the Watch Mode reason-line vocabulary is unchanged")
		})
	}
}

// TestBossRepairSkillTerminalOutcomesMintNoNewWatchTokens is the negative half of the terminal-outcome
// model. The final reason line's vocabulary is a CLOSED SET consumed by callers that classify it; a
// token outside the set degrades to a retry, which would send a case the skill just declared terminal
// back around the loop. Naming five new outcomes is exactly the edit that tempts an author to give
// each one its own token, so the watch window is pinned on the surviving vocabulary and asserted to
// contain none of the new names in the backticked form the reason line uses.
func TestBossRepairSkillTerminalOutcomesMintNoNewWatchTokens(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			watchMode := sectionBetween(t, skill, "## Watch Mode", "## Checklist")

			// The existing closed set is intact.
			assertContains(t, watchMode, "`green` / `parked` / `no-progress` / `max-attempts` / `blocked`")
			assertContains(t, watchMode, "`max-attempts` / `blocked`")

			// No outcome name became a reason token. Each is checked in the backticked spelling the
			// reason line uses, which is the only form a caller would parse.
			for _, minted := range []string{
				"`repaired`",
				"`nothing to repair`",
				"`nothing-to-repair`",
				"`parked no-op`",
				"`parked-no-op`",
				"`residual`",
				"`true stop`",
				"`true-stop`",
			} {
				assertNotContains(t, watchMode, minted)
			}
		})
	}
}

// TestBossRepairSkillPhase1AdmitsNoProblem pins the fourth categorization outcome. Phase 1.2's three
// categories all presuppose a problem exists, so a round that found none had nothing to select and no
// sanctioned way forward — the categorization step itself pushed toward inventing a problem.
//
// The window is the Phase 1 section narrowed to step 1.2 by its two bold sub-headings. sectionBetween
// cannot anchor on those directly (it requires markers beginning with `#`), so the narrowing is done
// here with an explicit ordering check: a window that silently spanned 1.1 and 1.3 would let the
// bullet be satisfied by prose under a step the categorizing worker never reads.
func TestBossRepairSkillPhase1AdmitsNoProblem(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			phase1 := sectionBetween(t, skill, "### Phase 1: Assess Current State", "### Phase 2: Execute Repair Strategy")
			step12 := boldStepWindow(t, phase1, "**1.2 Identify Problem Type**", "**1.3 Identify Project Gate Commands**")

			// The three pre-existing categories are still there — this is an addition, not a swap.
			assertContains(t, step12, "**Merge Conflict**")
			assertContains(t, step12, "**Failing Checks**")
			assertContains(t, step12, "**Review Feedback**")

			// The fourth category, its signal set, and the statement that it is a RESULT.
			assertContains(t, step12, "**No problem**")
			assertContains(t, step12, "every signal is already clear")
			assertContains(t, step12, "This is a **valid categorization result**, not a failure to categorize")
			assertContains(t, step12, "route straight to the **nothing to repair** outcome")
			assertContains(t, step12, "[Terminal outcomes](#terminal-outcomes)")
			assertContains(t, step12, "select no strategy")
		})
	}
}

// TestBossRepairSkillZeroDiffGatesRecordedNotRun pins the reporting rule for a pass that changed
// nothing. Both windows already PERMITTED skipping the gate/commit/push boxes on a zero-diff pass;
// neither said what to write in them instead. Permission to skip is not permission to fill in, and a
// quoted "gates passed" for a gate that was never invoked is worse than a blank — a reviewer and the
// next round both read it as evidence.
//
// Asserted in BOTH windows because the two state the same contract to two different readers (the
// completion checklist and the success definition), and a rule present in only one is one an editor
// working from the other will contradict.
func TestBossRepairSkillZeroDiffGatesRecordedNotRun(t *testing.T) {
	const (
		notRun = "A zero-diff pass records the gate boxes as **not run**."
		defect = "quoting a pass or fail status for a gate this pass never invoked is a **reporting defect**"
	)

	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			checklist := sectionBetween(t, skill, "## Checklist", "## Success Criteria")
			assertContains(t, checklist, notRun)
			assertContains(t, checklist, defect)
			assertContains(t, checklist, "Permission to skip a box is not permission to fill it in")

			success := sectionBetween(t, skill, "## Success Criteria", "### Post-terminal notes extensions (repo opt-in)")
			assertContains(t, success, notRun)
			assertContains(t, success, defect)
			assertContains(t, success, "Permission to skip a box is not permission to fill it in")
		})
	}
}

// TestBossRepairSkillStrategyCTriagesOnRemedy pins the axis change in Strategy C's triage. The old
// three categories graded a finding on its PREMISE alone and silently assumed a true premise implied
// its suggested change, which left three real dispositions homeless: a correct finding whose fix is
// out of the approved plan's scope, one whose suggestion sits in the wrong layer, and one whose
// remedy is feature-sized. A round meeting any of them had to either implement work it should not
// have, or decline a finding it knew was true.
//
// Category (b)'s file-and-line requirement is pinned separately because "already fixed" was the
// specific decline being settled against a commit subject line — which states intent and names its
// scope loosely, so it can close a thread over a live bug.
//
// Category (c)'s three obligations are each pinned: without the mandatory residual/follow-up record,
// (c) reads as a cheaper exit than fixing and becomes the escape hatch for an under-performing round.
func TestBossRepairSkillStrategyCTriagesOnRemedy(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			strategyC := sectionBetween(t, skill, "#### Strategy C: Review Feedback", "### Phase 3: Verify and Monitor")

			// Four categories, split on the remedy as well as the premise.
			assertContains(t, strategyC, "triage into one of four categories")
			assertContains(t, strategyC, "A true premise does **not** by itself license implementing the suggestion")
			assertNotContains(t, strategyC, "triage into one of three categories")

			// All four labels, in order, so a category cannot be dropped or renamed silently.
			labels := []string{
				"**a) Actionable — fix it:**",
				"**b) Premise does not hold — decline and resolve:**",
				"**c) Premise holds, remedy declined — affirm, record, resolve:**",
				"**d) Unclear — ask for clarification:**",
			}
			at := -1
			for _, label := range labels {
				assertContains(t, strategyC, label)
				next := strings.Index(strategyC, label)
				if next >= 0 && next < at {
					t.Errorf("category %q appears out of order", label)
				}
				if next >= 0 {
					at = next
				}
			}

			// (b): an "already fixed" decline is settled against the code, never a commit reference.
			assertContains(t, strategyC, "must cite the **file and line** in the current tree that satisfies the finding")
			assertContains(t, strategyC, "a commit reference alone cannot close a thread")

			// (c): the affirmation, the three shapes, and the mandatory record.
			assertContains(t, strategyC, "outside the approved plan's scope")
			assertContains(t, strategyC, "**wrong layer**")
			assertContains(t, strategyC, "**feature-sized**")
			assertContains(t, strategyC, "**affirm the defect is real**")
			assertContains(t, strategyC, "state precisely why the suggested change is not being applied")
			assertContains(t, strategyC, "**record a residual or follow-up instead of implementing it**")
			assertContains(t, strategyC, "it is not an escape hatch")

			// (d) keeps the behaviour the old (c) had, including its residual designation. Pinned so
			// the renumbering cannot quietly drop the clarification path's distinguishing rules.
			assertContains(t, strategyC, "Do NOT resolve the thread")
			assertContains(t, strategyC, "--disposition needs-human")
			assertContains(t, strategyC, "A thread left open this way is a **residual**")

			// The closing IMPORTANT paragraph must enumerate all four dispositions, not the old three.
			assertContains(t, strategyC, "affirm-and-record a declined remedy and resolve")
		})
	}
}

// TestBossRepairSkillStrategyCVerificationSteps pins the four cheap verifications as REQUIRED steps.
// Each costs a grep or two, and each prevented a wrong reply in an observed run: a multi-link claim
// answered wholesale, a documentation claim "fixed" in code that was already correct, a tunable
// constant invented from scratch beside a sibling that already set the shape, and a parked thread
// reported as a residual after the branch had already addressed it.
//
// The parked check is the load-bearing one: a park keys on the reviewer's last-comment identity, not
// on the head, so the branch can move underneath it indefinitely. Without a re-derivation the park is
// carried forward forever, and the un-park lever the probe already ships is never reached — so the
// exact `--disposition open` invocation is pinned rather than described.
func TestBossRepairSkillStrategyCVerificationSteps(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			strategyC := sectionBetween(t, skill, "#### Strategy C: Review Feedback", "### Phase 3: Verify and Monitor")

			// Required, not advisory — the framing the whole block turns on.
			assertContains(t, strategyC, "each is a required step, not a guideline")

			// Per-link verification of a multi-step causal claim, with the reply obligation.
			assertContains(t, strategyC, "**Verify each link of a multi-step causal claim separately.**")
			assertContains(t, strategyC, "check each link independently against the code")
			assertContains(t, strategyC, "The reply must state which links held")

			// Doc-claim check: adjacent code comment plus the NAME of the nearest test.
			assertContains(t, strategyC, "the **name** of the nearest test")
			assertContains(t, strategyC, "the fix is prose-only and **no code change is in scope**")

			// Sibling-constant lookup before designing a tunable-constant fix.
			assertContains(t, strategyC, "**Find the sibling constant before designing a tunable-constant fix.**")
			assertContains(t, strategyC, "grep the containing package for sibling constants")

			// Parked-premise re-derivation, its rationale, and the un-park lever.
			assertContains(t, strategyC, "A park keys on the **reviewer's last-comment identity**, not on the branch head")
			assertContains(t, strategyC, "**Never carry a prior pass's parked verdict forward.**")
			assertContains(t, strategyC, "**re-derive its premise against current HEAD**")
			assertContains(t, strategyC, "node scripts/review-feedback-probe.js mark --thread THREAD_ID --disposition open")
		})
	}
}

// TestBossRepairSkillWatchModeHeadScopedClean pins the head-scoping of a clean review probe. A clean
// probe at the end of a round predicts nothing about the next one: it was read against one head, and
// a reviewer who re-reviews every push opens fresh threads against the round's own fix. Without the
// scoping stated, a round reads its own clean probe as a steady state reached and treats the next
// round's threads as a regression — or, worse, as licence to batch them.
//
// The two-terminator clause is the other half. A round that replied and parked without pushing looks
// identical to step 9's no-progress stop (no new commit) while being its opposite: substantive work
// was done. Conflating them either spins the loop on a genuinely finished PR or reports a working
// round as unfixable.
func TestBossRepairSkillWatchModeHeadScopedClean(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			watchMode := sectionBetween(t, skill, "## Watch Mode", "## Checklist")

			assertContains(t, watchMode, "`repair_status=clean` is **head-scoped**")
			assertContains(t, watchMode, "it is clean only for the head it was read against")
			assertContains(t, watchMode, "**new threads since the previous round's head are an expected steady state**")
			assertContains(t, watchMode, "not a regression, and not a fresh failure")

			// Not licence to batch or defer.
			assertContains(t, watchMode, "**not licence to batch, defer, or wait for a quiet reviewer**")
			assertContains(t, watchMode, "What is bounded is the number of repair **rounds**, never the reviewer's cadence")

			// The re-poll ordering, stated as an every-round obligation.
			assertContains(t, watchMode, "**review re-poll runs after the CI-green check in every round**")
			assertContains(t, watchMode, "never skipped once checks pass")

			// Two distinct terminators, and the parked one explicitly separated from step 9.
			assertContains(t, watchMode, "there are **two distinct terminators**")
			assertContains(t, watchMode, "**parked without pushing**")
			assertContains(t, watchMode, "it is **not** the no-progress stop of step 9")
		})
	}
}

// TestBossRepairSkillWatchModeBoundResidualNamesPendingWork pins what the bounded loop must SAY when
// it stops. "Record what is still red" was the whole instruction, and it covers neither of the two
// things a bounded watch actually leaves behind: a pending check is not red, and review feedback that
// landed after the final push is not red either — so both were routinely omitted and the caller had
// to re-derive them.
//
// The flake clause is separate and easily lost: a round that ends because a failing check was
// re-rolled green has learned exactly where the fragility is, and dropping that knowledge makes the
// next occurrence pay the full triage cost from zero. It is pinned with the "or record that none"
// half, so an honest empty answer stays available and the rule does not push toward invention.
func TestBossRepairSkillWatchModeBoundResidualNamesPendingWork(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			watchMode := sectionBetween(t, skill, "## Watch Mode", "## Checklist")

			// The residual names both things "what is still red" misses, and says why.
			assertContains(t, watchMode, "must name **pending CI checks** and **review feedback that arrived after the final push**")
			assertContains(t, watchMode, "a pending check is not red, and a fresh unaddressed thread is not red either")

			// The reason line carries the budget actually spent and the pending state.
			assertContains(t, watchMode, "the **number of repair passes used** and the **pending-check state**")
			assertContains(t, watchMode, "so an exhausted budget is visible without re-deriving it")

			// A re-rolled flake leaves hardening candidates behind, or an explicit none.
			assertContains(t, watchMode, "**re-rolled and came back green**")
			assertContains(t, watchMode, "must name the concrete `file:line` hardening candidates")
			assertContains(t, watchMode, "or explicitly record that none was identified")
		})
	}
}

// TestBossRepairSkillPhase2AcceptsCronDispatchGrant pins the missing half of the inline-routing rule.
// The lead-in already said `BOSS_CRON=true` alone is not a prohibition; it never said what DOES clear
// the "unless the user requested it" condition a blanket no-subagent instruction is written against.
// So a managed run carrying an explicit operator grant still read the blanket instruction as binding
// and took the weaker single-voice route for every strategy.
//
// The negative list is re-asserted here rather than left to TestBossRepairSkillPhase2InlineBranchContract:
// this change adds prose to precisely that window, and the phrasing most likely to creep in while
// writing about a grant is a restatement of the prohibition it replaces.
func TestBossRepairSkillPhase2AcceptsCronDispatchGrant(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			phase2 := sectionBetween(t, skill, "### Phase 2: Execute Repair Strategy", "#### Strategy A: Merge Conflicts")

			assertContains(t, phase2, "A managed or cron **dispatch grant**")
			assertContains(t, phase2, "the operator's standing request for the protocol-mandated dispatches")
			assertContains(t, phase2, "**satisfies** the \"unless the user requested it\" condition")
			assertContains(t, phase2, "not an inline-routing trigger in such a session")

			lowered := strings.ToLower(phase2)
			for _, forbidden := range []string{
				"may not dispatch",
				"must not dispatch",
				"cannot dispatch",
				"forbidden from dispatching",
				"forbidden to dispatch",
				"cron runs are forbidden",
			} {
				assertNotContains(t, lowered, forbidden)
			}
		})
	}
}

// TestBossRepairSkillGateRedIsNotAutomaticallyAFinding pins the portable reading rule for a red gate.
// Both strategies that run repo gates could read an exit code as a defect in the branch, and an
// infrastructure flake — lock contention, a commit-hook signing or memory failure, a failure in a
// file this change never touched — then gets "fixed" in code that was never wrong.
//
// The rule deliberately names NO tool-specific flake string. This core extracts into every user's
// global skill directory, where a signature borrowed from one toolchain is dead weight at best and
// actively misleading at worst; the reasoning error it prevents is portable, the strings are not. The
// absence is asserted, not merely intended, because pasting a worked example verbatim is exactly how
// such a string arrives.
func TestBossRepairSkillGateRedIsNotAutomaticallyAFinding(t *testing.T) {
	const (
		rule    = "**An exit code is not a finding.**"
		read    = "read the failing output"
		flakes  = "are infrastructure flakes, not findings"
		confirm = "Re-run the affected target in isolation to confirm"
	)

	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			// Reachable from BOTH gate-running strategies: a worker in Strategy B never reads
			// Strategy C's step 3, and vice versa.
			strategyB := sectionBetween(t, skill, "#### Strategy B: Failing Checks", "#### Strategy C: Review Feedback")
			assertContains(t, strategyB, rule)
			assertContains(t, strategyB, read)
			assertContains(t, strategyB, flakes)
			assertContains(t, strategyB, confirm)

			strategyC := sectionBetween(t, skill, "#### Strategy C: Review Feedback", "### Phase 3: Verify and Monitor")
			assertContains(t, strategyC, rule)
			assertContains(t, strategyC, read)
			assertContains(t, strategyC, flakes)
			assertContains(t, strategyC, confirm)

			// It points at the repo's own instructions for the signatures, rather than carrying any.
			assertContains(t, strategyB, "consult the repo's own agent instructions for the flake signatures it records")

			// No tool-specific flake signature anywhere in the published core.
			lowered := strings.ToLower(skill)
			for _, toolSpecific := range []string{
				"golangci",
				"parallel golangci-lint is running",
				"gpg",
				"secret key not available",
			} {
				assertNotContains(t, lowered, toolSpecific)
			}
		})
	}
}

// boldStepWindow narrows an already-anchored section to one bold-labelled step. sectionBetween cannot
// do this itself — it requires both markers to be markdown `#` headings — but a gate on a numbered
// sub-step still needs a window, or the assertion can be satisfied by prose under a neighbouring step
// the reader of this one never sees. Both labels must be present and in order.
func boldStepWindow(t *testing.T, section, start, end string) string {
	t.Helper()

	begin := strings.Index(section, start)
	if begin == -1 {
		t.Fatalf("step label %q not found in section", start)
	}
	rest := section[begin:]
	stop := strings.Index(rest, end)
	if stop == -1 {
		t.Fatalf("step label %q not found after %q", end, start)
	}
	return rest[:stop]
}

// bossRepairSkillPayloads returns the boss-repair SKILL.md body from BOTH shipped payloads — the
// copy embedded in services/boss and the bossd-plugin-claude mirror — keyed by the name that
// identifies the tree in a subtest failure. A prose contract asserted against only the embedded copy
// goes green on a mirror `make copy-skills` has not refreshed, and the mirror is what the plugin
// actually installs.
func bossRepairSkillPayloads(t *testing.T) map[string]string {
	t.Helper()

	mirrorRoot := filepath.Join(findRepoRoot(t), "plugins", "bossd-plugin-claude", "skilldata", "skills", "boss-repair")
	mirrorBytes, err := fs.ReadFile(os.DirFS(mirrorRoot), "SKILL.md")
	if err != nil {
		t.Fatalf("read bossd-plugin-claude boss-repair SKILL.md under %s: %v", mirrorRoot, err)
	}

	return map[string]string{
		"embedded": readEmbeddedBossRepairSkill(t),
		"mirror":   string(mirrorBytes),
	}
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

// BOS-798: the two rules that bound the blast radius of a review finding quoting skill prose,
// plus the markdown-eyeball rule that governs the hunk such a fix produces.
//
// Both are here rather than in a reference because Strategy C's "required verifications before you
// reply" list IS the run's pre-reply checklist, and a rule a run reads after replying is a rule it
// did not use. They are pinned as whitespace-tolerant falsification pins for the reason the pin
// shape exists at all: these bullets sit in a list that reflows whenever a neighbouring bullet is
// edited, so a literal-space pattern would red on a rewrap while naming no defect — and, worse, a
// literal-space MUTATION during falsification matches nothing, exits 0, and produces a green run
// byte-identical to a genuinely vacuous pin.
var bossRepairProseBlastRadiusPins = regProsePins([]falsificationProsePin{
	{
		name:         "prose-finding-grep-upstream",
		pattern:      `Grep\s+upstream\s+before\s+treating\s+a\s+skill-prose\s+finding\s+as\s+single-site`,
		live:         "Grep upstream before treating a skill-prose finding as single-site",
		tokenRemoved: "Grep before treating a skill-prose finding as single-site",
	},
	{
		name:         "prose-finding-grep-scope",
		pattern:      `across\s+the\s+whole\s+skills\s+tree\s+\*\*and\*\*\s+the\s+contract\s+docs\s+those\s+skills\s+are\s+copied\s+from`,
		live:         "across the whole skills tree **and** the contract docs those skills are copied from",
		tokenRemoved: "across the whole skills tree",
	},
	{
		name:         "prose-finding-fix-upstream",
		pattern:      `fix\s+the\s+upstream\s+contract\s+passage\s+in\s+the\s+same\s+commit`,
		live:         "fix the upstream contract passage in the same commit",
		tokenRemoved: "fix the cited line in the same commit",
	},
	{
		name:         "prose-finding-sweep-rationale",
		pattern:      `Sweep\s+the\s+rationale,\s+not\s+only\s+the\s+restatements`,
		live:         "Sweep the rationale, not only the restatements",
		tokenRemoved: "Sweep the restatements",
	},
	{
		name:         "prose-finding-old-rule-as-reason",
		pattern:      `cite\s+the\s+\*\*old\*\*\s+rule\s+as\s+their\s+\*\*reason\*\*`,
		live:         "cite the **old** rule as their **reason**",
		tokenRemoved: "cite the old rule as their reason",
	},
	{
		name:         "prose-finding-rationale-not-greppable",
		pattern:      `A\s+restatement\s+is\s+greppable\s+and\s+a\s+rationale\s+is\s+not`,
		live:         "A restatement is greppable and a rationale is not",
		tokenRemoved: "A restatement is greppable",
	},
	// The markdown-eyeball rule. Strategy C implements, gates, and commits in one pass, so the
	// last reader of a markdown hunk before it lands is this skill — which is why the obligation
	// is stated here and not only in the reviewing core.
	{
		name:         "markdown-hunk-read-before-commit",
		pattern:      `read\s+the\s+rendered\s+hunk\s+before\s+you\s+commit`,
		live:         "read the rendered hunk before you commit",
		tokenRemoved: "read the rendered hunk",
	},
	{
		name:         "markdown-hunk-covers-delegated-edits",
		pattern:      `delegating\s+the\s+edit\s+does\s+not\s+delegate\s+this`,
		live:         "delegating the edit does not delegate this",
		tokenRemoved: "delegating the edit",
	},
	{
		name:         "markdown-hunk-formatter-is-not-the-check",
		pattern:      "`--check`\\s+reports\\s+as\\s+correctly\\s+formatted",
		live:         "`--check` reports as correctly formatted",
		tokenRemoved: "`--check` reports it",
	},
	// The hedge matters in a published core: a consumer repo may set proseWrap to "always", where
	// the described failure mode does not arise. Both halves of the hedge are load-bearing, so both
	// are falsified: the first pin below kills the word "default", the second kills the identity of
	// the config value it hedges. The pattern names that value directly rather than stepping over it
	// with a wildcard — a bounded `.{0,30}?` matched any replacement span, so the sentence could
	// drift to a prettier option that has nothing to do with reflow and stay green. `\s*` inside the
	// literal is the widest the no-literal-space rule allows; it cannot be a literal space, and
	// `\s+` there would additionally match a line break prettier never writes inside a code span.
	{
		name:         "markdown-hunk-prettier-default-is-hedged",
		pattern:      "Prettier's\\s+default\\s+`?proseWrap:\\s*preserve`?\\s+does\\s+not\\s+reflow\\s+prose",
		live:         "Prettier's default `proseWrap: preserve` does not reflow prose",
		tokenRemoved: "Prettier's `proseWrap: preserve` does not reflow prose",
	},
	{
		name:         "markdown-hunk-prettier-names-the-preserve-default",
		pattern:      "Prettier's\\s+default\\s+`?proseWrap:\\s*preserve`?\\s+does\\s+not\\s+reflow\\s+prose",
		live:         "Prettier's default `proseWrap: preserve` does not reflow prose",
		tokenRemoved: "Prettier's default `proseWrap: always` does not reflow prose",
	},
	// boss-review states this half of the rule and pins it; boss-repair carried only the symptom
	// ("an edited table cell can re-pad every row around it") with no prescribed action. Two derived
	// copies of one rule that have already diverged on their operational half is the exact defect
	// the two rules above exist to prevent, so the remedy is restated here and pinned alongside.
	{
		name:         "markdown-hunk-table-cell-remedy",
		pattern:      `run\s+the\s+formatter\s+immediately\s+after\s+editing\s+a\s+markdown\s+table\s+cell\s+and\s+confirm\s+the\s+churn\s+is\s+padding-only`,
		live:         "run the formatter immediately after editing a markdown table cell and confirm the churn is padding-only",
		tokenRemoved: "run the formatter after editing a markdown table cell",
	},
})

// bossRepairOmissionHypothesisPins pins the triage rule that stops a repair trusting a comment
// which justifies an omission. Its whole value is the judgement it demands, so the pin sits on
// "actually forced": a rule that keeps its title but loses that token collapses into "trust the
// comment" -- the exact over-conservative omission it exists to re-open.
var bossRepairOmissionHypothesisPins = regProsePins([]falsificationProsePin{
	{
		name:         "omission-comment-is-a-hypothesis",
		pattern:      `Check\s+whether\s+the\s+trade-off\s+is\s+actually\s+forced\s+before\s+trusting\s+it`,
		live:         "Check whether the trade-off is actually forced before trusting it",
		tokenRemoved: "Check whether the comment is accurate before trusting it",
	},
})

// bossRepairDocClaimReadOrderPins pins the read-cheapest-first half of the flagged-documentation-claim
// verification. It deliberately lives on the REQUIRED verification in Strategy C rather than in the
// Guidelines list: a Guidelines-tier copy would restate a step the skill explicitly calls "a required
// step, not a guideline", and would license concluding "doc-only" from the comment alone when the
// required form also demands the nearest test agree. The pin sits on "the package doc comment"
// because that is the only read this rule adds; without it the sentence still reads as guidance
// while licensing the multi-link implementation chain it exists to avoid.
var bossRepairDocClaimReadOrderPins = regProsePins([]falsificationProsePin{
	{
		name:         "read-adjacent-comment-first",
		pattern:      `beside\s+the\s+implementation\s+—\s+and\s+the\s+package\s+doc\s+comment\s+—`,
		live:         "beside the implementation — and the package doc comment — and the **name**",
		tokenRemoved: "beside the implementation and the **name**",
	},
})

// TestBossRepairSkillClaimTriageGuidelines asserts the omission-hypothesis rule against the real
// payload, in the Guidelines window rather than the whole document: it is a triage rule, and a
// triage rule that drifts out of the list a repair pass actually reads has stopped bounding
// anything while still grepping green.
func TestBossRepairSkillClaimTriageGuidelines(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			guidelines := sectionBetween(t, skill, "## Guidelines", "## Anti-Patterns")
			assertFalsificationPins(t, guidelines, bossRepairOmissionHypothesisPins)
		})
	}
}

// TestBossRepairSkillProseFindingBlastRadius pins the two scope-of-fix rules for a review finding
// that quotes skill prose, the markdown-eyeball rule that governs the hunk the fix produces, and the
// read-order rule that bounds the verification budget before any of them. A quoted line is a
// symptom: the same remedy is copied into every skill derived from the contract doc it came from, so
// a fix applied only where the reviewer pointed leaves that contract re-seeding the identical prose
// into the next copy. The second rule covers the half no grep hands you — passages that cite the OLD
// rule as their reason rather than restating it, which is how a round ships a contradiction one
// paragraph from the sentence it just corrected.
func TestBossRepairSkillProseFindingBlastRadius(t *testing.T) {
	for name, skill := range bossRepairSkillPayloads(t) {
		t.Run(name, func(t *testing.T) {
			strategyC := sectionBetween(t, skill, "#### Strategy C: Review Feedback", "### Phase 3: Verify and Monitor")
			assertFalsificationPins(t, strategyC, bossRepairProseBlastRadiusPins)
			assertFalsificationPins(t, strategyC, bossRepairDocClaimReadOrderPins)
		})
	}
}
