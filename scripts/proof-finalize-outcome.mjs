#!/usr/bin/env node
/**
 * proof-finalize-outcome.mjs — Outcome-classification seam for agent proof
 * finalize (BOS-139 T5/D7). Pure: no fs/env/Date/network, no imports from
 * proof.mjs or proof-agent-finalize.mjs (cycle guard — see the split in
 * proof-agent-finalize.mjs).
 *
 * Behavior-preserving extraction of the degraded/reasonCode + exit-code logic
 * that used to live inline in finalizeAgentProof.
 */

/**
 * Classifies a single proof run's outcome from its manifest-derived facts. The
 * `pipelineError` axis is handled by the caller (it is a separate, orthogonal
 * degradation that also forces exit 1); this covers the verdict/media/surface
 * dimensions only.
 *
 * @param {{ verdict: string, hasMedia: boolean, noSurface: boolean }} opts
 * @returns {{ degraded: boolean, reasonCode: 'no-ui-surface'|'agent-incomplete'|'no-media'|null }}
 */
export function classifyRunOutcome({ verdict, hasMedia, noSurface }) {
  const degraded = verdict !== 'passed' || !hasMedia
  if (!degraded) return { degraded: false, reasonCode: null }
  const reasonCode = noSurface
    ? 'no-ui-surface'
    : verdict !== 'passed'
      ? 'agent-incomplete'
      : 'no-media'
  return { degraded: true, reasonCode }
}

/** True when a surfaceRun produced any linkable media (video, poster, or stills). */
export function surfaceRunHasMedia(run) {
  return (run?.captureShapes ?? []).some(
    (c) => c?.fileName || c?.posterFileName || (c?.stills?.length ?? 0) > 0,
  )
}

function firstCaptureError(run) {
  return (run?.captureShapes ?? []).find((c) => c?.error)?.error ?? null
}

/**
 * Per-surface outcome for the consolidated manifest/comment (BOS-139 P2b).
 * reasonCode uses the BOS-138 Phase-1 taxonomy + 'budget-exceeded' (BOS-139).
 * A surface that never ran (a synthetic gate-miss / budget deferral) carries a
 * set `reasonCode` and `captureShapes: []`; a crashed stage carries
 * `pipelineError`; everything else is classified from its captures.
 *
 * @param {object[]} surfaceRuns
 * @returns {{ surface: string, outcome: 'passed'|'deferred', reasonCode: string|null, error: string|null }[]}
 */
export function classifySurfaceOutcomes(surfaceRuns) {
  return (surfaceRuns ?? []).map((run) => {
    if (run.pipelineError) {
      return {
        surface: run.surface,
        outcome: 'deferred',
        reasonCode: 'pipeline-error',
        error: run.pipelineError.message ?? null,
      }
    }
    // A synthetic never-ran surfaceRun (gate miss / budget) carries its own
    // reasonCode and no captures — trust it verbatim.
    if (run.reasonCode) {
      return {
        surface: run.surface,
        outcome: 'deferred',
        reasonCode: run.reasonCode,
        error: run.error ?? null,
      }
    }
    const hasMedia = surfaceRunHasMedia(run)
    const { degraded, reasonCode } = classifyRunOutcome({
      verdict: run.hasFailure ? 'failed' : 'passed',
      hasMedia,
      noSurface: run.noSurface,
    })
    if (degraded) {
      return {
        surface: run.surface,
        outcome: 'deferred',
        reasonCode,
        error: firstCaptureError(run),
      }
    }
    return { surface: run.surface, outcome: 'passed', reasonCode: null, error: null }
  })
}

/**
 * Exit-code contribution of a single per-surface outcome. Encodes the epic
 * exit policy (BOS-226 fail-loud):
 *   - a passed surface, and every neutral deferral (no-media, no-ui-surface,
 *     budget-exceeded, env-unavailable, agent-unavailable, and the BOS-354
 *     `tui-truncated` — a TUI capture cut off mid-flight by the per-run wall
 *     clock before any verdict) → 0;
 *   - `agent-incomplete` on either surface — web or tui (the agent ran and its
 *     captured evidence failed the judge) → 1;
 *   - `scenario-missing` (a TUI change shipped without a committed
 *     proof/scenarios/*.scenario.json) → 1 — proof is required for TUI;
 *   - a pipeline crash (`pipeline-error`) → 1.
 *
 * The `softened` flag is the rollback lever (see `softenTuiExit`): when an entry
 * is marked `softened: true` its contribution is forced to 0. That is the only
 * escape hatch — it is applied at the impure boundary (proof-agent-finalize.mjs)
 * gated on `BOSS_PROOF_TUI_SOFT`, and only to TUI agent-incomplete/scenario-missing
 * entries. `pipeline-error` and web contributions are never softened here.
 * @param {{ outcome: string, reasonCode: string|null, softened?: boolean }} entry
 * @returns {0|1}
 */
function surfaceExitContribution({ outcome, reasonCode, softened }) {
  if (outcome === 'passed') return 0
  if (reasonCode === 'pipeline-error') return 1
  if (softened) return 0
  if (reasonCode === 'agent-incomplete') return 1
  if (reasonCode === 'scenario-missing') return 1
  return 0
}

/**
 * Pure rollback lever for the BOS-226 TUI fail-loud flip. When `soft` is true,
 * returns a NEW array where any entry with `surface === 'tui'` AND
 * (`reasonCode === 'agent-incomplete'` OR `reasonCode === 'scenario-missing'`)
 * is mapped to `{ ...entry, softened: true }` (its exit contribution is then
 * forced to 0 by surfaceExitContribution); every other entry is returned
 * unchanged. When `soft` is false the input array is returned unchanged (a
 * no-op — no `softened` flag is added). `soft` is ALWAYS passed by the caller:
 * this module never reads `process.env`.
 * @param {{ surface: string, outcome: string, reasonCode: string|null }[]} perSurface
 * @param {{ soft?: boolean }} [opts]
 * @returns {object[]}
 */
export function softenTuiExit(perSurface, { soft = false } = {}) {
  const entries = perSurface ?? []
  if (!soft) return entries
  return entries.map((entry) => {
    const isSoftenable =
      entry.surface === 'tui' &&
      (entry.reasonCode === 'agent-incomplete' || entry.reasonCode === 'scenario-missing')
    return isSoftenable ? { ...entry, softened: true } : entry
  })
}

/**
 * Aggregate exit code across all per-surface outcomes (BOS-139 P2b, BOS-226
 * fail-loud): 1 if any surface contributes a failure (pipeline-error anywhere,
 * an agent-incomplete on either surface, or a scenario-missing), else 0.
 * Partial success (≥1 passed, ≥1 neutral deferral — including the BOS-354
 * `tui-truncated`, which falls through to 0) → 0. Empty input → 0.
 * Entries marked `softened` (via `softenTuiExit` at the boundary) contribute 0.
 * @param {{ outcome: string, reasonCode: string|null, softened?: boolean }[]} perSurface
 * @returns {0|1}
 */
export function aggregateExitCode(perSurface) {
  return (perSurface ?? []).some((p) => surfaceExitContribution(p) === 1) ? 1 : 0
}

/** Display label for a proof surface in the consolidated comment. */
export function surfaceLabel(surface) {
  if (surface === 'tui') return 'TUI'
  if (surface === 'web') return 'Web'
  const s = String(surface ?? '')
  return s ? s[0].toUpperCase() + s.slice(1) : 'Surface'
}
