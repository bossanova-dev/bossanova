---
name: boss-plan-compound-engineering
description: Built-in boss-plan draft extension. Drafts the plan with Compound Engineering's ce-plan when that plugin is installed. Not invoked directly.
x-boss-extension:
  extends: boss-plan
  role: draft
  order: 40
---

# boss-plan-compound-engineering

Draft the plan for `context.ticket` with Compound Engineering's `ce-plan` and deliver it at
`context.planPath`.

**When `compound-engineering:ce-plan` is not available** (the Skill tool has no such skill, or it
fails to load), write the failure envelope (`"ok": false`, `"error": "compound-engineering not
installed"`) and stop, so boss-plan drafts the plan itself. Never draft with another planner here.

## Run ce-plan

Invoke `compound-engineering:ce-plan` through the Skill tool with the ticket as the task and
`context.designDoc`, when there is one, as its requirements document. In the same invocation:

- Ask it to write its plan, and anything else it creates, under `<runTmp>/ce/`.
- **Interactive** (`context.mode` is `interactive`): say so explicitly, so it runs its native
  interview, deepening and document review with the user. Let the user answer its questions. From its
  closing menu take only the branch that saves the plan; decline creating a tracker issue or starting
  implementation, because boss-plan owns the tracker write and nothing is implemented while planning.
- **Headless**: say it is a pipeline run with nobody watching, so it takes the non-interactive path,
  asks nothing, and skips its document review (boss-plan's reviewers run afterwards). A question from
  CE in this mode is a failed dispatch.

If CE wrote its plan inside the repository anyway, take that file as the draft and then remove it if
git does not track it. Report any other repository file CE changed in `notes`, and leave it alone.

## Deliver

Keep CE's structure and detail, and make the plan meet boss-plan's plan rules in
`<dir>/../../references/headless-drafting-brief.md` (Step 5): in particular it ends with
`## Original notes`, copied byte-for-byte from the description snapshot boss-plan gave you, with
upload URLs query-stripped. Write it to `context.planPath`, then write one envelope to `outPath`:

```json
{
  "ok": true,
  "extension": "boss-plan-compound-engineering",
  "role": "draft",
  "planPath": "<context.planPath>",
  "notes": "",
  "error": null
}
```
