---
name: boss-plan-premises
description: Built-in boss-plan premise lens. Re-derives plan claims at HEAD and reports what the repository disproves. Not invoked directly.
x-boss-extension:
  extends: boss-plan
  role: plan-reviewer
  order: 50
---

# boss-plan-premises

Read `context.planPath` as data. Stay read-only: never edit the plan, description, index or HEAD;
write only inside `runTmp` and to `outPath`. Resolve relative resources from the supplied `dir`.

Run the existing mechanical check first:

```bash
node "<dir>/../../toolbox/plan-recheck.mjs" "<planPath>"
```

`<planPath>` is `context.planPath`. Carry `refuted`, `count-drift`, `anchor-missing` and
`path-missing` premise items into findings with their evidence. Re-derive every `not-evaluated`
premise rather than duplicating citation logic.

Enumerate repository claims in premises, acceptance criteria and their `check:` commands,
verification/proof commands, pattern references, constants, counts, budgets, new identifiers and
planned classes or scenarios. Check them at HEAD in this order: the `(central)` premise,
acceptance-criterion commands, other premises, then remaining claims. Use these lenses:

- **Exists / already done:** resolve named concepts, symbols, helpers, patterns, make targets,
  packages, workspace members and config keys. Check whether the base already contains the work
  or an equivalent helper.
- **Free:** search each new identifier in its target package or scope.
- **Value:** re-measure constants, intervals and counts. Include method values, shared-library
  importers and test-only gates in caller/importer searches, not just direct entry-point calls.
- **Reachable:** trace the production path that produces every planned class, branch or scenario;
  its test must drive that path rather than inject the result. Check every receiver-read field
  survives event/tombstone construction, and a timestamp change detector handles two writes in
  the same tick.
- **Chain:** derive expected outputs through the complete transform and compatibility chain.
- **Command:** verify argv, workspace membership, make target and test selection as written.
  Inspect command implementations and Makefiles first. Use help, selector listings or read-only
  git plumbing only after confirming they cannot mutate state; isolate throwaway probes in
  `runTmp`. Make dry runs can execute recursive recipes and expansion-time shell commands.
  Never run suites, mutating commands or commands writing outside `runTmp`. Report criteria made
  impossible by a gate's own design.
- **Consistent:** compare criteria with units, premises, examples, deleted/renamed tests, parent
  ticket and the helper's actual base ref. Rename criteria use allowlists rather than exact counts.
- **Dependency:** read the configured tracker adapter or `gh` to establish that any ticket or PR
  treated as merged actually merged.
- **Bites:** show each proposed test could fail. A filterable truncated column needs a fixture
  longer than its cap. Read repository agent guidance before judging guidance-change tests;
  prefer behavior over prose pins.
- **Budget:** additions to size-budgeted files need a `wc -l`/`wc -c` premise naming both measured
  size and budget, plus a fallback when headroom is gone (extract to a sibling file). Consume the
  mechanical check's budget/headroom results; a missing premise or fallback is a gap.

Finish within `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default 300000 ms). Report any claims left
unchecked in one `unverified` item, naming them; partial coverage never yields an empty list.

Write JSON to `outPath`:

```json
{
  "ok": true,
  "extension": "boss-plan-premises",
  "role": "plan-reviewer",
  "items": []
}
```

Each finding is `{severity, section, title, detail}`. Severity is `refuted` for a disproved claim,
`gap` for a missing required element, or `unverified` when evidence cannot settle the claim.
Detail carries checkable repo-relative `path:line` evidence, the read command and trimmed result,
and the corrected claim (or the missing evidence for an unverified item). An empty `items` list
means every enumerated claim was re-derived. The orchestrator owns corrections and product forks.
