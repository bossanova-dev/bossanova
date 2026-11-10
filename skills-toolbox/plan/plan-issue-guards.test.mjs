import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { writeFileSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { runReadIssue } from '../tracker/read-issue.mjs'
import { createLinearAdapter } from '../tracker/linear.mjs'
import { DEFAULT_CONFIG, requiredPlanSections } from '../skill-config.mjs'

// Pin this suite's gate-outcome destination. Why: ../gate-outcome.test.mjs.
process.env.BOSS_GATE_OUTCOME_FILE = join(tempDir('gate-outcome-suite-'), 'outcomes.tsv')

const guard = fileURLToPath(new URL('./plan-run-guards.mjs', import.meta.url))
const issue = {
  id: 'APP-1',
  identifier: 'APP-1',
  status: 'Todo',
  description: requiredPlanSections(DEFAULT_CONFIG)
    .map((h) => `${h}\n\nComplete planning details for this bounded change.`)
    .join('\n\n'),
  attachments: [{ id: 'a', title: 'Implementation plan (APP-1)' }],
}
function invoke(t, verb, selection, fresh = issue) {
  const dir = tempDir('snapshot-guard-', t),
    first = join(dir, 'selection.json'),
    second = join(dir, 'fresh.json'),
    record = join(dir, 'outcomes.tsv')
  writeFileSync(first, JSON.stringify(selection))
  writeFileSync(second, JSON.stringify(fresh))
  const result = spawnSync(
    process.execPath,
    [guard, verb, first, ...(verb === 'recheck' ? [second] : []), '--selected-id', 'APP-1'],
    { encoding: 'utf8', env: { ...process.env, BOSS_GATE_OUTCOME_FILE: record } },
  )
  return { ...result, record }
}
for (const field of ['id', 'status', 'description', 'attachments'])
  test(`guards refuse missing ${field} before producing a verdict`, (t) => {
    const incomplete = { ...issue }
    delete incomplete[field]
    if (field === 'id') delete incomplete.identifier
    for (const [verb, selection, fresh] of [
      ['idempotence', incomplete, issue],
      ['recheck', incomplete, issue],
      ['recheck', issue, incomplete],
    ]) {
      const r = invoke(t, verb, selection, fresh)
      assert.equal(r.status, 2)
      assert.equal(r.stdout, '')
      assert.match(r.stderr, /unreadable-input/)
      assert.match(
        readFileSync(r.record, 'utf8'),
        new RegExp(`plan-run-guards.${verb}\\t.*unreadable-input`),
      )
    }
  })
test('Linear snapshots round-trip through export and idempotence for planned and unplanned issues', async (t) => {
  const dir = tempDir('snapshot-roundtrip-', t)
  const target = join(dir, 'issue.json')
  writeFileSync(
    join(dir, '.boss-skills.json'),
    JSON.stringify({ trackerConfig: { linear: { states: { planned: 'Todo' } } } }),
  )
  for (const [state, action, reasons] of [
    ['Todo', 'noop', []],
    ['Backlog', 'plan', ['state-not-planned']],
  ]) {
    const adapter = createLinearAdapter({
      apiKey: 'key',
      cwd: dir,
      fetchImpl: async () => ({
        ok: true,
        json: async () => ({
          data: {
            issue: {
              id: 'uuid',
              identifier: issue.identifier,
              title: 'Plan this',
              description: issue.description,
              state: { name: state, type: 'unstarted' },
              attachments: { nodes: issue.attachments, pageInfo: { hasNextPage: false } },
            },
          },
        }),
      }),
    })
    assert.equal(
      await runReadIssue(['--id', 'APP-1', '--out-file', target], {
        write: () => {},
        errWrite: () => {},
        resolveAdapter: () => adapter,
      }),
      0,
    )
    const r = spawnSync(
      process.execPath,
      [guard, 'idempotence', target, '--selected-id', 'APP-1'],
      {
        encoding: 'utf8',
        cwd: dir,
        env: { ...process.env, BOSS_GATE_OUTCOME_FILE: join(dir, 'outcomes.tsv') },
      },
    )
    assert.equal(r.status, 0, r.stderr)
    assert.deepEqual(JSON.parse(r.stdout), { action, reasons })
  }
})
