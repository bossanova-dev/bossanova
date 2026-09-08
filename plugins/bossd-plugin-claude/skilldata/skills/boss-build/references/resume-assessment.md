# Step 4.5 — Assess adopted work (resume only)

Read this when **Step 2.5** marked the branch a **resume** (real work already committed on the branch,
whether by a prior bossd-managed or standalone run). The point is to know what a prior run implemented
before touching anything.

```bash
git diff --stat "$BASE_REF"...HEAD
git log --oneline "$BASE_REF..HEAD"
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

## Dispatch snapshot mechanics

**This section applies to every run, not only a resume** — the resume-only gate above scopes the
section before it. Step 5 sends you here on a fresh branch too: a subagent that never returned
leaves the same residue whether or not Step 4.5 ever executed.

The one procedure, written once. Step 5 states the invariant it settles — every dispatch's work is
committed before the run advances, and unattributable residue stops the run — and both the
orchestrator's own after-return check and the restarted-orchestrator recovery below run these
spellings.

Each shell invocation is a fresh process, so nothing set in the first block survives into the
second. Re-assign every variable in the block that uses it; `:?` aborts rather than letting an unset
`PLAN_DOC` become a bare `:(exclude)`, which excludes _everything_ and turns the check into a silent
pass. Keep `--untracked-files=all`: at the default `-unormal` git collapses an untracked directory
to a single `.claude/` entry that no per-file exclusion matches, silently restoring an every-run
false positive that stops the check discriminating.

**Before the dispatch**, as one invocation:

```bash
PLAN_DOC="docs/plans/<the file Step 4 saved>"   # also record this in the run notes
git status --porcelain --untracked-files=all -- . \
  ":(exclude)${PLAN_DOC:?PLAN_DOC unset — re-read it from the run notes}" \
  ':(exclude).claude/scheduled_tasks.lock' ':(exclude).claude/settings.local.json'
# …must print nothing. Only once it does, record the HEAD the dispatch starts from *and* which
# dispatch is starting — substitute the task's number for N:
printf '%s task-N\n' "$(git rev-parse HEAD)" \
  >"$(git rev-parse --git-dir)/boss-build-pre-dispatch-head"
```

If that status is not empty, resolve the dirt **and re-run this whole block** — the recorded HEAD
has to be the commit the dispatch actually starts from, or a cleanup commit alone makes the
after-return range non-empty and a dispatch that landed nothing reads as done.

The file lives under `$(git rev-parse --git-dir)`, not `/tmp`: it resolves to this worktree's own
git directory, so concurrent runs in sibling worktrees cannot overwrite each other's value, and it
is never committed.

**Label the snapshot for the dispatch unit**, and branch on which of the two forms you read rather
than assuming the per-task one:

- `task-N` — a per-task dispatch. Commit residue as `chore(task-N)` and re-assess task `N`.
- `ext-<name>` — one whole Tier-1 methodology extension. Recovery is extension-wide: commit residue
  as `chore(ext-<name>)` and re-assess that extension's entire Step-5 scope, never a single task
  inside it.

**After the subagent returns**, as a second self-contained invocation — same `PLAN_DOC`, same
pathspec:

```bash
PLAN_DOC="docs/plans/<the file Step 4 saved>"   # re-set: this is a new shell
git status --porcelain --untracked-files=all -- . \
  ":(exclude)${PLAN_DOC:?PLAN_DOC unset — re-read it from the run notes}" \
  ':(exclude).claude/scheduled_tasks.lock' ':(exclude).claude/settings.local.json'
# must be empty
git log --oneline "$(cut -d' ' -f1 "$(git rev-parse --git-dir)/boss-build-pre-dispatch-head")..HEAD"
# …must list this dispatch's commit(s)
```

**Recovering residue.** Stage exactly the attributed paths — the ones the status listed _and_ the
returned contract named (all of them when the subagent never returned to name any):

```bash
git add -- <the attributed residue paths>
# --only commits exactly these paths. A plain `git commit` would commit the whole index, which can
# hold a path the status above deliberately excluded ($PLAN_DOC, a daemon artifact) staged earlier
# and therefore invisible to the check — swept in silently.
git commit --only -m "chore(task-N): recover uncommitted subagent work" \
  -- <the same attributed paths>  # substitute the dispatch's label
```

That commit goes through the same hooks the subagent's did, so it can be rejected the same way:
adapt the subject to exactly what the hook's own error names and retry once. If it still will not
commit, leave the residue in the tree, stop dispatching, and go to **Stop cleanly** with BLOCKED
naming the uncommitted paths — never revert the work, and never continue on top of residue you
could not capture.

**Consume the snapshot** at **any** resolved outcome — the range checked out, a verification-only
dispatch declared _no commit_, or you recovered the residue yourself — not only on the clean path:

```bash
rm "$(git rev-parse --git-dir)/boss-build-pre-dispatch-head"
```

On the recovery path that means **after** the recovery commit lands, never before you start it:
delete it first and a crash in between throws away the clean-tree guarantee that made the residue
attributable. Consuming it is what keeps its meaning honest — the file exists **only** while a
dispatch is in flight, so a restarted orchestrator that finds one knows it belongs to the dispatch
that was interrupted rather than to one that already finished.

## Continue from committed state

Implementation subagents commit per task (the Step 5 commit-before-return contract), so an
interruption — a subagent that died mid-flight, a transient host error, a restarted run — usually
leaves the completed tasks already on the branch. On **any** resume or re-dispatch after an
interruption, inventory that committed state before dispatching anything:

```bash
git log --oneline "$BASE_REF..HEAD"
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
  the tree. Which recovery applies turns on the snapshot above, never on whether your process
  restarted —
  `"$(git rev-parse --git-dir)/boss-build-pre-dispatch-head"` is consumed on every resolved outcome,
  so it survives a crash and its presence still means a dispatch was in flight from a verified-clean
  tree. **Present** ⇒ everything dirty is that subagent's residue; recover it with the commands
  above,
  even though a different process wrote the file, reading the `task-N` (or the `ext-<name>` a
  whole-extension dispatch wrote) to scope the recovery commit to from the file's
  second field rather than guessing which task was in flight. An `ext-<name>` scopes that recovery
  to the extension, whose whole scope is re-assessed rather than one task. Only when it is
  **absent** is there no clean-tree guarantee, and then, unlike the Step 5 after-return check, you
  cannot assume every dirty path is residue. Attribute each path before staging it: a path a
  remaining task's brief would plausibly touch is residue; anything else (unrelated scratch, a
  human's in-flight edit, files no plan task names) is **not** — leave it alone and note it, never
  sweep it in. Commit only the attributed paths
  (`chore(task-N): recover uncommitted subagent work`, substituting the task's number for `N` —
  `chore(ext-<name>)` where the snapshot carried an extension label;
  stage exactly those paths, never a blanket `git add -A`), then re-assess — the recovered task may
  already be complete. Whatever cannot be attributed with confidence blocks the run: Step 5 dispatches
  only from a clean tree, so go to **Stop cleanly** with BLOCKED naming those paths rather than
  dispatching on top of them.
- Re-verify rather than trusting the log alone when a commit's diff is thinner than its task's
  criteria: treat that task as remaining and note the partial work in the brief.
