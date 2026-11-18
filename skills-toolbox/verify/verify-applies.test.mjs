import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { APPLIES_NOT_APPLICABLE_EXIT, appliesNote, partitionApplicable } from './verify-applies.mjs'
import { extensionEnvelopeFor } from './verify-extension-envelope.mjs'
import { validateVerifyContext as validateLegacyContext } from './fixtures/legacy-verify-context.mjs'

test('per-extension contexts preserve approval deltas and intent for the frozen legacy contract', () => {
  const context = {
    pr: { number: 7 },
    headSha: 'a'.repeat(40),
    baseSha: 'b'.repeat(40),
    changedPaths: ['ui/Panel.tsx'],
    ticket: null,
    range: { kind: 'delta', anchorSha: 'c'.repeat(40) },
    intent: { source: 'unknown', bullets: [] },
  }
  const extended = { ...context, uiFiles: ['ui/Panel.tsx'] }
  assert.equal(validateLegacyContext(extended).ok, false)
  const local = extensionEnvelopeFor({}, extended, ['ui/Panel.tsx'])
  assert.deepEqual(local, context)
  assert.equal(validateLegacyContext(local).ok, true)
  assert.deepEqual(extensionEnvelopeFor({ builtin: true }, context, extended.uiFiles), extended)
  assert.deepEqual(extended.uiFiles, ['ui/Panel.tsx'])
})

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

test('built-in checks run from their directory while local checks use the trusted root', (t) => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'applies-roots-'))
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 }))
  const root = path.join(tmp, 'base')
  const dir = path.join(tmp, 'installed')
  for (const folder of [root, dir]) fs.mkdirSync(folder)
  fs.writeFileSync(path.join(root, 'applies.mjs'), 'process.exitCode = 0\n')
  fs.writeFileSync(path.join(dir, 'applies.mjs'), 'process.exitCode = 10\n')
  const builtin = { name: 'builtin', builtin: true, dir, applies: 'applies.mjs' }
  const local = { name: 'local', dir, applies: 'applies.mjs' }
  const result = partitionApplicable(
    [builtin, local],
    {},
    {
      root,
      read: (command, args, options) => {
        const child = spawnSync(command, args, { ...options, encoding: 'utf8' })
        return { exit: child.status, output: child.stdout }
      },
    },
  )
  assert.deepEqual(result.notApplicable, [builtin])
  assert.deepEqual(result.applicable, [local])
})
