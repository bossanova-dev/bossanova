# Proof capture (Step 11) and TUI scenario authoring (Step 5)

For a TUI diff, read **Scenario authoring (TUI)** during Step 5 and complete its validation loop before
Step 6. Read the remaining sections only when Step 11 of `SKILL.md` fires — on the `REVIEW_READY` path
(green, ready PR, ticket already moved to **In Review**). Skip Step 11 entirely for `BLOCKED`, draft,
and `NO_CHANGE`. Step 11 may **never** change the terminal state: proof is best-effort and every
outcome below is recorded and ignored, never routed to BLOCKED.

## The single proof channel

`node scripts/proof.mjs run` posts its own PR comment, and its structured deferred note is the
**only** proof channel. A session **never**
hand-writes skip prose or a "proof skipped: …" one-line note, and never invents its own
"no recipe matched → write a PR note" fallback. When proof cannot run — no UI surface, a missing
prerequisite, or a pipeline bug — you still run the proof pipeline and let it post the honest
structured note. It classifies the outcome for you:

- **no-ui-surface** — the change has nothing to render; the note says so and contributes exit 0.
- **forced-no-surface** — a `## Required proof` bullet forced a surface onto a diff with no product
  source and no recipe; nothing to capture, contributes exit 0. `plan` predicts it as
  `forcedNoSurface: true`.
- **env-unavailable** — a required key, toolchain or built binary is missing; the note embeds the
  doctor report naming exactly what is absent, and contributes exit 0 so a human can provision it.
- **scenario-missing** — a TUI change shipped without a committed `proof/scenarios/*.scenario.json`
  **and** the TUI agent cannot capture (no proof key, or recipe mode); contributes exit 1. With a key,
  a scenario-less TUI diff runs the agent leg instead, whose brief may defer
  **no-ui-surface** (exit 0) when the diff touches no `services/boss/internal/views/` or `fixtures/`
  file. Read the manifest's `clamped` array before trusting a green process exit either way.
- **capture-failed** — a deterministic recipe capture failed; the note names each failed recipe with
  its error and the `--recipe` re-run command, and contributes exit 1.
- **pipeline-error** — a proof-pipeline bug (render/encode/bridge crash); the note names the failing
  stage and never blames the environment; contributes exit 1 as an internal retry signal only.
- **agent-incomplete** — a real surface the agent could not demonstrate; contributes exit 1
  (internal signal).

None of these exit codes gate finalization — Steps 8–9 already ran. The posted note, never any
hand-written prose, is the source of truth for reviewers.

## Reading the outcome

The process exit code is an aggregate across surfaces. The proof finalizer's `aggregateExitCode` and
`surfaceExitContribution` helpers make the whole process exit 1 when any surface contributes a
failure, so exit 1 does not prove the required surface failed. Read the per-surface `outcome` and
`reasonCode` from the run log before concluding anything.

`agent-incomplete` is not `scenario-missing`. The former is a driver or environment fact and is
routinely expected in an unattended worktree; the latter is a planning or wiring defect. When an
acceptance criterion names a specific `reasonCode`, grep the run log for that exact code rather than
for a deferral or for the process exit status, and say in the PR body which codes were and were not
present.

A web surface still needs the agent driver. Step 11 runs the pipeline only, so on a web-only diff in
an unattended run `agent-incomplete` is expected, not a misconfiguration. A fresh worktree without
the real-stack binaries in `bin/` defers the web surface `env-unavailable` (exit 0), with the doctor
detail naming each missing binary; treat a web still as unobtainable unattended rather than building
binaries solely to chase it.

An attached pane cannot be captured. The runner drives the TUI over the NDJSON bridge; once the
program hands the terminal to a wrapped process with `tea.Exec`, the bridge stops being read, so an
attached pane can be neither driven nor asserted. A committed scenario must stop strictly before the
attach, and a plan-time required-proof bullet naming an attached pane is unsatisfiable and must be
declared so at plan time rather than attempted.

A pipeline error at the upload stage is worth one plain re-run. After the proof CLI's bounded retry,
a surviving upload failure can still be transient edge trouble; re-run once, unmodified, before
treating the note as a proof tooling defect needing a human.

`clamped` is where a run learns its capture is not real evidence. The judge records which honesty
rules fired in the manifest's `clamped` array; entries such as `evidence-downgraded-scene-failure`,
`stub-caveat-appended`, or `confidence-downgraded` describe a downgraded stub, not a demonstration.
Never cite such a run as proof the change works.

An unproducible required-proof bullet is struck, with the reason. When a bullet cannot exist on this
branch at all — the surface is architecturally uncapturable, or producing it would require inventing
behavior the product does not have — strike it in the committed plan with the reason and name the
covering test in the PR body. Never fabricate the capture, and never route to BLOCKED for it.
Distinguish this from a bullet that merely duplicates an already-shipped scenario: cite the existing
scenario and state the delta the new evidence adds.

## Environment is daemon-injected

The upload credentials and proof API key (the `PROOF_ANTHROPIC_API_KEY`, `CLOUDFLARE_*`, and
`BOSS_PROOF_*` variables) are injected into the managed session environment by the daemon — you do
**not** source `.env` or export anything by hand. To see which prerequisites are present or missing,
run:

```bash
node scripts/proof.mjs doctor
```

It prints one set/MISSING line per prerequisite (never the values). If something is missing, that is
not a reason to hand-write a note — the proof run reports the same gap as an `env-unavailable` note
with the doctor output embedded, so a human can provision it.

## Running it

```bash
node scripts/proof.mjs plan                         # read `recipeLeg`, `order`, `forcedNoSurface`
node scripts/proof.mjs run                          # capture exactly what `plan` reported
node scripts/proof.mjs run --recipe <id> --recipe <id>   # narrow to specific recipe ids
```

There is no "default preset". The recipe catalog (`proof/recipes/default.json`) maps changed paths to
recipe ids through its `pathRules`; `plan` lists every match in `recipes` as `{id, surface, title}`.
Not every match is captured: a surface `order` names is proved **live by the agent**, so its recipes
are deliberately left to that agent. `recipeLeg` names exactly the recipe ids a bare `run` captures
through the recipe leg, and `order` the agent surfaces it drives — those are what `run` must account
for, captured or named in the per-surface summary with a reason code. Pass `--recipe` only to narrow,
for example to re-run one failing capture. The TUI surface is proved by a committed
`proof/scenarios/*.scenario.json`, not by `--recipe`.

When `plan` prints an empty `recipeLeg` and an empty `order`, or `forcedNoSurface: true`, the change
has no capturable surface — an expected outcome, not a failure to work around. One bare `run` posts
its own honest note, and that note is the terminal proof outcome: no further runs, and cite the diff
and tests as the evidence. Do **not** invent a recipe id, pass an unrelated one to produce a video,
or hand-write a proof note. If a page you actually changed
has no matching path rule, the gap is a missing entry in the catalog: fix it as its own change
rather than capturing something unrelated to stand in for it.

`run` performs the doctor preflight itself, scoped to the selected surface, and posts the appropriate
structured note. Do not run the finalize sequence here (it already ran in Steps 8–9). Never change
the terminal state, and respect proof's own privacy refusals.

## Scenario authoring (TUI) — required for a TUI-touching PR

A scenario is owed when `node scripts/proof.mjs plan` reports `surfaces.tui: true` **and** the change
alters what a TUI screen shows; such a PR **must commit a `proof/scenarios/*.scenario.json`**
demonstrating its specific change. Do not judge this from path prefixes yourself — `plan` already
applies them (a contract, generated-client or test-only edit raises no TUI surface on its own). The scenario is the deterministic, LLM-free replacement for the
flaky live-agent TUI capture: it drives the real TUI through named scenes over the NDJSON bridge and
asserts what each settled screen shows — so the proof is reproducible with **no** `PROOF_ANTHROPIC_API_KEY`.

If a TUI-visible diff ships without one, the outcome depends on the TUI agent: keyless (or recipe
mode) it is **scenario-missing** (exit 1); with a key the agent leg runs and may defer
**no-ui-surface** (exit 0) or upload a downgraded stub, with the honest signal only in the manifest's
`clamped` array. Neither makes a scenario-less TUI PR safe, so author the scenario now, as part of
the change. A scenario gates **only its own PR** — never add path rules, and never edit another PR's
scenario.

**Declining a scenario.** When `surfaces.tui` is true but the diff changes no rendered output —
argument threading, comments, startup wiring — do not author a filler scenario. State that reasoning
in the PR body and accept whatever outcome the pipeline posts; proof never gates finalization.

Author it and iterate to green **before** finalize:

```bash
node scripts/proof.mjs scenario validate proof/scenarios/<slug>.scenario.json   # shape + matcher errors, pointerful
node scripts/proof.mjs scenario run proof/scenarios/<slug>.scenario.json --dry-run   # local replay: cast/video/manifest, no upload, no API key
```

Loop `validate` → `run --dry-run` until the scenes pass, then commit the file in this PR. The schema,
a worked example, and the full step/matcher vocabulary live in-repo — read them before authoring:

- `proof/scenarios/README.md` and `proof/scenarios/schema.json` — the v1 document shape.
- `scripts/testdata/scenario-fixtures/valid-full.json` — a valid scenario exercising every step kind
  and expectation form.

Shape essentials (see the README for the authoritative detail):

- **fixture** — optional `{ preset, seed, env }`; `preset` is a named world (loader defaults to
  `demo`), `seed` is opaque state, `env` is a string→string map. Pick the preset that best stages your
  change; verify available preset names against the loader before relying on a non-default one.
- **scenes** — 1..4 named scenes, each a list of input steps. Step ops: `key` · `type` · `waitFor`
  (+ `timeoutMs`) · `waitMs` · `daemon` (an action object) · `expect`. Each step is exactly one op
  (plus an optional `caption` used as the on-screen chapter label).
- **expect** — a bare string (`normalized` match), an object `{ text, match?, label? }` with `match`
  one of `literal | normalized | normalized-ci | regex` (default `normalized`), or an `anyOf` group
  that passes if any alternative matches. Prefer `normalized` for terminal text; use `normalized-ci`
  only when case genuinely varies.

Keep expectations tied to text your change actually makes visible — that assertion is what makes the
video honest evidence rather than filler.
