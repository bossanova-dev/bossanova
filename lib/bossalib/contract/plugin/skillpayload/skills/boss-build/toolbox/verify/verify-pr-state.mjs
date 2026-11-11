import { classifyInputsFromPrState } from '../ci/pr-state-envelope.mjs'
import { judgeCiOnHead } from '../epic/merge-eligibility.mjs'
import {
  CHECK_STATES,
  classifyEntry,
  normalizeRollup,
  normalizeWorkflowRuns,
} from '../pr-check-state.mjs'
import {
  classifyBossResponse,
  FALLBACK_LABEL_PREFIX,
  GH_USAGE_LABEL_ENV,
  viewFromEnvelope,
} from '../pr-state.mjs'

const fallbacks = new WeakMap()

export const prStateCall = (pr, repo, { fresh = false } = {}) => [
  'boss',
  ['pr', 'state', String(pr), '--repo', repo, '--json', ...(fresh ? ['--fresh'] : [])],
  { json: true },
]

export function readVerifyPrState(read, { pr, repo, fresh = false }) {
  if (fallbacks.has(read)) return { source: 'gh-fallback', reason: fallbacks.get(read) }
  const r = read(...prStateCall(pr, repo, { fresh }))
  const result = classifyBossResponse({
    status: r.exit ?? (r.ok ? 0 : 1),
    stdout: r.output ?? JSON.stringify(r.payload),
    error: !r.ok && r.exit === null ? { code: r.error } : undefined,
  })
  if (result.kind === 'fallback') {
    fallbacks.set(read, result.reason)
    return { source: 'gh-fallback', reason: result.reason }
  }
  if (result.kind === 'error') return { source: null, error: result.error }
  const envelope = result.body
  const invalid = classifyInputsFromPrState(envelope).readError
  if (invalid || !Array.isArray(envelope.checks))
    return { source: null, error: invalid ?? 'PR state carried no checks' }
  return { source: 'daemon', envelope }
}

export const ghFallbackOpts = (opts = {}) => ({
  ...opts,
  env: { ...opts.env, [GH_USAGE_LABEL_ENV]: `${FALLBACK_LABEL_PREFIX}verify-gate` },
})

export function ciViewOf(envelope) {
  const { headRefOid, statusCheckRollup } = viewFromEnvelope(envelope)
  return { headRefOid, statusCheckRollup }
}

export function contextStatusOf(envelope, context) {
  const checks = envelope.checks.filter((check) => check?.name === context)
  if (checks.length === 0) return null
  if (checks.length > 1 || checks[0].is_unclassified) return undefined
  const check = checks[0]
  return {
    state:
      check.status !== 'completed'
        ? 'pending'
        : check.conclusion === 'success'
          ? 'success'
          : 'failure',
    description: check.description ?? '',
  }
}

const rollupReadCall = (pr, repo) => [
  'gh',
  ['pr', 'view', String(pr), '--repo', repo, '--json', 'headRefOid,statusCheckRollup'],
  ghFallbackOpts({ json: true }),
]

export function headView(read, row, pr, repo) {
  if (row && !row.gap) return row.view
  const result = readVerifyPrState(read, { pr: pr.number, repo })
  if (result.source === 'daemon') return ciViewOf(result.envelope)
  if (result.source !== 'gh-fallback') return null
  const r = read(...rollupReadCall(pr.number, repo))
  return r.ok ? r.payload : null
}

export function mergeStatus(read, { pr, repo, headSha, context }, fallback) {
  const result = readVerifyPrState(read, { pr, repo, fresh: true })
  if (result.source === 'gh-fallback') return fallback(ghFallbackOpts())
  if (result.source !== 'daemon') return { readable: false, status: null }
  if (result.envelope.head_sha.toLowerCase() !== headSha) return { headMoved: true }
  const status = contextStatusOf(result.envelope, context)
  return { readable: status !== undefined, status: status ?? null }
}

// CI half: exclude boss/*; pending is unsettled, failing is red, unproven green is unknown.
export function classifyHeadCi(view, headSha, workflowRuns = null) {
  const input = {
    headSHA: headSha,
    observedSHA: view?.headRefOid ?? '',
    rollup: view?.statusCheckRollup,
  }
  let ci = judgeCiOnHead(input)
  const failed =
    /** @type {string} */ (ci.verdict.state) === CHECK_STATES.FAILING
      ? normalizeRollup(input.rollup).filter(
          (entry) => !entry.name.startsWith('boss/') && classifyEntry(entry) === 'failed',
        )
      : []
  if (
    failed.length > 0 &&
    failed.every(
      (entry) => entry.conclusion === 'CANCELLED' && (entry.workflow || entry.workflowId),
    )
  ) {
    const payload = typeof workflowRuns === 'function' ? workflowRuns() : workflowRuns
    const runs = normalizeWorkflowRuns(payload, headSha)
    if (runs.known && !runs.truncated) ci = judgeCiOnHead({ ...input, workflowRuns: payload })
  }
  // classifyChecks has no declared return type, and tsc infers its state too narrowly.
  const failing = /** @type {string} */ (ci.verdict.state) === CHECK_STATES.FAILING
  if (ci.green) return null
  if (!ci.settled) return 'ci-unsettled'
  return failing ? 'ci-red' : 'unknown:ci'
}

export function readCandidateCi(read, view, headSha, repoPath) {
  return classifyHeadCi(view, headSha, () => {
    const runs = read(
      'gh',
      ['api', `${repoPath}/actions/runs?head_sha=${headSha}&per_page=100`, '--paginate', '--slurp'],
      { json: true },
    )
    return runs.ok ? runs.payload : null
  })
}

export function failedCiChecks(rollup, verdict) {
  const reconciled = new Set([
    ...verdict.supersededCancelled,
    ...verdict.cancelledPendingReplacement,
  ])
  return normalizeRollup(rollup).filter(
    (entry) =>
      !entry.name.startsWith('boss/') &&
      classifyEntry(entry) === 'failed' &&
      !reconciled.has(entry.name),
  )
}

/** Has every check named in `names` reported on the head's rollup, and passed every time? */
export function generationChecksPassed(rollup, names = []) {
  const entries = normalizeRollup(rollup)
  return names.every((name) => {
    const runs = entries.filter((entry) => entry.name === name)
    return runs.length > 0 && runs.every((e) => String(e.conclusion).toUpperCase() === 'SUCCESS')
  })
}
