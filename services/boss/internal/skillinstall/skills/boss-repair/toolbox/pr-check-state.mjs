#!/usr/bin/env node

// pr-check-state.mjs — the single agent-callable verdict on "are this PR's checks green".
//
// Every agent-facing surface that reads GitHub check state used to carry its own prose-only rule,
// and those rules disagreed with each other and with the daemon. This module is the agent-side peer
// of `lib/bossalib/vcs/checks_verdict.go` (`EvaluateChecks`), which is the single Go definition.
// The reason vocabulary below is pinned against that file by the sibling test, so a rename on
// either side fails a gate instead of drifting into two different meanings of the word green.
//
// Four states, four distinct non-green shapes:
//
//   absent   — a named gate the prior SHA carried is missing from the head SHA. Waiting on it never
//              resolves, so it is reported by its own reason (`absent-gate`) and never as `pending`
//              with no explanation, and never as `green`.
//   skipped  — the gate attached and deliberately did not run. Compatible with green only when the
//              caller explicitly accepts a set in which nothing ran (`acceptNoGateRan`).
//   pending  — the gate is queued or running. Never `failing`; a merge state degraded only by
//              pending checks, including one degraded by the run's own ready call, is pending too.
//   null-shaped — a rollup node whose conclusion is absent. Reconciled against the same named
//              context in the bucket payload before it is allowed to contribute `unknown`.
//
// Contract:
//   - Pure and offline (R6). No network, no child processes, no `gh`. Callers pass in payloads they
//     already fetched, which is what makes this unit-testable from fixtures alone.
//   - Node builtins only, so it runs from any installed skill toolbox in any repository.
//   - Every time comparison parses both sides to epoch milliseconds (R8). A lexicographic compare of
//     two ISO strings written in different UTC offsets silently matches nothing, which is
//     indistinguishable from a real empty result.

import { readFileSync } from 'node:fs'

// The aggregate states. Exactly four, matching the Go `CheckVerdictState` set.
export const CHECK_STATES = Object.freeze({
  GREEN: 'green',
  FAILING: 'failing',
  PENDING: 'pending',
  UNKNOWN: 'unknown',
})

// The reason vocabulary. The first eight are the Go `CheckVerdictReason*` constants verbatim; the
// last two are agent-side only — the daemon reads one SHA at a time and never calls `gh pr ready`,
// so it has no absent-gate and no post-ready advisory degrade to name.
export const CHECK_REASONS = Object.freeze({
  OK: 'ok',
  NO_GATE_RAN: 'no-gate-ran',
  FAILED: 'failed',
  PENDING: 'pending',
  NO_CHECKS: 'no-checks',
  UNCLASSIFIED: 'unclassified',
  UNREADABLE: 'unreadable',
  STALE_SHA: 'stale-sha',
  ABSENT_GATE: 'absent-gate',
  ADVISORY_UNSETTLED: 'advisory-unsettled',
})

// Liveness of a single in-flight run, from its timestamps alone.
export const RUN_LIVENESS = Object.freeze({
  ALIVE: 'alive',
  STALLED: 'stalled',
  UNKNOWN: 'unknown',
})

// A run whose last update is older than this has stopped reporting. The recorded discriminator is a
// five-minute-elapsed run updated four minutes ago (alive) against a seventy-minute-elapsed run with
// no update at all (the known upload-tail wedge).
export const DEFAULT_STALLED_AFTER_MS = 15 * 60 * 1000

// Per-entry classifications, ordered most-severe first. The order IS the merge precedence: when two
// payloads describe the same named check, the more severe classification wins, except
// `unclassified`, which is the weakest so that any payload naming the same context resolves a
// null-shaped node rather than being outvoted by it.
const SEVERITY = Object.freeze(['failed', 'pending', 'passed', 'skipped', 'unclassified'])

const PENDING_STATUSES = new Set([
  'QUEUED',
  'IN_PROGRESS',
  'PENDING',
  'WAITING',
  'REQUESTED',
  'EXPECTED',
  'STARTED',
])

const FAILED_CONCLUSIONS = new Set([
  'FAILURE',
  'ERROR',
  'CANCELLED',
  'TIMED_OUT',
  'ACTION_REQUIRED',
  'STARTUP_FAILURE',
])

const SKIPPED_CONCLUSIONS = new Set(['NEUTRAL', 'SKIPPED', 'STALE'])

const SUCCESS_CONCLUSIONS = new Set(['SUCCESS'])

function upper(value) {
  return typeof value === 'string' ? value.trim().toUpperCase() : ''
}

function nameOf(node) {
  if (!node || typeof node !== 'object') return ''
  for (const key of ['name', 'context', 'workflowName']) {
    const value = node[key]
    if (typeof value === 'string' && value.trim() !== '') return value.trim()
  }
  return ''
}

// classifyEntry maps one normalized node to a per-entry classification. A completed node with no
// conclusion is `unclassified` — the null-shaped case — not a pass and not a failure.
function classifyEntry({ status, conclusion, bucket }) {
  const st = upper(status)
  const cc = upper(conclusion)
  const bk = upper(bucket)

  if (FAILED_CONCLUSIONS.has(cc)) return 'failed'
  if (bk === 'FAIL' || bk === 'CANCEL') return 'failed'
  if (st !== '' && PENDING_STATUSES.has(st)) return 'pending'
  // A commit status (GraphQL `StatusContext`) carries its whole state in `state`, which
  // `normalizeRollup` folds into `conclusion` — it has no `status` at all. Testing the pending
  // vocabulary only against `status` therefore drops `PENDING` and `EXPECTED` through to
  // `unclassified`, and because `unclassified` is checked before `pending` in the aggregate below,
  // one such node would hold the whole verdict at `unknown`. That is a pending check read as
  // something other than pending — the exact misreading this module exists to stop.
  if (cc !== '' && PENDING_STATUSES.has(cc)) return 'pending'
  if (bk === 'PENDING') return 'pending'
  if (SUCCESS_CONCLUSIONS.has(cc)) return 'passed'
  if (bk === 'PASS') return 'passed'
  if (SKIPPED_CONCLUSIONS.has(cc)) return 'skipped'
  if (bk === 'SKIPPING') return 'skipped'
  return 'unclassified'
}

function asArray(payload, key) {
  if (payload == null) return []
  if (Array.isArray(payload)) {
    // `gh api --paginate --slurp` yields an ARRAY OF PAGE OBJECTS, each carrying the key. Without
    // this branch every page object maps to a nameless entry and is filtered out, so a paginated
    // read silently reports zero checks — which is exactly the too-small-to-prove-green misreading
    // this module exists to close. Flatten the pages instead.
    if (payload.some((page) => page && typeof page === 'object' && Array.isArray(page[key]))) {
      return payload.flatMap((page) =>
        page && typeof page === 'object' && Array.isArray(page[key]) ? page[key] : [],
      )
    }
    return payload
  }
  if (typeof payload === 'object' && Array.isArray(payload[key])) return payload[key]
  return []
}

// normalizeRollup reads a `gh pr view --json statusCheckRollup` payload (the object or the bare
// array). GraphQL splits the vocabulary across `status`/`conclusion` for check runs and `state` for
// commit statuses, so both are carried through.
export function normalizeRollup(payload) {
  return asArray(payload, 'statusCheckRollup')
    .map((node) => ({
      name: nameOf(node),
      status: node?.status ?? '',
      conclusion: node?.conclusion ?? node?.state ?? '',
      source: 'rollup',
    }))
    .filter((entry) => entry.name !== '')
}

// normalizeBuckets reads a `gh pr checks --json name,state,bucket` payload. The bucket view cannot
// separate a gate that ran and passed from one that never attached — that is what the two-SHA
// comparison below is for — but it IS the payload that resolves a null-shaped rollup node.
export function normalizeBuckets(payload) {
  return asArray(payload, 'checks')
    .map((node) => ({
      name: nameOf(node),
      status: node?.status ?? '',
      conclusion: node?.state ?? '',
      bucket: node?.bucket ?? '',
      source: 'buckets',
    }))
    .filter((entry) => entry.name !== '')
}

// normalizeCheckRuns reads a `gh api repos/{owner}/{repo}/commits/{sha}/check-runs` payload. This is
// the only one of the three keyed to an explicit SHA, which is what makes it the authority on which
// contexts attached to that SHA at all.
export function normalizeCheckRuns(payload) {
  return asArray(payload, 'check_runs')
    .map((node) => ({
      name: nameOf(node),
      status: node?.status ?? '',
      conclusion: node?.conclusion ?? '',
      source: 'check-runs',
    }))
    .filter((entry) => entry.name !== '')
}

// mergeEntries folds every payload into one name → classification map. Reading order is
// rollup, then the SHA-keyed check runs, then the buckets; severity decides the winner, so the
// order only settles ties between equally severe readings.
function mergeEntries(sources) {
  const merged = new Map()
  for (const entry of sources) {
    const kind = classifyEntry(entry)
    const prior = merged.get(entry.name)
    if (prior === undefined || SEVERITY.indexOf(kind) < SEVERITY.indexOf(prior.kind)) {
      merged.set(entry.name, { kind, name: entry.name })
    }
  }
  return merged
}

// contextNames extracts the set of named contexts from any of the three payload shapes, or from a
// bare array of names. Used for the two-SHA comparison, where only the names matter.
export function contextNames(payload) {
  if (payload == null) return []
  if (Array.isArray(payload) && payload.every((v) => typeof v === 'string')) {
    return payload.map((v) => v.trim()).filter((v) => v !== '')
  }
  const entries = [
    ...normalizeRollup(payload),
    ...normalizeCheckRuns(payload),
    ...normalizeBuckets(payload),
  ]
  return [...new Set(entries.map((e) => e.name))]
}

// normalizeDiffSide accepts any of the three payload shapes, a bare array of context names, or an
// already-merged entry list, so the comparison reads the same whichever the caller has to hand.
function normalizeDiffSide(side) {
  if (Array.isArray(side) && side.every((v) => typeof v === 'string')) {
    return side
      .map((name) => name.trim())
      .filter((n) => n !== '')
      .map((name) => ({ name, kind: 'unclassified' }))
  }
  if (Array.isArray(side) && side.every((v) => v && typeof v === 'object' && 'kind' in v)) {
    return side
  }
  return [
    ...mergeEntries([
      ...normalizeRollup(side),
      ...normalizeCheckRuns(side),
      ...normalizeBuckets(side),
    ]).values(),
  ]
}

// diffCheckSets is the two-SHA comparison that closes the absent-versus-pending gap. A path-filtered
// follow-up push shrinks the check set, and the jobs that vanish look identical to queued ones;
// comparing the head SHA's contexts against the prior SHA's is what tells them apart — a longer
// wait never can, because waiting on an absent gate does not resolve.
//
// `priorKnown` is false when the caller has no prior SHA recorded. That is not the same as "nothing
// is absent": it means completeness was never established, so a green verdict built on it is not
// proof of green (see `provesGreen`).
export function diffCheckSets({ head = null, prior = null } = {}) {
  const headEntries = normalizeDiffSide(head)

  const priorKnown = prior != null
  const priorNames = priorKnown ? contextNames(prior) : []
  const headNames = new Set(headEntries.map((e) => e.name))

  const ran = []
  const pending = []
  for (const entry of headEntries) {
    if (entry.kind === 'pending') pending.push(entry.name)
    else ran.push(entry.name)
  }

  const absent = priorNames.filter((name) => !headNames.has(name))

  return {
    ran: [...new Set(ran)].sort(),
    pending: [...new Set(pending)].sort(),
    absent: [...new Set(absent)].sort(),
    priorKnown,
  }
}

// assertOptions is the shape guard for the two exports below that take it — `classifyChecks` and
// `mergeStateVerdict`. It is NOT applied to every destructured-options export in this module:
// `diffCheckSets` and `runLiveness` are deliberately unguarded, because neither has the sibling
// affordance described below that makes the wrong call look right. Stated narrowly on purpose — a
// claim of module-wide coverage would have a future export inherit a guard it never received. A
// helper handed
// the wrong argument SHAPE must not answer anyway: a positional array destructures to all-defaults
// and returns a well-formed `no-checks`/`unknown` verdict for an all-green PR, and the caller acts on
// it. The sibling `contextNames(norm)` in the same expression DOES accept the bare array, which is
// exactly what makes the wrong call look right at the call site.
//
// `undefined` stays legal: `classifyChecks()` with no argument is the documented all-defaults call
// and every field is optional. What is refused is a non-object, and an object carrying none of the
// keys the function reads — which cannot carry the information the function needs.
//
// Message form is `<module>: <fn>(<expected shape>) — <what was actually passed>`, matching
// `skill-config.mjs`'s `assertConfigFirst`. That guard is a hand-synced idiom rather than a shared
// import on purpose: a new shared file would have to be added to every `VENDOR_MAP` entry whose
// skill ships an importer, and a missed entry breaks import resolution inside an installed toolbox.
function describeArgument(value) {
  if (value === null) return 'null'
  if (Array.isArray(value)) return `an array of ${value.length} item(s)`
  if (typeof value !== 'object') return `a ${typeof value}`
  const keys = Object.keys(value)
  return keys.length === 0 ? 'an empty object' : `an object with keys ${keys.join(', ')}`
}

function assertOptions(value, fn, expectedKeys) {
  if (value === undefined) return
  const shape = `{${expectedKeys.join(', ')}}`
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(
      `pr-check-state: ${fn}(${shape}) — expected a single options object, got ${describeArgument(value)}`,
    )
  }
  if (!expectedKeys.some((key) => key in value)) {
    throw new Error(
      `pr-check-state: ${fn}(${shape}) — the options object carries none of those keys; got ${describeArgument(value)}`,
    )
  }
}

const CLASSIFY_CHECKS_KEYS = Object.freeze([
  'headSHA',
  'observedSHA',
  'rollup',
  'buckets',
  'checkRuns',
  'priorContexts',
  'readError',
  'acceptNoGateRan',
])

const MERGE_STATE_KEYS = Object.freeze([
  'mergeState',
  'mergeStateStatus',
  'checkVerdict',
  'unresolvedThreads',
  'readiedThisRun',
])

// classifyChecks is the aggregate verdict. It mirrors the Go `EvaluateChecks` switch and adds the
// two discriminators the agent side needs and the daemon does not: the absent-gate arm, and the
// null-shaped reconcile that happens before `unclassified` can be counted.
//
// Inputs (all optional; every one of them missing is `unknown`, never green):
//   headSHA        the SHA the caller believes is the PR head
//   observedSHA    the SHA the payloads were actually read against
//   rollup         `gh pr view --json statusCheckRollup`
//   buckets        `gh pr checks --json name,state,bucket`
//   checkRuns      `gh api repos/{owner}/{repo}/commits/{sha}/check-runs`
//   priorContexts  the prior SHA's payload, or a bare array of its context names
//   readError      any non-null value means the read failed; the verdict is `unreadable`
//   acceptNoGateRan  the caller explicitly accepts a set in which nothing ran
export function classifyChecks(options) {
  assertOptions(options, 'classifyChecks', CLASSIFY_CHECKS_KEYS)
  const {
    headSHA = '',
    observedSHA = '',
    rollup = null,
    buckets = null,
    checkRuns = null,
    priorContexts = null,
    readError = null,
    acceptNoGateRan = false,
  } = options ?? {}
  const merged = mergeEntries([
    ...normalizeRollup(rollup),
    ...normalizeCheckRuns(checkRuns),
    ...normalizeBuckets(buckets),
  ])

  const counts = { passed: 0, failed: 0, pending: 0, skipped: 0, unclassified: 0 }
  for (const { kind } of merged.values()) counts[kind] += 1

  const diff = diffCheckSets({ head: [...merged.values()], prior: priorContexts })

  const verdict = {
    state: CHECK_STATES.UNKNOWN,
    reason: CHECK_REASONS.NO_CHECKS,
    headSHA,
    observedSHA,
    total: merged.size,
    ...counts,
    absent: diff.absent,
    priorKnown: diff.priorKnown,
  }

  if (readError != null) {
    verdict.state = CHECK_STATES.UNKNOWN
    verdict.reason = CHECK_REASONS.UNREADABLE
  } else if (headSHA !== '' && observedSHA !== '' && headSHA !== observedSHA) {
    verdict.state = CHECK_STATES.UNKNOWN
    verdict.reason = CHECK_REASONS.STALE_SHA
  } else if (counts.failed > 0) {
    verdict.state = CHECK_STATES.FAILING
    verdict.reason = CHECK_REASONS.FAILED
  } else if (counts.unclassified > 0) {
    verdict.state = CHECK_STATES.UNKNOWN
    verdict.reason = CHECK_REASONS.UNCLASSIFIED
  } else if (diff.absent.length > 0) {
    // Absent before pending: both are non-green, but only one of them resolves by waiting, and
    // reporting the wrong one is what sends a run into an unbounded wait for a job that will never
    // report.
    verdict.state = CHECK_STATES.PENDING
    verdict.reason = CHECK_REASONS.ABSENT_GATE
  } else if (counts.pending > 0) {
    verdict.state = CHECK_STATES.PENDING
    verdict.reason = CHECK_REASONS.PENDING
  } else if (merged.size === 0) {
    verdict.state = CHECK_STATES.UNKNOWN
    verdict.reason = CHECK_REASONS.NO_CHECKS
  } else if (counts.passed === 0) {
    // Nothing ran. The Go verdict calls this green because the daemon's callers accept a
    // non-blocking set; on the agent side the default is the opposite, because "every substantive
    // gate was skipped" is one of the two directions this class fails in.
    verdict.state = acceptNoGateRan ? CHECK_STATES.GREEN : CHECK_STATES.UNKNOWN
    verdict.reason = CHECK_REASONS.NO_GATE_RAN
  } else {
    verdict.state = CHECK_STATES.GREEN
    verdict.reason = CHECK_REASONS.OK
  }

  return verdict
}

// verdictAt binds a verdict to a SHA, mirroring the Go `CheckVerdict.At`. A verdict read against a
// SHA other than the head is `unknown`, never carried forward as its old state.
export function verdictAt(verdict, sha) {
  const bound = { ...verdict }
  const observed = bound.observedSHA || bound.headSHA
  if (observed !== '' && sha !== '' && observed === sha) return bound
  bound.state = CHECK_STATES.UNKNOWN
  bound.reason = CHECK_REASONS.STALE_SHA
  return bound
}

// isGreen is the state predicate. `unknown` is never green and never red.
export function isGreen(verdict) {
  return verdict?.state === CHECK_STATES.GREEN
}

// provesGreen is the stronger predicate: this check set is evidence the head passed its gates. It
// needs a green state, at least one gate that actually ran, and an established completeness claim —
// without a prior SHA to compare against, the head set may simply be too small to prove anything.
export function provesGreen(verdict) {
  return isGreen(verdict) && verdict.passed > 0 && verdict.priorKnown === true
}

// Why `provesGreen` is false, as a named token. The bare boolean collapsed two different situations
// with two different remedies into one `false`: "no prior SHA was recorded, so completeness was never
// established" is fixed by recording one, while "the head set is missing a gate the prior SHA
// carried" never resolves by waiting and needs the absent job re-triggered. A caller reading only the
// boolean cannot tell them apart and has no way to pick.
//
// ADDITIVE ONLY. `provesGreen`'s boolean is deliberately left exactly as it was — this function
// reports on a verdict, it does not participate in computing one.
export const PROVES_GREEN_REASONS = Object.freeze({
  OK: 'ok',
  NO_PRIOR_SHA: 'no-prior-sha',
  INCOMPLETE_HEAD_SET: 'incomplete-head-set',
  NOT_GREEN: 'not-green',
  NO_GATE_RAN: 'no-gate-ran',
})

export function provesGreenReason(verdict) {
  if (verdict == null || typeof verdict !== 'object') return PROVES_GREEN_REASONS.NOT_GREEN
  // Ordered by the remedy the caller must actually perform, and each rung reads ONE field's own
  // value rather than reconstructing a state from a pair:
  //
  //   1. `reason === 'absent-gate'` — the verdict's own account of why it is not green is a job the
  //      head set is missing. That is the one cause that never resolves by waiting, so it is named
  //      ahead of the generic non-greenness it is an instance of.
  //   2. Not green for any other reason — a failure, a pending gate, an unreadable read. The remedy
  //      is that failure, and this rung sits ahead of `NO_PRIOR_SHA` deliberately: the shipped
  //      recipes pass `--prior` into a fresh mktemp dir, so `priorKnown:false` is the ordinary
  //      first-push shape, and reporting red CI as "record a prior SHA" named a remedy that cannot
  //      help while the actual failure went unreported.
  //   3. Only a set with nothing else wrong with it is merely short of a completeness claim.
  //
  // Deciding rung 1 on `reason` rather than on `absent.length > 0` is what keeps rungs 1 and 2 from
  // trading places: a FAILING set can carry a non-empty `absent` too (measured), and reporting that
  // as an incomplete head set would send the caller to re-trigger a missing job while a gate it
  // already ran was red.
  if (verdict.reason === 'absent-gate') return PROVES_GREEN_REASONS.INCOMPLETE_HEAD_SET
  if (!isGreen(verdict)) return PROVES_GREEN_REASONS.NOT_GREEN
  if (verdict.priorKnown !== true) return PROVES_GREEN_REASONS.NO_PRIOR_SHA
  if (!(verdict.passed > 0)) return PROVES_GREEN_REASONS.NO_GATE_RAN
  return PROVES_GREEN_REASONS.OK
}

// The boolean and the reason are one predicate reported two ways, and `main()` prints BOTH onto the
// same JSON line — so `provesGreen(v) === (provesGreenReason(v) === OK)` has to hold, or that line
// carries `provesGreen:true` beside a named failure reason: this ticket's own misleading-verdict
// class, arriving inside the helper added to end it. Exported so the suite asserts it over the
// verdicts `classifyChecks` actually produces, instead of leaving the invariant as a comment nothing
// checks. Scoped to those: a hand-built object may hold a self-contradictory pair (`state:'green'`
// with `reason:'absent-gate'`) that no code path here can build, and the two answer it differently
// by construction.
export function provesGreenAgrees(verdict) {
  return provesGreen(verdict) === (provesGreenReason(verdict) === PROVES_GREEN_REASONS.OK)
}

const DIRTY_MERGE_STATES = new Set(['DIRTY', 'CONFLICTING'])
const UNSETTLED_MERGE_STATES = new Set(['BLOCKED', 'BEHIND', 'HAS_HOOKS', 'DRAFT'])

// mergeStateVerdict decides whether a `mergeStateStatus` value is a red-CI signal. `blocking` means
// exactly one thing: this opens a repair cycle. It is deliberately false for every merge state that
// is merely unsettled, because `UNSTABLE` with zero failing checks and zero unresolved threads looks
// identical to real red CI and is not — including the `UNSTABLE` a run induces by calling
// `gh pr ready` itself, which starts the non-draft-only advisory bot on a branch that has not
// changed (`readiedThisRun`).
export function mergeStateVerdict(options) {
  assertOptions(options, 'mergeStateVerdict', MERGE_STATE_KEYS)
  const {
    mergeState = '',
    // `mergeStateStatus` is the field name `gh pr view --json mergeable,mergeStateStatus` actually
    // returns, and handing that object straight in is the obvious call. It used to destructure to
    // `mergeState: ''` and report `{state:unknown, reason:unreadable}` — a verdict, not an error, so
    // only the `merge-state` CLI subcommand was really callable. Accepting the gh spelling as an
    // alias makes the obvious call CORRECT rather than merely failing better; `assertOptions` above
    // is what still refuses an object carrying NEITHER key.
    mergeStateStatus = undefined,
    checkVerdict = null,
    unresolvedThreads = 0,
    readiedThisRun = false,
  } = options ?? {}
  const state = upper(
    mergeState === '' && mergeStateStatus !== undefined ? mergeStateStatus : mergeState,
  )
  const red = (reason) => ({
    blocking: true,
    state: CHECK_STATES.FAILING,
    reason,
    mergeState: state,
  })

  if (checkVerdict?.state === CHECK_STATES.FAILING) return red(CHECK_REASONS.FAILED)
  if (Number(unresolvedThreads) > 0) return red(CHECK_REASONS.FAILED)
  if (DIRTY_MERGE_STATES.has(state)) return red(CHECK_REASONS.FAILED)

  if (state === 'UNSTABLE') {
    return {
      blocking: false,
      state: CHECK_STATES.PENDING,
      reason: readiedThisRun ? CHECK_REASONS.ADVISORY_UNSETTLED : CHECK_REASONS.PENDING,
      mergeState: state,
    }
  }

  if (UNSETTLED_MERGE_STATES.has(state)) {
    return {
      blocking: false,
      state: CHECK_STATES.PENDING,
      reason: CHECK_REASONS.PENDING,
      mergeState: state,
    }
  }

  if (state === 'CLEAN') {
    // `CLEAN` is GitHub's mergeability signal, not a check verdict. It never upgrades an unknown or
    // pending check set into green on its own.
    return {
      blocking: false,
      state: checkVerdict?.state ?? CHECK_STATES.UNKNOWN,
      reason: checkVerdict?.reason ?? CHECK_REASONS.UNREADABLE,
      mergeState: state,
    }
  }

  if (state === '' || state === 'UNKNOWN') {
    return {
      blocking: false,
      state: CHECK_STATES.UNKNOWN,
      reason: CHECK_REASONS.UNREADABLE,
      mergeState: state,
    }
  }

  return {
    blocking: false,
    state: CHECK_STATES.UNKNOWN,
    reason: CHECK_REASONS.UNCLASSIFIED,
    mergeState: state,
  }
}

// epochMs parses a timestamp to epoch milliseconds, or null when it is not a timestamp. Every
// comparison in this module goes through it: `%cI` renders in the commit's local offset while the
// GitHub API returns Zulu, so a string compare between the two is lexicographic nonsense that
// silently matches nothing.
export function epochMs(value) {
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value !== 'string' || value.trim() === '') return null
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? null : parsed
}

// compareInstants orders two timestamps by instant. Returns null when either side is unparseable —
// never 0, which would read as "equal" and let an unreadable value pass a same-instant test.
export function compareInstants(a, b) {
  const left = epochMs(a)
  const right = epochMs(b)
  if (left === null || right === null) return null
  if (left < right) return -1
  if (left > right) return 1
  return 0
}

// runLiveness tells a healthy pending CI run from a wedged one, from timestamps alone. A pending
// state and a wedged one are otherwise indistinguishable without reading the run.
export function runLiveness({
  startedAt = null,
  updatedAt = null,
  now = Date.now(),
  stalledAfterMs = DEFAULT_STALLED_AFTER_MS,
} = {}) {
  const startedMs = epochMs(startedAt)
  const nowMs = epochMs(now)
  // A run that has never been updated is as fresh as its start; that is the seventy-minute wedge's
  // exact shape, and it must be judged on elapsed time rather than treated as unreadable.
  const updatedMs = epochMs(updatedAt) ?? startedMs

  if (startedMs === null || updatedMs === null || nowMs === null) {
    return {
      state: RUN_LIVENESS.UNKNOWN,
      startedMs,
      updatedMs,
      elapsedMs: null,
      sinceUpdateMs: null,
    }
  }

  // Ordered by instant, not by string: the two sides routinely arrive in different UTC offsets.
  const latestMs = compareInstants(updatedMs, startedMs) === -1 ? startedMs : updatedMs
  const elapsedMs = nowMs - startedMs
  const sinceUpdateMs = nowMs - latestMs

  return {
    state: sinceUpdateMs >= stalledAfterMs ? RUN_LIVENESS.STALLED : RUN_LIVENESS.ALIVE,
    startedMs,
    updatedMs: latestMs,
    elapsedMs,
    sinceUpdateMs,
  }
}

// ---------------------------------------------------------------------------
// CLI entry point. Reads already-fetched payloads from files (or `-` for stdin) and prints one JSON
// line. It shells out to nothing and opens no socket; the caller runs `gh` and hands the bytes over.

function parseFlags(argv) {
  const flags = {}
  for (let i = 0; i < argv.length; i += 1) {
    const key = argv[i]
    if (typeof key !== 'string' || !key.startsWith('--')) continue
    const next = argv[i + 1]
    if (typeof next !== 'string' || next.startsWith('--')) {
      flags[key.slice(2)] = true
    } else {
      flags[key.slice(2)] = next
      i += 1
    }
  }
  return flags
}

// The flags are documented as a PATH or `-` for stdin. Handing one inline JSON instead is the
// natural mistake, and it used to produce two different wrong answers from the same wrong call: a
// short payload was swallowed as ENOENT → null → `no-checks`/exit 0 (an agent reads that as "the
// gates are not ready yet"), while a long one threw a raw ENAMETOOLONG straight past the ENOENT
// guard. Neither named the mistake. Inline JSON is now simply accepted, and every OTHER unreadable
// value raises naming the path-or-`-` contract.
const PAYLOAD_SHAPE = '<path>|- (stdin)|inline JSON'

function readPayload(flags, name) {
  const value = flags[name]
  if (value === undefined || value === true || value === '') return null
  // Inline arm: a value whose first non-space character opens a JSON array or object was never a
  // path. Decided by the leading character rather than by a failed stat, so it holds for a payload
  // far past the filesystem's name-length limit too.
  const trimmed = typeof value === 'string' ? value.trim() : ''
  if (trimmed.startsWith('[') || trimmed.startsWith('{')) {
    try {
      return JSON.parse(trimmed)
    } catch (err) {
      throw new Error(
        `pr-check-state: --${name} (${PAYLOAD_SHAPE}) — inline JSON payload is malformed: ${err?.message ?? err}`,
      )
    }
  }
  let raw
  try {
    raw = value === '-' ? readFileSync(0, 'utf8') : readFileSync(value, 'utf8')
  } catch (err) {
    // A payload file that is not there is an ABSENT input, not a reason to abort with no verdict at
    // all. Degrading to null keeps the documented fail-closed reading — a missing --prior reports
    // priorKnown:false / provesGreen:false, a missing --checks reports no-checks/unknown — and never
    // reports green. The miss goes to stderr so a mistyped path stays visible without corrupting the
    // single JSON line on stdout. This arm is deliberately NARROW: it covers a path that is simply
    // not there (the shipped recipes pass --prior unconditionally into a fresh mktemp dir), and
    // nothing else. Every other read failure — ENAMETOOLONG, EISDIR, EACCES — is a value that is not
    // a usable path at all, and raises by name instead of reaching a verdict.
    if (err?.code === 'ENOENT') {
      process.stderr.write(`pr-check-state: --${name} payload not found: ${value}\n`)
      return null
    }
    throw new Error(
      `pr-check-state: --${name} (${PAYLOAD_SHAPE}) — cannot read payload ${JSON.stringify(value)}: ${err?.code ?? err?.message ?? err}`,
    )
  }
  if (raw.trim() === '') return null
  return JSON.parse(raw)
}

export function main(argv) {
  const [cmd, ...rest] = argv
  const flags = parseFlags(rest)

  if (cmd === 'classify') {
    const verdict = classifyChecks({
      headSHA: typeof flags['head-sha'] === 'string' ? flags['head-sha'] : '',
      observedSHA: typeof flags['observed-sha'] === 'string' ? flags['observed-sha'] : '',
      rollup: readPayload(flags, 'rollup'),
      buckets: readPayload(flags, 'checks'),
      checkRuns: readPayload(flags, 'check-runs'),
      priorContexts: readPayload(flags, 'prior'),
      readError: typeof flags['read-error'] === 'string' ? flags['read-error'] : null,
      acceptNoGateRan: flags['accept-no-gate-ran'] === true,
    })
    return {
      ...verdict,
      green: isGreen(verdict),
      provesGreen: provesGreen(verdict),
      provesGreenReason: provesGreenReason(verdict),
    }
  }

  if (cmd === 'merge-state') {
    const checkVerdict =
      typeof flags['check-state'] === 'string'
        ? { state: flags['check-state'], reason: flags['check-reason'] ?? CHECK_REASONS.UNREADABLE }
        : null
    return mergeStateVerdict({
      mergeState: typeof flags['merge-state'] === 'string' ? flags['merge-state'] : '',
      checkVerdict,
      unresolvedThreads: Number(flags['unresolved-threads'] ?? 0),
      readiedThisRun: flags['readied-this-run'] === true,
    })
  }

  if (cmd === 'liveness') {
    return runLiveness({
      startedAt: typeof flags['started-at'] === 'string' ? flags['started-at'] : null,
      updatedAt: typeof flags['updated-at'] === 'string' ? flags['updated-at'] : null,
      now: typeof flags.now === 'string' ? flags.now : Date.now(),
      stalledAfterMs: Number(flags['stalled-after-ms'] ?? DEFAULT_STALLED_AFTER_MS),
    })
  }

  throw new Error(
    `unknown command: ${cmd ?? '(none)'} (expected "classify", "merge-state", or "liveness")`,
  )
}

import { isMainModule } from './main-module.mjs'

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(`${JSON.stringify(main(process.argv.slice(2)))}\n`)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 1
  }
}
