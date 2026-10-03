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

## Autonomous decisions
- <decision + rationale> (every task contract's decisions, the orchestrator's own, the base-drift
  note, and anything a human must verify outside the worktree)

## Cross-model review
<clean | findings-fixed (<per-finding dispositions>) | skipped: <reason> | error: <reason>>

## Review coverage
<full | full (skipped: <rounds>) | quick: <reason> | none: review stack did not run (<reason>) |
 none: review verdict unreadable (<reason>) | none: review coverage unknown (<reason>)>
```

- The first line must be `Linear issue: <url>` — downstream review keys off it.
- **Never omit** `## Cross-model review` or `## Review coverage`: an absent section reads as
  "passed clean" / "full coverage". Publish the token the run actually earned, never a cleaner one.
- `quick: <reason>` states which part of the depth rule picked it (e.g.
  `quick: no lens glob matched and 4 changed files is below the 20-file threshold`), so a reader can
  re-check it.
- A `(verify-only)` criterion is discharged by recording the check you actually ran, with the command
  **in backticks**. `validateVerifyOnlyEvidence(config, body)` (`toolbox/skill-config.mjs`) checks
  the shape before the first publish and again before readying; each `missingEvidence` item names a
  `reason` and a `remedy`. A failing item is an unmet in-scope criterion, not a blocker. You may
  reclassify a criterion as verify-only yourself when "no change was needed" is genuinely the right
  outcome — run the check, record it, and note the reclassification under `## Autonomous decisions`.
- Copy the plan's open questions (for `agent-question` tickets) into the body.
- The phrase `do not merge` never appears in a title or body except as the PARTIAL marker below —
  boss-epic's merge gate matches it. Rephrase any finding that contains it.

## The review comment

Exactly one `<!-- bs-review -->` comment per PR, upserted in place:

```bash
BS_REVIEW_BODY="$(mktemp)"   # the review report, or an honest fallback note — both lead with <!-- bs-review -->
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

The fallback note (no report exists) says what ran, why there is no verdict, and points at the two
coverage sections.

## REVIEW_READY with open findings

A capped, provisional or unreadable review on a pushed, green branch ships `REVIEW_READY`:

- the body adds a `## Review findings` section — count and severity split, pointing at the comment;
- the `<!-- bs-review -->` comment carries every open must-fix at `file:line` with its lens,
  severity and disposition (open, attempted-and-unverified, ineligible);
- a tracker comment carries the same summary plus the PR URL;
- `please-review` applied, PR readied, ticket moved to `.inReview`. The title gets no suffix.

## PARTIAL

Only when all three hold: **T1** at least one in-scope criterion satisfied and certified by a review
that really ran (the acceptance-criteria certification ran over the full list and raised no must-fix
against that criterion — a seed verdict or `BOSS_BS_REVIEW=0` certifies nothing, and `0/<total>` is
never PARTIAL); **T2** the branch is green (a settled CI reading on the readied PR); **T3** everything
left undone is an unmet in-scope criterion (no other open must-fix, no unattributed residue, the
reviewed tip shipped). A failed T1/T3 on a green branch is REVIEW_READY with
the items published; a failed T2 is BLOCKED.

- Title: `[<ISSUE-ID>] <issue title> (partial <satisfied>/<total>)`.
- Body: the normal body plus `Partial: <satisfied>/<total> acceptance criteria`, the full checklist
  (tick only what the diff and the review both support), one `file:line` reason per open box, and a
  `## Partial` section whose first line is exactly:

  ```
  do not merge — partial: <satisfied>/<total> acceptance criteria
  ```

- Ticket comment: the count, the checklist, the reasons, the marker and the PR URL.
- PR readied (non-draft) but **no** `please-review`, and the ticket **stays in `.inProgress`**.
  boss-epic holds a PARTIAL PR twice: by the ticket state and by the marker in the title/body.

## BLOCKED

- PR stays (or is put back to) draft; ticket stays in `.inProgress`; `please-review` removed.
- Blocker comment on the ticket: which cause (red gates / unpushable), the failing
  check or finding at `file:line`, what was tried, and where the work is (`push-branch.mjs`'s
  `pushed`: the session branch, the `rescue` ref it names, or — only when both failed — the unpushed
  SHAs). Include both coverage tokens under their own headings, and the base-drift note when there is
  one; there may be no PR body to carry them.
- If this route readied the PR earlier in the run (a red reading after readying), unwind first:
  restore the plain title and body, `gh pr ready --undo`, remove `please-review`.

## Tags

Commits carry `[#<PR>]`, injected by `finalize/cli.mjs inject-pr-tag` once the PR exists (Step 8),
then force-pushed with lease while the PR is still a draft. Report an injection that left commits
untagged (the injector lists them and why); it is a traceability gap, not a red check.
