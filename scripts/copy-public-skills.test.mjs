import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  existsSync,
  chmodSync,
  statSync,
  rmSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PUBLIC_SKILLS, BUILD_INPUTS, copyPublicSkills } from './copy-public-skills.mjs'

const DEST_REL = 'services/boss/internal/skillinstall/skills'

function seed(root) {
  for (const s of PUBLIC_SKILLS) {
    const src = join(root, '.claude', 'skills', s)
    mkdirSync(join(src, 'references'), { recursive: true })
    mkdirSync(join(src, 'toolbox'), { recursive: true })
    writeFileSync(join(src, 'SKILL.md'), `---\nname: ${s}\n---\nbody`)
    writeFileSync(join(src, 'construct.json'), '{}')
    writeFileSync(join(src, 'SKILL.md.tmpl'), 'tmpl')
    writeFileSync(join(src, 'references', 'a.md'), 'ref')
    writeFileSync(join(src, 'toolbox', 'lock.sh'), '#!/bin/sh\n')
    chmodSync(join(src, 'toolbox', 'lock.sh'), 0o755)
  }
}

test('copies constructed skills, strips build inputs, preserves +x', () => {
  const root = mkdtempSync(join(tmpdir(), 'pubskills-'))
  seed(root)
  copyPublicSkills({ rootDir: root, check: false })
  const dest = join(root, DEST_REL)
  for (const s of PUBLIC_SKILLS) {
    assert.ok(existsSync(join(dest, s, 'SKILL.md')))
    assert.ok(existsSync(join(dest, s, 'references', 'a.md')))
    assert.equal(statSync(join(dest, s, 'toolbox', 'lock.sh')).mode & 0o777, 0o755)
    for (const input of BUILD_INPUTS) {
      assert.ok(!existsSync(join(dest, s, input)), `${input} must not ship`)
    }
  }
  // A clean copy passes --check without throwing.
  copyPublicSkills({ rootDir: root, check: true })
  rmSync(root, { recursive: true, force: true })
})

test('--check skips when .claude/skills is stripped (public mirror)', () => {
  const root = mkdtempSync(join(tmpdir(), 'pubskills-'))
  // Deliberately never seed .claude/skills — mirror-public.yml strips it in the
  // public checkout, and `make test` there must not ENOENT on copy-public-skills-check.
  assert.doesNotThrow(() => copyPublicSkills({ rootDir: root, check: true }))
  assert.ok(!existsSync(join(root, DEST_REL)))
  rmSync(root, { recursive: true, force: true })
})

test('--check throws when the embedded copy is stale', () => {
  const root = mkdtempSync(join(tmpdir(), 'pubskills-'))
  seed(root)
  copyPublicSkills({ rootDir: root, check: false })
  writeFileSync(join(root, DEST_REL, PUBLIC_SKILLS[0], 'SKILL.md'), 'DRIFTED')
  assert.throws(() => copyPublicSkills({ rootDir: root, check: true }), /stale|drift/i)
  rmSync(root, { recursive: true, force: true })
})

test('--check throws when a build input leaked into the embedded tree', () => {
  const root = mkdtempSync(join(tmpdir(), 'pubskills-'))
  seed(root)
  copyPublicSkills({ rootDir: root, check: false })
  // Simulate a stale/hand-copied build input under the shipped tree. Write mode
  // never emits these, so --check must catch it rather than silently skipping it.
  const [firstInput] = BUILD_INPUTS
  writeFileSync(join(root, DEST_REL, PUBLIC_SKILLS[0], firstInput), 'leaked')
  assert.throws(() => copyPublicSkills({ rootDir: root, check: true }), /stale|drift/i)
  rmSync(root, { recursive: true, force: true })
})
