import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  findQuarantineProblems,
  gatherRepoInputs,
  parseBuildRules,
} from './check-flaky-quarantine.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

const FLAKY_BUILD = `go_library(
    name = "pkg",
    srcs = ["pkg.go"],
)

go_test(
    name = "pkg_test",
    srcs = ["pkg_test.go"],
    flaky = True,  # keep: quarantined
    embed = [":pkg"],
)
`
const PLAIN_BUILD = FLAKY_BUILD.replace('    flaky = True,  # keep: quarantined\n', '')
const TEST_SOURCE = 'package pkg\n\nfunc TestWobbly(t *testing.T) {\n}\n'

function entry(overrides = {}) {
  return {
    label: '//services/x/pkg:pkg_test',
    tests: ['TestWobbly'],
    owner: '@owner',
    tracking: 'https://example.invalid/issue/1',
    expires: '2026-11-17',
    reason: 'timer race under load',
    ...overrides,
  }
}

function problems({
  entries = [entry()],
  build = FLAKY_BUILD,
  sources = [TEST_SOURCE],
  ledger = [],
  today = '2026-10-18',
} = {}) {
  return findQuarantineProblems({
    entries,
    buildFiles: new Map([['services/x/pkg/BUILD.bazel', build]]),
    testSources: (pkg) => (pkg === 'services/x/pkg' ? sources : []),
    ledgerLabels: new Set(ledger),
    today,
  })
}

test('a complete, unexpired entry whose target carries flaky = True passes', () => {
  assert.deepEqual(problems(), [])
})

test('the expiry day itself still passes; the day after fails as expired', () => {
  assert.deepEqual(problems({ today: '2026-11-17' }), [])
  const out = problems({ today: '2026-11-18' })
  assert.equal(out.length, 1)
  assert.match(out[0], /EXPIRED on 2026-11-17/)
})

test('an expiry beyond the horizon fails, so an entry cannot be made permanent', () => {
  const out = problems({ entries: [entry({ expires: '2027-06-01' })] })
  assert.equal(out.length, 1)
  assert.match(out[0], /more than 60 days out/)
})

test('a malformed expiry date fails', () => {
  assert.match(
    problems({ entries: [entry({ expires: '2026-02-30' })] })[0],
    /not a YYYY-MM-DD date/,
  )
})

for (const field of ['owner', 'tracking', 'expires', 'reason']) {
  test(`an entry missing ${field} fails`, () => {
    const out = problems({ entries: [entry({ [field]: '' })] })
    assert.ok(
      out.some((line) => line.includes(`missing ${field}`)),
      `expected a missing-${field} problem, got ${JSON.stringify(out)}`,
    )
  })
}

test('an entry with no test names fails', () => {
  assert.ok(
    problems({ entries: [entry({ tests: [] })] }).some((line) => line.includes('missing tests')),
  )
})

test('a listed test that no longer exists in the package fails, naming it', () => {
  const out = problems({ entries: [entry({ tests: ['TestWobbly', 'TestRenamedAway'] })] })
  assert.equal(out.length, 1)
  assert.match(out[0], /TestRenamedAway not found/)
})

test('a listed target without flaky = True fails (Bazel would never retry it)', () => {
  const out = problems({ build: PLAIN_BUILD })
  assert.equal(out.length, 1)
  assert.match(out[0], /lacks `flaky = True/)
})

test('a ledger-only manual row needs no BUILD attribute', () => {
  assert.deepEqual(problems({ build: PLAIN_BUILD, ledger: ['//services/x/pkg:pkg_test'] }), [])
})

test('flaky = True with no list entry fails, naming the BUILD line', () => {
  const out = problems({ entries: [] })
  assert.equal(out.length, 1)
  assert.match(
    out[0],
    /^services\/x\/pkg\/BUILD\.bazel:6 \/\/services\/x\/pkg:pkg_test has flaky = True but no/,
  )
})

test('a label naming a rule that does not exist fails', () => {
  const out = problems({ entries: [entry({ label: '//services/x/pkg:gone_test' })] })
  assert.ok(out.some((line) => line.includes('no rule named gone_test')))
})

test('duplicate labels fail', () => {
  assert.ok(
    problems({ entries: [entry(), entry()] }).some((line) => line.includes('duplicate label')),
  )
})

test('parseBuildRules reads name and flaky per rule, not per file', () => {
  const rules = parseBuildRules(FLAKY_BUILD)
  assert.deepEqual(
    rules.map(({ kind, name, flaky }) => ({ kind, name, flaky })),
    [
      { kind: 'go_library', name: 'pkg', flaky: false },
      { kind: 'go_test', name: 'pkg_test', flaky: true },
    ],
  )
})

test('the checked-in quarantine agrees with the real tree today', () => {
  const inputs = gatherRepoInputs(repoRoot)
  assert.ok(
    inputs.buildFiles.size > 50,
    `expected the real BUILD corpus, read ${inputs.buildFiles.size}`,
  )
  assert.deepEqual(findQuarantineProblems({ ...inputs, today: '2026-10-18' }), [])
})

test('the CLI exits 1 and names the entry once the list has expired', () => {
  let failed = null
  try {
    execFileSync(
      'node',
      [path.join(repoRoot, 'scripts/check-flaky-quarantine.mjs'), '--today', '2099-01-01'],
      {
        encoding: 'utf8',
        stdio: 'pipe',
      },
    )
  } catch (error) {
    failed = error
  }
  const inputs = gatherRepoInputs(repoRoot)
  if (inputs.entries.length === 0) {
    assert.equal(failed, null, 'an empty quarantine cannot expire')
    return
  }
  assert.ok(failed, 'an expired quarantine must not exit 0')
  assert.equal(failed.status, 1)
  assert.match(String(failed.stderr), /EXPIRED/)
})

test('the CLI rejects unknown flags (strict parseArgs)', () => {
  assert.throws(
    () =>
      execFileSync(
        'node',
        [path.join(repoRoot, 'scripts/check-flaky-quarantine.mjs'), '--todya', '2026-10-18'],
        {
          stdio: 'pipe',
        },
      ),
    (error) => error.status !== 0,
  )
})
