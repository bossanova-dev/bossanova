import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { isBossdManaged } from './bossd-present.mjs'

test('isBossdManaged is true when BOSS_SESSION_ID is set', () => {
  assert.equal(isBossdManaged({ BOSS_SESSION_ID: 'sess-123' }), true)
})

test('isBossdManaged is false when BOSS_SESSION_ID is absent', () => {
  assert.equal(isBossdManaged({}), false)
})

test('isBossdManaged is false when BOSS_SESSION_ID is empty', () => {
  assert.equal(isBossdManaged({ BOSS_SESSION_ID: '' }), false)
})

const cli = fileURLToPath(new URL('./bossd-present.mjs', import.meta.url))

test('CLI exits 0 when managed', () => {
  execFileSync('node', [cli], { env: { ...process.env, BOSS_SESSION_ID: 'sess-1' } })
})

test('CLI exits 3 when standalone', () => {
  const env = { ...process.env }
  delete env.BOSS_SESSION_ID
  assert.throws(
    () => execFileSync('node', [cli], { env }),
    (err) => err.status === 3,
  )
})
