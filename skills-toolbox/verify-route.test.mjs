import { tempDir } from '../scripts/test-tmpdir.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawn as nodeSpawn, spawnSync } from 'node:child_process'
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, join } from 'node:path'

import {
  CALL_TIMEOUT_MS,
  DEFAULT_BATCH,
  discoveryLimit,
  DEFAULT_BUDGET_MS,
  OBSERVE_TIMEOUT_MS,
  START_FLOOR_MS,
  UsageError,
  liveSessionsFor,
  lsFailureIsUnreachable,
  newSessionArgv,
  parseSweepArgs,
  readMergeResult,
  spawnDetached,
  sweep,
  timedRunner,
  verifyMessage,
  MIN_QUOTA_SHARE,
  SWEEP_STATE_VERSION,
  forgetQuietFingerprint,
  readQuietFingerprint,
  storeQuietFingerprint,
  sweepScopeKey,
  sweepStatePath,
  tickWasQuiet,
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
 * is recorded; `mutating()` is every call that would write (a non-dry post, chat new/send), and
 * `spawns` every detached spawn. A merge is a detached `node verify-gate.mjs merge` spawn whose
 * result lands in its log `mergeDelay[pr]` ms of fake clock later (never, for Infinity); `sleep`
 * advances the clock, and `tick` (a number, or per-verb `ticks`) is what each `run` call costs.
 */
function world(opts = {}) {
  const w = {
    calls: [],
    spawns: [],
    merges: {},
    sleeps: [],
    reads: 0,
    clock: opts.clock ?? T0,
    tick: opts.tick ?? 0,
  }
  const verdicts = opts.verdicts ?? {}
  w.run = (cmd, args, options = {}) => {
    w.clock += (cmd === NODE ? opts.ticks?.[args[1]] : undefined) ?? w.tick
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
      if (verb === 'candidates') {
        if (opts.candidatesFail) return fail()
        // The tick fingerprint verify-gate would report, and the early `unchanged` answer to a
        // matching --quiet-fingerprint on a skippable tick.
        const tick =
          opts.fingerprint === undefined
            ? {}
            : { fingerprint: opts.fingerprint, skippable: opts.skippable ?? true }
        if (
          tick.skippable &&
          rest.includes('--quiet-fingerprint') &&
          flag('quiet-fingerprint') === opts.fingerprint
        )
          return ok({
            candidates: [],
            skipped: [],
            mergeRetries: [],
            unchanged: true,
            fingerprint: opts.fingerprint,
          })
        return ok({
          candidates: (opts.candidates ?? [candidate(1)]).filter(
            (c) =>
              (!opts.filterPrUrls ||
                !rest.includes('--pr-urls') ||
                flag('pr-urls').split(',').includes(c.url)) &&
              !flag('exclude-pr-urls')?.split(',').includes(c.url),
          ),
          skipped: [
            ...(opts.skipped ?? []),
            ...(opts.candidates ?? [candidate(1)])
              .filter(
                (c) =>
                  rest.includes('--exclude-pr-urls') &&
                  flag('exclude-pr-urls').split(',').includes(c.url),
              )
              .map((c) => ({ pr: c.pr, reason: 'remote-session' })),
          ],
          ...(opts.mergeRetries === undefined ? {} : { mergeRetries: opts.mergeRetries }),
          ...tick,
        })
      }
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
        if (verdict === 'human' && opts.ticketless?.includes(pr))
          return ok({ verdict, posted: true, trackerWrites: 'none', why: 'no-ticket' })
        return ok({ ...(dry ? { dryRun: true } : {}), verdict, posted: true })
      }
      if (verb === 'observe-merge') {
        const seen = opts.observed?.[pr]
        if (seen?.error) return fail(seen.error, { exit: null })
        return ok(seen ?? { merged: false, state: 'OPEN' })
      }
      return fail('unexpected-verb')
    }
    if (cmd === BOSS) {
      if (args[0] === 'ls') {
        if (args.includes('--all-daemons'))
          return opts.fleetResult ?? ok({ sessions: opts.fleetSessions ?? [] })
        if (opts.lsResult) return opts.lsResult
        return ok({ sessions: opts.sessions ?? [] })
      }
      if (args[0] === 'chats') return ok({ chats: opts.chats?.[args[2]] ?? [] })
      if (args[0] === 'chat' && args[1] === 'new')
        return ok({ chat: { agent_session_id: 'chat-new', session_id: args[2], title: 'verify' } })
      if (args[0] === 'chat' && args[1] === 'send') {
        if (opts.sendTimesOut) return fail('ETIMEDOUT', { exit: null })
        if (opts.sendResult) return opts.sendResult
        return ok({
          agent_session_id: args[2],
          delivery: { delivered: true, delivery_state: 'DELIVERY_STATE_SUBMITTED' },
        })
      }
      return fail('unexpected-boss')
    }
    if (cmd === 'gh' && args[0] === 'api' && args[1] === 'rate_limit')
      return opts.rateLimit ? ok({ resources: opts.rateLimit }) : fail()
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
    if (opts.spawnFails) throw Object.assign(new Error('spawn'), { code: opts.spawnFails })
    w.spawns.push({ cmd, args: [...args], options })
    if (cmd === NODE && args[1] === 'merge') {
      const pr = Number(args[args.indexOf('--pr') + 1])
      w.merges[pr] = {
        readyAt: w.clock + (opts.mergeDelay?.[pr] ?? 0),
        result: opts.mergeResults?.[pr] ?? { merged: true, mergeSha: sha(900 + pr) },
      }
    }
    return { pid: 4242, unref() {}, on() {} }
  }
  w.mergeSpawns = () => w.spawns.filter((s) => s.cmd === NODE && s.args[1] === 'merge')
  // A merge's log: stderr noise until its result is due, then the one JSON line verify-gate prints.
  w.readFile = (path) => {
    w.reads++
    const m = basename(path).match(/^boss-verify-merge-(\d+)-/)
    const merge = m ? w.merges[Number(m[1])] : undefined
    if (!merge) throw Object.assign(new Error('missing'), { code: 'ENOENT' })
    if (w.clock < merge.readyAt) return 'gh: still working\n'
    return `gh: still working\n${JSON.stringify(merge.result)}\n`
  }
  w.sleep = (ms) => {
    w.sleeps.push(ms)
    w.clock += ms
  }
  w.deps = (extra = {}) => ({
    run: w.run,
    nodeBin: NODE,
    readProjectConfig: () => null,
    now: () => w.clock,
    readFile: w.readFile,
    sleep: w.sleep,
    resolveBoss: () => opts.boss ?? { ok: true, path: BOSS, reason: '' },
    spawn: w.spawn,
    env: { PATH: '/usr/bin', LINEAR_API_KEY: 'k', REPO_DIR: '/repo' },
    cwd: '/repo',
    logDir: tempDir('verify-route-'),
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
    localSessionsOnly: false,
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
      localSessionsOnly: false,
      selectionArgs: ['--label', 'a,b', '--exclude-label=x'],
    },
  )
  assert.equal(parseSweepArgs(['--local-sessions-only', '--batch', '2']).localSessionsOnly, true)
  for (const argv of [
    ['42'],
    ['--bogus'],
    ['--batch', '0'],
    ['--batch', 'x'],
    ['--label'],
    ['--local-sessions-only=yes'],
    ['--local-sessions'],
  ])
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
  const dir = tempDir('verify-route-spawn-')
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

test('gate-only mode: a pass posts verified then hands the judged head to a detached merge', () => {
  const w = world()
  const r = sweep({}, w.deps())
  assert.deepEqual(
    w.verbs().map((v) => v[0]),
    ['candidates', 'judge', 'post'],
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
  const [merge] = w.mergeSpawns()
  assert.deepEqual(merge.args.slice(1), ['merge', '--pr', '1', '--head', sha(1), '--repo', 'o/r'])
  assert.match(merge.args[0], /verify-gate\.mjs$/)
  // Detached, its stdio a log file — never the gate's pipes.
  assert.equal(merge.options.detached, true)
  assert.ok(merge.options.stdio.every((s) => s === 'ignore' || (typeof s === 'number' && s > 2)))
  // verify-gate's own `boss` calls find the resolved binary first on PATH.
  assert.equal(merge.options.env.PATH, '/fake/bin:/usr/bin')
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
    verdicts: { 1: { verdict: 'human', reason: 'partial-build' } },
    trackerUnavailable: true,
  })
  assert.deepEqual(lines(sweep({}, u.deps())), ['#1 skipped: tracker writes unavailable'])
})

test('a ticketless park says so on its line', () => {
  const w = world({
    candidates: [candidate(1), candidate(5, { ticket: null })],
    verdicts: {
      1: { verdict: 'human', reason: 'partial-build' },
      5: { verdict: 'human', reason: 'no-receipt,partial-build' },
    },
    ticketless: [5],
  })
  assert.deepEqual(lines(sweep({}, w.deps())), [
    '#1 parked: needs human (partial-build)',
    '#5 parked: needs human (no-receipt,partial-build) · no ticket',
  ])
})

test('the sweep leaves label derivation to the human post gate', () => {
  const w = world({
    verdicts: { 1: { verdict: 'human', reason: 'human-review:ui', labels: ['web UI'] } },
  })
  sweep({}, w.deps())
  assert.equal(w.posts()[0].includes('--label'), false)
  assert.ok(w.posts()[0].includes('human-review:ui'))
})

test('a human post carries the policy fingerprint the judge read, so the park records it', () => {
  const fingerprint = '0123456789abcdef'
  const w = world({
    verdicts: { 1: { verdict: 'human', reason: 'no-receipt', policyFingerprint: fingerprint } },
  })
  sweep({}, w.deps())
  const human = w.posts()[0]
  assert.equal(human[human.indexOf('--policy') + 1], fingerprint)

  // A judge that could not read the policy names none, and none is posted.
  const unknown = world({ verdicts: { 1: { verdict: 'human', reason: 'no-receipt' } } })
  sweep({}, unknown.deps())
  assert.equal(unknown.posts()[0].includes('--policy'), false)
})

test('a set-valued park is posted unchanged and a set waiver is forwarded unchanged', () => {
  const set = 'always-human-path,human-review:ui'
  const w = world({ verdicts: { 1: { verdict: 'human', reason: set } } })
  const r = sweep({}, w.deps())
  assert.deepEqual(w.posts()[0].slice(5, 9), ['--verdict', 'human', '--reason', set])
  assert.deepEqual(lines(r), [`#1 parked: needs human (${set})`])

  const approved = world({
    candidates: [candidate(1, { approval: 'policy-park-approved', waive: set })],
  })
  sweep({}, approved.deps())
  const judgeCall = approved.verbs().find((v) => v[0] === 'judge')
  assert.equal(judgeCall[judgeCall.indexOf('--waive') + 1], set)
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
  assert.deepEqual(send.args, ['chat', 'send', 'chat-v', '/boss-verify 1 --claim tok-1', '--json'])
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
    ['chat', 'new', 's-new', '--title', 'verify', '--role', 'verify', '--json'],
    ['chat', 'send', 'chat-new', '/boss-verify 1 --claim tok-1', '--json'],
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
  assert.deepEqual(send.args, ['chat', 'send', 'v-new', '/boss-verify 1 --claim tok-1', '--json'])
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

test('only a submitted or queued delivery counts as dispatched', () => {
  const route = (sendResult) => {
    const w = world({
      verdicts: { 1: EXT },
      sendResult,
      sessions: [{ id: 's1', pr_url: url(1) }],
      chats: { s1: [{ agent_session_id: 'chat-v', title: 'verify' }] },
    })
    const r = sweep({}, w.deps())
    assert.equal(w.calls.filter((c) => c.args[1] === 'send').length, 1)
    return lines(r)
  }
  const delivery = (delivered, state) => ({
    ok: true,
    payload: { agent_session_id: 'chat-v', delivery: { delivered, delivery_state: state } },
    exit: 0,
  })
  assert.deepEqual(route(delivery(true, 'DELIVERY_STATE_SUBMITTED')), [
    '#1 dispatched: existing-verify-chat',
  ])
  assert.deepEqual(route(delivery(true, 'DELIVERY_STATE_QUEUED')), [
    '#1 dispatched: existing-verify-chat',
  ])
  assert.deepEqual(route(delivery(false, 'DELIVERY_STATE_UNCONFIRMED')), [
    '#1 dispatch-unknown: existing-verify-chat (DELIVERY_STATE_UNCONFIRMED)',
  ])
  assert.deepEqual(route({ ok: false, error: 'unreadable-json', exit: 0, output: 'sent' }), [
    '#1 dispatch-unknown: existing-verify-chat (unreadable-json)',
  ])
  assert.deepEqual(route({ ok: true, payload: { agent_session_id: 'chat-v' }, exit: 0 }), [
    '#1 dispatch-unknown: existing-verify-chat (unreadable-delivery)',
  ])
})

test('the batch bound: 5 candidates and --batch 2 act on 2 and defer 3', () => {
  const w = world({ candidates: [1, 2, 3, 4, 5].map((n) => candidate(n)) })
  const r = sweep({ batch: 2 }, w.deps())
  assert.deepEqual(w.verbs()[0].slice(0, 3), ['candidates', '--limit', '25'])
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
  // Each subprocess call advances the fake clock; after candidates + judge/post for #1 the clock
  // sits past budgetMs - START_FLOOR_MS, so #2 must not start.
  const w = world({ candidates: [candidate(1), candidate(2)], tick: 6_000 })
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

// ---------------------------------------------------------------------------
// The merge hand-off: a merge runs detached and is settled inside the budget (BOS-1532).

test('readMergeResult: the last complete JSON line with a boolean merged, past stderr noise', () => {
  const done = JSON.stringify({ merged: true, mergeSha: sha(5) })
  assert.deepEqual(readMergeResult(`warn: x\n${done}\nlate stderr\n`), {
    merged: true,
    mergeSha: sha(5),
  })
  assert.equal(readMergeResult(''), null)
  assert.equal(readMergeResult('gh: still working\n'), null)
  // A line still being written is not a result yet, even when it would parse.
  assert.equal(readMergeResult(done), null)
  assert.equal(readMergeResult('{"merged": tr'), null)
  // Only an object carrying a boolean `merged` is verify-gate's terminal line.
  assert.equal(readMergeResult('{"progress":1}\n[1]\n"merged"\n{"merged":"yes"}\n'), null)
  assert.equal(readMergeResult(null), null)
})

test('spawnDetached names a labelled log after its label; the default stays a dispatch log', () => {
  const dir = tempDir('verify-route-label-')
  const spawn = () => ({ pid: 1, unref() {}, on() {} })
  const a = spawnDetached(BOSS, [], { logDir: dir, now: () => 5, label: 'merge-7' }, { spawn })
  const b = spawnDetached(BOSS, [], { logDir: dir, now: () => 5, label: 'merge-8' }, { spawn })
  const c = spawnDetached(BOSS, [], { logDir: dir, now: () => 5 }, { spawn })
  assert.match(a.log, /boss-verify-merge-7-5-\d+\.log$/)
  assert.match(b.log, /boss-verify-merge-8-5-\d+\.log$/)
  assert.match(c.log, /boss-verify-dispatch-5-\d+\.log$/)
})

test('a slow merge that outlasts the per-call timeout and then succeeds is reported merged', () => {
  const w = world({ mergeDelay: { 1: CALL_TIMEOUT_MS + 5_000 } })
  const r = sweep({}, w.deps())
  assert.deepEqual(lines(r), [`#1 merged ${sha(901)}`])
  assert.equal(r.summary.merged, 1)
  assert.equal(r.summary.skipped, 0)
  assert.ok(lines(r).every((l) => !l.includes('ETIMEDOUT')))
  assert.ok(w.sleeps.length > 0, 'the settle phase polled the log')
  assert.ok(w.clock - T0 <= DEFAULT_BUDGET_MS)
  assert.equal(
    w.verbs().some((v) => v[0] === 'observe-merge'),
    false,
    'a merge whose result landed needs no observation',
  )
})

test('a merge GitHub confirms only after the poll window is reported merged via observe-merge', () => {
  const w = world({
    mergeDelay: { 1: Infinity },
    observed: { 1: { merged: true, mergeSha: sha(777), observed: 'merged' } },
  })
  const r = sweep({}, w.deps())
  assert.deepEqual(lines(r), [`#1 merged ${sha(777)}`])
  const observe = w.calls.filter((c) => c.args[1] === 'observe-merge')
  assert.equal(observe.length, 1)
  assert.deepEqual(observe[0].args.slice(2), ['--pr', '1', '--repo', 'o/r'])
  assert.ok(observe[0].timeoutMs <= OBSERVE_TIMEOUT_MS)
  assert.equal(w.mutating().filter((c) => c.args[1] === 'observe-merge').length, 0)
})

test('a merge still in flight at the deadline is dispatched, never skipped or ETIMEDOUT', () => {
  const w = world({ mergeDelay: { 1: Infinity } })
  const r = sweep({}, w.deps())
  const [merge] = w.mergeSpawns()
  assert.ok(merge)
  assert.equal(r.records[0].outcome, 'dispatched')
  assert.match(
    lines(r)[0],
    /^#1 dispatched: merge in flight \(log .*boss-verify-merge-1-.*\.log\)$/,
  )
  assert.deepEqual(r.summary, { merged: 0, parked: 0, dispatched: 1, skipped: 0, deferred: 0 })

  // An observation that itself times out is the same hand-off, not an error line.
  const t = world({ mergeDelay: { 1: Infinity }, observed: { 1: { error: 'ETIMEDOUT' } } })
  const timedOut = sweep({}, t.deps())
  assert.match(lines(timedOut)[0], /^#1 dispatched: merge in flight \(log /)
  assert.ok(lines(timedOut).every((l) => !l.includes('ETIMEDOUT')))
})

test('one slow merge no longer defers the others: three passes are all handed off', () => {
  // The incident's clock: candidates + judge + post, then a merge slower than CALL_TIMEOUT_MS.
  // Run synchronously, two such merges left #3 under START_FLOOR_MS and deferred it.
  const w = world({
    candidates: [candidate(1), candidate(2), candidate(3)],
    ticks: { candidates: 2_000, judge: 1_000, post: 1_000 },
    mergeDelay: {
      1: CALL_TIMEOUT_MS + 1_000,
      2: CALL_TIMEOUT_MS + 1_000,
      3: CALL_TIMEOUT_MS + 1_000,
    },
  })
  const r = sweep({}, w.deps())
  assert.equal(w.mergeSpawns().length, 3)
  assert.deepEqual(lines(r), [
    `#1 merged ${sha(901)}`,
    `#2 merged ${sha(902)}`,
    `#3 merged ${sha(903)}`,
  ])
  assert.equal(r.summary.deferred, 0)
  assert.ok(w.clock - T0 <= DEFAULT_BUDGET_MS)
})

test('a refused merge is still reported: reverify, wait reasons, and a lost claim', () => {
  const w = world({
    candidates: [candidate(1), candidate(2), candidate(3)],
    mergeResults: {
      1: { merged: false, verdict: 'reverify', reason: 'head-mismatch' },
      2: { merged: false, abandoned: 'claim-lost' },
      3: {
        merged: false,
        verdict: 'wait',
        reasons: ['merge-state-blocked', 'ci-not-green-on-head'],
      },
    },
  })
  const r = sweep({}, w.deps())
  assert.deepEqual(lines(r), [
    '#1 reverify: head-mismatch',
    '#2 skipped: claim lost',
    '#3 wait: merge-state-blocked,ci-not-green-on-head',
  ])
  assert.equal(r.summary.skipped, 3)
})

test('a crashed merge prints a merge-error result the sweep settles as skipped', () => {
  const gate = new URL('./verify-gate.mjs', import.meta.url).pathname
  const out = spawnSync(process.execPath, [gate, 'merge', '--pr', '1'], { encoding: 'utf8' })
  assert.notEqual(out.status, 0)
  assert.match(out.stderr, /merge needs --head/)
  const result = readMergeResult(out.stdout)
  assert.equal(result?.merged, false)
  assert.equal(result?.reason, 'merge-error')
  assert.match(result?.error, /merge needs --head/)

  const w = world({ mergeResults: { 1: result } })
  const r = sweep({}, w.deps())
  assert.equal(r.records[0].outcome, 'skipped')
  assert.deepEqual(lines(r), ['#1 merge: merge-error'])
})

test('a merge that crashed after merging (a failed tracker write) is reported merged, not skipped', () => {
  const w = world({
    mergeResults: { 1: { merged: false, reason: 'merge-error', error: 'tracker write failed' } },
    observed: { 1: { merged: true, mergeSha: sha(778), observed: 'merged' } },
  })
  const r = sweep({}, w.deps())
  assert.deepEqual(lines(r), [`#1 merged ${sha(778)}`])
  assert.equal(r.summary.merged, 1)
  assert.equal(r.summary.skipped, 0)
})

test('every observation fits the budget, and none starts once less than a second is left', () => {
  const w = world({
    candidates: [candidate(1), candidate(2), candidate(3)],
    mergeDelay: { 1: Infinity, 2: Infinity, 3: Infinity },
    ticks: { 'observe-merge': OBSERVE_TIMEOUT_MS },
  })
  const r = sweep({}, w.deps())
  const observe = w.calls.filter((c) => c.args[1] === 'observe-merge')
  assert.equal(observe.length, 3)
  assert.ok(observe.every((c) => c.timeoutMs <= OBSERVE_TIMEOUT_MS))
  assert.ok(w.clock - T0 <= DEFAULT_BUDGET_MS, `clock ran ${w.clock - T0} ms`)
  assert.equal(r.summary.dispatched, 3)

  // The candidate loop spent the whole budget: the merge is handed off and not observed at all.
  const late = world({ mergeDelay: { 1: Infinity }, ticks: { judge: 8_000, post: 8_000 } })
  const lr = sweep({ budgetMs: 16_000 }, late.deps())
  assert.equal(
    late.calls.some((c) => c.args[1] === 'observe-merge'),
    false,
  )
  assert.match(lines(lr)[0], /^#1 dispatched: merge in flight/)
  assert.ok(late.clock - T0 <= 16_000)
})

test('handed-off merges keep candidate order and each gets its own log', () => {
  const w = world({
    candidates: [candidate(1), candidate(2), candidate(3)],
    verdicts: { 2: { verdict: 'human', reason: 'no-receipt' } },
    // #3's merge lands before #1's.
    mergeDelay: { 1: 3_000, 3: 0 },
  })
  const r = sweep({}, w.deps())
  assert.deepEqual(lines(r), [
    `#1 merged ${sha(901)}`,
    '#2 parked: needs human (no-receipt)',
    `#3 merged ${sha(903)}`,
  ])
  const logs = r.records.filter((rec) => rec.log).map((rec) => rec.log)
  assert.equal(logs.length, 2)
  assert.notEqual(logs[0], logs[1])
})

test('a merge that cannot be spawned is a skip naming why', () => {
  const w = world({ spawnFails: 'EAGAIN' })
  const r = sweep({}, w.deps())
  assert.deepEqual(lines(r), ['#1 merge: spawn failed (EAGAIN)'])
  assert.equal(r.summary.skipped, 1)
})

test('real-process settle: a detached child writes its result to the log and the sweep reads it', () => {
  const mergeSha = sha(4321)
  const script = `setTimeout(() => {
    process.stderr.write('gh: merging\\n')
    process.stdout.write(JSON.stringify({ merged: true, mergeSha: '${mergeSha}' }) + '\\n')
  }, 300)`
  const w = world()
  const started = Date.now()
  const r = sweep(
    {},
    w.deps({
      // A real spawn needs a real cwd; the fake world's `/repo` does not exist.
      cwd: tmpdir(),
      now: Date.now,
      readFile: undefined,
      sleep: undefined,
      spawn: (cmd, args, options) => nodeSpawn(process.execPath, ['-e', script], options),
    }),
  )
  assert.deepEqual(lines(r), [`#1 merged ${mergeSha}`])
  assert.ok(Date.now() - started < DEFAULT_BUDGET_MS)
  assert.match(readFileSync(r.records[0].log, 'utf8'), /gh: merging/)
})

test('discovery keeps a floor for small batches and scales larger batches', () => {
  for (const [batch, limit] of [
    [1, 25],
    [8, 25],
    [9, 27],
    [10, 30],
  ]) {
    assert.equal(discoveryLimit(batch), limit)
  }
  const w = world()
  sweep({ batch: 1 }, w.deps())
  assert.deepEqual(w.verbs()[0], ['candidates', '--limit', '25'])
})

test('merge retries run head-pinned without judge or post and keep the judge batch', () => {
  const w = world({ mergeRetries: [candidate(10)], candidates: [candidate(1), candidate(2)] })
  const r = sweep({ batch: 1 }, w.deps())
  assert.deepEqual(lines(r), [
    `#10 merged ${sha(910)} (merge retry)`,
    `#1 merged ${sha(901)}`,
    '#2 deferred: budget',
  ])
  assert.equal(r.records[0].retry, true)
  assert.deepEqual(w.mergeSpawns()[0].args.slice(1), [
    'merge',
    '--pr',
    '10',
    '--head',
    sha(10),
    '--repo',
    'o/r',
  ])
  assert.equal(
    w.verbs().some((v) => ['judge', 'post'].includes(v[0]) && v.includes('10')),
    false,
  )
})

test('merge retries have a separate batch cap and a time budget', () => {
  const w = world({ candidates: [], mergeRetries: [candidate(1), candidate(2), candidate(3)] })
  const r = sweep({ batch: 2 }, w.deps())
  assert.equal(w.mergeSpawns().length, 2)
  assert.equal(r.records[2].outcome, 'deferred')
  assert.equal(r.records[2].retry, true)
  assert.match(r.records[2].line, /deferred: budget/)
  const expired = world({ candidates: [], mergeRetries: [candidate(1)] })
  assert.match(
    lines(sweep({ budgetMs: START_FLOOR_MS - 1 }, expired.deps()))[0],
    /deferred: budget/,
  )
  assert.equal(expired.spawns.length, 0)
})

test('merge retries settle refused and in-flight merges using the existing observer', () => {
  const w = world({
    candidates: [],
    mergeRetries: [candidate(1), candidate(2)],
    mergeResults: { 1: { merged: false, verdict: 'wait', reasons: ['sent-back', 'ci-red'] } },
    mergeDelay: { 2: Infinity },
  })
  const r = sweep({}, w.deps())
  assert.equal(r.records[0].outcome, 'skipped')
  assert.match(r.records[0].line, /wait: sent-back,ci-red.*merge retry/)
  assert.equal(r.records[1].outcome, 'dispatched')
  assert.match(r.records[1].line, /merge in flight.*merge retry/)
  assert.ok(r.records.every((r) => r.retry === true))
})

test('a merge refused for no local session says whether the PR was told', () => {
  const w = world({
    candidates: [],
    mergeRetries: [candidate(1)],
    mergeResults: {
      1: { merged: false, verdict: 'human', reason: 'no-session', notice: 'posted' },
    },
  })
  assert.deepEqual(lines(sweep({}, w.deps())), [
    '#1 human: no-session (PR notice posted) (merge retry)',
  ])
})

test('merge retries dry-run without mutations and defer when boss is unreachable', () => {
  const w = world({ candidates: [], mergeRetries: [candidate(1)] })
  const r = sweep({ dryRun: true }, w.deps())
  assert.match(lines(r)[0], new RegExp(`dry-run: would retry merge ${sha(1)}`))
  assert.equal(r.records[0].retry, true)
  assert.equal(w.spawns.length, 0)
  assert.equal(w.mutating().length, 0)
  const missing = world({
    mergeRetries: [candidate(10), candidate(11)],
    boss: { ok: false, reason: 'missing' },
  })
  const deferred = sweep({}, missing.deps())
  assert.ok(
    deferred.records.every((r) => r.outcome === 'deferred' && /undispatchable/.test(r.line)),
  )
  assert.equal(missing.spawns.length, 0)
})

// ---------------------------------------------------------------------------
// --local-sessions-only: a daemon sharing the repo with another machine acts on its own PRs only.

const LOCAL = { localSessionsOnly: true }
const liveRow = (n, extra = {}) => ({
  id: `s${n}`,
  pr_url: url(n),
  updated_at: '2026-10-01T00:00:00Z',
  ...extra,
})
// Every call that names `pr` (a judge, post or merge argv, or a boss argv), plus its spawns.
const touched = (w, pr) => [
  ...w.calls.filter((c) => c.args.includes(String(pr)) || c.args.includes(url(pr))),
  ...w.spawns.filter((s) => s.args.includes(String(pr))),
]

test('local-sessions-only: a PR whose session is elsewhere gets no judge, claim, post, spawn or merge', () => {
  for (const verdict of [
    { verdict: 'pass', reason: 'verified' },
    EXT,
    { verdict: 'human', reason: 'no-receipt' },
  ]) {
    const w = world({
      verdicts: { 2: verdict },
      candidates: [candidate(2)],
      // Only an archived row binds this PR here, which is no live session.
      sessions: [liveRow(2, { archived_at: '2026-10-02T00:00:00Z' }), liveRow(9)],
    })
    const r = sweep(LOCAL, w.deps())
    assert.deepEqual(touched(w, 2), [], verdict.verdict)
    assert.equal(w.mutating().length, 0)
    assert.equal(w.spawns.length, 0)
    assert.deepEqual(lines(r), ['#2 skipped: session on another machine'])
    assert.equal(r.records[0].remote, true)
    assert.equal(r.undispatchable, null)
  }
})

test('local-sessions-only: a local PR is handled exactly as without the flag', () => {
  const opts = () => ({
    verdicts: { 1: EXT },
    candidates: [candidate(1)],
    sessions: [liveRow(1)],
    chats: { s1: [{ agent_session_id: 'chat-v', title: 'verify' }] },
  })
  const plain = world(opts())
  const local = world(opts())
  const a = sweep({}, plain.deps())
  const b = sweep(LOCAL, local.deps())
  assert.deepEqual(lines(b), lines(a))
  assert.deepEqual(lines(b), ['#1 dispatched: existing-verify-chat'])
  // The same calls, except that local mode reads the session list before discovery and names its
  // PRs to discovery.
  const shape = (w) => w.calls.map((c) => [c.cmd, ...c.args.slice(c.cmd === NODE ? 1 : 0)])
  const withoutLs = (w) => shape(w).filter((c) => !(c[0] === BOSS && c[1] === 'ls'))
  assert.deepEqual(shape(local)[0], [BOSS, 'ls', '--json'])
  assert.equal(shape(local).length, shape(plain).length)
  const discovery = (w) => withoutLs(w).find((c) => c[1] === 'candidates')
  assert.deepEqual(discovery(local), [...discovery(plain), '--pr-urls', url(1)])
  const others = (w) => withoutLs(w).filter((c) => c[1] !== 'candidates')
  assert.deepEqual(others(local), others(plain))

  const pass = world({ candidates: [candidate(1)], sessions: [liveRow(1)] })
  assert.deepEqual(lines(sweep(LOCAL, pass.deps())), [`#1 merged ${sha(901)}`])
  assert.equal(pass.mergeSpawns().length, 1)
})

test('local-sessions-only: no live PR session here means no discovery and no GitHub read', () => {
  for (const sessions of [
    [],
    [liveRow(2, { archived_at: '2026-10-02T00:00:00Z' })],
    [{ id: 's1' }],
  ]) {
    const w = world({ sessions, rateLimit: { core: { limit: 5000, remaining: 5000 } } })
    const r = sweep(LOCAL, w.deps())
    assert.deepEqual(
      w.calls.map((c) => [c.cmd, c.args[0]]),
      [[BOSS, 'ls']],
    )
    assert.equal(r.idle, 'no session on this machine has a PR')
    assert.deepEqual(r.records, [])
  }
})

test("local-sessions-only: discovery is told this machine's PRs, and the PRs it skipped print as remote", () => {
  const w = world({
    candidates: [candidate(3)],
    skipped: [
      { ticket: 'BOS-1', pr: 1, reason: 'remote-session' },
      { ticket: 'BOS-2', pr: 2, reason: 'verified' },
    ],
    sessions: [liveRow(3), liveRow(3, { id: 's3b' }), liveRow(4)],
  })
  const r = sweep(LOCAL, w.deps())
  assert.deepEqual(
    w.verbs().find((v) => v[0] === 'candidates'),
    ['candidates', '--limit', '25', '--pr-urls', `${url(3)},${url(4)}`],
  )
  assert.equal(lines(r)[0], '#1 skipped: session on another machine')
  assert.equal(r.records[0].remote, true)
  assert.equal(
    lines(r).some((line) => line.startsWith('#2 ')),
    false,
    'other skips stay unprinted',
  )
  assert.equal(
    lines(r).some((line) => line.startsWith('#3 ')),
    true,
  )
})

test('a run reads nothing from GitHub while either quota is below MIN_QUOTA_SHARE', () => {
  const healthy = { limit: 5000, remaining: 4000, reset: 1_800_000_000 }
  for (const [name, rateLimit, idle] of [
    [
      'core low',
      { core: { ...healthy, remaining: 1000 }, graphql: healthy },
      /^GitHub core quota low \(1000\/5000 left, resets 2027-01-15T08:00:00\.000Z\)$/,
    ],
    [
      'graphql low',
      { core: healthy, graphql: { ...healthy, remaining: 10 } },
      /^GitHub graphql quota low \(10\/5000 left/,
    ],
    ['both healthy', { core: healthy, graphql: healthy }, null],
    ['unreadable', undefined, null],
    ['malformed', { core: { limit: 0, remaining: 0 } }, null],
  ]) {
    const w = world({ rateLimit, candidates: [] })
    const r = sweep({}, w.deps())
    const discovered = w.verbs().some((v) => v[0] === 'candidates')
    if (idle) {
      assert.match(r.idle, idle, name)
      assert.equal(discovered, false, name)
    } else {
      assert.equal(r.idle, undefined, name)
      assert.equal(discovered, true, name)
    }
  }
  assert.equal(MIN_QUOTA_SHARE, 0.25)
})

test('local-sessions-only: remote PRs never eat the batch, and local ones still go first-come', () => {
  const w = world({
    candidates: [candidate(1), candidate(2), candidate(3), candidate(4)],
    sessions: [liveRow(3), liveRow(4)],
  })
  const r = sweep({ ...LOCAL, batch: 1 }, w.deps())
  assert.deepEqual(lines(r), [
    '#1 skipped: session on another machine',
    '#2 skipped: session on another machine',
    `#3 merged ${sha(903)}`,
    '#4 deferred: budget',
  ])
  assert.deepEqual(r.summary, { merged: 1, parked: 0, dispatched: 0, skipped: 2, deferred: 1 })
})

test('local-sessions-only: merge retries skip heads whose session is elsewhere', () => {
  const w = world({
    candidates: [],
    mergeRetries: [candidate(10), candidate(11), candidate(12)],
    sessions: [liveRow(11)],
  })
  const r = sweep({ ...LOCAL, batch: 1 }, w.deps())
  assert.deepEqual(lines(r), [
    '#10 skipped: session on another machine',
    `#11 merged ${sha(911)} (merge retry)`,
    '#12 skipped: session on another machine',
  ])
  assert.deepEqual(
    w.mergeSpawns().map((s) => s.args[3]),
    ['11'],
  )
  assert.ok(r.records.every((rec) => rec.retry === true))
})

test('local-sessions-only: an unreadable session list throws before discovery, writing nothing', () => {
  const cases = [
    { boss: { ok: false, path: null, reason: 'BOSS_BIN is unset' } },
    { lsResult: { ok: false, error: 'UNAVAILABLE', exit: 1, output: '' } },
    { lsResult: { ok: false, error: 'PERMISSION_DENIED', exit: 1, output: '', payload: {} } },
    { lsResult: { ok: true, payload: { rows: [] }, output: '{}', exit: 0 } },
  ]
  for (const extra of cases) {
    const w = world({ candidates: [candidate(1)], verdicts: { 1: EXT }, ...extra })
    assert.throws(
      () => sweep(LOCAL, w.deps()),
      /--local-sessions-only: this machine's session list is unreadable/,
      JSON.stringify(extra),
    )
    assert.deepEqual(w.verbs(), [], JSON.stringify(extra))
    assert.equal(w.spawns.length, 0)
  }
  // Without the flag the same unreachable boss is the old undispatchable hand-off.
  const w = world({ verdicts: { 1: EXT }, boss: { ok: false, path: null, reason: 'x' } })
  assert.equal(sweep({}, w.deps()).undispatchable, 'x')
})

// ---------------------------------------------------------------------------
// Unchanged ticks: the cron gate's sweep stores a quiet tick's fingerprint and skips a match.

const FP = 'f'.repeat(64)
const SKIP = { skipUnchanged: true }
const stateDir = () => tempDir('verify-route-state-')
const stateIn = (dir) => join(dir, 'boss-verify', 'sweep-state.json')
const KEY = sweepScopeKey({})
const storedFp = (file, key = KEY) => readQuietFingerprint(file, key)
// A quiet tick's discovery: nothing to judge, only steady skips.
const quietSkips = [
  { ticket: 'BOS-1', pr: 1, reason: 'parked' },
  { ticket: 'BOS-2', pr: 2, reason: 'claimed' },
  { ticket: 'BOS-3', pr: 3, reason: 'verified' },
  { ticket: 'BOS-4', pr: 4, reason: 'remote-session' },
  { ticket: null, pr: 5, reason: 'no-session' },
  { ticket: 'BOS-6', pr: 6, reason: 'ci-red' },
]
const quietWorld = (opts = {}) =>
  world({ candidates: [], skipped: quietSkips, fingerprint: FP, ...opts })
const candidatesArgs = (w) => w.verbs().filter((v) => v[0] === 'candidates')

test('a quiet tick stores its fingerprint, and each unchanged tick after it judges, posts and merges nothing', () => {
  const file = stateIn(stateDir())
  const first = quietWorld()
  const r = sweep(SKIP, first.deps({ stateFile: file }))
  assert.equal(r.unchanged, undefined)
  assert.equal(storedFp(file), FP)
  assert.equal(candidatesArgs(first)[0].includes('--quiet-fingerprint'), false)
  const state = JSON.parse(readFileSync(file, 'utf8'))
  assert.equal(state.version, SWEEP_STATE_VERSION)
  assert.match(state.entries[KEY].at, /^\d{4}-\d{2}-\d{2}T/)

  for (let tick = 0; tick < 3; tick++) {
    const w = quietWorld({ candidates: [candidate(9)], mergeRetries: [candidate(8)] })
    const u = sweep(SKIP, w.deps({ stateFile: file }))
    assert.equal(u.unchanged, true, `tick ${tick}`)
    assert.deepEqual([u.records, u.undispatchable], [[], null])
    assert.deepEqual(
      w.verbs().map((v) => v[0]),
      ['candidates'],
      'one candidates verb and no judge, post or merge',
    )
    const args = candidatesArgs(w)[0]
    assert.equal(args[args.indexOf('--quiet-fingerprint') + 1], FP)
    assert.deepEqual(
      w.calls.filter((c) => c.cmd !== NODE).map((c) => [c.cmd, ...c.args]),
      [['gh', 'api', 'rate_limit']],
      'plus the free rate-limit read',
    )
    assert.equal(w.spawns.length, 0)
    assert.equal(storedFp(file), FP, 'the entry stands')
  }
})

test('a tick that is not quiet removes the stored fingerprint, or never stores one', () => {
  const merged = { 1: { merged: true, mergeSha: sha(901), writes: [{ kind: 'merge' }] } }
  const cases = [
    ['dispatched', { candidates: [candidate(1)], verdicts: { 1: EXT } }],
    ['merged', { candidates: [candidate(1)], mergeResults: merged }],
    ['in flight', { candidates: [candidate(1)], mergeDelay: { 1: Infinity } }],
    [
      'parked',
      {
        candidates: [candidate(1)],
        verdicts: { 1: { verdict: 'human', reason: 'partial-build' } },
      },
    ],
    [
      'waited',
      {
        candidates: [candidate(1)],
        verdicts: { 1: { verdict: 'wait', reason: 'ci-not-settled' } },
      },
    ],
    [
      'an unknown skip',
      { skipped: [...quietSkips, { ticket: 'BOS-7', pr: 7, reason: 'unknown:status' }] },
    ],
    ['an incomplete discovery', { skippable: false }],
    ['a retry that merged', { mergeRetries: [candidate(1)], mergeResults: merged }],
    [
      'a retry that waited on an unreadable read',
      {
        mergeRetries: [candidate(1)],
        mergeResults: {
          1: { merged: false, verdict: 'wait', reason: 'pr-unreadable', writes: [] },
        },
      },
    ],
    // Reasons-only and CI waits may rest on CI evidence the fingerprint does not hash.
    ...[
      ['ci-not-green-on-head'],
      ['ci-not-settled'],
      ['merge-state-unknown'],
      ['ci-not-settled', 'ci-not-green-on-head'],
    ].map((reasons) => [
      `a retry that waited on ${reasons.join(',')}`,
      {
        mergeRetries: [candidate(1)],
        mergeResults: { 1: { merged: false, verdict: 'wait', reasons, writes: [] } },
      },
    ]),
    [
      'a retry that waited on an absent gate',
      {
        mergeRetries: [candidate(1)],
        mergeResults: {
          1: {
            merged: false,
            verdict: 'wait',
            reason: 'ci-absent-gate',
            reasons: ['ci-absent-gate'],
            writes: [],
          },
        },
      },
    ],
    [
      'a retry that told the PR',
      {
        mergeRetries: [candidate(1)],
        mergeResults: {
          1: {
            merged: false,
            verdict: 'human',
            reason: 'no-session',
            notice: 'posted',
            writes: [{ kind: 'pr-comment' }],
          },
        },
      },
    ],
    [
      'an undispatchable retry',
      { mergeRetries: [candidate(1)], boss: { ok: false, path: null, reason: 'x' } },
    ],
  ]
  for (const [name, opts] of cases) {
    const file = stateIn(stateDir())
    storeQuietFingerprint(file, KEY, 'e'.repeat(64), 'then')
    sweep(SKIP, quietWorld(opts).deps({ stateFile: file }))
    assert.equal(storedFp(file), null, name)
  }

  // Deferred by budget: 5 candidates with batch 3 never leave a stored fingerprint, tick after tick.
  const file = stateIn(stateDir())
  for (let tick = 0; tick < 3; tick++) {
    const w = quietWorld({
      candidates: [1, 2, 3, 4, 5].map((n) => candidate(n)),
      verdicts: Object.fromEntries(
        [1, 2, 3, 4, 5].map((n) => [n, { verdict: 'human', reason: 'partial-build' }]),
      ),
    })
    const r = sweep({ ...SKIP, batch: 3 }, w.deps({ stateFile: file }))
    assert.ok(r.records.some((x) => x.outcome === 'deferred'))
    assert.equal(storedFp(file), null, `tick ${tick}`)
  }

  // A retry that settled as a write-free wait is quiet: the merge state in the fingerprint reopens it.
  const waiting = stateIn(stateDir())
  sweep(
    SKIP,
    quietWorld({
      mergeRetries: [candidate(1)],
      mergeResults: {
        1: {
          merged: false,
          verdict: 'wait',
          reason: 'merge-state-dirty',
          reasons: ['merge-state-dirty'],
          writes: [],
        },
      },
    }).deps({ stateFile: waiting }),
  )
  assert.equal(storedFp(waiting), FP)
})

test('a dry run and a hand-run sweep never pass --quiet-fingerprint and never touch the state', () => {
  for (const options of [{ ...SKIP, dryRun: true }, {}, { dryRun: true }]) {
    const file = stateIn(stateDir())
    storeQuietFingerprint(file, KEY, FP, 'then')
    const before = readFileSync(file, 'utf8')
    for (const opts of [{}, { candidates: [candidate(1)], verdicts: { 1: EXT } }]) {
      const w = quietWorld(opts)
      const r = sweep(options, w.deps({ stateFile: file }))
      assert.equal(r.unchanged, undefined, JSON.stringify(options))
      assert.equal(candidatesArgs(w)[0].includes('--quiet-fingerprint'), false)
    }
    assert.equal(readFileSync(file, 'utf8'), before, JSON.stringify(options))
  }
})

test('missing, corrupt, other-version or unwritable state runs a full tick and never throws', () => {
  const corrupt = (file, text) => {
    storeQuietFingerprint(file, KEY, 'x', 'then')
    writeFileSync(file, text)
  }
  const cases = [
    ['missing', () => {}],
    ['corrupt', (file) => corrupt(file, '{not json')],
    [
      'other version',
      (file) =>
        corrupt(file, JSON.stringify({ version: 2, entries: { [KEY]: { fingerprint: FP } } })),
    ],
    [
      'entries not an object',
      (file) => corrupt(file, JSON.stringify({ version: 1, entries: [FP] })),
    ],
    [
      'another scope only',
      (file) => storeQuietFingerprint(file, sweepScopeKey({ localSessionsOnly: true }), FP, 'then'),
    ],
  ]
  for (const [name, seed] of cases) {
    const file = stateIn(stateDir())
    seed(file)
    const w = quietWorld({
      candidates: [candidate(1)],
      verdicts: { 1: { verdict: 'wait', reason: 'x' } },
    })
    sweep(SKIP, w.deps({ stateFile: file }))
    assert.equal(candidatesArgs(w)[0].includes('--quiet-fingerprint'), false, name)
    assert.equal(
      w.verbs().some((v) => v[0] === 'judge'),
      true,
      name,
    )
  }
  // A directory that cannot be created: the tick runs and nothing throws, quiet or not.
  const blocked = join(stateDir(), 'file')
  writeFileSync(blocked, '')
  const file = join(blocked, 'boss-verify', 'sweep-state.json')
  const r = sweep(SKIP, quietWorld().deps({ stateFile: file }))
  assert.deepEqual(r.summary, { merged: 0, parked: 0, dispatched: 0, skipped: 1, deferred: 0 })
  assert.equal(existsSync(file), false)
  assert.doesNotThrow(() =>
    sweep(
      SKIP,
      quietWorld({ candidates: [candidate(1)], verdicts: { 1: EXT } }).deps({ stateFile: file }),
    ),
  )
})

test('each selection and --local-sessions-only scope keeps its own entry', () => {
  const file = stateIn(stateDir())
  const scopes = [
    {},
    { selectionArgs: ['--label', 'backend'] },
    { selectionArgs: ['--label', 'frontend'] },
    { localSessionsOnly: true },
  ]
  assert.equal(new Set(scopes.map(sweepScopeKey)).size, scopes.length)
  scopes.forEach((scope, i) => storeQuietFingerprint(file, sweepScopeKey(scope), String(i), 'then'))
  scopes.forEach((scope, i) =>
    assert.equal(readQuietFingerprint(file, sweepScopeKey(scope)), String(i)),
  )
  assert.equal(forgetQuietFingerprint(file, sweepScopeKey(scopes[1])), true)
  assert.deepEqual(
    scopes.map((scope) => readQuietFingerprint(file, sweepScopeKey(scope))),
    ['0', null, '2', '3'],
  )

  // The sweep passes only its own scope's fingerprint.
  storeQuietFingerprint(file, sweepScopeKey({ selectionArgs: ['--label', 'backend'] }), FP, 'then')
  const other = quietWorld()
  sweep({ ...SKIP, selectionArgs: ['--label', 'frontend'] }, other.deps({ stateFile: file }))
  assert.equal(candidatesArgs(other)[0].includes(FP), false)
  const own = quietWorld()
  const u = sweep({ ...SKIP, selectionArgs: ['--label', 'backend'] }, own.deps({ stateFile: file }))
  assert.equal(u.unchanged, true)
})

test('the state file lives under the git common dir', () => {
  const run = (out) => (cmd, args) =>
    cmd === 'git' && args.join(' ') === 'rev-parse --git-common-dir'
      ? { ok: true, output: out, exit: 0 }
      : { ok: false, error: 'x', exit: 1, output: '' }
  assert.equal(
    sweepStatePath(run('.git'), '/repo', 1000),
    '/repo/.git/boss-verify/sweep-state.json',
  )
  assert.equal(
    sweepStatePath(run('/main/.git'), '/repo/wt', 1000),
    '/main/.git/boss-verify/sweep-state.json',
  )
  assert.equal(
    sweepStatePath(() => ({ ok: false, error: 'x' }), '/repo', 1000),
    null,
  )
  // No state file: nothing is read or written, and the tick runs in full.
  assert.equal(readQuietFingerprint(null, KEY), null)
  assert.equal(storeQuietFingerprint(null, KEY, FP, 'then'), false)
})

test('tickWasQuiet needs a complete discovery with nothing to judge, and only quiet records', () => {
  const payload = { candidates: [], skipped: quietSkips, fingerprint: FP, skippable: true }
  assert.equal(tickWasQuiet(payload, [], null), true)
  assert.equal(tickWasQuiet(payload, [{ remote: true }, { quiet: true }], null), true)
  assert.equal(tickWasQuiet({ ...payload, skippable: false }, [], null), false)
  assert.equal(tickWasQuiet({ ...payload, fingerprint: undefined }, [], null), false)
  assert.equal(tickWasQuiet({ ...payload, candidates: [candidate(1)] }, [], null), false)
  assert.equal(tickWasQuiet({ ...payload, skipped: [{ reason: 'unknown:ci' }] }, [], null), false)
  assert.equal(tickWasQuiet(payload, [{ outcome: 'skipped' }], null), false)
  assert.equal(tickWasQuiet(payload, [], 'boss unreachable'), false)
})

const sharedConfig = () => ({ daemons: [{ name: 'a' }, { name: 'b' }] })

test('shared fleet excludes remote live sessions before judging and leaves local sessions in scope', () => {
  const w = world({ candidates: [candidate(1)], fleetSessions: [{ id: 'remote', pr_url: url(1) }] })
  const result = sweep({}, w.deps({ readProjectConfig: sharedConfig }))
  assert.equal(result.records[0].line, '#1 skipped: session on another machine')
  assert.deepEqual(
    w.calls.map((c) => (c.cmd === NODE ? ['gate', ...c.args.slice(1)] : [c.cmd, ...c.args])),
    [
      [BOSS, 'ls', '--json'],
      [BOSS, 'ls', '--all-daemons', '--json'],
      ['gh', 'api', 'rate_limit'],
      ['gate', 'candidates', '--limit', '25', '--exclude-pr-urls', url(1)],
    ],
  )
  assert.equal(w.spawns.length, 0)
})

test('shared fleet routes real orphans and local sessions as before', () => {
  for (const local of [false, true]) {
    const sessions = local ? [{ id: 'local', pr_url: url(1) }] : []
    const w = world({
      sessions,
      fleetSessions: sessions,
      verdicts: { 1: { verdict: 'extensions-required' } },
    })
    const result = sweep({ dryRun: true }, w.deps({ readProjectConfig: sharedConfig }))
    assert.equal(result.records[0].route, local ? 'new-verify-chat' : 'new-session')
    assert.equal(w.posts().length, 1)
  }
})

for (const [name, fleetResult] of [
  ['failed', { ok: false, error: 'offline' }],
  ['malformed', { ok: true, payload: {} }],
  [
    'partial',
    {
      ok: true,
      payload: {
        sessions: [],
        failed_organizations: [{ organization_id: 'org', reason: 'offline' }],
      },
    },
  ],
])
  test(`unreadable shared fleet (${name}) blocks orphan claims, keeps merges, and forgets quiet state`, () => {
    const file = join(tempDir('fleet-quiet-'), 'state.json')
    const key = sweepScopeKey({ shared: true })
    storeQuietFingerprint(file, key, 'old', 'now')
    const w = world({
      candidates: [candidate(1), candidate(2)],
      fleetResult,
      verdicts: { 1: { verdict: 'extensions-required' } },
      fingerprint: 'next',
    })
    const result = sweep(
      { skipUnchanged: true },
      w.deps({ readProjectConfig: sharedConfig, stateFile: file }),
    )
    assert.match(result.fleetUnreadable, /.+/)
    assert.match(result.records[0].line, /skipped: unknown:fleet-sessions/)
    assert.equal(result.summary.skipped, 1)
    assert.equal(result.summary.merged, 1)
    assert.equal(
      w.posts().some((a) => a.includes('claim')),
      false,
    )
    assert.equal(
      w.spawns.some((a) => a.cmd === BOSS),
      false,
    )
    assert.equal(w.verbs()[0].includes('--quiet-fingerprint'), false)
    assert.equal(readQuietFingerprint(file, key), null)
  })

test('unreadable local list or project config prevents spawning but not local routing', () => {
  for (const readProjectConfig of [
    sharedConfig,
    () => {
      throw new Error('invalid JSON')
    },
  ]) {
    const w = world({
      lsResult: { ok: false, error: 'offline' },
      verdicts: { 1: { verdict: 'extensions-required' } },
    })
    const result = sweep({}, w.deps({ readProjectConfig }))
    assert.match(result.records[0].line, /skipped: unknown:fleet-sessions/)
    assert.equal(w.posts().length, 0)
    assert.equal(w.spawns.length, 0)
  }
  const sessions = [{ id: 'local', pr_url: url(1) }]
  const w = world({
    sessions,
    fleetResult: { ok: false, error: 'offline' },
    verdicts: { 1: { verdict: 'extensions-required' } },
  })
  assert.equal(
    sweep({ dryRun: true }, w.deps({ readProjectConfig: sharedConfig })).records[0].route,
    'new-verify-chat',
  )
})

test('unshared and local-only sweeps never read the fleet or pass exclusions', () => {
  for (const config of [null, {}, { daemons: [{}] }]) {
    const w = world()
    sweep({ dryRun: true }, w.deps({ readProjectConfig: () => config }))
    assert.equal(
      w.calls.some((c) => c.args.includes('--all-daemons')),
      false,
    )
    assert.equal(w.verbs()[0].includes('--exclude-pr-urls'), false)
  }
  const w = world({ sessions: [{ id: 'local', pr_url: url(1) }] })
  sweep({ localSessionsOnly: true, dryRun: true }, w.deps({ readProjectConfig: sharedConfig }))
  assert.equal(
    w.calls.some((c) => c.args.includes('--all-daemons')),
    false,
  )
})

test('shared config is resolved from the git root, including malformed present JSON', () => {
  for (const content of [JSON.stringify(sharedConfig()), '{']) {
    const root = tempDir('fleet-config-')
    writeFileSync(join(root, '.boss-project.json'), content)
    const w = world({ verdicts: { 1: { verdict: 'extensions-required' } } })
    writeFileSync(join(root, '.git'), 'gitdir: /fixture/worktree')
    const cwd = join(root, 'nested')
    mkdirSync(cwd)
    const result = sweep({ dryRun: true }, w.deps({ cwd, readProjectConfig: undefined }))
    if (content === '{') assert.match(result.fleetUnreadable, /project config/)
    else assert.ok(w.calls.some((c) => c.args.includes('--all-daemons')))
  }
})

test('sweep CLI serializes the unreadable fleet diagnostic on an idle tick', (t) => {
  const root = tempDir('fleet-cli-')
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  assert.equal(spawnSync('git', ['init', '-q', root]).status, 0)
  writeFileSync(join(root, '.boss-project.json'), JSON.stringify(sharedConfig()))
  writeFileSync(
    join(root, 'gh'),
    `#!/bin/sh
printf '%s\\n' '{"resources":{"core":{"limit":100,"remaining":0}}}'
`,
  )
  chmodSync(join(root, 'gh'), 0o755)
  const out = spawnSync(
    process.execPath,
    [new URL('./verify-route.mjs', import.meta.url).pathname, 'sweep', '--dry-run'],
    {
      cwd: root,
      encoding: 'utf8',
      env: { ...process.env, BOSS_BIN: '/usr/bin/false', PATH: `${root}:${process.env.PATH}` },
    },
  )
  assert.equal(out.status, 0, out.stderr)
  const rows = out.stdout
    .trim()
    .split('\n')
    .map((line) => JSON.parse(line))
  assert.match(rows[0].fleetUnreadable, /boss ls:/)
  assert.equal(rows.at(-1).dryRun, true)
})

test('event sweeps discover and judge only the named PR without a quiet fingerprint', () => {
  const file = stateIn(stateDir())
  storeQuietFingerprint(file, KEY, FP, new Date(T0).toISOString())
  const w = world({
    filterPrUrls: true,
    candidates: [candidate(1), candidate(2)],
    verdicts: { 2: { verdict: 'wait', reason: 'ci-pending' } },
  })
  const result = sweep({ ...SKIP, eventPrUrl: url(2) }, w.deps({ stateFile: file }))
  const args = candidatesArgs(w)[0]
  assert.equal(args[args.indexOf('--pr-urls') + 1], url(2))
  assert.equal(args.includes('--quiet-fingerprint'), false)
  assert.deepEqual(
    w.verbs().map((v) => v[0]),
    ['candidates', 'judge'],
  )
  assert.equal(w.verbs()[1][2], '2')
  assert.deepEqual(
    result.records.map((r) => r.pr),
    [2],
  )
  assert.equal(w.calls.length, 3, 'quota, discovery, one judge')
  assert.equal(storedFp(file), null, 'a judged event invalidates the cron quiet entry')
  assert.deepEqual(Object.keys(JSON.parse(readFileSync(file, 'utf8')).entries), [])
})

test('quiet events leave the cron entry untouched and never persist their own fingerprint', () => {
  const file = stateIn(stateDir())
  storeQuietFingerprint(file, KEY, FP, new Date(T0).toISOString())
  const before = readFileSync(file, 'utf8')
  const w = quietWorld()
  sweep({ eventPrUrl: url(2) }, w.deps({ stateFile: file }))
  assert.equal(readFileSync(file, 'utf8'), before)
  assert.equal(candidatesArgs(w)[0].includes('--quiet-fingerprint'), false)
  assert.notEqual(sweepScopeKey({ eventPrUrl: url(2) }), KEY)
})

test('an unknown event skip invalidates only its matching cron selection scope', () => {
  const file = stateIn(stateDir())
  const scope = { selectionArgs: ['--label', 'backend'], localSessionsOnly: true }
  storeQuietFingerprint(file, KEY, FP, new Date(T0).toISOString())
  storeQuietFingerprint(file, sweepScopeKey(scope), FP, new Date(T0).toISOString())
  const w = quietWorld({
    sessions: [{ id: 's2', pr_url: url(2) }],
    skipped: [{ pr: 2, reason: 'unknown:status' }],
  })
  sweep({ ...scope, eventPrUrl: url(2) }, w.deps({ stateFile: file }))
  assert.equal(storedFp(file, sweepScopeKey(scope)), null)
  assert.equal(storedFp(file), FP)
})

test('event local-session intersection idles without any GitHub read when the PR is not local', () => {
  for (const sessions of [[], [{ id: 's1', pr_url: url(1) }]]) {
    const w = world({ sessions })
    const result = sweep({ eventPrUrl: url(2), localSessionsOnly: true }, w.deps())
    assert.equal(result.idle, 'event PR has no session on this machine')
    assert.deepEqual(
      w.calls.map((c) => [c.cmd, ...c.args]),
      [[BOSS, 'ls', '--json']],
    )
  }
  const w = world({
    filterPrUrls: true,
    sessions: [
      { id: 's2', pr_url: url(2) },
      { id: 's1', pr_url: url(1) },
    ],
    candidates: [candidate(1), candidate(2)],
  })
  sweep({ eventPrUrl: url(2), localSessionsOnly: true, dryRun: true }, w.deps())
  const args = candidatesArgs(w)[0]
  assert.equal(args[args.indexOf('--pr-urls') + 1], url(2))
  assert.deepEqual(
    w
      .verbs()
      .filter((v) => v[0] === 'judge')
      .map((v) => v[2]),
    ['2'],
  )
})

test('safety net skips a fresh quiet scope before quota, session or discovery calls', () => {
  for (const localSessionsOnly of [false, true]) {
    const scope = { localSessionsOnly }
    const file = stateIn(stateDir())
    storeQuietFingerprint(file, sweepScopeKey(scope), FP, new Date(T0).toISOString())
    const w = world({ clock: T0 + 1000 })
    const before = readFileSync(file, 'utf8')
    const result = sweep({ ...SKIP, ...scope, safetyNetMs: 60_000 }, w.deps({ stateFile: file }))
    assert.equal(
      result.idle,
      `webhooks deliver; next safety-net sweep after ${new Date(T0 + 60_000).toISOString()}`,
    )
    assert.deepEqual(w.calls, [])
    assert.deepEqual(w.spawns, [])
    assert.equal(readFileSync(file, 'utf8'), before)
  }
})

test('safety net uses checkedAt and unchanged ticks refresh it', () => {
  const file = stateIn(stateDir())
  const first = quietWorld()
  sweep(SKIP, first.deps({ stateFile: file }))
  let entry = JSON.parse(readFileSync(file, 'utf8')).entries[KEY]
  assert.equal(entry.checkedAt, T0)
  const later = quietWorld({ clock: T0 + 60_000 })
  const result = sweep({ ...SKIP, safetyNetMs: 60_000 }, later.deps({ stateFile: file }))
  assert.equal(result.unchanged, true)
  entry = JSON.parse(readFileSync(file, 'utf8')).entries[KEY]
  assert.equal(entry.checkedAt, T0 + 60_000)
  const next = quietWorld({ clock: T0 + 60_001 })
  assert.ok(sweep({ ...SKIP, safetyNetMs: 60_000 }, next.deps({ stateFile: file })).idle)
  assert.deepEqual(next.calls, [])
})

test('legacy quiet entries use at, stale or disabled windows run the normal unchanged path', () => {
  for (const [age, safetyNetMs, skip] of [
    [1000, 60_000, true],
    [60_000, 60_000, false],
    [1000, 0, false],
    [1000, undefined, false],
  ]) {
    const file = stateIn(stateDir())
    storeQuietFingerprint(file, KEY, FP, new Date(T0).toISOString())
    const state = JSON.parse(readFileSync(file, 'utf8'))
    delete state.entries[KEY].checkedAt
    writeFileSync(file, JSON.stringify(state))
    const w = quietWorld({ clock: T0 + age })
    const result = sweep({ ...SKIP, safetyNetMs }, w.deps({ stateFile: file }))
    if (skip) {
      assert.ok(result.idle?.startsWith('webhooks deliver;'))
      assert.deepEqual(w.calls, [])
    } else {
      assert.equal(result.unchanged, true)
      assert.deepEqual(
        w.calls.map((c) => c.cmd),
        ['gh', NODE],
      )
    }
  }
})

test('event scope bypasses the safety net and quota still precedes discovery', () => {
  const file = stateIn(stateDir())
  storeQuietFingerprint(file, KEY, FP, new Date(T0).toISOString())
  const w = quietWorld()
  sweep({ ...SKIP, safetyNetMs: 60_000, eventPrUrl: url(2) }, w.deps({ stateFile: file }))
  assert.deepEqual(
    w.calls.map((c) => c.cmd),
    ['gh', NODE],
  )
})

test('event results discard unrelated remote skips, merge retries and candidates', () => {
  const w = world({
    candidates: [candidate(1), candidate(2)],
    mergeRetries: [candidate(3)],
    skipped: [{ pr: 4, reason: 'remote-session' }],
    verdicts: { 2: { verdict: 'wait', reason: 'ci-pending' } },
  })
  const result = sweep({ eventPrUrl: url(2) }, w.deps())
  assert.deepEqual(
    result.records.map((r) => r.pr),
    [2],
  )
  assert.deepEqual(
    w.verbs().map((v) => [v[0], v[2]]),
    [
      ['candidates', '25'],
      ['judge', '2'],
    ],
  )
  assert.deepEqual(w.spawns, [])
})

test('shared event scope does not combine include and exclude flags and leaves remote owners alone', () => {
  const w = world({
    candidates: [candidate(2)],
    sessions: [],
    fleetSessions: [
      { id: 'remote', pr_url: url(2) },
      { id: 'other', pr_url: url(3) },
    ],
  })
  const result = sweep({ eventPrUrl: url(2) }, w.deps({ readProjectConfig: sharedConfig }))
  const args = candidatesArgs(w)[0]
  assert.equal(args[args.indexOf('--pr-urls') + 1], url(2))
  assert.equal(args.includes('--exclude-pr-urls'), false)
  assert.deepEqual(lines(result), ['#2 skipped: session on another machine'])
  assert.deepEqual(
    w.verbs().map((v) => v[0]),
    ['candidates'],
  )
  assert.deepEqual(w.spawns, [])
})
