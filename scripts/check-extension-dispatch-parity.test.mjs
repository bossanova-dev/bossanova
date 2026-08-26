import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import {
  checkExtensionDispatchParity,
  extensionDispatchRows,
} from './check-extension-dispatch-parity.mjs'

function fixtureRoot(prose) {
  const root = mkdtempSync(join(tmpdir(), 'extension-dispatch-parity-'))
  const auditPath = join(root, 'docs/skills/dispatch-graph-audit.md')
  const skillPath = join(root, 'services/boss/internal/skillinstall/skills/boss-review/SKILL.md')
  mkdirSync(dirname(auditPath), { recursive: true })
  mkdirSync(dirname(skillPath), { recursive: true })
  writeFileSync(
    auditPath,
    '| id | file | phase | dispatch shape | verdict | await citation | evidence |\n' +
      '| --- | --- | --- | --- | --- | --- | --- |\n' +
      '| BR-PR-ROUND | `services/boss/internal/skillinstall/skills/boss-review/SKILL.md` | Phase R | parallel read-only reviewers | independent | required | cites |\n',
  )
  writeFileSync(skillPath, prose)
  return root
}

test('extensionDispatchRows selects required published-core dispatch rows', () => {
  const rows = extensionDispatchRows(
    '| id | file | phase | dispatch shape | verdict | await citation | evidence |\n' +
      '| --- | --- | --- | --- | --- | --- | --- |\n' +
      '| X | `services/boss/internal/skillinstall/skills/boss-review/SKILL.md` | p | d | v | required | e |\n' +
      '| E | `services/boss/internal/skillinstall/skills/boss-epic/SKILL.md` | p | d | v | required | e |\n' +
      '| Y | `services/boss/internal/skillinstall/skills/boss-finalize/SKILL.md` | p | d | v | required | e |\n' +
      '| Z | `services/boss/internal/skillinstall/skills/boss-plan/SKILL.md` | p | d | v | not-applicable | e |\n',
  )
  assert.deepEqual(
    rows.map((row) => row.id),
    ['X', 'E'],
  )
})

test('dispatch parity gate fails when prose names only one agent mechanism', () => {
  const root = fixtureRoot('Dispatch one `Task` call with `subagent_type: general-purpose`.\n')
  const result = checkExtensionDispatchParity({ root })
  assert.equal(result.ok, false)
  assert.match(result.failures[0], /Claude mechanism without the other/)
})

test('dispatch parity gate requires a positive Claude dispatch binding', () => {
  const root = fixtureRoot(
    'Dispatch via Codex `spawn_agent` then `wait_agent`; never use `run_in_background`.\n',
  )
  const result = checkExtensionDispatchParity({ root })
  assert.equal(result.ok, false)
  assert.match(result.failures[0], /Codex mechanism without the other/)
})

test('dispatch parity gate passes when prose names both agent mechanisms', () => {
  const root = fixtureRoot(
    'Dispatch via the awaited mechanism: Claude `Task`, or Codex `spawn_agent` then `wait_agent`.\n',
  )
  assert.equal(checkExtensionDispatchParity({ root }).ok, true)
})
