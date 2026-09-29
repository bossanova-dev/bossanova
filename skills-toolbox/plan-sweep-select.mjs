// plan-sweep-select.mjs
//
// Which queued `agent-plan` ticket does the planning sweep take next, and does the
// defect a ticket names still exist in the code?
//
// Two verbs, one module:
//
//   select     — the ranking ladder bs-sweep-plan used to re-derive in prose on every
//                run. It is LAZY: the caller hands over what it has already read, and the
//                helper either selects or names exactly the next read that can still
//                change the answer (`need`). The caller answers and calls again.
//   referents  — a fail-open proof that every code referent a defect ticket cites was
//                REMOVED from the code of a ref (absent now, present once, not renamed).
//
// THE RANKING LADDER (each key breaks ties of the one above):
//   1. effective priority tier — own priority, raised to the best priority of any QUEUED
//      ticket it transitively blocks (rule `blocker-inherits-priority`), 1 > 2 > 3 > 4 > none;
//      only the best tier competes;
//   2. readiness — no queued blocker and every external blocker in a cleared state. It
//      ORDERS and never excludes: a tier in which every member is blocked still yields a pick;
//   3. bucket (none tier only) — the first `durable` in rank order, asked one at a time; a
//      per-defect bucket list reduces to `durable` if any element is (`worst-contained-failure`);
//   4. transitive unlock count over queued dependents, descending;
//   5. oldest `createdAt`;
//   6. id.
//
// TERMINATION. Every `need` names only facts the input does not hold yet, and every answer —
// including the explicit `"unreadable"` — makes that fact known. The unknown set is finite and
// strictly shrinks, so a caller that answers every round reaches `select` or `empty`.
//
// Node built-ins plus two import-only owners: dag-scheduler.mjs owns the graph and the unlock
// count, linear-deps-lib.mjs owns the cleared-blocker rule. Neither is edited here.

import { readFileSync } from 'node:fs'
import { runGit } from './base-drift.mjs'
import { buildGraph, transitiveDependentCounts, transitiveDependents } from './dag-scheduler.mjs'
import { BLOCKER_CLEARED_STATE_TYPES } from './linear-deps-lib.mjs'
import { isMainModule } from './main-module.mjs'

// Most- to least-urgent; Linear's integer, 0 = none. Kept local (dag-scheduler's copy is
// internal); agreement with readyTickets on the shared keys is asserted by the test suite.
const PRIORITY_ORDER = [1, 2, 3, 4, 0]
const NONE_RANK = PRIORITY_ORDER.indexOf(0)
const BUCKETS = new Set(['durable', 'in-run'])
const UNREADABLE = 'unreadable'

/** A caller error: the input does not match the wire contract. The CLI maps it to exit 2. */
export class UsageError extends Error {}

const rankOf = (priority) => PRIORITY_ORDER.indexOf(priority)
const isPlainObject = (value) =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
const isIdList = (value) => Array.isArray(value) && value.every((id) => typeof id === 'string')

function validate(input) {
  if (!isPlainObject(input)) throw new UsageError('input must be a JSON object')
  const { candidates, blockerStates = {} } = input
  if (!Array.isArray(candidates)) throw new UsageError('`candidates` must be an array')
  if (!isPlainObject(blockerStates)) throw new UsageError('`blockerStates` must be an object')
  for (const [id, state] of Object.entries(blockerStates)) {
    if (typeof state !== 'string' || state === '') {
      throw new UsageError(`blockerStates.${id} must be a state type or "${UNREADABLE}"`)
    }
  }
  const seen = new Set()
  for (const candidate of candidates) {
    if (!isPlainObject(candidate) || typeof candidate.id !== 'string' || candidate.id === '') {
      throw new UsageError('every candidate needs a non-empty string `id`')
    }
    const { id } = candidate
    if (seen.has(id)) throw new UsageError(`duplicate candidate ${id}`)
    seen.add(id)
    const priority = candidate.priority ?? 0
    if (!Number.isInteger(priority) || rankOf(priority) === -1) {
      throw new UsageError(`${id}: priority must be an integer 0-4`)
    }
    if (candidate.createdAt != null && Number.isNaN(new Date(candidate.createdAt).getTime())) {
      throw new UsageError(`${id}: unparseable createdAt`)
    }
    if (candidate.relations !== undefined && candidate.relations !== UNREADABLE) {
      throw new UsageError(`${id}: relations may only be "${UNREADABLE}"`)
    }
    for (const key of ['blockedBy', 'blocks']) {
      if (candidate[key] !== undefined && !isIdList(candidate[key])) {
        throw new UsageError(`${id}: ${key} must be a list of ids`)
      }
    }
    const buckets = candidate.bucket === undefined ? [] : [candidate.bucket].flat()
    if (
      (Array.isArray(candidate.bucket) && buckets.length === 0) ||
      buckets.some((b) => !BUCKETS.has(b))
    ) {
      throw new UsageError(`${id}: bucket must be "durable", "in-run", or a non-empty list of them`)
    }
  }
  return { candidates, blockerStates }
}

/** Rule `worst-contained-failure`: a per-defect list is durable if any defect is. */
export function reduceBucket(bucket) {
  if (bucket === undefined) return undefined
  return [bucket].flat().includes('durable') ? 'durable' : 'in-run'
}

const relationsKnown = (c) => c.relations === UNREADABLE || Array.isArray(c.blockedBy)

// Union of every supplied `blockedBy` and `blocks`, so an edge reported on one side still counts.
// Returns Map<id, Set<blocker id>> for every candidate (external ids included as blockers).
function unionBlockers(candidates) {
  const queued = new Set(candidates.map((c) => c.id))
  const blockers = new Map(candidates.map((c) => [c.id, new Set()]))
  for (const c of candidates) {
    if (c.relations === UNREADABLE) continue
    for (const b of c.blockedBy ?? []) if (b !== c.id) blockers.get(c.id).add(b)
    for (const d of c.blocks ?? []) if (d !== c.id && queued.has(d)) blockers.get(d).add(c.id)
  }
  return blockers
}

const byId = (a, b) => (a < b ? -1 : a > b ? 1 : 0)

function byCreatedAt(a, b) {
  const diff = new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime()
  return Number.isNaN(diff) ? 0 : diff
}

/**
 * Decide the next candidate, or name the next read that can change the decision.
 * Pure. Throws UsageError on input outside the wire contract.
 * @returns {{action:'need', need:object} | {action:'select', id:string, rationale:object} | {action:'empty'}}
 */
export function selectCandidate(input) {
  const { candidates, blockerStates } = validate(input)
  if (candidates.length === 0) return { action: 'empty' }

  const byIdMap = new Map(candidates.map((c) => [c.id, c]))
  const baseRank = (c) => rankOf(c.priority ?? 0)
  const bestRank = Math.min(...candidates.map(baseRank))

  // Relations: the best base tier, then — transitively — every queued blocker of a candidate
  // whose relations are known. Exactly the set that can change an effective priority.
  let blockers = unionBlockers(candidates)
  const wanted = new Set(candidates.filter((c) => baseRank(c) === bestRank).map((c) => c.id))
  for (let grew = true; grew;) {
    grew = false
    for (const id of [...wanted]) {
      if (!relationsKnown(byIdMap.get(id))) continue
      for (const b of blockers.get(id)) {
        if (byIdMap.has(b) && !wanted.has(b)) {
          wanted.add(b)
          grew = true
        }
      }
    }
  }
  const missingRelations = [...wanted].filter((id) => !relationsKnown(byIdMap.get(id))).sort(byId)
  if (missingRelations.length > 0) return { action: 'need', need: { relations: missingRelations } }

  blockers = unionBlockers(candidates)
  const graph = buildGraph(candidates.map((c) => ({ ...c, blockedBy: [...blockers.get(c.id)] })))

  // Rule `blocker-inherits-priority`: a queued blocker competes at the best tier it gates.
  const effective = new Map()
  for (const c of candidates) {
    let rank = baseRank(c)
    let from = null
    const dependents = [...transitiveDependents(graph, [c.id])].sort(byId)
    for (const d of dependents) {
      if (d !== c.id && baseRank(byIdMap.get(d)) < rank) {
        rank = baseRank(byIdMap.get(d))
        from = d
      }
    }
    effective.set(c.id, { rank, from })
  }
  const tier = candidates.filter((c) => effective.get(c.id).rank === bestRank)

  const missingStates = new Set()
  for (const c of tier) {
    for (const b of graph.externalBlockers.get(c.id)) {
      if (blockerStates[b] === undefined) missingStates.add(b)
    }
  }
  if (missingStates.size > 0) {
    return { action: 'need', need: { blockerState: [...missingStates].sort(byId) } }
  }

  const isReady = (c) =>
    graph.inEpicBlockers.get(c.id).length === 0 &&
    graph.externalBlockers.get(c.id).every((b) => BLOCKER_CLEARED_STATE_TYPES.has(blockerStates[b]))
  const readyGroup = tier.filter(isReady)
  const group = readyGroup.length > 0 ? readyGroup : tier
  const unlocks = transitiveDependentCounts(graph)
  const ordered = [...group].sort(
    (a, b) => unlocks.get(b.id) - unlocks.get(a.id) || byCreatedAt(a, b) || byId(a.id, b.id),
  )

  let pick = ordered[0]
  let bucket
  let bucketDecided = false
  if (bestRank === NONE_RANK) {
    const durable = ordered.find((c) => {
      if (c.bucket === undefined) return true
      return reduceBucket(c.bucket) === 'durable'
    })
    if (durable && durable.bucket === undefined) {
      return { action: 'need', need: { bucket: durable.id } }
    }
    pick = durable ?? ordered[0]
    bucket = reduceBucket(pick.bucket)
    bucketDecided = durable !== undefined && durable !== ordered[0]
  }

  const next = ordered.find((c) => c !== pick)
  let decidedBy = 'only-candidate'
  if (bucketDecided) decidedBy = 'bucket'
  else if (next) {
    if (unlocks.get(next.id) !== unlocks.get(pick.id)) decidedBy = 'unlocks'
    else decidedBy = byCreatedAt(pick, next) !== 0 ? 'createdAt' : 'id'
  } else if (group.length < tier.length) decidedBy = 'readiness'
  else if (candidates.length > 1) decidedBy = 'tier'

  const { from } = effective.get(pick.id)
  const rationale = {
    tier: PRIORITY_ORDER[bestRank],
    tierSource: from === null ? 'own' : 'inherited',
    ...(from === null ? {} : { inheritedFrom: from }),
    ready: readyGroup.length > 0,
    ...(bucket === undefined ? {} : { bucket }),
    unlocks: unlocks.get(pick.id),
    decidedBy,
  }
  const unreadableRelations = candidates.filter((c) => c.relations === UNREADABLE).map((c) => c.id)
  const unreadableStates = Object.keys(blockerStates)
    .filter((id) => blockerStates[id] === UNREADABLE)
    .sort(byId)
  if (unreadableRelations.length + unreadableStates.length > 0) {
    rationale.unreadable = { relations: unreadableRelations, blockerStates: unreadableStates }
  }
  const parents = new Set(candidates.map((c) => c.parentId ?? null))
  const [parent] = parents
  if (parents.size === 1 && typeof parent === 'string' && parent !== '') {
    rationale.sameEpicParent = parent
  }
  return { action: 'select', id: pick.id, rationale }
}

// ---------------------------------------------------------------------------------------------
// referents

// Documentation is excluded from the CONTENT search and the symbol history: committed plans
// routinely quote the very symbols their PRs removed, so a whole-tree search would call every
// removed defect live. A tracked PATH is different: any `ls-tree` hit, a `.md` file included,
// means the referent exists at the ref and was not removed.
const CODE_PATHSPEC = ['--', '.', ':(exclude)docs/', ':(exclude)*.md']

/**
 * Judge whether every cited referent was removed from the code of `ref`.
 * `git(args)` returns `{status, stdout, stderr}`; the default runs git in `cwd`.
 * Never throws; every failure is a verdict.
 * @returns {{verdict:'live'|'stale'|'unknown', reason?:string, ref:string, present?:string[], absent?:string[]}}
 */
export function referentVerdict({ ref, referents = [], git }) {
  const base = { ref }
  const unknown = (reason, extra = {}) => ({ verdict: 'unknown', reason, ...base, ...extra })
  if (referents.length === 0) return unknown('no-referents')

  const resolved = git(['rev-parse', '--verify', '--quiet', `${ref}^{commit}`])
  if (resolved.status === 1) return unknown('ref-unresolvable')
  if (resolved.status !== 0) return unknown('git-failed', { step: 'rev-parse' })
  const commit = resolved.stdout.trim()

  const present = []
  for (const referent of referents) {
    const tree = git([
      '--literal-pathspecs',
      'ls-tree',
      '-r',
      '--name-only',
      commit,
      '--',
      referent,
    ])
    if (tree.status !== 0) return unknown('git-failed', { step: 'ls-tree', referent })
    if (tree.stdout.trim() !== '') {
      present.push(referent)
      continue
    }
    const grep = git(['grep', '-F', '-q', '-e', referent, commit, ...CODE_PATHSPEC])
    if (grep.status === 0) present.push(referent)
    else if (grep.status !== 1) return unknown('git-failed', { step: 'grep', referent })
  }
  if (present.length > 0) return { verdict: 'live', ...base, present }

  // Every referent is absent. Absence alone is also what a typo or a paraphrase looks like, so
  // each one must have code history (it existed, then went), and a removed PATH must not have
  // been a rename source (a moved file still exists).
  for (const referent of referents) {
    const pathLog = git(['--literal-pathspecs', 'log', '-1', '--format=%H', commit, '--', referent])
    if (pathLog.status !== 0) return unknown('git-failed', { step: 'log', referent })
    const remover = pathLog.stdout.trim()
    if (remover !== '') {
      const diff = git([
        'diff-tree',
        '-r',
        '-M',
        '-z',
        '--diff-filter=R',
        '--name-status',
        '--no-commit-id',
        remover,
      ])
      if (diff.status !== 0) return unknown('git-failed', { step: 'diff-tree', referent })
      const fields = diff.stdout.split('\0')
      for (let i = 0; i + 2 < fields.length; i += 3) {
        const source = fields[i + 1]
        if (source === referent || source.startsWith(`${referent.replace(/\/+$/, '')}/`)) {
          return unknown('renamed', { referent, to: fields[i + 2] })
        }
      }
      continue
    }
    const pickaxe = git(['log', '-1', '--format=%H', '-S', referent, commit, ...CODE_PATHSPEC])
    if (pickaxe.status !== 0) return unknown('git-failed', { step: 'log -S', referent })
    if (pickaxe.stdout.trim() === '') return unknown('no-history', { referent })
  }
  return { verdict: 'stale', ...base, absent: [...referents] }
}

// ---------------------------------------------------------------------------------------------
// CLI

const USAGE = [
  'usage: plan-sweep-select.mjs select <path>|-',
  '       plan-sweep-select.mjs referents --ref <tree-ish> -- <referent>...',
  '',
  'Prints one JSON line and EXITS 0 FOR EVERY VERDICT — read `.action` / `.verdict`, never the',
  'exit code. Exit 2 is a usage error only (bad arguments or input outside the wire contract).',
  'select:    {"action":"need","need":{...}} | {"action":"select","id":…} | {"action":"empty"}',
  'referents: {"verdict":"live"|"stale"|"unknown","reason":…}',
].join('\n')

function usageExit(message) {
  process.stderr.write(`plan-sweep-select.mjs: ${message}\n${USAGE}\n`)
  process.exit(2)
}

if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  if (cmd === 'select') {
    const source = rest[0]
    if (source === undefined || source === '') usageExit('select needs <path> or - (stdin)')
    let input
    try {
      input = JSON.parse(readFileSync(source === '-' ? 0 : source, 'utf8'))
    } catch (err) {
      usageExit(`unreadable input: ${err?.message ?? err}`)
    }
    let verdict
    try {
      verdict = selectCandidate(input)
    } catch (err) {
      if (err instanceof UsageError) usageExit(err.message)
      throw err
    }
    process.stdout.write(`${JSON.stringify(verdict)}\n`)
    process.exit(0)
  } else if (cmd === 'referents') {
    const sep = rest.indexOf('--')
    const flags = sep === -1 ? rest : rest.slice(0, sep)
    const referents = sep === -1 ? [] : rest.slice(sep + 1).filter((r) => r !== '')
    const refAt = flags.indexOf('--ref')
    const ref = refAt === -1 ? undefined : flags[refAt + 1]
    if (ref === undefined || ref === '' || ref.startsWith('--')) usageExit('--ref requires a value')
    if (flags.length !== 2) usageExit(`unexpected arguments: ${flags.join(' ')}`)
    const cwd = process.cwd()
    const verdict = referentVerdict({ ref, referents, git: (args) => runGit(cwd, args) })
    process.stdout.write(`${JSON.stringify(verdict)}\n`)
    process.exit(0)
  } else if (cmd === undefined || cmd === 'help' || cmd === '--help' || cmd === '-h') {
    process.stdout.write(`${USAGE}\n`)
    process.exit(0)
  } else {
    usageExit(`unknown command ${cmd}`)
  }
}
