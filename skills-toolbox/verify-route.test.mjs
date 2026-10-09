import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import {
  CALL_TIMEOUT_MS,
  DEFAULT_BATCH,
  DEFAULT_BUDGET_MS,
  START_FLOOR_MS,
  UsageError,
  liveSessionsFor,
  lsFailureIsUnreachable,
  newSessionArgv,
  parseSweepArgs,
  spawnDetached,
  sweep,
  timedRunner,
  verifyMessage,
} from './verify-route.mjs'

const BOSS = '/fake/bin/boss'
const NODE = 'node'
const T0 = 1_000_000
const sha = (n) => n.toString(16).padStart(40, '0')
const url = (n) => `https://github.com/o/r/pull/${n}`
const candidate = (n, extra = {}) => ({
  ticket: `BOS-${n}`,
  pr: n,
  url: url(n),
  headSha: sha(n),
  repo: 'o/r',
  claim: 'none',
  ...extra,
})

/**
 * A fake world behind the one `run` seam: `node verify-gate.mjs <verb>` and `boss <cmd>`. Every call
 * is recorded; `mutating()` is every call that would write (a non-dry post/merge, chat new/send),
 * and `spawns` every detached spawn.
 */
function world(opts = {}) {
  const w = {
    calls: [],
    spawns: [],
    clock: opts.clock ?? T0,
    tick: opts.tick ?? 0,
  }
  const verdicts = opts.verdicts ?? {}
  w.run = (cmd, args, options = {}) => {
    w.clock += w.tick
    w.calls.push({ cmd, args: [...args], timeoutMs: options.timeoutMs, env: options.env })
    const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
    const fail = (error = 'exit-1', extra = {}) => ({
      ok: false,
      error,
      exit: 1,
      output: '',
      ...extra,
    })
    if (cmd === NODE) {
      const [, verb, ...rest] = args
      const flag = (name) => rest[rest.indexOf(`--${name}`) + 1]
      const pr = Number(flag('pr'))
      if (verb === 'candidates')
        return opts.candidatesFail
          ? fail()
          : ok({ candidates: opts.candidates ?? [candidate(1)], skipped: [] })
      if (verb === 'judge') {
        const v = verdicts[pr] ?? { verdict: 'pass', reason: 'verified' }
        return ok({ headSha: sha(pr), ...v })
      }
      if (verb === 'post') {
        const verdict = flag('verdict')
        const dry = rest.includes('--dry-run')
        if (verdict === 'claim') {
          const lost = opts.claimLost?.includes(pr)
          return ok({ ...(dry ? { dryRun: true } : {}), won: !lost, token: 'tok-1' })
        }
        if (verdict === 'human' && opts.trackerUnavailable)
          return ok({ verdict, posted: false, trackerWrites: 'unavailable' })
        return ok({ ...(dry ? { dryRun: true } : {}), verdict, posted: true })
      }
      if (verb === 'merge') return ok({ merged: true, mergeSha: sha(900 + pr) })
      return fail('unexpected-verb')
    }
    if (cmd === BOSS) {
      if (args[0] === 'ls') {
        if (opts.lsResult) return opts.lsResult
        return ok({ sessions: opts.sessions ?? [] })
      }
      if (args[0] === 'chats') return ok({ chats: opts.chats?.[args[2]] ?? [] })
      if (args[0] === 'chat' && args[1] === 'new')
        return ok({ chat: { agent_session_id: 'chat-new', session_id: args[2], title: 'verify' } })
      if (args[0] === 'chat' && args[1] === 'send')
        return opts.sendTimesOut ? fail('ETIMEDOUT', { exit: null }) : ok({})
      return fail('unexpected-boss')
    }
    return fail('unexpected-command')
  }
  w.verbs = () => w.calls.filter((c) => c.cmd === NODE).map((c) => [c.args[1], ...c.args.slice(2)])
  w.posts = () => w.verbs().filter((v) => v[0] === 'post')
  w.mutating = () =>
    w.calls.filter(
      (c) =>
        (c.cmd === NODE &&
          ['post', 'merge'].includes(c.args[1]) &&
          !c.args.includes('--dry-run')) ||
        (c.cmd === BOSS && c.args[0] === 'chat'),
    )
  w.spawn = (cmd, args, options) => {
    w.spawns.push({ cmd, args: [...args], options })
    return { pid: 4242, unref() {}, on() {} }
  }
  w.deps = (extra = {}) => ({
    run: w.run,
    nodeBin: NODE,
    now: () => w.clock,
    resolveBoss: () => opts.boss ?? { ok: true, path: BOSS, reason: '' },
    spawn: w.spawn,
    env: { PATH: '/usr/bin', LINEAR_API_KEY: 'k', REPO_DIR: '/repo' },
    cwd: '/repo',
    logDir: mkdtempSync(join(tmpdir(), 'verify-route-')),
    ...extra,
  })
  return w
}

const lines = (result) => result.records.map((r) => r.line)

// ---------------------------------------------------------------------------
// Arguments and pure helpers.

test('parseSweepArgs keeps selection flags verbatim and refuses anything else', () => {
  assert.deepEqual(parseSweepArgs([]), {
    batch: DEFAULT_BATCH,
    budgetMs: DEFAULT_BUDGET_MS,
    dryRun: false,
    selectionArgs: [],
  })
  assert.deepEqual(
    parseSweepArgs([
      '--label',
      'a,b',
      '--batch',
      '2',
      '--dry-run',
      '--exclude-label=x',
      '--budget-ms=3000',
    ]),
    {
      batch: 2,
      budgetMs: 3000,
      dryRun: true,
      selectionArgs: ['--label', 'a,b', '--exclude-label=x'],
    },
  )
  for (const argv of [['42'], ['--bogus'], ['--batch', '0'], ['--batch', 'x'], ['--label']])
    assert.throws(() => parseSweepArgs(argv), UsageError, JSON.stringify(argv))
})

test('the message, the orphan argv and the live-session pick', () => {
  assert.equal(verifyMessage(7, 'tok'), '/boss-verify 7 --claim tok')
  assert.equal(verifyMessage(7, 'tok', 'policy'), '/boss-verify 7 --claim tok --waive policy')
  assert.equal(
    newSessionArgv({ pr: 7, repoDir: '/r', token: 't', waive: 'policy' }).includes(
      '/boss-verify 7 --claim t --waive policy',
    ),
    true,
  )
  assert.deepEqual(
    newSessionArgv({
      pr: 7,
      repoDir: '/repo',
      ticket: 'BOS-7',
      token: 'tok',
      trackerSource: 'linear',
    }),
    [
      'new',
      '--repo',
      '/repo',
      '--pr',
      '7',
      '--prompt',
      '/boss-verify 7 --claim tok',
      '--title',
      'verify #7',
      '--tmux-unattended',
      '--tracker-id',
      'BOS-7',
      '--tracker-source',
      'linear',
    ],
  )
  assert.equal(newSessionArgv({ pr: 7, repoDir: '/r', token: 't' }).includes('--tracker-id'), false)
  const rows = [
    { id: 'old', pr_url: url(7), updated_at: '2026-01-01T00:00:00Z' },
    {
      id: 'archived',
      pr_url: url(7),
      updated_at: '2026-03-01T00:00:00Z',
      archived_at: '2026-03-02T00:00:00Z',
    },
    { id: 'new', pr_url: url(7), updated_at: '2026-02-01T00:00:00Z' },
    { id: 'other', pr_url: url(8), updated_at: '2026-04-01T00:00:00Z' },
  ]
  assert.deepEqual(
    liveSessionsFor(rows, url(7)).map((r) => r.id),
    ['new', 'old'],
  )
  assert.deepEqual(liveSessionsFor(rows, ''), [])
})

test('lsFailureIsUnreachable: launch, socket and daemon errors, not a well-formed refusal', () => {
  assert.equal(lsFailureIsUnreachable({ ok: true }), false)
  assert.equal(lsFailureIsUnreachable({ ok: false, error: 'ENOENT', exit: null }), true)
  assert.equal(lsFailureIsUnreachable({ ok: false, error: 'exit-1', exit: 1 }), true)
  assert.equal(
    lsFailureIsUnreachable({ ok: false, error: 'UNAVAILABLE', exit: 1, payload: {} }),
    true,
  )
  assert.equal(
    lsFailureIsUnreachable({ ok: false, error: 'PERMISSION_DENIED', exit: 1, payload: {} }),
    false,
  )
})

test('spawnDetached: detached, own stdio to a log file, unref’d — never the caller’s pipes', () => {
  const dir = mkdtempSync(join(tmpdir(), 'verify-route-spawn-'))
  const seen = []
  let unrefd = false
  const r = spawnDetached(
    BOSS,
    ['new'],
    { logDir: dir, now: () => 5, env: { A: '1' }, cwd: '/repo' },
    {
      spawn: (cmd, args, options) => {
        seen.push({ cmd, args, options })
        return { pid: 9, unref: () => (unrefd = true), on() {} }
      },
    },
  )
  assert.equal(r.ok, true)
  assert.match(r.log, /boss-verify-dispatch-5-\d+\.log$/)
  const { options } = seen[0]
  assert.equal(options.detached, true)
  assert.equal(options.stdio[0], 'ignore')
  for (const fd of options.stdio.slice(1)) {
    assert.equal(typeof fd, 'number')
    assert.ok(![0, 1, 2].includes(fd), `stdio fd ${fd} is the caller's`)
  }
  assert.notEqual(options.stdio, 'inherit')
  assert.equal(unrefd, true)
  assert.equal(readFileSync(r.log, 'utf8'), '')
})

test('timedRunner: a failed read is unknown, never an empty success', () => {
  const run = timedRunner()
  const good = run(process.execPath, ['-e', 'console.log(JSON.stringify({a:1}))'], { json: true })
  assert.deepEqual([good.ok, good.payload], [true, { a: 1 }])
  const bad = run(process.execPath, ['-e', 'process.exit(3)'], { json: true })
  assert.deepEqual([bad.ok, bad.error], [false, 'exit-3'])
  const pretty = run(
    process.execPath,
    ['-e', 'console.log(JSON.stringify({ sessions: [{ id: 1 }] }, null, 2))'],
    { json: true },
  )
  assert.deepEqual([pretty.ok, pretty.payload], [true, { sessions: [{ id: 1 }] }])
  const garbled = run(process.execPath, ['-e', 'console.log("nope")'], { json: true })
  assert.deepEqual([garbled.ok, garbled.error], [false, 'unreadable-json'])
  const missing = run('/definitely/not/here', [], {})
  assert.deepEqual([missing.ok, missing.exit], [false, null])
})

// ---------------------------------------------------------------------------
// The sweep.

test('gate-only mode: a pass posts verified then merges the judged head', () => {
  const w = world()
  const r = sweep({}, w.deps())
  assert.deepEqual(
    w.verbs().map((v) => v[0]),
    ['candidates', 'judge', 'post', 'merge'],
  )
  assert.deepEqual(w.posts()[0].slice(0, 7), [
    'post',
    '--pr',
    '1',
    '--head',
    sha(1),
    '--verdict',
    'pass',
  ])
  assert.deepEqual(lines(r), [`#1 merged ${sha(901)}`])
  assert.equal(r.summary.merged, 1)
  // verify-gate's own `boss` calls find the resolved binary first on PATH.
  const merge = w.calls.find((c) => c.args[1] === 'merge')
  assert.equal(merge.env.PATH, '/fake/bin:/usr/bin')
})

test('a human verdict posts needs-human and parks; tracker writes unavailable is a skip', () => {
  const w = world({ verdicts: { 1: { verdict: 'human', reason: 'no-receipt' } } })
  const r = sweep({}, w.deps())
  assert.deepEqual(w.posts()[0].slice(5, 9), ['--verdict', 'human', '--reason', 'no-receipt'])
  assert.deepEqual(lines(r), ['#1 parked: needs human (no-receipt)'])
  assert.equal(
    w.calls.some((c) => c.cmd === BOSS),
    false,
    'a human park needs no boss',
  )

  const u = world({
    verdicts: { 1: { verdict: 'human', reason: 'ledger-open' } },
    trackerUnavailable: true,
  })
  assert.deepEqual(lines(sweep({}, u.deps())), ['#1 skipped: tracker writes unavailable'])
})

test('a wait verdict writes nothing and does not count against the batch', () => {
  const w = world({
    candidates: [candidate(1), candidate(2)],
    verdicts: { 1: { verdict: 'wait', reason: 'base-ci-red' } },
  })
  const r = sweep({ batch: 1 }, w.deps())
  assert.deepEqual(lines(r), ['#1 skipped: wait base-ci-red', `#2 merged ${sha(902)}`])
})

const EXT = { verdict: 'extensions-required', reason: 'extensions-installed' }

test('with extensions: the claim is posted before routing and its token is in the message', () => {
  const w = world({
    verdicts: { 1: EXT },
    sessions: [{ id: 's1', pr_url: url(1), updated_at: '2026-10-01T00:00:00Z' }],
    chats: {
      s1: [
        { agent_session_id: 'chat-v', title: 'Verify' },
        { agent_session_id: 'chat-x', title: 'main' },
      ],
    },
  })
  const r = sweep({}, w.deps())
  const order = w.calls.map((c) => (c.cmd === NODE ? c.args[1] : c.args.slice(0, 2).join(' ')))
  assert.ok(order.indexOf('post') < order.indexOf('chat send'), order.join(','))
  assert.deepEqual(w.posts()[0].slice(5, 7), ['--verdict', 'claim'])
  // An existing `verify` chat (matched case-insensitively) gets the message.
  const send = w.calls.find((c) => c.args[0] === 'chat' && c.args[1] === 'send')
  assert.deepEqual(send.args, ['chat', 'send', 'chat-v', '/boss-verify 1 --claim tok-1'])
  assert.equal(
    w.calls.some((c) => c.args[1] === 'new'),
    false,
  )
  assert.deepEqual(lines(r), ['#1 dispatched: existing-verify-chat'])
  assert.equal(w.spawns.length, 0)
})

test('no verify chat: chat new --title verify, then chat send to the new chat', () => {
  const w = world({
    verdicts: { 1: EXT },
    sessions: [
      { id: 's-old', pr_url: url(1), updated_at: '2026-09-01T00:00:00Z' },
      { id: 's-new', pr_url: url(1), updated_at: '2026-10-01T00:00:00Z' },
    ],
    chats: { 's-new': [{ agent_session_id: 'c1', title: 'main' }] },
  })
  const r = sweep({}, w.deps())
  const bossCalls = w.calls.filter((c) => c.cmd === BOSS).map((c) => c.args)
  assert.deepEqual(bossCalls.slice(1), [
    ['chats', '--json', 's-new'],
    ['chat', 'new', 's-new', '--title', 'verify', '--json'],
    ['chat', 'send', 'chat-new', '/boss-verify 1 --claim tok-1'],
  ])
  assert.deepEqual(lines(r), ['#1 dispatched: new-verify-chat (newest of 2 live sessions)'])
})

test('the verify chat is the one boss-build arms: newest live wins, a STOPPED one is passed over', () => {
  const w = world({
    verdicts: { 1: EXT },
    sessions: [{ id: 's1', pr_url: url(1), updated_at: '2026-10-01T00:00:00Z' }],
    chats: {
      s1: [
        { agent_session_id: 'v-old', title: 'verify', created_at: '2026-09-01T00:00:00Z' },
        {
          agent_session_id: 'v-dead',
          title: 'verify',
          status: 'STOPPED',
          created_at: '2026-10-02T00:00:00Z',
        },
        { agent_session_id: 'v-new', title: 'verify', created_at: '2026-10-01T00:00:00Z' },
      ],
    },
  })
  sweep({}, w.deps())
  const send = w.calls.find((c) => c.args[0] === 'chat' && c.args[1] === 'send')
  assert.deepEqual(send.args, ['chat', 'send', 'v-new', '/boss-verify 1 --claim tok-1'])
})

test('no live session: a detached boss new --pr whose stdio is not the gate’s', () => {
  const w = world({ verdicts: { 1: EXT }, sessions: [] })
  const r = sweep({}, w.deps())
  assert.equal(w.spawns.length, 1)
  const { cmd, args, options } = w.spawns[0]
  assert.equal(cmd, BOSS)
  assert.deepEqual(args.slice(0, 7), [
    'new',
    '--repo',
    '/repo',
    '--pr',
    '1',
    '--prompt',
    '/boss-verify 1 --claim tok-1',
  ])
  assert.ok(args.includes('--tmux-unattended'))
  assert.deepEqual(args.slice(-4), ['--tracker-id', 'BOS-1', '--tracker-source', 'linear'])
  assert.equal(options.detached, true)
  assert.ok(options.stdio.every((s) => s === 'ignore' || (typeof s === 'number' && s > 2)))
  assert.match(lines(r)[0], /^#1 dispatched: new-session \(log .*boss-verify-dispatch-.*\.log\)$/)
})

test('a lost claim routes nothing', () => {
  const w = world({
    verdicts: { 1: EXT },
    claimLost: [1],
    sessions: [{ id: 's1', pr_url: url(1) }],
    chats: { s1: [{ agent_session_id: 'chat-v', title: 'verify' }] },
  })
  const r = sweep({}, w.deps())
  assert.deepEqual(lines(r), ['#1 skipped: claim lost'])
  assert.equal(
    w.calls.some((c) => c.cmd === BOSS && c.args[0] !== 'ls'),
    false,
  )
  assert.equal(w.spawns.length, 0)
})

test('a send that times out is dispatch-unknown and is not retried', () => {
  const w = world({
    verdicts: { 1: EXT },
    sendTimesOut: true,
    sessions: [{ id: 's1', pr_url: url(1) }],
    chats: { s1: [{ agent_session_id: 'chat-v', title: 'verify' }] },
  })
  const r = sweep({}, w.deps())
  assert.deepEqual(lines(r), ['#1 dispatch-unknown: existing-verify-chat (ETIMEDOUT)'])
  assert.equal(w.calls.filter((c) => c.args[1] === 'send').length, 1)
})

test('the batch bound: 5 candidates and --batch 2 act on 2 and defer 3', () => {
  const w = world({ candidates: [1, 2, 3, 4, 5].map((n) => candidate(n)) })
  const r = sweep({ batch: 2 }, w.deps())
  assert.deepEqual(w.verbs()[0].slice(0, 3), ['candidates', '--limit', '6'])
  assert.deepEqual(lines(r), [
    `#1 merged ${sha(901)}`,
    `#2 merged ${sha(902)}`,
    '#3 deferred: budget',
    '#4 deferred: budget',
    '#5 deferred: budget',
  ])
  assert.equal(w.verbs().filter((v) => v[0] === 'judge').length, 2)
  assert.deepEqual(r.summary, { merged: 2, parked: 0, dispatched: 0, skipped: 0, deferred: 3 })
})

test('the deadline: no new PR starts once less than the start floor remains', () => {
  const budgetMs = 30_000
  // Each subprocess call advances the fake clock; after candidates + judge/post/merge for #1 the
  // clock sits past budgetMs - START_FLOOR_MS, so #2 must not start.
  const w = world({ candidates: [candidate(1), candidate(2)], tick: 4_000 })
  const r = sweep({ budgetMs }, w.deps())
  assert.ok(w.clock - T0 > budgetMs - START_FLOOR_MS)
  assert.deepEqual(lines(r), [`#1 merged ${sha(901)}`, '#2 deferred: budget'])
  assert.equal(
    w.verbs().some((v) => v[0] === 'judge' && v[2] === '2'),
    false,
  )
  // Every call is bounded by min(15 s, remaining).
  assert.ok(w.calls.every((c) => c.timeoutMs <= CALL_TIMEOUT_MS))
})

test('a clock already past the floor starts nothing at all', () => {
  const w = world({ candidates: [candidate(1)], clock: T0 })
  const r = sweep({ budgetMs: START_FLOOR_MS - 1 }, w.deps())
  assert.deepEqual(lines(r), ['#1 deferred: budget'])
  assert.deepEqual(
    w.verbs().map((v) => v[0]),
    ['candidates'],
  )
})

test('undispatchable: an unresolvable boss defers every PR that needs it and stops acting', () => {
  const w = world({
    candidates: [candidate(1), candidate(2)],
    boss: {
      ok: false,
      path: null,
      reason: 'BOSS_BIN is unset; no executable boss in 1 PATH entry',
    },
  })
  const r = sweep({}, w.deps())
  assert.match(r.undispatchable, /BOSS_BIN is unset/)
  assert.deepEqual(lines(r), [
    '#1 deferred: undispatchable (BOSS_BIN is unset; no executable boss in 1 PATH entry)',
    '#2 deferred: undispatchable',
  ])
  assert.equal(w.mutating().length, 0)

  const sock = world({
    lsResult: {
      ok: false,
      error: 'UNAVAILABLE',
      exit: 1,
      payload: { error: { code: 'UNAVAILABLE' } },
    },
  })
  assert.match(sweep({}, sock.deps()).undispatchable, /boss ls: UNAVAILABLE/)

  // A human park needs no boss, so it is not undispatchable.
  const human = world({
    verdicts: { 1: { verdict: 'human', reason: 'no-receipt' } },
    boss: { ok: false, path: null, reason: 'x' },
  })
  assert.equal(sweep({}, human.deps()).undispatchable, null)
})

test('unknown evidence: a failed candidates read throws and nothing is written or dispatched', () => {
  const w = world({ candidatesFail: true })
  assert.throws(() => sweep({}, w.deps()), /candidates failed/)
  assert.equal(w.mutating().length, 0)
  assert.equal(w.spawns.length, 0)
})

test('--dry-run: zero mutating calls and zero spawns, the planned routes printed', () => {
  const w = world({
    candidates: [candidate(1), candidate(2), candidate(3)],
    verdicts: {
      1: { verdict: 'human', reason: 'no-receipt' },
      2: EXT,
      3: { verdict: 'pass', reason: 'verified' },
    },
    sessions: [],
  })
  const r = sweep({ dryRun: true }, w.deps())
  assert.equal(w.mutating().length, 0)
  assert.equal(w.spawns.length, 0)
  assert.ok(w.posts().every((p) => p.includes('--dry-run')))
  assert.equal(
    w.verbs().some((v) => v[0] === 'merge'),
    false,
  )
  assert.deepEqual(lines(r), [
    '#1 parked: needs human (no-receipt) (dry-run)',
    '#2 dispatched: new-session (dry-run)',
    `#3 merged (dry-run: would merge ${sha(3)})`,
  ])
  assert.deepEqual(r.records[1].argv.slice(0, 5), ['new', '--repo', '/repo', '--pr', '2'])
})
