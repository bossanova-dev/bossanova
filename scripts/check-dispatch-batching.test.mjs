import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'

import { checkDispatchBatching } from './check-dispatch-batching.mjs'

function writeFixture(root, row, skillText = 'findings-round-') {
  const auditPath = join(root, 'docs/skills/dispatch-graph-audit.md')
  mkdirSync(dirname(auditPath), { recursive: true })
  writeFileSync(
    auditPath,
    '| id | file | phase | dispatch shape | verdict | parallel out-path | await citation | evidence |\n' +
      '| --- | --- | --- | --- | --- | --- | --- | --- |\n' +
      row,
  )
  mkdirSync(join(root, 'skill'), { recursive: true })
  writeFileSync(join(root, 'skill/SKILL.md'), skillText)
}

test('checkDispatchBatching passes a valid independent declaration', () => {
  const root = mkdtempSync(join(tmpdir(), 'dispatch-batching-'))
  writeFixture(
    root,
    '| X | `skill/SKILL.md` | p | d | independent | `findings-round-<name>.json` | required | cites |\n',
  )
  assert.equal(checkDispatchBatching({ root }).ok, true)
})

test('checkDispatchBatching fails blank parallel out-path on independent rows', () => {
  const root = mkdtempSync(join(tmpdir(), 'dispatch-batching-'))
  writeFixture(root, '| X | `skill/SKILL.md` | p | d | independent | — | required | cites |\n')
  const result = checkDispatchBatching({ root })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /independent row has blank parallel out-path/)
})

test('checkDispatchBatching fails populated parallel out-path on ordered rows', () => {
  const root = mkdtempSync(join(tmpdir(), 'dispatch-batching-'))
  writeFixture(
    root,
    '| X | `skill/SKILL.md` | p | d | ordered | `findings-round-<name>.json` | required | cites |\n',
  )
  const result = checkDispatchBatching({ root })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /non-independent row declares parallel out-path/)
})

test('checkDispatchBatching fails declared stems absent from cited skill', () => {
  const root = mkdtempSync(join(tmpdir(), 'dispatch-batching-'))
  writeFixture(
    root,
    '| X | `skill/SKILL.md` | p | d | independent | `findings-round-<name>.json` | required | cites |\n',
    'no dispatch artifact here',
  )
  const result = checkDispatchBatching({ root })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /declared parallel out-path stem absent/)
})
