# Publishing the PR (Steps 7–9)

Everything a reader of the PR or the ticket needs to see, per route. Every route runs the same
pipeline — push, PR, tag, green — and differs only in what it publishes at the end.

## PR body

Write it to a temp file **outside** the worktree, then `gh pr create --body-file` / `gh pr edit
--body-file`. To change a published body (a resume, a repair, one corrected line), write only the
changed sections to a file and merge them into the live body (`gh pr view --json body -q .body`):
`node "$BOSS_BUILD_TOOLBOX/build/pr-body.mjs" merge --body-file <live> --update-file <sections> --out <path>`.
It replaces each section in place, so reruns converge on one copy. An optional section that is now
empty (`## Human follow-up`, `## After merge`, `## Host notes`, `## Unapplied review findings`) gets
`--drop '<heading>'`, or the merge keeps its stale published copy. A warning that a heading survives in several copies means the
update must render that section too. Never patch a body with
`String#replace` or `sed`: their replacement patterns (`$'`, `&`) expand, and `-run '^X$'`
contains one.

```
Tracker issue: <url>

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
- [ ] <anything a human must do or verify outside the worktree — optional, omitted when empty>

## After merge
- [ ] <an observation only possible once merged — optional, non-gating, omitted when empty>

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

- The first line must be `Tracker issue: <url>` — downstream review keys off it.
- Every section is for people reading the PR. The only text a merge gate reads is the review
  sections and the PARTIAL marker below.
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
  rebase and no-CI runs. Each bullet is an optional phase label ending with `:` (backtick spans
  allowed), one backticked command, an optional non-empty `(qualifier)`, `→` or `->`, and a
  non-empty result, or `gap: <explanation>`. Labels cannot contain arrows outside backtick spans;
  qualifiers cannot contain parentheses. Put any other prose after the result. For example:
  ``mutation (`>=` became `>` in `isEligible`): `go test ./pkg` → FAIL, as intended``.
  `validateLocalVerification(config, body)` (`toolbox/skill-config.mjs`) owns this grammar; fix
  findings in the body and validate again. A `gap:` entry is honest output, not a failure, and
  validator findings never change the route or yield BLOCKED.
- Take `## Human follow-up`, `## After merge`, `## Host notes` and `## Review follow-ups` from the
  `sections` that `followup-route.mjs route` prints (SKILL.md Step 7.3); never hand-write their
  items.

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
  (tick only what the diff and the review both support), one `file:line` reason per open box (a retried criterion carries the acceptance retry helper's `reason`), and a
  `## Partial` section whose first line is exactly:

  ```
  do not merge — partial: <satisfied>/<total> acceptance criteria
  ```

- Ticket comment: the count, the checklist, the reasons, the marker and the PR URL.
- PR readied (non-draft), and the ticket **stays in `.inProgress`**.
  verify parks a PARTIAL PR as `partial-build`, and boss-epic holds it by the ticket state and the marker.

## BLOCKED

- PR stays (or is put back to) draft; ticket stays in `.inProgress`.
- Blocker comment on the ticket: which cause (red gates / unpushable / review not clean), the
  failing check or finding at `file:line`, what was tried, and where the work is (`push-branch.mjs`'s
  `refused` reason and `pushed`: the session branch, the `rescue` ref it names, or — only when both failed — the unpushed
  SHAs). Include both coverage tokens under their own headings, and the base-drift note when there is
  one; there may be no PR body to carry them.
- If this route readied the PR earlier in the run (a red reading after readying), unwind first:
  restore the plain title and body, `gh pr ready --undo`.

## Tags

Commits carry `[#<PR>]`, injected by `finalize/cli.mjs inject-pr-tag` once the PR exists (Step 8),
then force-pushed with lease while the PR is still a draft. Report an injection that left commits
untagged (the injector lists them and why); it is a traceability gap, not a red check.
