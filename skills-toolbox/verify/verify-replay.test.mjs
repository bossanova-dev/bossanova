import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { classifyReadCall, judge, UsageError } from './verify-gate.mjs'
import {
  BASE,
  REPO,
  extensionRecord,
  forge,
  headOf,
  judgeRecord,
  parkStatus,
} from './verify-replay-fixture.mjs'
import { parkNote } from './verify-park-note.mjs'
import { levelLabel } from './verify-level.mjs'
import {
  REFUSED,
  guardedReader,
  main,
  replay,
  replayCallKind,
  replayRow,
} from './verify-replay.mjs'

const HIGH = { auto: [{ level: 'high' }] }
const TWO_RULES = { auto: [{ level: 'high' }, { area: 'ui', proof: 'tests' }] }
const SIZE = { ok: true, thresholds: { p90: 100, p99: 200 } }
const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })

const rowsOf = (report, candidate) => report.rows.filter((row) => row.candidate === candidate)
const outcomes = (report, candidate) => rowsOf(report, candidate).map((row) => row.outcome)

/** Candidate files under a temp dir removed after the test. */
function candidateFiles(t, candidates) {
  const dir = mkdtempSync(join(tmpdir(), 'verify-replay-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  return Object.entries(candidates).flatMap(([name, merge]) => {
    const file = join(dir, `${name}.json`)
    writeFileSync(file, JSON.stringify(merge))
    return ['--candidate', `${name}=${file}`]
  })
}

const WORLD = () => ({
  prs: [
    { number: 3, paths: ['src/a.go'], comments: [judgeRecord(3)] },
    { number: 4, paths: ['main.tf'], comments: [judgeRecord(4)] },
    {
      number: 5,
      paths: ['src/b.go'],
      comments: [judgeRecord(5), extensionRecord(5, 'acme-check:pass')],
      statuses: [parkStatus('needs human: human-merge')],
    },
  ],
  baseConfig: { verifyDefaults: { alwaysReview: { Billing: ['billing/**'] } } },
  extensions: [{ name: 'acme-check', role: 'verify' }],
})

test('replaying the same world and candidates twice is byte-identical, with no run-specific field', async (t) => {
  const args = candidateFiles(t, { high: HIGH, both: TWO_RULES })
  const texts = []
  for (const format of ['json', 'markdown'])
    for (let i = 0; i < 2; i++) {
      const { deps } = forge(WORLD())
      texts.push((await main(['--repo', REPO, ...args, '--format', format], deps)).text)
    }
  assert.equal(texts[0], texts[1])
  assert.equal(texts[2], texts[3])
  assert.ok(!texts[0].includes(tmpdir()) && !texts[2].includes(tmpdir()))
  assert.ok(!/20\d\d-\d\d-\d\dT/.test(texts[0]))
})

test('the default candidate merges nothing; a level-high candidate merges the high head', async () => {
  const prs = [{ number: 3, paths: ['src/a.go'], comments: [judgeRecord(3)] }]
  for (const baseConfig of [null, { verifyDefaults: { merge: HIGH } }]) {
    const report = await replay(
      { repo: REPO, candidates: [{ name: 'high', merge: HIGH }] },
      forge({ prs, baseConfig }).deps,
    )
    const [row] = rowsOf(report, 'default')
    assert.deepEqual([row.outcome, row.codes], ['park', ['human-merge']])
    assert.deepEqual(row.decidedBy, [{ code: 'human-merge', by: 'no-level-rule' }])
    assert.deepEqual(outcomes(report, 'high'), ['merge'])
    assert.deepEqual(rowsOf(report, 'high')[0].decidedBy, [
      { by: 'level-rule', rule: { index: 0, rule: { level: 'high' } } },
    ])
    assert.deepEqual(report.summary.perCandidate.default.merge, 0)
  }
})

test('a full replay reaches the forge only through cached reads and writes nothing', async () => {
  const world = forge(WORLD())
  const report = await replay(
    {
      repo: REPO,
      candidates: [
        { name: 'high', merge: HIGH },
        { name: 'both', merge: TWO_RULES },
      ],
    },
    world.deps,
  )
  assert.equal(report.rows.length, 9)
  assert.ok(world.calls.length > 0)
  for (const [command, args] of world.calls) {
    assert.equal(replayCallKind(command, args), 'read', `${command} ${args.join(' ')}`)
    if (command === 'gh') assert.equal(classifyReadCall(command, args), 'cached')
  }
  assert.equal(report.summary.refusedWrites, 0)
  assert.deepEqual(world.writes, [])
})

test('the guard refuses every writing command kind before it reaches the reader', () => {
  for (const [command, args] of [
    ['gh', ['pr', 'merge', '7', '--squash']],
    ['gh', ['pr', 'comment', '7', '--body', 'hello']],
    ['gh', ['api', '-X', 'POST', `repos/${REPO}/statuses/${BASE}`, '-f', 'state=success']],
    ['gh', ['api', 'graphql', '-f', 'query=mutation { addComment { clientMutationId } }']],
    ['gh', ['pr', 'edit', '7', '--add-label', 'needs-human']],
    ['boss', ['merge', 'sess-1', '--head', BASE]],
    ['boss', ['callback', 'add', '--pr', '7']],
    ['git', ['-C', '/repo', 'push', 'origin', 'HEAD']],
  ]) {
    const reached = []
    const refused = []
    const guard = guardedReader((...call) => reached.push(call) && ok({}), refused)
    assert.deepEqual(guard(command, args), { ok: false, error: REFUSED, exit: null, output: '' })
    assert.deepEqual(reached, [], `${command} ${args.join(' ')}`)
    assert.equal(refused.length, 1)
  }
  const reached = []
  const guard = guardedReader((...call) => reached.push(call) && ok({}))
  for (const [command, args] of [
    ['gh', ['pr', 'view', '7', '--json', 'state']],
    ['gh', ['api', `repos/${REPO}/commits/main`]],
    ['boss', ['ls', '--json']],
    ['git', ['merge-base', BASE, headOf(3)]],
    ['tar', ['-xf', 'tree.tar']],
  ])
    guard(command, args)
  assert.equal(reached.length, 5)
})

test('a judge path that attempts a write reports it on the row and exits 1', async () => {
  const world = forge({ prs: [{ number: 3, paths: ['src/a.go'], comments: [judgeRecord(3)] }] })
  world.deps.readSizeBaseline = ({ read }) => {
    read('gh', ['pr', 'comment', '3', '--body', 'replayed'])
    return SIZE
  }
  const { report, exitCode } = await main(['--repo', REPO, '--pr', '3'], world.deps)
  assert.equal(exitCode, 1)
  assert.equal(report.summary.refusedWrites, 1)
  assert.deepEqual(rowsOf(report, 'default')[0].refused, ['gh pr comment 3'])
  assert.deepEqual(world.writes, [])
})

test('a judge-raised ask parks human-review:judge with no protected-path code', () => {
  const confidence = {
    level: 'moderate',
    asks: [{ area: 'judge:access', label: 'Access', source: 'judge' }],
    reasons: [],
    loweredBy: ['area-asks'],
    health: [],
    merge: {
      decisions: [
        {
          area: 'judge:access',
          label: 'Access',
          source: 'judge',
          code: 'human-review:judge',
          floor: false,
          covered: false,
          rule: null,
          prose: false,
        },
      ],
      residual: [],
      levelRule: null,
      holds: [],
    },
  }
  const judged = {
    verdict: 'human',
    reason: 'human-review:judge',
    reasons: ['human-review:judge'],
    confidence,
  }
  const row = replayRow({ candidate: 'default', pr: 3, headSha: headOf(3), judged })
  assert.deepEqual(row.codes, ['human-review:judge'])
  assert.equal(row.unattributedAlwaysHumanPath, undefined)
  assert.deepEqual(row.decidedBy, [
    { code: 'human-review:judge', by: 'uncovered', asks: ['Access'] },
  ])
})

test('an alwaysReview path is attributed to its config entry', async () => {
  const report = await replay(
    { repo: REPO, candidates: [{ name: 'high', merge: HIGH }] },
    forge({
      prs: [{ number: 6, paths: ['billing/a.go'], comments: [judgeRecord(6)] }],
      baseConfig: { verifyDefaults: { alwaysReview: { Billing: ['billing/**'] } } },
    }).deps,
  )
  for (const row of report.rows) {
    assert.ok(row.codes.includes('always-human-path'), row.candidate)
    const protectedAsk = row.asks.find((ask) => ask.code === 'always-human-path')
    assert.deepEqual([protectedAsk.from, protectedAsk.floor], ['config', true])
    assert.equal(row.unattributedAlwaysHumanPath, undefined)
  }
  assert.equal(report.summary.unattributedAlwaysHumanPath, 0)
})

test('a protected-path park with no config or extension ask is flagged and counted', () => {
  const judged = {
    verdict: 'human',
    reason: 'always-human-path',
    reasons: ['always-human-path'],
    confidence: {
      level: 'moderate',
      asks: [],
      reasons: [],
      loweredBy: [],
      health: [],
      merge: { decisions: [], residual: [], levelRule: null, holds: [] },
    },
  }
  const row = replayRow({ candidate: 'default', pr: 3, headSha: headOf(3), judged })
  assert.equal(row.unattributedAlwaysHumanPath, true)
})

test('each ask names the rule that covered it, or null beside its own park code', async () => {
  const report = await replay(
    { repo: REPO, candidates: [{ name: 'both', merge: TWO_RULES }] },
    forge({
      prs: [
        {
          number: 8,
          paths: ['web/App.tsx', 'web/App.test.tsx', 'api/x.proto'],
          contents: { 'web/App.test.tsx': "import { App } from './App'\n" },
          comments: [judgeRecord(8)],
        },
      ],
      baseConfig: { verifyDefaults: { humanReview: { ui: ['web/**'] } } },
    }).deps,
  )
  const [row] = rowsOf(report, 'both')
  assert.equal(row.outcome, 'park')
  const ui = row.asks.find((ask) => ask.code === 'human-review:ui')
  const api = row.asks.find((ask) => ask.code === 'human-review:api-contract')
  assert.deepEqual(ui.rule, { index: 1, rule: { area: 'ui', proof: 'tests' } })
  assert.equal(api.rule, null)
  assert.deepEqual(row.codes, ['human-review:api-contract'])
  const [defaultRow] = rowsOf(report, 'default')
  assert.ok(defaultRow.asks.every((ask) => ask.rule === null))
})

test('a recorded extension run is reused, a missing one is unknown, a standing park is fresh', async () => {
  const world = WORLD()
  const report = await replay(
    { repo: REPO, candidates: [{ name: 'high', merge: HIGH }] },
    forge({
      ...world,
      prs: [
        ...world.prs,
        { number: 9, paths: ['src/c.go'], comments: [judgeRecord(9)] }, // no extension record
      ],
    }).deps,
  )
  const byPr = Object.fromEntries(rowsOf(report, 'high').map((row) => [row.pr, row.outcome]))
  assert.equal(byPr[3], 'unknown: extensions-unrecorded')
  assert.equal(byPr[9], 'unknown: extensions-unrecorded')
  // PR 5 stands parked human-merge with a recorded pass: the high candidate merges it.
  assert.equal(byPr[5], 'merge')
  const standing = forge(world)
  const live = await judge({ pr: 5, repo: REPO }, standing.deps)
  assert.equal(live.held, true)
})

test('a head with no judge record reports unknown: judge-missing', async () => {
  const report = await replay(
    { repo: REPO },
    forge({ prs: [{ number: 3, paths: ['src/a.go'], comments: [] }] }).deps,
  )
  assert.deepEqual(outcomes(report, 'default'), ['unknown: judge-missing'])
})

test('a named PR that is not open is not replayable', async () => {
  const report = await replay(
    { repo: REPO, prs: [3] },
    forge({ prs: [{ number: 3, paths: ['src/a.go'], state: 'MERGED' }] }).deps,
  )
  assert.deepEqual(outcomes(report, 'default'), ['not-replayable: pr-not-open'])
})

test('an invalid candidate is a usage error', async (t) => {
  const { deps, calls } = forge(WORLD())
  for (const merge of [
    { auto: [{ level: 'high', bogus: 'x' }] },
    { policy: '../merge-policy.md' },
    { auto: [{ level: 'highest' }] },
  ]) {
    const args = candidateFiles(t, { bad: merge })
    await assert.rejects(main(['--repo', REPO, ...args], deps), UsageError)
  }
  await assert.rejects(main(['--repo', REPO, '--format', 'yaml'], deps), UsageError)
  await assert.rejects(main(['--repo', REPO, '--bogus'], deps), UsageError)
  assert.deepEqual(calls, [])
})

test('a park row carries the note post would leave for the same codes and facts', async () => {
  const report = await replay(
    { repo: REPO },
    forge({ prs: [{ number: 4, paths: ['main.tf'], comments: [judgeRecord(4)] }] }).deps,
  )
  const [row] = rowsOf(report, 'default')
  assert.equal(row.outcome, 'park')
  const live = await judge(
    { pr: 4, repo: REPO },
    forge({ prs: [{ number: 4, paths: ['main.tf'], comments: [judgeRecord(4)] }] }).deps,
  )
  const expected = parkNote({
    codes: row.codes,
    headSha: headOf(4),
    facts: {
      title: 'feat: widget 4',
      body: 'Adds widget 4.',
      label: levelLabel(live.confidence),
      confidence: live.confidence,
    },
  }).body
  assert.equal(row.note, expected)
})
