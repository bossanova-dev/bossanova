#!/usr/bin/env node
/**
 * proof-finalize-render.mjs — Section-building seam for the consolidated
 * multi-surface finalize comment (BOS-139 P2b). Imports ONLY from
 * proof-finalize-outcome.mjs (pure) — never from proof.mjs or
 * proof-agent-finalize.mjs (cycle guard). The actual markdown assembly lives in
 * proof-lib.mjs `renderConsolidatedComment`; this module maps each surfaceRun +
 * its classified outcome into the render-ready section descriptor.
 */

import { surfaceLabel } from './proof-finalize-outcome.mjs'

/**
 * Maps each surfaceRun (paired with its classified per-surface outcome) into the
 * render-ready section descriptor consumed by `renderConsolidatedComment`. A
 * passed surface carries its agent summary + captures (for the Evidence/
 * Confidence lines); a deferred surface carries the reason code plus a
 * surface-scoped recapture hint (and, for a pipeline crash, the failed stage +
 * scrubbed stderr tail). A no-ui-surface deferral gets no recapture hint (there
 * is nothing to re-run).
 *
 * @param {{ runs: object[], perSurface: {outcome:string, reasonCode:string|null}[] }} opts
 * @returns {object[]}
 */
export function buildSurfaceSections({ runs, perSurface }) {
  return runs.map((run, i) => {
    const p = perSurface[i]
    const label = surfaceLabel(run.surface)
    if (p.outcome === 'passed') {
      return {
        label,
        outcome: 'passed',
        summary: run.agentResult?.summary ?? null,
        captures: run.captureShapes ?? [],
        genAi: run.brief?.genAi ?? false,
      }
    }
    const isNoSurface = p.reasonCode === 'no-ui-surface'
    const pe = run.pipelineError ?? null
    return {
      label,
      outcome: 'deferred',
      reasonCode: p.reasonCode,
      // No recapture hint for a no-surface skip: there is nothing to re-run.
      recaptureHint: isNoSurface
        ? undefined
        : `BOSS_PROOF_AGENT_SURFACE=${run.surface} node scripts/proof.mjs run`,
      stage: pe?.stage,
      stderrTail: pe?.stderrTail,
      missing: run.missing,
      // Carries scene data through so a scene-failure deferral can still show
      // its per-scene ✗ chapter block in the comment (BOS-140 P3d fixup); an
      // ordinary env/pipeline deferral has no scenes and renders unchanged.
      captures: run.captureShapes ?? [],
    }
  })
}
