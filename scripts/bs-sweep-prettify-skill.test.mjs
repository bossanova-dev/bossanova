// Behaviour tests for the bs-sweep-prettify drift gate (scripts/sweep-prettify-gate.mjs).

import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  detectDrift,
  parseGoFiles,
  chunk,
  GO_BATCH,
  BIOME_FORMAT_DRIFT_MARKER,
  prettierReportedDrift,
  syncpackReportedDrift,
  SYNCPACK_DRIFT_MARKER,
} from './sweep-prettify-gate.mjs'

// parseGoFiles is the fail-closed Go-file enumeration, tested by BEHAVIOR (not a
// source grep) so a rename or a dropped guard is caught. Both failure shapes of a
// broken `git ls-files` must throw; a clean result parses to a trimmed list.
test('parseGoFiles: ENOENT spawn error throws (fail closed)', () => {
  assert.throws(() => parseGoFiles({ error: new Error('no git') }), /git[ ]ls-files[ ]failed/)
})

test('parseGoFiles: non-zero exit throws (fail closed), not an empty list', () => {
  assert.throws(
    () => parseGoFiles({ status: 128, stdout: '' }),
    /git[ ]ls-files[ ]failed: exit[ ]128/,
  )
})

test('parseGoFiles: clean result parses to a trimmed, blank-filtered list', () => {
  assert.deepEqual(parseGoFiles({ status: 0, stdout: 'a.go\n b.go \n\n' }), ['a.go', 'b.go'])
})

// ---------------------------------------------------------------------------
// Gate behavior-parity — detectDrift runs the sweep exactly when a formatter
// reports drift, and fails closed when a required formatter cannot be spawned.
// A fake runner keyed by command stands in for the real execFileSync probes.
// ---------------------------------------------------------------------------

const SYNCPACK_LINT_KEY = 'pnpm syncpack lint'
const SYNCPACK_FORMAT_KEY = 'pnpm syncpack format --check'
const SCRIPTS_PRETTIER_KEY =
  'pnpm exec prettier --check scripts/*.{cjs,mjs} scripts/bazel/*.mjs scripts/changelog/*.{cjs,mjs} scripts/skill-parity/*.{cjs,mjs} skills-toolbox/*.mjs skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.{cjs,mjs} .claude/skills/*/gate/*.mjs'
const DOCS_PRETTIER_KEY = 'pnpm --dir services/docs run lint'
const WEB_BIOME_KEY = 'pnpm --dir services/web run lint'

const goFiles = ['a.go', 'b.go']

// Real prettier `--check` drift: exit 1 WITH its marker on stderr (pnpm forwards
// prettier's stderr). The marker is what proves this is drift, not a tooling failure.
const PRETTIER_DRIFT = {
  status: 1,
  stdout: 'Checking formatting...\n',
  stderr:
    '[warn] docs/x.md\n[warn] Code style issues found in the above file. Run Prettier with --write to fix.\n',
}
const SYNCPACK_DRIFT = {
  status: 1,
  stdout: '',
  stderr: `${SYNCPACK_DRIFT_MARKER} dependencies differ from syncpack policy\n`,
}
const BIOME_FORMAT_DRIFT = {
  status: 1,
  stdout: 'Checked 248 files in 208ms. No fixes applied.\nFound 1 error.\n',
  stderr: `example.ts format ━━━━━━━━━\n\n  × ${BIOME_FORMAT_DRIFT_MARKER} the following content:\n`,
}

test('syncpackReportedDrift matches syncpack findings marker only', () => {
  assert.equal(syncpackReportedDrift(SYNCPACK_DRIFT), true)
  assert.equal(syncpackReportedDrift({ status: 1, stderr: '✓ No issues found' }), false)
  assert.equal(syncpackReportedDrift({ status: 1, stderr: '✗ error: unexpected argument' }), false)
  assert.equal(syncpackReportedDrift({}), false)
})

for (const { name, key, label, reason, drift } of [
  {
    name: 'syncpack lint',
    key: SYNCPACK_LINT_KEY,
    label: 'syncpack lint',
    reason: 'syncpack',
    drift: SYNCPACK_DRIFT,
  },
  {
    name: 'syncpack format',
    key: SYNCPACK_FORMAT_KEY,
    label: 'syncpack format',
    reason: 'syncpack',
    drift: SYNCPACK_DRIFT,
  },
  {
    name: 'scripts prettier',
    key: SCRIPTS_PRETTIER_KEY,
    label: 'scripts prettier',
    reason: 'scripts-prettier',
    drift: PRETTIER_DRIFT,
  },
  {
    name: 'docs prettier',
    key: DOCS_PRETTIER_KEY,
    label: 'docs prettier',
    reason: 'docs-prettier',
    drift: PRETTIER_DRIFT,
  },
  {
    name: 'web biome',
    key: WEB_BIOME_KEY,
    label: 'web biome',
    reason: 'web-biome',
    drift: BIOME_FORMAT_DRIFT,
  },
]) {
  for (const status of [2, 127]) {
  }
}

// prettierReportedDrift is the pure predicate that disambiguates exit 1; test it
// directly on both stream carriers and both non-drift shapes.
test('prettierReportedDrift matches the --check marker on either stream, else false', () => {
  assert.equal(prettierReportedDrift({ stderr: 'Code style issues found in 2 files.' }), true)
  assert.equal(
    prettierReportedDrift({ stdout: 'Code style issues found in the above file.' }),
    true,
  )
  assert.equal(prettierReportedDrift({ stdout: 'Checking formatting...', stderr: '' }), false)
  assert.equal(prettierReportedDrift({ stderr: 'ELIFECYCLE  Command failed.' }), false)
  assert.equal(prettierReportedDrift({}), false)
})

test('detectDrift uses check-mode commands only', () => {
  const calls = []
  detectDrift(
    (cmd, args = []) => {
      calls.push([cmd, args])
      return { status: 0, stdout: '' }
    },
    { goFiles },
  )

  for (const [cmd, args] of calls) {
    assert.ok(!args.includes('--write'), `${cmd} ${args.join(' ')} must not write`)
    assert.ok(!args.includes('-w'), `${cmd} ${args.join(' ')} must not write`)
  }

  const syncpackCalls = calls.filter(([cmd, args]) => cmd === 'pnpm' && args[0] === 'syncpack')
  assert.deepEqual(syncpackCalls, [
    ['pnpm', ['syncpack', 'lint']],
    ['pnpm', ['syncpack', 'format', '--check']],
  ])
})

test('chunk batches Go files under GO_BATCH to stay within ARG_MAX', () => {
  const files = Array.from({ length: GO_BATCH * 2 + 5 }, (_, i) => `f${i}.go`)
  const batches = chunk(files)
  assert.equal(batches.length, 3)
  assert.ok(batches.every((b) => b.length <= GO_BATCH))
  assert.equal(batches.flat().length, files.length)
})
