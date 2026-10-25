import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { tempDir } from './test-tmpdir.mjs'
import {
  admissionConfig,
  staleSlot,
  inheritedAdmission,
  acquireGateSlot,
  releaseGateSlot,
} from './gate-admission-lib.mjs'

test('configuration coordinates homes and disables admission explicitly or in CI', () => {
  assert.equal(admissionConfig({}, 10).slots, 2)
  assert.equal(admissionConfig({}, 2).slots, 1)
  assert.equal(admissionConfig({ BOSS_GATE_MAX_CONCURRENT: '0' }, 10).slots, 0)
  assert.equal(admissionConfig({ CI: '1' }, 10).slots, 0)
  assert.equal(admissionConfig({ BOSS_GATE_MAX_CONCURRENT: '3' }, 10).slots, 3)
})

test('dead and malformed records are stale; live inherited tokens require an absolute slot path', () => {
  assert.equal(
    staleSlot({ pid: 7 }, () => false),
    true,
  )
  assert.equal(
    staleSlot(null, () => true),
    true,
  )
  assert.equal(
    staleSlot({ pid: 7 }, () => true),
    false,
  )
  assert.equal(
    inheritedAdmission(
      { BOSS_GATE_ADMITTED: '/slots/slot-0', BOSS_GATE_ADMISSION_DIR: '/slots' },
      () => ({ pid: 7 }),
      () => true,
    ),
    '/slots/slot-0',
  )
  assert.equal(
    inheritedAdmission(
      { BOSS_GATE_ADMITTED: '/slots/slot-0', BOSS_GATE_ADMISSION_DIR: '/slots' },
      () => ({ pid: 7 }),
      () => false,
    ),
    null,
  )
  assert.equal(
    inheritedAdmission(
      { BOSS_GATE_ADMITTED: 'foreign' },
      () => ({ pid: 7 }),
      () => true,
    ),
    null,
  )
})

test('stale records are reclaimed in the same pass and release checks ownership', async () => {
  const dir = tempDir('gate-admission-')
  fs.writeFileSync(path.join(dir, 'slot-0'), JSON.stringify({ pid: 123 }))
  const handle = await acquireGateSlot({
    dir,
    slots: 1,
    timeoutMs: 100,
    pollMs: 1,
    isAlive: () => false,
    sleep: () => assert.fail('must not wait'),
  })
  assert.equal(handle.waitedMs, 0)
  fs.writeFileSync(handle.file, JSON.stringify({ pid: 999 }))
  releaseGateSlot(handle)
  assert.equal(fs.existsSync(handle.file), true)
  fs.writeFileSync(handle.file, JSON.stringify({ pid: process.pid }))
  releaseGateSlot(handle)
  assert.equal(fs.existsSync(handle.file), false)
})

test('queue logs only holder changes and refuses without a slot at its budget', async () => {
  const dir = tempDir('gate-admission-')
  fs.writeFileSync(
    path.join(dir, 'slot-0'),
    JSON.stringify({ pid: 123, cwd: '/one/worktree', label: 'test' }),
  )
  let clock = 0
  const logs = []
  await assert.rejects(
    acquireGateSlot({
      dir,
      slots: 1,
      timeoutMs: 30,
      pollMs: 10,
      isAlive: () => true,
      now: () => clock,
      sleep: async (ms) => {
        clock += ms
      },
      log: (line) => logs.push(line),
    }),
    { code: 'GATE_ADMISSION_TIMEOUT' },
  )
  assert.equal(logs.length, 1)
  assert.match(logs[0], /123.*worktree.*test/)
})

test('a live token from another slot directory does not bypass admission', () => {
  assert.equal(
    inheritedAdmission(
      { BOSS_GATE_ADMITTED: '/foreign/slot-0', BOSS_GATE_ADMISSION_DIR: '/slots' },
      () => ({ pid: 7 }),
      () => true,
    ),
    null,
  )
})
