import assert from 'node:assert/strict'
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'

import { REPO_ROOT, computeAll } from './capture-baseline.mjs'
import {
  checkDeterministicParity,
  checkDirectoryCoverage,
  checkExtensionParity,
  main,
  runParityGate,
} from './parity-gate.mjs'

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

test('injected extension drift reports DRIFT for boss-build', async () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-ext-'))
  try {
    const srcSkills = join(REPO_ROOT, '.claude', 'skills')
    const dstSkills = join(tmp, '.claude', 'skills')
    // Copy only one of the two authored boss-build extensions (drop boss-build-knowledge).
    cpSync(join(srcSkills, 'boss-build-ce'), join(dstSkills, 'boss-build-ce'), { recursive: true })
    const { ok, findings } = await checkExtensionParity({ root: tmp })
    assert.equal(ok, false)
    const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-build-knowledge'))
    assert.ok(
      drift,
      `expected a DRIFT finding naming boss-build-knowledge, got: ${findings.join(' | ')}`,
    )
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('an unexpected role-mismatch skip reports DRIFT (typo not a known sibling)', async () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-role-'))
  try {
    // Start from the fully wired tree so every real spec resolves clean...
    cpSync(join(REPO_ROOT, '.claude', 'skills'), join(tmp, '.claude', 'skills'), {
      recursive: true,
    })
    // ...then add a same-prefix boss-build extension with a typo'd role. It is
    // NOT one of the known cross-role siblings, so its `role "methodolgy"` skip
    // must surface as drift rather than be silently swallowed.
    const typoDir = join(tmp, '.claude', 'skills', 'boss-build-typo')
    mkdirSync(typoDir, { recursive: true })
    writeFileSync(
      join(typoDir, 'SKILL.md'),
      [
        '---',
        'name: boss-build-typo',
        'description: typo fixture',
        'x-boss-extension:',
        '  extends: boss-build',
        '  role: methodolgy',
        '---',
        '',
        '# boss-build-typo',
        '',
      ].join('\n'),
    )
    const { ok, findings } = await checkExtensionParity({ root: tmp })
    assert.equal(ok, false)
    const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-build-typo'))
    assert.ok(
      drift,
      `expected a DRIFT finding naming boss-build-typo, got: ${findings.join(' | ')}`,
    )
    assert.match(drift, /unexpected skipped extension\(s\)/)
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

/**
 * Writes a synthetic `.claude/skills/<name>/SKILL.md` into a scratch tree.
 * @param {string} root scratch repo root
 * @param {string} name skill directory name
 * @param {string[]} markerLines the `x-boss-extension:` block, or [] for no marker at all
 */
function writeFixtureSkill(root, name, markerLines) {
  const dir = join(root, '.claude', 'skills', name)
  mkdirSync(dir, { recursive: true })
  writeFileSync(
    join(dir, 'SKILL.md'),
    [
      '---',
      `name: ${name}`,
      'description: fixture',
      ...markerLines,
      '---',
      '',
      `# ${name}`,
      '',
    ].join('\n'),
  )
}

// BOS-852: the case ONLY the directory-coverage assertion catches: a valid-but-unspecced KNOWN
// role is silently `continue`d by discovery, so it never reaches `skipped` for badSkips to flag
// and never reaches `extensions` for the names check to miss. Stub checkDirectoryCoverage out and
// this test must go red.
test('a boss-build directory with a valid-but-unspecced known role reports DRIFT', async () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-cover-'))
  try {
    cpSync(join(REPO_ROOT, '.claude', 'skills'), join(tmp, '.claude', 'skills'), {
      recursive: true,
    })
    writeFixtureSkill(tmp, 'boss-build-fixture', [
      'x-boss-extension:',
      '  extends: boss-build',
      '  role: surface',
      '  order: 90',
    ])
    const { ok, findings } = await runParityGate({ root: tmp })
    assert.equal(ok, false, 'a directory no spec claims must not be invisible to the gate')
    const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-build-fixture'))
    assert.ok(
      drift,
      `expected a DRIFT finding naming boss-build-fixture, got: ${findings.join(' | ')}`,
    )
    assert.match(drift, /not claimed by any EXPECTED_EXTENSIONS spec/)
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

// badSkips carries what discovery cannot re-derive — a failed declaration of a real extension.
test('a boss-build directory with no x-boss-extension marker still reports DRIFT', async () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-marker-'))
  try {
    cpSync(join(REPO_ROOT, '.claude', 'skills'), join(tmp, '.claude', 'skills'), {
      recursive: true,
    })
    writeFixtureSkill(tmp, 'boss-build-fixture', [])
    const { ok, findings } = await checkExtensionParity({ root: tmp })
    assert.equal(ok, false)
    const drift = findings.find((f) => f.includes('DRIFT') && f.includes('boss-build-fixture'))
    assert.ok(
      drift,
      `expected a DRIFT finding naming boss-build-fixture, got: ${findings.join(' | ')}`,
    )
    assert.match(drift, /unexpected skipped extension\(s\)/)
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

// BOS-852: the coverage check reads a directory the gate cannot assume exists. A checkout with
// no repo-local extensions at all must stay green, exactly as discoverExtensions no-ops there —
// throwing an ENOENT would turn "nothing installed" into a gate failure.
test('checkDirectoryCoverage no-ops when .claude/skills is absent', () => {
  const tmp = mkdtempSync(join(tmpdir(), 'parity-empty-'))
  try {
    const { ok, findings } = checkDirectoryCoverage({ root: tmp })
    assert.equal(ok, true)
    assert.deepEqual(findings, [])
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('main returns 0 on the wired tree', async () => {
  assert.equal(await main(), 0)
})

test('CLI entrypoint lets stdout drain by avoiding process.exit()', () => {
  const source = readFileSync(
    join(REPO_ROOT, 'scripts', 'skills', 'skill-parity', 'parity-gate.mjs'),
    'utf8',
  )
  assert.equal(source.includes('process.exit('), false)
  assert.equal(source.includes('process.exitCode'), true)
})
