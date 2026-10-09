import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { evaluateBossVerifyGate } from './boss-verify.mjs'

const BOSS = '/fake/bin/boss'
const NODE = 'node'
const sha = (n) => n.toString(16).padStart(40, '0')
const ENV = { PATH: '/usr/bin', LINEAR_API_KEY: 'lin_test', REPO_DIR: '/repo' }

// A fake `run` seam playing verify-gate's verbs (`node verify-gate.mjs <verb>`) and `boss`.
function world({
  candidates = [1],
  verdict = 'pass',
  reason = 'verified',
  boss,
  candidatesFail,
} = {}) {
  const w = { calls: [], spawns: [] }
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  w.run = (cmd, args) => {
    w.calls.push({ cmd, args: [...args] })
    if (cmd === NODE) {
      const verb = args[1]
      const pr = Number(args[args.indexOf('--pr') + 1])
      if (verb === 'candidates') {
        if (candidatesFail)
          return { ok: false, error: 'exit-1', exit: 1, output: 'gh pr list failed (exit-1)' }
        return ok({
          candidates: candidates.map((n) => ({
            ticket: `BOS-${n}`,
            pr: n,
            url: `https://github.com/o/r/pull/${n}`,
            headSha: sha(n),
            repo: 'o/r',
            claim: 'none',
          })),
          skipped: [],
        })
      }
      if (verb === 'judge') return ok({ verdict, reason, headSha: sha(pr) })
      if (verb === 'post') return ok({ posted: true, won: true, token: 'tok' })
      if (verb === 'merge') return ok({ merged: true, mergeSha: sha(900) })
    }
    if (cmd === BOSS && args[0] === 'ls') return ok({ sessions: [] })
    return { ok: false, error: 'unexpected', exit: 1, output: '' }
  }
  w.deps = {
    run: w.run,
    nodeBin: NODE,
    now: () => 0,
    resolveBoss: () => boss ?? { ok: true, path: BOSS, reason: '' },
    spawn: (cmd, a, options) => {
      w.spawns.push({ cmd, args: a, options })
      return { pid: 1, unref() {}, on() {} }
    },
    cwd: '/repo',
    logDir: mkdtempSync(join(tmpdir(), 'boss-verify-gate-')),
  }
  w.writes = () =>
    w.calls.filter(
      (c) =>
        c.cmd === NODE && ['post', 'merge'].includes(c.args[1]) && !c.args.includes('--dry-run'),
    )
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

test('a human verdict posts and exits 1', () => {
  const w = world({ verdict: 'human', reason: 'ledger-open' })
  const { exitCode, lines } = gate(w)
  assert.equal(lines[0], '#1 parked: needs human (ledger-open)')
  assert.equal(exitCode, 1)
})

test('nothing to do exits 1 with only the summary line', () => {
  const { exitCode, lines } = gate(world({ candidates: [] }))
  assert.deepEqual(lines, ['boss-verify gate: merged=0 parked=0 dispatched=0 skipped=0 deferred=0'])
  assert.equal(exitCode, 1)
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
  for (const argv of [['42'], ['--labels', 'x'], ['--batch']]) {
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
    '6',
    '--exclude-label',
    'infra',
    '--assignee=me',
  ])
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
