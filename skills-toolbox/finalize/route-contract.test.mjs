import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  OPTIONAL_ROUTE_TOKENS,
  TERMINAL_ROUTES,
  assertRouteSatisfied,
  stampObligation,
} from './route-contract.mjs'

const helper = fileURLToPath(new URL('./route-contract.mjs', import.meta.url))

function fixture() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'route-contract-'))
  return { dir, receipt: path.join(dir, 'receipt.json'), runId: 'run-1' }
}

function cli(args) {
  return spawnSync(process.execPath, [helper, ...args], { encoding: 'utf8' })
}

test('terminal routes cover exactly the four published outcomes', () => {
  assert.deepEqual(Object.keys(TERMINAL_ROUTES), [
    'REVIEW_READY',
    'PARTIAL',
    'BLOCKED',
    'NO_CHANGE',
  ])
  assert.deepEqual(OPTIONAL_ROUTE_TOKENS, [
    'blocked-pr-left-draft',
    'entry-state-restored',
    'no-change-breadcrumb-written',
  ])
  for (const [outcome, tokens] of Object.entries(TERMINAL_ROUTES)) {
    assert.ok(tokens.length > 0, `${outcome} must owe at least one token`)
  }
})

test('REVIEW_READY refuses absent stamps, downgrades on BLOCKED stamps, and passes when fully stamped', () => {
  const { receipt, runId } = fixture()
  let result = cli(['assert', '--outcome', 'REVIEW_READY', '--receipt', receipt, '--run-id', runId])
  assert.equal(result.status, 1)
  assert.equal(result.stdout.trim(), 'ROUTE_UNSATISFIED')

  for (const token of TERMINAL_ROUTES.BLOCKED) {
    execFileSync(process.execPath, [
      helper,
      'stamp',
      '--receipt',
      receipt,
      '--token',
      token,
      '--run-id',
      runId,
    ])
  }
  result = cli(['assert', '--outcome', 'REVIEW_READY', '--receipt', receipt, '--run-id', runId])
  assert.equal(result.status, 0)
  assert.equal(result.stdout.trim(), 'BLOCKED')

  fs.rmSync(receipt)

  for (const token of TERMINAL_ROUTES.REVIEW_READY) {
    execFileSync(process.execPath, [
      helper,
      'stamp',
      '--receipt',
      receipt,
      '--token',
      token,
      '--run-id',
      runId,
    ])
  }
  result = cli(['assert', '--outcome', 'REVIEW_READY', '--receipt', receipt, '--run-id', runId])
  assert.equal(result.status, 0)
  assert.equal(result.stdout.trim(), 'REVIEW_READY')
})

test('all outcomes fail closed on absent, empty, malformed and partial evidence', () => {
  for (const outcome of Object.keys(TERMINAL_ROUTES)) {
    const { receipt, runId } = fixture()
    let result = cli(['assert', '--outcome', outcome, '--receipt', receipt, '--run-id', runId])
    assert.equal(result.status, 1)
    assert.equal(result.stdout.trim(), 'ROUTE_UNSATISFIED')
    fs.writeFileSync(receipt, '')
    result = cli(['assert', '--outcome', outcome, '--receipt', receipt, '--run-id', runId])
    assert.equal(result.status, 1)
    assert.equal(result.stdout.trim(), 'ROUTE_UNSATISFIED')
    fs.writeFileSync(receipt, '{')
    result = cli(['assert', '--outcome', outcome, '--receipt', receipt, '--run-id', runId])
    assert.equal(result.status, 1)
    assert.equal(result.stdout.trim(), 'ROUTE_UNSATISFIED')
    fs.rmSync(receipt)
    stampObligation(receipt, TERMINAL_ROUTES[outcome][0], {
      runId,
      now: '2026-08-25T00:00:00.000Z',
    })
    if (TERMINAL_ROUTES[outcome].length > 1) {
      result = cli(['assert', '--outcome', outcome, '--receipt', receipt, '--run-id', runId])
      assert.equal(result.status, 1)
      assert.equal(result.stdout.trim(), 'ROUTE_UNSATISFIED')
    }
  }
})

test('generic BLOCKED and NO_CHANGE accept only universal cleanup obligations', () => {
  for (const outcome of ['BLOCKED', 'NO_CHANGE']) {
    const { receipt, runId } = fixture()
    for (const token of TERMINAL_ROUTES[outcome]) {
      stampObligation(receipt, token, { runId, now: '2026-08-25T00:00:00.000Z' })
    }
    const result = cli(['assert', '--outcome', outcome, '--receipt', receipt, '--run-id', runId])
    assert.equal(result.status, 0)
    assert.equal(result.stdout.trim(), outcome)
  }
})

test('route obligations must be stamped in order', () => {
  const { receipt, runId } = fixture()
  for (const token of [...TERMINAL_ROUTES.REVIEW_READY].reverse()) {
    stampObligation(receipt, token, { runId, now: '2026-08-25T00:00:00.000Z' })
  }
  const result = cli([
    'assert',
    '--outcome',
    'REVIEW_READY',
    '--receipt',
    receipt,
    '--run-id',
    runId,
  ])
  assert.equal(result.status, 1)
  assert.equal(result.stdout.trim(), 'ROUTE_UNSATISFIED')
  assert.match(result.stderr, /out of order/)
})

test('unknown tokens are reported rather than ignored', () => {
  const { receipt, runId } = fixture()
  fs.writeFileSync(
    receipt,
    JSON.stringify({
      runId,
      stamps: TERMINAL_ROUTES.BLOCKED.map((token) => ({ token })).concat({ token: 'surprise' }),
    }),
  )
  const result = cli(['assert', '--outcome', 'BLOCKED', '--receipt', receipt, '--run-id', runId])
  assert.equal(result.status, 1)
  assert.equal(result.stdout.trim(), 'ROUTE_UNSATISFIED')
  assert.match(result.stderr, /surprise/)
})

test('a receipt written under another run id reads as absent', () => {
  const { receipt, runId } = fixture()
  fs.writeFileSync(
    receipt,
    JSON.stringify({
      runId: 'other-run',
      stamps: TERMINAL_ROUTES.NO_CHANGE.map((token) => ({ token })),
    }),
  )
  const result = cli(['assert', '--outcome', 'NO_CHANGE', '--receipt', receipt, '--run-id', runId])
  assert.equal(result.status, 1)
  assert.equal(result.stdout.trim(), 'ROUTE_UNSATISFIED')
  assert.match(result.stderr, /absent/)
})

test('assertRouteSatisfied can classify in-memory stamps', () => {
  assert.equal(
    assertRouteSatisfied(
      'PARTIAL',
      TERMINAL_ROUTES.PARTIAL.map((token) => ({ token })),
    ).honestOutcome,
    'PARTIAL',
  )
})
