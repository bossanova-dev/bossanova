import assert from 'node:assert/strict'
import { test } from 'node:test'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { rollSample, main } from './notes-record.mjs'

function harness(t, config) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'notes-roll-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const output = [],
    errors = []
  const configure = (value) =>
    fs.writeFileSync(path.join(dir, '.boss-skills.json'), JSON.stringify(value))
  if (config !== undefined) configure(config)
  const deps = {
    cwd: dir,
    gitDir: dir,
    env: {},
    random: () => 0.5,
    now: () => new Date('2026-11-09T12:00:00Z'),
    stdout: (text) => output.push(text),
    stderr: (text) => errors.push(text),
  }
  const record = path.join(dir, 'boss-notes-roll-boss-plan.json')
  const roll = (extra = {}) => rollSample({ core: 'boss-plan', ...extra }, deps)
  return { dir, deps, record, roll, configure, output, errors }
}

for (const [rate, random, sampled] of [
  [1, 0.999999, true],
  [0, 0, false],
  [0.33, 0.32, true],
  [0.33, 0.33, false],
])
  test(`roll compares ${random} strictly against rate ${rate}`, (t) => {
    const h = harness(t, rate === 1 ? undefined : { notesDefaults: { sampleRate: rate } })
    h.deps.random = () => random
    assert.deepEqual(h.roll(), {
      ok: true,
      core: 'boss-plan',
      rate,
      sampled,
      source: 'fresh',
      persisted: true,
      failures: [],
    })
    assert.equal(fs.statSync(h.record).mode & 0o777, 0o600)
  })

test('same run reuses its verdict across a config edit and consume removes the record', (t) => {
  const h = harness(t)
  h.deps.env.BOSS_AGENT_SESSION_ID = 'chat-one'
  h.roll()
  h.configure({ notesDefaults: { sampleRate: 0 } })
  const reused = h.roll({ consume: true })
  assert.equal(reused.source, 'reused')
  assert.equal(reused.rate, 1)
  assert.equal(reused.sampled, true)
  assert.equal(fs.existsSync(h.record), false)
  const fresh = h.roll({ consume: true })
  assert.equal(fresh.source, 'fresh')
  assert.equal(fresh.sampled, false)
  assert.equal(fs.existsSync(h.record), false)
})

test('fresh ignores a live verdict and explicit run id takes precedence over environment', (t) => {
  const h = harness(t)
  h.deps.env = { BOSS_AGENT_SESSION_ID: 'chat', BOSS_SESSION_ID: 'session' }
  h.roll({ runId: 'explicit' })
  assert.equal(JSON.parse(fs.readFileSync(h.record)).runId, 'explicit')
  h.configure({ notesDefaults: { sampleRate: 0 } })
  assert.equal(h.roll({ fresh: true, runId: 'explicit' }).sampled, false)
  assert.equal(h.roll({ fresh: true, consume: true }).source, 'fresh')
  assert.equal(fs.existsSync(h.record), false)
})

for (const [name, change] of [
  ['different run', { runId: 'other' }],
  ['expired', { at: '2026-11-09T00:00:00Z' }],
  ['future', { at: '2026-11-09T12:00:01Z' }],
  ['another core', { core: 'boss-build' }],
  ['invalid version', { v: 2 }],
  ['invalid rate', { rate: 2 }],
  ['invalid sampled', { sampled: 'yes' }],
  ['invalid timestamp', { at: 'bad' }],
  ['malformed JSON', null],
])
  test(`roll replaces ${name} records`, (t) => {
    const h = harness(t)
    h.roll()
    const stored = JSON.parse(fs.readFileSync(h.record))
    fs.writeFileSync(h.record, change ? JSON.stringify({ ...stored, ...change }) : '{')
    assert.equal(h.roll().source, 'fresh')
  })

test('core rolls do not read each other and session id is the fallback identity', (t) => {
  const h = harness(t)
  h.deps.env.BOSS_SESSION_ID = 'session'
  h.roll()
  assert.equal(JSON.parse(fs.readFileSync(h.record)).runId, 'session')
  assert.equal(h.roll({ core: 'boss-build' }).source, 'fresh')
})

for (const kind of ['invalid', 'unreadable'])
  test(`config ${kind} falls back to reporting and returns zero`, (t) => {
    const h = harness(t)
    const config = path.join(h.dir, '.boss-skills.json')
    if (kind === 'invalid') fs.writeFileSync(config, '{')
    else fs.mkdirSync(config)
    assert.equal(main(['roll', '--core', 'boss-plan'], h.deps), 0)
    const result = JSON.parse(h.output[0])
    assert.equal(result.rate, 1)
    assert.equal(result.sampled, true)
    assert.equal(result.failures.length, 1)
    assert.equal(h.errors.length, 1)
  })

for (const kind of ['git-dir', 'write', 'consume'])
  test(`failed ${kind} still returns a usable verdict`, (t) => {
    const h = harness(t)
    if (kind === 'git-dir')
      h.deps.gitDir = () => {
        throw new Error('git unavailable')
      }
    else
      h.deps.fs = {
        ...fs,
        [kind === 'write' ? 'writeFileSync' : 'unlinkSync']: () => {
          throw new Error('denied')
        },
      }
    assert.equal(
      main(['roll', '--core', 'boss-plan', ...(kind === 'consume' ? ['--consume'] : [])], h.deps),
      0,
    )
    const result = JSON.parse(h.output[0])
    assert.equal(result.sampled, true)
    assert.equal(result.persisted, false)
    assert.equal(result.failures.length, 1)
  })

for (const flags of [['--unknown'], ['--fresh=true'], ['--fresh', 'true'], [], ['--core', '../x']])
  test(`invalid roll arguments remain nonfatal: ${flags.join(' ')}`, (t) => {
    const h = harness(t)
    assert.equal(
      main(
        [
          'roll',
          ...(flags.includes('--core') || flags.length === 0 ? [] : ['--core', 'boss-plan']),
          ...flags,
        ],
        h.deps,
      ),
      0,
    )
    const result = JSON.parse(h.output[0])
    assert.equal(result.ok, false)
    assert.equal(result.sampled, true)
    assert.equal(result.rate, 1)
    assert.equal(h.errors.length, 1)
  })

test('real CLI emits one JSON line with default reporting enabled', (t) => {
  const h = harness(t)
  assert.equal(spawnSync('git', ['init', '--quiet', h.dir]).status, 0)
  const result = spawnSync(
    process.execPath,
    [path.resolve('skills-toolbox/notes-record.mjs'), 'roll', '--core', 'boss-plan', '--consume'],
    { cwd: h.dir, encoding: 'utf8' },
  )
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout.trim().split('\n').length, 1)
  const value = JSON.parse(result.stdout)
  assert.equal(value.rate, 1)
  assert.equal(value.sampled, true)
})
