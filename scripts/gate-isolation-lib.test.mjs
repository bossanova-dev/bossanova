import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import {
  extractFailedTargets,
  isolationCommandFor,
  decideIsolationVerdict,
  renderIsolationLines,
  renderLoadLine,
} from './gate-isolation-lib.mjs'

const cwd = '/repo'
const bazel = {
  kind: 'bazel',
  label: '//lib/example:example_test',
  names: ['TestFirst', 'TestSecond'],
}
const node = { kind: 'node-test', file: '/repo/scripts/example.test.mjs' }

test('Bazel associates failure names with their target output block and deduplicates parents', () => {
  const log = `==================== Test output for //lib/example:example_test:
--- FAIL: TestFirst/sub (0.00s)
--- FAIL: TestFirst (0.00s)
--- FAIL: TestSecond (0.00s)
================================================================================
==================== Test output for //lib/other:other_test:
--- FAIL: TestOther (0.00s)
================================================================================
//lib/example:example_test FAILED in 0.3s
//lib/slow:slow_test TIMEOUT in 300.0s`
  assert.deepEqual(extractFailedTargets(log, { cwd }), {
    targets: [bazel, { kind: 'bazel', label: '//lib/slow:slow_test', names: [] }],
    unattributed: [],
  })
})

test('Vitest turbo failures deduplicate each package and file', () => {
  const log = `web:test:  FAIL  src/pages/Subscribe.test.tsx > page > renders
web:test:  FAIL  src/pages/Subscribe.test.tsx > page > subscribes
marketing:test:  FAIL  tests/home.test.ts > home`
  assert.deepEqual(extractFailedTargets(log, { cwd }).targets, [
    { kind: 'vitest', pkg: 'web', file: 'src/pages/Subscribe.test.tsx' },
    { kind: 'vitest', pkg: 'marketing', file: 'tests/home.test.ts' },
  ])
})

test('node TAP locations and top-level file entries identify files, not test names', () => {
  const log = `not ok 1 - example
  ---
  location: '/repo/scripts/example.test.mjs:3:1'
  ...
not ok 2 - /repo/scripts/example.test.mjs
not ok 3 - arbitrary test name`
  assert.deepEqual(extractFailedTargets(log, { cwd }).targets, [node])
})

test('node TAP nested failure indentation and spec summaries resolve relative paths', () => {
  const log = `    not ok 1 - nested
      location: 'scripts/example.test.mjs:3:1'
✖ failing tests:
 test at scripts/example.test.mjs:3:1`
  assert.deepEqual(extractFailedTargets(log, { cwd }).targets, [node])
})

for (const signal of [
  '//lib/broken:broken_test FAILED TO BUILD',
  '//lib/broken:broken_test NO STATUS',
  'ERROR: /repo/BUILD:2:1: compilation failed',
  'web:lint: ERROR: command finished with error: command exited (1)',
  'Failed: web#typecheck',
  'FAIL\tgithub.com/example/pkg [build failed]',
  'FAIL\tgithub.com/example/pkg [setup failed]',
  'FAIL\tgithub.com/example/pkg 0.01s',
  '# tests 1\n# fail 0\nFAILED: teardown hook',
]) {
  test(`unattributed signal prevents flaky classification: ${signal}`, () => {
    const reading = extractFailedTargets(signal, { cwd })
    assert.ok(reading.unattributed.length > 0)
    assert.notEqual(
      decideIsolationVerdict({ ...reading, targets: [node], results: [{ target: node, exit: 0 }] }),
      'flaky-under-load',
    )
  })
}

test('Bazel command preserves race, bypasses cache, escapes regex test names', () => {
  assert.deepEqual(isolationCommandFor(bazel, { BAZEL: '/tools/bazel', RACE: '1' }), [
    '/tools/bazel',
    'test',
    '--test_output=errors',
    '--nocache_test_results',
    '--config=race',
    '--test_filter=^(TestFirst|TestSecond)$',
    bazel.label,
  ])
  assert.deepEqual(isolationCommandFor({ ...bazel, names: [] }, { RACE: '0' }), [
    'bazel',
    'test',
    '--test_output=errors',
    '--nocache_test_results',
    bazel.label,
  ])
  assert.ok(
    isolationCommandFor({ ...bazel, names: ['TestX+'] }, {}).includes('--test_filter=^(TestX\\+)$'),
  )
})

test('Vitest and node commands use argv without shell interpolation', () => {
  assert.deepEqual(isolationCommandFor({ kind: 'vitest', pkg: 'web', file: 'src/a test.ts' }, {}), [
    'pnpm',
    '--filter',
    'web',
    'exec',
    'vitest',
    'run',
    'src/a test.ts',
  ])
  assert.deepEqual(isolationCommandFor(node, {}), [process.execPath, '--test', node.file])
})

test('only a complete set of successful target reruns is flaky', () => {
  const input = {
    targets: [node],
    unattributed: [],
    results: [{ target: node, exit: 0 }],
    budgetExhausted: false,
  }
  assert.equal(decideIsolationVerdict(input), 'flaky-under-load')
  assert.equal(decideIsolationVerdict({ ...input, targets: [] }), 'none-found')
  assert.equal(decideIsolationVerdict({ ...input, unattributed: ['build failed'] }), 'unattributed')
  assert.equal(
    decideIsolationVerdict({ ...input, results: [{ target: node, exit: 1 }] }),
    'reproduced',
  )
  assert.equal(
    decideIsolationVerdict({ ...input, results: [{ target: node, exit: null }] }),
    'reproduced',
  )
  assert.equal(decideIsolationVerdict({ ...input, results: [] }), 'partial')
  assert.equal(
    decideIsolationVerdict({ ...input, results: [{ target: bazel, exit: 0 }] }),
    'partial',
  )
  assert.equal(decideIsolationVerdict({ ...input, budgetExhausted: true }), 'skipped-budget')
  assert.equal(decideIsolationVerdict({ ...input, maxTargets: 0 }), 'skipped-cap')
})

test('rendered isolation lines include counts, target and duration', () => {
  const line = renderIsolationLines({
    targets: [node],
    unattributed: [],
    results: [{ target: node, exit: 0, durationMs: 1234 }],
    decision: 'flaky-under-load',
  })
  assert.match(line, /^isolation: 1 of 1 failed targets passed alone/)
  assert.match(line, /example\.test\.mjs 1\.2s/)
  assert.match(
    renderIsolationLines({ targets: [], results: [], decision: 'none-found' }),
    /no re-runnable failure target in the log/,
  )
  assert.match(
    renderIsolationLines({
      targets: [node],
      results: [{ target: node, exit: 1 }],
      decision: 'reproduced',
    }),
    /reproduced alone/,
  )
  assert.match(
    renderIsolationLines({ targets: [node], results: [], decision: 'skipped-cap', maxTargets: 0 }),
    /1 targets exceed the cap of 0/,
  )
})

test('load line reports measurement, heavy threshold and missing evidence', () => {
  assert.equal(renderLoadLine(null), 'load: unavailable')
  assert.equal(
    renderLoadLine({
      cpus: 12,
      gate: { peak: 48.2, mean: 30.1, samples: 41 },
      isolation: { peak: 6, mean: 4, samples: 2 },
    }),
    'load: gate peak 48.2 / 12 CPUs (4.02x heavy), mean 30.1 over 41 samples - isolation peak 6.0 (0.50x normal)',
  )
  assert.match(renderLoadLine({ cpus: 2, gate: { peak: 2, mean: 0, samples: 1 } }), /1\.00x heavy/)
  assert.equal(renderLoadLine({ cpus: 0, gate: { peak: 2 } }), 'load: unavailable')
})

test('zero passed count stays visible and missing isolation phase is not invented', () => {
  assert.match(
    renderIsolationLines({ targets: [], results: [], decision: 'none-found' }),
    /^isolation: 0 of 0 failed targets passed alone/,
  )
  assert.equal(
    renderLoadLine({ cpus: 2, gate: { peak: 0, mean: 0, samples: 1 } }),
    'load: gate peak 0.0 / 2 CPUs (0.00x normal), mean 0.0 over 1 samples',
  )
})

test('native Go failure remains unattributed beside a rerunnable Bazel failure', () => {
  const log = '//lib/example:example_test FAILED in 0.3s\nFAIL\tgithub.com/example/other 0.01s'
  const reading = extractFailedTargets(log, { cwd })
  assert.equal(reading.targets.length, 1)
  assert.equal(reading.unattributed.length, 1)
  assert.equal(
    decideIsolationVerdict({ ...reading, results: [{ target: reading.targets[0], exit: 0 }] }),
    'unattributed',
  )
})

test('a result repeated for one target cannot stand in for another target', () => {
  const other = { kind: 'node-test', file: '/repo/scripts/other.test.mjs' }
  assert.equal(
    decideIsolationVerdict({
      targets: [node, other],
      results: [
        { target: node, exit: 0 },
        { target: node, exit: 0 },
      ],
    }),
    'partial',
  )
})

test('captured real Bazel failure supplies its label and Go filter names', () => {
  const log = readFileSync(
    new URL('./fixtures/gate-isolation/bazel-failure.log', import.meta.url),
    'utf8',
  )
  assert.deepEqual(extractFailedTargets(log, { cwd }), {
    targets: [
      {
        kind: 'bazel',
        label: '//lib/bossalib/buildinfo:buildinfo_test',
        names: ['TestGateIsolationFixture'],
      },
    ],
    unattributed: [],
  })
})

test('captured real Vitest failure supplies the turbo package and file', () => {
  const log = readFileSync(
    new URL('./fixtures/gate-isolation/vitest-failure.log', import.meta.url),
    'utf8',
  )
  assert.deepEqual(extractFailedTargets(log, { cwd }), {
    targets: [{ kind: 'vitest', pkg: 'web', file: 'tests/sessionStatus.test.ts' }],
    unattributed: [],
  })
})

test('timed-out target cannot certify flaky even if termination exits zero', () => {
  const target = { kind: 'node-test', file: '/tmp/timeout.test.mjs' }
  assert.equal(
    decideIsolationVerdict({ targets: [target], results: [{ target, exit: 0, timedOut: true }] }),
    'reproduced',
  )
})
