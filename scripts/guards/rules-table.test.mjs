import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import * as prettier from 'prettier'

import { loadRegistry } from './registry.mjs'
import { BEGIN_MARKER, END_MARKER, renderAgentsRegion, renderRulesDoc } from './rules-table.mjs'

import { spliceRegion as splice } from './generated-region.mjs'
const spliceRegion = (text, body) => splice(text, body, 'guard-registry', 'make rules-table-update')

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const CLI = path.join(repoRoot, 'scripts', 'guards', 'rules-table.mjs')

const FIXTURE_REGION = `# Fixture instructions

## Rules

Intro sentence that stays.

${BEGIN_MARKER}
| stale | stale |
${END_MARKER}

| Rule | Enforced by |
| ---- | ----------- |
| x    | y           |
`

function fixtureDir() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rules-table-'))
  fs.writeFileSync(path.join(dir, 'rules-fixture.md'), FIXTURE_REGION)
  return dir
}

function cleanup(dir) {
  fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
}

function run(dir, mode) {
  return spawnSync(
    process.execPath,
    [
      CLI,
      mode,
      '--region-file',
      path.join(dir, 'rules-fixture.md'),
      '--doc-file',
      path.join(dir, 'rules-doc-fixture.md'),
    ],
    { encoding: 'utf8' },
  )
}

const read = (dir, name) => fs.readFileSync(path.join(dir, name), 'utf8')
const write = (dir, name, text) => fs.writeFileSync(path.join(dir, name), text)

function written() {
  const dir = fixtureDir()
  const result = run(dir, '--write')
  assert.equal(result.status, 0, result.stderr)
  return dir
}

const RECORDS = [
  {
    id: 'a-guard',
    rule: 'Pipes | are escaped.',
    command: 'node scripts/a.mjs',
    wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
    scans: ['scripts/**'],
    baseline: null,
    proof: { file: 'scripts/a.test.mjs', test: 'a test' },
  },
  {
    id: 'b-guard',
    rule: 'Second rule.',
    command: 'node scripts/b.mjs',
    wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
    scans: ['scripts/**'],
    baseline: 'scripts/b-baseline.json',
    proof: { file: 'scripts/b_test.go', test: 'TestB' },
  },
]

test('renderAgentsRegion writes one unpadded row per record inside a prettier-ignore range', () => {
  assert.equal(
    renderAgentsRegion(RECORDS),
    [
      '<!-- prettier-ignore-start -->',
      '- Pipes | are escaped.',
      '- Second rule.',
      '<!-- prettier-ignore-end -->',
      '',
    ].join('\n'),
  )
})

test('renderRulesDoc writes every record field', () => {
  const doc = renderRulesDoc(RECORDS)
  assert.ok(
    doc.includes(
      '| `b-guard` | Second rule. | `node scripts/b.mjs` | `make lint-scripts` | `test-scripts.yml` `scripts` | `scripts/b-baseline.json` | `scripts/b_test.go`: TestB |',
    ),
  )
  assert.ok(doc.includes('| `a-guard` | Pipes \\| are escaped. |'))
})

test('a CI-only record renders its CI job alone, with no make target', () => {
  const ciOnly = {
    ...RECORDS[0],
    id: 'c-guard',
    rule: 'CI-only rule.',
    wiring: { make: null, ci: { workflow: 'mirror.yml', job: 'mirror' } },
  }
  assert.ok(renderAgentsRegion([ciOnly]).includes('- CI-only rule.'))
  assert.ok(
    renderRulesDoc([ciOnly]).includes(
      '| `c-guard` | CI-only rule. | `node scripts/a.mjs` | — | `mirror.yml` `mirror` |',
    ),
  )
})

test('--check fails on a hand-edited row in the region', () => {
  const dir = written()
  try {
    assert.equal(run(dir, '--check').status, 0)
    const text = read(dir, 'rules-fixture.md')
    const firstRow = text.split('\n').find((line) => line.startsWith('- '))
    assert.ok(firstRow, 'the generated region must hold a registry row')
    write(dir, 'rules-fixture.md', text.replace(firstRow, firstRow.replace('- ', '- Edited ')))
    const result = run(dir, '--check')
    assert.equal(result.status, 1)
    assert.match(result.stderr, /rules-fixture\.md is out of sync with generated inputs/)
    assert.match(result.stderr, /^-\- Edited /m)
    assert.match(result.stderr, /Run: make rules-table-update/)
  } finally {
    cleanup(dir)
  }
})

test('--check fails on a hand-edited rules doc', () => {
  const dir = written()
  try {
    write(
      dir,
      'rules-doc-fixture.md',
      `${read(dir, 'rules-doc-fixture.md')}\nA hand-written note.\n`,
    )
    const result = run(dir, '--check')
    assert.equal(result.status, 1)
    assert.match(result.stderr, /rules-doc-fixture\.md is out of sync with generated inputs/)
  } finally {
    cleanup(dir)
  }
})

test('--check fails on a fixture with a missing END marker', () => {
  const dir = fixtureDir()
  try {
    write(dir, 'rules-fixture.md', FIXTURE_REGION.replace(`${END_MARKER}\n`, ''))
    for (const mode of ['--check', '--write']) {
      const result = run(dir, mode)
      assert.equal(result.status, 1, mode)
      assert.match(result.stderr, /found 0/, mode)
    }
  } finally {
    cleanup(dir)
  }
})

test('spliceRegion rejects duplicate and misordered markers instead of treating them as empty', () => {
  assert.throws(
    () => spliceRegion(`${BEGIN_MARKER}\n${BEGIN_MARKER}\n${END_MARKER}\n`, 'x\n'),
    /exactly one BEGIN guard-registry marker line, found 2/,
  )
  assert.throws(
    () => spliceRegion(`${END_MARKER}\n${BEGIN_MARKER}\n`, 'x\n'),
    /END guard-registry marker comes before the BEGIN marker/,
  )
  assert.equal(
    spliceRegion(`a\n${BEGIN_MARKER}\nold\n${END_MARKER}\nb\n`, 'new\n'),
    `a\n${BEGIN_MARKER}\nnew\n${END_MARKER}\nb\n`,
  )
})

test('a second --write changes no bytes', () => {
  const dir = written()
  try {
    const region = read(dir, 'rules-fixture.md')
    const doc = read(dir, 'rules-doc-fixture.md')
    assert.ok(
      region.startsWith('# Fixture instructions\n\n## Rules\n\nIntro sentence that stays.\n'),
    )
    assert.ok(
      region.endsWith(
        `${END_MARKER}\n\n| Rule | Enforced by |\n| ---- | ----------- |\n| x    | y           |\n`,
      ),
    )
    assert.equal(run(dir, '--write').status, 0)
    assert.equal(read(dir, 'rules-fixture.md'), region)
    assert.equal(read(dir, 'rules-doc-fixture.md'), doc)
  } finally {
    cleanup(dir)
  }
})

test('the generated region and rules doc are byte-identical after prettier', async () => {
  const { records, errors } = await loadRegistry(repoRoot)
  assert.deepEqual(errors, [])
  assert.ok(records.length > 0)
  const options = {
    ...(await prettier.resolveConfig(path.join(repoRoot, 'docs', 'rules-fixture.md'))),
    parser: 'markdown',
  }
  for (const text of [
    spliceRegion(FIXTURE_REGION, renderAgentsRegion(records)),
    spliceRegion(FIXTURE_REGION, renderAgentsRegion(RECORDS)),
    renderRulesDoc(records),
    renderRulesDoc(RECORDS),
  ]) {
    assert.equal(await prettier.format(text, options), text)
  }
})

test('the CLI requires exactly one of --write and --check', () => {
  const dir = fixtureDir()
  try {
    assert.equal(run(dir, '--write').status, 0)
    const neither = spawnSync(process.execPath, [CLI], { encoding: 'utf8' })
    assert.equal(neither.status, 2)
    const stray = spawnSync(process.execPath, [CLI, '--check', '--bogus'], { encoding: 'utf8' })
    assert.notEqual(stray.status, 0)
  } finally {
    cleanup(dir)
  }
})

test('--check fails when a registered guard is missing from the rules doc', async () => {
  const dir = written()
  try {
    const { records } = await loadRegistry(repoRoot)
    write(dir, 'rules-doc-fixture.md', renderRulesDoc(records.slice(0, -1)))
    const result = run(dir, '--check')
    assert.equal(result.status, 1)
    assert.match(result.stderr, /rules-doc-fixture\.md is out of sync with generated inputs/)
  } finally {
    cleanup(dir)
  }
})
