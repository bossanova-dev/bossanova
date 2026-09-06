---
title: Skills
description: The boss-* skill suite that automates the ticket-to-merged-PR lifecycle, and the adapter model that ports it to any repo.
slug: /skills
---

# Skills

The `boss-*` skills are a suite of portable, config-driven Claude Code and Codex
skills that automate the software delivery lifecycle: planning a backlog ticket,
implementing it to a review-ready pull request, reviewing the branch, orchestrating
a whole epic, capturing proof media, repairing a red PR, and landing the work. Each
skill is a self-contained workflow you invoke by name (for example `/boss-build`),
and together they take a ticket from **Unplanned → planned Todo → PR → review →
merge → proof** with minimal hand-holding.

Every skill is built as a **project-agnostic core** plus a few pluggable seams, so
the same suite runs in any repository. The Bossanova-specific coupling (the issue
tracker, proof-artifact publishing, the session runner, and the PR-finalize policy)
lives behind adapters that default to the Bossanova reference implementation. A repo
adopting these skills configures them with a single `.boss-skills.json` file, and can
extend a core skill's behaviour with repo-local add-ons without editing the skill
body (see [Extension System](/skills/extensions)).

## The skill suite

Each skill runs on its own, but they also compose: `boss-plan` emits a plan that
`boss-build` consumes, `boss-build` dispatches `boss-review` and `boss-proof` inside
its flow, and `boss-epic` orchestrates many `boss-build` runs and drives
`boss-repair` on failures.

| Skill           | What it does                                                                                                                                                                                                                                          |
| --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `boss-plan`     | Plans a backlog ticket: recon, a plan-review pass, finalizes the rendered plan as a native tracker attachment, and writes a summary, labels, estimate, and priority back to the tracker. Moves the ticket Unplanned → Todo.                           |
| `boss-build`    | Implements one planned ticket to a review-ready PR via subagent-driven TDD, a bounded review stack, and a clear terminal state (`REVIEW_READY` / `PARTIAL` / `BLOCKED` / `NO_CHANGE`), where `PARTIAL` opens a do-not-merge PR for a certified slice. |
| `boss-review`   | Multi-lens, subagent-driven code review of a branch; fixes must-fix findings and emits an Assessment / Evidence / Confidence report. Invoked by `boss-build` or run by hand.                                                                          |
| `boss-epic`     | Orchestrates a whole epic of planned tickets to merged PRs: dependency-ordered schedule, parallel implement sessions, serialized merges, and progress reported on the parent ticket.                                                                  |
| `boss-proof`    | Captures proof-of-implementation media (screenshots and video) for a PR's changed surfaces and comments it on the PR.                                                                                                                                 |
| `boss-repair`   | Automated PR repair — fixes merge conflicts, failing checks, and review feedback.                                                                                                                                                                     |
| `boss-finalize` | End-of-session workflow ensuring all work is committed and pushed ("land the plane").                                                                                                                                                                 |

## The adapter model

The suite keeps its Bossanova coupling confined to **four seams**. Each seam is an
adapter selected by configuration and defaulting to the Bossanova reference
implementation, so omitting the configuration reproduces the built-in behaviour. To
port the skills to a different stack, a repo ships a small module that conforms to
the seam's capability contract and points the adapter at it; the skill bodies never
change.

| Seam            | What it abstracts                                                                | Default implementation |
| --------------- | -------------------------------------------------------------------------------- | ---------------------- |
| `tracker`       | Selecting, claiming, moving, and commenting on issues in the tracker.            | `linear`               |
| `finalize`      | The tag-injection → draft-to-ready → repair policy when finalizing a PR.         | `boss-finalize`        |
| `publish`       | Publishing proof artifacts; implementation plans use native tracker attachments. | `proof`                |
| `sessionRunner` | Session choreography plus implement / repair sub-skill dispatch.                 | `bossd`                |

### The `.boss-skills.json` config

A consuming repo drops a single `.boss-skills.json` file at its root. The skills read
it instead of any hard-coded settings: the loader walks up from the working
directory, merges the file over built-in defaults, and validates it. Alongside the
adapter selection, the config declares the review `lensMap`, the build/lint/test
`commands`, the headless-mode `env` signals, and the versioned `planContract` that
`boss-plan` emits and `boss-build` consumes.

The adapter block in Bossanova's own config reads:

```json
{
  "adapters": {
    "tracker": "linear",
    "finalize": "boss-finalize",
    "publish": "proof",
    "sessionRunner": "bossd"
  }
}
```

Editing `.boss-skills.json` is how a repo retargets the suite: change an adapter value
to swap a seam, adjust `commands` to match the repo's build system, or reclassify a
`planContract` section. Because the config is declarative and validated, the skills
fail fast on an unknown adapter selector rather than silently misbehaving.

## Learn more

- [Extension System](/skills/extensions) — how to extend a core skill's behaviour
  with repo-local add-ons (extra review lenses, proof surfaces, plan reviewers, and
  implementation methodologies) without editing the skill body.
