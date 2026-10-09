---
name: boss-build-ce
description: Built-in boss-build methodology extension. Implements the plan with Compound Engineering's ce-work when that plugin is installed. Not invoked directly.
x-boss-extension:
  extends: boss-build
  role: methodology
  order: 40
---

# boss-build-ce

Implement the scope boss-build handed you with Compound Engineering's `ce-work`, passing the plan
file as its input document:

```
Skill(skill: "compound-engineering:ce-work", args: "<plan file path>")
```

**When `compound-engineering:ce-work` is not available** (the Skill tool has no such skill, or it
fails to load), do nothing: return with no work landed and `residualRisks: ["compound-engineering
not installed"]`, so boss-build falls through to its own implementation tiers. Never improvise a
replacement loop.

boss-build owns the worktree, branch, review and shipping. Tell `ce-work` so when you invoke it, and
hold it to that:

- Run its implementation phases and its local checks (tests, the repo's lint gate and the tests relevant to the change (boss-build `## Verification`), never the full suite; report each command and its result in `testsAddedOrPassing`) only.
- Stop before its shipping workflow: no push, no PR or tracker writes, no branch create, switch or
  rename, no merge or rebase, and no edits to the plan file.
- Do not run `ce-code-review` from inside it; boss-build reviews the branch next.
- Never ask questions. Decide ambiguity yourself and record each decision.
- Commit under the commit contract boss-build gave you, which overrides any commit convention
  `ce-work` brings.

Return only boss-build's task contract: `taskId`, `filesTouched`, `testsAddedOrPassing`,
`interfaceSignatures`, `residualRisks`, `decisionsRecorded`, `commitsMade` (every commit this dispatch
landed, as `<short SHA> <subject>`).
