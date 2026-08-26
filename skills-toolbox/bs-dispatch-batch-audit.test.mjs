import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { auditBatching, batchKeyFor, collectDispatches } from './bs-dispatch-batch-audit.mjs'

const PHASE_RULES = [{ id: 'ROUND', pattern: 'findings-round-' }]

function assistantEntry({ requestId, messageId, uuid = 'entry-uuid', outPath, name = 'Agent' }) {
  return JSON.stringify({
    type: 'assistant',
    uuid,
    requestId,
    message: {
      id: messageId,
      content: [
        {
          type: 'tool_use',
          name,
          input: {
            description: outPath,
            prompt: `write ${outPath} under /tmp/boss-review.run`,
          },
        },
      ],
    },
  })
}

test('batchKeyFor uses requestId then message.id and never entry uuid', () => {
  assert.equal(batchKeyFor({ requestId: 'req', message: { id: 'msg' }, uuid: 'uuid' }), 'req')
  assert.equal(batchKeyFor({ message: { id: 'msg' }, uuid: 'uuid' }), 'msg')
  assert.equal(batchKeyFor({ uuid: 'uuid' }), null)
})

test('three entries sharing one requestId are one batched phase', () => {
  const text = [
    assistantEntry({ requestId: 'req-1', outPath: 'findings-round-a.json' }),
    assistantEntry({ requestId: 'req-1', outPath: 'findings-round-b.json' }),
    assistantEntry({ requestId: 'req-1', outPath: 'findings-round-c.json' }),
  ].join('\n')
  const result = auditBatching(collectDispatches(text), { phaseRules: PHASE_RULES })
  assert.equal(result.phases[0].dispatchCount, 3)
  assert.equal(result.phases[0].maxBatchSize, 3)
  assert.equal(result.phases[0].verdict, 'batched')
})

test('distinct requestIds for the same phase are serial', () => {
  const text = [
    assistantEntry({ requestId: 'req-1', outPath: 'findings-round-a.json' }),
    assistantEntry({ requestId: 'req-2', outPath: 'findings-round-b.json' }),
    assistantEntry({ requestId: 'req-3', outPath: 'findings-round-c.json' }),
  ].join('\n')
  const result = auditBatching(collectDispatches(text), { phaseRules: PHASE_RULES })
  assert.equal(result.phases[0].verdict, 'serial')
  assert.equal(result.ok, false)
})

test('one attributed dispatch is single, not serial', () => {
  const text = assistantEntry({ requestId: 'req-1', outPath: 'findings-round-a.json' })
  const result = auditBatching(collectDispatches(text), { phaseRules: PHASE_RULES })
  assert.equal(result.phases[0].dispatchCount, 1)
  assert.equal(result.phases[0].verdict, 'single')
  assert.equal(result.ok, true)
})

test('missing requestId and message.id is indeterminate and not ok', () => {
  const text = assistantEntry({ outPath: 'findings-round-a.json' })
  const result = auditBatching(collectDispatches(text), { phaseRules: PHASE_RULES })
  assert.equal(result.phases[0].verdict, 'indeterminate')
  assert.equal(result.phases[0].ok, false)
  assert.equal(result.ok, true)
})

test('truncated trailing line is ignored after preceding entries are counted', () => {
  const text = `${assistantEntry({ requestId: 'req-1', outPath: 'findings-round-a.json' })}\n{"type":`
  const dispatches = collectDispatches(text)
  assert.equal(dispatches.length, 1)
  assert.equal(auditBatching(dispatches, { phaseRules: PHASE_RULES }).phases[0].verdict, 'single')
})

test('CLI exits non-zero for serial and zero for batched', () => {
  const dir = mkdtempSync(join(tmpdir(), 'dispatch-batch-audit-'))
  const serial = join(dir, 'serial.jsonl')
  const batched = join(dir, 'batched.jsonl')
  writeFileSync(
    serial,
    [
      assistantEntry({ requestId: 'req-1', outPath: 'findings-round-a.json' }),
      assistantEntry({ requestId: 'req-2', outPath: 'findings-round-b.json' }),
    ].join('\n'),
  )
  writeFileSync(
    batched,
    [
      assistantEntry({ requestId: 'req-1', outPath: 'findings-round-a.json' }),
      assistantEntry({ requestId: 'req-1', outPath: 'findings-round-b.json' }),
    ].join('\n'),
  )
  const serialResult = spawnSync('node', [
    'skills-toolbox/bs-dispatch-batch-audit.mjs',
    'audit',
    '--transcript',
    serial,
    '--format',
    'text',
  ])
  assert.equal(serialResult.status, 1)
  assert.match(String(serialResult.stdout), /BR-PR-ROUND: serial/)

  const output = execFileSync('node', [
    'skills-toolbox/bs-dispatch-batch-audit.mjs',
    'audit',
    '--transcript',
    batched,
    '--format',
    'text',
  ])
  assert.match(String(output), /BR-PR-ROUND: batched/)
})

test('CLI strict mode fails indeterminate while default succeeds', () => {
  const dir = mkdtempSync(join(tmpdir(), 'dispatch-batch-audit-'))
  const transcript = join(dir, 'indeterminate.jsonl')
  writeFileSync(transcript, assistantEntry({ outPath: 'findings-round-a.json' }))

  assert.equal(
    spawnSync('node', [
      'skills-toolbox/bs-dispatch-batch-audit.mjs',
      'audit',
      '--transcript',
      transcript,
    ]).status,
    0,
  )
  assert.equal(
    spawnSync('node', [
      'skills-toolbox/bs-dispatch-batch-audit.mjs',
      'audit',
      '--transcript',
      transcript,
      '--strict',
    ]).status,
    1,
  )
})
