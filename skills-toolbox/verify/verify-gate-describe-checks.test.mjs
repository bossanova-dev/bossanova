import test from 'node:test'
import assert from 'node:assert/strict'
import { judge } from './verify-gate.mjs'

const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)
const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })

async function checkWorld({ conflict = false, status = 'SUCCESS' } = {}) {
  const seen = []
  const rollup = [
    {
      name: 'unit',
      status: status === 'PENDING' ? 'IN_PROGRESS' : 'COMPLETED',
      conclusion: status === 'PENDING' ? null : status,
    },
    { context: 'external', state: 'SUCCESS' },
    { name: 'skipped', status: 'COMPLETED', conclusion: 'SKIPPED' },
  ]
  const read = (command, args) => {
    if (command !== 'gh') return { ok: false, exit: 1 }
    if (args[0] === 'pr')
      return ok({
        number: 7,
        state: 'OPEN',
        isDraft: false,
        headRefOid: HEAD,
        baseRefName: 'main',
        mergeStateStatus: conflict ? 'DIRTY' : 'CLEAN',
        mergeable: conflict ? 'CONFLICTING' : 'MERGEABLE',
        body: '',
        title: '',
        url: 'https://github.com/o/r/pull/7',
        statusCheckRollup: rollup,
      })
    const route = args[1]
    if (route === 'graphql')
      return ok({
        data: {
          repository: { pullRequest: { reviews: { nodes: [] }, timelineItems: { nodes: [] } } },
        },
      })
    if (route.includes('/comments?') || route.includes('/statuses?')) return ok([[]])
    if (route.includes('/git/ref/heads/')) return ok({ object: { sha: BASE } })
    if (route.includes('/contents/'))
      return ok({ type: 'file', encoding: 'base64', content: Buffer.from('{}').toString('base64') })
    if (route.includes('/actions/runs?'))
      return ok([
        {
          workflow_runs: [
            {
              name: 'unit',
              status: status === 'PENDING' ? 'in_progress' : 'completed',
              conclusion:
                status === 'SUCCESS' ? 'success' : status === 'FAILURE' ? 'failure' : null,
              head_sha: HEAD,
            },
          ],
        },
      ])
    if (route.includes('/check-runs?')) return ok([{ check_runs: [] }])
    if (route.includes('/status?')) return ok({ statuses: [] })
    if (route.includes('/files?')) return ok([[{ filename: 'README.md', status: 'modified' }]])
    return { ok: false, exit: 1 }
  }
  const result = await judge(
    { pr: 7, repo: 'o/r' },
    {
      read,
      now: () => Date.UTC(2026, 10, 1),
      loadConfig: () => ({}),
      trustedTree: () => ({ ok: true, root: '/trusted/base' }),
      discover: () => ({ extensions: [], skipped: [] }),
      treeAreaFacts: (_files, _policy, options) => {
        seen.push(options.checks)
        return { areas: [], labels: [] }
      },
    },
  )
  return { seen, result }
}

test('settled green CI sends only passed checks to describers', async () => {
  const { seen, result } = await checkWorld()
  assert.deepEqual(seen, [['unit', 'external']], JSON.stringify(result))
})

test('conflicted heads with pending or failing CI send no passed-check assertion', async () => {
  for (const status of ['PENDING', 'FAILURE']) {
    const { seen, result } = await checkWorld({ conflict: true, status })
    assert.deepEqual(seen, [undefined], JSON.stringify(result))
  }
})
