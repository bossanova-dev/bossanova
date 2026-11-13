import { gitFixture } from './verify-git-fixture.mjs'
import { commandReader } from './verify-gate.mjs'
import { buildBlockers } from './verify-blockers.mjs'
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'
import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  resolutionFacts,
  validateResolution,
  staleFindingBlockers,
  splitResolved,
  resolutionComment,
  parseResolutionLine,
  resolvedOn,
} from './verify-resolution.mjs'
const head = 'a'.repeat(40),
  old = 'b'.repeat(40),
  id = 'review:123456789abc'
const entry = {
  id,
  status: 'resolved',
  file: 'app.js',
  line: 2,
  test: 'handles failure',
  command: 'node --test app.test.mjs',
  exitCode: 0,
}
const input = (entries = [entry]) => ({ version: 1, head, entries })
const opts = { headSha: head, ids: [id], treeHas: (p) => p === 'app.js', lineCount: () => 3 }
test('resolution validates only complete passing evidence on the judged tree', () => {
  assert.equal(validateResolution(input(), opts).get(id).status, 'resolved')
  for (const patch of [
    { file: 'missing' },
    { line: 0 },
    { line: 4 },
    { test: '' },
    { test: 'one\ntwo' },
    { exitCode: 1 },
    { exitCode: undefined },
    { command: '' },
  ])
    assert.equal(
      validateResolution(input([{ ...entry, ...patch }]), opts).get(id)?.status,
      'unsure',
    )
  for (const value of [
    null,
    '{',
    { ...input(), head: old },
    input([{ ...entry, id: 'review:ffffffffffff' }]),
    input([entry, entry]),
  ])
    assert.equal(validateResolution(value, opts).size, 0)
})
test('present and unsure records retain blockers without repeatedly scheduling the lens', () => {
  const blockers = [
    { id, kind: 'review-finding' },
    { id: 'conflict', kind: 'conflict' },
  ]
  const stale = { reportHead: old, headSha: head }
  assert.deepEqual(staleFindingBlockers(blockers, stale), [blockers[0]])
  assert.deepEqual(staleFindingBlockers(blockers, { ...stale, reportHead: head }), [])
  assert.deepEqual(staleFindingBlockers(blockers, { ...stale, reportHead: undefined }), [])
  for (const status of ['present', 'unsure']) {
    const resolved = validateResolution(input([{ id, status }]), opts)
    assert.deepEqual(staleFindingBlockers(blockers, { ...stale, resolved }), [])
    assert.deepEqual(splitResolved(blockers, resolved).blockers, blockers)
  }
  assert.deepEqual(splitResolved(blockers, validateResolution(input(), opts)).blockers, [
    blockers[1],
  ])
})
test('resolution markers round-trip, reject tampering, and select newest records on the exact head', () => {
  const note = resolutionComment(head, [entry])
  assert.deepEqual(resolutionComment(head, [entry]), note)
  const parsed = parseResolutionLine(note.body.split('\n').at(-1))
  assert.equal(parsed.head, head)
  assert.equal(parsed.entries[0].id, id)
  assert.equal(
    parseResolutionLine(note.body.split('\n').at(-1).replace(note.digest, '0'.repeat(16))),
    null,
  )
  const markers = [
    { ...parsed, at: 1, id: 1 },
    { ...parsed, at: 2, id: 2, entries: [{ id, status: 'present' }] },
  ]
  assert.equal(resolvedOn(markers, head).get(id).status, 'present')
  assert.equal(resolvedOn(markers, old).size, 0)
})

test('resolution evidence must follow the standing report generation', () => {
  const report = { at: 2, id: 20 }
  const marker = { kind: 'resolution', head, at: 1, id: 10, entries: [entry] }
  assert.equal(resolvedOn([marker], head, {}).size, 0)
  for (const patch of [{}, { at: 2, id: 19 }, { at: 2, id: 20 }])
    assert.equal(resolvedOn([{ ...marker, ...patch }], head, report).size, 0)
  for (const patch of [{ at: 3 }, { at: 2, id: 21 }])
    assert.equal(resolvedOn([{ ...marker, ...patch }], head, report).get(id).status, 'resolved')
  const second = { ...entry, id: 'review:abcdef123456' }
  const mixed = resolvedOn([marker, { ...marker, at: 3, entries: [second] }], head, report)
  assert.deepEqual([...mixed.keys()], [second.id])
})

test('resolution citations require a nonempty file blob from the judged commit', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'src/app.js': 'fixed\n', 'empty.js': '' })
  const headSha = repo.commit()
  const finding = {
    severity: 'P2',
    title: 'Handle failure',
    file: 'src/app.js',
    line: 1,
    evidence: 'missing guard',
    introduced: true,
    verified: true,
  }
  const id = buildBlockers({ reviewFindings: [finding] })[0].id
  const report = { head: old, findings: [finding] },
    path = join(repo.dir, 'resolution.json')
  for (const [file, status] of [
    ['src/app.js', 'resolved'],
    ['src', 'unsure'],
    ['empty.js', 'unsure'],
    ['absent.js', 'unsure'],
  ]) {
    writeFileSync(
      path,
      JSON.stringify({ version: 1, head: headSha, entries: [{ ...entry, id, file, line: 1 }] }),
    )
    const result = resolutionFacts({ headSha }, report, [], path, commandReader({ cwd: repo.dir }))
    assert.equal(result.resolution.get(id).status, status, file)
  }
})
