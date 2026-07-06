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
