import assert from 'node:assert/strict'
import { test } from 'node:test'
import { cpSync, existsSync, mkdirSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { spawnSync } from 'node:child_process'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'

function fixture() {
  const root = tempDir('plan-pin-step-')
  const skill = join(root, 'skills/boss-plan')
  const toolbox = join(skill, 'toolbox')
  mkdirSync(join(toolbox, 'plan/run-steps'), { recursive: true })
  for (const file of [
    'skill-pin.mjs',
    'toolbox-drift.mjs',
    'main-module.mjs',
    'plan/boss-plan-run.sh',
    'plan/plan-scratch-paths.mjs',
    'plan/plan-slug.mjs',
  ]) {
    cpSync(resolve('skills-toolbox', file), join(toolbox, file))
  }
  const step = resolve('skills-toolbox/plan/run-steps/pin.sh')
  if (existsSync(step)) cpSync(step, join(toolbox, 'plan/run-steps/pin.sh'))
  writeFileSync(join(skill, 'SKILL.md'), 'fixture instructions')
  writeFileSync(join(toolbox, 'original.mjs'), 'console.log("original helper")')
  mkdirSync(join(root, '.linear-plans/run-abcdefgh'), { recursive: true })
  const run = (scratch = '.linear-plans/run-abcdefgh') =>
    spawnSync('bash', [join(toolbox, 'plan/boss-plan-run.sh'), 'pin', '--run-scratch', scratch], {
      cwd: root,
      encoding: 'utf8',
    })
  return { root, skill, toolbox, run, pin: join(root, '.linear-plans/pin-abcdefgh/boss-plan') }
}

test('pin step captures the runner payload and keeps serving it after a peer install', () => {
  const f = fixture()
  const result = f.run()
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /^pin: .linear-plans\/pin-abcdefgh\/boss-plan\n/)
  assert.ok(existsSync(join(f.pin, '.boss-skill-pin.json')))
  rmSync(f.skill, { recursive: true, maxRetries: 3 })
  mkdirSync(f.skill)
  writeFileSync(join(f.skill, 'SKILL.md'), 'peer install without helper')
  const pinned = spawnSync(
    'bash',
    [join(f.pin, 'toolbox/plan/boss-plan-run.sh'), 'node', 'original.mjs'],
    { cwd: f.root, encoding: 'utf8' },
  )
  assert.equal(pinned.status, 0, pinned.stderr)
  assert.equal(pinned.stdout, 'original helper\n')
})

test('pin step rejects malformed scratch paths and a symlinked scratch root', () => {
  const f = fixture()
  for (const bad of [
    '.linear-plans',
    '.linear-plans/run-../other',
    '.linear-plans/run-abc/other',
    '.linear-plans/run-abc\\other',
  ]) {
    assert.equal(f.run(bad).status, 64, bad)
  }
  rmSync(join(f.root, '.linear-plans'), { recursive: true, maxRetries: 3 })
  mkdirSync(join(f.root, 'elsewhere'))
  symlinkSync(join(f.root, 'elsewhere'), join(f.root, '.linear-plans'))
  assert.equal(f.run().status, 64)
})

test('pin step reports a failed capture as BLOCKED', () => {
  const f = fixture()
  rmSync(join(f.skill, 'toolbox'), { recursive: true, maxRetries: 3 })
  const result = spawnSync(
    'bash',
    [
      resolve('skills-toolbox/plan/run-steps/pin.sh'),
      '--run-scratch',
      '.linear-plans/run-abcdefgh',
    ],
    {
      cwd: f.root,
      encoding: 'utf8',
      env: {
        ...process.env,
        BOSS_PLAN_TOOLBOX: resolve('skills-toolbox'),
        BOSS_SKILLS_HOME: join(f.root, 'skills'),
      },
    },
  )
  assert.equal(result.status, 1, result.stderr)
  assert.match(result.stdout + result.stderr, /BLOCKED/)
})
