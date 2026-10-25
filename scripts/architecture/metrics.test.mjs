import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { test } from 'node:test'
import { generateMetrics, markdownSummary } from './metrics.mjs'
import { sourceFiles, e2eMeasurements } from './source.mjs'

const goAvailable = !process.env.TEST_SRCDIR && spawnSync('go', ['version']).status === 0
const skip = goAvailable ? false : 'go list needs the Go toolchain outside the Bazel sandbox'

test('known two-module repository measures exact architecture facts', { skip }, async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'architecture-'))
  const write = (file, content) => {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), content)
  }
  const git = (...args) => {
    const result = spawnSync('git', args, { cwd: root, encoding: 'utf8' })
    assert.equal(result.status, 0, result.stderr)
    return result.stdout
  }
  try {
    git('init')
    git('config', 'commit.gpgsign', 'false')
    git('config', 'user.name', 'Fixture')
    git('config', 'user.email', 'fixture@example.test')
    write('go.work', 'go 1.26.2\nuse (\n ./services/a\n ./lib/b\n)\n')
    write('services/a/go.mod', 'module example.test/a\n\ngo 1.26.2\n')
    write('lib/b/go.mod', 'module example.test/b\n\ngo 1.26.2\n')
    write(
      'lib/b/b.go',
      'package b\ntype Wide interface {\n One()\n Two()\n Three()\n}\nconst Exported = 1\n',
    )
    write('services/a/a.go', 'package a\nimport "example.test/b"\nvar Value = b.Exported\n')
    write(
      'services/a/e2e/sample_cli_test.go',
      'package e2e\nimport "testing"\nfunc TestSample(t *testing.T) {\n t.Run("sample.AC1",func(t *testing.T){})\n}\n',
    )
    write('services/web/e2e/sample.spec.ts', "test('sample.AC2 works', () => {})\n")
    write('docs/features/README.md', '---\nignore: []\nuiRoots: []\nuserFacingRoots: []\n---\n')
    write(
      'docs/features/sample.md',
      '---\nid: sample\nowns: [services/a/**]\n---\n- AC1: first\n- AC2: second\n',
    )
    const record = (id, baseline, extra = {}) =>
      `export default ${JSON.stringify({ id, rule: 'fixture', command: 'node fixture.mjs', wiring: { make: 'test', ci: { workflow: 'test.yml', job: 'test' } }, scans: ['**/*.go'], baseline, proof: { file: 'proof.test.mjs', test: 'fixture' }, ...extra })}\n`
    write('scripts/guards/registry/array.mjs', record('array', 'array.json', { boundary: true }))
    write(
      'scripts/guards/registry/object.mjs',
      record('object', 'object.json', { baselineFormat: 'json' }),
    )
    write('array.json', '["a","b"]\n')
    write('object.json', '{"a":2,"b":3}\n')
    write(
      'scripts/guards/budgets.json',
      JSON.stringify({
        'go-file-size': { default: 10, subjects: { 'lib/b/b.go': 8 } },
        'go-interface-size': { default: 4, subjects: { 'b:Wide': 3 } },
      }),
    )
    write('scripts/bazel/flaky-tests.json', '[{"tests":["one","two"]}]\n')
    git('add', '.')
    git('commit', '-m', 'fixture')
    const report = await generateMetrics({ root, since: '2000-01-01', top: 25 })
    const m = report.measurements
    assert.equal(report.schemaVersion, 1)
    assert.equal(report.commit, git('rev-parse', 'HEAD').trim())
    assert.deepEqual(report.unavailable, [])
    assert.deepEqual(m.packages, {
      'lib/b': { lines: 7, testLines: 0, files: 1, exported: 2 },
      'services/a': { lines: 3, testLines: 0, files: 1, exported: 1 },
      'services/a/e2e': { lines: 0, testLines: 5, files: 0, exported: 0 },
    })
    assert.deepEqual(m.interfaces, [
      { pkg: 'lib/b', name: 'Wide', exported: true, methods: 3, embeds: 0 },
    ])
    assert.deepEqual(m.fanInOut, {
      'example.test/a': { fanIn: 0, fanOut: 1 },
      'example.test/a/e2e': { fanIn: 0, fanOut: 0 },
      'example.test/b': { fanIn: 1, fanOut: 0 },
    })
    assert.equal(m.largestFiles.find((row) => row.file === 'lib/b/b.go').lines, 7)
    assert.deepEqual(m.baselines, {
      array: { baseline: 'array.json', subjects: 2, total: 2, boundary: true },
      object: { baseline: 'object.json', subjects: 2, total: 5, boundary: false },
    })
    assert.deepEqual(m.budgets['go-file-size'].subjects['lib/b/b.go'], {
      measured: 7,
      limit: 8,
      used: 7 / 8,
    })
    assert.deepEqual(m.budgets['go-file-size'].defaultSubjects, [
      { subject: 'services/a/a.go', measured: 3, limit: 10, used: 0.3 },
    ])
    assert.deepEqual(m.budgets['go-interface-size'].subjects['b:Wide'], {
      measured: null,
      limit: 3,
      used: null,
    })
    assert.equal(m.testRatio.parts['lib/b'].prodLines, 7)
    assert.deepEqual(m.testRatio.total, { prodLines: 12, testLines: 6, ratio: 0.5 })
    assert.equal(m.largestFiles.length, 6)
    assert.deepEqual(m.testRatio.parts['services/a'], { prodLines: 3, testLines: 5, ratio: 5 / 3 })
    assert.deepEqual(m.e2e, {
      features: {
        sample: {
          bySurface: { cli: 1, tui: 0, web: 1 },
          criteria: { 'sample.AC1': 1, 'sample.AC2': 1 },
          criteriaWithoutE2e: [],
          testsWithoutCriterion: 0,
        },
      },
      unmappedFiles: [],
    })
    assert.deepEqual(m.flakyQuarantine, { entries: 1, tests: 2 })
    assert.deepEqual(m.churn, { 'lib/b': 7, 'services/a': 3 })
    assert.ok(markdownSummary(report).includes('sample'))
    fs.rmSync(path.join(root, 'scripts/guards/registry'), { recursive: true, maxRetries: 3 })
    const missing = await generateMetrics({ root })
    assert.equal(missing.measurements.baselines, null)
    assert.ok(missing.unavailable.some((row) => row.measurement === 'baselines'))
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('source selection excludes generated and vendored files', () => {
  assert.deepEqual(
    sourceFiles(
      [
        'main.go',
        'x/generated.go',
        'gen/a.go',
        'x.pb.go',
        'a.generated.ts',
        'a.test.ts',
        '.codex/a.mjs',
        '.claude/skills/a/toolbox/a.mjs',
        'x/testdata/a.go',
      ],
      new Set(['x/generated.go']),
    ),
    ['main.go', 'a.test.ts'],
  )
})

test('web coverage counts literal test titles without suite titles or helper calls', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'architecture-titles-'))
  try {
    const file = 'services/web/e2e/sample.spec.ts'
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(
      path.join(root, file),
      `
      test.describe('sample.AC1 suite', () => {
        test.only('sample.AC2 works', () => {})
        test.step('sample.AC1 step', () => {})
      })
      it('sample.AC1 alias', () => {})
      // test('sample.AC1 comment', () => {})
    `,
    )
    const measured = e2eMeasurements(
      root,
      [file],
      [],
      [{ id: 'sample', criteria: [{ id: 'sample.AC1' }, { id: 'sample.AC2' }] }],
    )
    assert.deepEqual(measured.features.sample, {
      bySurface: { cli: 0, tui: 0, web: 1 },
      criteria: { 'sample.AC1': 0, 'sample.AC2': 1 },
      criteriaWithoutE2e: ['sample.AC1'],
      testsWithoutCriterion: 0,
    })
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 })
  }
})

test(
  'churn includes removed and renamed source but excludes historic generated and test files',
  { skip },
  async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'architecture-history-'))
    const write = (file, content) => {
      fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
      fs.writeFileSync(path.join(root, file), content)
    }
    const git = (...args) => {
      const result = spawnSync('git', args, { cwd: root, encoding: 'utf8' })
      assert.equal(result.status, 0, result.stderr)
      return result.stdout
    }
    try {
      git('init')
      git('config', 'commit.gpgsign', 'false')
      git('config', 'user.name', 'Fixture')
      git('config', 'user.email', 'fixture@example.test')
      write('go.work', 'go 1.26.2\nuse ./app\n')
      write('app/go.mod', 'module example.test/history\n\ngo 1.26.2\n')
      write('app/keep.go', 'package history\n')
      write('app/old/removed.go', 'package old\nvar Gone = 1\n')
      write('app/old/moved.go', 'package old\nvar Moved = 1\n')
      write(
        'app/generated.go',
        '// Code generated by fixture. DO NOT EDIT.\npackage history\nvar Generated = 1\n',
      )
      write('app/source_test.go', 'package history\nfunc TestGone() {}\n')
      write('app/testdata/fixture.go', 'package fixture\n')
      write('app/gen/wire.go', 'package wire\n')
      git('add', '.')
      git('commit', '-m', 'seed')
      fs.rmSync(path.join(root, 'app/old/removed.go'))
      git('mv', 'app/old/moved.go', 'app/moved.go')
      write('app/moved.go', 'package history\nvar Moved = 2\nvar Added = 3\n')
      for (const file of [
        'app/generated.go',
        'app/source_test.go',
        'app/testdata/fixture.go',
        'app/gen/wire.go',
      ])
        fs.rmSync(path.join(root, file))
      git('add', '-u')
      git('commit', '-m', 'remove and rename source')
      const report = await generateMetrics({ root, since: '2000-01-01' })
      assert.deepEqual(report.measurements.churn, { app: 4, 'app/old': 8 })
      assert.equal(
        report.unavailable.some((row) => row.measurement === 'churn'),
        false,
      )
    } finally {
      fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 })
    }
  },
)
