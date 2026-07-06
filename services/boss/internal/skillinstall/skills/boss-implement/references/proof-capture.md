# Proof capture (Step 11) — full gate detail

Read this when Step 11 of `SKILL.md` fires — i.e. on the `REVIEW_READY` path (green, ready PR, ticket
already moved to **In Review**) and only then. Skip entirely for `BLOCKED`, draft, and `NO_CHANGE`.
This step may **never** change the terminal state: proof is best-effort and every outcome below is
recorded and ignored, never routed to BLOCKED.

## The single proof channel

`node scripts/proof.mjs run` posts its own PR comment, and its structured deferred note is the **only**
proof channel. A session **never** hand-writes skip prose or a "proof skipped: …" one-line note, and
never invents its own "no recipe matched → write a PR note" fallback. When proof cannot run — no UI
surface, a missing prerequisite, or a pipeline bug — you still run `node scripts/proof.mjs run` and let
it post the honest structured note. It classifies the outcome for you:

- **no-ui-surface** — the change has nothing to render; the note says so and exits 0.
- **env-unavailable** — a required key or toolchain is missing; the note embeds the doctor report
  naming exactly what is absent, and exits 0 so a human can provision it.
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
