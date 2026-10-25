import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import test from 'node:test'

import { fixture, cli } from './docs-guard-fixture.mjs'

function guideFixture(t) {
  const root = fixture(t, {
    'scripts/guards/budgets.json': JSON.stringify({
      'part-guides': { default: 32, subjects: { 'services/example/AGENTS.md': 32 } },
    }),
    'AGENTS.md': 'Root is outside this guard.\n'.repeat(10),
    'services/example/AGENTS.md': 'Part guide.\n',
  })
  fs.symlinkSync('AGENTS.md', path.join(root, 'services/example/CLAUDE.md'))
  execFileSync('git', ['add', '--', 'services/example/CLAUDE.md'], { cwd: root })
  return root
}

const run = (root) => cli('check-part-guides', root)

test('part-guides rejects seeded byte growth and passes at budget', (t) => {
  const root = guideFixture(t)
  fs.writeFileSync(path.join(root, 'services/example/AGENTS.md'), 'é'.repeat(17))
  const red = run(root)
  assert.equal(red.status, 1, red.stderr)
  assert.match(red.stderr, /services\/example\/AGENTS.md: 34 is over its budget of 32/)
  assert.match(red.stderr, /scripts\/guards\/budgets.json/)
  fs.writeFileSync(path.join(root, 'services/example/AGENTS.md'), 'é'.repeat(16))
  assert.equal(run(root).status, 0)
})

test('part-guides rejects a missing budgeted guide even when removed from git', (t) => {
  const root = guideFixture(t)
  execFileSync('git', ['rm', '-f', '--', 'services/example/AGENTS.md'], { cwd: root })
  const red = run(root)
  assert.equal(red.status, 1)
  assert.match(red.stderr, /AGENTS.md has a budget but no measurement/)
  assert.match(red.stderr, /CLAUDE.md.*no sibling AGENTS.md/)
})

test('part-guides requires both filesystem and tracked symlink modes', (t) => {
  const root = guideFixture(t)
  const sibling = path.join(root, 'services/example/CLAUDE.md')
  fs.unlinkSync(sibling)
  fs.writeFileSync(sibling, 'Part guide.\n')
  assert.match(run(root).stderr, /CLAUDE.md.*must be a symlink/)
  execFileSync('git', ['add', '--', 'services/example/CLAUDE.md'], { cwd: root })
  fs.unlinkSync(sibling)
  fs.symlinkSync('AGENTS.md', sibling)
  assert.match(run(root).stderr, /CLAUDE.md.*tracked mode.*120000/)
  execFileSync('git', ['add', '--', 'services/example/CLAUDE.md'], { cwd: root })
  assert.equal(run(root).status, 0)
})

test('part-guides rejects absent, untracked and wrongly targeted siblings', (t) => {
  const root = guideFixture(t)
  const sibling = path.join(root, 'services/example/CLAUDE.md')
  fs.unlinkSync(sibling)
  assert.match(run(root).stderr, /CLAUDE.md.*missing/)
  execFileSync('git', ['rm', '--cached', '--', 'services/example/CLAUDE.md'], { cwd: root })
  fs.symlinkSync('AGENTS.md', sibling)
  assert.match(run(root).stderr, /CLAUDE.md.*tracked mode.*120000/)
  fs.unlinkSync(sibling)
  fs.symlinkSync('./AGENTS.md', sibling)
  execFileSync('git', ['add', '--', 'services/example/CLAUDE.md'], { cwd: root })
  assert.match(run(root).stderr, /CLAUDE.md.*target.*exactly AGENTS.md/)
})

test('part-guides rejects symlinked or missing AGENTS files and caps new guides', (t) => {
  const root = guideFixture(t)
  const guide = path.join(root, 'services/example/AGENTS.md')
  fs.unlinkSync(guide)
  fs.symlinkSync('../../AGENTS.md', guide)
  assert.match(run(root).stderr, /AGENTS.md.*regular file/)
  fs.unlinkSync(guide)
  assert.match(run(root).stderr, /AGENTS.md.*missing/)
  fs.writeFileSync(guide, 'Part guide.\n')
  fs.mkdirSync(path.join(root, 'plugins'))
  fs.writeFileSync(path.join(root, 'plugins/AGENTS.md'), 'x'.repeat(33))
  fs.symlinkSync('AGENTS.md', path.join(root, 'plugins/CLAUDE.md'))
  execFileSync('git', ['add', '--', 'plugins'], { cwd: root })
  assert.match(run(root).stderr, /plugins\/AGENTS.md: 33 is over the default budget of 32/)
})
