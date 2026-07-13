#!/usr/bin/env node

import path from 'node:path'

import { MAX_CAPTION } from './proof-caption-spec.mjs'
import { surfaceLabel } from './proof-finalize-outcome.mjs'

// Single source of truth for proof artifact media types. Adding a new
// uploadable artifact type is one entry here — the upload content-type, the
// manifest URL encoding, and the path validator all derive from this map.
export const PROOF_MEDIA_TYPES = {
  png: 'image/png',
  webm: 'video/webm',
  gif: 'image/gif',
  mp4: 'video/mp4',
}

/** True when the media type is a video (webm or mp4). */
function isVideoMediaType(mt) {
  return mt === 'webm' || mt === 'mp4'
}

export function mediaTypeForPath(fileName) {
  const ext = String(fileName ?? '')
    .toLowerCase()
    .split('.')
    .pop()
  const contentType = PROOF_MEDIA_TYPES[ext]
  if (!contentType) {
    throw new Error(`unsupported proof media extension: ${ext || '<none>'}`)
  }
  return contentType
}

export function normalizeChangedFiles(files) {
  return files
    .filter((file) => file !== null && file !== undefined)
    .map((file) => String(file).trim().replaceAll('\\', '/').replace(/^\.\//, ''))
    .filter(Boolean)
}

/**
 * Generic pure prefix-match: true when ANY changed file (after
 * normalizeChangedFiles) starts with ANY of `prefixes`. Surface-agnostic — the
 * concrete prefix lists live in proof-surfaces.mjs (BOS-201).
 * @param {string[]|null|undefined} changedFiles
 * @param {readonly string[]} prefixes
 * @returns {boolean}
 */
export function matchesAnyPrefix(changedFiles, prefixes) {
  const normalized = normalizeChangedFiles(changedFiles ?? [])
  return normalized.some((file) => prefixes.some((prefix) => file.startsWith(prefix)))
}

/**
 * Normalizes a recipe so browser (web/marketing/docs) surfaces prefer video.
 * - TUI is agent-only (no TUI recipes in the catalog); any stray surface:'tui'
 *   recipe passes through unchanged — browser-style video normalization never
 *   applies to it.
 * - A browser recipe with capture unset becomes a video; capture 'image' or
 *   'still' is preserved verbatim (it is not 'video', so every downstream
 *   `capture === 'video'` check reads it as a still). Preserving the opt-out
 *   value — rather than stripping the key — keeps the function idempotent: the
 *   runner re-normalizes the recipe.json that proof.mjs already wrote, and a
 *   stripped key would re-default an explicit `capture: 'still'` recipe to video.
 * - A resulting video recipe with no non-empty steps but a `route` gets default
 *   steps: goto the route, settle, then scroll the whole page (so the full page
 *   is always captured). The poster .png the video branch emits is the "both" still.
 * Pure and idempotent.
 *
 * The agent-surface test is injected (BOS-201): callers pass
 * `isAgentSurface` from proof-surfaces.mjs so this engine never hardcodes a
 * surface name. The default predicate `(s) => s === 'tui'` keeps behavior
 * byte-identical for any caller that omits it.
 * @param {object} recipe
 * @param {{ isAgentSurface?: (surface: string) => boolean }} [opts]
 * @returns {object}
 */
export function normalizeRecipe(recipe, { isAgentSurface = (s) => s === 'tui' } = {}) {
  if (!recipe || isAgentSurface(recipe.surface)) {
    return recipe
  }
  const optedOutStill = recipe.capture === 'still' || recipe.capture === 'image'
  if (optedOutStill) {
    return recipe
  }
  const hasSteps = Array.isArray(recipe.steps) && recipe.steps.length > 0
  const normalized = { ...recipe, capture: 'video' }
  if (!hasSteps && recipe.route) {
    normalized.steps = [
      { action: 'goto', route: recipe.route, caption: recipe.title },
      { action: 'wait', timeoutMs: 800 },
      {
        action: 'scroll',
        fullPage: true,
        caption: 'Automated demonstration unavailable — page overview only',
      },
    ]
  }
  return normalized
}

// The TUI/web surface path-prefix maps and their classifiers (classifyTuiSurface,
// webUiSurfacePresent, classifySurfaces) now live in scripts/proof-surfaces.mjs
// (BOS-201); the generic prefix matcher they build on is matchesAnyPrefix above.

// Default TOTAL wall-clock budget shared across ALL agent surfaces in one proof
// run (BOS-139 / epic D5). A NEW orchestrator concept — distinct from
// BOSS_PROOF_AGENT_TIMEOUT_MS (the per-runner outer SIGKILL backstop / per-SDK
// abort). Overridable per run via BOSS_PROOF_TOTAL_BUDGET_MS.
// BOS-354: raised 15→17 min so a combined TUI(6) + web(12) run keeps both
// per-surface defaults within the shared pool rather than squeezing the web
// floor (the TUI wall clock rose 4→6 min in the same change).
export const DEFAULT_TOTAL_PROOF_BUDGET_MS = 17 * 60 * 1000

// The per-surface wall-clock defaults + viability floors (the old
// SURFACE_BUDGET_SPEC) now live in scripts/proof-surfaces.mjs as descriptor
// `budget` values; planSurfaceBudget takes the resolved `budget` as a parameter
// (BOS-201). The ladder logic + DEFAULT_TOTAL_PROOF_BUDGET_MS stay here.

// Extra wall-clock granted to a run that drives a genuine live-agent scene
// (BOS-142: unstubbed live-agent scenes drive a real Claude session over tmux,
// which is materially slower than the stub's synchronous finish). The CALLER
// (scripts/proof.mjs) decides which surface is eligible (web today) and passes
// `liveAgent: true` only for it; planSurfaceBudget stays surface-name-agnostic.
// Extending the shared TOTAL pool so a live web run does not squeeze a sibling
// TUI floor is also the caller's job (bump `totalBudgetMs` before calling this
// for every surface in the run) — this function only ever widens the granted
// ceiling against whatever `remaining` it is given.
export const LIVE_AGENT_EXTRA_MS = 4 * 60 * 1000

// Extra wall-clock granted to a TUI run that ships a committed scenario, so the
// automatic agent→replay fallback (BOS-223) has room for BOTH legs without
// squeezing the agent leg below its floor. Mirrors LIVE_AGENT_EXTRA_MS: the
// CALLER (scripts/proof.mjs `runAgentSurfaces`) decides eligibility (a TUI
// surface with a committed scenario in the diff) and bumps the shared
// `totalBudgetMs` by this reserve before planning per-surface slices. The env
// override (`Number(process.env.TUI_REPLAY_EXTRA_MS) || …`) happens at that call
// site (BOS-223 Task 2), not here — this exported default stays a plain const so
// every reader sees the same number.
export const TUI_REPLAY_EXTRA_MS = 120 * 1000

/**
 * Sequential shared-budget planner (BOS-139 / epic D5). Pure. `elapsedMs` is the
 * wall-clock already spent by earlier surface runs in THIS proof invocation.
 * `budget` is the resolved `{defaultMs, floorMs}` for this surface (from
 * proof-surfaces.mjs `surfaceBudget`) — a null/absent budget (an unknown or
 * recipe surface the ladder never grants) → `budget-exceeded`. Returns a grant
 * clamped to the surface default and the remaining budget, or a
 * `budget-exceeded` deferral when what remains is below the viability floor.
 *
 * `liveAgent` (BOS-142, default false) grants an extra `LIVE_AGENT_EXTRA_MS` of
 * wall-clock (a genuine live-agent scene is slower than the stub). The CALLER
 * gates it to the eligible surface (web today) — this engine applies it whenever
 * `liveAgent` is true and stays surface-name-agnostic. A no-op when false/omitted,
 * so every existing caller that never passes it sees byte-identical output.
 *
 * @param {{ surface: string, elapsedMs: number, totalBudgetMs?: number, budget: {defaultMs:number,floorMs:number}|null, liveAgent?: boolean }} opts
 * @returns {{ run: true, maxWallClockMs: number } | { run: false, reasonCode: 'budget-exceeded' }}
 */
export function planSurfaceBudget({
  surface,
  elapsedMs,
  totalBudgetMs = DEFAULT_TOTAL_PROOF_BUDGET_MS,
  budget,
  liveAgent = false,
}) {
  const extraMs = liveAgent ? LIVE_AGENT_EXTRA_MS : 0
  const remaining = totalBudgetMs - elapsedMs
  if (!budget || remaining < budget.floorMs) return { run: false, reasonCode: 'budget-exceeded' }
  return { run: true, maxWallClockMs: Math.min(budget.defaultMs + extraMs, remaining) }
}

// Matches a committed deterministic TUI-proof scenario file
// (`proof/scenarios/**/*.scenario.json`, any depth). Kept BYTE-IDENTICAL to
// `SCENARIO_FILE_RE` in scripts/proof-surfaces.mjs (which cannot be imported here
// without a proof-lib↔proof-surfaces module cycle); an anti-drift test in
// proof-lib.test.mjs asserts discoverChangedScenarios and that module's
// committedScenarioPresent classifier always agree. Keep the two in sync.
const SCENARIO_FILE_RE = /^proof\/scenarios\/.+\.scenario\.json$/

/**
 * Diff-only scenario discovery (BOS-223, pure). Returns the subset of
 * `changedFiles` that are added/modified `proof/scenarios/**\/*.scenario.json`
 * paths, normalized (trim, `./`-strip, `\`→`/`) and de-duplicated in first-seen
 * order. Deletions/renames are already excluded upstream — `changedFiles` is the
 * PR's added/modified list — so this only filters by the path convention. Zero
 * matches ⇒ `[]`. NEVER a directory or catalog scan (committed-but-untouched
 * scenarios are history, not a catalog — standing project decision).
 * @param {string[]|null|undefined} changedFiles
 * @returns {string[]}
 */
export function discoverChangedScenarios(changedFiles) {
  const matches = normalizeChangedFiles(changedFiles ?? []).filter((f) => SCENARIO_FILE_RE.test(f))
  return [...new Set(matches)]
}

/**
 * Plan which TUI proof legs run (BOS-223, pure). Agent capture is attempted
 * first; the committed-scenario replay is the automatic fallback.
 * - `{agentUsable:T, scenarioPresent:T}` ⇒ both legs (agent first, replay fallback).
 * - `{agentUsable:T, scenarioPresent:F}` ⇒ agent only (no scenario to replay).
 * - `{agentUsable:F, scenarioPresent:T}` ⇒ replay only (keyless / recipe mode + scenario).
 * - `{agentUsable:F, scenarioPresent:F}` ⇒ neither; `deferReason:'agent-unavailable'`.
 *
 * The `deferReason` key is present ONLY on the `{F,F}` row (callers deep-equal the
 * result). BOS-350 reordered the upstream BOS-220 gate: a scenario-less TUI diff is
 * now deferred `scenario-missing` upstream ONLY when the agent is also unusable
 * (keyless), so `{T,F}` (key-present scenario-less) IS live-reachable and drives the
 * agent-only leg, while `{F,F}` (keyless scenario-less) is still deferred upstream and
 * the dispatcher never reaches it — that row stays here for a total, self-contained
 * decision table.
 * @param {{ agentUsable: boolean, scenarioPresent: boolean }} opts
 * @returns {{ runAgent: boolean, runReplay: boolean, deferReason?: 'agent-unavailable' }}
 */
export function planTuiLegs({ agentUsable, scenarioPresent }) {
  const runAgent = Boolean(agentUsable)
  const hasScenario = Boolean(scenarioPresent)
  if (!runAgent && !hasScenario) {
    return { runAgent: false, runReplay: false, deferReason: 'agent-unavailable' }
  }
  // Replay runs as the agent leg's fallback whenever a scenario exists, and is the
  // sole leg when the agent is unusable (keyless / recipe mode) but a scenario is.
  return { runAgent, runReplay: hasScenario }
}

/**
 * Classify a two-leg TUI proof run into `{proofSource, reasonCode, errorDetail}`
 * (BOS-223, pure, post-hoc). Driven solely by the two leg outcomes plus the
 * failure-text inputs the caller threads through — `legs` (the planTuiLegs
 * result) is accepted for caller symmetry/traceability but does not steer the
 * mapping (the outcomes fully determine it).
 *
 * Each outcome is `'passed'|'gate-failed'|'crashed'|'not-attempted'`.
 * `proofSource` is `'agent'|'replay'|null`.
 *
 * Decision table (exhaustive):
 * - agent `passed` ⇒ `{agent, null, null}` (replay not run — best case).
 * - a leg `passed` (media exists) with the OTHER leg failed/crashed ⇒ that leg's
 *   source, `reasonCode:null`; a failed/crashed agent leg's text still rides
 *   `errorDetail` so the fallback disclosure can render (agent gate-fail reason
 *   or crash text). A keyless replay-only pass carries `errorDetail:null`.
 * - NO leg passed + ANY leg crashed ⇒ `pipeline-error` (exit 1, unchanged
 *   BOS-138), `errorDetail` = the crash text(s).
 * - NO leg passed, NO crash ⇒ `agent-incomplete` (exit 1 since BOS-226) with a
 *   combined per-leg `errorDetail` — UNLESS the agent leg was cut off mid-flight
 *   by the per-run wall clock before any verdict (`agentTruncated:true`) AND the
 *   replay leg did not genuinely gate-fail, in which case it softens to the
 *   neutral, exit-0 `tui-truncated` deferral (BOS-354). A genuine judge-rejection
 *   — `done({passed:false})` on the agent leg (never sets `agentTruncated`) or a
 *   replay leg that gate-failed on real evidence — stays fatal `agent-incomplete`.
 *
 * `errorDetail` string shapes (consumed verbatim by Task 2):
 * - fallback disclosure detail → the raw agent-fail reason / crash text.
 * - combined both-leg detail → `agent: <a>; replay: <b>`.
 * - missing-scenario detail → `agent: <a>; replay: no committed scenario (<file>)`.
 * - replay-only failure detail → `replay: <b>`.
 *
 * @param {{
 *   legs: { runAgent: boolean, runReplay: boolean, deferReason?: string },
 *   agentOutcome: 'passed'|'gate-failed'|'crashed'|'not-attempted',
 *   replayOutcome: 'passed'|'gate-failed'|'crashed'|'not-attempted',
 *   agentDetail?: string|null,
 *   replayDetail?: string|null,
 *   missingScenario?: string|null,
 *   agentTruncated?: boolean,
 * }} opts
 * @returns {{ proofSource: 'agent'|'replay'|null, reasonCode: string|null, errorDetail: string|null }}
 */
export function classifyTuiOutcome({
  legs,
  agentOutcome,
  replayOutcome,
  agentDetail = null,
  replayDetail = null,
  missingScenario = null,
  agentTruncated = false,
}) {
  void legs // accepted for caller symmetry; outcomes fully determine the mapping.
  const agentFailed = agentOutcome === 'gate-failed' || agentOutcome === 'crashed'

  // Best case: the agent leg produced proof — replay never runs.
  if (agentOutcome === 'passed') {
    return { proofSource: 'agent', reasonCode: null, errorDetail: null }
  }

  // Fallback success: media exists via replay. Surface the agent leg's failure
  // text (gate-fail reason or crash text) so the disclosure line can render; a
  // keyless replay-only pass has no agent leg and carries no errorDetail.
  if (replayOutcome === 'passed') {
    return {
      proofSource: 'replay',
      reasonCode: null,
      errorDetail: agentFailed ? agentDetail : null,
    }
  }

  // No leg produced media. A crash in any leg is a machinery failure ⇒
  // pipeline-error (exit 1), reporting whichever leg(s) crashed.
  const agentCrashed = agentOutcome === 'crashed'
  const replayCrashed = replayOutcome === 'crashed'
  if (agentCrashed || replayCrashed) {
    let errorDetail
    if (agentCrashed && replayCrashed)
      errorDetail = `agent: ${agentDetail}; replay: ${replayDetail}`
    else if (agentCrashed) errorDetail = agentDetail
    else errorDetail = replayDetail
    return { proofSource: null, reasonCode: 'pipeline-error', errorDetail }
  }

  // No pass, no crash ⇒ agent-incomplete (exit 1 since BOS-226), combining each
  // attempted leg's gate-fail reason (naming the missing scenario when replay was
  // skipped for lack of one).
  const parts = []
  if (agentOutcome === 'gate-failed') parts.push(`agent: ${agentDetail}`)
  if (replayOutcome === 'gate-failed') parts.push(`replay: ${replayDetail}`)
  else if (replayOutcome === 'not-attempted' && missingScenario) {
    parts.push(`replay: no committed scenario (${missingScenario})`)
  }
  const errorDetail = parts.length > 0 ? parts.join('; ') : null
  // BOS-354: a mid-flight wall-clock truncation (the agent leg ran out of the
  // per-run wall clock before reaching any done() verdict) softens to a neutral,
  // exit-0 `tui-truncated` deferral — but ONLY when the replay leg did not
  // genuinely gate-fail on evidence (a replay judge-rejection is real evidence
  // failure and keeps the fatal agent-incomplete, BOS-226 intact). A
  // done({passed:false}) on the agent leg never sets agentTruncated, so it also
  // stays agent-incomplete. errorDetail is preserved verbatim either way.
  if (agentTruncated && replayOutcome !== 'gate-failed') {
    return { proofSource: null, reasonCode: 'tui-truncated', errorDetail }
  }
  return { proofSource: null, reasonCode: 'agent-incomplete', errorDetail }
}

export function selectRecipes(catalog, changedFiles, explicitRecipeIds = []) {
  const recipesById = new Map(catalog.recipes.map((recipe) => [recipe.id, recipe]))
  const selectedIds = []

  if (explicitRecipeIds.length > 0) {
    for (const id of explicitRecipeIds) {
      if (!recipesById.has(id)) {
        throw new Error(`unknown proof recipe: ${id}`)
      }
      selectedIds.push(id)
    }
    return uniqueInCatalogOrder(catalog.recipes, selectedIds)
  }

  const normalized = normalizeChangedFiles(changedFiles)
  for (const rule of catalog.pathRules) {
    if (normalized.some((file) => rule.patterns.some((pattern) => file.startsWith(pattern)))) {
      for (const id of rule.recipeIds) {
        if (!recipesById.has(id)) {
          throw new Error(`unknown proof recipe: ${id}`)
        }
        selectedIds.push(id)
      }
    }
  }

  return uniqueInCatalogOrder(catalog.recipes, selectedIds)
}

/**
 * Run-level agent-runner-stubbed flag (BOS-142). A single manifest carries ONE
 * `agentRunnerStubbed` boolean; the judge's honesty clamp reads it. It is true
 * (stubbed, conservative) unless the ENTIRE run was proven against the real
 * agent runner: there must be at least one scene AND every scene must carry the
 * explicit `agentRunnerStubbed === false` marker the web runner sets only for a
 * scene that ran live against the real Claude plugin. A scene WITHOUT the field
 * (stub-run, live-deferred, or a legacy capture) counts as stubbed; a capture
 * set with no scenes at all counts as stubbed (nothing was proven live). This
 * keeps all-stub captures byte-identical to the pre-BOS-142 goldens (the field
 * is only ever ADDED as `false` on genuinely-live scenes). Pure.
 * @param {Array<{scenes?: Array<{agentRunnerStubbed?: boolean}>}>} captures
 * @returns {boolean}
 */
export function capturesAgentRunnerStubbed(captures) {
  const scenes = (captures ?? []).flatMap((c) => c.scenes ?? [])
  if (scenes.length === 0) return true
  return scenes.some((s) => s?.agentRunnerStubbed !== false)
}

export function buildManifest({
  commit,
  prNumber,
  runId,
  publicBaseUrl,
  captures,
  agentRunnerStubbed,
  title,
  verdict,
  genAiLive = false,
  agentSummary = null,
  brief = null,
}) {
  const normalizedBase = publicBaseUrl ? publicBaseUrl.replace(/\/$/, '') : null
  const publicLiveCapture = captures.some((capture) => capture.privacy === 'live')
  return {
    version: 1,
    generatedAt: new Date().toISOString(),
    commit,
    prNumber: String(prNumber),
    runId,
    ...(title ? { title } : {}),
    ...(verdict ? { verdict } : {}),
    genAiLive,
    ...(agentSummary ? { agentSummary } : {}),
    ...(brief ? { brief } : {}),
    ...(normalizedBase ? { publicBaseUrl: normalizedBase } : {}),
    publicLiveCapture,
    ...(agentRunnerStubbed ? { agentRunnerStubbed: true } : {}),
    captures: captures.map((capture) => {
      const mediaType = capture.mediaType ?? 'png'
      // Normalize captured stills to public URLs whenever they exist, even when
      // the primary media is missing (e.g. video conversion failed but Playwright
      // still produced screenshots) — those stills are uploaded, so they must
      // stay linkable evidence instead of being dropped.
      const stills =
        Array.isArray(capture.stills) && capture.stills.length > 0
          ? capture.stills.map((still) => ({
              ...still,
              ...(normalizedBase
                ? {
                    url: `${normalizedBase}/${encodePathSegments(validateProofUploadRelativePath(still.fileName))}`,
                  }
                : {}),
            }))
          : null
      if (!capture.fileName) {
        return { ...capture, mediaType, ...(stills ? { stills } : {}) }
      }
      // Surface media URLs whenever file names exist, regardless of status, so
      // Unsatisfactory agent runs are reviewable without re-running.
      const url = normalizedBase
        ? `${normalizedBase}/${encodePathSegments(validateProofUploadRelativePath(capture.fileName))}`
        : null
      const out = { ...capture, mediaType, ...(url ? { url } : {}) }
      if (url && isVideoMediaType(capture.mediaType)) {
        out.videoUrl = url
        if (capture.posterFileName) {
          out.posterUrl = `${normalizedBase}/${encodePathSegments(validateProofUploadRelativePath(capture.posterFileName))}`
        }
      }
      if (stills) {
        out.stills = stills
      }
      return out
    }),
  }
}

/**
 * Evidence/Confidence verdict block (ported from wc-proof lib/publish.mjs).
 * Evidence is Satisfactory only when the run passed AND media exists.
 * Confidence is Low when not satisfactory, Medium when a gen-AI feature was
 * demoed UI-only (brief.genAi && !genAiLive), else High.
 * @param {{verdict?:string, captures?:Array<{fileName?:string,posterFileName?:string,stills?:Array<object>}>, genAiLive?:boolean, brief?:{genAi?:boolean}}} manifest
 * @returns {{evidence:string, confidence:string, evidenceOk:boolean, confidenceOk:boolean}}
 */
export function deriveVerdictBlock(manifest) {
  const hasMedia = (manifest.captures ?? []).some((c) =>
    Boolean(c.fileName || c.posterFileName || (c.stills?.length ?? 0) > 0),
  )
  const satisfactory = manifest.verdict === 'passed' && hasMedia
  const evidence = satisfactory ? 'Satisfactory' : 'Unsatisfactory'
  let confidence = 'Low'
  if (satisfactory) confidence = manifest.brief?.genAi && !manifest.genAiLive ? 'Medium' : 'High'
  return {
    evidence,
    confidence,
    evidenceOk: evidence !== 'Unsatisfactory',
    confidenceOk: confidence !== 'Low',
  }
}

/** The two ✅/❌ Evidence + Confidence lines, shared by the comment and the gallery README. */
export function verdictBlockLines(manifest) {
  const v = deriveVerdictBlock(manifest)
  return [
    `${v.evidenceOk ? '✅' : '❌'} **Evidence:** ${v.evidence}`,
    '',
    `${v.confidenceOk ? '✅' : '❌'} **Confidence:** ${v.confidence}`,
  ]
}

/**
 * Maps a fresh-context judge result (BOS-141 P4a/P4b) to the comment headline
 * emoji+title. Derives ONLY from `judge` (D12) — never from the mechanical
 * verdict/outcome. A missing/null judge and an explicit `{unjudged}` verdict
 * both collapse to the same honest "unjudged" headline; an unrecognized
 * `evidence` value (future schema drift) falls back to unjudged rather than
 * throwing. Pure.
 * @param {{unjudged?:true, evidence?:string}|null|undefined} judge
 * @returns {{emoji:string, title:string}}
 */
export function judgeHeadline(judge) {
  if (!judge || judge.unjudged) {
    return { emoji: '📸', title: 'Proof (unjudged)' }
  }
  if (judge.evidence === 'satisfactory') {
    return { emoji: '✅', title: 'Proof — judged convincing' }
  }
  if (judge.evidence === 'partial') {
    return { emoji: '⚠️', title: 'Proof produced — partially convincing' }
  }
  if (judge.evidence === 'unsatisfactory') {
    return { emoji: '⚠️', title: 'Proof produced but not convincing' }
  }
  return { emoji: '📸', title: 'Proof (unjudged)' }
}

/** Uppercases the first character; used to display enum values like 'partial' as 'Partial'. */
function capitalizeFirst(value) {
  const v = String(value ?? '')
  return v.length > 0 ? v.charAt(0).toUpperCase() + v.slice(1) : v
}

/**
 * Looks up a scene's human title by id across every capture's mechanical
 * `scenes[]` (BOS-140 shape: `{id, title, passed, missing, outputMs}`) in a
 * manifest-shaped object. A judge `perScene` entry carries only
 * `{id, verdict, reason}` — no title — so this recovers a display title when
 * one exists; returns null when no capture's scenes carry a matching id.
 * @param {{captures?: Array<{scenes?: Array<{id:string, title:string}>}>}} manifest
 * @param {string} sceneId
 * @returns {string|null}
 */
function findSceneTitle(manifest, sceneId) {
  for (const capture of manifest?.captures ?? []) {
    const match = (capture.scenes ?? []).find((s) => s?.id === sceneId)
    if (match?.title) return match.title
  }
  return null
}

/**
 * Judge verdict block lines (BOS-141 P4b/D12), REPLACING the bare mechanical
 * `verdictBlockLines(manifest)` call at every comment/gallery render site.
 * Labeled honestly (D16):
 *   - A judged run renders a `**Fresh-context judge (<model>):**` header line
 *     (Evidence/Confidence capitalized for display), one `- Scene <id> —
 *     <title>: ✗ <reason>` line for every `perScene` entry whose verdict is
 *     not `'passed'` (reasons/titles are model-authored data — escaped via
 *     `escapeMarkdown`), then one `- Caveat: <text>` line per caveat.
 *   - `{unjudged}` (or a missing/null judge) falls back to the EXISTING
 *     self-graded `verdictBlockLines(manifest)` output, followed by a
 *     `_Self-graded (unjudged): judge unavailable — <reason>._` label so the
 *     mechanical grade is never mistaken for a judged one.
 *
 * `sceneIds` is an OPT-IN per-surface scope: when provided (a Set/array of the
 * scene ids belonging to THIS surface's captures), the run-level judge's
 * `perScene` ✗ lines are filtered to only those ids, so a multi-surface comment
 * never misattributes one surface's scene failure to another surface's section
 * (BOS-141 P4b review fix). Omitted ⇒ every non-passed perScene entry renders
 * (the single-surface comment/gallery paths, byte-identical to before). The
 * run-level Evidence/Confidence header and caveats are NOT scoped — they are a
 * property of the whole run and may repeat per surface.
 * Pure.
 * @param {{judge: object|null|undefined, manifest: object, sceneIds?: Iterable<string>}} opts
 * @returns {string[]}
 */
export function judgeVerdictLines({ judge, manifest, sceneIds }) {
  if (!judge || judge.unjudged) {
    const reason = judge?.reason ?? 'no judge available'
    return [
      ...verdictBlockLines(manifest),
      '',
      `_Self-graded (unjudged): judge unavailable — ${escapeMarkdown(reason)}._`,
    ]
  }
  const scope = sceneIds ? new Set(sceneIds) : null
  const lines = [
    `**Fresh-context judge (${judge.model}):** Evidence: ${capitalizeFirst(judge.evidence)} · Confidence: ${capitalizeFirst(judge.confidence)}`,
  ]
  for (const scene of judge.perScene ?? []) {
    if (scene?.verdict === 'passed') continue
    if (scope && !scope.has(scene.id)) continue
    const title = findSceneTitle(manifest, scene.id)
    const label = title ? `Scene ${scene.id} — ${escapeMarkdown(title)}` : `Scene ${scene.id}`
    lines.push(`- ${label}: ✗ ${escapeMarkdown(scene.reason)}`)
  }
  for (const caveat of judge.caveats ?? []) {
    lines.push(`- Caveat: ${escapeMarkdown(caveat)}`)
  }
  return lines
}

/** Collects the set of mechanical scene ids carried by a list of captureShapes. Pure. */
function captureSceneIds(captures) {
  const ids = new Set()
  for (const capture of captures ?? []) {
    for (const scene of capture.scenes ?? []) {
      if (scene?.id) ids.add(scene.id)
    }
  }
  return ids
}

/**
 * '0:12' / '12:05' for an output-clock chapter offset; null-safe (BOS-140 P3d).
 * @param {number} outputMs
 * @returns {string|null}
 */
export function formatChapterOffset(outputMs) {
  if (!Number.isFinite(outputMs)) return null
  const totalSec = Math.round(outputMs / 1000)
  return `${Math.floor(totalSec / 60)}:${String(totalSec % 60).padStart(2, '0')}`
}

/**
 * Markdown chapter lines for a capture's scenes (BOS-140 P3d), shared by the
 * consolidated comment (`surfaceSectionLines`) and the gallery (`pushCapture`).
 * Each line links into the capture's mp4 via a media-fragment URL (`#t=SECONDS`)
 * when both a retimed timestamp and a videoUrl are available; otherwise it
 * still shows the ✅/✗ mark and title, unlinked. Captures with zero scenes, or
 * exactly one PASSED scene, render nothing — this is the short-circuit that
 * keeps single-scene passed captures byte-identical to the pre-P3d output.
 * Pure.
 * @param {{ scenes?: Array<{id:string,title:string,passed:boolean,missing:string[],outputMs:number|null}>, videoUrl?: string }} capture
 * @returns {string[]}
 */
export function renderSceneChapters(capture) {
  const scenes = capture.scenes ?? []
  // Short-circuit a single passed STUB scene (byte-identical to pre-P3d output).
  // A single passed LIVE scene (agentRunnerStubbed === false, BOS-142) still
  // renders so its `live agent (unstubbed)` disclosure is visible.
  if (
    scenes.length === 0 ||
    (scenes.length === 1 && scenes[0].passed && scenes[0].agentRunnerStubbed !== false)
  )
    return []
  const lines = ['', '**Scenes:**']
  scenes.forEach((s, i) => {
    const mark = s.passed ? '✅' : '✗'
    const stamp = formatChapterOffset(s.outputMs)
    const label = `Scene ${i + 1} — ${escapeMarkdown(s.title)}`
    const linked =
      stamp && capture.videoUrl
        ? `[${stamp}] ${mark} [${label}](${capture.videoUrl}#t=${Math.round(s.outputMs / 1000)})`
        : `${mark} ${label}`
    const miss = s.passed
      ? ''
      : s.missing?.length
        ? ` (missing: ${escapeMarkdown(s.missing.join(', '))})`
        : ' (no interaction recorded)'
    // BOS-142: only a genuinely-live (unstubbed) scene gets the disclosure
    // suffix; stub scenes render exactly as before (byte-compat).
    const live = s.agentRunnerStubbed === false ? ' — _live agent (unstubbed)_' : ''
    lines.push(`- ${linked}${miss}${live}`)
  })
  return lines
}

/**
 * Minimal sticky PR comment: a single prominent gallery link, the title, the
 * run metadata line, an optional agent summary, and the Evidence/Confidence
 * block. No inline media (the gallery README is the clickthrough). No Verdict
 * line (it dupes Evidence).
 */
export function renderComment({ marker, manifest }) {
  const lines = [marker]
  const { emoji, title: headlineTitle } = judgeHeadline(manifest.judge)
  lines.push(`### ${emoji} ${headlineTitle}`, '')
  if (manifest.reportUrl) lines.push(`### [📸 Proof gallery](${manifest.reportUrl})`, '')
  else if (manifest.publicBaseUrl)
    lines.push(`### [📸 Proof manifest](${manifest.publicBaseUrl}/manifest.json)`, '')
  lines.push(`**${escapeMarkdown(manifest.title ?? 'Proof of implementation')}**`, '')
  const genAi = manifest.genAiLive ? 'live providers' : 'not live (UI-only demo)'
  lines.push(
    `**Commit:** \`${manifest.commit}\`  **Run:** ${manifest.runId}  **Gen-AI:** ${genAi}`,
    '',
  )
  if (manifest.publicLiveCapture) {
    lines.push('**PUBLIC LIVE CAPTURE:** one or more screenshots came from live state.', '')
  }
  if (manifest.agentSummary) {
    lines.push(
      '<details><summary>Agent summary</summary>',
      '',
      manifest.agentSummary,
      '',
      '</details>',
      '',
    )
  }
  for (const l of judgeVerdictLines({ judge: manifest.judge, manifest })) lines.push(l)
  return `${lines.join('\n')}\n`
}

/**
 * Renders the header block shared by the consolidated multi-surface comment:
 * the gallery/manifest link, the title, and the commit/run/gen-AI metadata line.
 * Pure. Internal helper.
 * @param {{ marker: string, manifest: object }} opts
 * @returns {string[]}
 */
function consolidatedHeaderLines({ marker, manifest }) {
  const lines = [marker]
  const { emoji, title: headlineTitle } = judgeHeadline(manifest.judge)
  lines.push(`### ${emoji} ${headlineTitle}`, '')
  if (manifest.reportUrl) lines.push(`### [📸 Proof gallery](${manifest.reportUrl})`, '')
  else if (manifest.publicBaseUrl)
    lines.push(`### [📸 Proof manifest](${manifest.publicBaseUrl}/manifest.json)`, '')
  lines.push(`**${escapeMarkdown(manifest.title ?? 'Proof of implementation')}**`, '')
  const genAi = manifest.genAiLive ? 'live providers' : 'not live (UI-only demo)'
  lines.push(
    `**Commit:** \`${manifest.commit}\`  **Run:** ${manifest.runId}  **Gen-AI:** ${genAi}`,
    '',
  )
  if (manifest.publicLiveCapture) {
    lines.push('**PUBLIC LIVE CAPTURE:** one or more screenshots came from live state.', '')
  }
  return lines
}

/**
 * Renders one `#### <Label> — …` section for the consolidated comment (BOS-139
 * P2b). A passed surface renders an agent-summary details block + its own
 * Evidence/Confidence lines; a deferred surface renders the honest
 * deferredReasonMessage (never a ❌ verdict block) + optional stage/stderr and
 * recapture hint. Pure. Internal helper.
 * @param {object} section
 * @returns {string[]}
 */
function surfaceSectionLines(section, judge) {
  if (section.outcome === 'passed') {
    const out = [`#### ${section.label} — ✅ proven`, '']
    // BOS-223 D-Disclosure: a surface proven by the scenario-replay fallback
    // (not the live agent) gets ONE emphasized disclosure line, right under the
    // header. The two fallbackReasons render distinct copy. An agent/web section
    // (proofSource 'agent' or absent) renders none — byte-identical to before.
    if (section.proofSource === 'replay') {
      const disclosure =
        section.fallbackReason === 'agent-unavailable'
          ? 'agent unavailable; scenario replay shown'
          : 'agent capture failed; scenario replay shown'
      out.push(`_${disclosure}_`, '')
    } else if (section.scenarioOwed) {
      // BOS-350: a key-present agent-only TUI proof on a scenario-less diff. The
      // agent video IS the proof, but a committed proof/scenarios/*.scenario.json is
      // still owed for deterministic keyless replay (and BOS-226 will make it fatal).
      // Non-fatal author nudge, one emphasized line under the passed header — it does
      // NOT change the section's ✅ verdict or any exit code.
      out.push(
        '_agent capture shown; commit a proof/scenarios/\\*.scenario.json for deterministic replay_',
        '',
      )
    }
    if (section.summary) {
      out.push(
        '<details><summary>Agent summary</summary>',
        '',
        section.summary,
        '',
        '</details>',
        '',
      )
    }
    const vb = judgeVerdictLines({
      judge,
      manifest: {
        verdict: 'passed',
        captures: section.captures ?? [],
        genAiLive: false,
        brief: { genAi: section.genAi ?? false },
      },
      // Scope the run-level judge's per-scene ✗ lines to THIS surface's own
      // scenes so a peer surface's scene failure never surfaces here.
      sceneIds: captureSceneIds(section.captures),
    })
    out.push(...vb)
    // Chaptered scenes (BOS-140 P3d), keyed off the surface's PRIMARY capture.
    // renderSceneChapters short-circuits to [] for a single passed scene (or no
    // scenes at all), so this is a no-op for every pre-P3d/legacy shape.
    const primaryCapture = (section.captures ?? [])[0]
    if (primaryCapture) {
      out.push(...renderSceneChapters(primaryCapture))
    }
    out.push('')
    return out
  }
  const out = [`#### ${section.label} — ⏸ deferred (${section.reasonCode})`, '']
  out.push(deferredReasonMessage(section.reasonCode, { missing: section.missing }), '')
  if (section.stage) out.push(`**Failed stage:** \`${section.stage}\``, '')
  if (section.missing?.length) {
    out.push('**Missing prerequisites:**', ...section.missing.map((m) => `- ${m}`), '')
  }
  if (section.stderrTail) out.push('Error output (tail):', '', '```', section.stderrTail, '```', '')
  if (section.recaptureHint) {
    out.push('Re-capture from a dev environment:', '', '```bash', section.recaptureHint, '```', '')
  }
  // Chaptered scenes (BOS-140 P3d fixup): a scene failure forces the whole
  // capture to `status: 'failed'`, which classifies as deferred/agent-incomplete
  // above — without this, the comment never shows WHICH scene failed. Keyed off
  // the surface's primary capture; renderSceneChapters short-circuits to [] for
  // a scene-less or single-passed capture, so an ordinary env/pipeline deferral
  // (no scenes) renders unchanged.
  const primaryCapture = (section.captures ?? [])[0]
  if (primaryCapture && section.reasonCode !== 'no-ui-surface') {
    const chapters = renderSceneChapters(primaryCapture)
    // Every branch above ends `out` with a trailing blank separator line, and
    // renderSceneChapters starts with one too — drop the duplicate so the
    // gap stays a single blank line, matching the passed-section spacing.
    if (chapters.length > 0) out.push(...chapters.slice(1), '')
  }
  return out
}

/**
 * Renders ONE consolidated proof comment (BOS-139 P2b): a single header + one
 * `####` section per surface in run order. Partial success is first-class —
 * passed and deferred surfaces sit side by side, and there is NO global ❌
 * verdict block (each section carries its own ✅/⏸ state). Reuses the same
 * `proofCommentMarker` so `collapsePriorProofComments` still leaves exactly one
 * live comment. Pure.
 * @param {{ marker: string, manifest: object, sections: object[] }} opts
 * @returns {string}
 */
export function renderConsolidatedComment({ marker, manifest, sections }) {
  const lines = consolidatedHeaderLines({ marker, manifest })
  for (const section of sections ?? []) {
    lines.push(...surfaceSectionLines(section, manifest.judge))
  }
  // Drop a single trailing blank so the body ends with exactly one newline.
  while (lines.length > 0 && lines[lines.length - 1] === '') lines.pop()
  return `${lines.join('\n')}\n`
}

/**
 * Neutral "proof deferred" comment for environment limitations. Unlike
 * renderComment it never emits the ❌ Evidence/Confidence verdict block — a
 * deferral is not a failed change.
 * @param {{ marker: string, manifest: object, reasonCode: string, recaptureHint?: string }} opts
 * @returns {string}
 */
/**
 * The human-readable body sentence for a deferred-proof reason. Each class
 * gets an honest message: genuinely env-bound classes (agent-unavailable) may
 * name an environment limitation, but our own bugs/shortfalls (pipeline-error,
 * agent-incomplete, no-media) and the unhandled-code fallback never do — that
 * mislabel is the exact regression BOS-138 removes.
 * @param {string} reasonCode
 * @returns {string}
 */
export function deferredReasonMessage(reasonCode, { missing } = {}) {
  if (reasonCode === 'budget-exceeded') {
    // A neutral deferral (BOS-139 / D5): the shared per-run proof budget was
    // consumed by an earlier surface in this multi-surface PR. NOT a problem
    // with the change and NEVER an "environment limitation".
    return (
      'This surface was deferred because the shared proof budget (~17min per run) was ' +
      'consumed by an earlier surface in this multi-surface PR. The change itself is fine; ' +
      're-run proof for this surface alone to capture it.'
    )
  }
  if (reasonCode === 'tui-truncated') {
    // BOS-354: the TUI agent started and captured this change but the per-run
    // wall clock cut it off before it reached a done() verdict. This is NOT a
    // judge-rejection (those stay agent-incomplete) and NEVER an "environment
    // limitation" (that phrase is reserved for the genuinely env-bound classes) —
    // the change itself is fine and a re-run completes under the raised budget.
    return (
      'The TUI proof agent started and captured this change but was cut off by the ' +
      'per-run wall clock before it finished. The change itself is fine — re-run proof ' +
      'for the TUI surface and it typically completes under the raised per-run budget.'
    )
  }
  if (reasonCode === 'agent-unavailable') {
    return (
      'agentic TUI proof unavailable: agent mode is not enabled in this environment ' +
      '(set PROOF_ANTHROPIC_API_KEY, or unset BOSS_PROOF_MODE=recipe). ' +
      'The TUI surface is proved by an agent driving the real TUI, so there is no recipe ' +
      'fallback. This is an environment limitation, not a problem with the change.'
    )
  }
  if (reasonCode === 'no-ui-surface') {
    return (
      'No web UI surface to demonstrate for this change. The web proof agent drives the ' +
      'running app to show a user-visible change (a page, component, or interaction); this ' +
      'PR changes code with no such surface, so there is nothing to capture in a video. ' +
      "See the PR's diff and tests for the change. This is expected, not a problem with the change."
    )
  }
  if (reasonCode === 'env-unavailable') {
    const list = missing?.length ? missing.join(', ') : 'one or more prerequisites'
    return (
      'bs-proof could not run because required prerequisites are missing in this ' +
      `environment: ${list}. Provision the missing keys/toolchain (run ` +
      '`node scripts/proof.mjs doctor` to see the full report) and re-run. This is a ' +
      'provisioning gap, not a problem with the change.'
    )
  }
  if (reasonCode === 'pipeline-error') {
    // CRITICAL (BOS-138): a pipeline crash is OUR bug. This message MUST NEVER
    // claim an environment limitation — that mislabel is the exact regression
    // (TDZ render crash, ffmpeg hang) this class was created to stop.
    return (
      'The bs-proof pipeline hit an internal error while capturing this change. ' +
      'This is a defect in the proof tooling that must be fixed — the failed stage and ' +
      'error output are shown below. It is not a problem with the change itself.'
    )
  }
  if (reasonCode === 'agent-incomplete') {
    // A ran-but-did-not-finish agent, OR a degraded capture (e.g. a
    // timeout-killed ffmpeg encode that soft-fails to no media). This is a
    // proof-run shortfall to investigate — NOT an "environment limitation":
    // that mislabel is the exact regression (ffmpeg hang) BOS-138 kills, and it
    // is reachable here because a killed encode folds into this class.
    return (
      'The bs-proof agent ran but did not produce a complete capture this run. ' +
      'See the run log / manifest below for what it produced. This is a proof-run ' +
      'shortfall to investigate, not a confirmed problem with the change.'
    )
  }
  if (reasonCode === 'no-media') {
    return (
      'The bs-proof run finished without producing any media to post. See the ' +
      'manifest below for details. This is a proof-run shortfall to investigate, ' +
      'not a confirmed problem with the change.'
    )
  }
  if (reasonCode === 'scenario-missing') {
    // BOS-220: a TUI change shipped without a committed
    // proof/scenarios/*.scenario.json demonstrating it, so the deterministic TUI
    // proof (BOS-219) had nothing to replay. This is an AUTHORING nudge, never an
    // "environment limitation": the fix is in the author's hands (commit a
    // scenario), not the environment's. BOS-226: proof is required for TUI, so this
    // now exits 1 (fail-loud) rather than the old warn-only exit 0.
    return (
      'This TUI change did not commit a `proof/scenarios/*.scenario.json` demonstrating ' +
      'it, so the deterministic TUI proof had nothing to replay. Author one and iterate ' +
      'to green with `node scripts/proof.mjs scenario validate <file>` then ' +
      '`node scripts/proof.mjs scenario run <file> --dry-run`, and commit it in this PR. ' +
      'Proof is required for TUI — this exits 1 until a scenario is committed. This is an ' +
      'authoring gap, not a problem with the change.'
    )
  }
  // Fallback for any unhandled reason code. It MUST NOT assert an "environment
  // limitation" — that phrase is reserved for the genuinely env-bound classes
  // above (agent-unavailable) and must never leak onto our own bugs/shortfalls.
  return `bs-proof could not complete the proof this run (${reasonCode}). See the details below.`
}

/**
 * @param {{
 *   marker: string,
 *   manifest: object,
 *   reasonCode: string,
 *   recaptureHint?: string,
 *   missing?: string[],
 *   stage?: string,
 *   stderrTail?: string,
 *   details?: string,
 * }} opts
 * @returns {string}
 */
export function renderDeferredComment({
  marker,
  manifest,
  reasonCode,
  recaptureHint,
  missing,
  stage,
  stderrTail,
  details,
}) {
  const lines = [marker]
  if (manifest.reportUrl) lines.push(`### [📸 Proof gallery](${manifest.reportUrl})`, '')
  else if (manifest.publicBaseUrl)
    lines.push(`### [📸 Proof manifest](${manifest.publicBaseUrl}/manifest.json)`, '')
  else lines.push('')
  lines.push('### Proof deferred')
  lines.push('')
  lines.push(deferredReasonMessage(reasonCode, { missing }))
  if (stage) lines.push('', `**Failed stage:** \`${stage}\``)
  if (missing?.length) {
    lines.push('', '**Missing prerequisites:**', ...missing.map((m) => `- ${m}`))
  }
  if (details) lines.push('', details)
  if (stderrTail) lines.push('', 'Error output (tail):', '', '```', stderrTail, '```')
  if (manifest.commit) lines.push('', `**Commit:** \`${manifest.commit}\``)
  if (recaptureHint) {
    lines.push('', 'Re-capture from a dev environment:', '', '```bash', recaptureHint, '```')
  }
  return `${lines.join('\n')}\n`
}

/**
 * Orders captures for the gallery report: video captures first (so the most
 * informative artifact leads), then the rest. Stable within each group. Pure.
 * @param {Array<{mediaType?: string}>} captures
 * @returns {Array<object>}
 */
export function orderCapturesForReport(captures) {
  const list = captures ?? []
  const videos = list.filter((c) => isVideoMediaType(c.mediaType))
  const rest = list.filter((c) => !isVideoMediaType(c.mediaType))
  return [...videos, ...rest]
}

/**
 * Renders a Markdown README for the bs-proof report repo. Pure — no fs/env/Date.
 * @param {{ manifest: object }} opts
 * @returns {string}
 */
export function renderGallery({ manifest }) {
  const captureCount = (manifest.captures ?? []).length
  const hasSurfaces = Array.isArray(manifest.surfaces) && manifest.surfaces.length > 0
  const lines = [
    `# Proof report — PR ${manifest.prNumber}`,
    '',
    `- Commit: \`${manifest.commit}\``,
    `- Run: \`${manifest.runId}\``,
    `- Generated: ${manifest.generatedAt}`,
    `- Captures: ${captureCount}`,
  ]

  if (!hasSurfaces) {
    lines.push('')
    for (const l of judgeVerdictLines({ judge: manifest.judge, manifest })) lines.push(l)
  }
  if (manifest.agentRunnerStubbed)
    lines.push('', '_agent-runner stubbed; UI + orchestration + persistence exercised._')

  // Render a list of stills as blank-line-separated markdown images.
  const pushStills = (stills) => {
    for (let i = 0; i < stills.length; i++) {
      lines.push(`![${escapeMarkdown(stills[i].label)}](${stills[i].url})`)
      if (i < stills.length - 1) {
        lines.push('')
      }
    }
  }

  // Groups a capture's stills into one `### Scene N — <title> <mark>`
  // sub-heading per scene (BOS-140 P3d), matched by `still.sceneId`. Stills
  // without a sceneId (legacy captures predating scenes, or unmapped) fall
  // into a trailing flat "Step screenshots" group so old manifests keep
  // rendering. A scene with no matching stills gets no heading.
  const pushSceneGroupedStills = (stills, scenes) => {
    const bySceneId = new Map()
    const unlabeled = []
    for (const still of stills) {
      if (still.sceneId) {
        if (!bySceneId.has(still.sceneId)) bySceneId.set(still.sceneId, [])
        bySceneId.get(still.sceneId).push(still)
      } else {
        unlabeled.push(still)
      }
    }
    scenes.forEach((scene, i) => {
      const sceneStills = bySceneId.get(scene.id) ?? []
      if (sceneStills.length === 0) return
      const mark = scene.passed ? '✅' : '✗'
      // BOS-142: unstubbed live scenes disclose in the gallery heading; stub
      // scenes render exactly as before (byte-compat).
      const live = scene.agentRunnerStubbed === false ? ' _(live agent — unstubbed)_' : ''
      lines.push('', `### Scene ${i + 1} — ${escapeMarkdown(scene.title)} ${mark}${live}`, '')
      pushStills(sceneStills)
    })
    if (unlabeled.length > 0) {
      lines.push('', '### Step screenshots', '')
      pushStills(unlabeled)
    }
  }

  const pushCapture = (capture, headingLevel) => {
    lines.push('', `${'#'.repeat(headingLevel)} ${escapeMarkdown(capture.title)}`, '')
    lines.push(
      `**Surface:** ${escapeMarkdown(capture.surface)} · **Status:** ${escapeMarkdown(capture.status)}`,
      '',
    )

    if (capture.status !== 'passed') {
      lines.push(escapeMarkdown(capture.error ?? 'capture failed'))
    }

    const stills = capture.stills ?? []
    if (isVideoMediaType(capture.mediaType) && (capture.videoUrl || capture.url)) {
      // Thumbnail for the video link (BOS-251): the play-button poster when the
      // run produced one, else the first still. NEVER the mp4 URL itself — an
      // image tag pointing at a video renders as a broken image on GitHub, so
      // with no raster candidate at all we emit a plain link instead.
      const thumb = capture.posterUrl ?? stills.find((st) => st?.url)?.url
      if (capture.status !== 'passed') lines.push('')
      lines.push(
        thumb
          ? `[![${escapeMarkdown(capture.title)}](${thumb})](${capture.videoUrl ?? capture.url})`
          : `[${escapeMarkdown(capture.title)}](${capture.videoUrl ?? capture.url})`,
        '',
        '▶ Video',
      )
      // Chapter list + per-scene still grouping (BOS-140 P3d). Both no-op for a
      // single-scene (or scene-less) capture: renderSceneChapters short-circuits
      // to [] and the still-grouping falls back to the flat block below, so a
      // single-scene passed capture renders byte-identically to before P3d.
      lines.push(...renderSceneChapters(capture))
      if (stills.length > 0) {
        const scenes = capture.scenes ?? []
        if (scenes.length > 1) {
          pushSceneGroupedStills(stills, scenes)
        } else {
          lines.push('', '### Step screenshots', '')
          pushStills(stills)
        }
      }
    } else if (capture.url) {
      if (capture.status !== 'passed') lines.push('')
      lines.push(`![${escapeMarkdown(capture.title)}](${capture.url})`)
    } else if (stills.length > 0) {
      // No linkable primary media (e.g. video conversion failed) but Playwright
      // captured stills — surface them directly so there is still linked evidence.
      if (capture.status !== 'passed') lines.push('')
      pushStills(stills)
    }
  }

  const orderedCaptures = orderCapturesForReport(manifest.captures)
  if (hasSurfaces) {
    for (const surface of manifest.surfaces) {
      const label = surfaceLabel(surface.surface)
      const surfaceCaptures = orderedCaptures.filter(
        (capture) => capture.surface === surface.surface,
      )
      lines.push('', `## ${escapeMarkdown(label)}`, '')
      if (surface.outcome === 'passed') {
        lines.push(`#### ${escapeMarkdown(label)} — ✅ proven`, '')
        for (const l of judgeVerdictLines({
          judge: manifest.judge,
          manifest: {
            verdict: 'passed',
            captures: surfaceCaptures,
            genAiLive: manifest.genAiLive,
            brief: manifest.brief,
          },
          // Scope per-scene ✗ lines to this surface's own scenes (mirrors the
          // consolidated comment fix) so the gallery never misattributes them.
          sceneIds: captureSceneIds(surfaceCaptures),
        }))
          lines.push(l)
      } else {
        lines.push(
          `#### ${escapeMarkdown(label)} — ⏸ deferred (${escapeMarkdown(surface.reasonCode)})`,
          '',
        )
        lines.push(deferredReasonMessage(surface.reasonCode), '')
      }
      for (const capture of surfaceCaptures) {
        pushCapture(capture, 3)
      }
    }
    return `${lines.join('\n')}\n`
  }

  for (const capture of orderedCaptures) {
    pushCapture(capture, 2)
  }

  return `${lines.join('\n')}\n`
}

function uniqueInCatalogOrder(recipes, selectedIds) {
  const selected = new Set(selectedIds)
  const emitted = new Set()
  const out = []
  for (const recipe of recipes) {
    if (selected.has(recipe.id) && !emitted.has(recipe.id)) {
      emitted.add(recipe.id)
      out.push(recipe)
    }
  }
  return out
}

function encodePathSegments(fileName) {
  return String(fileName)
    .split('/')
    .map((segment) => encodeURIComponent(segment))
    .join('/')
}

function escapeMarkdown(value) {
  return String(value ?? '')
    .replaceAll('|', '\\|')
    .replaceAll('\n', ' ')
}

export const SCENARIO_USAGE =
  'usage: scenario validate <file> | scenario run <file> [--dry-run] [--pr <n>]'

export function parseProofArgs(argv) {
  const [firstArg, ...tail] = argv
  // A completely empty invocation prints usage rather than silently running a
  // real (uploading, comment-posting) capture. Flags-only invocations still
  // default to `run` so documented `--recipe`/`--dry-run` usage is unchanged.
  const command =
    argv.length === 0 ? 'help' : firstArg && !firstArg.startsWith('--') ? firstArg : 'run'
  const rest = command === firstArg ? tail : argv
  const parsed = {
    command,
    recipes: [],
    changedFiles: [],
    dryRun: false,
    json: false,
  }

  // BOS-219: ONLY the `scenario` command accepts a subcommand token + one
  // positional file path (plus --dry-run/--pr). Every other command parses
  // exactly as before (regression-pinned), so this branch is fully isolated.
  if (command === 'scenario') {
    return parseScenarioArgs(parsed, rest)
  }

  for (let i = 0; i < rest.length; i += 1) {
    const arg = rest[i]
    if (arg === '--recipe') {
      parsed.recipes.push(requireValue(rest, i, arg))
      i += 1
      continue
    }
    if (arg === '--changed-file') {
      parsed.changedFiles.push(requireValue(rest, i, arg))
      i += 1
      continue
    }
    if (arg === '--dry-run') {
      parsed.dryRun = true
      continue
    }
    if (arg === '--json') {
      parsed.json = true
      continue
    }
    throw new Error(`unknown proof argument: ${arg}`)
  }

  return parsed
}

function requireValue(args, index, flag) {
  const value = args[index + 1]
  if (!value || value.startsWith('--')) {
    throw new Error(`${flag} requires a value`)
  }
  return value
}

/**
 * Parses the `scenario` subcommand tail: one subcommand token (`validate`|`run`)
 * and one positional file path, in any order relative to the flags `--dry-run`
 * and `--pr <n>`. Adds `{sub, file, pr}` to the parsed object. Throws with the
 * usage string on an unknown/missing subcommand, a missing file, an unknown flag,
 * or an extra positional. Gated behind `command === 'scenario'` so no other
 * command's parse is affected.
 */
function parseScenarioArgs(parsed, rest) {
  parsed.sub = null
  parsed.file = null
  parsed.pr = null
  const positionals = []
  for (let i = 0; i < rest.length; i += 1) {
    const arg = rest[i]
    if (arg === '--dry-run') {
      parsed.dryRun = true
      continue
    }
    if (arg === '--pr') {
      parsed.pr = requireValue(rest, i, arg)
      i += 1
      continue
    }
    if (arg.startsWith('--')) {
      throw new Error(`unknown proof argument: ${arg}`)
    }
    positionals.push(arg)
  }
  parsed.sub = positionals[0] ?? null
  parsed.file = positionals[1] ?? null
  if (parsed.sub !== 'validate' && parsed.sub !== 'run') {
    throw new Error(`unknown scenario subcommand: ${parsed.sub ?? '(none)'}\n${SCENARIO_USAGE}`)
  }
  if (!parsed.file) {
    throw new Error(`scenario ${parsed.sub} requires a file argument\n${SCENARIO_USAGE}`)
  }
  if (positionals.length > 2) {
    throw new Error(`unexpected extra argument: ${positionals[2]}\n${SCENARIO_USAGE}`)
  }
  return parsed
}

/**
 * Bound a per-turn narration string for display as a single-line proof caption.
 * Mirrors the TS overlay's formatCaption (services/web/tests/e2e/agent/overlay.ts)
 * EXACTLY: collapse all whitespace runs to single spaces, trim, and truncate to
 * MAX_CAPTION chars (proof-caption-spec.mjs, BOS-140 D8) with a U+2026 ellipsis
 * ('…', length 1) so a truncated result is exactly MAX_CAPTION chars.
 * Whitespace-only/empty/nullish input collapses to ''. This is display-only —
 * secret scanning must always see the full untruncated text.
 * @param {unknown} text
 * @returns {string}
 */
export function formatCaption(text) {
  const collapsed = String(text ?? '')
    .trim()
    .replace(/\s+/g, ' ')
  return collapsed.length <= MAX_CAPTION ? collapsed : `${collapsed.slice(0, MAX_CAPTION - 1)}…`
}

export function terminalRenderCommand({ input, output, title, caption }) {
  const args = [
    '--dir',
    'services/web',
    'exec',
    'node',
    '../../scripts/proof-render-terminal.mjs',
    '--input',
    input,
    '--output',
    output,
    '--title',
    title,
  ]
  // Append the caption only when it is a non-empty string so the no-caption
  // invocation stays byte-identical (backward compatible).
  if (typeof caption === 'string' && caption.length > 0) {
    args.push('--caption', caption)
  }
  return ['pnpm', args]
}

/**
 * pnpm argv to render a single caption-bar strip PNG (BOS-121) at `width` px via
 * proof-render-terminal.mjs `--strip` mode. Mirrors terminalRenderCommand so the
 * strip renderer runs in the same services/web Playwright context.
 */
export function captionStripRenderCommand({ caption, width, output }) {
  const args = [
    '--dir',
    'services/web',
    'exec',
    'node',
    '../../scripts/proof-render-terminal.mjs',
    '--strip',
    '--width',
    String(width),
    '--output',
    output,
  ]
  if (typeof caption === 'string' && caption.length > 0) {
    args.push('--caption', caption)
  }
  return ['pnpm', args]
}

/**
 * pnpm argv to render a BATCH of terminal stills / caption strips in ONE browser
 * via proof-render-terminal.mjs `--manifest` mode. `manifest` is the path to a
 * JSON file holding the job array. Batching collapses the per-frame cold Chromium
 * launches (one per still + one per strip) into a single browser start.
 */
export function terminalRenderManifestCommand({ manifest }) {
  return [
    'pnpm',
    [
      '--dir',
      'services/web',
      'exec',
      'node',
      '../../scripts/proof-render-terminal.mjs',
      '--manifest',
      manifest,
    ],
  ]
}

export function tuiAgentBridgeBuildCommand({ outBin }) {
  return [
    'go',
    ['build', '-tags', 'e2e', '-o', outBin, './cmd/proof-tui-agent'],
    { cwd: 'services/boss' },
  ]
}

export function bossE2eBuildCommand({ outBin }) {
  return ['go', ['build', '-tags', 'e2e', '-o', outBin, './cmd'], { cwd: 'services/boss' }]
}

// The surface→descriptor map now lives in scripts/proof-surfaces.mjs
// (resolveSurfaceRegistry / captureSurfaceDescriptor); the command builders below
// take a resolved surface `descriptor` as an opaque parameter (BOS-201/BOS-202) and
// never reference surface names, so a consumer-declared surface flows through
// unchanged. The descriptor carries `serviceDir` (the package whose
// playwright.config.ts webServer builds+serves the site), `specRoot` (where the
// runner writes its spec), `defaultCropToSelector` (the video crop fallback), and
// optional `stageEnv` (extra env the runner exports before Playwright).
export function browserCaptureCommand({ descriptor, surface, recipePath, outputDir }) {
  const { serviceDir, specRoot, defaultCropToSelector, stageEnv } = descriptor
  const argv = [
    '--dir',
    serviceDir,
    'exec',
    'node',
    relativeFromService(serviceDir, './scripts/proof-playwright-runner.mjs'),
    '--surface',
    surface,
    '--recipe',
    relativeFromService(serviceDir, recipePath),
    '--output-dir',
    relativeFromService(serviceDir, outputDir),
    '--spec-root',
    specRoot,
    '--default-crop',
    defaultCropToSelector,
  ]
  if (stageEnv && Object.keys(stageEnv).length > 0) {
    argv.push('--stage-env', JSON.stringify(stageEnv))
  }
  return ['pnpm', argv]
}

export function introCardCommand({ serviceDir, out, width, height, label, title = '' }) {
  return [
    'pnpm',
    [
      '--dir',
      serviceDir,
      'exec',
      'node',
      '../../scripts/proof-render-intro-card.mjs',
      '--out',
      relativeFromService(serviceDir, out),
      '--width',
      String(width),
      '--height',
      String(height),
      '--label',
      label,
      '--title',
      title,
    ],
  ]
}

/**
 * Ancestor directories of a proof run dir, deepest first, ending at the
 * `.proof` dir itself (the run dir's own path is NOT included). Returns []
 * when the path has no `.proof` segment. Pure — string-only.
 * @param {string} localDir
 * @returns {string[]}
 */
export function proofAncestorDirs(localDir) {
  const parts = String(localDir ?? '')
    .replaceAll('\\', '/')
    .replace(/\/+$/, '')
    .split('/')
  const proofIdx = parts.indexOf('.proof')
  if (proofIdx === -1) return []
  const out = []
  for (let end = parts.length - 1; end > proofIdx; end -= 1) {
    out.push(parts.slice(0, end).join('/'))
  }
  return out
}

/**
 * Resolves the recipe-catalog path. An `override` (typically from
 * `BOSS_PROOF_CATALOG`) lets an ad-hoc/experimental recipe set be trialled
 * without editing the committed catalog: an absolute path is used as-is, a
 * relative one is resolved against the current working directory. With no
 * override, falls back to `proof/recipes/default.json` under `repoRoot`. Pure —
 * no fs/env/Date (the env value is passed in by the caller).
 * @param {string} repoRoot
 * @param {string} [override]
 * @returns {string}
 */
export function resolveCatalogPath(repoRoot, override) {
  const trimmed = String(override ?? '').trim()
  if (trimmed) {
    return path.isAbsolute(trimmed) ? trimmed : path.resolve(trimmed)
  }
  return path.join(repoRoot, 'proof', 'recipes', 'default.json')
}

export function proofRunPaths({ prNumber, commit, runId, token }) {
  // `token` is an optional random segment that keeps the public URL from being
  // fully derivable from the PR number and commit alone. The local and public
  // paths stay parallel so uploadBundle's relative-path mapping holds.
  const suffix = token
    ? `pr-${prNumber}/${commit}/${runId}/${token}`
    : `pr-${prNumber}/${commit}/${runId}`
  return {
    localDir: `.proof/${suffix}`,
    publicPrefix: `proof/bossanova/${suffix}`,
  }
}

export function r2UploadCommand({ bucket, key, file, contentType }) {
  return [
    'pnpm',
    [
      'dlx',
      // Pinned to a v4 line: v3 prints an "out-of-date, update to v4" warning on
      // every object put. NOTE: v4 changed `r2 object put` to default to the
      // LOCAL miniflare store — `--remote` is REQUIRED to write the real bucket
      // (without it the upload silently lands in a local simulator and the public
      // URL 404s).
      'wrangler@4.42.0',
      'r2',
      'object',
      'put',
      `${bucket}/${key}`,
      '--file',
      file,
      '--content-type',
      contentType,
      '--remote',
    ],
  ]
}

export function githubCommentCommand({ prNumber, bodyFile }) {
  return ['gh', ['pr', 'comment', String(prNumber), '--body-file', bodyFile]]
}

// Hidden marker embedded in every proof comment so prior runs can be found and
// collapsed. Keep in sync with the marker passed to renderComment.
export function proofCommentMarker(prNumber) {
  return `<!-- bossanova-proof:pr-${prNumber} -->`
}

// Lists a PR's comments as JSON (id + body + isMinimized) for upsert lookup.
export function listProofCommentsCommand({ prNumber }) {
  return ['gh', ['pr', 'view', String(prNumber), '--json', 'comments']]
}

const MINIMIZE_COMMENT_MUTATION =
  'mutation($id:ID!){minimizeComment(input:{subjectId:$id,classifier:OUTDATED})' +
  '{minimizedComment{isMinimized}}}'

// Collapses a comment as "Outdated" via GitHub's GraphQL minimizeComment
// mutation. commentId is the GraphQL node id (the `IC_…` id from listProofComments).
export function minimizeCommentCommand({ commentId }) {
  return [
    'gh',
    ['api', 'graphql', '-f', `query=${MINIMIZE_COMMENT_MUTATION}`, '-f', `id=${commentId}`],
  ]
}

// Pure selector: given the JSON printed by listProofCommentsCommand, return the
// node ids of proof comments that carry `marker` and are not already hidden.
// Used to collapse prior runs before posting the fresh comment.
export function selectOutdatedProofCommentIds({ commentsJson, marker }) {
  let parsed
  try {
    parsed = typeof commentsJson === 'string' ? JSON.parse(commentsJson) : commentsJson
  } catch {
    return []
  }
  const comments = parsed?.comments ?? []
  return comments
    .filter((c) => c && !c.isMinimized && typeof c.body === 'string' && c.body.includes(marker))
    .map((c) => c.id)
    .filter(Boolean)
}

// Crop blank rows from the top and bottom of a captured terminal screen so the
// rendered PNG sizes to its actual content. The TUI always dumps a full
// fixed-height screen (e.g. 36 rows), leaving short screens with large blank
// margins. Internal blank lines (between sections) are preserved.
export function trimTerminalBlankLines(text) {
  const lines = String(text ?? '').split('\n')
  let start = 0
  let end = lines.length
  while (start < end && lines[start].trim() === '') {
    start += 1
  }
  while (end > start && lines[end - 1].trim() === '') {
    end -= 1
  }
  return lines.slice(start, end).join('\n')
}

export function proofUploadFiles({ manifest, localDir }) {
  const files = [
    {
      file: path.join(localDir, 'manifest.json'),
      relative: 'manifest.json',
      contentType: 'application/json',
    },
  ]

  const pushArtifact = (fileName) => {
    if (!fileName) return
    const relative = validateProofUploadRelativePath(fileName)
    files.push({
      file: path.join(localDir, ...relative.split('/')),
      relative,
      contentType: mediaTypeForPath(relative),
    })
  }

  for (const capture of manifest.captures ?? []) {
    // Upload media whenever a fileName is present (passed OR failed), so
    // Unsatisfactory agent runs remain reviewable. Recipe failures never reach
    // this path with a fileName because proof.mjs deletes their artifacts
    // upstream before buildManifest is called.
    pushArtifact(capture.fileName)
    pushArtifact(capture.posterFileName)
    for (const still of capture.stills ?? []) {
      pushArtifact(still.fileName)
    }
  }

  return files
}

export function validateRecipeId(id) {
  const value = String(id ?? '')
  if (!/^[a-z0-9][a-z0-9-]*$/.test(value)) {
    throw new Error(`invalid proof recipe id: ${value || '<missing>'}`)
  }
  return value
}

export function validateProofUploadRelativePath(fileName) {
  const relative = String(fileName ?? '').replaceAll('\\', '/')
  const segments = relative.split('/')
  const ext = relative.toLowerCase().split('.').pop()
  if (
    !Object.prototype.hasOwnProperty.call(PROOF_MEDIA_TYPES, ext) ||
    relative.startsWith('/') ||
    /^[A-Za-z]:\//.test(relative) ||
    segments.some((segment) => !segment || segment === '.' || segment === '..')
  ) {
    throw new Error(`invalid proof upload path: ${relative || '<missing>'}`)
  }
  return relative
}

export function validateBrowserRoute(route) {
  const value = String(route ?? '')
  if (
    !value.startsWith('/') ||
    value.startsWith('//') ||
    value.includes('\\') ||
    /[\u0000-\u001F\u007F]/.test(value)
  ) {
    throw new Error(`proof browser route must be relative: ${value || '<missing>'}`)
  }
  return value
}

function relativeFromService(serviceDir, target) {
  if (!target.startsWith('.')) {
    return target
  }
  return path.posix.relative(serviceDir, target.replaceAll('\\', '/'))
}
