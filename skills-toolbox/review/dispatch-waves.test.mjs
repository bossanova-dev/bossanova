import assert from 'node:assert/strict'
import { test } from 'node:test'
import { main, planWaves, resolveSlots, selectJobs } from './dispatch-waves.mjs'

const jobs = (severities) => severities.map((severity, i) => ({ id: `job-${i}`, severity }))

test('Codex admits twelve prioritized verifiers in four bounded waves', () => {
  const input = jobs([...Array(13).fill('P2'), 'P1', 'P1'])
  const result = planWaves({ host: 'codex', kind: 'verifier', jobs: input, env: {} })
  assert.deepEqual(result.waves, [
    ['job-13', 'job-14', 'job-0'],
    ['job-1', 'job-2', 'job-3'],
    ['job-4', 'job-5', 'job-6'],
    ['job-7', 'job-8', 'job-9'],
  ])
  assert.deepEqual(result.dropped, ['job-10', 'job-11', 'job-12'])
  assert.equal(result.slots, 3)
})

test('selection keeps every serious verifier, stable ties, and six resolvers', () => {
  const input = jobs(['P3', 'P1', 'P0', 'P2', 'P1', 'P2', 'P3'])
  assert.deepEqual(selectJobs('finder', input), { kept: input, dropped: [] })
  assert.deepEqual(
    selectJobs('resolver', input).kept.map((j) => j.id),
    ['job-2', 'job-1', 'job-4', 'job-3', 'job-5', 'job-0'],
  )
  const serious = jobs(Array(14).fill('P1'))
  assert.equal(selectJobs('verifier', serious).kept.length, 14)
  assert.deepEqual(planWaves({ host: 'claude', kind: 'finder', jobs: input, env: {} }).waves, [
    input.map((j) => j.id),
  ])
  assert.deepEqual(planWaves({ host: 'claude', kind: 'finder', jobs: [], env: {} }).waves, [])
})

test('slots use explicit, environment, then host defaults and reject unreadable values', () => {
  assert.equal(resolveSlots('codex', 9, '2', { BOSS_REVIEW_AGENT_SLOTS: '4' }), 2)
  for (const bad of ['0', '-1', '3.5', 'abc', '1e2', 'Infinity', '9007199254740992']) {
    assert.equal(resolveSlots('codex', 9, bad, { BOSS_REVIEW_AGENT_SLOTS: '4' }), 4)
    assert.equal(resolveSlots('codex', 9, bad, { BOSS_REVIEW_AGENT_SLOTS: bad }), 3)
  }
  assert.equal(resolveSlots('claude', 9, undefined, {}), 9)
})

test('CLI rejects malformed jobs and unknown options with exit 2', () => {
  const run = (args, data) => {
    let stdout = '',
      stderr = ''
    const code = main(args, {
      readFile: () => JSON.stringify(data),
      stdout: (s) => {
        stdout += s
      },
      stderr: (s) => {
        stderr += s
      },
      env: {},
    })
    return { code, stdout, stderr }
  }
  const args = ['plan', '--host', 'codex', '--kind', 'finder', '--jobs', '/jobs.json']
  for (const input of [
    {},
    [null],
    [{ id: '' }],
    [{ id: ' ' }],
    [{ id: 4 }],
    [{ id: 'x', severity: 'style' }],
    [{ id: 'x' }, { id: 'x' }],
  ]) {
    const result = run(args, input)
    assert.equal(result.code, 2)
    assert.ok(result.stderr)
    assert.equal(result.stdout, '')
  }
  assert.equal(run([...args, '--unknown'], []).code, 2)
  assert.equal(run(args, [{ id: 'x' }]).code, 0)
  assert.equal(
    main(args, {
      readFile: () => {
        throw new Error('unreadable')
      },
      stderr: () => {},
    }),
    2,
  )
})
