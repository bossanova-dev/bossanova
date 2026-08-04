import assert from 'node:assert/strict'
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'

import { REPO_ROOT, computeAll } from './capture-baseline.mjs'
import {
  checkDeterministicParity,
  checkExtensionParity,
  isValidDriverShape,
  main,
  runParityGate,
} from './parity-gate.mjs'
import { ROLE_SCHEMAS } from '../../skills-toolbox/skill-extensions.mjs'

test('runParityGate is green on the wired-correct tree', async () => {
  const { ok, findings } = await runParityGate()
  assert.equal(ok, true, `expected green, got findings: ${findings.join(' | ')}`)
  assert.deepEqual(findings, [])
})

test('injected deterministic drift reports DRIFT for boss-epic', () => {
  const snapshots = structuredClone(computeAll())
  // Reorder the dag-schedule readyIds — order IS the signature, so any reorder drifts.
  snapshots['dag-schedule.snapshot.json'].readyIds.reverse()
  const { ok, findings } = checkDeterministicParity({ snapshots })
  assert.equal(ok, false)
  const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-epic'))
  assert.ok(drift, `expected a DRIFT finding naming boss-epic, got: ${findings.join(' | ')}`)
})

test('injected extension drift reports DRIFT for boss-review', async () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-ext-'))
  try {
    const srcSkills = join(REPO_ROOT, '.claude', 'skills')
    const dstSkills = join(tmp, '.claude', 'skills')
    // Copy only two of the three boss-review lens extensions (drop boss-review-web).
    for (const name of ['boss-review-golang', 'boss-review-tui']) {
      cpSync(join(srcSkills, name), join(dstSkills, name), { recursive: true })
    }
    const { ok, findings } = await checkExtensionParity({ root: tmp })
    assert.equal(ok, false)
    const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-review'))
    assert.ok(drift, `expected a DRIFT finding naming boss-review, got: ${findings.join(' | ')}`)
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

// BOS-678: dropping a lens extension's `lens:` marker line unwires it from boss-review Phase 1
// while discovery still returns the descriptor, so the names check above stays green. Only the
// binding assertion catches it — this is the "verified by temporarily removing one `lens:` marker
// line" check from the ticket's acceptance criteria, run mechanically against a scratch copy.
test('a lens extension that lost its lens binding reports DRIFT', async () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-bind-'))
  try {
    cpSync(join(REPO_ROOT, '.claude', 'skills'), join(tmp, '.claude', 'skills'), {
      recursive: true,
    })
    const victim = join(tmp, '.claude', 'skills', 'boss-review-tui', 'SKILL.md')
    const before = readFileSync(victim, 'utf8')
    assert.match(
      before,
      /^ {2}lens: tui$/m,
      'fixture precondition: the marker declares its binding',
    )
    writeFileSync(victim, before.replace(/^ {2}lens: tui\n/m, ''))

    const { ok, findings } = await checkExtensionParity({ root: tmp })

    assert.equal(ok, false)
    const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-review-tui'))
    assert.ok(
      drift,
      `expected a DRIFT finding naming boss-review-tui, got: ${findings.join(' | ')}`,
    )
    assert.match(drift, /declares lens \(none\), expected "tui"/)
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('a lens extension whose binding names the wrong lens id reports DRIFT', async () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-bind-'))
  try {
    cpSync(join(REPO_ROOT, '.claude', 'skills'), join(tmp, '.claude', 'skills'), {
      recursive: true,
    })
    const victim = join(tmp, '.claude', 'skills', 'boss-review-web', 'SKILL.md')
    const before = readFileSync(victim, 'utf8')
    writeFileSync(victim, before.replace(/^ {2}lens: web$/m, '  lens: wbe'))

    const { ok, findings } = await checkExtensionParity({ root: tmp })

    assert.equal(ok, false)
    const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-review-web'))
    assert.ok(
      drift,
      `expected a DRIFT finding naming boss-review-web, got: ${findings.join(' | ')}`,
    )
    assert.match(drift, /declares lens "wbe", expected "web"/)
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('ROLE_SCHEMAS covers lens, round, and surface; driver-shape predicate accepts/rejects', () => {
  assert.ok(ROLE_SCHEMAS.lens, 'ROLE_SCHEMAS.lens should be defined')
  assert.ok(ROLE_SCHEMAS.round, 'ROLE_SCHEMAS.round should be defined')
  assert.ok(ROLE_SCHEMAS.surface, 'ROLE_SCHEMAS.surface should be defined')
  const wellFormed = {
    surface: 'x',
    budgetClass: 'x',
    usable: () => true,
    preflightMissing: () => [],
    run: () => {},
  }
  assert.equal(isValidDriverShape(wellFormed), true)
  const { run, ...missingRun } = wellFormed
  assert.equal(isValidDriverShape(missingRun), false)
  // prebuild is optional but, when present, must be a function — mirror the real
  // isValidDriverRecord contract in proof-agent-drivers.mjs so a typo'd hook
  // (e.g. prebuild: true) that the loader would reject cannot pass the gate.
  assert.equal(isValidDriverShape({ ...wellFormed, prebuild: () => {} }), true)
  assert.equal(isValidDriverShape({ ...wellFormed, prebuild: true }), false)
})

test('an unexpected role-mismatch skip reports DRIFT (typo not a known sibling)', async () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-role-'))
  try {
    // Start from the fully wired tree so every real spec resolves clean...
    cpSync(join(REPO_ROOT, '.claude', 'skills'), join(tmp, '.claude', 'skills'), {
      recursive: true,
    })
    // ...then add a same-prefix boss-proof extension with a typo'd role. It is
    // NOT one of the known cross-role siblings, so its `role "surfcae"` skip
    // must surface as drift rather than be silently swallowed.
    const typoDir = join(tmp, '.claude', 'skills', 'boss-proof-typo')
    mkdirSync(typoDir, { recursive: true })
    writeFileSync(
      join(typoDir, 'SKILL.md'),
      [
        '---',
        'name: boss-proof-typo',
        'description: typo fixture',
        'x-boss-extension:',
        '  extends: boss-proof',
        '  role: surfcae',
        '---',
        '',
        '# boss-proof-typo',
        '',
      ].join('\n'),
    )
    const { ok, findings } = await checkExtensionParity({ root: tmp })
    assert.equal(ok, false)
    const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-proof-typo'))
    assert.ok(
      drift,
      `expected a DRIFT finding naming boss-proof-typo, got: ${findings.join(' | ')}`,
    )
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('main returns 0 on the wired tree', async () => {
  assert.equal(await main(), 0)
})

test('CLI entrypoint lets stdout drain by avoiding process.exit()', () => {
  const source = readFileSync(join(REPO_ROOT, 'scripts', 'skill-parity', 'parity-gate.mjs'), 'utf8')
  assert.equal(source.includes('process.exit('), false)
  assert.equal(source.includes('process.exitCode'), true)
})
