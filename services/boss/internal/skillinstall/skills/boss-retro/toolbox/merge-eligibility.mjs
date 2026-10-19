// The PR-side merge-eligibility facts every merge path shares: the PR body section reader, the four
// shared ineligibility checks, CI-on-head for the verify stage, and the one pinned merge argv.
//
// Pure and offline by contract: no subprocess, no filesystem, no `process` reads. Session facts
// (outcome, review verdict, tree drift, launch origin, watchers, authorization) stay in
// `completion-gate.mjs`, which calls these checks at their original positions so its reason order
// is unchanged. Every reason here is a member of `completion-gate.mjs`'s `INELIGIBLE_REASONS`.
import {
  CHECK_STATES,
  classifyChecks,
  isGreen,
  mergeStateVerdict,
  verdictAt,
} from './pr-check-state.mjs'

const SHA = /^[0-9a-f]{40}$/i
const sha = (value) => typeof value === 'string' && SHA.test(value)

const MERGE_STATES = [
  'CLEAN',
  'HAS_HOOKS',
  'UNSTABLE',
  'BLOCKED',
  'BEHIND',
  'DRAFT',
  'DIRTY',
  'UNKNOWN',
]

// Every reason the shared checks can emit, in `sharedEligibility` order.
export const SHARED_INELIGIBLE_REASONS = Object.freeze([
  'ci-not-settled',
  'ci-not-green-on-head',
  ...MERGE_STATES.map((state) => `merge-state-${state.toLowerCase()}`),
  'pr-not-open',
  'pr-draft',
  'partial-marker',
  'head-unknown',
  'head-mismatch',
])

// Ignore fenced examples before recognizing headings.
export function sections(body) {
  const result = new Map()
  if (typeof body !== 'string') return result
  let fence = null
  let current = null
  for (const line of body.split(/\r?\n/)) {
    const marker = line.match(/^\s{0,3}(`{3,}|~{3,})/)
    if (marker) {
      if (!fence) fence = marker[1]
      else if (
        marker[1][0] === fence[0] &&
        marker[1].length >= fence.length &&
        /^\s*(`+|~+)\s*$/.test(line)
      )
        fence = null
      continue
    }
    if (fence) continue
    const heading = line.match(/^\s{0,3}##\s+(.+?)\s*#*\s*$/)
    if (heading) {
      current = heading[1].trim().toLowerCase()
      // A repeated section is undecidable, so a reader never takes one copy over another.
      if (result.has(current)) result.set(current, null)
      else result.set(current, [])
    } else if (current && result.get(current)) result.get(current).push(line)
  }
  return result
}

// The first non-blank line of a `## <heading>` section, or '' when it is absent or undecidable.
export function sectionToken(body, heading) {
  const lines = sections(body).get(heading)
  return lines?.map((s) => s.trim()).find(Boolean) ?? ''
}

export function ciReasons({ ciWaitState, checkVerdict, headSha } = {}) {
  const reasons = []
  if (ciWaitState !== 'settled') reasons.push('ci-not-settled')
  if (!sha(headSha) || !isGreen(verdictAt(checkVerdict ?? {}, headSha)))
    reasons.push('ci-not-green-on-head')
  return reasons
}

export function mergeStateReasons(prView) {
  const state =
    typeof prView?.mergeStateStatus === 'string' ? prView.mergeStateStatus.toUpperCase() : ''
  if (!state || state === 'UNKNOWN') return ['merge-state-unknown']
  if (
    !['CLEAN', 'HAS_HOOKS', 'UNSTABLE'].includes(state) ||
    mergeStateVerdict({ mergeState: state }).blocking
  )
    return [
      SHARED_INELIGIBLE_REASONS.includes(`merge-state-${state.toLowerCase()}`)
        ? `merge-state-${state.toLowerCase()}`
        : 'merge-state-unknown',
    ]
  return []
}

// boss-build's PARTIAL marker, the one explicit hold a PR body or title carries: the first line of
// `## Partial` reads `do not merge — partial: n/m …`, or the title carries `(partial n/m)`. Nothing
// else in a PR's own text holds a merge.
const PARTIAL_LINE = /^do not merge\s*[—–-]+\s*partial:\s*\d+\s*\/\s*\d+/i
const PARTIAL_TITLE = /\(partial\s+\d+\s*\/\s*\d+\)/i
export function isPartial({ title, body } = {}) {
  return (
    (typeof title === 'string' && PARTIAL_TITLE.test(title)) ||
    PARTIAL_LINE.test(sectionToken(body, 'partial'))
  )
}

export function prShapeReasons(prView) {
  const reasons = []
  if (prView?.state !== 'OPEN') reasons.push('pr-not-open')
  if (prView?.isDraft !== false) reasons.push('pr-draft')
  if (isPartial(prView ?? {})) reasons.push('partial-marker')
  return reasons
}

// Every head must be a 40-hex SHA and all must agree. boss-build passes
// `[pushedHead, localHead, upstreamHead, prView.headRefOid]`; the verify stage passes
// `[judgedHead, prView.headRefOid]`.
export function headReasons(heads) {
  if (!Array.isArray(heads) || heads.length === 0 || !heads.every(sha)) return ['head-unknown']
  return new Set(heads).size !== 1 ? ['head-mismatch'] : []
}

// The shared checks in their `computeEligibility` relative order.
export function sharedEligibility(facts = {}) {
  const f = facts ?? {}
  const reasons = [
    ...ciReasons({ ciWaitState: f.ciWaitState, checkVerdict: f.checkVerdict, headSha: f.headSha }),
    ...mergeStateReasons(f.prView),
    ...prShapeReasons(f.prView),
    ...headReasons(f.heads),
  ]
  return { eligible: reasons.length === 0, reasons }
}

// `boss/*` contexts are the pipeline's own receipts and verdicts, never a CI gate on the head.
const BOSS_PREFIX = 'boss/'
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

// Drops `boss/*` nodes from any payload shape classifyChecks accepts (a bare node array, a bare
// name array, an envelope object, or a `--paginate --slurp` page array), keeping the shape.
function dropBossContexts(payload, excluded) {
  if (payload == null) return payload
  const keep = (node) => {
    const name = contextName(node)
    if (!name.startsWith(BOSS_PREFIX)) return true
    excluded.add(name)
    return false
  }
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

// CI on the judged head for the verify stage: drop every `boss/*` context from the head and prior
// sets, then classify what remains. This is a filter here rather than a classifyChecks option, so
// the verdict vocabulary pinned against the Go checks verdict is untouched.
export function judgeCiOnHead({
  headSHA,
  observedSHA,
  rollup,
  checkRuns,
  priorContexts,
  workflowRuns,
  readError,
  structurallyAbsentContexts,
} = {}) {
  const excluded = new Set()
  const verdict = classifyChecks({
    headSHA,
    observedSHA,
    rollup: dropBossContexts(rollup, excluded),
    checkRuns: dropBossContexts(checkRuns, excluded),
    priorContexts: dropBossContexts(priorContexts, excluded),
    workflowRuns,
    readError,
    structurallyAbsentContexts,
  })
  return {
    verdict,
    settled: verdict.state !== CHECK_STATES.PENDING,
    // classifyChecks compares SHAs only when both are present, so green also requires a well-formed
    // head that the observed SHA matches; otherwise the checks are not proven to be the head's.
    green: sha(headSHA) && headSHA === observedSHA && isGreen(verdict) && verdict.passed > 0,
    excluded: [...excluded].sort(),
  }
}

// The one merge argv. A merge without a head pin is a hazard, so misuse throws rather than
// returning a plausible value.
export function mergeArgv({ sessionId, headSha } = {}) {
  if (typeof sessionId !== 'string' || sessionId.trim() === '')
    throw new TypeError('mergeArgv: sessionId must be a non-empty string')
  if (!sha(headSha)) throw new TypeError('mergeArgv: headSha must be a 40-hex SHA')
  return ['merge', sessionId, '--yes', '--json', '--match-head', headSha]
}

// Maps a commandReader result of the merge argv to `accepted`, `head-mismatch`, or
// `merge-refused:<code>`.
export function classifyMergeResult(read) {
  if (read?.ok === true) return 'accepted'
  if (read?.error === 'HEAD_MISMATCH') return 'head-mismatch'
  return `merge-refused:${read?.error}`
}
