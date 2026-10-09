import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { evaluateBossRetroGate } from './boss-retro.mjs'
const note = (id = 'n', at = '2026-10-08T00:00:00Z') => ({
  id,
  tags: ['improvement'],
  created_at: at,
})
const evaluate = (options = {}) =>
  evaluateBossRetroGate({
    env: {},
    listNotes: () => [],
    readState: () => null,
    collectSignals: () => ({ planned: [] }),
    ...options,
  })
test('threshold below, at and above default', async () => {
  for (const count of [4, 5, 6])
    assert.equal(
      (
        await evaluate({
          listNotes: () => Array.from({ length: count }, (_, i) => note(String(i))),
        })
      ).hasWork,
      count >= 5,
    )
})
test('threshold precedence flag > env > config > default', async () => {
  for (const [options, threshold] of [
    [{}, 5],
    [{ config: { retro: { gateThreshold: 3 } } }, 3],
    [{ config: { retro: { gateThreshold: 3 } }, env: { BOSS_RETRO_GATE_THRESHOLD: '2' } }, 2],
    [
      {
        argv: ['--threshold=1'],
        env: { BOSS_RETRO_GATE_THRESHOLD: '2' },
        config: { retro: { gateThreshold: 3 } },
      },
      1,
    ],
  ]) {
    const result = await evaluate({ ...options, listNotes: () => [note()] })
    assert.match(result.reason, new RegExp(`threshold ${threshold} `))
    assert.equal(result.hasWork, threshold === 1)
  }
})
test('only notes newer than last write run count', async () => {
  const result = await evaluate({
    argv: ['--threshold', '1'],
    listNotes: () => [note('old', '2026-10-07T00:00:00Z'), note('equal')],
    readState: () => ({ version: 1, lastRunAt: '2026-10-08T00:00:00Z' }),
  })
  assert.equal(result.hasWork, false)
  assert.match(result.reason, /0 new notes/)
})
test('signals only collected below threshold and failures contribute zero', async () => {
  let called = 0
  const collectSignals = () => {
    called++
    return { planned: [{}, {}] }
  }
  assert.equal(
    (await evaluate({ argv: ['--threshold', '2'], listNotes: () => [note()], collectSignals }))
      .hasWork,
    true,
  )
  assert.equal(called, 1)
  assert.equal(
    (await evaluate({ argv: ['--threshold', '1'], listNotes: () => [note()], collectSignals }))
      .hasWork,
    true,
  )
  assert.equal(called, 1)
  const result = await evaluate({
    collectSignals: () => {
      throw new Error('timeout')
    },
  })
  assert.equal(result.hasWork, false)
  assert.match(result.reason, /contribute 0 \(timeout\)/)
})
test('unreachable binary runs the session; malformed notes and flags fail closed', async () => {
  assert.equal((await evaluate({ listNotes: () => ({ unreachable: true }) })).hasWork, true)
  for (const notes of [
    null,
    {},
    [null],
    [{}],
    [{ ...note(), tags: ['other'] }],
    [{ ...note(), created_at: 'bad' }],
  ])
    assert.equal((await evaluate({ listNotes: () => notes })).hasWork, false)
  for (const argv of [['--wat'], ['--threshold'], ['--threshold', '0'], ['--threshold', 'NaN']])
    assert.equal((await evaluate({ argv })).hasWork, false)
  for (const options of [
    { env: { BOSS_RETRO_GATE_THRESHOLD: 'bad' } },
    { config: { retro: { gateThreshold: 0 } } },
  ])
    assert.equal((await evaluate(options)).hasWork, false)
})
const gate = new URL('./boss-retro.mjs', import.meta.url).pathname
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'retro-gate-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  return dir
}
function fake(dir, output) {
  const file = path.join(dir, 'boss')
  fs.writeFileSync(file, `#!/bin/sh\nprintf '%s\\n' '${output}'\n`)
  fs.chmodSync(file, 0o755)
  return file
}
function run(dir, env, argv = ['--threshold', '1']) {
  return spawnSync(process.execPath, [gate, ...argv], { cwd: dir, env, encoding: 'utf8' })
}
test('process fake BOSS_BIN validates JSON, notes and threshold', (t) => {
  const dir = fixture(t)
  for (const output of ['oops', '[null]', '[{}]', JSON.stringify([{ ...note(), tags: ['other'] }])])
    assert.equal(run(dir, { BOSS_BIN: fake(dir, output), PATH: '' }).status, 1)
  assert.equal(run(dir, { BOSS_BIN: fake(dir, JSON.stringify([note()])), PATH: '' }).status, 0)
  assert.equal(
    run(dir, { BOSS_BIN: fake(dir, JSON.stringify([note()])), PATH: '' }, ['--threshold', 'bad'])
      .status,
    1,
  )
})
test('process resolver falls through stale BOSS_BIN to PATH and missing CLI runs', (t) => {
  const dir = fixture(t)
  fake(dir, JSON.stringify([note()]))
  assert.equal(run(dir, { BOSS_BIN: path.join(dir, 'gone'), PATH: dir }).status, 0)
  const result = run(dir, { BOSS_BIN: path.join(dir, 'gone'), PATH: '' })
  assert.equal(result.status, 0)
  assert.match(result.stderr, /CLI unreachable/)
})
