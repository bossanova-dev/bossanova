import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { tierForEstimate, planBodyBytes } from './plan-draft-tier.mjs'

const helper = resolve('skills-toolbox/plan/plan-draft-tier.mjs')
function fixture() {
  const scratch = tempDir('draft-tier-')
  const plan = join(scratch, 'ABC-1-plan.md')
  writeFileSync(plan, 'A small plan.\n')
  const run = (cmd, estimate = 3) =>
    spawnSync(
      process.execPath,
      [
        helper,
        cmd,
        '--run-scratch',
        scratch,
        '--issue',
        'ABC-1',
        '--estimate',
        String(estimate),
        ...(cmd === 'check' ? ['--plan', plan] : []),
      ],
      { encoding: 'utf8' },
    )
  return { scratch, plan, run }
}

test('estimates select the bounded drafting tiers', () => {
  for (const [n, tier] of [
    [0, 'light'],
    [1, 'light'],
    [2, 'standard'],
    [3, 'standard'],
    [5, 'full'],
    [8, 'full'],
  ]) {
    assert.equal(tierForEstimate(n), tier)
  }
})

test('selection records every re-triage and exposes only the chosen tier', () => {
  const f = fixture()
  for (const [n, tier] of [
    [1, 'light'],
    [3, 'standard'],
    [5, 'full'],
  ]) {
    const r = f.run('select', n)
    assert.equal(r.status, 0, r.stderr)
    const output = JSON.parse(r.stdout)
    assert.equal(output.tier, tier)
    assert.equal(typeof output.recon, 'string')
    assert.ok('testRuns' in output)
    assert.ok('planBytes' in output)
  }
  const record = JSON.parse(readFileSync(join(f.scratch, 'ABC-1.draft-tier.json'), 'utf8'))
  assert.equal(record.estimate, 5)
  assert.equal(record.tier, 'full')
  assert.deepEqual(
    record.history.map(({ estimate, tier }) => [estimate, tier]),
    [
      [1, 'light'],
      [3, 'standard'],
      [5, 'full'],
    ],
  )
  assert.ok(record.history.every(({ at }) => Number.isFinite(Date.parse(at))))
})

test('check refuses missing and stale tiers and over-budget plans', () => {
  const f = fixture()
  assert.match(f.run('check').stderr, /draft-tier-missing/)
  assert.notEqual(f.run('check').status, 0)
  assert.equal(f.run('select', 1).status, 0)
  assert.match(f.run('check', 3).stderr, /draft-tier-stale/)
  assert.notEqual(f.run('check', 3).status, 0)
  writeFileSync(f.plan, 'x'.repeat(8192))
  assert.equal(f.run('check', 1).status, 0)
  writeFileSync(f.plan, 'x'.repeat(8193))
  const over = f.run('check', 1)
  assert.notEqual(over.status, 0)
  assert.match(over.stderr, /plan-over-tier-budget.*8193.*8192/)
  f.run('select', 3)
  writeFileSync(f.plan, 'x'.repeat(24576))
  assert.equal(f.run('check', 3).status, 0)
  writeFileSync(f.plan, 'x'.repeat(24577))
  assert.notEqual(f.run('check', 3).status, 0)
  f.run('select', 5)
  assert.equal(f.run('check', 5).status, 0)
})

test('body measurement excludes only a line-anchored original-notes heading', () => {
  const body = 'é: inline ## Original notes is still body.\n'
  assert.equal(planBodyBytes(body), Buffer.byteLength(body))
  assert.equal(
    planBodyBytes(body + '## Original notes\r\n' + 'x'.repeat(30000)),
    Buffer.byteLength(body),
  )
  const f = fixture()
  f.run('select', 1)
  writeFileSync(f.plan, body + '## Original notes\n' + 'x'.repeat(30000))
  assert.equal(f.run('check', 1).status, 0)
  writeFileSync(f.plan, 'inline ## Original notes\n' + 'x'.repeat(8193) + '\n## Original notes\n')
  assert.notEqual(f.run('check', 1).status, 0)
})

test('fenced template headings count toward the plan body budget', () => {
  const f = fixture()
  f.run('select', 1)
  for (const [opening, closing, eol] of [
    ['```markdown', '```', '\n'],
    ['~~~markdown', '~~~~', '\n'],
    ['```markdown', '```', '\r\n'],
  ]) {
    const body = [opening, '## Original notes', closing, 'x'.repeat(8193), ''].join(eol)
    assert.equal(planBodyBytes(body), Buffer.byteLength(body))
    assert.equal(
      planBodyBytes(body + '## Original notes' + eol + 'Reporter notes.'),
      Buffer.byteLength(body),
    )
    writeFileSync(f.plan, body + '## Original notes' + eol + 'Reporter notes.')
    const result = f.run('check', 1)
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, /plan-over-tier-budget/)
  }
})
