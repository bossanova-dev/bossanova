---
name: boss-review-ce
description: Built-in boss-review round extension. Reviews the branch with Compound Engineering's ce-code-review when that plugin is installed. Not invoked directly.
x-boss-extension:
  extends: boss-review
  role: round
  order: 10
  capability: code-review
---

# boss-review-ce

A read-only whole-branch round: never change the worktree, index or HEAD.

**When `compound-engineering:ce-code-review` is not available**, or it loads but its reviewers cannot
run, or it returns nothing parseable, write the failure envelope (`"ok": false`, `"error"` saying
which) so boss-review runs its own round instead. Only a CE pass that ran and found nothing returns
`ok: true` with empty `items`.

Run it in report-only mode over exactly this round's diff:

```
Skill(skill: "compound-engineering:ce-code-review", args: "mode:agent base:<context.base> plan:<plan path>")
```

Never pass a PR number or branch alongside `base:`. For `plan:`, use the plan document the branch
adds, if there is exactly one:

```bash
git diff --name-only --diff-filter=A "<context.mergeBase>...<context.head>" -- 'docs/plans/*.md'
```

None or several ⇒ omit `plan:` and say which in `notes`. When `context.carriedClaims` is non-empty,
ask CE to re-check those `{findingId, file, anchor}` claims too.

`mode:agent` returns one JSON object whose `findings[]` carry `title`, `severity`, `file`, `line`,
`why_it_matters`, `evidence` and `suggested_fix`. Map each to a boss-review finding
`{severity, file, line, title, detail, patch, lens: "ce-code-review"}`: severity `P0` and `P1` ⇒
`Critical`, `P2` ⇒ `Warning`, `P3` ⇒ `Suggestion`; `detail` from the why and evidence; `patch` the
verbatim edit when one is safe, otherwise `null` with a `patchReason`.

Write one envelope to `outPath` and check it with
`node "<dir>/../../toolbox/skill-extensions.mjs" validate --role round --file <outPath>`:

```json
{
  "ok": true,
  "extension": "boss-review-ce",
  "role": "round",
  "items": [],
  "notes": "",
  "error": null
}
```
