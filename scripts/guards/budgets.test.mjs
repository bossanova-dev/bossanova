import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import { BUDGETS_PATH, checkBudget, readBudgets, validateBudgets } from './budgets.mjs'

function fixture(text) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'guard-budgets-'))
  fs.mkdirSync(path.join(root, path.dirname(BUDGETS_PATH)), { recursive: true })
  fs.writeFileSync(path.join(root, BUDGETS_PATH), text)
  return root
}

function cleanup(root) {
  fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
}

const BUDGETS = {
  'lines-per-file': { default: 10, subjects: { 'big.go': 50, 'gone.go': 20 } },
}

test('checkBudget fails a subject over its own limit', () => {
  const { failures } = checkBudget({
    budgets: BUDGETS,
    guardId: 'lines-per-file',
    measured: { 'big.go': 51, 'gone.go': 1 },
  })
  assert.equal(failures.length, 1)
  assert.match(failures[0], /big\.go: 51 is over its budget of 50/)
})

test('checkBudget fails a subject over default', () => {
  const { failures } = checkBudget({
    budgets: BUDGETS,
    guardId: 'lines-per-file',
    measured: new Map([
      ['small.go', 11],
      ['big.go', 1],
      ['gone.go', 1],
    ]),
  })
  assert.equal(failures.length, 1)
  assert.match(failures[0], /small\.go: 11 is over the default budget of 10/)
})

test('checkBudget fails a subjects entry with no measurement', () => {
  const { failures } = checkBudget({
    budgets: BUDGETS,
    guardId: 'lines-per-file',
    measured: { 'big.go': 1 },
  })
  assert.equal(failures.length, 1)
  assert.match(failures[0], /gone\.go has a budget but no measurement — remove it/)
})

test('checkBudget passes a subject under budget', () => {
  const { failures } = checkBudget({
    budgets: BUDGETS,
    guardId: 'lines-per-file',
    measured: { 'big.go': 49, 'gone.go': 20, 'small.go': 10 },
  })
  assert.deepEqual(failures, [])
})

test('checkBudget treats a guard with no budget entry as unlimited', () => {
  const { failures } = checkBudget({ budgets: {}, guardId: 'other', measured: { a: 1000 } })
  assert.deepEqual(failures, [])
})

test('readBudgets accepts the empty shipped shape', () => {
  const root = fixture('{}\n')
  try {
    assert.deepEqual(readBudgets(root), { budgets: {}, errors: [] })
  } finally {
    cleanup(root)
  }
})

test('readBudgets rejects unsorted keys', () => {
  const root = fixture(
    JSON.stringify({
      'z-guard': { default: 1 },
      'a-guard': { subjects: { b: 1, a: 2 } },
    }),
  )
  try {
    const { errors } = readBudgets(root)
    assert.equal(errors.length, 2)
    assert.match(errors[0], /top-level keys must be sorted/)
    assert.match(errors[1], /a-guard\.subjects keys must be sorted/)
  } finally {
    cleanup(root)
  }
})

test('readBudgets rejects values that are not positive integers', () => {
  for (const bad of [0, -1, 1.5, '3', null]) {
    const errors = validateBudgets({ guard: { default: bad } })
    assert.equal(errors.length, 1, `default ${JSON.stringify(bad)}`)
    assert.match(errors[0], /guard\.default must be a positive integer/)
    const subjectErrors = validateBudgets({ guard: { subjects: { a: bad } } })
    assert.match(subjectErrors[0], /guard\.subjects\.a must be a positive integer/)
  }
  const root = fixture(JSON.stringify({ guard: { default: 0 } }))
  try {
    assert.match(readBudgets(root).errors[0], /positive integer/)
  } finally {
    cleanup(root)
  }
})

test('readBudgets rejects malformed JSON, a non-object and unknown keys', () => {
  const root = fixture('{')
  try {
    assert.match(readBudgets(root).errors[0], /cannot read/)
  } finally {
    cleanup(root)
  }
  assert.match(validateBudgets([])[0], /must be a JSON object/)
  assert.match(validateBudgets({ guard: { limit: 1 } })[0], /unknown key limit/)
})
