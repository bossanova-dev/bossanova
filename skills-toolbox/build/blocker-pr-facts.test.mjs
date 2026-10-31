import assert from 'node:assert/strict'
import { test } from 'node:test'

import { classifyChecks } from '../pr-check-state.mjs'

import { createPrLookup } from './blocker-pr-facts.mjs'

const url = 'https://github.com/owner/repo/pull/12'
const facts = { state: 'MERGED', isDraft: false, mergedAt: '2026-10-30T00:00:00Z' }
const unknown = {
  files: null,
  checks: classifyChecks({ rollup: null, readError: 'invalid-rollup' }),
  green: false,
}
const expectedFacts = { ...facts, ...unknown }

test('looks up PR facts once per URL, including simultaneous requests', async () => {
  const calls = []
  const lookup = createPrLookup({
    timeoutMs: 1000,
    run: async (...args) => {
      calls.push(args)
      return { status: 0, stdout: JSON.stringify(facts) }
    },
  })
  assert.deepEqual(await Promise.all([lookup(url), lookup(url)]), [expectedFacts, expectedFacts])
  assert.deepEqual(await lookup(url), expectedFacts)
  assert.equal(calls.length, 1)
  assert.deepEqual(calls[0].slice(0, 2), [
    'gh',
    ['pr', 'view', url, '--json', 'state,isDraft,mergedAt,files,statusCheckRollup'],
  ])
  assert.equal(calls[0][2].timeout, 1000)
  await lookup('https://github.com/owner/repo/pull/13')
  assert.equal(calls.length, 2)
})

test('failed, missing and malformed command results resolve errors and are cached', async () => {
  const runners = [
    () => ({ status: 1, stdout: JSON.stringify(facts) }),
    () => ({ status: null, error: { code: 'ETIMEDOUT' } }),
    () => ({ status: 0, stdout: 'not json' }),
    () => ({ status: 0, stdout: 'null' }),
    () => ({ status: 0, stdout: JSON.stringify({ state: 'MERGED' }) }),
    () => undefined,
    () => {
      throw new Error('unavailable')
    },
    async () => {
      throw new Error('unavailable')
    },
  ]
  for (const runner of runners) {
    let calls = 0
    const lookup = createPrLookup({
      run: (...args) => {
        calls++
        return runner(...args)
      },
    })
    const result = await lookup(url)
    assert.equal(typeof result.error, 'string')
    assert.ok(result.error)
    assert.deepEqual(await lookup(url), result)
    assert.equal(calls, 1)
  }
})

test('bounds an asynchronous runner that never settles', async () => {
  const lookup = createPrLookup({ run: () => new Promise(() => {}), timeoutMs: 1 })
  assert.deepEqual(await lookup(url), { error: 'timeout' })
})

test('accepts open draft PRs with no merge timestamp', async () => {
  const open = { state: 'OPEN', isDraft: true, mergedAt: null }
  const lookup = createPrLookup({ run: () => ({ status: 0, stdout: JSON.stringify(open) }) })
  assert.deepEqual(await lookup(url), { ...open, ...unknown })
})

const passed = { name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }
const open = { state: 'OPEN', isDraft: false, mergedAt: null }

async function lookupFacts(extra) {
  return createPrLookup({
    run: () => ({ status: 0, stdout: JSON.stringify({ ...open, ...extra }) }),
  })(url)
}

test('exposes changed paths and shared check verdict for green, pending and failed PRs', async () => {
  for (const [check, green, state] of [
    [passed, true, 'green'],
    [{ name: 'test', status: 'IN_PROGRESS', conclusion: null }, false, 'pending'],
    [{ ...passed, conclusion: 'FAILURE' }, false, 'failing'],
  ]) {
    const result = await lookupFacts({
      files: [{ path: 'src/main.mjs' }],
      statusCheckRollup: [check],
    })
    assert.deepEqual(result.files, ['src/main.mjs'])
    assert.equal(result.green, green)
    assert.equal(result.checks.state, state)
    assert.deepEqual(result.checks, classifyChecks({ rollup: [check] }))
  }
})

test('missing, malformed and possibly truncated files are unknown while merge facts survive', async () => {
  for (const files of [
    undefined,
    null,
    {},
    [{ path: '' }],
    [{ path: 1 }],
    [null],
    Array.from({ length: 100 }, (_, i) => ({ path: `file-${i}.mjs` })),
  ]) {
    const result = await lookupFacts({ files, statusCheckRollup: [passed] })
    assert.equal(result.files, null)
    assert.equal(result.state, 'OPEN')
  }
  assert.deepEqual((await lookupFacts({ files: [], statusCheckRollup: [passed] })).files, [])
  assert.equal(
    (
      await lookupFacts({
        files: Array.from({ length: 99 }, (_, i) => ({ path: `file-${i}.mjs` })),
      })
    ).files.length,
    99,
  )
})

test('missing, malformed and empty check rollups never count as green', async () => {
  for (const statusCheckRollup of [
    undefined,
    null,
    {},
    [],
    [null],
    [passed, {}],
    [passed, { name: 'unknown', conclusion: 'UNRECOGNIZED' }],
  ]) {
    const result = await lookupFacts({ files: [{ path: 'src/main.mjs' }], statusCheckRollup })
    assert.equal(result.green, false)
    assert.equal(result.checks.state, 'unknown')
  }
})
