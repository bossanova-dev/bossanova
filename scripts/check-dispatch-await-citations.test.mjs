import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { auditRows, checkDispatchAwaitCitations } from './check-dispatch-await-citations.mjs'

test('auditRows parses required citation rows from the checked-in table', () => {
  const rows =
    auditRows(`| id | file | phase | dispatch shape | verdict | await citation | evidence |
| --- | --- | --- | --- | --- | --- | --- |
| X | \`skill/SKILL.md\` | p | d | ordered | required | cites |
`)
  assert.deepEqual(rows, [{ id: 'X', file: 'skill/SKILL.md', citation: 'required' }])
})

test('checkDispatchAwaitCitations fails a required row with no helper citation', () => {
  const root = mkdtempSync(join(tmpdir(), 'dispatch-citation-'))
  const auditPath = join(root, 'docs/skills/dispatch-graph-audit.md')
  mkdirSync(dirname(auditPath), { recursive: true })
  writeFileSync(
    auditPath,
    '| id | file | phase | dispatch shape | verdict | await citation | evidence |\n' +
      '| --- | --- | --- | --- | --- | --- | --- |\n' +
      '| X | `skill/SKILL.md` | p | d | ordered | required | cites |\n',
  )
  mkdirSync(join(root, 'skill'), { recursive: true })
  writeFileSync(join(root, 'skill/SKILL.md'), 'await every subagent\n')
  const result = checkDispatchAwaitCitations({ root })
  assert.equal(result.ok, false)
  assert.match(result.failures[0], /does not cite toolbox\/bs-dispatch-await\.mjs/)
})

test('checkDispatchAwaitCitations passes required rows that cite the helper', () => {
  const root = mkdtempSync(join(tmpdir(), 'dispatch-citation-'))
  const auditPath = join(root, 'docs/skills/dispatch-graph-audit.md')
  mkdirSync(dirname(auditPath), { recursive: true })
  writeFileSync(
    auditPath,
    '| id | file | phase | dispatch shape | verdict | await citation | evidence |\n' +
      '| --- | --- | --- | --- | --- | --- | --- |\n' +
      '| X | `skill/SKILL.md` | p | d | ordered | required | cites |\n' +
      '| Y | `missing/SKILL.md` | p | d | n/a | not-applicable | waits elsewhere |\n',
  )
  mkdirSync(join(root, 'skill'), { recursive: true })
  writeFileSync(join(root, 'skill/SKILL.md'), 'use `toolbox/bs-dispatch-await.mjs`\n')
  assert.equal(checkDispatchAwaitCitations({ root }).ok, true)
})
