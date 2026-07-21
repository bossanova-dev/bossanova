# Proof capture (Step 11) and TUI scenario authoring (Step 5)

For a TUI diff, read **Scenario authoring (TUI)** during Step 5 and complete its validation loop before
Step 6. Read the remaining sections only when Step 11 of `SKILL.md` fires — on the `REVIEW_READY` path
(green, ready PR, ticket already moved to **In Review**). Skip Step 11 entirely for `BLOCKED`, draft,
and `NO_CHANGE`. Step 11 may **never** change the terminal state: proof is best-effort and every
outcome below is recorded and ignored, never routed to BLOCKED.

## The single proof channel

`node scripts/proof.mjs run` posts its own PR comment, and its structured deferred note is the **only**
proof channel. A session **never** hand-writes skip prose or a "proof skipped: …" one-line note, and
never invents its own "no recipe matched → write a PR note" fallback. When proof cannot run — no UI
surface, a missing prerequisite, or a pipeline bug — you still run `node scripts/proof.mjs run` and let
it post the honest structured note. It classifies the outcome for you:

- **no-ui-surface** — the change has nothing to render; the note says so and exits 0.
- **env-unavailable** — a required key or toolchain is missing; the note embeds the doctor report
  naming exactly what is absent, and exits 0 so a human can provision it.
- **scenario-missing** — a TUI change shipped without a committed `proof/scenarios/*.scenario.json`
  demonstrating it; the note names the missing scenario and exits 1 — proof is required for TUI.
  Author a scenario (below) so the deterministic TUI proof can run.
- **pipeline-error** — a proof-pipeline bug (render/encode/bridge crash); the note names the failing
  stage and never blames the environment; exits 1 as an internal retry signal only.
- **agent-incomplete** — a real surface the agent could not demonstrate; exits 1 (internal signal).

None of these exit codes gate finalization — Steps 8–9 already ran. The posted note, never any hand-written
prose, is the source of truth for reviewers.

## Environment is daemon-injected

The upload credentials and proof API key (the `PROOF_ANTHROPIC_API_KEY`, `CLOUDFLARE_*`, and
`BOSS_PROOF_*` variables) are injected into the managed session environment by the daemon — you do
**not** source `.env` or export anything by hand. To see which prerequisites are present or missing,
run:

```bash
node scripts/proof.mjs doctor
```

It prints one set/MISSING line per prerequisite (never the values). If something is missing, that is
not a reason to hand-write a note — `node scripts/proof.mjs run` reports the same gap as an
`env-unavailable` note with the doctor output embedded, so a human can provision it.

## Running it

```bash
node scripts/proof.mjs plan   # classify the changed surface (optional preview)
node scripts/proof.mjs run    # capture, upload, and post the single proof comment
```

`run` performs the doctor preflight itself, scoped to the surface it classified, and posts the
appropriate structured note. Do not run the finalize sequence here (it already ran in Steps 8–9). Never
change the terminal state, and respect proof's own privacy refusals.

## Scenario authoring (TUI) — required for a TUI-touching PR

Any PR that touches the TUI surface (`services/boss/internal/views/`, `tuidriver/`, `client/`,
`cmd/`, `proto/`, and the other TUI prefixes) **must commit a `proof/scenarios/*.scenario.json`**
demonstrating its specific change. The scenario is the deterministic, LLM-free replacement for the
flaky live-agent TUI capture: it drives the real TUI through named scenes over the NDJSON bridge and
asserts what each settled screen shows — so the proof is reproducible with **no** `PROOF_ANTHROPIC_API_KEY`.

If the diff ships without one, the pipeline posts a single **scenario-missing** deferred note and
exits 1 — proof is required for TUI — so author the scenario now, as part of
the change, not as an afterthought. A scenario gates **only its own PR** — never add path rules, and
never edit another PR's scenario.

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
