// ci/verify-hold.mjs — the verify-stage hold the build and repair CI waits leave out.
//
// A pending `boss/verify` claim (`verifying… <token>`) or `waiting:` hold on the head is the verify
// stage's work, not ordinary CI. On the gh path the head's commit statuses classify the pending node
// with `claimState`; on the daemon path the envelope's verify phase does. Only a pending claim,
// stale claim or waiting hold is a hold; every terminal verify status still counts.
//
// Pure; Node builtins and sibling toolbox modules only.

import { classifyRead } from '../commit-status.mjs'
import { classifyEntry } from '../pr-check-state.mjs'
import { VERIFY_CONTEXT, claimState } from '../verify/verify-vocabulary.mjs'

// The `claimState` kinds that are verify-stage holds rather than a verify verdict; the R7 line names
// each one so the transcript shows why the count went down.
export const VERIFY_HOLD_LABELS = Object.freeze({
  foreign: 'live boss/verify claim',
  stale: 'stale boss/verify claim',
  waiting: 'boss/verify waiting hold',
})
const PENDING_STATES = new Set(['PENDING', 'EXPECTED'])
const ENVELOPE_KEYS = ['statusCheckRollup', 'checks', 'check_runs', 'workflow_runs']

// Resolved the way pr-check-state.mjs resolves a context name: `name`, then `context`, then
// `workflowName`.
function contextName(node) {
  if (typeof node === 'string') return node.trim()
  if (!node || typeof node !== 'object') return ''
  for (const key of ['name', 'context', 'workflowName']) {
    const value = node[key]
    if (typeof value === 'string' && value.trim() !== '') return value.trim()
  }
  return ''
}

function isEnvelope(value) {
  return (
    value !== null &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    ENVELOPE_KEYS.some((key) => Array.isArray(value[key]))
  )
}

// dropContext removes every node named `name` from any payload shape classifyChecks accepts (a bare
// node array, a bare name array, an envelope object, or a `--paginate --slurp` page array), keeping
// the shape.
export function dropContext(payload, name) {
  if (payload == null) return payload
  const keep = (node) => contextName(node) !== name
  const filterEnvelope = (object) => {
    const copy = { ...object }
    for (const key of ENVELOPE_KEYS)
      if (Array.isArray(copy[key])) copy[key] = copy[key].filter(keep)
    return copy
  }
  if (Array.isArray(payload)) {
    if (payload.some(isEnvelope))
      return payload.map((page) => (isEnvelope(page) ? filterEnvelope(page) : page))
    return payload.filter(keep)
  }
  return isEnvelope(payload) ? filterEnvelope(payload) : payload
}

export function hasPendingVerifyNode(rollup) {
  const nodes = Array.isArray(rollup?.statusCheckRollup) ? rollup.statusCheckRollup : []
  return nodes.some(
    (node) =>
      contextName(node) === VERIFY_CONTEXT &&
      PENDING_STATES.has(String(node?.state ?? node?.conclusion ?? '').toUpperCase()),
  )
}

// verifyHoldOf classifies the statuses read. It returns the raw latest `boss/verify` status (null when
// the read failed or carried none) and the hold it is, or a null hold when the node must stay: a
// failed or malformed read, no status, a status no longer pending, or a description that is neither
// a claim nor a waiting hold. The build holds no claim, so `claimState` never answers `mine` here.
export function verifyHoldOf(read, headSha, now) {
  if (!read?.ok) return { status: null, hold: null }
  // classifyRead owns the latest-per-context rule and reads gh's stdout, so hand it the payload back.
  const classified = classifyRead({
    exit: 0,
    stdout: JSON.stringify(read.payload ?? null),
    context: VERIFY_CONTEXT,
    sha: headSha,
  })
  const status = classified.contexts?.[VERIFY_CONTEXT] ?? null
  if (status === null || status.state !== 'pending') return { status, hold: null }
  const claim = claimState(status, { now })
  if (!Object.hasOwn(VERIFY_HOLD_LABELS, claim.kind)) return { status, hold: null }
  return {
    status,
    hold: {
      kind: claim.kind,
      token: claim.token ?? null,
      description: status.description,
      updatedAt: status.updatedAt,
    },
  }
}

// verifyHoldFromPhase is verifyHoldOf for a daemon reading: the envelope's verify phase classifies
// the pending `boss/verify` node, a `verifying` claim as foreign and a `waiting` hold as waiting.
export function verifyHoldFromPhase(payload) {
  const kind = { verifying: 'foreign', waiting: 'waiting' }[payload?.verifyPhase]
  const nodes = Array.isArray(payload?.statusCheckRollup) ? payload.statusCheckRollup : []
  const node = nodes.find(
    (n) => contextName(n) === VERIFY_CONTEXT && classifyEntry(n) === 'pending',
  )
  if (kind === undefined || node === undefined) return { status: null, hold: null }
  return {
    status: null,
    hold: { kind, token: null, description: node.description, updatedAt: null },
  }
}
