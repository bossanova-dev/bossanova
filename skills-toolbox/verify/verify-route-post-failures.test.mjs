import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { parkNote } from './verify-park-note.mjs'
import { buildNotes } from './verify-notes.mjs'
import { sweep, timedRunner } from './verify-route.mjs'

const head = 'a'.repeat(40)
function fixture() {
  const dir = tempDir('post-failures-')
  const w = { head, fail: true, escalationFails: false, calls: [] }
  w.deps = {
    cwd: dir,
    logDir: dir,
    stateFile: join(dir, 'state.json'),
    nodeBin: 'node',
    env: {},
    readProjectConfig: () => null,
    resolveBoss: () => ({ ok: true, path: '/boss' }),
    run(cmd, args) {
      const ok = (payload) => ({ ok: true, payload })
      const flag = (name) => args[args.indexOf(`--${name}`) + 1]
      if (cmd === '/boss') return ok({ sessions: [] })
      if (args[1] === 'candidates')
        return ok({ candidates: [{ pr: 1, repo: 'o/r', headSha: w.head }] })
      if (args[1] === 'judge')
        return ok({
          verdict: 'human',
          reason: 'no-session',
          headSha: w.head,
          policyFingerprint: '0123456789abcdef',
        })
      if (args[1] === 'post') {
        w.calls.push(args)
        const escalation = flag('reason') === 'verify-post-failed'
        if (escalation ? w.escalationFails : w.fail)
          return { ok: false, error: 'exit-2', stderr: 'bad argument' }
        return ok({ posted: true })
      }
      return { ok: false, error: 'unexpected' }
    },
  }
  w.tick = (dryRun = false) => sweep({ dryRun }, w.deps).records[0]
  w.clean = () => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  return w
}

test('three failures park the same head with policy and error evidence', () => {
  const w = fixture()
  try {
    const first = w.tick()
    assert.match(first.line, /post failed \(exit-2: bad argument\)/)
    assert.equal(first.stderr, 'bad argument')
    assert.equal(w.tick().outcome, 'skipped')
    assert.equal(w.tick().outcome, 'parked')
    const args = w.calls.at(-1)
    assert.equal(args[args.indexOf('--reason') + 1], 'verify-post-failed')
    assert.equal(args[args.indexOf('--policy') + 1], '0123456789abcdef')
    const notes = JSON.parse(fs.readFileSync(args[args.indexOf('--notes') + 1], 'utf8'))
    assert.match(JSON.stringify(notes), /human.*3.*bad argument/)
    assert.match(buildNotes(notes)[0].text, /bad argument/)
    assert.match(
      parkNote({ codes: ['verify-post-failed'], headSha: head, facts: { postFailureNotes: notes } })
        .body,
      /bad argument/,
    )
  } finally {
    w.clean()
  }
})

test('head changes reset failures, successful posts clear them and dry runs never count', () => {
  const w = fixture()
  try {
    w.tick()
    w.tick()
    w.head = 'b'.repeat(40)
    assert.equal(w.tick().outcome, 'skipped')
    w.fail = false
    w.tick()
    w.fail = true
    assert.equal(w.tick().outcome, 'skipped')
    for (let i = 0; i < 4; i++) assert.equal(w.tick(true).outcome, 'skipped')
    assert.equal(w.tick().outcome, 'skipped')
    assert.equal(w.tick().outcome, 'parked')
  } finally {
    w.clean()
  }
})

test('a failed escalation is visible and retried on the next tick', () => {
  const w = fixture()
  try {
    w.tick()
    w.tick()
    w.escalationFails = true
    assert.match(w.tick().line, /escalation failed \(exit-2: bad argument\)/)
    w.escalationFails = false
    assert.equal(w.tick().outcome, 'parked')
  } finally {
    w.clean()
  }
})

test('post errors use redacted single-line stderr capped at 200 runes', () => {
  const w = fixture()
  try {
    const captured = timedRunner({
      spawn: () => ({
        status: 2,
        stderr: `old\nerror ghp_${'a'.repeat(36)} ${'x'.repeat(190)}${'😅'.repeat(30)}\n`,
      }),
    })('node', [])
    const run = w.deps.run
    w.deps.run = (cmd, args) => (args[1] === 'post' ? captured : run(cmd, args))
    const record = w.tick()
    assert.ok(Array.from(record.stderr).length <= 200)
    assert.ok(record.stderr.endsWith('…'))
    assert.ok(!record.line.includes('a'.repeat(36)))
    assert.ok(!record.line.includes('\n'))
  } finally {
    w.clean()
  }
})
