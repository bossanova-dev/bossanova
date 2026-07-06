---
name: boss-proof
description: Capture proof-of-implementation screenshots for TUI, marketing, docs, or web UI changes and post them to the current GitHub PR. Internal Bossanova project skill.
---

# BS Proof

Use this skill when the user asks for implementation proof, screenshots, visual evidence, or PR proof comments.

This skill is internal to the Bossanova project and is not installed for boss users.

This skill is optional. Do not run boss-finalize from this skill. Do not block finalization when proof is absent.

## Single proof channel

`node scripts/proof.mjs run`'s own PR comment — its structured deferred note — is the **only** proof
channel. Never hand-write skip prose or a "proof skipped: …" one-line note, and never invent a "no
recipe matched → write a PR note" fallback. When proof cannot run (no UI surface, a missing
prerequisite, or a pipeline bug), run `node scripts/proof.mjs run` anyway and let it post the honest
structured note. The outcome classes and their process exit codes are:

- **no-ui-surface** — nothing to render; note posted, exit 0.
- **env-unavailable** — a required key or toolchain is missing; the note embeds the `doctor` report
  naming what is absent, exit 0.
- **agent-incomplete** — a real surface the agent could not demonstrate; exit 1 (internal retry signal).
- **pipeline-error** — a proof-pipeline bug (render/encode/bridge crash); the note names the failing
  stage and never blames the environment; exit 1 (internal retry signal).

None of these exit codes gate finalization — the posted note, never hand-written prose, is the source
of truth for reviewers.

## Workflow

The proof scripts live at the repo-root `scripts/` directory (`scripts/proof*.mjs`), not inside this skill folder. Run all commands below from the repo root.

- **Leave no local artifacts.** At every terminal state, discard the scratch you created (gitignored dirs, seeded design docs, `mktemp` files) so the worktree is clean. This holds in all modes; headless (`BOSS_CRON=true`) runs especially must self-clean. On a normal upload run `proof.mjs` removes its capture directory; if you used `BOSS_PROOF_UPLOAD=0`/`--dry-run` (which keeps the recipe directory), delete that directory before finishing.

**Preflight — ensure dependencies (best-effort).** The TUI PNG render shells out
to `pnpm --dir services/web exec …` and agent mode needs the root
`@anthropic-ai/sdk`, so a worktree provisioned without `node_modules` (e.g. a
cron/agent worktree whose setup script didn't run, or `run_setup_command` was
off) would otherwise force a proof skip. If the deps are missing, install them by
running the repo setup script — it installs the JS deps and also seeds `.env`,
which the upload step needs. `make setup-worktree` requires `REPO_DIR` and
`WORKTREE_DIR`; bossd sets these only when _it_ runs the setup script, so they
are absent in a chat and must be derived from git:

```bash
# Gate on services/web/node_modules — the specific dependency the TUI render
# needs (root node_modules alone is not enough); covers the root install too.
if [ ! -d node_modules ] || [ ! -d services/web/node_modules ]; then
  WORKTREE_DIR="$(git rev-parse --show-toplevel)"
  REPO_DIR="$(dirname "$(git rev-parse --path-format=absolute --git-common-dir)")"
  if [ "$REPO_DIR" != "$WORKTREE_DIR" ]; then
    REPO_DIR="$REPO_DIR" WORKTREE_DIR="$WORKTREE_DIR" make setup-worktree
  else
    pnpm install --frozen-lockfile   # already in the main checkout, not a worktree
  fi
fi
```

This is best-effort: if it fails (no `pnpm`, lockfile drift), proceed anyway —
proof stays non-fatal and will skip cleanly rather than blocking.

1. Inspect the current diff and requested proof scope.
2. Preview selected recipes:

   ```bash
   node scripts/proof.mjs plan
   ```

3. If the user requested specific shots, pass recipe ids explicitly (web or
   marketing recipe ids only — the TUI surface is agent-only and cannot be
   `--recipe`-selected):

   ```bash
   node scripts/proof.mjs plan --recipe web-sessions
   ```

4. Before uploading, confirm the upload environment is present. In a managed
   session the daemon injects the upload credentials and proof API key
   (`PROOF_ANTHROPIC_API_KEY`, `CLOUDFLARE_*`, `BOSS_PROOF_*`) — do **not** source
   `.env` by hand. Run the doctor to see which prerequisites are present or
   missing (one set/MISSING line each, never the values):

   ```bash
   node scripts/proof.mjs doctor
   ```

5. Capture, upload, and comment:

   ```bash
   node scripts/proof.mjs run
   ```

6. For local-only validation without upload:

   ```bash
   BOSS_PROOF_UPLOAD=0 node scripts/proof.mjs run --dry-run
   ```

   To trial an experimental recipe set without editing the committed catalog,
   point `BOSS_PROOF_CATALOG` at an alternate `recipes.json` (absolute, or
   relative to the cwd):

   ```bash
   BOSS_PROOF_CATALOG=/tmp/recipes.json BOSS_PROOF_UPLOAD=0 \
     node scripts/proof.mjs run --dry-run --recipe my-experiment
   ```

   The committed catalog (`proof/recipes/default.json`) uses `pathRules` to
   map changed-file prefixes to recipe sets, covering web/marketing/docs
   surfaces. The TUI surface has no catalog recipes — TUI changed-file prefixes
   (`services/boss/`, `proto/`) route to the **agentic TUI proof path**
   (via `classifyTuiSurface` in `proof-surfaces.mjs`), independent of the catalog.

   **Declaring a custom browser surface (consumer extension).** A consuming repo
   can add its own Playwright-captured proof surface purely through its
   `BOSS_PROOF_CATALOG` file — no core edits — by adding a top-level `surfaces`
   array. Each entry overlays the built-ins (`BUILTIN_SURFACES` in
   `proof-surfaces.mjs`) by `name`, field-by-field:

   | field                   | meaning                                                                                              |
   | ----------------------- | ---------------------------------------------------------------------------------------------------- |
   | `name`                  | surface id used in `recipe.surface` / `pathRules` (slug: `^[a-z0-9][a-z0-9-]*$`)                     |
   | `kind`                  | `"browser"` (Playwright-driven site surface)                                                         |
   | `serviceDir`            | package that owns the surface's `playwright.config.ts` — its `webServer` **is** the build+serve hook |
   | `specRoot`              | dir (relative to `serviceDir`) the runner writes its generated proof spec into                       |
   | `defaultCropToSelector` | crop the video pipeline falls back to when a recipe sets no `cropToSelector`                         |
   | `stageEnv`              | optional extra env the runner exports before Playwright (e.g. web's `{ "VITE_E2E": "1" }`)           |

   The `serviceDir`'s `playwright.config.ts` `webServer` owns build+serve — the
   descriptor only names which config owns the surface. Example: a `portal`
   surface served from `apps/portal`:

   ```json
   {
     "version": 1,
     "surfaces": [
       {
         "name": "portal",
         "kind": "browser",
         "serviceDir": "apps/portal",
         "specRoot": "e2e",
         "defaultCropToSelector": "main"
       }
     ],
     "recipes": [
       {
         "id": "portal-home",
         "surface": "portal",
         "route": "/",
         "viewport": { "width": 1440, "height": 1000 }
       }
     ],
     "pathRules": [{ "name": "Portal", "patterns": ["apps/portal/"], "recipeIds": ["portal-home"] }]
   }
   ```

## Required Environment For Upload

In a managed session the daemon injects these into the environment — do **not**
source `.env` by hand. Run `node scripts/proof.mjs doctor` to confirm which are
present or missing (set/MISSING per key, never the values); when a credential is
absent the Wrangler R2 upload fails with a `403: Forbidden`, and `proof.mjs run`
posts an honest `env-unavailable` note embedding the doctor report rather than a
hand-written skip line.

- `BOSS_PROOF_R2_BUCKET`: R2 bucket name, e.g. `bossanova-proof-production`
- `BOSS_PROOF_PUBLIC_BASE_URL`: public base URL, e.g. `https://proof.bossanova.dev`
- `CLOUDFLARE_API_TOKEN`: R2-write-scoped token Wrangler uses to upload
- `CLOUDFLARE_ACCOUNT_ID`: Cloudflare account that owns the bucket
- Authenticated `gh`
- The public base URL's R2 custom domain must already be attached to the bucket
  in Cloudflare. Terraform creates the bucket but does not manage the custom
  domain while the Cloudflare v4 provider is in use.

## Video recipes

Browser (web/marketing) recipes can capture a short **video** instead of a
still. Set `"capture": "video"` and drive the flow with a `steps[]` action
sequence instead of a single `route`:

```json
{
  "id": "web-new-session-flow",
  "surface": "web",
  "capture": "video",
  "viewport": { "width": 1440, "height": 1000 },
  "steps": [
    { "action": "goto", "route": "/" },
    { "action": "wait", "selector": ".data-table-wrap" },
    { "action": "click", "selector": ".data-table-wrap .row-clickable" },
    { "action": "wait", "timeoutMs": 800 }
  ]
}
```

Supported step actions: `goto` (`route`), `click` (`selector`), `type`
(`selector`, `value`), `wait` (`selector` to wait for visibility, or
`timeoutMs`), and `scroll` (`toSelector` for smooth `scrollIntoView`, or `byPx`
for smooth `scrollBy`). The runner records the flow with Playwright's native
`recordVideo` and produces a condensed `.mp4` plus a poster `.png` — no extra
dependency beyond ffmpeg (see below).

**Followability affordances** — paced actions, on-screen captions, and click
ripples make the recorded mp4 easy to follow without a voiceover:

- **`caption`** (per step, string) — an on-screen subtitle shown as an overlay
  during that step. Author-written, world-readable text; never put secrets here.
- **`scroll`** action — smoothly scrolls the page. Provide `toSelector` (smooth
  `scrollIntoView`) or `byPx` (smooth `scrollBy`); at least one is required.
- **`slowMo`** (per recipe, integer ms; default 350) — paces clicks and typing so
  the video is followable. Set at the recipe level alongside `capture`/`viewport`.

**Agent-mode captions (web and TUI):** In web **agent** mode the proof agent calls
a dedicated `caption` tool before each navigation action; this injects a top-anchored
DOM overlay onto the live page so the caption is visible in every screenshot and
video frame, and the same text feeds the run summary. In **TUI** agent frames
(rendered by `scripts/proof-render-terminal.mjs`) a blue caption line appears at the
top of the frame carrying the agent's per-step narration; it is absent when no
narration is set. Recipe-mode per-step `caption` values (the bullet above) are also
now rendered at the top rather than the bottom, keeping both surfaces consistent.
Caption and narration text are world-readable and uploaded to the public proof
bucket as-is — never put secrets in caption or narration text.

### Browser video post-processing (mp4 pipeline)

After the Playwright recording finishes, the pipeline automatically post-processes
the raw `.webm` into a polished `.mp4` via `scripts/proof-video.mjs`:

1. **Leading white-flash trim** — the browser's blank pre-roll is detected and
   removed so `0:00` is the first real app frame.
2. **Elapsed timer** — a `mm:ss` overlay is burned top-right on the original
   timeline (rendered in pure Node; no libfreetype required).
3. **Idle-section speedup** — stretches of 8+ seconds with no visible change are
   compressed to ~4s at a variable speed; the timer visibly fast-forwards through
   them and shows a `>>` marker.
4. **Branded intro card** — a ~2s intro card showing the PR `repo#num` + title
   is prepended via `scripts/proof-video-intro.mjs`.
5. **Play-button poster** — the final-frame screenshot receives a YouTube-style
   play-button composite (`scripts/proof-poster.mjs`,
   `scripts/assets/youtube-play-button.png`).

The manifest records `mediaType: "mp4"` with `videoUrl` and `posterUrl`. The
gallery README renders the video as a clickable poster thumbnail:
`[![poster](poster.png)](video.mp4)` with a `▶ Video` caption. PNG and GIF
artifacts render inline in the gallery. The PR comment links to the gallery
instead of embedding media.

**ffmpeg/ffprobe requirement:** `ffmpeg` and `ffprobe` must be installed for
browser-video recipes and for TUI video/agent video post-processing
(`brew install ffmpeg`). Missing `agg` causes TUI agent video captures to degrade to stills-only
proof with an install hint, but `finishVideo` still needs ffmpeg when a video is
produced. A video proof run on a machine without ffmpeg fails fast with a clear
install hint; it does not affect still-only surfaces in the same run.

**Optional hardware encode:** set `BOSS_PROOF_HW_ENCODE=1` to encode the proof
video with Apple-Silicon VideoToolbox (`h264_videotoolbox`) instead of libx264.
It is opt-in and self-gating — used only when ffmpeg actually advertises the
encoder, so the default and every non-macOS environment (e.g. Linux CI) stay on
portable libx264. Output is plain H.264 in mp4, byte-different but equally
playable.

**Playwright browser requirement:** all browser (web/marketing) recipes — still
and video — drive a headless Chromium via Playwright. On a fresh checkout the
browser binary is not present and the first run fails with
`browserType.launch: Executable doesn't exist`. Install it once with
`pnpm --dir services/web exec playwright install chromium`. The TUI agent also
uses headless Chromium for frame rasterization (see Agent-mode prerequisites
below) — it is not recipe-driven, but the Chromium binary is still required.

### Re-processing an existing recording

If you need to re-run post-processing (e.g. after tuning the analysis constants)
without capturing a new recording, use:

```bash
node scripts/proof-postprocess-video.mjs --proof-dir <recipeDir>
```

This finds the single `*.webm` in `<recipeDir>`, regenerates the mp4 and updates
the poster in place.

**Caveat:** the normal pipeline deletes the `.webm` after a successful run. The
webm is only preserved when the proof run was executed with `BOSS_PROOF_UPLOAD=0`
(which skips the upload and keeps the full recipe directory intact). If the webm
is absent, run a fresh capture instead.

Video recipes are **fixture-only** by convention (like all recipes here) and
reuse the existing end-of-flow DOM-text secret scan — no per-frame scanning.
Existing still recipes are unchanged.

## PR comment format

The proof comment is intentionally **minimal**: it contains a gallery link (if a
report URL exists), the run title, one metadata line with commit hash, run ID,
and Gen-AI status, an optional collapsible agent summary, and an
Evidence/Confidence block. **No inline media**, no per-capture thumbnails, and
no `Verdict` line appear in the comment itself.

All visual content (captured images, videos, step-by-step stills) lives in the
**gallery README** — the target of the `📸 Proof gallery` link. That README
renders the full visual evidence with metadata. This separation keeps the GitHub
PR comment noise-free while preserving all proof details one click away.

### Example comment structure

The comment carries one `####` section per proven/deferred surface (a
single-surface run has exactly one section):

```
<!-- marker/CI footer -->

### [📸 Proof gallery](link-to-report)

**Implementation proof: compact mode**

**Commit:** `abc1234`  **Run:** `run-123`  **Gen-AI:** not live (UI-only demo)

#### TUI — ✅ proven

<collapsible agent summary>

✅ **Evidence:** Satisfactory

✅ **Confidence:** High

#### Web — ⏸ deferred (budget-exceeded)

This surface was deferred because the shared proof budget (~15min per run) was
consumed by an earlier surface… re-run proof for this surface alone to capture it.
```

## Fresh-context judge (BOS-141)

Agent proof runs are graded by a **fresh-context judge** before the comment is
rendered. It is a cheap vision model (default `claude-haiku-4-5`) given the
plan's `## Required proof` bullets, the brief's scenes, the per-scene mechanical
pass/fail, the driver's transcript summary, and a small set of representative
stills — but **no shared message history with the driver** that produced the
proof. It emits a structured verdict `{evidence, confidence, perScene, caveats}`.

**What it changes.** The comment **headline** derives from the judge grade
(`✅ judged convincing` / `⚠️ partially convincing` / `⚠️ produced but not
convincing` / `📸 unjudged`), the verdict block is replaced by a labeled
"Fresh-context judge (model)" block that discloses per-scene reasons and
caveats, and an unsatisfactory grade applies a `proof-invalid` PR label
(removed again on any later non-unsatisfactory run).

**What it does NOT change — advisory only (D6).** The judge never alters the
outcome class or the process exit code; those stay 100% mechanical (media
present, scenes passed). A judge failure (missing key, API error, timeout,
parse error) degrades to the existing self-graded block explicitly labeled
**"Self-graded (unjudged)"** with the reason — never a crashed or blocked run.
Honesty rules are clamped mechanically, not trusted to the model: any failed
scene caps the grade at `partial`, a stubbed agent-runner always carries the
stub caveat, and `Confidence: High` requires all scenes passed plus stub
disclosure.

**What it does NOT protect against (honest naming, D16).** It is a
fresh-_context_ judge, not a fresh-_provider_ one: same vendor, same
`PROOF_ANTHROPIC_API_KEY`. It catches self-serving summaries and unexamined
galleries — a driver claiming success no reviewer ever checked against the
stills — but NOT a bad brief (garbage-in), missing required-proof bullets, or
same-provider bias (a shared vendor blind spot could affect driver and judge
alike). Author-intent bullets (P4c) and the adversarial eval (D9) are the
mitigations for those, not the judge itself.

**Knobs.** `BOSS_PROOF_JUDGE_MODEL` overrides the model;
`BOSS_PROOF_JUDGE_TIMEOUT_MS` overrides the 120s per-call timeout. Both are
optional; the judge uses the same key the drivers use, so no new secret is
required.

## TUI video behavior

When the TUI **agent** records video (via `agg` + asciinema post-processing),
the clip receives the same polished proof-video treatment as browser videos:

1. **Branded intro card** — a ~2s intro card shows the PR repo#num + title,
   rendered via `proof-video-intro.mjs`. This provides immediate context without
   requiring the viewer to click back to the gallery README.
2. **Burned-in timer** — a `mm:ss` timer is burned into the video.
3. **Burned-in per-step captions** — the agent's per-step narration (the same
   blue caption bar shown in the `frame-NN.png` stills, via the shared bar markup
   in `proof-render-terminal.mjs`) is burned into the video, timed to each step
   from the asciinema `.cast` clock (`raw/caption-timings.json`). The strip sits
   at the very top, clear of the top-right timer pill. Captions are additive and
   best-effort: missing timings or a strip-render failure degrades to the same
   no-caption video and never fails the proof.
4. **Idle-section speed-up** — long idle stretches are compressed and marked
   with `>>`. Captions are burned before the speed-up, so a caption spanning a
   compressed stretch simply plays faster.
5. **Play-button poster** — the final-frame screenshot receives a YouTube-style
   play-button composite to signal "this is a video, click to play."
6. **Boot lead-in trim** — TUI agent videos have no white-flash pre-roll;
   only dark/blank boot lead-in is trimmed. Caption windows are offset by the
   same trim so they stay aligned with the trimmed timeline.

## Run cleanup

A successful proof run (real upload with no capture failures) **cleans up its
local `.proof/<run>` directory** after upload succeeds. Failed captures and
dry-run captures (`--dry-run`) preserve the full directory for debugging. The
browser `.webm` intermediate is deleted on all successful runs unless
`--dry-run` is active.

## Agent mode

When `PROOF_ANTHROPIC_API_KEY` is set, `node scripts/proof.mjs run` automatically
switches to **agent mode**: an Anthropic model navigates the real running stack,
decides what to verify, and records stills and, when video prerequisites are
present, a video of what it sees — rather than replaying a fixed recipe
sequence. Agent mode covers both the **web** and **TUI** surfaces; a TUI-surface
PR auto-selects the TUI agent in the same way a web-surface PR selects the web
agent.

**Marketing and docs changes never use the LLM agent.** They are static,
deterministic captures of known routes (`services/marketing` → the Astro site,
`services/docs` → the Docusaurus site), so even when an API key is present
`agentSurface` returns `recipe` for a change set whose recipes are all
`marketing`/`docs` and the run falls through to the recipe path. Each site owns
its Playwright config (`services/<svc>/playwright.config.ts`) which builds and
serves the site; the docs site is outside the pnpm workspace, so install its
deps standalone with `pnpm --dir services/docs install --ignore-workspace`. A
mixed PR that also touches `services/web` still uses the web agent.

### Multi-surface coverage (a PR that touches BOTH surfaces)

A PR that touches both the boss TUI and the web app proves **both** surfaces in
one run. Dispatch classifies the diff into a surface SET (`classifySurfaces` in
`proof-surfaces.mjs`) instead of picking a single winner, then runs each agent path
**sequentially** (never in parallel — both drivers share one ITPM-capped key)
under ONE shared wall-clock budget:

- The shared total defaults to **~15 minutes** and is overridable with
  `BOSS_PROOF_TOTAL_BUDGET_MS`. It is distinct from `BOSS_PROOF_AGENT_TIMEOUT_MS`,
  which is the per-runner outer SIGKILL backstop / per-SDK-call abort, not a run
  budget.
- Per-surface defaults mirror the runner defaults (TUI ~4min / web ~12min) with
  viability floors (TUI 2min / web 6min). Because 4+12 > 15, the planner clamps
  each surface's grant to what remains. A second surface that cannot fit its
  floor **defers with `budget-exceeded`** (exit 0, neutral) — re-run proof for
  that surface alone (`BOSS_PROOF_AGENT_SURFACE=<surface> node scripts/proof.mjs run`).
- **Ordering:** the surface named by the plan's `## Required proof` bullets runs
  FIRST; a tie (or a plan-less PR) breaks cheap-first (TUI before web).

Both surfaces are posted in **ONE consolidated PR comment** — a single marker,
one `####` section per surface (`✅ proven` or `⏸ deferred (<reason>)`). Partial
success is first-class ("web proven, TUI deferred (budget-exceeded)"); there is no
global ❌ verdict when at least one surface passed. `collapsePriorProofComments`
still leaves exactly one live comment per PR. Exit policy: a pipeline-error
anywhere or a **web** agent-incomplete exits 1; a **TUI** agent-incomplete and
every neutral deferral (env-unavailable / budget-exceeded / no-ui-surface) exit 0.

**Known limitation (surface classification is by file PATH).** A backend-only
change with a UI-visible effect (e.g. a bossd handler that alters what the web app
renders) touches no `services/web/src/` or TUI path, so it classifies as **no
surface**. Mitigation: a plan `## Required proof` bullet that NAMES a surface
FORCES it into the set — required-proof bullets are the primary brief source
precisely because file paths cannot see behavior. Write a bullet like "The
/account page renders the new field in the browser" to force the web agent to run
on a path-miss diff. (This limitation is also documented in the `classifySurfaces`
jsdoc in `scripts/proof-surfaces.mjs`; keep the two in sync.)

### Agent-mode prerequisites

- Run `pnpm install` at the repo root; this provides the `@anthropic-ai/sdk`
  model client used by the proof agent.
- Install `services/web` dependencies and Playwright Chromium with
  `pnpm --dir services/web exec playwright install chromium`; the TUI frame
  renderer rasterizes through headless Chromium.
- Install `agg` with
  `cargo install --git https://github.com/asciinema/agg`. When `agg` is
  missing, TUI agent video capture degrades to stills-only proof and prints an
  install hint.
- Export `PROOF_ANTHROPIC_API_KEY` with a low-privilege, spend-capped, rotatable key.
- The TUI agent bridge binary is built automatically by
  `node scripts/proof.mjs run` via `go build`. Set
  `BOSS_PROOF_TUI_BRIDGE_BIN` only to reuse a prebuilt bridge binary. A
  dependency-free cron worktree cannot run agent mode until deps are installed —
  the Preflight step above (`make setup-worktree`) provisions them.

**Cloud-access fixture (`BOSS_CLOUD_ACCESS_E2E_SEQUENCE=active`):**

The TUI agent bridge seeds `BOSS_CLOUD_ACCESS_E2E_SEQUENCE=active` so the
fixture daemon returns a healthy cloud-access status. This is now the only
TUI boot path (recipe stills and VHS video recordings have been removed):

- TUI agent bridge via `BOSS_CLOUD_ACCESS_E2E_SEQUENCE=active` set in
  `scripts/proof-tui-agent.mjs`.

Without this, the TUI home view shows a "Cloud access status unavailable"
banner and any recipe that navigates deeper (cron list, cron form, repo
settings) times out waiting for a screen that never renders.

Cron-worktree dependency installation (the remaining prerequisite for
fully headless agent mode) arrives via a separate setup-script PR, not via
this skill's runtime.

**Mode selection:**

| Condition                                                    | Mode                                    |
| ------------------------------------------------------------ | --------------------------------------- |
| `BOSS_PROOF_MODE=agent`                                      | agent (regardless of key or `--recipe`) |
| `BOSS_PROOF_MODE=recipe`                                     | recipe (regardless of key)              |
| No `BOSS_PROOF_MODE`, explicit `--recipe` flag               | recipe (recipe flag wins over key)      |
| No `BOSS_PROOF_MODE`, `PROOF_ANTHROPIC_API_KEY` set, no flag | agent                                   |
| No key, no mode, no `--recipe`                               | recipe (fallback)                       |

Note: an explicit `--recipe <id>` selection disables agent mode when no `BOSS_PROOF_MODE` is set — it does not override `BOSS_PROOF_MODE=agent`.

**TUI surface caveat:** For the TUI surface there is no recipe fallback. When
the TUI surface is detected and no usable agent is available (no
`PROOF_ANTHROPIC_API_KEY` and no `BOSS_PROOF_MODE=agent`), the run posts an
honest deferred comment and exits 0 — no media is produced. The "recipe
(fallback)" row in the table above does not apply to TUI.

**Web surface caveat (BOS-118):** In agent mode the web surface is **agent-only
with no recipe floor**. A failed or declined agent run posts an honest deferred
comment with the agent's own capture — never a gallery of generic per-route
recipe clips. Two extra honest outcomes:

- **No web UI surface → no agent run.** Before spending a ~12-minute agent run,
  `webUiSurfacePresent(changedFiles)` (path pre-gate, `WEB_UI_SURFACE_PREFIXES`)
  checks whether the change touches the Vite app's user-visible surface
  (`services/web/src/`, `index.html`, `public/`). A scripts-only, backend-only,
  or tests-only PR posts a `no-ui-surface` note and exits **0** — nothing is
  demonstrated because there is nothing to demonstrate.
- **Agent-declared no-surface backstop.** If the change touches `services/web/src/`
  but turns out to be non-visual (e.g. a pure refactor), the agent can call
  `done({passed:false, noSurface:true})`. The orchestrator then posts the same
  honest `no-ui-surface` note and exits 0 (a neutral skip, not a red failure).

A genuine agent failure (the change has a surface but the agent could not
demonstrate it) still defers with `agent-incomplete` and **exits 1** so the proof
check reflects that the change was not captured. The explicit `--recipe
web-sessions` path and the no-key recipe fallthrough are unchanged — they are for
manual/dev capture, not the production agent path.

**Operational note:** image-heavy web agent runs can 429 against a spend-capped
proof key (the org caps sonnet ITPM). The default model is now
`claude-haiku-4-5`, which stays under the cap; only override `BOSS_PROOF_MODEL`
to a larger model when a run genuinely needs it.

**Environment variables:**

- `PROOF_ANTHROPIC_API_KEY` — required for agent mode. Use a **low-privilege,
  spend-capped, rotatable** key (see Key-exfil risk below).
- `BOSS_PROOF_MODEL` — Anthropic model to use; default `claude-haiku-4-5`
  (the agent drive is text-only, so haiku fits, runs faster, and dodges the
  proof key's sonnet ITPM cap). Override to a larger model when needed.
- `BOSS_PROOF_BRIEF` — optional path to a pre-written `brief.json` that
  overrides the auto-generated brief. When absent, the brief is generated
  from the PR diff via `generateBriefFromDiff`.
- `BOSS_PROOF_MODE=agent|recipe` — force a specific mode, bypassing the
  key-presence check.
- `BOSS_PROOF_AGENT_SURFACE=tui|web` — hard-override the surface selected by
  catalog routing (see Surface routing below). Highest-precedence override.
- `BOSS_PROOF_AGENT_TIMEOUT_MS` — wall-clock budget for the agent drive, in
  milliseconds. Default `600000` (10 min). When the budget fires before the
  agent completes, the run yields a **degraded** (no-media or incomplete) result
  rather than throwing; the deferral ladder then takes over (see Agent time-box
  below).

> The standalone TUI navigation eval (`scripts/proof-tui-agent.eval.mjs`) keys off
> the raw **`ANTHROPIC_API_KEY`** (it calls the SDK directly), not
> `PROOF_ANTHROPIC_API_KEY`. With only `PROOF_ANTHROPIC_API_KEY` set, the eval
> prints `skipped: no ANTHROPIC_API_KEY` and does nothing. **Spend caution:**
> because it reads the broad `ANTHROPIC_API_KEY`, running the eval in a shell that
> already exports one will boot a real Go bridge + real model and spend against
> that key — run it deliberately, with a low-privilege, spend-capped key.

**Surface routing:**

The surface used by the agent is derived from
`agentSurface({catalog, changedFiles})` in `scripts/proof.mjs`. TUI is
detected first by **changed-file path** via `classifyTuiSurface`
(`proof-surfaces.mjs`), independent of the recipe catalog — a boss/TUI diff
matches zero catalog recipes but still routes to the agentic TUI proof path.
When no TUI prefix matches, `'recipe'` is returned when every catalog-matched
recipe is a deterministic static surface (`marketing`/`docs`); otherwise `'web'`
(product web, mixed dynamic surfaces, or no match). Override precedence,
highest first:

1. `BOSS_PROOF_AGENT_SURFACE=tui|web` — hard override.
2. The `surface` field in an explicit `BOSS_PROOF_BRIEF` file.
3. The path-classifier / catalog-derived result above.

**Agent time-box:**

`BOSS_PROOF_AGENT_TIMEOUT_MS` (default `600000` ms / 10 min) caps how long the
agent drive may run:

- **Web agent** (`proof-agent.mjs`): the Playwright subprocess is killed after
  the budget elapses (`runInterruptible`); the run continues with whatever
  captures completed so far, yielding a degraded (possibly no-media) result.
- **TUI agent** (`proof-tui-agent.mjs`): the Anthropic SDK call is guarded by
  an `AbortController`; on abort the SDK request resolves immediately with no
  captures, also yielding a degraded result.

In both cases the degraded result is handed to `finalizeAgentProof`, which
routes it through `renderDeferredComment` (see Deferral policy below) so a
neutral "Proof deferred" comment is posted and the ❌ Unsatisfactory verdict is
never shown for a timeout.

**Stubbed agent-runner limitation:**

The Playwright `agent-proof` project exercises the **UI, orchestration, and
persistence layers** of the real running stack. However, it runs with a
`bossd-plugin-claude` stub (`agentRunnerStubbed: true` in the manifest) — the
actual agent-runner subprocess is not launched during the proof run. A note
appears in the PR comment identifying this limitation.

**Agent-first with recipe-still floor (web only):**

The architecture is agent-first: when agent mode runs and succeeds, it produces
the proof. For the **web agent**, when a run is degraded (timeout, no media,
agent incomplete), the deferral ladder fires and posts a neutral comment; the
recipe still(s) from `proof/recipes/default.json` serve as the floor — at
minimum, a recipe still is captured so an image-only reviewer always has
something to view.

Recipes remain the default when no `PROOF_ANTHROPIC_API_KEY` is present for
web/marketing/docs surfaces. Agent mode is additive for web — both modes
produce a manifest + PR comment; the recipe path is unchanged.

**TUI is agent-only with no floor.** There is no recipe fallback for the TUI
surface. When the TUI agent is unavailable or fails, the run posts an honest
deferred comment and exits 0 — no media is produced. The recipe-still floor
does not apply to TUI.

**Guaranteed still:**

For the **web agent** (which records video), the orchestrator always produces
at least one still: if no `\d\d-*.png` screenshots were taken during the
session, the final frame is extracted from the recorded video as
`01-final-frame.png`. For the **TUI agent**, when it aborts or times out before
producing media, the run posts a neutral deferred comment — there is no
recipe-still floor to fall back to. When even the web recipe floor yields
nothing (e.g. tooling unavailable), a neutral deferred comment is posted with
no captures.

**Key-exfil risk (accepted):**

The LLM-generated brief and agent summary flow through the secret scan
(`classifySecretRisk`) before any upload — a high-risk hit aborts the run
before R2 or GitHub are touched. However, a sufficiently evasive output could
still bypass the pattern-based scan. Mitigate by using a **low-privilege,
spend-capped, rotatable** API key that can be revoked quickly if misuse is
detected. Do not use a production key or a key with broad organizational
permissions.

### Deferral policy

**A missing recipe is never grounds to defer a visual change.** When no recipe
or affordance exists for a visual change, this skill must author a recipe and/or
add the affordances it needs, commit them in the same run, then capture. Missing
fixtures, missing `data-testid` attributes, or incomplete routes are not
deferrals — they are things this skill is authorized to fix.

**This skill is authorized** to add fixture data, deterministic seeds, test
hooks, `data-testid` attributes, stable routes, and recipes — and to commit them
within the same run — in order to make a feature provable.

The only legitimate deferrals are:

- **(a)** Genuinely no visual surface: the change is pure backend or logic with
  nothing to render on screen.
- **(b)** Renderer or tooling unavailable: `agg` or `ffmpeg` is not installed
  on the machine.
- **(c)** No API key when agent mode is required. For the **TUI surface** this
  means any unavailability: the TUI surface has no recipe path at all, so a
  missing `PROOF_ANTHROPIC_API_KEY` always defers.
- **(d)** An irreducibly nondeterministic flow that cannot be made reproducible
  through seeding, fixtures, or test hooks.

**Deferred comment behavior (neutral, never ❌):**

When a run is degraded — the agent did not pass OR produced no media — it is
routed through `renderDeferredComment` in `proof-lib.mjs` and posts a neutral
"Proof deferred" note. The ❌ Evidence/Confidence "Unsatisfactory" verdict block
is **never** posted for an environment limitation. Specifically:

- `manifest.deferred = true` is set.
- `reasonCode` names the outcome class: `'no-ui-surface'` (nothing to render,
  exit 0), `'env-unavailable'` (a required key/toolchain is missing, exit 0,
  doctor report embedded), `'agent-incomplete'` (a real surface the agent could
  not demonstrate, exit 1), or `'pipeline-error'` (a proof-pipeline bug —
  render/encode/bridge crash — that names the failing stage and never blames the
  environment, exit 1). `'no-media'` remains for a degraded run that produced no
  captures.
- The single post point is `finalizeAgentProof` in
  `scripts/proof-agent-finalize.mjs`.

A normal **passing** run with at least one captured image still shows the ✅
verdict via `renderComment`.

**Exit-code semantics:**

boss-proof's process exit code is **not a PR gate** and must never block
finalization. The honest outcome classes exit `env-unavailable` → 0 and
`no-ui-surface` → 0 (nothing to fix in the change), while `agent-incomplete` → 1
and `pipeline-error` → 1 signal a genuine gap (agent non-completion, or our own
pipeline bug) — a non-zero exit is an internal retry signal for the invoking
cron or agent only; the docs-only path exits 0. In all cases the neutral PR
comment — never a ❌ for an environment limitation — is the source of truth for
human reviewers.

### Docs-only changes

When `isDocsOnlyChange(changedFiles)` returns true but no proof recipe matched,
the web agent is skipped entirely and a neutral docs build-check note is posted
via `renderDeferredComment` with `reasonCode: 'docs-build-check'`. The
recapture hint is `pnpm --dir services/docs build`. No ❌ verdict is shown; the
comment is labeled a build check, not an implementation proof.

`services/docs` changes normally match Docusaurus recipes (`docs-home`, and
`docs-mcp-guide` for `services/docs/docs/guides/mcp*`) and therefore continue to
the deterministic recipe path instead of posting only a build-check note.
This branch is handled before any agent dispatch in `proof.mjs`.
`isDocsOnlyChange` and `shouldPostDocsBuildCheck` are exported for unit testing.

## Multi-scene briefs (BOS-140)

A brief can describe a proof as multiple **scenes** — distinct user-visible
flows captured back-to-back in ONE continuous recording per surface, chaptered
by scene instead of split into separate videos.

**Schema:** `brief.scenes[]` is an array of 1–4 entries, each
`{id, title, stepsHints?, expectedEvidence[]}`. `id` and `stepsHints` are
optional (a missing `id` defaults to `scene-01`, `scene-02`, …); `title` and
`expectedEvidence` are required. More than 4 scenes is a validation error, not
a silent clamp — split the flow further instead. **Scene-less back-compat:** a
brief with no `scenes[]` (every pre-BOS-140 brief, and any brief that only sets
top-level `expectedEvidence`) is treated as ONE synthetic scene, so all
downstream gate and render logic has exactly one code path.

**`begin_scene(id)` protocol:** both the TUI and web agents expose a
`begin_scene` tool. The agent calls it once per scene, in order, before that
scene's first action; scene 1 is implicitly active from the start, so a model
that never calls it degrades gracefully to single-scene attribution. Each call
burns a `Scene N — <title>` caption into the recording at that moment (TUI: the
existing blue caption bar, timed off the asciinema cast clock; web: the overlay
caption, timed off a wall-clock marker) — this is how a viewer sees the
chapter boundary land in the video itself. An out-of-order or unknown scene id
is rejected without changing state; the model is expected to self-correct.

**Per-scene evidence gate:** each scene's `expectedEvidence` is checked only
against what happened while that scene was active — a scene no longer needs to
leave its evidence on the final screen to pass.

- **TUI:** an evidence substring passes when it appears on ANY settled screen
  captured within the scene's window (from its `begin_scene` marker to the
  next one, or the end of the run for the last scene). This replaces the old
  final-screen-only gate.
- **Web:** a scene passes when it has at least one interaction AND its
  evidence substrings are found in a DOM-text snapshot taken after some action
  within the scene's window.
- Evidence stays a literal, case-sensitive substring match against SHORT
  on-screen tokens (a button label, a status word) — never a sentence and
  never fuzzy matching. Scenes are graded independently: one scene failing
  does not fail the others, and the outcome reports pass/fail per scene.

**Chaptered comment and gallery:** a multi-scene capture's consolidated PR
comment gains a `**Scenes:**` block per surface, one line per scene —
`[m:ss] Scene N — title`, with the timestamp linking into that surface's mp4
via a media-fragment `#t=<sec>` URL, each line marked ✅ or ✗ (a ✗ line also
lists its missing evidence). The linked gallery groups stills under a
`### Scene N — title` heading per scene instead of one flat step list. A
single-scene capture that fully passes renders exactly as it did before this
feature — no chapter noise on the common case.

## Live-agent scenes (BOS-142)

A scene may set `scene.liveAgent: true` to run the web proof demo against the
**real** `bossd-plugin-claude` — a genuine Claude session driven live in the
web UI — instead of the default stub runner.

**Opt-in, never model-settable.** A live scene is opt-in ONLY via a
hand-authored `BOSS_PROOF_BRIEF` file that sets `scene.liveAgent:true`. It is
absent from the model-facing brief schema and stripped from generated briefs, so
a model can never turn it on — which is also why a plan's `## Required proof`
`live-agent` marker does NOT by itself run a live scene: a generated brief has no
scene to drive, so booting the real runner off the marker alone would spend with
nothing to demonstrate. That marker only WIDENS the shared wall-clock budget (a
live scene is slower than the stub); the authored `scene.liveAgent:true` flag is
the sole switch that actually runs one. **Max ONE live scene per brief** — the
rest run stubbed as usual.

**Prereqs (doctor-gated).** A live scene runs only when ALL of these are present,
otherwise that one scene defers `env-unavailable` naming the missing item while
every other scene still runs: the `bossd-plugin-claude` binary built (`make
plugins`), `claude` on PATH, `tmux` on PATH, and `PROOF_ANTHROPIC_API_KEY` set.
The orchestrator signals the run via `BOSS_PROOF_LIVE_AGENT=1` — never set that
by hand in a normal run.

**Stub mode is the default and is hardened.** The harness strips
`ANTHROPIC_API_KEY` / `PROOF_ANTHROPIC_API_KEY` / `CLAUDE_*` from bossd's env in
BOTH modes (closing an ambient-key leak); stub mode carries no claude plugin
entry and never dispatches a real runner.

**Spend-cap posture (read this honestly — no dollar metering exists anywhere in
this stack, do not imply one does):**

> No dollar/token/turn cap exists anywhere in this stack — StartAgentRunRequest
> carries no budget fields and there is no metering API. A hard dollar cap is
> not feasible without proto/daemon changes (out of scope). The honest per-run
> spend cap is input + duration bounding with a guaranteed kill, all mechanical:
> (1) dedicated key — the live session authenticates ONLY via ANTHROPIC_API_KEY
> mapped from PROOF_ANTHROPIC_API_KEY at bossd spawn (the org-capped proof key;
> the developer's ambient key is stripped); (2) one live scene per brief, one
> short fixed prompt, in a tiny fixture repo; (3) cheap model pin where the
> session surface exposes model selection; (4) an extended wall-clock scene
> budget (LIVE_AGENT_EXTRA_MS) after which the scene fails and the driver stops
> the session — plus a teardown tmux sweep that is the guaranteed stop (a live
> pane outlives bossd). Never claim a dollar cap exists.

**Evidence gate is stable chrome only.** The chat pane is a `<canvas>`, invisible
to DOM-text, so a live scene gates on stable chrome: the fixed session name, the
running/working status spinner (the stub completes synchronously and ends
`stopped`, so the spinner is what discriminates a genuinely-running agent), and
the `N attached` indicator. Never gate on model prose.

## Privacy

Screenshots upload to a **public** bucket. The automated secret scan inspects
**rendered DOM text only** — it cannot read pixels drawn to a `<canvas>`,
background images, or `<svg>`. Recipes that screenshot canvas surfaces (e.g. the
xterm terminal) must set `"canvas": true` and use `"privacy": "fixture"`; the
runner refuses to capture a live canvas recipe.

Live captures are allowed for DOM-text surfaces, and every PR comment labels
public live captures. Do not upload screenshots if the capture visibly contains
passwords, tokens, private keys, customer data, or private repository contents
unrelated to the PR.
