import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import { execFileSync, spawnSync } from 'node:child_process'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  findQuarantineProblems,
  gatherRepoInputs,
  parseBuildRules,
} from './check-flaky-quarantine.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

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

test('native ledger retry labels load without installed Node dependencies', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'native-quarantine-import-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  for (const file of [
    'scripts/guards/check-flaky-quarantine.mjs',
    'scripts/guards/web-quarantine.mjs',
    'skills-toolbox/main-module.mjs',
  ]) {
    const destination = path.join(root, file)
    fs.mkdirSync(path.dirname(destination), { recursive: true })
    fs.copyFileSync(path.join(repoRoot, file), destination)
  }
  const result = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      `
      import assert from 'node:assert/strict'
      import { quarantinedLabels } from './scripts/guards/check-flaky-quarantine.mjs'
      assert.deepEqual([...quarantinedLabels(JSON.stringify([
        { label: '//services/x/pkg:pkg_test' },
        { runner: 'playwright', file: 'services/web/e2e/example.spec.ts' },
      ]))], ['//services/x/pkg:pkg_test'])
    `,
    ],
    { cwd: root, encoding: 'utf8', env: { ...process.env, NODE_PATH: '' } },
  )
  assert.equal(result.status, 0, result.stderr)
})

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
      [path.join(repoRoot, 'scripts/guards/check-flaky-quarantine.mjs'), '--today', '2099-01-01'],
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
        [path.join(repoRoot, 'scripts/guards/check-flaky-quarantine.mjs'), '--todya', '2026-10-18'],
        {
          stdio: 'pipe',
        },
      ),
    (error) => error.status !== 0,
  )
})

const WEB_FILES = {
  playwright: 'services/web/e2e/example.spec.ts',
  vitest: 'services/web/src/example.test.tsx',
}
function webEntry(runner, overrides = {}) {
  const { label, ...fields } = entry()
  return {
    ...fields,
    runner,
    file: WEB_FILES[runner],
    tests: ['outer > inner > works'],
    ...overrides,
  }
}
function webProblems({ entries = [], playwright = '', vitest = '', vitestConfig = '' } = {}) {
  return findQuarantineProblems({
    entries,
    buildFiles: new Map(),
    testSources: () => [],
    ledgerLabels: new Set(),
    today: '2026-10-18',
    vitestConfig,
    webSources: new Map([
      [WEB_FILES.playwright, playwright],
      [WEB_FILES.vitest, vitest],
    ]),
  })
}
const PLAYWRIGHT_SOURCE = `test.describe('outer', () => { test.describe('inner', () => {
  test('works', { tag: '@quarantine' }, async () => {})
}) })`
const VITEST_SOURCE = `describe('outer', () => { describe('inner', () => {
  it('works', { retry: 2 }, async () => {})
}) })`

test('valid Playwright and vitest retry licences pass together', () => {
  assert.deepEqual(
    webProblems({
      entries: [webEntry('playwright'), webEntry('vitest')],
      playwright: PLAYWRIGHT_SOURCE,
      vitest: VITEST_SOURCE,
    }),
    [],
  )
})
test('a stale Playwright test id fails even when its title exists in a different describe', () => {
  assert.match(
    webProblems({
      entries: [webEntry('playwright', { tests: ['other > inner > works'] })],
      playwright: PLAYWRIGHT_SOURCE,
    }).join('\n'),
    /other > inner > works.*stale/,
  )
})
test('a vitest entry without a per-test retry fails', () => {
  assert.match(
    webProblems({
      entries: [webEntry('vitest')],
      vitest: VITEST_SOURCE.replace('{ retry: 2 }, ', ''),
    }).join('\n'),
    /lacks retry marker/,
  )
})
test('a Playwright quarantine tag with no entry fails', () => {
  assert.match(
    webProblems({ playwright: PLAYWRIGHT_SOURCE }).join('\n'),
    /outer > inner > works.*no quarantine entry/,
  )
})
test('a vitest retry option with no entry fails', () => {
  assert.match(
    webProblems({ vitest: VITEST_SOURCE }).join('\n'),
    /outer > inner > works.*no quarantine entry/,
  )
})
test('an expired web entry fails', () => {
  assert.match(
    webProblems({
      entries: [webEntry('playwright', { expires: '2026-10-17' })],
      playwright: PLAYWRIGHT_SOURCE,
    }).join('\n'),
    /EXPIRED/,
  )
})
test('a Playwright entry without a tag fails', () => {
  assert.match(
    webProblems({
      entries: [webEntry('playwright')],
      playwright: PLAYWRIGHT_SOURCE.replace("{ tag: '@quarantine' }, ", ''),
    }).join('\n'),
    /lacks @quarantine marker/,
  )
})
test('missing web files, duplicate test ids and unknown runners fail', () => {
  assert.match(
    webProblems({
      entries: [webEntry('playwright', { file: 'services/web/e2e/gone.spec.ts' })],
    }).join('\n'),
    /file does not exist/,
  )
  assert.match(
    webProblems({
      entries: [webEntry('playwright'), webEntry('playwright')],
      playwright: PLAYWRIGHT_SOURCE,
    }).join('\n'),
    /duplicate test/,
  )
  assert.match(webProblems({ entries: [webEntry('other')] }).join('\n'), /unknown runner/)
})
test('global vitest retry is forbidden, including zero and string property names', () => {
  for (const vitestConfig of [
    'export default { test: { retry: 2 } }',
    "export default { test: { 'retry': 0 } }",
  ]) {
    assert.match(webProblems({ vitestConfig }).join('\n'), /global retry is forbidden/)
  }
})
test('comments, callback strings and neighbouring tests cannot lend a marker', () => {
  const vitest = `describe('outer', () => { describe('inner', () => {
    // retry: 2
    it('works', () => { const message = 'retry: 2 @quarantine' })
    it('other', { retry: 2 }, () => {})
  }) })`
  assert.match(
    webProblems({ entries: [webEntry('vitest')], vitest }).join('\n'),
    /works lacks retry/,
  )
  assert.deepEqual(webProblems({ vitestConfig: '// retry: 2\n export default { test: {} }' }), [])
})
test('Playwright tag arrays and suite tags, and vitest trailing options resolve', () => {
  const playwright = `test.describe('outer', { tag: ['@quarantine'] }, () => { test.describe('inner', () => { test('works', () => {}) }) })`
  const vitest = VITEST_SOURCE.replace(
    '{ retry: 2 }, async () => {}',
    'async () => {}, { retry: 2 }',
  )
  assert.deepEqual(
    webProblems({ entries: [webEntry('playwright'), webEntry('vitest')], playwright, vitest }),
    [],
  )
})

// Exercise the exact guard command in an isolated repository, including in test-scripts CI.
test('CLI seeded stale Playwright entry fails red, then passes with the entry removed', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'web-quarantine-proof-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  for (const relative of [
    'scripts/guards/check-flaky-quarantine.mjs',
    'scripts/guards/web-quarantine.mjs',
    'skills-toolbox/main-module.mjs',
  ]) {
    fs.mkdirSync(path.dirname(path.join(root, relative)), { recursive: true })
    fs.copyFileSync(path.join(repoRoot, relative), path.join(root, relative))
  }
  fs.symlinkSync(path.join(repoRoot, 'node_modules'), path.join(root, 'node_modules'), 'dir')
  fs.mkdirSync(path.join(root, 'scripts/bazel'), { recursive: true })
  fs.writeFileSync(path.join(root, 'scripts/bazel/ledger.json'), '[]')
  const quarantine = path.join(root, 'scripts/bazel/flaky-tests.json')
  const expires = new Date(Date.now() + 7 * 86_400_000).toISOString().slice(0, 10)
  fs.writeFileSync(
    quarantine,
    JSON.stringify([webEntry('playwright', { expires, tests: ['seeded stale Playwright test'] })]),
  )
  const run = () =>
    spawnSync('node', ['scripts/guards/check-flaky-quarantine.mjs'], {
      cwd: root,
      encoding: 'utf8',
    })
  const red = run()
  assert.equal(red.status, 1, red.stderr)
  assert.match(red.stderr, /seeded stale Playwright test.*stale/)
  fs.writeFileSync(quarantine, '[]')
  const green = run()
  assert.equal(green.status, 0, green.stderr)
})

test('unsupported test factories cannot hide unlisted retry or quarantine markers', () => {
  const vitest = `it.each([1, 2])('case %s', { retry: 2 }, () => {})`
  const playwright = `test.someFactory()('case', { tag: '@quarantine' }, () => {})`
  assert.match(webProblems({ vitest }).join('\n'), /unsupported test factory.*retry marker/)
  assert.match(webProblems({ playwright }).join('\n'), /unsupported test factory.*retry marker/)
})

test('Playwright substring quarantine markers require matching entries', () => {
  for (const { playwright, id } of [
    { playwright: "test('works @quarantine: flaky', () => {})", id: 'works @quarantine: flaky' },
    { playwright: "test('works', { tag: '@quarantine: flaky' }, () => {})", id: 'works' },
    {
      playwright: "test('works', { tag: ['@mobile', '@quarantine: flaky'] }, () => {})",
      id: 'works',
    },
    {
      playwright: "test.describe('outer@quarantine:flaky', () => { test('works', () => {}) })",
      id: 'outer@quarantine:flaky > works',
    },
  ]) {
    assert.match(
      webProblems({ playwright }).join('\n'),
      /has a retry marker but no quarantine entry/,
    )
    assert.deepEqual(
      webProblems({ playwright, entries: [webEntry('playwright', { tests: [id] })] }),
      [],
    )
  }
})

test('literal computed retry and tag properties require matching entries', () => {
  const vitest = VITEST_SOURCE.replace('retry: 2', "['retry']: 2")
  const playwright = PLAYWRIGHT_SOURCE.replace(
    "tag: '@quarantine'",
    "['tag']: '@quarantine: flaky'",
  )
  assert.match(webProblems({ vitest }).join('\n'), /has a retry marker but no quarantine entry/)
  assert.match(webProblems({ playwright }).join('\n'), /has a retry marker but no quarantine entry/)
  assert.deepEqual(
    webProblems({ vitest, playwright, entries: [webEntry('vitest'), webEntry('playwright')] }),
    [],
  )
})

test('literal computed global vitest retry is forbidden', () => {
  assert.match(
    webProblems({ vitestConfig: "export default { test: { ['retry']: 2 } }" }).join('\n'),
    /global retry is forbidden/,
  )
})

test('shorthand per-test retry requires an entry and a licensed test passes', () => {
  const vitest = `const retry = 2; ${VITEST_SOURCE.replace('retry: 2', 'retry')}`
  assert.match(webProblems({ vitest }).join('\n'), /has a retry marker but no quarantine entry/)
  assert.deepEqual(webProblems({ vitest, entries: [webEntry('vitest')] }), [])
})

test('shorthand global retry remains forbidden and shorthand tags fail closed', () => {
  assert.match(
    webProblems({ vitestConfig: 'const retry = 2; export default { test: { retry } }' }).join('\n'),
    /global retry is forbidden/,
  )
  assert.match(
    webProblems({ playwright: "const tag = '@quarantine'; test('works', { tag }, () => {})" }).join(
      '\n',
    ),
    /shorthand tag.*literal tag value/,
  )
})

test('unsupported test factories cannot inherit an unlisted suite retry', () => {
  const vitest = "describe('suite', { retry: 2 }, () => { it.each([1, 2])('case %s', () => {}) })"
  assert.match(webProblems({ vitest }).join('\n'), /unsupported test factory.*retry marker/)
  assert.deepEqual(webProblems({ vitest: vitest.replace('{ retry: 2 }, ', '') }), [])
})

test('quarantined suites allow lifecycle and configuration helpers around real tests', () => {
  const playwright = `test.describe('suite', { tag: '@quarantine' }, () => {
    test.beforeEach(async () => {})
    test.afterEach(async () => {})
    test.beforeAll(async () => {})
    test.afterAll(async () => {})
    const tag = 'fixture value'
    test.use({ tag })
    test.setTimeout(30000)
    test.step('preparation @quarantine', async () => {})
    test.info().attach('details', { body: 'fixture' })
    test.extend({})
    test.describe.configure({ mode: 'serial' })
    test('case', { tag: '@quarantine' }, async () => {})
  })`
  assert.deepEqual(
    webProblems({ playwright, entries: [webEntry('playwright', { tests: ['suite > case'] })] }),
    [],
  )
})

for (const [name, vitest] of [
  ['test.each', "test.each`a ${1}`('case $a', { retry: 2 }, () => {})"],
  [
    'inherited it.each',
    "describe('suite', { retry: 2 }, () => { it.each`a ${1}`('case $a', () => {}) })",
  ],
  ['describe.each', "describe.each`a ${1}`('suite $a', { retry: 2 }, () => {})"],
  ['test.for', "test.for`a ${1}`('case $a', { retry: 2 }, () => {})"],
  ['test.concurrent.each', "test.concurrent.each`a ${1}`('case $a', { retry: 2 }, () => {})"],
]) {
  test(`tagged-template ${name} fails closed on retry markers`, () => {
    assert.match(webProblems({ vitest }).join('\n'), /unsupported test factory.*retry marker/)
    assert.deepEqual(webProblems({ vitest: vitest.replace('{ retry: 2 }, ', '') }), [])
  })
}

test('tagged-template bodies cannot supply retry or quarantine markers', () => {
  for (const runner of ['vitest', 'playwright']) {
    assert.deepEqual(
      webProblems({ [runner]: "test.each`retry: 2 @quarantine ${1}`('case', () => {})" }),
      [],
    )
  }
})

for (const modifier of [
  'concurrent',
  'sequential',
  'shuffle',
  'only',
  'skip',
  'todo',
  'concurrent.only',
]) {
  test(`vitest describe.${modifier} preserves nested identities and retry inheritance`, () => {
    const vitest = `describe.${modifier}('outer', { retry: 2 }, () => {
      describe('inner', () => { it('works', () => {}) })
    })`
    assert.match(
      webProblems({ vitest }).join('\n'),
      /outer > inner > works.*has a retry marker but no quarantine entry/,
    )
    assert.deepEqual(webProblems({ vitest, entries: [webEntry('vitest')] }), [])
    assert.deepEqual(webProblems({ vitest: vitest.replace('{ retry: 2 }, ', '') }), [])
  })
}

test('Playwright describe.fixme preserves nested identities and quarantine inheritance', () => {
  const playwright = `test.describe.fixme('outer', { tag: '@quarantine' }, () => {
    test.describe('inner', () => { test('works', () => {}) })
  })`
  assert.match(
    webProblems({ playwright }).join('\n'),
    /outer > inner > works.*has a retry marker but no quarantine entry/,
  )
  assert.deepEqual(webProblems({ playwright, entries: [webEntry('playwright')] }), [])
  assert.deepEqual(
    webProblems({ playwright: playwright.replace("{ tag: '@quarantine' }, ", '') }),
    [],
  )
})

test('vitest it.fails requires a licence for its retry marker', () => {
  const vitest = VITEST_SOURCE.replace("it('works'", "it.fails('works'")
  assert.match(
    webProblems({ vitest }).join('\n'),
    /outer > inner > works.*has a retry marker but no quarantine entry/,
  )
  assert.deepEqual(webProblems({ vitest, entries: [webEntry('vitest')] }), [])
  assert.deepEqual(webProblems({ vitest: vitest.replace('{ retry: 2 }, ', '') }), [])
})
