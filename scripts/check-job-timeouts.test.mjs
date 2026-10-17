import assert from 'node:assert/strict'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  findJobsWithoutTimeout,
  findJobsWithoutTimeoutInRepo,
  renderVerdict,
} from './check-job-timeouts.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

const workflow = (jobs) => ['name: synthetic', 'on: push', 'jobs:', ...jobs, ''].join('\n')

test('a job without timeout-minutes is reported with its line', () => {
  const text = workflow([
    '  build:',
    '    runs-on: ubicloud',
    '    steps:',
    '      - run: make',
    '  lint:',
    '    runs-on: ubicloud',
    '    timeout-minutes: 10',
    '    steps:',
    '      - run: make lint',
  ])
  assert.deepEqual(findJobsWithoutTimeout(text), [{ job: 'build', line: 4 }])
})

test('a step-level uses: at the job key indent does not exempt the job', () => {
  const text = workflow([
    '  web:',
    '    runs-on: ubicloud',
    '    steps:',
    '    - uses: actions/checkout@0000000000000000000000000000000000000000',
    '      timeout-minutes: 5',
  ])
  assert.deepEqual(findJobsWithoutTimeout(text), [{ job: 'web', line: 4 }])
})

test('a reusable-workflow job is exempt because it cannot set timeout-minutes', () => {
  const text = workflow(['  call:', '    uses: ./.github/workflows/other.yml'])
  assert.deepEqual(findJobsWithoutTimeout(text), [])
})

test('timeout-minutes inside a run: block scalar does not count', () => {
  const text = workflow([
    '  build:',
    '    runs-on: ubicloud',
    '    steps:',
    '      - run: |',
    '          timeout-minutes: 3',
  ])
  assert.deepEqual(findJobsWithoutTimeout(text), [{ job: 'build', line: 4 }])
})

test('an expression value is accepted', () => {
  const text = workflow([
    '  smoke:',
    "    timeout-minutes: ${{ matrix.pass == 'race' && 50 || 35 }}",
    '    runs-on: ubicloud',
  ])
  assert.deepEqual(findJobsWithoutTimeout(text), [])
})

test('an empty corpus is a failure, not a pass', () => {
  assert.equal(renderVerdict({ files: [], offenders: [] }).ok, false)
})

test('every job in the real workflow tree declares timeout-minutes', () => {
  const result = findJobsWithoutTimeoutInRepo(repoRoot)
  assert.ok(result.files.length > 0, 'no workflow files read')
  assert.deepEqual(result.offenders, [])
})
