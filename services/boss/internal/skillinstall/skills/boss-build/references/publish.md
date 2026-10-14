# Publishing the PR (Steps 7–9)

Everything a reader of the PR or the ticket needs to see, per route. Every route runs the same
pipeline — push, PR, tag, green — and differs only in what it publishes at the end.

## PR body

Write it to a temp file **outside** the worktree, then `gh pr create --body-file` / `gh pr edit
--body-file`. On a resume, regenerate it from the current done-vs-remaining map and replace sections
rather than appending.

```
Linear issue: <url>

## Premise discharge
- <the plan's central premise and the evidence it still holds — or the departure: what the premise
  was, the evidence against it, and what the build did instead>

## Acceptance criteria
- [x] <criterion the diff and tests demonstrate>
- [x] (verify-only) <criterion no diff can show> — checked: `<command>` → <result>
- [ ] <criterion still open> — <what is missing, at file:line>

## Local verification
- `<local command>` → <the command’s final summary line>
- after rebase: `<covering command>` → <final summary line>
- gap: <what could not be selected reliably and why>

## Autonomous decisions
- <decision + rationale> (every task contract's decisions, the orchestrator's own, the base-drift
  note)

## Human follow-up
- [ ] <anything a human must verify outside the worktree>
- none

## Open questions
- [ ] <unresolved plan question>
- none

## Host notes
- <build-host or environment problem — optional, non-gating, omitted when empty>

## Unapplied review findings
- [ ] <verified, non-blocking finding — omitted when there are none>

## Cross-model review
<ran (<agent>) | skipped: <reason> | error: <reason>>

## Review coverage
<full | none: review did not run (<reason>) | none: review verdict unusable (<reason>) |
 none: review coverage unknown (<reason>)>
```

- The first line must be `Linear issue: <url>` — downstream review keys off it.
- **Never omit** `## Cross-model review` or `## Review coverage`: an absent section reads as
  "passed clean" / "full coverage". Publish the token the run actually earned, never a cleaner one.
- `## Review findings`, `## Unapplied review findings`, `## Cross-model review` and
  `## Review coverage` are exactly what `review-verdict.mjs render <verdict> --pr-section` prints for
  the run's pooled verdict — never hand-written. `## Review findings` appears only when something
  blocks.
- A `(verify-only)` criterion is discharged by recording the check you actually ran, with the command
  **in backticks**. `validateVerifyOnlyEvidence(config, body)` (`toolbox/skill-config.mjs`) checks
  the shape before the first publish and again before readying; each `missingEvidence` item names a
  `reason` and a `remedy`. A failing item is an unmet in-scope criterion, not a blocker. You may
  reclassify a criterion as verify-only yourself when "no change was needed" is genuinely the right
  outcome — run the check, record it, and note the reclassification under `## Autonomous decisions`.
- `## Local verification` records every local command and its final summary, including repair,
  rebase and no-CI runs. Each bullet is an optional phase label followed by a backticked command,
  `→` or `->`, and a non-empty result, or `gap: <explanation>`.
  `validateLocalVerification(config, body)` (`toolbox/skill-config.mjs`) owns this grammar; fix
  findings in the body and validate again. A `gap:` entry is honest output, not a failure, and
  validator findings never change the route or yield BLOCKED.
- Always include `## Human follow-up` and `## Open questions`. Each contains only checklist
  items (`- [ ]` open, `- [x]` done), or a single `- none` when empty; in the template choose
  items or `- none`, never both. Heading matching is case-insensitive and fenced blocks are ignored.
  Any other non-blank line makes the section malformed. Run
  `node "$BOSS_BUILD_TOOLBOX/completion-gate.mjs" followups --body-file <path>` beside the
  evidence validator and fix its reported problems before publishing; a malformed section is
  recoverable, not a blocker.
- Take `## Human follow-up`, `## Open questions` and `## Host notes` from the `sections` that
  `followup-route.mjs route` prints (SKILL.md Step 7.3); never hand-write their items.
- The phrase `do not merge` never appears in a title or body except as the PARTIAL marker below —
  boss-epic's merge gate matches it. Rephrase any finding that contains it.

## The review comment

Exactly one `<!-- bs-review -->` comment per PR, upserted in place:

```bash
BS_REVIEW_BODY="$(mktemp)"   # Step 6's review-report.md, or the disabled note under BOSS_BS_REVIEW=0; both lead with <!-- bs-review -->
ME="$(gh api user --jq '.login' 2>/dev/null || true)"
CID=$(gh pr view "$PR_NUMBER" --json comments \
  | jq -r --arg me "$ME" '[.comments[]
      | select(.body | startswith("<!-- bs-review -->"))
      | select($me == "" or (.author.login // "") == $me)
      | .url][-1] // ""')
if [ -n "$CID" ]; then
  gh api -X PATCH "repos/{owner}/{repo}/issues/comments/${CID##*-}" -F body=@"$BS_REVIEW_BODY"
else
  gh pr comment "$PR_NUMBER" --body-file "$BS_REVIEW_BODY"
fi
rm -f "$BS_REVIEW_BODY"
```

The body is `$(git rev-parse --git-dir)/boss-build/review-report.md` (`review-verdict.mjs render
--report`), which also reports a missing or unusable verdict honestly.

## BLOCKED: review not clean (cause 3)

`ready-gate.mjs` was not ready on the pushed head. The work is pushed and published
as a draft so a human or a later run can pick it up; nothing marks it finished:

- the body's `## Review findings` (from `render --pr-section`) lists every blocking finding at
  `file:line`, followed by the gate's `reasons`; for `tree-changed-since-review` or
  `tree-drift-unknown`, name the reviewed head and the head that shipped;

- the `<!-- bs-review -->` comment is still upserted (the report, which states an unusable verdict too);
- the ticket's blocker comment carries cause 3, the gate's reasons, the same blocking findings,
  the PR URL and both coverage tokens;
- PR stays (or goes back to) draft, ticket stays `.inProgress`, no `boss/build` receipt (the helper
  refuses it anyway). The title gets no suffix.

## PARTIAL

Only when all three hold: **T1** at least one in-scope criterion satisfied by the diff and tests,
on a branch whose review produced a verdict (an `error` verdict or `BOSS_BS_REVIEW=0` supports
nothing, and `0/<total>` is never PARTIAL); **T2** the branch is green (a settled CI reading on the
readied PR); **T3** everything left undone is an unmet in-scope criterion (no blocking finding
outside those criteria, no unattributed residue, the reviewed tip shipped). On the PARTIAL route (Step 9 persists it) `ready-gate.mjs` admits a capped review; T3 is
what makes that admission honest. A failed T1/T3 on a green branch is REVIEW_READY
with the items published only when the gate is `ready`, else BLOCKED cause 3; a failed T2 is
BLOCKED.

- Title: `[<ISSUE-ID>] <issue title> (partial <satisfied>/<total>)`.
- Body: the normal body plus `Partial: <satisfied>/<total> acceptance criteria`, the full checklist
  (tick only what the diff and the review both support), one `file:line` reason per open box, and a
  `## Partial` section whose first line is exactly:

  ```
  do not merge — partial: <satisfied>/<total> acceptance criteria
  ```

- Ticket comment: the count, the checklist, the reasons, the marker and the PR URL.
- PR readied (non-draft), and the ticket **stays in `.inProgress`**.
  boss-epic holds a PARTIAL PR twice: by the ticket state and by the marker in the title/body.

## BLOCKED

- PR stays (or is put back to) draft; ticket stays in `.inProgress`.
- Blocker comment on the ticket: which cause (red gates / unpushable / review not clean), the
  failing check or finding at `file:line`, what was tried, and where the work is (`push-branch.mjs`'s
  `pushed`: the session branch, the `rescue` ref it names, or — only when both failed — the unpushed
  SHAs). Include both coverage tokens under their own headings, and the base-drift note when there is
  one; there may be no PR body to carry them.
- If this route readied the PR earlier in the run (a red reading after readying), unwind first:
  restore the plain title and body, `gh pr ready --undo`.

## Tags

Commits carry `[#<PR>]`, injected by `finalize/cli.mjs inject-pr-tag` once the PR exists (Step 8),
then force-pushed with lease while the PR is still a draft. Report an injection that left commits
untagged (the injector lists them and why); it is a traceability gap, not a red check.
