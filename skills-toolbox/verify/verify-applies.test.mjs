import { test } from 'node:test'
import assert from 'node:assert/strict'
import { APPLIES_NOT_APPLICABLE_EXIT, appliesNote, partitionApplicable } from './verify-applies.mjs'

test('appliesNote takes the last JSON line note and ignores earlier log lines', () => {
  assert.equal(
    appliesNote('{"note":"first"}\n{"note":"evidence: not applicable"}'),
    'evidence: not applicable',
  )
  assert.equal(
    appliesNote('scanning 3 paths\nnot json at all\n{"applies":false,"note":"skip"}\n\n'),
    'skip',
  )
})

test('appliesNote makes the note one line and trims it', () => {
  assert.equal(appliesNote(JSON.stringify({ note: '  a\r\nb\n\n c\t d  ' })), 'a b c d')
})

test('appliesNote caps at 200 runes with a trailing ellipsis', () => {
  const long = appliesNote(JSON.stringify({ note: '😀'.repeat(250) }))
  assert.equal(Array.from(long).length, 200)
  assert.ok(long.endsWith('…'))
  assert.equal(Array.from(long).slice(0, 199).join(''), '😀'.repeat(199))
  const exact = '😀'.repeat(200)
  assert.equal(appliesNote(JSON.stringify({ note: exact })), exact)
})

test('appliesNote returns null for anything but a non-empty string note', () => {
  for (const output of [
    '',
    undefined,
    null,
    'not json',
    '{"note": "broken',
    '{"note":42}',
    '{"note":null}',
    '{"note":""}',
    '{"note":"  \\n "}',
    '{"applies":false}',
    '["note"]',
    'null',
    '{"note":"x"}\nplain trailing log',
  ])
    assert.equal(appliesNote(output), null, JSON.stringify(output))
})

const result = (exit, output = '') => ({ ok: exit === 0, exit, output })
function partition(outcomes, extensions) {
  const runs = []
  const read = (command, args, opts) => {
    runs.push({ command, args, input: opts.input })
    return outcomes[args[0]]
  }
  return { ...partitionApplicable(extensions, { pr: 7 }, { read, nodeBin: 'node' }), runs }
}

test('partitionApplicable keeps notes only from exit-10 checks that printed one', () => {
  const extensions = [
    { name: 'noted', applies: 'noted.mjs' },
    { name: 'silent', applies: 'silent.mjs' },
    { name: 'applies', applies: 'applies.mjs' },
    { name: 'crashed', applies: 'crashed.mjs' },
    { name: 'unchecked' },
    { name: 'second', applies: 'second.mjs' },
  ]
  const split = partition(
    {
      'noted.mjs': result(APPLIES_NOT_APPLICABLE_EXIT, 'log\n{"note":"noted: no change"}'),
      'silent.mjs': result(APPLIES_NOT_APPLICABLE_EXIT, '{"applies":false}'),
      'applies.mjs': result(0, '{"note":"applies anyway"}'),
      'crashed.mjs': result(1, '{"note":"crashed"}'),
      'second.mjs': result(APPLIES_NOT_APPLICABLE_EXIT, '{"note":"second: skip"}'),
    },
    extensions,
  )
  assert.deepEqual(
    split.applicable.map((ext) => ext.name),
    ['applies', 'crashed', 'unchecked'],
  )
  assert.deepEqual(
    split.notApplicable.map((ext) => ext.name),
    ['noted', 'silent', 'second'],
  )
  assert.deepEqual(split.notes, [
    { extension: 'noted', note: 'noted: no change' },
    { extension: 'second', note: 'second: skip' },
  ])
  assert.equal(split.runs.length, 5, 'an extension with no check never runs one')
  assert.deepEqual(split.runs[0], { command: 'node', args: ['noted.mjs'], input: '{"pr":7}' })
})

test('partitionApplicable resolves checks under the trusted root', () => {
  const runs = []
  partitionApplicable(
    [{ name: 'x', applies: 'gate/applies.mjs' }],
    {},
    {
      read: (command, args) => (runs.push(args[0]), result(0)),
      root: '/trusted',
    },
  )
  assert.deepEqual(runs, ['/trusted/gate/applies.mjs'])
})
