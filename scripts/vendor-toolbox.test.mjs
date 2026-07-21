import { test } from 'node:test'
import assert from 'node:assert/strict'
import { VENDOR_MAP, vendorToolbox } from './vendor-toolbox.mjs'
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  chmodSync,
  statSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

test('VENDOR_MAP routes each helper to the right skills', () => {
  assert.deepEqual(VENDOR_MAP['boss-epic'].sort(), [
    'bs-epic-lib.mjs',
    'bs-run-sentinel.mjs',
    'dag-scheduler.mjs',
  ])
  assert.ok(VENDOR_MAP['boss-plan'].includes('bs-run-sentinel.mjs'))
  assert.ok(VENDOR_MAP['boss-build'].includes('worktree-lock.sh'))
})

test('the review-specific helpers route only to boss-review (BOS-196)', () => {
  // detect/cross-review-lib/codex-review/claude-review are review-only: bundling
  // them into any other skill's toolbox would leak review machinery it never runs.
  // Assert exclusive boss-review routing.
  const reviewOnly = [
    'bs-review-detect.mjs',
    'cross-review-lib.mjs',
    'codex-review.mjs',
    'claude-review.mjs',
  ]
  for (const helper of reviewOnly) {
    assert.ok(VENDOR_MAP['boss-review'].includes(helper), `boss-review must vendor ${helper}`)
    for (const [skill, files] of Object.entries(VENDOR_MAP)) {
      if (skill === 'boss-review') continue
      assert.ok(!files.includes(helper), `${helper} must not leak into ${skill}`)
    }
  }
  // skill-config.mjs (BOS-192) is NOT review-exclusive: boss-review vendors it as a
  // transitive dep of bs-review-detect.mjs, and boss-build vendors it as a direct
  // dep of the Step 4 plan-contract check (validatePlanDescription, BOS-204). It must
  // still not leak into any skill that consumes neither.
  const skillConfigConsumers = new Set(['boss-review', 'boss-build', 'boss-plan'])
  for (const [skill, files] of Object.entries(VENDOR_MAP)) {
    if (skillConfigConsumers.has(skill)) {
      assert.ok(files.includes('skill-config.mjs'), `${skill} must vendor skill-config.mjs`)
    } else {
      assert.ok(!files.includes('skill-config.mjs'), `skill-config.mjs must not leak into ${skill}`)
    }
  }
  // All load-bearing review helpers are present in boss-review's toolbox.
  for (const helper of [
    'bs-review-caps.mjs',
    'bs-review-report.mjs',
    'skill-config.mjs',
    ...reviewOnly,
  ]) {
    assert.ok(VENDOR_MAP['boss-review'].includes(helper), `boss-review missing ${helper}`)
  }
})

// Every distinct filename any VENDOR_MAP skill needs — the engine reads each
// canonical source, so all must exist for a full run.
const ALL_SOURCES = [...new Set(Object.values(VENDOR_MAP).flat())]

test('vendorToolbox copies bytes and --check detects drift', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-'))
  const sourceRoot = join(root, 'skills-toolbox')
  const skillsRoot = join(root, 'skills')
  mkdirSync(sourceRoot, { recursive: true })
  for (const file of ALL_SOURCES) {
    writeFileSync(join(sourceRoot, file), `export const x = '${file}'\n`)
  }
  writeFileSync(join(sourceRoot, 'bs-run-sentinel.mjs'), 'export const x = 1\n')
  const write = vendorToolbox({ sourceRoot, skillsRoot, check: false })
  assert.equal(write.changed, true)
  const copied = readFileSync(
    join(skillsRoot, 'boss-plan', 'toolbox', 'bs-run-sentinel.mjs'),
    'utf8',
  )
  assert.equal(copied, 'export const x = 1\n')
  const clean = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(clean.changed, false)
  writeFileSync(join(skillsRoot, 'boss-plan', 'toolbox', 'bs-run-sentinel.mjs'), 'tampered\n')
  const drift = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(drift.changed, true)
  assert.ok(drift.differences.length > 0)
  rmSync(root, { recursive: true, force: true })
})

test('vendorToolbox --check skips when the skills root is stripped', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-'))
  const sourceRoot = join(root, 'skills-toolbox')
  const skillsRoot = join(root, 'skills') // deliberately never created (mirror strips .claude)
  mkdirSync(sourceRoot, { recursive: true })
  for (const file of ALL_SOURCES) {
    writeFileSync(join(sourceRoot, file), `export const x = '${file}'\n`)
  }
  const res = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(res.skipped, true)
  assert.equal(res.changed, false)
  assert.deepEqual(res.differences, [])
  rmSync(root, { recursive: true, force: true })
})

test('vendorToolbox preserves and --check detects executable-mode drift', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-'))
  const sourceRoot = join(root, 'skills-toolbox')
  const skillsRoot = join(root, 'skills')
  mkdirSync(sourceRoot, { recursive: true })
  for (const file of ALL_SOURCES) {
    writeFileSync(join(sourceRoot, file), `export const x = '${file}'\n`)
  }
  // worktree-lock.sh is invoked directly, so it ships +x; vendoring must preserve that bit.
  chmodSync(join(sourceRoot, 'worktree-lock.sh'), 0o755)
  vendorToolbox({ sourceRoot, skillsRoot, check: false })
  const dest = join(skillsRoot, 'boss-build', 'toolbox', 'worktree-lock.sh')
  assert.equal(statSync(dest).mode & 0o777, 0o755)
  const clean = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(clean.changed, false)

  // Content stays byte-identical, only the executable bit is lost.
  chmodSync(dest, 0o644)
  const drift = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(drift.changed, true)
  assert.ok(drift.differences.some((d) => d.startsWith('mode mismatch')))
  rmSync(root, { recursive: true, force: true })
})
