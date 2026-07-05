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
 * exit policy: a passed surface and every neutral deferral (no-media,
 * no-ui-surface, budget-exceeded, env-unavailable, agent-unavailable) → 0; a
 * pipeline crash → 1; an agent-incomplete → 1 for web but 0 for TUI (preserves
 * the Q6 non-fatal reset the old single-select TUI path applied).
 * @param {{ surface: string, outcome: string, reasonCode: string|null }} entry
 * @returns {0|1}
 */
function surfaceExitContribution({ surface, outcome, reasonCode }) {
  if (outcome === 'passed') return 0
  if (reasonCode === 'pipeline-error') return 1
  if (reasonCode === 'agent-incomplete') return surface === 'web' ? 1 : 0
  return 0
}

/**
 * Aggregate exit code across all per-surface outcomes (BOS-139 P2b): 1 if any
 * surface contributes a failure (pipeline-error anywhere, or a web
 * agent-incomplete), else 0. Partial success (≥1 passed, ≥1 neutral deferral)
 * → 0. Empty input → 0.
 * @param {{ surface: string, outcome: string, reasonCode: string|null }[]} perSurface
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
