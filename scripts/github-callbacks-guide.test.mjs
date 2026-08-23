import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

import { region } from './gate-region-lib.mjs'

const guide = readFileSync(
  new URL('../services/docs/docs/guides/github-callbacks.md', import.meta.url),
  'utf8',
)
const prose = guide.replace(/\s+/g, ' ')

test('GitHub callbacks guide documents delivered as successful send, not consumption', () => {
  assert.match(
    prose,
    /`delivered`\s+\|\s+The\s+send\s+call\s+reported\s+success\s+to\s+the\s+daemon/i,
  )
  assert.match(prose, /redelivery,\s+not\s+loss/i)
  assert.doesNotMatch(prose, /The\s+prompt\s+was\s+successfully\s+delivered\.\s+Terminal\./)
})

test('GitHub callbacks guide documents group mutual exclusivity and valid merged-or-closed use', () => {
  assert.match(
    prose,
    /share\s+a\s+group\s+only\s+when\s+at\s+most\s+one\s+member\s+can\s+ever\s+be\s+satisfied/i,
  )
  assert.match(
    prose,
    /mutually\s+exclusive\s+triggers\s+against\s+the\s+same\s+daemon-local\s+`--group`/i,
    'merged/closed example must explain why grouping is safe',
  )
})

test('GitHub callbacks guide documents boss callback list JSON keys', () => {
  const listSection = region(guide, '### `boss callback list`', null, 'github callback list')
  for (const key of [
    'id',
    'group_id',
    'target_chat_id',
    'repo_owner',
    'repo_name',
    'pr_number',
    'trigger',
    'state',
    'attempt_count',
    'last_event',
    'last_error',
    'triggered_at',
    'delivered_at',
    'expires_at',
    'created_at',
    'updated_at',
  ]) {
    assert.match(listSection, new RegExp(`\\\`${key}\\\``), `missing JSON key ${key}`)
  }
  assert.match(listSection, /group\s+surfaces\s+as\s+`group_id`/i)
  assert.match(listSection, /message\s+body\s+is\s+deliberately\s+not\s+emitted/i)
  assert.match(listSection, /machine\s+contract/i)
})
