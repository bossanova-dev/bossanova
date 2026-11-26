import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { DEFAULT_DISPATCH_LEG_TIMEOUT_MS } from '../bs-dispatch-await.mjs'
import { classifyVerifyOutcome } from '../skill-extensions.mjs'
import { CLAIM_HEARTBEAT_EVERY_MS, CLAIM_STALE_AFTER_MS, claimState } from './verify-vocabulary.mjs'
import {
  HARNESS_STATE_PATHS,
  LEG_EXIT_CODES,
  finishLeg,
  gitIn,
  startLeg,
  waitLeg,
} from './verify-extension-leg.mjs'

const T0 = Date.parse('2026-11-25T00:00:00Z')
const MIN = 60_000

function fixture(t) {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-extension-leg-'))
  t.after(() => fs.rmSync(base, { recursive: true, force: true, maxRetries: 5 }))
  const repo = path.join(base, 'repo')
  const runDir = path.join(base, 'run')
  fs.mkdirSync(repo)
  fs.mkdirSync(runDir)
  const sh = (...args) => execFileSync('git', args, { cwd: repo, stdio: 'pipe' })
  sh('init', '-q')
  sh('config', 'user.email', 'leg@example.invalid')
  sh('config', 'user.name', 'leg')
  sh('config', 'commit.gpgsign', 'false')
  fs.writeFileSync(path.join(repo, '.gitignore'), 'out/\n')
  fs.writeFileSync(path.join(repo, 'tracked.txt'), 'one\n')
  sh('add', '.')
  sh('commit', '-q', '-m', 'init')
  const write = (rel, body = 'x\n') => {
    fs.mkdirSync(path.dirname(path.join(repo, rel)), { recursive: true })
    fs.writeFileSync(path.join(repo, rel), body)
  }
  return { repo, runDir, git: gitIn(repo), write }
}

const discovery = (descriptor) => ({
  extensions: [{ name: 'boss-verify-rec', role: 'verify', ...descriptor }],
  skipped: [],
})

function clock(start) {
  let at = start
  return { now: () => at, sleep: async (ms) => void (at += ms), set: (ms) => void (at = ms) }
}

test('a declared timeout sets the leg deadline; an undeclared leg keeps the default', async (t) => {
  const f = fixture(t)
  const leg = startLeg({
    ...f,
    discovery: discovery({ timeoutMinutes: 20 }),
    name: 'boss-verify-rec',
    now: T0,
    env: {},
  })
  assert.equal(leg.deadlineAt, T0 + 20 * MIN)
  const c = clock(T0 + 19 * MIN)
  const beat = () => ({ won: true })
  const held = await waitLeg({ ...f, name: 'boss-verify-rec', beat, budgetMs: 30_000, ...c })
  assert.equal(held.outcome, 'budget', 'past the five-minute default but inside 20 minutes')
  const end = await waitLeg({ ...f, name: 'boss-verify-rec', beat, budgetMs: 2 * MIN, ...c })
  assert.equal(end.outcome, 'timed-out')
  assert.equal(c.now(), T0 + 20 * MIN)
  assert.equal(LEG_EXIT_CODES[end.outcome], 96)
  const entry = finishLeg({ ...f, name: 'boss-verify-rec', resultsPath: `${f.runDir}/r.json` })
  assert.deepEqual([entry.timedOut, entry.crashed], [true, false])
  assert.equal(classifyVerifyOutcome(entry).reason, 'timed-out')

  const plain = startLeg({
    ...f,
    discovery: discovery({}),
    name: 'boss-verify-rec',
    now: T0,
    env: {},
  })
  assert.equal(plain.deadlineAt, T0 + DEFAULT_DISPATCH_LEG_TIMEOUT_MS)
})

test('a written result completes the leg and becomes its results entry', async (t) => {
  const f = fixture(t)
  const leg = startLeg({ ...f, discovery: discovery({ optional: true }), name: 'boss-verify-rec' })
  const result = { ok: true, extension: 'boss-verify-rec', role: 'verify', verdict: 'abstain' }
  fs.writeFileSync(leg.outPath, JSON.stringify(result))
  const done = await waitLeg({ ...f, name: 'boss-verify-rec', beat: () => ({ won: true }) })
  assert.equal(done.outcome, 'completed')
  const resultsPath = path.join(f.runDir, 'results.json')
  fs.writeFileSync(resultsPath, JSON.stringify([{ extension: 'boss-verify-other', crashed: true }]))
  finishLeg({ ...f, name: 'boss-verify-rec', resultsPath })
  const entries = JSON.parse(fs.readFileSync(resultsPath, 'utf8'))
  assert.deepEqual(entries, [
    { extension: 'boss-verify-other', crashed: true },
    { extension: 'boss-verify-rec', optional: true, timedOut: false, crashed: false, result },
  ])
})

test('a landed result does not end the hold while the subagent may still write', async (t) => {
  const f = fixture(t)
  const leg = startLeg({
    ...f,
    discovery: discovery({ timeoutMinutes: 30 }),
    name: 'boss-verify-rec',
    now: T0,
  })
  const c = clock(T0)
  const beats = []
  const beat = () => (beats.push(c.now()), { won: true })
  fs.writeFileSync(leg.outPath, JSON.stringify({ ok: true, verdict: 'abstain' }))
  const landed = await waitLeg({ ...f, name: 'boss-verify-rec', beat, budgetMs: 2 * MIN, ...c })
  assert.equal(LEG_EXIT_CODES[landed.outcome], 0)
  c.set(T0 + CLAIM_HEARTBEAT_EVERY_MS)
  const held = await waitLeg({ ...f, name: 'boss-verify-rec', beat, budgetMs: 2 * MIN, ...c })
  assert.equal(held.outcome, 'budget', 'the subagent has not returned, so the hold re-arms')
  assert.deepEqual(beats, [T0 + CLAIM_HEARTBEAT_EVERY_MS], 'the held leg keeps its claim live')
  f.write('late.txt')
  const entry = finishLeg({ ...f, name: 'boss-verify-rec', resultsPath: `${f.runDir}/r.json` })
  assert.deepEqual([entry.timedOut, entry.dirtied, entry.changed], [false, true, ['late.txt']])
})

test('a deadline after the result landed stops the subagent but keeps the result', async (t) => {
  const f = fixture(t)
  const leg = startLeg({
    ...f,
    discovery: discovery({ timeoutMinutes: 20 }),
    name: 'boss-verify-rec',
    now: T0,
  })
  const result = { ok: true, extension: 'boss-verify-rec', role: 'verify', verdict: 'pass' }
  const c = clock(T0 + 19 * MIN)
  const beat = () => ({ won: true })
  fs.writeFileSync(leg.outPath, JSON.stringify(result))
  const landed = await waitLeg({ ...f, name: 'boss-verify-rec', beat, budgetMs: 2 * MIN, ...c })
  assert.equal(landed.outcome, 'completed')
  const late = await waitLeg({ ...f, name: 'boss-verify-rec', beat, budgetMs: 2 * MIN, ...c })
  assert.equal(LEG_EXIT_CODES[late.outcome], 96, 'the deadline still stops the subagent')
  const entry = finishLeg({ ...f, name: 'boss-verify-rec', resultsPath: `${f.runDir}/r.json` })
  assert.deepEqual([entry.timedOut, entry.crashed, entry.result], [false, false, result])
  assert.notEqual(classifyVerifyOutcome(entry).reason, 'timed-out')
})

test('the heartbeat keeps the claim live for a leg longer than the stale window', async (t) => {
  const f = fixture(t)
  startLeg({ ...f, discovery: discovery({ timeoutMinutes: 45 }), name: 'boss-verify-rec', now: T0 })
  let status = { state: 'pending', description: 'verifying… mine0001', updatedAt: iso(T0) }
  const beats = []
  const c = clock(T0)
  const beat = () => {
    beats.push(c.now())
    status = { ...status, updatedAt: iso(c.now()) }
    return { won: true }
  }
  for (let calls = 0; calls < 30; calls += 1) {
    const r = await waitLeg({ ...f, name: 'boss-verify-rec', beat, budgetMs: 110_000, ...c })
    const peer = claimState(status, { token: 'peer0001', now: c.now() })
    assert.equal(peer.kind, 'foreign', `a peer sees a live claim at +${(c.now() - T0) / MIN}m`)
    if (r.outcome !== 'budget') break
  }
  assert.ok(c.now() - T0 > CLAIM_STALE_AFTER_MS, 'the hold outlived the stale window')
  assert.equal(beats[0], T0, 'the first hold beats at once')
  for (let i = 1; i < beats.length; i += 1)
    assert.ok(beats[i] - beats[i - 1] < CLAIM_STALE_AFTER_MS, 'no gap reaches the stale window')
  assert.ok(beats.length >= 3 && beats[1] - beats[0] >= CLAIM_HEARTBEAT_EVERY_MS)
  const stopped = claimState(status, {
    token: 'peer0001',
    now: beats.at(-1) + CLAIM_STALE_AFTER_MS + 1,
  })
  assert.equal(stopped.kind, 'stale', 'once beating stops, a peer may take the claim over')
})

test('a lost claim ends the hold, and a failed beat retries before the next interval', async (t) => {
  const f = fixture(t)
  startLeg({ ...f, discovery: discovery({ timeoutMinutes: 30 }), name: 'boss-verify-rec', now: T0 })
  const c = clock(T0)
  const replies = [{ won: false, reason: 'status-post-failed' }, { won: true }]
  const beats = []
  const beat = () => (beats.push(c.now()), replies.shift() ?? { won: false, reason: 'claim-lost' })
  const first = await waitLeg({ ...f, name: 'boss-verify-rec', beat, budgetMs: 2 * MIN, ...c })
  assert.equal(first.outcome, 'budget')
  assert.deepEqual(beats, [T0, T0 + MIN], 'the failed beat was retried a minute later')
  c.set(T0 + MIN + CLAIM_HEARTBEAT_EVERY_MS)
  const lost = await waitLeg({ ...f, name: 'boss-verify-rec', beat, ...c })
  assert.equal(lost.outcome, 'claim-lost')
  assert.equal(LEG_EXIT_CODES[lost.outcome], 94)
})

test('the worktree check fails tracked, unignored and undeclared ignored changes', async (t) => {
  const f = fixture(t)
  const run = (descriptor, act) => {
    const leg = startLeg({ ...f, discovery: discovery(descriptor), name: 'boss-verify-rec' })
    fs.writeFileSync(leg.outPath, JSON.stringify({ ok: true, verdict: 'abstain' }))
    act()
    const resultsPath = path.join(f.runDir, 'results.json')
    return finishLeg({ ...f, name: 'boss-verify-rec', resultsPath })
  }
  const declared = run({ writes: 'ignored' }, () => f.write('out/demo.mp4'))
  assert.equal(declared.dirtied, undefined, 'declared ignored output is allowed')

  const unignored = run({ writes: 'ignored' }, () => f.write('notes/x.log'))
  assert.deepEqual([unignored.dirtied, unignored.changed], [true, ['notes/x.log']])

  fs.rmSync(path.join(f.repo, 'out'), { recursive: true, force: true, maxRetries: 5 })
  const ignoredUndeclared = run({}, () => f.write('out/demo.mp4'))
  assert.deepEqual([ignoredUndeclared.dirtied, ignoredUndeclared.changed], [true, ['out/']])

  const tracked = run({ writes: 'ignored', optional: true }, () => f.write('tracked.txt', 'two\n'))
  assert.deepEqual([tracked.dirtied, tracked.changed], [true, ['tracked.txt']])
  const outcome = classifyVerifyOutcome(tracked)
  assert.deepEqual(
    [outcome.outcome, outcome.reason],
    ['failed', 'extension-dirtied-worktree'],
    'even an optional extension fails when it dirties the worktree',
  )
})

test('rewriting a path that was already dirty before the leg still dirties it', async (t) => {
  const f = fixture(t)
  f.write('tracked.txt', 'edited before the leg\n')
  f.write('notes/a.log', 'untracked before the leg\n')
  f.write('out/cache.bin', 'ignored before the leg\n')
  const run = (descriptor, act) => {
    const leg = startLeg({ ...f, discovery: discovery(descriptor), name: 'boss-verify-rec' })
    fs.writeFileSync(leg.outPath, JSON.stringify({ ok: true, verdict: 'abstain' }))
    act()
    return finishLeg({ ...f, name: 'boss-verify-rec', resultsPath: `${f.runDir}/r.json` })
  }
  assert.equal(run({}, () => {}).dirtied, undefined, 'an untouched dirty baseline is clean')

  const tracked = run({ writes: 'ignored' }, () => f.write('tracked.txt', 'rewritten\n'))
  assert.deepEqual([tracked.dirtied, tracked.changed], [true, ['tracked.txt']])

  const untracked = run({ writes: 'ignored' }, () => f.write('notes/a.log', 'rewritten\n'))
  assert.deepEqual([untracked.dirtied, untracked.changed], [true, ['notes/a.log']])

  const sibling = run({ writes: 'ignored' }, () => f.write('notes/b.log'))
  assert.deepEqual([sibling.dirtied, sibling.changed], [true, ['notes/b.log']])

  const ignored = run({}, () => f.write('out/cache.bin', 'rewritten, a longer body\n'))
  assert.deepEqual([ignored.dirtied, ignored.changed], [true, ['out/']])

  const deep = run({}, () => f.write('out/deep/new.bin'))
  assert.deepEqual([deep.dirtied, deep.changed], [true, ['out/']])

  const allowed = run({ writes: 'ignored' }, () => f.write('out/cache.bin', 'declared\n'))
  assert.equal(allowed.dirtied, undefined, 'a declared ignored rewrite stays allowed')
})

test('harness state the agent rewrites during a leg never dirties it', async (t) => {
  const run = (f, act) => {
    const leg = startLeg({ ...f, discovery: discovery({}), name: 'boss-verify-rec' })
    fs.writeFileSync(leg.outPath, JSON.stringify({ ok: true, verdict: 'abstain' }))
    act()
    return finishLeg({ ...f, name: 'boss-verify-rec', resultsPath: `${f.runDir}/r.json` })
  }
  const named = fixture(t)
  fs.appendFileSync(path.join(named.repo, '.gitignore'), `/${HARNESS_STATE_PATHS.join('\n/')}\n`)
  for (const rel of HARNESS_STATE_PATHS) named.write(rel, 'before the leg\n')
  const rewrite = (f) => () => {
    for (const rel of HARNESS_STATE_PATHS) f.write(rel, 'rewritten by the harness mid-leg\n')
  }
  assert.equal(run(named, rewrite(named)).dirtied, undefined, 'ignored by name')

  const collapsed = fixture(t)
  fs.appendFileSync(path.join(collapsed.repo, '.gitignore'), '.claude/\n')
  collapsed.write('.claude/skills/x/SKILL.md')
  assert.equal(run(collapsed, rewrite(collapsed)).dirtied, undefined, 'inside an ignored dir')
  const leg = run(collapsed, () => collapsed.write('.claude/skills/x/SKILL.md', 'leg wrote\n'))
  assert.deepEqual([leg.dirtied, leg.changed], [true, ['.claude/']], 'other ignored writes count')
})

function iso(ms) {
  return new Date(ms).toISOString()
}
