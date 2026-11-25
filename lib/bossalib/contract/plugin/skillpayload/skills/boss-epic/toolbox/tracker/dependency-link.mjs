import { readFileSync } from 'node:fs'
import { setTimeout as sleepTimer } from 'node:timers/promises'
import { parseArgs } from 'node:util'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { OperationFailure, resolveOperation, runOperation } from './operation-runner.mjs'
import { TRACKER_OUTCOME_REASONS, TRACKER_VERDICTS, classifyTrackerOutcome } from './outcome.mjs'
import { settleRead } from './settle-read.mjs'

const EX_USAGE = 64
const DEFAULT_SETTLE_BUDGET_MS = 10_000
const DEFAULT_SETTLE_INTERVAL_MS = 1_000

export const LINK_DEPENDENCY_USAGE = `  link-dependency --issue <issueId> --blocked-by <blockerId> [--verify] [--issue-file <path> --blocker-file <path>] [--settle-ms <n>]
      Check, write once, and verify a blocking edge from both sides; exits 0 linked/already-linked/descriptor-emitted/verification-required, 2 error, 3 refused, 4 verification failed, 64 usage.
`

/** @typedef {{id: string, keys: string[], stateType: string | null}} NormalizedRelationEntry */
/**
 * @typedef {{
 *   identifier: string,
 *   id: string,
 *   uuid: string | null,
 *   keys: Set<string>,
 *   stateType: string | null,
 *   blockedBy: NormalizedRelationEntry[],
 *   blocks: NormalizedRelationEntry[],
 * }} NormalizedIssue
 */
/** @typedef {{issueHasBlockedBy: boolean, blockerHasBlocks: boolean}} LinkSideState */
/** @typedef {{outcome: 'linked' | 'one-sided' | 'missing', exitCode: 0 | 4, state: LinkSideState, problems?: string[]}} LinkEvaluation */
/**
 * @typedef {{
 *   action: 'refuse' | 'skip' | 'fail' | 'write',
 *   outcome?: 'refused-self-edge' | 'refused-cycle' | 'already-linked' | 'one-sided',
 *   exitCode?: 0 | 3 | 4,
 *   state?: LinkSideState,
 *   cycle?: string[],
 *   problems?: string[],
 * }} PreWriteDecision
 */
/** @typedef {{issueNorm: NormalizedIssue, blockerNorm: NormalizedIssue, link: LinkEvaluation}} IssuePair */
/** @typedef {'files' | 'snapshot' | 'getIssue-command' | 'none'} ReadSource */

/** @param {unknown} message */
const oneLine = (message) => String(message ?? '').replace(/[\r\n]+/g, ' ')

/** @param {unknown} value @returns {string | null} */
function nonEmptyString(value) {
  return typeof value === 'string' && value.trim() !== '' ? value.trim() : null
}

/** @param {unknown} obj @returns {string | null} */
function extractStateType(obj) {
  if (!obj || typeof obj !== 'object') return null
  const record = /** @type {Record<string, unknown>} */ (obj)
  const state =
    record.state && typeof record.state === 'object'
      ? /** @type {Record<string, unknown>} */ (record.state)
      : null
  return (
    nonEmptyString(state?.type) ??
    nonEmptyString(record.stateType) ??
    nonEmptyString(record.statusType) ??
    null
  )
}

/** @param {unknown} payload */
function hasRelationArrays(payload) {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return false
  const relations = /** @type {Record<string, unknown>} */ (payload).relations
  if (!relations || typeof relations !== 'object' || Array.isArray(relations)) return false
  const rel = /** @type {Record<string, unknown>} */ (relations)
  return Array.isArray(rel.blockedBy) && Array.isArray(rel.blocks)
}

/** @param {unknown} entry @param {'blockedBy' | 'blocks'} field @param {string} label @returns {NormalizedRelationEntry} */
function normalizeRelationEntry(entry, field, label) {
  if (typeof entry === 'string') {
    const id = nonEmptyString(entry)
    if (!id) throw new Error(`${label}: blank entry in relations.${field}`)
    return { id, keys: [id.toLowerCase()], stateType: null }
  }
  if (!entry || typeof entry !== 'object' || Array.isArray(entry)) {
    throw new Error(`${label}: invalid entry in relations.${field}`)
  }
  const rec = /** @type {Record<string, unknown>} */ (entry)
  const identifier = nonEmptyString(rec.identifier)
  const id = nonEmptyString(rec.id)
  const uuid = nonEmptyString(rec.uuid)
  const canonical = identifier ?? id ?? uuid
  if (!canonical) {
    throw new Error(`${label}: entry in relations.${field} lacks identifier, id, or uuid`)
  }
  const keys = [
    ...new Set(
      [identifier, id, uuid].filter(Boolean).map((v) => /** @type {string} */ (v).toLowerCase()),
    ),
  ]
  return { id: canonical, keys, stateType: extractStateType(rec) }
}

/**
 * Normalize one issue payload from snapshot, GitHub hydrate, or MCP getIssue shape.
 * @param {unknown} payload
 * @param {string} requestedId
 * @param {string} [label]
 * @returns {NormalizedIssue}
 */
export function normalizeIssueRelations(payload, requestedId, label = requestedId) {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) {
    throw new Error(`${label}: issue payload must be a JSON object`)
  }
  const rec = /** @type {Record<string, unknown>} */ (payload)
  const relations = rec.relations
  if (!relations || typeof relations !== 'object' || Array.isArray(relations)) {
    throw new Error(`${label}: missing relations object`)
  }
  const rel = /** @type {Record<string, unknown>} */ (relations)
  if (!Array.isArray(rel.blockedBy)) {
    throw new Error(`${label}: missing relations.blockedBy array`)
  }
  if (!Array.isArray(rel.blocks)) {
    throw new Error(`${label}: missing relations.blocks array`)
  }
  const identifier =
    nonEmptyString(rec.identifier) ??
    nonEmptyString(rec.id) ??
    nonEmptyString(requestedId) ??
    requestedId
  const id = nonEmptyString(rec.id) ?? identifier
  const uuid = nonEmptyString(rec.uuid)
  const req = nonEmptyString(requestedId)
  const keys = new Set(
    [req, identifier, id, uuid].filter(Boolean).map((v) => /** @type {string} */ (v).toLowerCase()),
  )
  return {
    identifier,
    id,
    uuid,
    keys,
    stateType: extractStateType(rec),
    blockedBy: rel.blockedBy.map((entry) => normalizeRelationEntry(entry, 'blockedBy', label)),
    blocks: rel.blocks.map((entry) => normalizeRelationEntry(entry, 'blocks', label)),
  }
}

/** @param {NormalizedIssue} a @param {NormalizedIssue} b */
function sameIssue(a, b) {
  for (const key of a.keys) {
    if (b.keys.has(key)) return true
  }
  return false
}

/** @param {NormalizedRelationEntry[]} entries @param {NormalizedIssue} target */
function listContainsIssue(entries, target) {
  return entries.some((entry) => entry.keys.some((key) => target.keys.has(key)))
}

/** @param {NormalizedRelationEntry} a @param {NormalizedRelationEntry} b */
function entriesMatch(a, b) {
  return a.keys.some((key) => b.keys.includes(key))
}

/**
 * Evaluate whether the `{issue, blockedBy}` edge is present on both sides, one side, or neither.
 * @param {NormalizedIssue} issueNorm
 * @param {NormalizedIssue} blockerNorm
 * @returns {LinkEvaluation}
 */
export function evaluateLinkState(issueNorm, blockerNorm) {
  const issueHasBlockedBy = listContainsIssue(issueNorm.blockedBy, blockerNorm)
  const blockerHasBlocks = listContainsIssue(blockerNorm.blocks, issueNorm)
  const state = { issueHasBlockedBy, blockerHasBlocks }
  if (issueHasBlockedBy && blockerHasBlocks) {
    return { outcome: 'linked', exitCode: 0, state }
  }
  if (issueHasBlockedBy !== blockerHasBlocks) {
    const problem = issueHasBlockedBy
      ? `${blockerNorm.identifier} relations.blocks is missing ${issueNorm.identifier}`
      : `${issueNorm.identifier} relations.blockedBy is missing ${blockerNorm.identifier}`
    return { outcome: 'one-sided', exitCode: 4, state, problems: [problem] }
  }
  return {
    outcome: 'missing',
    exitCode: 4,
    state,
    problems: [
      `${issueNorm.identifier} relations.blockedBy is missing ${blockerNorm.identifier}`,
      `${blockerNorm.identifier} relations.blocks is missing ${issueNorm.identifier}`,
    ],
  }
}

/**
 * Pure pre-write decision over normalized `{issue, blocker}` relations (KTD5).
 * @param {NormalizedIssue} issueNorm
 * @param {NormalizedIssue} blockerNorm
 * @returns {PreWriteDecision}
 */
export function decidePreWrite(issueNorm, blockerNorm) {
  const link = evaluateLinkState(issueNorm, blockerNorm)
  if (sameIssue(issueNorm, blockerNorm)) {
    return {
      action: 'refuse',
      outcome: 'refused-self-edge',
      exitCode: 3,
      state: link.state,
      problems: [`${issueNorm.identifier} cannot be blocked by itself`],
    }
  }
  if (
    listContainsIssue(issueNorm.blocks, blockerNorm) ||
    listContainsIssue(blockerNorm.blockedBy, issueNorm)
  ) {
    return {
      action: 'refuse',
      outcome: 'refused-cycle',
      exitCode: 3,
      state: link.state,
      cycle: [issueNorm.identifier, blockerNorm.identifier],
    }
  }
  for (const issueBlock of issueNorm.blocks) {
    if (blockerNorm.blockedBy.some((blockerDep) => entriesMatch(issueBlock, blockerDep))) {
      return {
        action: 'refuse',
        outcome: 'refused-cycle',
        exitCode: 3,
        state: link.state,
        cycle: [issueNorm.identifier, issueBlock.id, blockerNorm.identifier],
      }
    }
  }
  if (link.outcome === 'linked') {
    return { action: 'skip', outcome: 'already-linked', exitCode: 0, state: link.state }
  }
  if (link.outcome === 'one-sided') {
    return {
      action: 'fail',
      outcome: 'one-sided',
      exitCode: 4,
      state: link.state,
      problems: link.problems,
    }
  }
  return { action: 'write', state: link.state }
}

/** @param {NormalizedIssue | null} issueNorm @param {NormalizedIssue | null} blockerNorm */
function buildRelationsById(issueNorm, blockerNorm) {
  /** @param {NormalizedIssue} norm */
  const toEntry = (norm) => ({
    stateType: norm.stateType,
    blockedBy: norm.blockedBy.map((entry) => ({ id: entry.id, stateType: entry.stateType })),
    blocks: norm.blocks.map((entry) => ({ id: entry.id, stateType: entry.stateType })),
  })
  /** @type {Record<string, {stateType: string | null, blockedBy: {id: string, stateType: string | null}[], blocks: {id: string, stateType: string | null}[]}>} */
  const map = {}
  if (issueNorm) map[issueNorm.identifier] = toEntry(issueNorm)
  if (blockerNorm) map[blockerNorm.identifier] = toEntry(blockerNorm)
  return map
}

/** @param {string} value */
function shellArg(value) {
  return /^[\w./:@-]+$/.test(value) ? value : `'${value.replace(/'/g, `'\\''`)}'`
}

/**
 * @param {string} issue
 * @param {string} blockedBy
 * @param {{source: ReadSource, settleMs: number, settleMsExplicit: boolean}} opts
 */
function buildNextVerifyCommand(issue, blockedBy, { source, settleMs, settleMsExplicit }) {
  const parts = [
    'node tracker/cli.mjs link-dependency',
    '--issue',
    shellArg(issue),
    '--blocked-by',
    shellArg(blockedBy),
    '--verify',
  ]
  if (source === 'files') {
    parts.push('--issue-file <fresh-issue-file>', '--blocker-file <fresh-blocker-file>')
  }
  if (settleMsExplicit) {
    parts.push('--settle-ms', String(settleMs))
  }
  return parts.join(' ')
}

/** @param {string[]} rest */
function parseLinkDependencyFlags(rest) {
  const parsed = parseArgs({
    args: rest,
    strict: true,
    tokens: true,
    allowPositionals: false,
    options: {
      issue: { type: 'string' },
      'blocked-by': { type: 'string' },
      verify: { type: 'boolean' },
      'issue-file': { type: 'string' },
      'blocker-file': { type: 'string' },
      'settle-ms': { type: 'string' },
    },
  })
  const seen = new Set()
  for (const token of parsed.tokens) {
    if (token.kind !== 'option') continue
    if (seen.has(token.name)) throw new Error(`repeated --${token.name}`)
    seen.add(token.name)
    if (token.name !== 'verify') {
      if (typeof token.value !== 'string' || !token.value.trim() || /^--/.test(token.value)) {
        throw new Error(`--${token.name} requires a non-empty value`)
      }
    }
  }
  const values = parsed.values
  const issue = nonEmptyString(values.issue)
  const blockedBy = nonEmptyString(values['blocked-by'])
  if (!issue || !blockedBy) throw new Error('--issue and --blocked-by are required')
  const issueFile = nonEmptyString(values['issue-file'])
  const blockerFile = nonEmptyString(values['blocker-file'])
  if ((issueFile === null) !== (blockerFile === null)) {
    throw new Error('--issue-file and --blocker-file must both be provided')
  }
  let settleMs = DEFAULT_SETTLE_BUDGET_MS
  const settleMsExplicit = values['settle-ms'] !== undefined
  if (settleMsExplicit) {
    const raw = String(values['settle-ms']).trim()
    const num = Number(raw)
    if (!/^\d+$/.test(raw) || !Number.isSafeInteger(num)) {
      throw new Error('--settle-ms must be a non-negative integer')
    }
    settleMs = num
  }
  return {
    issue,
    blockedBy,
    verify: values.verify === true,
    issueFile,
    blockerFile,
    settleMs,
    settleMsExplicit,
  }
}

/** @param {string} filePath */
function readJsonFile(filePath) {
  let raw
  try {
    raw = readFileSync(filePath, 'utf8')
  } catch (error) {
    const msg = error instanceof Error ? error.message : String(error)
    throw new Error(`${filePath}: could not read file (${msg})`)
  }
  try {
    return JSON.parse(raw)
  } catch (error) {
    const msg = error instanceof Error ? error.message : String(error)
    throw new Error(`${filePath}: malformed JSON (${msg})`)
  }
}

/** @param {unknown} issueRaw @param {unknown} blockerRaw @param {string} issueId @param {string} blockerId @param {string} issueLabel @param {string} blockerLabel @returns {IssuePair} */
function toIssuePair(issueRaw, blockerRaw, issueId, blockerId, issueLabel, blockerLabel) {
  const issueNorm = normalizeIssueRelations(issueRaw, issueId, issueLabel)
  const blockerNorm = normalizeIssueRelations(blockerRaw, blockerId, blockerLabel)
  return { issueNorm, blockerNorm, link: evaluateLinkState(issueNorm, blockerNorm) }
}

/**
 * Resolve a live (non-file) KTD2 read source and perform its initial two-issue read.
 * @param {{issue: string, blockedBy: string}} flags
 * @param {{getAdapter: () => any, runOp: typeof runOperation}} deps
 * @returns {Promise<
 *   | {source: ReadSource, initialPair: IssuePair, reader: () => Promise<IssuePair>, fallbackReason: null}
 *   | {source: null, initialPair: null, reader: null, fallbackReason: string}
 * >}
 */
async function resolveLiveReadSource(flags, { getAdapter, runOp }) {
  const adapter = getAdapter()
  let fallbackReason = 'adapter has no readIssueSnapshot capability'
  if (typeof adapter?.readIssueSnapshot === 'function') {
    try {
      const issueSnap = await adapter.readIssueSnapshot(flags.issue)
      const blockerSnap = await adapter.readIssueSnapshot(flags.blockedBy)
      if (hasRelationArrays(issueSnap) && hasRelationArrays(blockerSnap)) {
        const readSnapshot = async () =>
          toIssuePair(
            await adapter.readIssueSnapshot(flags.issue),
            await adapter.readIssueSnapshot(flags.blockedBy),
            flags.issue,
            flags.blockedBy,
            flags.issue,
            flags.blockedBy,
          )
        return {
          source: 'snapshot',
          initialPair: toIssuePair(
            issueSnap,
            blockerSnap,
            flags.issue,
            flags.blockedBy,
            flags.issue,
            flags.blockedBy,
          ),
          reader: readSnapshot,
          fallbackReason: null,
        }
      }
      fallbackReason = 'readIssueSnapshot omitted complete relations'
    } catch (error) {
      if (
        error &&
        typeof error === 'object' &&
        /** @type {{code?: unknown}} */ (error).code === TRACKER_CREDENTIALS_MISSING
      ) {
        fallbackReason = error instanceof Error ? error.message : 'tracker credentials missing'
      } else {
        throw error
      }
    }
  }

  const getIssueOp = resolveOperation(adapter?.operationMap, 'getIssue')
  if (getIssueOp && getIssueOp.transport === 'command') {
    const readCommand = async () => {
      const issueRes = runOp(getIssueOp, { id: flags.issue })
      const blockerRes = runOp(getIssueOp, { id: flags.blockedBy })
      return toIssuePair(
        issueRes?.result,
        blockerRes?.result,
        flags.issue,
        flags.blockedBy,
        flags.issue,
        flags.blockedBy,
      )
    }
    return {
      source: 'getIssue-command',
      initialPair: await readCommand(),
      reader: readCommand,
      fallbackReason: null,
    }
  }

  return { source: null, initialPair: null, reader: null, fallbackReason }
}

/**
 * Resolve the KTD2 read source and perform the initial two-issue read.
 * @param {{issue: string, blockedBy: string, issueFile: string | null, blockerFile: string | null}} flags
 * @param {{getAdapter: () => any, runOp: typeof runOperation}} deps
 * @returns {Promise<{source: ReadSource, initialPair: IssuePair, reader: () => Promise<IssuePair>}>}
 */
async function resolveReadSource(flags, { getAdapter, runOp }) {
  if (flags.issueFile && flags.blockerFile) {
    const { issueFile, blockerFile } = flags
    const readFiles = async () =>
      toIssuePair(
        readJsonFile(issueFile),
        readJsonFile(blockerFile),
        flags.issue,
        flags.blockedBy,
        issueFile,
        blockerFile,
      )
    return { source: 'files', initialPair: await readFiles(), reader: readFiles }
  }

  const live = await resolveLiveReadSource(flags, { getAdapter, runOp })
  if (live.reader) {
    return { source: live.source, initialPair: live.initialPair, reader: live.reader }
  }

  throw new Error(
    `cannot read issue relations directly (${live.fallbackReason}); run the tracker's MCP getIssue operation with relations and statusType for both ${flags.issue} and ${flags.blockedBy}, save each payload as JSON, and re-run with --issue-file <path> --blocker-file <path>`,
  )
}

/** @param {unknown} error */
function classifyWriteFailure(error) {
  // OperationFailure.status is a process exit code or signal, not an HTTP status; strip it so
  // classifyTrackerOutcome parses any HTTP status embedded in the stderr diagnostic instead.
  const observed =
    error instanceof OperationFailure &&
    /** @type {{outcome?: unknown}} */ (/** @type {unknown} */ (error)).outcome !== 'indeterminate'
      ? {
          error: error.message,
          ...(typeof (/** @type {{kind?: unknown}} */ (/** @type {unknown} */ (error)).kind) ===
          'string'
            ? { kind: /** @type {{kind: string}} */ (/** @type {unknown} */ (error)).kind }
            : {}),
        }
      : error
  const verdict = classifyTrackerOutcome(observed, { operation: 'write' })
  const definitive =
    verdict.verdict !== TRACKER_VERDICTS.INDETERMINATE &&
    verdict.reason !== TRACKER_OUTCOME_REASONS.UNRECOGNIZED
  return { verdict, definitive }
}

/**
 * Execute `tracker/cli.mjs link-dependency`.
 * @param {string[]} rest
 * @param {{
 *   write: (chunk: string) => void,
 *   errWrite: (chunk: string) => void,
 *   env?: NodeJS.ProcessEnv,
 *   resolveAdapter?: (opts: {env?: NodeJS.ProcessEnv}) => any,
 *   sleep?: (ms: number) => Promise<void>,
 *   now?: () => number,
 *   runOperation?: typeof runOperation,
 * }} io
 * @returns {Promise<number>}
 */
export async function runLinkDependency(
  rest,
  {
    write,
    errWrite,
    env = process.env,
    resolveAdapter,
    sleep = sleepTimer,
    now = Date.now,
    runOperation: runOp = runOperation,
  },
) {
  const fail = (message, code = 2) => {
    errWrite(`link-dependency: ${oneLine(message)}\n`)
    return code
  }
  /** @param {object} payload @param {number} code */
  const emit = (payload, code) => {
    write(JSON.stringify(payload) + '\n')
    return code
  }

  let flags
  try {
    flags = parseLinkDependencyFlags(rest)
  } catch (error) {
    return fail(`usage: ${error instanceof Error ? error.message : error}`, EX_USAGE)
  }

  if (!flags.verify && flags.issue.toLowerCase() === flags.blockedBy.toLowerCase()) {
    return emit(
      {
        outcome: 'refused-self-edge',
        issue: flags.issue,
        blockedBy: flags.blockedBy,
        source: 'none',
        before: null,
        problems: [`${flags.issue} cannot be blocked by itself`],
        relationsById: {},
      },
      3,
    )
  }

  let cachedAdapter = null
  const getAdapter = () => {
    if (cachedAdapter === null) {
      if (typeof resolveAdapter !== 'function') {
        throw new Error('no tracker adapter resolver available')
      }
      cachedAdapter = resolveAdapter({ env })
    }
    return cachedAdapter
  }

  let source
  let initialPair
  let reader
  try {
    ;({ source, initialPair, reader } = await resolveReadSource(flags, { getAdapter, runOp }))
  } catch (error) {
    return fail(error instanceof Error ? error.message : error, 2)
  }

  /** @param {ReadSource} [readSource] */
  const settleOpts = (readSource = source) => {
    const budgetMs = readSource === 'files' ? 0 : flags.settleMs
    const intervalMs = budgetMs > 0 ? Math.min(DEFAULT_SETTLE_INTERVAL_MS, budgetMs) : 1
    return { budgetMs, intervalMs, sleep, now }
  }

  if (flags.verify) {
    try {
      let first = /** @type {IssuePair | null} */ (initialPair)
      const settled = await settleRead({
        read: async () => {
          if (first) {
            const value = first
            first = null
            return value
          }
          return reader()
        },
        isSettled: (pair) => pair.link.state.issueHasBlockedBy && pair.link.state.blockerHasBlocks,
        ...settleOpts(),
      })
      const { issueNorm, blockerNorm, link } = settled.value
      return emit(
        {
          outcome: link.outcome,
          issue: issueNorm.identifier,
          blockedBy: blockerNorm.identifier,
          source,
          before: null,
          after: link.state,
          attempts: settled.attempts,
          ...(link.problems ? { problems: link.problems } : {}),
          relationsById: buildRelationsById(issueNorm, blockerNorm),
        },
        link.exitCode,
      )
    } catch (error) {
      return fail(error instanceof Error ? error.message : error, 2)
    }
  }

  const { issueNorm, blockerNorm, link: beforeLink } = initialPair
  const before = beforeLink.state
  const decision = decidePreWrite(issueNorm, blockerNorm)
  if (decision.action !== 'write') {
    return emit(
      {
        outcome: decision.outcome,
        issue: issueNorm.identifier,
        blockedBy: blockerNorm.identifier,
        source,
        before,
        ...(decision.cycle ? { cycle: decision.cycle } : {}),
        ...(decision.problems ? { problems: decision.problems } : {}),
        relationsById: buildRelationsById(issueNorm, blockerNorm),
      },
      decision.exitCode ?? 4,
    )
  }

  let appendOp
  try {
    appendOp = resolveOperation(getAdapter()?.operationMap, 'appendDependency')
  } catch (error) {
    return fail(error instanceof Error ? error.message : error, 2)
  }
  if (!appendOp) {
    return fail('adapter has no usable appendDependency operation', 2)
  }

  const writeArgs = { id: issueNorm.identifier, blockedBy: [blockerNorm.identifier] }
  if (appendOp.transport === 'mcp') {
    return emit(
      {
        outcome: 'descriptor-emitted',
        issue: issueNorm.identifier,
        blockedBy: blockerNorm.identifier,
        source,
        before,
        tool: appendOp.tool,
        args: writeArgs,
        next: buildNextVerifyCommand(issueNorm.identifier, blockerNorm.identifier, {
          source,
          settleMs: flags.settleMs,
          settleMsExplicit: flags.settleMsExplicit,
        }),
        relationsById: buildRelationsById(issueNorm, blockerNorm),
      },
      0,
    )
  }

  /** @type {string[] | null} */
  let warnings = null
  /** @type {string | null} */
  let writeError = null
  try {
    const writeRes = runOp(appendOp, writeArgs)
    if (Array.isArray(writeRes?.result?.warnings) && writeRes.result.warnings.length > 0) {
      warnings = writeRes.result.warnings
    }
  } catch (error) {
    const { verdict, definitive } = classifyWriteFailure(error)
    if (definitive) {
      const msg = error instanceof Error ? error.message : String(error)
      return fail(`appendDependency failed (${verdict.reason}): ${msg}`, 2)
    }
    writeError = oneLine(error instanceof Error ? error.message : error)
  }

  try {
    let postSource = source
    let postReader = reader
    let firstPost = /** @type {IssuePair | null} */ (null)
    if (source === 'files') {
      const live = await resolveLiveReadSource(flags, { getAdapter, runOp })
      if (!live.reader) {
        const next = buildNextVerifyCommand(issueNorm.identifier, blockerNorm.identifier, {
          source: 'files',
          settleMs: flags.settleMs,
          settleMsExplicit: flags.settleMsExplicit,
        })
        const relationsById = buildRelationsById(issueNorm, blockerNorm)
        if (writeError) {
          return emit(
            {
              outcome: 'indeterminate',
              issue: issueNorm.identifier,
              blockedBy: blockerNorm.identifier,
              source,
              before,
              next,
              problems: [
                `cannot verify post-write relations directly (${live.fallbackReason}); fetch fresh payloads and run next`,
              ],
              writeError,
              relationsById,
            },
            4,
          )
        }
        return emit(
          {
            outcome: 'verification-required',
            issue: issueNorm.identifier,
            blockedBy: blockerNorm.identifier,
            source,
            before,
            next,
            ...(warnings ? { warnings } : {}),
            relationsById,
          },
          0,
        )
      }
      postSource = live.source
      firstPost = live.initialPair
      postReader = live.reader
    }
    const settled = await settleRead({
      read: async () => {
        if (firstPost) {
          const value = firstPost
          firstPost = null
          return value
        }
        return postReader()
      },
      isSettled: (pair) => pair.link.state.issueHasBlockedBy && pair.link.state.blockerHasBlocks,
      ...settleOpts(postSource),
    })
    const { issueNorm: postIssue, blockerNorm: postBlocker, link: postLink } = settled.value
    const relationsById = buildRelationsById(postIssue, postBlocker)
    if (postLink.outcome === 'linked') {
      return emit(
        {
          outcome: 'linked',
          issue: postIssue.identifier,
          blockedBy: postBlocker.identifier,
          source: postSource,
          before,
          after: postLink.state,
          attempts: settled.attempts,
          ...(warnings ? { warnings } : {}),
          ...(writeError ? { writeError } : {}),
          relationsById,
        },
        0,
      )
    }
    if (writeError) {
      return emit(
        {
          outcome: 'indeterminate',
          issue: postIssue.identifier,
          blockedBy: postBlocker.identifier,
          source: postSource,
          before,
          after: postLink.state,
          attempts: settled.attempts,
          ...(postLink.problems ? { problems: postLink.problems } : {}),
          writeError,
          relationsById,
        },
        4,
      )
    }
    return emit(
      {
        outcome: postLink.outcome,
        issue: postIssue.identifier,
        blockedBy: postBlocker.identifier,
        source: postSource,
        before,
        after: postLink.state,
        attempts: settled.attempts,
        ...(postLink.problems ? { problems: postLink.problems } : {}),
        ...(warnings ? { warnings } : {}),
        relationsById,
      },
      4,
    )
  } catch (error) {
    return fail(error instanceof Error ? error.message : error, 2)
  }
}
