import assert from 'node:assert/strict'
import test from 'node:test'

import { parsePlanPublishArgs, runPlanPublish } from './plan-publish.mjs'

test('parsePlanPublishArgs reads --issue, --file, --dry-run', () => {
  assert.deepEqual(
    parsePlanPublishArgs(['--issue', 'BOS-205', '--file', '/tmp/p.md', '--dry-run']),
    { issue: 'BOS-205', file: '/tmp/p.md', dryRun: true },
  )
})

test('parsePlanPublishArgs throws when --issue is missing', () => {
  assert.throws(() => parsePlanPublishArgs(['--file', '/tmp/p.md']), /--issue/)
})

test('parsePlanPublishArgs throws when --file is missing', () => {
  assert.throws(() => parsePlanPublishArgs(['--issue', 'BOS-1']), /--file/)
})

test('runPlanPublish resolves the adapter and returns its url', async () => {
  const url = await runPlanPublish({
    argv: ['--issue', 'BOS-205', '--file', '/tmp/p.md'],
    env: {},
    resolve: () => ({
      name: 'stub',
      publish: async ({ issueId }) => ({ url: `u/${issueId}`, key: 'k' }),
    }),
  })
  assert.equal(url, 'u/BOS-205')
})
