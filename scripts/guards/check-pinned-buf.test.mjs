import assert from 'node:assert/strict'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { findBareBufCalls, scanRepository } from './check-pinned-buf.mjs'

const workflow = (steps) => `jobs:\n  build:\n    steps:\n${steps}`
const setup = '      - uses: bufbuild/buf-action@abc\n        with:\n          version: 1.66.1\n'

test('a bare buf call site is flagged in every file class', () => {
  const cases = [
    ['a.mjs', "const buf = process.env.BUF || 'buf'\nspawnSync(buf, [])", 'js-fallback', 1],
    ['b.ts', "spawnSync('buf', ['lint'])", 'js-exec', 1],
    ['Makefile', 'target:\n\tbuf lint\n\t@buf generate\n\tcommand -v buf >/dev/null', 'make', 2],
    ['a.sh', '#!/bin/sh\nbuf --version', 'shell', 2],
    ['package.json', '{"scripts":{"gen":"buf generate"}}', 'package', 1],
    [
      '.github/workflows/test.yml',
      workflow('      - run: >-\n          buf generate\n          proto\n'),
      'workflow',
      5,
    ],
  ]
  for (const [file, text, kind, line] of cases) {
    const findings = findBareBufCalls([{ file, text }])
    assert.ok(findings.length > 0, file)
    assert.deepEqual(
      { file: findings[0].file, kind: findings[0].kind, line: findings[0].line },
      { file, kind, line },
    )
  }
  assert.equal(findBareBufCalls([{ file: 'Makefile', text: cases[2][1] }]).length, 3)
})

test('pinned call sites pass', () => {
  assert.deepEqual(
    findBareBufCalls([
      {
        file: 'Makefile',
        text: 'buf-check-version:\n\t"$(BUF)" lint\n\tnode scripts/test-infra/run-gate.mjs --label "buf lint" -- "$(BUF)" lint\n\t@echo "pinned buf absent — committed gen used"\n\t"$$BUF" generate -o "$$stage"',
      },
      { file: 'a.mjs', text: 'spawnSync(buf, [])' },
      { file: '.github/workflows/test.yml', text: workflow(setup + '      - run: buf generate\n') },
    ]),
    [],
  )
})

test('a buf action in another job does not license a bare call', () => {
  const text = workflow(setup) + '  other:\n    steps:\n      - run: buf generate\n'
  assert.deepEqual(
    findBareBufCalls([{ file: '.github/workflows/test.yml', text }]).map((x) => x.line),
    [9],
  )
  const later = workflow('      - run: buf lint\n' + setup)
  assert.equal(findBareBufCalls([{ file: '.github/workflows/test.yml', text: later }]).length, 1)
})

test('shell segmentation detects wrappers and substitutions without scanning quoted prose', () => {
  for (const text of [
    'env FOO=x buf lint',
    'echo ok && buf lint',
    'node gate -- buf lint',
    'which buf',
    'type buf',
    'echo $(buf --version)',
    'echo "$(buf --version)"',
    'echo `buf --version`',
    "'buf' lint",
    'sudo buf lint',
    'command env FOO=x buf lint',
  ]) {
    assert.equal(findBareBufCalls([{ file: 'a.sh', text }]).length, 1, text)
  }
  assert.deepEqual(
    findBareBufCalls([
      { file: 'a.sh', text: 'echo "buf lint; buf generate"\necho buf\nbuf-check-version' },
    ]),
    [],
  )
})

test('the real tree has no bare buf call sites', () => {
  const result = scanRepository(fileURLToPath(new URL('../..', import.meta.url)))
  assert.deepEqual(result.findings, [])
  for (const count of Object.values(result.counts)) assert.ok(count > 0)
  assert.ok(result.licensed > 0)
})

test('Make continuations and shell expansions are executable call sites', () => {
  for (const text of [
    'target:\n\t@env FOO=x \\\n\tbuf lint',
    'VERSION := $(shell buf --version)',
    'target:\n\t@ - + buf lint',
  ]) {
    assert.equal(findBareBufCalls([{ file: 'Makefile', text }]).length, 1)
  }
})

test('JS call and fallback forms are detected while comments and fixtures are ignored', () => {
  for (const text of [
    "child.execFileSync('buf', [])",
    "execSync('buf lint')",
    "execa('buf', [])",
    "process.env['BUF'] ?? 'buf'",
  ]) {
    assert.equal(findBareBufCalls([{ file: 'a.ts', text }]).length, 1, text)
  }
  assert.deepEqual(
    findBareBufCalls([
      { file: 'a.mjs', text: '// spawnSync("buf")\nconst fixture = `spawnSync("buf")`' },
    ]),
    [],
  )
})

test('workflow setup requires a pin and supports named and composite steps', () => {
  const named =
    '      - name: Setup buf\n        uses: bufbuild/buf-setup-action@abc\n        with:\n          version: 1.66.1\n      - run: buf lint\n'
  assert.deepEqual(
    findBareBufCalls([{ file: '.github/workflows/a.yml', text: workflow(named) }]),
    [],
  )
  assert.equal(
    findBareBufCalls([
      {
        file: '.github/workflows/a.yml',
        text: workflow('      - uses: bufbuild/buf-action@abc\n      - run: buf lint\n'),
      },
    ]).length,
    1,
  )
  assert.deepEqual(
    findBareBufCalls([
      {
        file: '.github/actions/foo/action.yml',
        text: 'runs:\n  using: composite\n  steps:\n' + named,
      },
    ]),
    [],
  )
})
