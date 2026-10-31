import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { writeFileSync } from 'node:fs'
import { sweepScopeKey } from '../verify/verify-route.mjs'
import { spawnSync } from 'node:child_process'

import { basename, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { evaluateBossVerifyGate } from './boss-verify.mjs'

const BOSS = '/fake/bin/boss'
const NODE = 'node'
const sha = (n) => n.toString(16).padStart(40, '0')
const ENV = { PATH: '/usr/bin', LINEAR_API_KEY: 'lin_test', REPO_DIR: '/repo' }

// A fake `run` seam playing verify-gate's verbs (`node verify-gate.mjs <verb>`) and `boss`. A merge
// is a detached spawn whose result lands in its log `mergeDelay` ms of fake clock later.
function world({
  candidates = [1],
  verdict = 'pass',
  reason = 'verified',
  boss,
  candidatesFail,
  mergeDelay = 0,
  mergeRetries,
  sessions = [],
  lsFail,
  fingerprint,
  admission,
} = {}) {
  const w = { calls: [], spawns: [], merges: {}, clock: 0 }
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  w.run = (cmd, args) => {
    w.calls.push({ cmd, args: [...args] })
    if (cmd === NODE) {
      const verb = args[1]
      const pr = Number(args[args.indexOf('--pr') + 1])
      if (verb === 'candidates') {
        if (candidatesFail)
          return { ok: false, error: 'exit-1', exit: 1, output: 'gh pr list failed (exit-1)' }
        const quiet = args.indexOf('--quiet-fingerprint')
        if (fingerprint && quiet !== -1 && args[quiet + 1] === fingerprint)
          return ok({ candidates: [], skipped: [], mergeRetries: [], unchanged: true, fingerprint })
        return ok({
          ...(fingerprint ? { fingerprint, skippable: true } : {}),
          candidates: candidates.map((n) => ({
            ticket: `BOS-${n}`,
            pr: n,
            url: `https://github.com/o/r/pull/${n}`,
            headSha: sha(n),
            repo: 'o/r',
            claim: 'none',
          })),
          skipped: [],
          ...(admission ? { admission } : {}),
          ...(mergeRetries
            ? {
                mergeRetries: mergeRetries.map((n) => ({
                  ticket: `BOS-${n}`,
                  pr: n,
                  url: `https://github.com/o/r/pull/${n}`,
                  headSha: sha(n),
                  repo: 'o/r',
                })),
              }
            : {}),
        })
      }
      if (verb === 'judge') return ok({ verdict, reason, headSha: sha(pr) })
      if (verb === 'post') return ok({ posted: true, won: true, token: 'tok' })
      if (verb === 'observe-merge') return ok({ merged: false, state: 'OPEN' })
    }
    if (cmd === BOSS && args[0] === 'ls')
      return lsFail ? { ok: false, error: lsFail, exit: 1, output: '' } : ok({ sessions })
    if (cmd === BOSS && args[0] === 'callback' && args[1] === 'list') return ok([])
    if (cmd === BOSS && args[0] === 'chats')
      return ok({ chats: [{ agent_session_id: 'chat-v', title: 'verify' }] })
    if (cmd === BOSS && args[0] === 'chat' && args[1] === 'send')
      return ok({ delivery: { delivered: true, delivery_state: 'DELIVERY_STATE_SUBMITTED' } })
    return { ok: false, error: 'unexpected', exit: 1, output: '' }
  }
  w.deps = {
    loadConfig: () => ({ adapters: { tracker: 'linear' } }),
    run: w.run,
    nodeBin: NODE,
    readProjectConfig: () => null,
    now: () => w.clock,
    resolveBoss: () => boss ?? { ok: true, path: BOSS, reason: '' },
    spawn: (cmd, a, options) => {
      w.spawns.push({ cmd, args: a, options })
      if (cmd === NODE && a[1] === 'merge')
        w.merges[Number(a[a.indexOf('--pr') + 1])] = w.clock + mergeDelay
      return { pid: 1, unref() {}, on() {} }
    },
    readFile: (path) => {
      const readyAt = w.merges[Number(basename(path).match(/^boss-verify-merge-(\d+)-/)?.[1])]
      if (readyAt === undefined || w.clock < readyAt) return ''
      return `${JSON.stringify({ merged: true, mergeSha: sha(900) })}\n`
    },
    sleep: (ms) => {
      w.clock += ms
    },
    cwd: '/repo',
    logDir: tempDir('boss-verify-gate-'),
  }
  w.writes = () => [
    ...w.calls.filter(
      (c) =>
        c.cmd === NODE && ['post', 'merge'].includes(c.args[1]) && !c.args.includes('--dry-run'),
    ),
    ...w.spawns.filter((s) => s.cmd === NODE && s.args[1] === 'merge'),
  ]
  return w
}

const gate = (w, argv = [], env = ENV) => evaluateBossVerifyGate({ argv, env, deps: w.deps })

test('gate-only mode: post then merge, a readable line per PR plus the summary, exit 1', () => {
  const w = world()
  const { exitCode, lines } = gate(w)
  assert.deepEqual(
    w.writes().map((c) => c.args[1]),
    ['post', 'merge'],
  )
  assert.deepEqual(lines, [
    `#1 merged ${sha(900)}`,
    'boss-verify gate: merged=1 parked=0 dispatched=0 skipped=0 deferred=0',
  ])
  assert.equal(exitCode, 1)
})

test('a merge slower than the per-call timeout that succeeds reports merged, not ETIMEDOUT', () => {
  const w = world({ mergeDelay: 20_000 })
  const { exitCode, lines } = gate(w)
  assert.deepEqual(lines, [
    `#1 merged ${sha(900)}`,
    'boss-verify gate: merged=1 parked=0 dispatched=0 skipped=0 deferred=0',
  ])
  assert.equal(exitCode, 1)
})

test('a merge still in flight at the deadline is dispatched with its log, and exits 1', () => {
  const w = world({ mergeDelay: Infinity })
  const { exitCode, lines } = gate(w)
  assert.match(lines[0], /^#1 dispatched: merge in flight \(log .*boss-verify-merge-1-.*\.log\)$/)
  assert.equal(lines[1], 'boss-verify gate: merged=0 parked=0 dispatched=1 skipped=0 deferred=0')
  assert.equal(exitCode, 1)
})

test('a human verdict posts and exits 1', () => {
  const w = world({ verdict: 'human', reason: 'partial-build' })
  const { exitCode, lines } = gate(w)
  assert.equal(lines[0], '#1 parked: needs human (partial-build)')
  assert.equal(exitCode, 1)
})

test('an unchanged tick after a quiet one says so, judges nothing and exits 1', () => {
  const stateFile = join(tempDir('boss-verify-state-'), 'sweep-state.json')
  const quiet = world({ candidates: [], fingerprint: 'a'.repeat(64) })
  const first = evaluateBossVerifyGate({ argv: [], env: ENV, deps: { ...quiet.deps, stateFile } })
  assert.deepEqual(first.lines, [
    'boss-verify gate: merged=0 parked=0 dispatched=0 skipped=0 deferred=0',
  ])
  const w = world({ candidates: [1], fingerprint: 'a'.repeat(64) })
  const { exitCode, lines } = evaluateBossVerifyGate({
    argv: [],
    env: ENV,
    deps: { ...w.deps, stateFile },
  })
  assert.deepEqual(lines, [
    'boss-verify gate: unchanged since the last quiet tick',
    'boss-verify gate: merged=0 parked=0 dispatched=0 skipped=0 deferred=0',
  ])
  assert.equal(exitCode, 1)
  assert.deepEqual(
    w.calls.filter((c) => c.cmd === NODE).map((c) => c.args[1]),
    ['candidates'],
  )
  assert.equal(w.writes().length, 0)
})

test('nothing to do exits 1 with only the summary line', () => {
  const { exitCode, lines } = gate(world({ candidates: [] }))
  assert.deepEqual(lines, ['boss-verify gate: merged=0 parked=0 dispatched=0 skipped=0 deferred=0'])
  assert.equal(exitCode, 1)
})

test('an idle run says why before the summary and exits 1', () => {
  const w = world({ sessions: [] })
  const { exitCode, lines } = gate(w, ['--local-sessions-only'])
  assert.deepEqual(lines, [
    'boss-verify gate: idle: no session on this machine has a PR',
    'boss-verify gate: merged=0 parked=0 dispatched=0 skipped=0 deferred=0',
  ])
  assert.equal(exitCode, 1)
  // It reads nothing from GitHub; the one other call finds the unchanged-tick state to drop its entry.
  assert.deepEqual(
    w.calls.map((c) => [c.cmd, ...c.args.slice(0, 2)]),
    [
      [BOSS, 'ls', '--json'],
      ['git', 'rev-parse', '--git-common-dir'],
    ],
  )
})

test('undispatchable with a candidate that needs boss exits 0; with no candidates exits 1', () => {
  const boss = { ok: false, path: null, reason: 'BOSS_BIN is unset' }
  const w = world({ boss, verdict: 'extensions-required', reason: 'extensions-installed' })
  const r = gate(w)
  assert.equal(r.exitCode, 0)
  assert.ok(r.lines.some((l) => l.startsWith('undispatchable: BOSS_BIN is unset')))
  assert.equal(w.writes().length, 0)
  assert.equal(w.spawns.length, 0)

  const none = gate(world({ boss, candidates: [] }))
  assert.equal(none.exitCode, 1)
  assert.equal(
    none.lines.some((l) => l.startsWith('undispatchable')),
    false,
  )
})

test('unknown evidence fails closed: exit 1, nothing written or dispatched', () => {
  const w = world({ candidatesFail: true })
  const { exitCode, lines } = gate(w)
  assert.equal(exitCode, 1)
  assert.match(lines[0], /^boss-verify gate: candidates failed \(exit-1\): gh pr list failed/)
  assert.equal(w.writes().length, 0)
  assert.equal(w.spawns.length, 0)
})

test('--dry-run writes and dispatches nothing, prints the plan, and exits 1 even when undispatchable', () => {
  const w = world({ verdict: 'extensions-required', reason: 'extensions-installed' })
  const r = gate(w, ['--dry-run'])
  assert.equal(w.writes().length, 0)
  assert.equal(w.spawns.length, 0)
  assert.deepEqual(r.lines, [
    '#1 dispatched: new-session (dry-run)',
    'boss-verify gate: merged=0 parked=0 dispatched=1 skipped=0 deferred=0 (dry-run)',
  ])
  assert.equal(r.exitCode, 1)

  const stuck = gate(world({ boss: { ok: false, path: null, reason: 'x' } }), ['--dry-run'])
  assert.equal(stuck.exitCode, 1)
})

test('the gate refuses a positional or unknown argument before any call', () => {
  for (const argv of [['42'], ['--labels', 'x'], ['--batch'], ['--local-sessions-only=1']]) {
    const w = world()
    const { exitCode, lines } = gate(w, argv)
    assert.equal(exitCode, 1, JSON.stringify(argv))
    assert.match(lines[0], /^boss-verify gate: /)
    assert.equal(w.calls.length, 0)
  }
})

test('selection flags reach the candidates read verbatim; discovery reads past the batch', () => {
  const w = world()
  gate(w, ['--batch', '2', '--exclude-label', 'infra', '--assignee=me'])
  const cand = w.calls.find((c) => c.args[1] === 'candidates')
  // `--batch` bounds the actions, not discovery: the read always looks past the batch.
  assert.deepEqual(cand.args.slice(2), [
    '--limit',
    '25',
    '--exclude-label',
    'infra',
    '--assignee=me',
  ])
})

const row = (n) => ({ id: `s${n}`, pr_url: `https://github.com/o/r/pull/${n}` })

test('--local-sessions-only: only PRs with a session here are handled; the rest are named and untouched', () => {
  const w = world({
    candidates: [1, 2, 3],
    mergeRetries: [7, 8],
    verdict: 'extensions-required',
    reason: 'extensions-installed',
    sessions: [row(2), row(8)],
  })
  const { exitCode, lines } = gate(w, ['--local-sessions-only'])
  assert.deepEqual(lines, [
    '#7 skipped: session on another machine',
    `#8 merged ${sha(900)} (merge retry)`,
    '#1 skipped: session on another machine',
    '#2 dispatched: existing-verify-chat',
    '#3 skipped: session on another machine',
    'boss-verify gate: merged=1 parked=0 dispatched=1 skipped=3 deferred=0',
  ])
  assert.equal(exitCode, 1)
  // Only PR 2 is judged and claimed, only PR 8 is merged, and nothing spawns a session.
  const verbs = w.calls.filter((c) => c.cmd === NODE).map((c) => [c.args[1], c.args[3]])
  assert.deepEqual(verbs, [
    ['candidates', '25'],
    ['judge', '2'],
    ['post', '2'],
  ])
  assert.deepEqual(
    w.spawns.map((s) => [s.cmd, s.args[1], s.args[3]]),
    [[NODE, 'merge', '8']],
  )
})

test('--local-sessions-only with boss or its session list unreadable exits 1 and does nothing', () => {
  for (const opts of [
    { boss: { ok: false, path: null, reason: 'BOSS_BIN is unset' } },
    { lsFail: 'UNAVAILABLE' },
  ]) {
    const w = world({ ...opts, verdict: 'extensions-required', reason: 'extensions-installed' })
    const r = gate(w, ['--local-sessions-only'])
    assert.equal(r.exitCode, 1, JSON.stringify(opts))
    assert.equal(r.lines.length, 1)
    assert.match(
      r.lines[0],
      /^boss-verify gate: --local-sessions-only: this machine's session list is unreadable/,
    )
    assert.equal(
      w.calls.some((c) => c.cmd === NODE),
      false,
    )
    assert.equal(w.writes().length, 0)
    assert.equal(w.spawns.length, 0)
    // Without the flag the same world takes the exit-0 hand-off.
    if (opts.boss)
      assert.equal(gate(world({ ...opts, verdict: 'extensions-required' })).exitCode, 0)
  }
})

// Process-level pin: the real entry point, spawned with no LINEAR_API_KEY, refuses before any read.
test('spawned without LINEAR_API_KEY the gate exits 1 naming the missing key', () => {
  const env = { ...process.env }
  delete env.LINEAR_API_KEY
  const result = spawnSync(
    process.execPath,
    [fileURLToPath(new URL('./boss-verify.mjs', import.meta.url))],
    {
      env,
      encoding: 'utf8',
      timeout: 30_000,
    },
  )
  assert.equal(result.status, 1)
  assert.match(result.stdout, /boss-verify gate: LINEAR_API_KEY is not set/)
})

// Process-level pin for the local-sessions entry point: with no `boss` resolvable it fails closed
// before any tracker or GitHub read, which only the --local-sessions-only path does.
test('the local-sessions entry point runs the gate with --local-sessions-only and fails closed', () => {
  const dir = tempDir('boss-verify-local-')
  const result = spawnSync(
    process.execPath,
    [fileURLToPath(new URL('./boss-verify-local-sessions.mjs', import.meta.url))],
    {
      cwd: dir,
      env: { PATH: join(dir, 'empty-path'), HOME: dir, LINEAR_API_KEY: 'lin_test' },
      encoding: 'utf8',
      timeout: 30_000,
    },
  )
  assert.equal(result.status, 1, result.stderr)
  assert.match(
    result.stdout,
    /^boss-verify gate: --local-sessions-only: this machine's session list is unreadable/,
  )
})

test('unreadable shared fleet reports its warning and exits without handing off', () => {
  const w = world({ verdict: 'extensions-required' })
  w.deps.readProjectConfig = () => {
    throw new Error('invalid project JSON')
  }
  const result = gate(w)
  assert.equal(result.exitCode, 1)
  assert.ok(
    result.lines.some(
      (line) =>
        line.includes('cross-daemon sessions unreadable') &&
        line.includes('no new sessions this tick'),
    ),
  )
  assert.ok(result.lines.at(-1).includes('skipped=1'))
  assert.deepEqual(w.writes(), [])
})

test('shared unreadable local sessions do not hand a mechanical verdict to a blind session', () => {
  const w = world({ lsFail: 'offline' })
  w.deps.readProjectConfig = () => ({ daemons: [{}, {}] })
  const result = gate(w)
  assert.equal(result.exitCode, 1)
  assert.ok(result.lines.some((line) => line.includes('cross-daemon sessions unreadable')))
  assert.equal(
    result.lines.some((line) => line.includes('exit 0 hands')),
    false,
  )
})

test('event env scopes candidates and marks the event summary', () => {
  const w = world({ verdict: 'human' })
  const result = gate(w, ['--dry-run', '--exclude-label', 'infra'], {
    ...ENV,
    BOSS_VERIFY_EVENT_PR_URL: '  https://github.com/o/r/pull/1  ',
  })
  const args = w.calls.find((c) => c.args[1] === 'candidates').args
  assert.equal(args[args.indexOf('--pr-urls') + 1], 'https://github.com/o/r/pull/1')
  assert.equal(args.includes('--quiet-fingerprint'), false)
  assert.ok(args.includes('--exclude-label'))
  assert.match(result.lines.at(-1), /^boss-verify gate \(event #1\):/)
  assert.equal(result.exitCode, 1)
})

test('malformed event scope fails closed without subprocess calls; whitespace is unset', () => {
  for (const value of [
    'not-a-url',
    'https://github.com/o/r/issues/1',
    'https://github.com/o/r/pull/0',
    'https://github.com/o/r/pull/1/extra',
    'https://example.com/o/r/pull/1',
  ]) {
    const w = world()
    const result = gate(w, [], { ...ENV, BOSS_VERIFY_EVENT_PR_URL: value })
    assert.deepEqual(result, {
      exitCode: 1,
      lines: ['boss-verify gate: BOSS_VERIFY_EVENT_PR_URL is not a pull request URL'],
    })
    assert.deepEqual(w.calls, [])
  }
  const w = world({ candidates: [] })
  assert.equal(
    gate(w, [], { ...ENV, BOSS_VERIFY_EVENT_PR_URL: '  ' }).lines.at(-1),
    'boss-verify gate: merged=0 parked=0 dispatched=0 skipped=0 deferred=0',
  )
})

test('healthy env enables safety net by default, only 1 enables it and zero minutes disables it', () => {
  for (const [healthy, argv, skips] of [
    ['1', [], true],
    ['1', ['--safety-net-minutes', '0'], false],
    ['0', [], false],
    [undefined, [], false],
    ['true', [], false],
    ['1', ['--safety-net-minutes=1'], true],
  ]) {
    const file = join(tempDir('verify-gate-safety-'), 'state.json')
    writeFileSync(
      file,
      JSON.stringify({
        version: 1,
        entries: {
          [sweepScopeKey({})]: {
            fingerprint: 'a'.repeat(64),
            at: new Date(0).toISOString(),
            checkedAt: 0,
          },
        },
      }),
    )
    const w = world({ candidates: [], fingerprint: 'a'.repeat(64) })
    w.clock = 1000
    const result = evaluateBossVerifyGate({
      argv,
      env: { ...ENV, BOSS_WEBHOOKS_HEALTHY: healthy },
      deps: { ...w.deps, stateFile: file },
    })
    assert.equal(result.exitCode, 1)
    if (skips) {
      assert.match(
        result.lines[0],
        /^boss-verify gate: idle: webhooks deliver; next safety-net sweep after /,
      )
      assert.deepEqual(w.calls, [])
    } else {
      assert.ok(result.lines[0].includes('unchanged'))
      assert.ok(w.calls.some((c) => c.args[1] === 'candidates'))
    }
  }
})

test('safety-net minutes refuses negative, missing and non-integer values without reads', () => {
  for (const argv of [
    ['--safety-net-minutes', '-1'],
    ['--safety-net-minutes', 'x'],
    ['--safety-net-minutes'],
    ['--safety-net-minutes=1.5'],
    ['--safety-net-minutes='],
  ]) {
    const w = world()
    const result = gate(w, argv)
    assert.equal(result.exitCode, 1)
    assert.match(result.lines[0], /boss-verify gate: .*safety-net-minutes/)
    assert.deepEqual(w.calls, [])
  }
})

for (const tracker of ['linear', undefined]) {
  for (const key of [undefined, '  ']) {
    test(`missing Linear credential refuses before reads (tracker=${tracker}, key=${key})`, () => {
      const w = world()
      w.deps.loadConfig = () => ({ adapters: { tracker } })
      assert.deepEqual(gate(w, [], { ...ENV, LINEAR_API_KEY: key }), {
        exitCode: 1,
        lines: ['boss-verify gate: LINEAR_API_KEY is not set'],
      })
      assert.equal(w.calls.length, 0)
    })
  }
}

test('configured GitHub reaches discovery without a Linear key', () => {
  const w = world()
  w.deps.loadConfig = () => ({ adapters: { tracker: 'github' } })
  const result = gate(w, [], { PATH: ENV.PATH })
  assert.equal(result.exitCode, 1)
  assert.ok(w.calls.some((c) => c.args[1] === 'candidates'))
  assert.match(result.lines[0], /merged/)
})

test('configured GitHub discovery failure fails closed without a Linear key', () => {
  const w = world({ candidatesFail: true })
  w.deps.loadConfig = () => ({ adapters: { tracker: 'github' } })
  const result = gate(w, [], { PATH: ENV.PATH })
  assert.equal(result.exitCode, 1)
  assert.match(result.lines[0], /^boss-verify gate: .*gh pr list failed/)
})

test('config failure refuses before reads and receives the gate cwd', () => {
  const w = world()
  w.deps.loadConfig = ({ cwd }) => {
    assert.equal(cwd, '/repo')
    throw new Error('config unreadable')
  }
  assert.deepEqual(gate(w), { exitCode: 1, lines: ['boss-verify gate: config unreadable'] })
  assert.equal(w.calls.length, 0)
})

for (const tracker of ['jira', '', 'constructor']) {
  test(`unregistered tracker ${JSON.stringify(tracker)} refuses before reads`, () => {
    const w = world()
    assert.deepEqual(gate(w, [], { ...ENV, TRACKER: tracker }), {
      exitCode: 1,
      lines: [`boss-verify gate: unknown tracker: ${tracker}`],
    })
    assert.equal(w.calls.length, 0)
  })
}

test('event runs leave failed, deferred and undispatchable heads eligible', () => {
  const env = { ...ENV, BOSS_VERIFY_EVENT_PR_URL: 'https://github.com/o/r/pull/1' }
  const failed = world()
  const run = failed.deps.run
  failed.deps.run = (cmd, args) =>
    cmd === NODE && args[1] === 'judge'
      ? { ok: false, error: 'ETIMEDOUT', exit: null, output: '' }
      : run(cmd, args)
  const timeout = gate(failed, [], env)
  assert.match(timeout.lines[0], /judge failed \(ETIMEDOUT\)/)
  assert.equal(timeout.exitCode, 69)
  for (const opts of [
    { candidatesFail: true },
    { verdict: 'wait', reason: 'ci-pending' },
    { boss: { ok: false, path: null, reason: 'unavailable' }, verdict: 'extensions-required' },
  ])
    assert.equal(gate(world(opts), [], env).exitCode, 69, JSON.stringify(opts))
  assert.equal(gate(world(), ['--budget-ms', '1'], env).exitCode, 69)
  assert.equal(gate(world({ verdict: 'human' }), [], env).exitCode, 1)
  assert.equal(gate(world(), [], { ...env, LINEAR_API_KEY: '' }).exitCode, 69)
  const config = world()
  config.deps.loadConfig = () => {
    throw new Error('config unreadable')
  }
  assert.equal(gate(config, [], env).exitCode, 69)
})

test('event discovery without admission evidence remains retryable; a known exclusion settles', () => {
  const env = { ...ENV, BOSS_VERIFY_EVENT_PR_URL: 'https://github.com/o/r/pull/1' }
  const unknown = world({ candidates: [] })
  const pending = gate(unknown, [], env)
  assert.equal(pending.exitCode, 69)
  assert.match(pending.lines[0], /unknown:admission/)
  assert.equal(unknown.writes().length, 0)

  const excluded = world({
    candidates: [],
    admission: [{ pr: 1, reason: 'not-admitted:bot' }],
  })
  const settled = gate(excluded, [], env)
  assert.equal(settled.exitCode, 1)
  assert.match(settled.lines[0], /not-admitted:bot/)
  assert.equal(excluded.writes().length, 0)
})
