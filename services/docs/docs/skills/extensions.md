---
title: Extension System
description: How repo-local extensions add behaviour to a core boss-* skill without editing its body, and the extension roles available.
slug: /skills/extensions
---

# Extension System

A core `boss-*` skill ships a project-agnostic workflow. The **extension system**
lets one repository plug in its own behaviour (an extra plan reviewer, an opinionated
implementation methodology, end-of-run notes) without editing the skill body
and without forcing that behaviour on any other repo. Extensions live in the
repository, are discovered at run time, and an absent extension is a silent no-op.

## The model: core + repo-local extension

Every core skill discovers its extensions from the repository's own
`.claude/skills/` directory. Because a Bossanova worktree (including cron
checkouts) runs with its working directory set to the worktree, and the repo commits
its extensions, every checkout carries and finds its own add-ons. The core skills are
installed globally; the extensions live in the repository.

## Naming convention

An extension of a core skill named `<core>` is a skill directory named
`<core>-<suffix>` under `.claude/skills/`. For example, an implementation methodology for
`boss-build` is `.claude/skills/boss-build-ce/`.

## The `x-boss-extension` discovery marker

The name prefix is not enough on its own. A directory is only treated as an extension
when its `SKILL.md` frontmatter carries an `x-boss-extension` marker block:

```yaml
x-boss-extension:
  extends: boss-build
  role: methodology
  order: 40
```

- `extends` — the core skill this extension joins. An extension is only accepted when
  `extends` equals the core being discovered.
- `role` — one of the roles below; it decides which pipeline the extension
  plugs into.
- `order` — optional integer run order, default `100`. Extensions run in ascending
  `(order, name)` order, so runs are reproducible regardless of filesystem
  enumeration.
- `modes`: the run modes the extension is discovered in, a comma-separated subset of
  `interactive` and `headless` (for example `modes: interactive`). Absent means every mode. A
  present but unusable value (empty, or a token outside that set) is skipped as `invalid-modes`
  rather than read as "every mode". When a caller passes `--mode`, an extension whose `modes` omits
  it is skipped as `mode-not-declared`.

### Discovery command

Each core discovers its extensions by shelling out to the shipped helper:

```bash
node scripts/skill-extensions.mjs discover --core <core> --role <role> --json
```

It prints an ordered `extensions` array of matched descriptors plus a `skipped` array
recording any directory that was excluded and why. With nothing installed it prints
`{"extensions":[],"skipped":[]}` and exits `0`, never an error.

Passing `--mode <interactive|headless>` names the run mode the caller is in. Omitting it applies
no mode filter; an unknown value exits `2` before scanning.

Each `skipped` entry carries four fields: the extension `name`, the human `reason`, a stable `code`
to branch on, and a `deliberate` classification.

```json
{
  "extensions": [],
  "skipped": [
    {
      "name": "boss-build-ce",
      "reason": "invalid \"modes\" (expected a comma-separated subset of interactive, headless)",
      "code": "invalid-modes",
      "deliberate": false
    },
    {
      "name": "boss-review-helper",
      "reason": "missing x-boss-extension marker",
      "code": "missing-marker",
      "deliberate": true
    }
  ]
}
```

`deliberate: true` means the skip is the contract working as designed: the directory is a
same-prefix skill that simply is not an extension of this core — a markerless helper, or one
extending a core that this one's prefix merely nests into (`boss-` also matches `boss-plan-notes`),
or an extension that declared it does not run in the requested `--mode`. Reporting it would cry
wolf on every run. Only three codes are classified that way (`missing-marker`, `extends-other-core`
and `mode-not-declared`).

A wrong `extends` is **not** one of them. An extension of `<core>` is a directory named
`<core>-<suffix>`, so `boss-review-x` declaring `extends: boss-plan` is unreachable from
`boss-plan` too, so nothing anywhere would report it. That case gets its own reportable code,
`extends-unrelated-core`, with the same `reason` sentence.

Every other code is a real misconfiguration, and a core **must record each one** as
`extension <name>: skipped (<reason>)` in its ledger, at every site that calls `discover`. A
misconfigured extension that vanishes with no ledger line is indistinguishable from one that was
never installed, so the core's fallback tier reads as though it had always been the intended tier.
Recording is all that is due; a discovery skip is never fatal and never changes control flow.

## Extension roles

Each role attaches to a specific core skill and plugs into a
specific step of that core's pipeline.

| Role            | Extends       | What it adds                                                                                      | Example extension                |
| --------------- | ------------- | ------------------------------------------------------------------------------------------------- | -------------------------------- |
| `surface`       | —             | An extra declarative evidence surface. The role stays in the schema; no in-repo core reads it.    | —                                |
| `plan-reviewer` | `boss-plan`   | An extra plan-review voice scoped to plan sections.                                               | `boss-plan-<reviewer>`           |
| `agent-driver`  | —             | A bespoke, code-driven evidence surface. The role stays in the schema; no in-repo core reads it.  | —                                |
| `draft`         | `boss-plan`   | The plan-drafting methodology `boss-plan` runs to write the plan.                                 | `boss-plan-compound-engineering` |
| `methodology`   | `boss-build`  | The opinionated implementation loop `boss-build` runs.                                            | `boss-build-ce`                  |
| `notes`         | `boss-build`  | Post-terminal persistence of end-of-run notes to an external store.                               | `boss-build-notes`               |
| `knowledge`     | `boss-build`  | Pre-PR capture of what the run learned, committed as an artifact inside the PR.                   | `boss-build-knowledge`           |
| `verify`        | `boss-verify` | A verification judgment on one PR head: `pass` with evidence, `fail` with findings, or `abstain`. | `boss-verify-<verifier>`         |

The `plan-reviewer` and `verify` roles have no default Bossanova extension
shipped; their example names above are illustrative of the `<core>-<suffix>` convention. Every
other role has a concrete reference extension committed under `.claude/skills/`.

## Graceful degradation

Extensions are additive and never load-bearing:

- **No extension installed** for a core → the phase is a silent no-op; the core runs
  exactly as if the extension system were not present.
- A **malformed extension** (bad frontmatter, wrong `extends`, or a mismatched `role`) is
  recorded in the discovery `skipped` array and passed over with a warning; likewise an
  extension that is discovered but returns a failing result at run time is skipped by the
  core rather than folded in. Neither is ever a hard failure; it can never abort a core run
  or block a merge.
- Where a core defines a **fallback tier**, it uses that when no extension **ran
  successfully**, not merely when none was discovered. A discovered extension that fails to
  load or returns no valid result is recorded as skipped and the core falls through to the
  next tier, so the layer is never silently dropped.

Because discovery is repo-local and degradation is silent, a repository can adopt the
`boss-*` cores as-is and add its own extensions incrementally, while other repos that
install the same cores inherit none of them.
