import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { buildGraph, readyTickets } from './dag-scheduler.mjs'
import { reduceBucket, referentVerdict, selectCandidate, UsageError } from './plan-sweep-select.mjs'
import { runGit } from './base-drift.mjs'

const SCRIPT = fileURLToPath(new URL('./plan-sweep-select.mjs', import.meta.url))
const T0 = Date.parse('2026-09-27T13:17:55.000Z')
const at = (seconds) => new Date(T0 + seconds * 1000).toISOString()

// A candidate whose relations are already known to be edge-free unless overridden.
const c = (id, overrides = {}) => ({
  id,
  priority: 0,
  createdAt: at(0),
  blockedBy: [],
  blocks: [],
  ...overrides,
})

const select = (candidates, blockerStates) =>
  selectCandidate({ candidates, ...(blockerStates ? { blockerStates } : {}) })

function runCli(args, { input, cwd } = {}) {
  const res = spawnSync(process.execPath, [SCRIPT, ...args], { input, cwd, encoding: 'utf8' })
  return { status: res.status, stdout: res.stdout, stderr: res.stderr }
}

// Answer every `need` from a fixed truth table until the helper selects, recording each request.
// The loop is bounded so a regression that re-asks forever fails instead of hanging the suite.
function drive(candidates, truth) {
  const working = candidates.map((cand) => ({ ...cand }))
  const blockerStates = {}
  const requests = []
  for (let round = 0; round < 100; round += 1) {
    const result = selectCandidate({ candidates: working, blockerStates })
    if (result.action !== 'need') return { result, requests }
    const { relations, blockerState, bucket } = result.need
    for (const id of relations ?? []) {
      requests.push(`relations:${id}`)
      Object.assign(
        working.find((w) => w.id === id),
        truth.relations[id],
      )
    }
    for (const id of blockerState ?? []) {
      requests.push(`blockerState:${id}`)
      blockerStates[id] = truth.blockerStates[id]
    }
    if (bucket !== undefined) {
      requests.push(`bucket:${bucket}`)
      working.find((w) => w.id === bucket).bucket = truth.buckets[bucket]
    }
  }
  throw new Error('selection did not terminate within 100 rounds')
}

// ---------------------------------------------------------------------------------------------
// select — ranking rungs

test('tier precedence: priority 2 beats priority 3 and none regardless of age', () => {
  const result = select([
    c('BOS-3', { priority: 3, createdAt: at(0) }),
    c('BOS-0', { priority: 0, createdAt: at(1), bucket: 'durable' }),
    c('BOS-2', { priority: 2, createdAt: at(99) }),
  ])
  assert.equal(result.action, 'select')
  assert.equal(result.id, 'BOS-2')
  assert.equal(result.rationale.tier, 2)
  assert.equal(result.rationale.tierSource, 'own')
  assert.equal(result.rationale.decidedBy, 'tier')
})

test('tier precedence: an unprioritized candidate never beats a prioritized one it does not block', () => {
  const result = select([c('BOS-N', { createdAt: at(0) }), c('BOS-4', { priority: 4 })])
  assert.equal(result.id, 'BOS-4')
})

test('ready before blocked: a ready candidate beats an older one whose external blocker is started', () => {
  const candidates = [
    c('BOS-OLD', { priority: 2, createdAt: at(0), blockedBy: ['EXT-1'] }),
    c('BOS-NEW', { priority: 2, createdAt: at(50) }),
  ]
  const blocked = select(candidates, { 'EXT-1': 'started' })
  assert.equal(blocked.id, 'BOS-NEW')
  assert.equal(blocked.rationale.ready, true)
  for (const cleared of ['completed', 'canceled']) {
    const result = select(candidates, { 'EXT-1': cleared })
    assert.equal(result.id, 'BOS-OLD', `${cleared} blocker must clear readiness`)
  }
})

test('readiness is decisive when the only other tier member is blocked', () => {
  const result = select(
    [c('BOS-A', { priority: 1, blockedBy: ['EXT-1'] }), c('BOS-B', { priority: 1 })],
    { 'EXT-1': 'started' },
  )
  assert.equal(result.id, 'BOS-B')
  assert.equal(result.rationale.decidedBy, 'readiness')
})

test('all blocked still selects: readiness orders and never excludes', () => {
  const result = select(
    [
      c('BOS-A', { priority: 2, createdAt: at(5), blockedBy: ['EXT-1'] }),
      c('BOS-B', { priority: 2, createdAt: at(1), blockedBy: ['EXT-2'] }),
    ],
    { 'EXT-1': 'started', 'EXT-2': 'unstarted' },
  )
  assert.equal(result.action, 'select')
  assert.equal(result.id, 'BOS-B')
  assert.equal(result.rationale.ready, false)
})

test('blocker-inherits-priority: P3 -> P2 -> P1 chain selects the P3 root at inherited tier 1', () => {
  const result = select([
    c('A', { priority: 3, createdAt: at(30) }),
    c('B', { priority: 2, createdAt: at(20), blockedBy: ['A'] }),
    c('C', { priority: 1, createdAt: at(10), blockedBy: ['B'] }),
  ])
  assert.equal(result.id, 'A')
  assert.equal(result.rationale.tier, 1)
  assert.equal(result.rationale.tierSource, 'inherited')
  assert.equal(result.rationale.inheritedFrom, 'C')
  assert.equal(result.rationale.ready, true)
})

test('transitive relation requests walk the queued chain and nothing outside it', () => {
  const chain = [
    { id: 'A', priority: 3, createdAt: at(3) },
    { id: 'B', priority: 2, createdAt: at(2) },
    { id: 'C', priority: 1, createdAt: at(1) },
    { id: 'D', priority: 3, createdAt: at(0) },
  ]
  assert.deepEqual(selectCandidate({ candidates: chain }).need, { relations: ['C'] })
  chain[2].blockedBy = ['B']
  assert.deepEqual(selectCandidate({ candidates: chain }).need, { relations: ['B'] })
  chain[1].blockedBy = ['A']
  assert.deepEqual(selectCandidate({ candidates: chain }).need, { relations: ['A'] })
  chain[0].blockedBy = []
  const result = selectCandidate({ candidates: chain })
  assert.equal(result.action, 'select')
  assert.equal(result.id, 'A')
})

test('one-sided edge union: an edge given only as A.blocks still blocks B', () => {
  const result = select([
    c('B', { priority: 1, createdAt: at(0), blockedBy: [] }),
    c('A', { priority: 3, createdAt: at(9), blocks: ['B'] }),
  ])
  assert.equal(result.id, 'A')
  assert.equal(result.rationale.tierSource, 'inherited')
})

test('sameEpicParent: an unprioritized sibling chain selects the root and names the parent', () => {
  const truth = {
    relations: {
      'BOS-11': { blockedBy: [] },
      'BOS-12': { blockedBy: ['BOS-11'] },
      'BOS-13': { blockedBy: ['BOS-12'] },
    },
    buckets: { 'BOS-11': 'in-run', 'BOS-12': 'in-run', 'BOS-13': 'in-run' },
    blockerStates: {},
  }
  const siblings = ['BOS-13', 'BOS-12', 'BOS-11'].map((id, i) => ({
    id,
    priority: 0,
    createdAt: at(i),
    parentId: 'BOS-10',
  }))
  const { result } = drive(siblings, truth)
  assert.equal(result.id, 'BOS-11')
  assert.equal(result.rationale.sameEpicParent, 'BOS-10')
})

test('sameEpicParent is absent when candidates span parents', () => {
  const result = select([
    c('A', { priority: 1, parentId: 'P-1' }),
    c('B', { priority: 1, parentId: 'P-2' }),
  ])
  assert.equal(result.rationale.sameEpicParent, undefined)
})

test('unlock count beats createdAt in a batch created within 28 seconds', () => {
  const batch = Array.from({ length: 30 }, (_, i) =>
    c(`BOS-${100 + i}`, { priority: 2, createdAt: new Date(T0 + i * 950).toISOString() }),
  )
  const blocker = batch[29] // newest
  batch[0].blockedBy = [blocker.id]
  batch[1].blockedBy = [blocker.id]
  const result = select(batch)
  assert.equal(result.id, blocker.id)
  assert.equal(result.rationale.unlocks, 2)
  assert.equal(result.rationale.decidedBy, 'unlocks')
})

test('createdAt then id fallback', () => {
  const byAge = select([
    c('BOS-2', { priority: 1, createdAt: at(1) }),
    c('BOS-9', { priority: 1, createdAt: at(0) }),
  ])
  assert.equal(byAge.id, 'BOS-9')
  assert.equal(byAge.rationale.decidedBy, 'createdAt')
  const byId = select([c('BOS-9', { priority: 1 }), c('BOS-2', { priority: 1 })])
  assert.equal(byId.id, 'BOS-2')
  assert.equal(byId.rationale.decidedBy, 'id')
})

// ---------------------------------------------------------------------------------------------
// select — lazy bucket classification (none tier)

test('lazy bucket requests: one at a time, in rank order, stopping at the first durable', () => {
  const queue = [
    c('X', { createdAt: at(0) }),
    c('Y', { createdAt: at(1) }),
    c('Z', { createdAt: at(2) }),
  ]
  assert.deepEqual(select(queue).need, { bucket: 'X' })
  queue[0].bucket = 'in-run'
  assert.deepEqual(select(queue).need, { bucket: 'Y' })
  queue[1].bucket = 'durable'
  const result = select(queue)
  assert.equal(result.id, 'Y')
  assert.equal(result.rationale.bucket, 'durable')
  assert.equal(result.rationale.decidedBy, 'bucket')
  assert.equal(queue[2].bucket, undefined, 'Z is never classified')
})

test('all in-run falls back to the first-ranked candidate', () => {
  const result = select([
    c('X', { createdAt: at(0), bucket: 'in-run' }),
    c('Y', { createdAt: at(1), bucket: 'in-run' }),
  ])
  assert.equal(result.id, 'X')
  assert.equal(result.rationale.bucket, 'in-run')
})

test('worst-contained-failure reduces a per-defect bucket list', () => {
  assert.equal(reduceBucket(['in-run', 'durable']), 'durable')
  assert.equal(reduceBucket(['in-run', 'in-run']), 'in-run')
  assert.equal(reduceBucket('durable'), 'durable')
  const result = select([
    c('X', { createdAt: at(0), bucket: ['in-run', 'in-run'] }),
    c('Y', { createdAt: at(1), bucket: ['in-run', 'durable'] }),
  ])
  assert.equal(result.id, 'Y')
  assert.equal(result.rationale.bucket, 'durable')
})

test('bucket scope: a blocked durable candidate is never asked while a ready one exists', () => {
  const result = select(
    [c('BLOCKED', { createdAt: at(0), blockedBy: ['EXT-1'] }), c('READY', { createdAt: at(5) })],
    { 'EXT-1': 'started' },
  )
  assert.deepEqual(result.need, { bucket: 'READY' })
})

test('prioritized tiers never ask for a bucket', () => {
  const result = select([c('A', { priority: 3 }), c('B', { priority: 3, createdAt: at(1) })])
  assert.equal(result.action, 'select')
  assert.equal(result.rationale.bucket, undefined)
})

// ---------------------------------------------------------------------------------------------
// select — the need protocol and its termination

test('absent blockedBy means not fetched (need); [] means no blockers (ready)', () => {
  const absent = selectCandidate({ candidates: [{ id: 'A', priority: 1, createdAt: at(0) }] })
  assert.deepEqual(absent, { action: 'need', need: { relations: ['A'] } })
  const empty = selectCandidate({
    candidates: [{ id: 'A', priority: 1, createdAt: at(0), blockedBy: [] }],
  })
  assert.equal(empty.action, 'select')
  assert.equal(empty.rationale.ready, true)
  assert.equal(empty.rationale.decidedBy, 'only-candidate')
})

test('deduplicated blockerState requests across tier members', () => {
  const result = select([
    c('A', { priority: 2, blockedBy: ['EXT-2', 'EXT-1'] }),
    c('B', { priority: 2, blockedBy: ['EXT-1'] }),
    c('LOW', { priority: 4, blockedBy: ['EXT-9'] }),
  ])
  assert.deepEqual(result, { action: 'need', need: { blockerState: ['EXT-1', 'EXT-2'] } })
})

test('unreadable relations are edge-free and flagged', () => {
  const result = selectCandidate({
    candidates: [
      { id: 'A', priority: 1, createdAt: at(0), relations: 'unreadable' },
      { id: 'B', priority: 1, createdAt: at(1), blockedBy: [] },
    ],
  })
  assert.equal(result.action, 'select')
  assert.equal(result.id, 'A')
  assert.deepEqual(result.rationale.unreadable, { relations: ['A'], blockerStates: [] })
})

test('an unreadable blocker state counts as open and is flagged', () => {
  const result = select(
    [
      c('A', { priority: 1, createdAt: at(0), blockedBy: ['EXT-1'] }),
      c('B', { priority: 1, createdAt: at(9) }),
    ],
    { 'EXT-1': 'unreadable' },
  )
  assert.equal(result.id, 'B')
  assert.deepEqual(result.rationale.unreadable, { relations: [], blockerStates: ['EXT-1'] })
})

test('a two-node blocker cycle still selects without hanging', () => {
  const result = select([
    c('A', { priority: 2, createdAt: at(1), blockedBy: ['B'] }),
    c('B', { priority: 2, createdAt: at(0), blockedBy: ['A'] }),
  ])
  assert.equal(result.action, 'select')
  assert.equal(result.id, 'B')
  assert.equal(result.rationale.ready, false)
})

test('termination: a fixture answering every round never sees a repeated request', () => {
  const candidates = [
    { id: 'A', priority: 0, createdAt: at(4) },
    { id: 'B', priority: 0, createdAt: at(3) },
    { id: 'C', priority: 0, createdAt: at(2) },
    { id: 'D', priority: 0, createdAt: at(1) },
    { id: 'E', priority: 0, createdAt: at(0) },
  ]
  const truth = {
    relations: {
      A: { blockedBy: ['EXT-1'] },
      B: { blockedBy: ['A'] },
      C: { relations: 'unreadable' },
      D: { blockedBy: ['EXT-2', 'EXT-3'], blocks: ['E'] },
      E: { blockedBy: [] },
    },
    blockerStates: { 'EXT-1': 'completed', 'EXT-2': 'unreadable', 'EXT-3': 'canceled' },
    buckets: { A: 'in-run', B: 'durable', C: ['in-run', 'in-run'], D: 'durable', E: 'in-run' },
  }
  const { result, requests } = drive(candidates, truth)
  assert.equal(result.action, 'select')
  assert.equal(
    new Set(requests).size,
    requests.length,
    `repeated request in ${requests.join(', ')}`,
  )
  // A (unlocks B) and C are the ready group; both classify in-run, so first-ranked A wins.
  assert.equal(result.id, 'A')
  assert.equal(result.rationale.bucket, 'in-run')
})

test('empty candidates select nothing', () => {
  assert.deepEqual(selectCandidate({ candidates: [] }), { action: 'empty' })
})

test('invalid input is a usage error', () => {
  for (const bad of [
    null,
    [],
    { candidates: 'nope' },
    { candidates: [{ priority: 1 }] },
    { candidates: [{ id: 'A', priority: 7 }] },
    { candidates: [{ id: 'A', createdAt: 'yesterday-ish' }] },
    { candidates: [{ id: 'A', bucket: 'catastrophic' }] },
    { candidates: [{ id: 'A', bucket: [] }] },
    { candidates: [{ id: 'A' }, { id: 'A' }] },
    { candidates: [{ id: 'A', blockedBy: 'B' }] },
    { candidates: [], blockerStates: { X: 3 } },
  ]) {
    assert.throws(() => selectCandidate(bad), UsageError, JSON.stringify(bad))
  }
})

test('agreement with readyTickets on an edge-free single-tier fixture', () => {
  const nodes = [
    { id: 'BOS-5', priority: 2, createdAt: at(3) },
    { id: 'BOS-3', priority: 2, createdAt: at(1) },
    { id: 'BOS-4', priority: 2, createdAt: at(1) },
    { id: 'BOS-1', priority: 2, createdAt: at(7) },
    { id: 'BOS-2', priority: 2, createdAt: at(0) },
  ]
  const expected = readyTickets(buildGraph(nodes), {
    merged: new Set(),
    failed: new Set(),
    inFlight: new Set(),
    externallyCleared: new Set(),
  }).map((n) => n.id)
  const remaining = nodes.map((n) => ({ ...n, blockedBy: [] }))
  const picked = []
  while (remaining.length > 0) {
    const { id } = selectCandidate({ candidates: remaining })
    picked.push(id)
    remaining.splice(
      remaining.findIndex((n) => n.id === id),
      1,
    )
  }
  assert.deepEqual(picked, expected)
})

test('CLI select: reads stdin, prints one JSON line, exits 0', () => {
  const res = runCli(['select', '-'], {
    input: JSON.stringify({ candidates: [c('BOS-1', { priority: 1 })] }),
  })
  assert.equal(res.status, 0, res.stderr)
  const lines = res.stdout.trim().split('\n')
  assert.equal(lines.length, 1)
  assert.equal(JSON.parse(lines[0]).id, 'BOS-1')
})

test('CLI select: invalid input exits 2 with a stderr reason', () => {
  for (const input of ['not json', JSON.stringify({ candidates: [{ id: 'A', priority: 9 }] })]) {
    const res = runCli(['select', '-'], { input })
    assert.equal(res.status, 2)
    assert.equal(res.stdout, '')
    assert.match(res.stderr, /plan-sweep-select\.mjs: /)
  }
  assert.equal(runCli(['select']).status, 2)
  assert.equal(runCli(['bogus']).status, 2)
})

// ---------------------------------------------------------------------------------------------
// referents

const GIT_ENV = {
  ...process.env,
  GIT_AUTHOR_NAME: 'Referent Test',
  GIT_AUTHOR_EMAIL: 'referent@example.invalid',
  GIT_COMMITTER_NAME: 'Referent Test',
  GIT_COMMITTER_EMAIL: 'referent@example.invalid',
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_SYSTEM: '/dev/null',
}

function git(cwd, args) {
  const res = spawnSync('git', args, { cwd, encoding: 'utf8', env: GIT_ENV })
  assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`)
  return res.stdout
}

function write(dir, file, body) {
  fs.mkdirSync(path.dirname(path.join(dir, file)), { recursive: true })
  fs.writeFileSync(path.join(dir, file), body)
}

function commit(dir, message) {
  git(dir, ['add', '-A'])
  git(dir, ['commit', '-q', '-m', message])
}

// History: live code, a symbol and a path that were committed then removed, a path moved with
// `git mv`, and a removed symbol still quoted in a committed plan doc.
function makeRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'plan-sweep-select-'))
  git(dir, ['init', '-q', '-b', 'main'])
  write(dir, 'src/live.mjs', 'export function liveSymbol() {}\n')
  write(dir, 'src/doomed.mjs', 'export function removedSymbol() {}\n')
  write(dir, 'src/gone.mjs', 'export const unrelated = 1\n')
  write(dir, 'src/moved.mjs', 'export const moved = 1\n')
  commit(dir, 'initial')
  write(dir, 'src/doomed.mjs', 'export function survivor() {}\n')
  fs.rmSync(path.join(dir, 'src/gone.mjs'))
  commit(dir, 'remove the defect')
  git(dir, ['mv', 'src/moved.mjs', 'src/renamed.mjs'])
  commit(dir, 'rename')
  write(dir, 'docs/plans/old-plan.md', 'This plan removed `removedSymbol` from src/doomed.mjs.\n')
  write(dir, 'NOTES.md', 'neverInCode appears only here\n')
  commit(dir, 'plan docs')
  return dir
}

const REPO = makeRepo()
const judge = (referents, ref = 'main') =>
  referentVerdict({ ref, referents, git: (args) => runGit(REPO, args) })

test('referents live on a present code symbol and a present code path', () => {
  assert.equal(judge(['liveSymbol']).verdict, 'live')
  assert.equal(judge(['src/live.mjs']).verdict, 'live')
})

test('referents stale on a symbol committed then removed and a path added then deleted', () => {
  const symbol = judge(['removedSymbol'])
  assert.equal(symbol.verdict, 'stale')
  assert.deepEqual(symbol.absent, ['removedSymbol'])
  assert.equal(judge(['src/gone.mjs']).verdict, 'stale')
  assert.equal(judge(['removedSymbol', 'src/gone.mjs']).verdict, 'stale')
})

test('referents stay stale when the removed symbol survives only in a committed markdown plan', () => {
  const tracked = git(REPO, ['grep', '-l', 'removedSymbol', 'main'])
  assert.match(tracked, /docs\/plans\/old-plan\.md/, 'fixture must quote the symbol in a plan doc')
  assert.equal(judge(['removedSymbol']).verdict, 'stale')
})

test('referents live on a tracked markdown path that no code file mentions', () => {
  const mention = runGit(REPO, ['grep', '-q', '-F', '-e', 'docs/plans/old-plan.md', 'main'])
  assert.equal(mention.status, 1, 'fixture must leave the plan path unmentioned anywhere')
  const doc = judge(['docs/plans/old-plan.md'])
  assert.deepEqual([doc.verdict, doc.present], ['live', ['docs/plans/old-plan.md']])
  assert.equal(judge(['docs/plans']).verdict, 'live')
})

test('referents unknown/no-history for a symbol never committed to code', () => {
  const typo = judge(['removedSymbl'])
  assert.deepEqual([typo.verdict, typo.reason], ['unknown', 'no-history'])
  const docsOnly = judge(['neverInCode'])
  assert.deepEqual([docsOnly.verdict, docsOnly.reason], ['unknown', 'no-history'])
})

test('referents unknown/renamed for a path moved with git mv', () => {
  const result = judge(['src/moved.mjs'])
  assert.deepEqual([result.verdict, result.reason], ['unknown', 'renamed'])
  assert.equal(result.to, 'src/renamed.mjs')
})

test('referents live on a partial miss', () => {
  const result = judge(['removedSymbol', 'liveSymbol'])
  assert.equal(result.verdict, 'live')
  assert.deepEqual(result.present, ['liveSymbol'])
})

test('referents judge the ref, not an uncommitted working-tree edit', () => {
  write(
    REPO,
    'src/live.mjs',
    'export function liveSymbol() {}\nexport function removedSymbol() {}\n',
  )
  try {
    assert.equal(judge(['removedSymbol']).verdict, 'stale')
  } finally {
    git(REPO, ['checkout', '--', 'src/live.mjs'])
  }
})

test('referents unknown with distinct reasons: no-referents, ref-unresolvable, git-failed', () => {
  assert.equal(judge([]).reason, 'no-referents')
  assert.equal(judge(['liveSymbol'], 'does-not-exist').reason, 'ref-unresolvable')
  const failing = () => ({ status: 128, stdout: '', stderr: 'fatal: boom' })
  const allFail = referentVerdict({ ref: 'main', referents: ['x'], git: failing })
  assert.deepEqual([allFail.verdict, allFail.reason], ['unknown', 'git-failed'])
  const grepFails = referentVerdict({
    ref: 'main',
    referents: ['x'],
    git: (args) => (args[0] === 'grep' ? failing() : runGit(REPO, args)),
  })
  assert.deepEqual([grepFails.verdict, grepFails.reason], ['unknown', 'git-failed'])
})

test('CLI referents exits 0 with one JSON line for every verdict', () => {
  const cases = [
    [['--ref', 'main', '--', 'liveSymbol'], 'live'],
    [['--ref', 'main', '--', 'removedSymbol'], 'stale'],
    [['--ref', 'main', '--', 'removedSymbl'], 'unknown'],
    [['--ref', 'main', '--'], 'unknown'],
    [['--ref', 'does-not-exist', '--', 'liveSymbol'], 'unknown'],
  ]
  for (const [args, verdict] of cases) {
    const res = runCli(['referents', ...args], { cwd: REPO })
    assert.equal(res.status, 0, res.stderr)
    const lines = res.stdout.trim().split('\n')
    assert.equal(lines.length, 1)
    assert.equal(JSON.parse(lines[0]).verdict, verdict, args.join(' '))
  }
})

test('CLI referents without a --ref value exits 2', () => {
  for (const args of [['--ref'], ['--ref', '--', 'x'], ['--', 'x']]) {
    const res = runCli(['referents', ...args], { cwd: REPO })
    assert.equal(res.status, 2, args.join(' '))
    assert.equal(res.stdout, '')
  }
})
