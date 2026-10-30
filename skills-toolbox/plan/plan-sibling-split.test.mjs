import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  SIBLING_MAX,
  normalizeSiblings,
  parseSiblingMarker,
  siblingEdges,
  siblingMarker,
  siblingSplitPlan,
  verifySiblings,
} from './plan-sibling-split.mjs'

const CLI = fileURLToPath(new URL('./plan-sibling-split.mjs', import.meta.url))

const sibling = (key, overrides = {}) => ({
  key,
  title: `Part ${key}`,
  estimate: 2,
  summary: `Ship part ${key}.`,
  keyChanges: [`src/${key}.mjs — part ${key}`],
  blockedBy: ['source'],
  ...overrides,
})

const input = (overrides = {}) => ({
  sourceId: 'SRC-1',
  sourceTitle: 'Big feature',
  parentId: 'PAR-1',
  // Listed out of dependency order on purpose: c needs b.
  siblings: [sibling('c', { blockedBy: ['b'] }), sibling('b')],
  existingChildren: [],
  ...overrides,
})

const errorsFor = (siblings) => normalizeSiblings(siblings).errors

test('normalizeSiblings accepts a valid split and defaults the optional edge lists', () => {
  const { siblings, errors } = normalizeSiblings([
    { key: 'b', title: ' Part b ', estimate: 3, summary: 'x', keyChanges: ['a — b'] },
  ])
  assert.deepEqual(errors, [])
  assert.deepEqual(siblings, [
    {
      key: 'b',
      title: 'Part b',
      estimate: 3,
      summary: 'x',
      keyChanges: ['a — b'],
      blockedBy: [],
      blocks: [],
    },
  ])
})

test('normalizeSiblings rejects a part that is not single-PR sized', () => {
  for (const estimate of [5, 0, 2.5, '2']) {
    assert.match(errorsFor([sibling('b', { estimate })]).join(), /estimate/, String(estimate))
  }
})

test('normalizeSiblings rejects duplicate keys, the reserved key and malformed keys', () => {
  assert.match(errorsFor([sibling('b'), sibling('b')]).join(), /duplicate sibling keys: b/)
  assert.match(errorsFor([sibling('source')]).join(), /key "source"/)
  assert.match(errorsFor([sibling('Has Space')]).join(), /key "Has Space"/)
})

test('normalizeSiblings rejects a blockedBy naming neither the source nor a sibling', () => {
  assert.match(errorsFor([sibling('b', { blockedBy: ['zzz'] })]).join(), /unknown key\(s\): zzz/)
})

test('normalizeSiblings rejects a blockedBy cycle among siblings', () => {
  const errors = errorsFor([sibling('b', { blockedBy: ['c'] }), sibling('c', { blockedBy: ['b'] })])
  assert.match(errors.join(), /cycle/)
})

test('normalizeSiblings rejects an empty list and more than SIBLING_MAX parts', () => {
  assert.notDeepEqual(errorsFor([]), [])
  const many = Array.from({ length: SIBLING_MAX + 1 }, (_, i) => sibling(`p${i}`))
  assert.match(errorsFor(many).join(), /Oversized-child/)
  assert.deepEqual(errorsFor(many.slice(0, SIBLING_MAX)), [])
})

test('normalizeSiblings rejects a part with no title, summary or key change', () => {
  assert.match(errorsFor([sibling('b', { title: ' ' })]).join(), /title/)
  assert.match(errorsFor([sibling('b', { summary: '' })]).join(), /summary/)
  assert.match(errorsFor([sibling('b', { keyChanges: [] })]).join(), /keyChanges/)
  assert.match(errorsFor([sibling('b', { keyChanges: [''] })]).join(), /keyChanges/)
})

test('the marker round-trips and a foreign comment does not parse as one', () => {
  assert.deepEqual(parseSiblingMarker(`notes\n${siblingMarker('SRC-1', 'b')}\n`), {
    sourceId: 'SRC-1',
    key: 'b',
  })
  assert.equal(parseSiblingMarker('<!-- boss-plan-epic-child:b -->'), null)
  assert.equal(parseSiblingMarker(undefined), null)
})

test('siblingSplitPlan creates in dependency order with the marker in every body', () => {
  const { creates, reused } = siblingSplitPlan(input())
  assert.deepEqual(
    creates.map((c) => c.key),
    ['b', 'c'],
  )
  assert.deepEqual(reused, {})
  for (const create of creates) {
    assert.deepEqual(parseSiblingMarker(create.body), { sourceId: 'SRC-1', key: create.key })
    assert.match(create.body, /^## Summary\n/)
    assert.match(create.body, /## Key changes\n\n- src\//)
    assert.match(create.body, /SRC-1 \(Big feature\)/)
  }
})

test('siblingSplitPlan reuses a child carrying the marker and creates only the rest', () => {
  const { creates, reused } = siblingSplitPlan(
    input({
      existingChildren: [
        { identifier: 'KID-9', description: `old\n${siblingMarker('SRC-1', 'b')}` },
        { identifier: 'KID-8', description: siblingMarker('OTHER-1', 'c') },
        { identifier: 'KID-7', description: 'unrelated child' },
      ],
    }),
  )
  assert.deepEqual(reused, { b: 'KID-9' })
  assert.deepEqual(
    creates.map((c) => c.key),
    ['c'],
  )
})

test('siblingSplitPlan refuses to decide on a truncated child description', () => {
  assert.throws(
    () =>
      siblingSplitPlan(
        input({
          existingChildren: [
            {
              identifier: 'KID-9',
              description: 'abc (truncated, use get_issue for full description)',
            },
          ],
        }),
      ),
    /KID-9 has a truncated description/,
  )
})

test('siblingSplitPlan refuses two children claiming one key', () => {
  const marker = siblingMarker('SRC-1', 'b')
  assert.throws(
    () =>
      siblingSplitPlan(
        input({
          existingChildren: [
            { identifier: 'KID-1', description: marker },
            { identifier: 'KID-2', description: marker },
          ],
        }),
      ),
    /KID-1 and KID-2/,
  )
})

test('siblingEdges points source, sibling and blocks edges the right way, deduplicated', () => {
  const edges = siblingEdges({
    sourceId: 'SRC-1',
    siblings: [
      sibling('b', { blocks: ['EXT-5'] }),
      sibling('c', { blockedBy: ['source', 'b', 'b'], blocks: ['EXT-5'] }),
    ],
    idByKey: { b: 'NEW-B', c: 'NEW-C' },
  })
  assert.deepEqual(
    new Set(edges.map((e) => `${e.issue}<-${e.blockedBy}`)),
    new Set(['NEW-B<-SRC-1', 'EXT-5<-NEW-B', 'NEW-C<-SRC-1', 'NEW-C<-NEW-B', 'EXT-5<-NEW-C']),
  )
  assert.equal(edges.length, 5)
})

test('siblingEdges refuses a missing id and a blocks entry inside the split', () => {
  assert.throws(
    () => siblingEdges({ sourceId: 'SRC-1', siblings: [sibling('b')], idByKey: {} }),
    /no issue id for sibling "b"/,
  )
  assert.throws(
    () =>
      siblingEdges({
        sourceId: 'SRC-1',
        siblings: [sibling('b', { blocks: ['SRC-1'] })],
        idByKey: { b: 'NEW-B' },
      }),
    /blocks SRC-1/,
  )
})

const readBackIssue = (key, overrides = {}) => ({
  identifier: `NEW-${key.toUpperCase()}`,
  parentId: 'PAR-1',
  description: `## Summary\n\nx\n\n${siblingMarker('SRC-1', key)}\n`,
  ...overrides,
})

test('verifySiblings passes when every sibling exists under the parent with its marker', () => {
  const verdict = verifySiblings({
    ...input(),
    readBack: {
      b: readBackIssue('b'),
      c: readBackIssue('c', { parentId: undefined, parent: { id: 'PAR-1' } }),
    },
  })
  assert.deepEqual(verdict, { ok: true, problems: [] })
})

test('verifySiblings flags a wrong parent, a missing marker and a missing sibling', () => {
  const verdict = verifySiblings({
    ...input({ siblings: [sibling('b'), sibling('c'), sibling('d')] }),
    readBack: {
      b: readBackIssue('b', { parentId: 'PAR-2' }),
      c: readBackIssue('c', { description: siblingMarker('SRC-1', 'b') }),
      d: null,
    },
  })
  assert.equal(verdict.ok, false)
  assert.equal(verdict.problems.length, 3, verdict.problems.join('\n'))
  assert.match(verdict.problems[0], /^b: NEW-B parent is PAR-2, want PAR-1/)
  assert.match(verdict.problems[1], /^c: NEW-C lacks the marker/)
  assert.equal(verdict.problems[2], 'd: missing')
})

const runCli = (args) => spawnSync(process.execPath, [CLI, ...args], { encoding: 'utf8' })

test('CLI plan writes each body under the declared scratch name and reports reuse', (t) => {
  const dir = tempDir('plan-sibling-split-', t)
  const inputFile = path.join(dir, 'SRC-1.siblings-in.json')
  writeFileSync(
    inputFile,
    JSON.stringify(
      input({
        existingChildren: [{ identifier: 'KID-9', description: siblingMarker('SRC-1', 'c') }],
      }),
    ),
  )
  const run = runCli(['plan', '--input', inputFile, '--out-dir', dir])
  assert.equal(run.status, 0, run.stderr)
  const out = JSON.parse(run.stdout)
  assert.deepEqual(out.reused, { c: 'KID-9' })
  assert.deepEqual(out.creates, [
    { key: 'b', title: 'Part b', estimate: 2, bodyFile: path.join(dir, 'SRC-1.sibling-b.md') },
  ])
  assert.deepEqual(parseSiblingMarker(readFileSync(out.creates[0].bodyFile, 'utf8')), {
    sourceId: 'SRC-1',
    key: 'b',
  })
})

test('CLI edges and verify read the created map and the read-back, with verify exit codes', (t) => {
  const dir = tempDir('plan-sibling-split-', t)
  const inputFile = path.join(dir, 'in.json')
  const created = path.join(dir, 'created.json')
  const readBack = path.join(dir, 'readback.json')
  writeFileSync(inputFile, JSON.stringify(input()))
  writeFileSync(created, JSON.stringify({ b: 'NEW-B', c: 'NEW-C' }))

  const edges = runCli(['edges', '--input', inputFile, '--created', created])
  assert.equal(edges.status, 0, edges.stderr)
  assert.deepEqual(JSON.parse(edges.stdout), [
    { issue: 'NEW-C', blockedBy: 'NEW-B' },
    { issue: 'NEW-B', blockedBy: 'SRC-1' },
  ])

  writeFileSync(readBack, JSON.stringify({ b: readBackIssue('b'), c: readBackIssue('c') }))
  assert.equal(runCli(['verify', '--input', inputFile, '--read-back', readBack]).status, 0)
  writeFileSync(readBack, JSON.stringify({ b: readBackIssue('b') }))
  const failed = runCli(['verify', '--input', inputFile, '--read-back', readBack])
  assert.equal(failed.status, 1)
  assert.match(failed.stderr, /c: missing/)
})

test('CLI exits 64 on an unknown verb, an unknown flag or a missing flag', () => {
  assert.equal(runCli(['bogus']).status, 64)
  assert.equal(runCli(['plan', '--input', 'x', '--out-dir', 'y', '--extra', 'z']).status, 64)
  assert.equal(runCli(['verify', '--input', 'x']).status, 64)
})
