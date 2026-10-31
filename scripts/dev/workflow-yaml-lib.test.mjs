import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  WORKFLOW_DIR,
  changePaths,
  classifyLines,
  commentStart,
  decodeEscapes,
  flowPairs,
  keyValue,
  listWorkflowFiles,
  nestedUnder,
  structuralLines,
  unquote,
} from './workflow-yaml-lib.mjs'

test('commentStart ignores a hash inside a quoted scalar', () => {
  assert.equal(commentStart('      key: "a # b"'), -1)
  assert.equal(commentStart("      key: 'a # b' # real"), 19)
  assert.equal(commentStart('      key: value'), -1)
  assert.equal(commentStart('# whole-line comment'), 0)
  // An apostrophe inside a plain scalar opens no quote, and quoted scalars honour their
  // escapes, so the real comment after them is still found.
  assert.equal(commentStart("      name: it's here # note"), 22)
  assert.equal(commentStart('      key: "a\\" # b" # real'), 21)
  assert.equal(commentStart("      key: 'it''s # x' # real"), 23)
})

test('a block scalar on a keyed dash line ends at the key column', () => {
  const text = '- name: >-\n    body\n  uses: a@b\n'
  assert.deepEqual(
    structuralLines(text).map((entry) => entry.text),
    ['- name: >-', 'uses: a@b'],
  )
})

test('a keyless `- |` block scalar ends at its dash, so a body at the key column stays body', () => {
  for (const opener of ['- |', '- >-', '- - |']) {
    const dash = opener.lastIndexOf('-')
    const body = ' '.repeat(dash + 2)
    const text = `a:\n  ${opener}\n  ${body}uses: foo/bar@v1\n  - next\n`
    assert.deepEqual(
      classifyLines(text).map((entry) => entry.kind),
      ['structural', 'structural', 'scalar', 'structural', 'blank'],
      opener,
    )
  }
})

test('classifyLines tells comments, blanks and scalar bodies apart', () => {
  const text = 'run: |\n  echo "a # b"\n  # shell comment\n\n# yaml comment\nkey: v # c\n'
  assert.deepEqual(
    classifyLines(text).map((entry) => [entry.kind, entry.text ?? null]),
    [
      ['structural', 'run: |'],
      ['scalar', null],
      ['scalar', null],
      ['blank', null],
      ['comment', null],
      ['structural', 'key: v'],
      ['blank', null],
    ],
  )
})

test('keyValue reads plain, quoted, escaped and spaced keys, and only real keys', () => {
  assert.deepEqual(keyValue('- uses: a@b'), { key: 'uses', value: 'a@b' })
  assert.deepEqual(keyValue('"uses": a@b'), { key: 'uses', value: 'a@b' })
  assert.deepEqual(keyValue("'it''s': 'x'"), { key: "it's", value: 'x' })
  assert.deepEqual(keyValue('uses : a@b'), { key: 'uses', value: 'a@b' })
  assert.deepEqual(keyValue('"u\\x73es":"a@b"'), { key: 'uses', value: 'a@b' })
  assert.deepEqual(keyValue('jobs:'), { key: 'jobs', value: '' })
  assert.equal(keyValue('uses:a@b'), null, 'a plain `key:value` is one scalar')
  assert.equal(keyValue('- plain item'), null)
})

test('unquote decodes YAML escapes and leaves an out-of-range code point alone', () => {
  assert.equal(unquote('"a\\tb\\u0041\\x42"'), 'a\tbAB')
  assert.equal(unquote('"\\UFFFFFFFF"'), '\\UFFFFFFFF')
  assert.equal(unquote("'it''s \\n'"), "it's \\n")
  assert.equal(unquote('plain'), 'plain')
})

test('flowPairs splits on top-level commas outside quotes', () => {
  assert.deepEqual(flowPairs('- { name: \'a, b\', with: { x: 1 }, "uses":"c@d" }'), [
    { key: 'name', value: 'a, b' },
    { key: 'with', value: '{ x: 1 }' },
    { key: 'uses', value: 'c@d' },
  ])
  assert.equal(flowPairs('uses: a@b'), null)
})

test('nestedUnder stops at the first line that is not deeper', () => {
  const lines = structuralLines('a:\n  b: 1\n  c:\n    d: 2\ne: 3\n')
  assert.deepEqual(nestedUnder(lines, 0), [1, 2, 3])
  assert.deepEqual(nestedUnder(lines, 2), [3])
})

test('listWorkflowFiles lists YAML files directly under the workflow directory, sorted', () => {
  const stub = {
    existsSync: () => true,
    readdirSync: () => ['b.yaml', 'a.yml', 'notes.md'],
  }
  assert.deepEqual(listWorkflowFiles('/repo', { fs: stub }), [
    `${WORKFLOW_DIR}/a.yml`,
    `${WORKFLOW_DIR}/b.yaml`,
  ])
  assert.deepEqual(listWorkflowFiles('/repo', { fs: { existsSync: () => false } }), [])
})

test('a blank line inside a block-scalar body does not end the body', () => {
  const text = 'run: |\n  a\n\n  echo "x # y"\nnext: 1\n'
  assert.deepEqual(
    classifyLines(text).map((entry) => entry.kind),
    ['structural', 'scalar', 'blank', 'scalar', 'structural', 'blank'],
  )
})

test('a line exactly at a block scalar end column is structure again', () => {
  // `- - |` ends at its last dash (column 4); `- name: |` at its key column (column 4).
  for (const opener of ['  - - |', '  - name: |']) {
    const text = `${opener}\n      body\n    - next\n`
    assert.deepEqual(
      classifyLines(text).map((entry) => entry.kind),
      ['structural', 'scalar', 'structural', 'blank'],
      opener,
    )
  }
})

test('a block scalar behind an anchor or tag is still a block scalar', () => {
  for (const opener of ['run: &build |', 'run: !!str >-', '- &x |', 'run: &a !!str |']) {
    const text = `${opener}\n    uses: not/a-ref@v1\nnext: 1\n`
    assert.deepEqual(
      classifyLines(text).map((entry) => entry.kind),
      ['structural', 'scalar', 'structural', 'blank'],
      opener,
    )
  }
})

test('decodeEscapes decodes every YAML double-quoted escape form', () => {
  assert.equal(decodeEscapes('a\\nb\\tc\\"d\\\\e\\/f'), 'a\nb\tc"d\\e/f')
  assert.equal(decodeEscapes('\\x6epm \\u0063\\U00000069'), 'npm ci')
  assert.equal(decodeEscapes('n\\0pm'), 'n\0pm')
  assert.equal(decodeEscapes('\\U00110000'), '\\U00110000')
  assert.equal(decodeEscapes('plain'), 'plain')
})

test('changePaths unions the push and pull_request path filters', () => {
  const workflow = [
    'on:',
    '  push:',
    '    branches: [main]',
    '    paths:',
    '      - a/**',
    '  pull_request:',
    '    paths:',
    '      - b/**',
    '',
  ].join('\n')
  assert.deepEqual(changePaths(workflow), ['a/**', 'b/**'])
})

test('changePaths reads an unfiltered pull_request trigger as every file', () => {
  const workflow = [
    'on:',
    '  push:',
    '    paths:',
    '      - a/**',
    '  pull_request:',
    '  schedule:',
    '    - cron: x',
    '',
  ].join('\n')
  assert.equal(changePaths(workflow), null)
  assert.deepEqual(
    changePaths(['on:', '  pull_request:', '    paths:', '      - b/**', ''].join('\n')),
    ['b/**'],
  )
})
