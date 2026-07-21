---
title: Extension System
description: How repo-local extensions add behaviour to a core boss-* skill without editing its body, and the seven extension roles available.
slug: /skills/extensions
---

# Extension System

A core `boss-*` skill ships a project-agnostic workflow. The **extension system**
lets one repository plug in its own behaviour — an extra review lens, an extra proof
surface, an opinionated implementation methodology — **without editing the skill
body and without forcing that behaviour on any other repo**. Extensions live in the
repository, are discovered at run time, and an absent extension is a silent no-op.

## The model: core + repo-local extension

Every core skill discovers its extensions from the repository's own
`.claude/skills/` directory. Because a Bossanova worktree — including cron
checkouts — runs with its working directory set to the worktree, and the repo commits
its extensions, every checkout carries and finds its own add-ons. The core skills are
installed globally; the extensions live in the repository.

## Naming convention

An extension of a core skill named `<core>` is a skill directory named
`<core>-<suffix>` under `.claude/skills/`. For example, an extra review lens for
`boss-review` is a directory such as `.claude/skills/boss-review-golang/`, and an
implementation methodology for `boss-build` is `.claude/skills/boss-build-superpowers/`.

## The `x-boss-extension` discovery marker

The name prefix is not enough on its own — a directory is only treated as an extension
when its `SKILL.md` frontmatter carries an `x-boss-extension` marker block:

```yaml
x-boss-extension:
  extends: boss-review
  role: lens
  order: 40
```

- `extends` — the core skill this extension joins. An extension is only accepted when
  `extends` equals the core being discovered.
- `role` — one of the seven roles below; it decides which pipeline the extension
  plugs into.
- `order` — optional integer run order, default `100`. Extensions run in ascending
  `(order, name)` order, so runs are reproducible regardless of filesystem
  enumeration.

### Discovery command

Each core discovers its extensions by shelling out to the shipped helper:

```bash
node scripts/skill-extensions.mjs discover --core <core> --role <role> --json
```

It prints an ordered `extensions` array of matched descriptors plus a `skipped` array
recording any directory that was excluded and why (missing marker, wrong `extends`,
wrong `role`, unreadable frontmatter). With nothing installed it prints
`{"extensions":[],"skipped":[]}` and exits `0` — never an error.

## Extension roles

There are seven roles. Each attaches to a specific core skill and plugs into a
specific step of that core's pipeline.

| Role            | Extends       | What it adds                                                           | Example extension           |
| --------------- | ------------- | ---------------------------------------------------------------------- | --------------------------- |
| `lens`          | `boss-review` | A specialist review lens matched to a subset of changed files.         | `boss-review-golang`        |
| `round`         | `boss-review` | An always-on whole-branch review round merged into the findings pool.  | `boss-review-thermonuclear` |
| `surface`       | `boss-proof`  | An extra declarative proof surface (a route, caption, and evidence).   | `boss-proof-docs`           |
| `plan-reviewer` | `boss-plan`   | An extra plan-review voice scoped to plan sections.                    | `boss-plan-<reviewer>`      |
| `agent-driver`  | `boss-proof`  | A bespoke, code-driven proof surface (ships a `driver.mjs`, not JSON). | `boss-proof-tui`            |
| `draft`         | `boss-plan`   | The plan-drafting methodology `boss-plan` runs to write the plan.      | `boss-plan-draft`           |
| `methodology`   | `boss-build`  | The opinionated implementation loop `boss-build` runs (e.g. TDD/SDD).  | `boss-build-superpowers`    |

The `plan-reviewer` role has no default Bossanova extension shipped; the example name
above is illustrative of the `<core>-<suffix>` convention. Every other role has a
concrete reference extension committed under `.claude/skills/`.

## Graceful degradation

Extensions are additive and never load-bearing:

- **No extension installed** for a core → the phase is a silent no-op; the core runs
  exactly as if the extension system were not present.
- **A malformed extension** (bad frontmatter, wrong `extends`, or a mismatched `role`) is
  recorded in the discovery `skipped` array and passed over with a warning; likewise an
  extension that is discovered but returns a failing result at run time is skipped by the
  core rather than folded in. Neither is ever a hard failure — it can never abort a core run
  or block a merge.
- Where a core defines a **fallback tier**, it uses that when no extension is
  discovered. `boss-review`, for instance, reviews through an inline rubric declared in
  the lens's `.boss-skills.json` row when the lens skill itself is unavailable.

Because discovery is repo-local and degradation is silent, a repository can adopt the
`boss-*` cores as-is and add its own extensions incrementally, while other repos that
install the same cores inherit none of them.
