# Step 4.5 — Assess adopted work (resume only)

Read this when **Step 2.5** marked the branch a **resume** (real work already committed on the branch,
whether by a prior bossd-managed or standalone run). The point is to know what a prior run implemented
before touching anything.

```bash
git diff --stat "$BASE_BRANCH"...HEAD
git log --oneline "$BASE_BRANCH..HEAD"
gh pr view "$PR_NUMBER" --json body -q .body
```

Build a done-vs-remaining map against the plan's acceptance criteria, cross-checking the PR-body
`- [x]/- [ ]` checklist against the actual diff (trust the diff over a stale checkbox). Then set the
implement scope for Step 5:

- **All acceptance criteria satisfied** → scope is _none_; skip Step 5 and go straight to the green
  tail (Step 6 review of the adopted diff → Step 7 reuse → Step 8 → Step 9).
- **Partially done** → scope is the _remaining_ criteria only.
- **Only the bootstrap commit / nothing real** → treat as fresh, full plan.

Adoption never reverts or force-pushes the prior work; you build on top of it. On a resume the Step 6
review reviews the **whole** branch; pass its reviewer this map as prior-disposition context so it does
not block already-shipped work.

## Continue from committed state

Implementation subagents commit per task (the Step 5 commit-before-return contract), so an
interruption — a subagent that died mid-flight, a transient host error, a restarted run — usually
leaves the completed tasks already on the branch. On **any** resume or re-dispatch after an
interruption, inventory that committed state before dispatching anything:

```bash
git log --oneline "$BASE_BRANCH..HEAD"
# residue from a subagent that died before committing — scoped exactly like the Step 5 check,
# so the plan deliverable and host artifacts don't read as residue. Exclude the single
# "$PLAN_DOC" Step 4 copied, never the whole docs/plans directory: a directory-wide exclusion
# would also hide a stray edit to another plan doc, which IS residue. Re-set PLAN_DOC in the
# same invocation — `:?` aborts rather than letting an unset variable become a bare
# `:(exclude)`, which excludes everything and reports a clean tree that isn't.
PLAN_DOC="docs/plans/<the file Step 4 saved>"
git status --porcelain --untracked-files=all -- . \
  ":(exclude)${PLAN_DOC:?PLAN_DOC unset — re-read it from the run notes}" \
  ':(exclude).claude/scheduled_tasks.lock' ':(exclude).claude/settings.local.json'
```

Map those commits onto the plan's task list, one row per task: **committed** (a commit exists whose
scope matches the task and whose diff satisfies the task's criteria — trust the diff, not the
subject) or **remaining**. Then:

- Dispatch **only** the remaining tasks. Carry the standing instruction _continue from committed
  state; do not redo committed tasks_ into every re-dispatched subagent, along with the list of
  tasks already committed, so it builds on top instead of re-implementing them.
- If that scoped `git status` is non-empty, the interrupted subagent **may** have died with work in
  the tree. Which recovery applies turns on Step 5's snapshot, exactly as it does there —
  `"$(git rev-parse --git-dir)/boss-build-pre-dispatch-head"` is consumed on every resolved outcome,
  so it survives a crash and its presence still means a dispatch was in flight from a verified-clean
  tree. **Present** ⇒ everything dirty is that subagent's residue; recover it the way Step 5 does,
  even though a different process wrote the file, reading the `task-N` to scope the recovery commit
  to from the file's second field rather than guessing which task was in flight. Only when it is
  **absent** is there no clean-tree guarantee, and then, unlike the Step 5 after-return check, you
  cannot assume every dirty path is residue. Attribute each path before staging it: a path a
  remaining task's brief would plausibly touch is residue; anything else (unrelated scratch, a
  human's in-flight edit, files no plan task names) is **not** — leave it alone and note it, never
  sweep it in. Commit only the attributed paths
  (`chore(task-N): recover uncommitted subagent work`, substituting the task's number for `N`;
  stage exactly those paths, never a blanket `git add -A`), then re-assess — the recovered task may
  already be complete. Whatever cannot be attributed with confidence blocks the run: Step 5 dispatches
  only from a clean tree, so go to **Stop cleanly** with BLOCKED naming those paths rather than
  dispatching on top of them.
- Re-verify rather than trusting the log alone when a commit's diff is thinner than its task's
  criteria: treat that task as remaining and note the partial work in the brief.
