// scripts/finalize/boss-finalize.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  createBossFinalizeAdapter,
  finalizeOperationMap,
  resolveTagInject,
} from './boss-finalize.mjs'

test('injectPrTag shells the tag-injection command with PR number and BASE_BRANCH', () => {
  const calls = []
  const a = createBossFinalizeAdapter({
    runImpl: (cmd, args, opts) => calls.push({ cmd, args, opts }),
  })
  a.injectPrTag('1234', { baseBranch: 'main' })
  assert.equal(calls.length, 1)
  assert.match(calls[0].cmd, /add-pr-numbers\.sh$/)
  assert.deepEqual(calls[0].args, ['1234'])
  assert.equal(calls[0].opts.env.BASE_BRANCH, 'main')
})

test('injectPrTag coerces a numeric PR number to a string argument', () => {
  const calls = []
  const a = createBossFinalizeAdapter({
    runImpl: (cmd, args, opts) => calls.push({ cmd, args, opts }),
  })
  a.injectPrTag(1234, { baseBranch: 'main' })
  assert.deepEqual(calls[0].args, ['1234'])
})

test('the operation map names the ready and repair ops', () => {
  const a = createBossFinalizeAdapter()
  assert.equal(a.operationMap.readyPr.command, 'gh pr ready')
  assert.equal(a.operationMap.repair.skill, 'boss-repair')
  assert.equal(a.operationMap, finalizeOperationMap)
})

test('resolveTagInject prefers the Claude tree when it ships the helper', () => {
  const cmd = resolveTagInject((p) => p.includes('.claude'))
  assert.match(cmd, /\.claude\/skills\/bossanova\/boss-finalize\/add-pr-numbers\.sh$/)
})

test('resolveTagInject falls back to the Codex tree for a Codex-only install', () => {
  const cmd = resolveTagInject((p) => p.includes('.codex'))
  assert.match(cmd, /\.codex\/skills\/bossanova\/boss-finalize\/add-pr-numbers\.sh$/)
})

test('resolveTagInject defaults to the Claude path when neither tree exists', () => {
  const cmd = resolveTagInject(() => false)
  assert.match(cmd, /\.claude\/skills\/bossanova\/boss-finalize\/add-pr-numbers\.sh$/)
})

test('policy constants match the current SKILL values', () => {
  const a = createBossFinalizeAdapter()
  assert.equal(a.policy.tagFormat, '[#<PR>]')
  assert.equal(a.policy.repairCap, 5)
  assert.equal(a.policy.settleCap, 3)
})
