import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { globToRegExp } from '../../skills-toolbox/skill-config.mjs'
import { pushPaths } from '../workflow-yaml-lib.mjs'
import { checkRegistry, commandScript } from './check-registry.mjs'
import { scannedFiles, validateRecord } from './registry.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const CLI = path.join(repoRoot, 'scripts', 'guards', 'check-registry.mjs')

const WORKFLOW = `name: fixture
on:
  push:
    paths:
      - scripts/**
      - Makefile
jobs:
  scripts:
    runs-on: ubuntu-latest
`

const PROOF_TEST = `import test from 'node:test'
test('fixture guard rejects a seeded violation', () => {})
`

function record(overrides = {}) {
  return {
    id: 'fixture-guard',
    rule: 'Fixture rule.',
    command: 'node scripts/fixture-guard.mjs',
    wiring: { make: 'lint-scripts', ci: { workflow: 'fixture.yml', job: 'scripts' } },
    scans: ['scripts/*.mjs'],
    baseline: null,
    proof: {
      file: 'scripts/fixture-guard.test.mjs',
      test: 'fixture guard rejects a seeded violation',
    },
    ...overrides,
  }
}

// A guard no filename pattern finds names its record in a marker comment; the fixture guard
// script carries one per fixture record unless a test overrides the file.
const markers = (records) => records.map((entry) => `// guard-id: ${entry.id}\n`).join('')

// A throwaway git repo holding the files a registry check reads. Files are staged, never
// committed, so `git ls-files` sees them and no host signing config is involved.
function fixtureRepo({ records = [record()], files = {} } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'guard-registry-'))
  const base = {
    Makefile: 'lint-scripts:\n\t@true\n',
    '.github/workflows/fixture.yml': WORKFLOW,
    'scripts/fixture-guard.mjs': `${markers(records)}export {}\n`,
    'scripts/fixture-guard.test.mjs': PROOF_TEST,
    'scripts/guards/budgets.json': '{}\n',
  }
  for (const entry of records) {
    base[`scripts/guards/registry/${entry.id}.mjs`] = `export default ${JSON.stringify(entry)}\n`
  }
  for (const [file, text] of Object.entries({ ...base, ...files })) {
    if (text === null) continue
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }
  const env = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' }
  execFileSync('git', ['init', '-q'], { cwd: root, env })
  execFileSync('git', ['add', '-A'], { cwd: root, env })
  return root
}

function cleanup(root) {
  fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
}

async function failuresFor(options) {
  const root = fixtureRepo(options)
  try {
    return (await checkRegistry({ repoRoot: root })).failures
  } finally {
    cleanup(root)
  }
}

test('checkRegistry passes a well-formed fixture registry', async () => {
  const root = fixtureRepo()
  try {
    const result = await checkRegistry({ repoRoot: root })
    assert.deepEqual(result.failures, [])
    assert.equal(result.guards, 1)
    assert.equal(result.filesScanned, 2)
  } finally {
    cleanup(root)
  }
})

test('checkRegistry fails a seeded record whose proof test is missing', async () => {
  const failures = await failuresFor({
    files: { 'scripts/fixture-guard.test.mjs': "import test from 'node:test'\n" },
  })
  assert.deepEqual(failures, [
    'fixture-guard: proof.file scripts/fixture-guard.test.mjs declares no test named "fixture guard rejects a seeded violation"',
  ])
})

test('checkRegistry fails a proof file that is untracked or not a test file', async () => {
  const missing = await failuresFor({ files: { 'scripts/fixture-guard.test.mjs': null } })
  assert.match(
    missing.join('\n'),
    /proof\.file scripts\/fixture-guard\.test\.mjs is not a tracked file/,
  )
  const notATest = await failuresFor({
    records: [record({ proof: { file: 'scripts/fixture-guard.mjs', test: 'x' } })],
  })
  assert.match(notATest.join('\n'), /must be a \.test\.mjs or _test\.go file/)
})

test('checkRegistry finds a Go proof test by its func declaration', async () => {
  const goProof = { file: 'scripts/fixture_test.go', test: 'TestFixtureGuard' }
  const found = await failuresFor({
    records: [record({ proof: goProof, scans: ['scripts/**'] })],
    files: { 'scripts/fixture_test.go': 'package x\n\nfunc TestFixtureGuard(t *testing.T) {}\n' },
  })
  assert.deepEqual(found, [])
  const absent = await failuresFor({
    records: [record({ proof: goProof, scans: ['scripts/**'] })],
    files: { 'scripts/fixture_test.go': 'package x\n\n// TestFixtureGuard is gone\n' },
  })
  assert.match(absent.join('\n'), /declares no test named "TestFixtureGuard"/)
})

test('checkRegistry finds a Python proof test by its def declaration', async () => {
  const found = await failuresFor({
    records: [record({ proof: { file: 'scripts/test_fixture.py', test: 'test_fixture_guard' } })],
    files: {
      'scripts/test_fixture.py':
        'class GuardTest:\n    def test_fixture_guard(self):\n        pass\n',
    },
  })
  assert.deepEqual(found, [])
})

test('checkRegistry fails a Python proof file that declares no such test', async () => {
  const failures = await failuresFor({
    records: [record({ proof: { file: 'scripts/test_fixture.py', test: 'test_fixture_guard' } })],
    files: {
      'scripts/test_fixture.py': '# def test_fixture_guard(self):\nname = "test_fixture_guard"\n',
    },
  })
  assert.deepEqual(failures, [
    'fixture-guard: proof.file scripts/test_fixture.py declares no test named "test_fixture_guard"',
  ])
})

test('checkRegistry fails a seeded record whose scans glob matches zero tracked files', async () => {
  const failures = await failuresFor({
    records: [record({ scans: ['scripts/*.mjs', 'scripts/nothing-here/**'] })],
  })
  assert.deepEqual(failures, [
    'fixture-guard: scans glob scripts/nothing-here/** matches zero tracked files',
  ])
})

test('checkRegistry fails a scanned file that the fixture workflow push paths miss', async () => {
  const failures = await failuresFor({
    records: [record({ scans: ['scripts/*.mjs', 'docs/*.md'] })],
    files: { 'docs/notes.md': 'notes\n' },
  })
  assert.deepEqual(failures, [
    'fixture-guard: scans docs/notes.md, which no push path in .github/workflows/fixture.yml matches',
  ])
})

test('checkRegistry counts a workflow with no push paths filter as covered', async () => {
  const failures = await failuresFor({
    records: [record({ scans: ['scripts/*.mjs', 'docs/*.md'] })],
    files: {
      'docs/notes.md': 'notes\n',
      '.github/workflows/fixture.yml': 'name: fixture\non:\n  push:\n    branches: [x]\n',
    },
  })
  assert.deepEqual(failures, [])
})

test('checkRegistry fails closed on a negated push path', async () => {
  const failures = await failuresFor({
    files: {
      '.github/workflows/fixture.yml': WORKFLOW.replace(
        '      - Makefile\n',
        "      - Makefile\n      - '!scripts/vendor/**'\n",
      ),
    },
  })
  assert.match(failures.join('\n'), /push paths use negation \(!scripts\/vendor\/\*\*\)/)
})

test('checkRegistry fails broken schema references', async () => {
  const failures = await failuresFor({
    records: [
      record({
        command: 'node scripts/missing.mjs',
        wiring: { make: 'no-such-target', ci: { workflow: 'missing.yml', job: 'scripts' } },
      }),
    ],
  })
  assert.deepEqual(failures, [
    'fixture-guard: command runs scripts/missing.mjs, which does not exist',
    'fixture-guard: wiring.make no-such-target is not a target in the root Makefile',
    'fixture-guard: wiring.ci.workflow .github/workflows/missing.yml does not exist',
  ])
})

test('checkRegistry fails an id that does not equal its filename', async () => {
  const root = fixtureRepo()
  try {
    fs.renameSync(
      path.join(root, 'scripts/guards/registry/fixture-guard.mjs'),
      path.join(root, 'scripts/guards/registry/other-name.mjs'),
    )
    // The rejected record registers nothing, so its marker would also read as an orphan.
    fs.writeFileSync(path.join(root, 'scripts/fixture-guard.mjs'), 'export {}\n')
    const { failures } = await checkRegistry({ repoRoot: root })
    assert.deepEqual(failures, [
      'scripts/guards/registry/other-name.mjs: id fixture-guard must equal the filename (other-name)',
    ])
  } finally {
    cleanup(root)
  }
})

test('validateRecord rejects missing and ill-typed fields and a multi-line rule', () => {
  const file = 'scripts/guards/registry/fixture-guard.mjs'
  assert.deepEqual(validateRecord(record(), file), [])
  const { proof: _proof, ...withoutProof } = record()
  assert.deepEqual(validateRecord(withoutProof, file), [`${file}: missing field proof`])
  assert.deepEqual(validateRecord(record({ rule: 'one\ntwo' }), file), [
    `${file}: rule must be a single non-empty line`,
  ])
  assert.deepEqual(validateRecord(record({ scans: [] }), file), [
    `${file}: scans must be a non-empty array of globs`,
  ])
  assert.deepEqual(validateRecord(record({ baseline: 3, extra: true }), file), [
    `${file}: record has unknown field extra`,
    `${file}: baseline must be null or a repo-relative path`,
  ])
  assert.deepEqual(validateRecord(null, file), [`${file}: must \`export default\` a record object`])
})

test('checkRegistry fails a malformed baseline and a budget for an unregistered guard', async () => {
  const failures = await failuresFor({
    records: [record({ baseline: 'scripts/fixture-baseline.json' })],
    files: {
      'scripts/fixture-baseline.json': '["b", "a"]\n',
      'scripts/guards/budgets.json': '{ "unregistered": { "default": 1 } }\n',
    },
  })
  assert.deepEqual(failures, [
    'fixture-guard: scripts/fixture-baseline.json must stay sorted',
    'scripts/guards/budgets.json: unregistered is not a registered guard',
  ])
})

test('checkRegistry runs the golangci caps check when golangci-uncapped is registered', async () => {
  const capsRecord = record({ id: 'golangci-uncapped', scans: ['.golangci.yml'] })
  const failures = await failuresFor({
    records: [capsRecord],
    files: {
      '.golangci.yml':
        'version: "2"\nissues:\n  max-issues-per-linter: 0\n  max-same-issues: 3\n  uniq-by-line: false\n',
      '.github/workflows/fixture.yml': WORKFLOW.replace(
        '      - Makefile\n',
        '      - .golangci.yml\n',
      ),
    },
  })
  assert.deepEqual(failures, [
    'golangci-uncapped: .golangci.yml issues.max-same-issues is 3 — set it to 0',
  ])
})

test('checkRegistry fails a scripts/check-* gate script that no record registers', async () => {
  const failures = await failuresFor({ files: { 'scripts/check-orphan.mjs': 'export {}\n' } })
  assert.deepEqual(failures, ['unregistered gate: scripts/check-orphan.mjs'])
  const shell = await failuresFor({ files: { 'scripts/check-orphan.sh': 'exit 0\n' } })
  assert.deepEqual(shell, ['unregistered gate: scripts/check-orphan.sh'])
})

test('a gate script is registered by a command that runs it or a proof that is its sibling test', async () => {
  const byCommand = await failuresFor({
    records: [record({ command: 'node scripts/check-orphan.mjs --flag' })],
    files: { 'scripts/check-orphan.mjs': 'export {}\n' },
  })
  assert.deepEqual(byCommand, [])
  const siblingTest = {
    file: 'scripts/check-orphan.test.mjs',
    test: 'fixture guard rejects a seeded violation',
  }
  const byProof = await failuresFor({
    records: [record({ command: 'node scripts/lint-entry.mjs', proof: siblingTest })],
    files: {
      'scripts/check-orphan.mjs': 'export {}\n',
      'scripts/check-orphan.test.mjs': PROOF_TEST,
      'scripts/lint-entry.mjs': 'export {}\n',
    },
  })
  assert.deepEqual(byProof, [])
})

test('checkRegistry fails a sibling-less scripts/check-*.test.mjs that no record names', async () => {
  const failures = await failuresFor({ files: { 'scripts/check-lonely.test.mjs': PROOF_TEST } })
  assert.deepEqual(failures, ['unregistered gate: scripts/check-lonely.test.mjs'])
  const named = await failuresFor({
    records: [
      record({
        command: 'node --test scripts/check-lonely.test.mjs',
        proof: {
          file: 'scripts/check-lonely.test.mjs',
          test: 'fixture guard rejects a seeded violation',
        },
      }),
    ],
    files: { 'scripts/check-lonely.test.mjs': PROOF_TEST },
  })
  assert.deepEqual(named, [])
})

test('checkRegistry fails a guard-id marker whose record was deleted', async () => {
  const failures = await failuresFor({
    files: {
      'services/x/rule_test.go': 'package x\n\n// guard-id: deleted-guard\nfunc TestX() {}\n',
    },
  })
  assert.deepEqual(failures, [
    'unregistered gate: services/x/rule_test.go:3 carries guard-id: deleted-guard, which no record registers',
  ])
})

test('checkRegistry fails a marker-owned record whose source lost its marker', async () => {
  const failures = await failuresFor({ files: { 'scripts/fixture-guard.mjs': 'export {}\n' } })
  assert.deepEqual(failures, [
    'fixture-guard: no filename pattern finds this gate, and no file it names (command, proof.file, scans) carries a guard-id: fixture-guard comment',
  ])
  // A marker in a file the record does not name does not count.
  const elsewhere = await failuresFor({
    files: {
      'scripts/fixture-guard.mjs': 'export {}\n',
      'docs/other.md': '# guard-id: fixture-guard\n',
    },
  })
  assert.match(
    elsewhere.join('\n'),
    /no file it names .* carries a guard-id: fixture-guard comment/,
  )
})

test('only a whole comment line is a guard-id marker', async () => {
  const failures = await failuresFor({
    files: {
      'scripts/notes.mjs':
        "const text = '// guard-id: not-a-marker'\n// see guard-id: prose-mention\n",
    },
  })
  assert.deepEqual(failures, [])
})

test('a CI-only gate may set wiring.make to null', async () => {
  const ciOnly = record({ wiring: { make: null, ci: { workflow: 'fixture.yml', job: 'scripts' } } })
  assert.deepEqual(validateRecord(ciOnly, 'scripts/guards/registry/fixture-guard.mjs'), [])
  assert.deepEqual(await failuresFor({ records: [ciOnly] }), [])
  const noCi = record({ wiring: { make: null, ci: null } })
  assert.deepEqual(validateRecord(noCi, 'scripts/guards/registry/fixture-guard.mjs'), [
    'scripts/guards/registry/fixture-guard.mjs: wiring.ci must be an object { workflow, job }',
  ])
})

test('commandScript skips node flags and reads bash scripts', () => {
  assert.equal(commandScript('node scripts/a.mjs --check'), 'scripts/a.mjs')
  assert.equal(
    commandScript('node --test --test-reporter=tap scripts/a.test.mjs'),
    'scripts/a.test.mjs',
  )
  assert.equal(commandScript('bash scripts/a.sh base head'), 'scripts/a.sh')
  assert.equal(commandScript("go -C services/x test ./... -run '^TestX$'"), null)
})

test('scannedFiles resolves globs against tracked files only', () => {
  const tracked = ['a/b.go', 'a/c/d.go', 'a/e.md', 'f.go']
  assert.deepEqual(scannedFiles('/unused', ['a/**'], { tracked }), ['a/b.go', 'a/c/d.go', 'a/e.md'])
  assert.deepEqual(scannedFiles('/unused', ['**/*.go'], { tracked }), [
    'a/b.go',
    'a/c/d.go',
    'f.go',
  ])
  assert.deepEqual(scannedFiles('/unused', ['a/*.go'], { tracked }), ['a/b.go'])
})

test('the real scripts workflow push paths cover Go-only, golangci, rules-doc and warehouse edits', () => {
  const text = fs.readFileSync(path.join(repoRoot, '.github/workflows/test-scripts.yml'), 'utf8')
  const { paths } = pushPaths(text)
  assert.ok(Array.isArray(paths) && paths.length > 0, 'test-scripts.yml must have push paths')
  assert.deepEqual(
    paths.filter((entry) => entry.startsWith('!')),
    [],
    'negated push paths are not evaluated by the registry check',
  )
  const patterns = paths.map(globToRegExp)
  for (const file of [
    'services/bosso/internal/server/example.go',
    'services/warehouse/models/growth/example.sql',
    '.golangci.yml',
    'docs/philosophy/rules.md',
    'scripts/guards/registry/guard-registry.mjs',
  ]) {
    assert.ok(
      patterns.some((pattern) => pattern.test(file)),
      `${file} must match a test-scripts.yml push path`,
    )
  }
})

test('the glob matcher agrees with GitHub push-path semantics for the shapes in use', () => {
  const cases = [
    ['scripts/**', 'scripts/a/b/c.mjs', true],
    ['scripts/**', 'scriptsx/a.mjs', false],
    ['**/*.go', 'main.go', true],
    ['**/*.go', 'a/b/c.go', true],
    ['**/*.go', 'a/b/c.go.txt', false],
    ['services/*/Dockerfile.k8s', 'services/bossd/Dockerfile.k8s', true],
    ['services/*/Dockerfile.k8s', 'services/a/b/Dockerfile.k8s', false],
    ['.golangci.yml', '.golangci.yml', true],
    ['.golangci.yml', 'x.golangci.yml', false],
  ]
  for (const [glob, file, expected] of cases) {
    assert.equal(globToRegExp(glob).test(file), expected, `${glob} vs ${file}`)
  }
})

test('the real guard registry passes at head', async () => {
  const { failures, guards } = await checkRegistry({ repoRoot })
  assert.deepEqual(failures, [])
  assert.ok(guards >= 2)
})

test('the CLI rejects stray arguments', () => {
  const result = spawnSync(process.execPath, [CLI, 'stray'], { encoding: 'utf8' })
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /Unexpected argument|ERR_PARSE_ARGS/)
})
