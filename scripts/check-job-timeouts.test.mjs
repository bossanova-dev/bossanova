import assert from 'node:assert/strict'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  findJobsWithOverrunStepCaps,
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

test('step caps that reach the job cap are reported with both sums', () => {
  const text = workflow([
    '  nightly:',
    '    runs-on: ubicloud',
    '    timeout-minutes: 30',
    '    steps:',
    '      - uses: actions/checkout@0000000000000000000000000000000000000000',
    '      - name: Ledger',
    '        timeout-minutes: 20',
    '        run: make ledger',
    '      - name: Sweeps',
    '        run: |',
    '          timeout-minutes: 99',
    '        timeout-minutes: 10',
    '  fits:',
    '    runs-on: ubicloud',
    '    timeout-minutes: 31',
    '    steps:',
    '    - timeout-minutes: 20',
    '      run: make ledger',
    '    - run: make sweeps',
    '      timeout-minutes: 10',
    '    env:',
    '      timeout-minutes: 50',
  ])
  assert.deepEqual(findJobsWithOverrunStepCaps(text), [
    { job: 'nightly', line: 4, jobMinutes: 30, stepMinutes: 30 },
  ])
})

test('an expression cap on the job or a step is not added up', () => {
  const text = workflow([
    '  smoke:',
    "    timeout-minutes: ${{ matrix.pass == 'race' && 50 || 35 }}",
    '    steps:',
    '      - run: make',
    '        timeout-minutes: 60',
    '  nightly:',
    '    timeout-minutes: 10',
    '    steps:',
    '      - run: make',
    '        timeout-minutes: ${{ inputs.cap }}',
  ])
  assert.deepEqual(findJobsWithOverrunStepCaps(text), [])
})

test('an overrun alone fails the verdict and names the job', () => {
  const file = '.github/workflows/x.yml'
  const verdict = renderVerdict({
    files: [file],
    offenders: [],
    overruns: [{ file, line: 4, job: 'nightly', jobMinutes: 30, stepMinutes: 30 }],
  })
  assert.equal(verdict.ok, false)
  assert.match(verdict.text, /x\.yml:4 job `nightly`: steps 30 min >= job 30 min/)
})

test('an empty corpus is a failure, not a pass', () => {
  assert.equal(renderVerdict({ files: [], offenders: [] }).ok, false)
})

test('every job in the real workflow tree declares timeout-minutes', () => {
  const result = findJobsWithoutTimeoutInRepo(repoRoot)
  assert.ok(result.files.length > 0, 'no workflow files read')
  assert.deepEqual(result.offenders, [])
  assert.deepEqual(result.overruns, [])
})
