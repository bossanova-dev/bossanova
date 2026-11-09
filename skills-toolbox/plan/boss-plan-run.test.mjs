import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  cpSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
  existsSync,
  realpathSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { spawnSync } from 'node:child_process'

function fixture(t) {
  const root = realpathSync(mkdtempSync(join(tmpdir(), 'boss-plan-run-test-')))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const toolbox = join(root, 'skills/boss-plan/toolbox')
  mkdirSync(join(toolbox, 'plan'), { recursive: true })
  for (const path of [
    'plan/boss-plan-run.sh',
    'plan/run-steps',
    'plan/plan-scratch-paths.mjs',
    'main-module.mjs',
    'bs-run-sentinel.mjs',
  ]) {
    cpSync(resolve('skills-toolbox', path), join(toolbox, path), { recursive: true })
  }
  writeFileSync(
    join(toolbox, 'plan/echo-args.mjs'),
    'console.log(JSON.stringify({args:process.argv.slice(2),toolbox:process.env.BOSS_PLAN_TOOLBOX,home:process.env.BOSS_SKILLS_HOME}));process.exit(Number(process.env.HELPER_STATUS || 0))',
  )
  writeFileSync(
    join(toolbox, 'bs-dispatch-await.mjs'),
    'console.log(JSON.stringify(process.argv.slice(2)));process.exit(Number(process.env.HELPER_STATUS || 0))',
  )
  const run = (...args) =>
    spawnSync('bash', [join(toolbox, 'plan/boss-plan-run.sh'), ...args], {
      cwd: root,
      encoding: 'utf8',
      env: { ...process.env, BOSS_SKILLS_HOME: '/ignored', TMPDIR: root },
    })
  return { root, toolbox, run }
}

test('runner locates its own toolbox through symlinks and forwards literal argv and status', (t) => {
  const { root, toolbox, run } = fixture(t)
  const args = ['space here', '"quoted"', '$literal']
  const result = run('node', 'plan/echo-args.mjs', ...args)
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(result.stdout), { args, toolbox, home: join(root, 'skills') })
  symlinkSync(join(toolbox, 'plan/boss-plan-run.sh'), join(root, 'runner'))
  const linked = spawnSync('bash', [join(root, 'runner'), 'node', 'plan/echo-args.mjs', ...args], {
    cwd: root,
    encoding: 'utf8',
    env: { ...process.env, HELPER_STATUS: '23' },
  })
  assert.equal(linked.status, 23, linked.stderr)
  assert.equal(linked.stdout, result.stdout)
})

test('runner rejects invalid helpers and steps without executing anything', (t) => {
  const { run } = fixture(t)
  for (const args of [
    [],
    ['unknown'],
    ['../cleanup'],
    ['node'],
    ['node', '../x.mjs'],
    ['node', '/abs.mjs'],
    ['node', 'plan/x.js'],
  ]) {
    const result = run(...args)
    assert.equal(result.status, 64, JSON.stringify(args))
    assert.equal(result.stdout, '')
  }
  const missing = run('node', 'plan/missing.mjs')
  assert.equal(missing.status, 1)
  assert.match(missing.stdout, /^BLOCKED:/)
  assert.equal(
    run('steps').stdout,
    'cleanup\ncontract-gate\ndeps\ndispatch-mark\ndispatch-wait\nimage-gates\nopen-prs\npremise-repass\npremises\nread-base\nsentinel-ctx\nwriteback-verify\n',
  )
})

test('cleanup confines removal to a single run scratch directory', (t) => {
  const { root, run } = fixture(t)
  mkdirSync(join(root, '.linear-plans/run-XXXXXXXX'), { recursive: true })
  writeFileSync(join(root, '.linear-plans/keep'), 'keep')
  for (const path of ['.linear-plans', '..', root, '.linear-plans/run-../keep']) {
    assert.equal(run('cleanup', '--run-scratch', path).status, 64)
    assert.equal(existsSync(join(root, '.linear-plans/keep')), true)
  }
  assert.equal(run('cleanup', '--run-scratch', '.linear-plans/run-XXXXXXXX').status, 0)
  assert.equal(existsSync(join(root, '.linear-plans/run-XXXXXXXX')), false)
  assert.equal(run('cleanup', '--run-scratch', '.linear-plans/run-XXXXXXXX').status, 0)
})

test('sentinel context reports the real sentinel and created run directory', (t) => {
  const { toolbox, run } = fixture(t)
  const result = run('sentinel-ctx')
  assert.equal(result.status, 0, result.stderr)
  const lines = Object.fromEntries(
    result.stdout
      .trim()
      .split('\n')
      .map((line) => line.split('=')),
  )
  assert.equal(lines.RUN_SENTINEL, join(toolbox, 'bs-run-sentinel.mjs'))
  assert.ok(lines.RUN_ID)
  assert.equal(existsSync(lines.RUN_DIR), true)
})

test('dispatch steps preserve recorded timestamp, budget and helper exit status', (t) => {
  const { root, run, toolbox } = fixture(t)
  assert.equal(run('dispatch-mark', '--run-dir', join(root, 'missing')).status, 64)
  assert.equal(
    run('dispatch-wait', '--run-dir', root, '--run-id', 'id', '--heartbeat', 'beat').status,
    64,
  )
  assert.equal(run('dispatch-mark', '--run-dir', root).status, 0)
  const timestamp = readFileSync(join(root, 'draft.dispatched-at'), 'utf8').trim()
  assert.match(timestamp, /^\d+$/)
  const result = run(
    'dispatch-wait',
    '--run-dir',
    root,
    '--run-id',
    'id',
    '--heartbeat',
    'beat',
    '--budget',
    '250',
  )
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(result.stdout), [
    'wait',
    root,
    'id',
    'draft',
    '--heartbeat',
    'beat',
    '--dispatched-at',
    timestamp,
    '--while-live',
    '--budget',
    '250',
  ])
  const status = spawnSync(
    'bash',
    [
      join(toolbox, 'plan/boss-plan-run.sh'),
      'dispatch-wait',
      '--run-dir',
      root,
      '--run-id',
      'id',
      '--heartbeat',
      'beat',
    ],
    { encoding: 'utf8', env: { ...process.env, HELPER_STATUS: '98' } },
  )
  assert.equal(status.status, 98)
  for (const step of ['cleanup', 'dispatch-mark', 'dispatch-wait', 'sentinel-ctx']) {
    assert.equal(run(step, '--bad', 'value').status, 64)
    assert.equal(run(step, '--run-dir').status, 64)
  }
})

for (const shell of ['zsh', 'fish']) {
  test(`one-line call has identical output under ${shell}`, (t) => {
    const available = spawnSync(shell, ['--version'], { encoding: 'utf8' })
    if (available.error) return t.skip(`${shell} is not installed`)
    const { toolbox, run } = fixture(t)
    const result = spawnSync(
      shell,
      ['-c', `bash '${join(toolbox, 'plan/boss-plan-run.sh')}' steps`],
      { encoding: 'utf8' },
    )
    assert.equal(result.status, 0, result.stderr)
    assert.equal(result.stdout, run('steps').stdout)
  })
}
