# Multi-root combined runs (`--epic`)

Situational detail for the additive `--epic` selector. The resident SKILL body
carries the rules a driver needs while it is deciding; this file carries the
reasoning, the rejected alternatives, and the failure modes behind them.

## Why `--epic` rather than re-reading positionals

Two or more positional refs already mean an **explicit list of work items**.
Re-interpreting them as several parents would be a silent breaking change: the
same command line would stop scheduling the tickets named and start scheduling
their children instead. `--epic` is therefore the only multi-root selector, and
mixing it with positionals is rejected rather than guessed — a positional next
to an `--epic` is genuinely ambiguous (root, or work item?), and the two
readings schedule different tickets.

## Why the parse shape is what it is

- `mode` is the discriminator: `parent` (one positional), `list` (two or more),
  `parents` (one or more `--epic`).
- `parentIds` is present in every mode — `[id]`, `[]`, and the deduplicated root
  list respectively — so combined-run code reads one key regardless of how the
  run was selected.
- `parentId` stays **null** in `parents` mode. A legacy single-parent consumer
  must fail to find a root rather than silently run only the first of several.
- `--epic` refs deduplicate first-seen; **positionals do not**. The positional
  COUNT is what picks `parent` vs `list`, so collapsing a repeated positional
  would flip a two-ticket explicit list into a single-parent run.

## Membership is not dependency

`buildCombinedRun` returns `childrenByParent` and `parentsByChild` alongside the
deduplicated `childIds`. A child under two roots is **one** node in the graph and
**two** membership entries. Association alone is never an edge: two roots sharing
a child does not make either root's other children depend on anything, and a
shared child that fails is not a failure of the roots — it cascade-skips only its
real graph dependents, on every projection that contains them.

Keep the two apart in every decision:

- hydrate / classify / graph / launch / reconcile / merge → keyed on `childIds`,
  each child exactly once;
- report → keyed on `childrenByParent`, the same child on every owning root.

## Cross-root edges and external blockers

A `blockedBy` edge between two children of **different** selected roots is an
ordinary in-set edge once both endpoints are in the combined node set — that is
the whole point of one graph. An edge to a ticket outside the combined eligible
set stays an **external** blocker and is re-checked every poll cycle, so an
outside ticket finishing mid-run unparks its dependents without a restart. A
blocker owned by a session outside this run is reported `cannot-evaluate-here`,
never counted as mergeable.

## Unlock ranking across roots

`transitiveDependentCounts` is scoped to the combined graph, so a child that
unlocks work under a _different_ selected root outranks one that unlocks nothing
under its own. This is why one coordinator beats several: run the roots
separately and each scheduler sees only its own slice of the critical path.

`transitiveDependents` (cascade-skip) is a different question over the same edges
— what a failure poisons, not what a launch frees. Do not substitute one for the
other.

## Terminal decisions over a combined run

A combined run is terminal only when the decision holds over the **whole**
deduplicated child universe: every eligible child terminal or settled, no ready
candidates, no in-flight work, and no unresolved reconciliation state. One root
finishing is not a terminal result — its children may still be blocked behind a
sibling root's work, and unknown liveness stays parked rather than becoming
success.

## Per-root progress writes are separate side effects

Each selected root gets its own upserted comment, so a combined report is N
tracker writes that can partially fail. A failed upsert on one root is reported
against that root and retried on the next transition; it must never cause a
second coordinator, a second session, or a duplicate comment on the roots that
did succeed — the marker anchor is what makes the retry an update rather than an
insert.
