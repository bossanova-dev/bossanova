import assert from 'node:assert/strict'
import { writeFileSync } from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { evaluateBossPlanGate } from '../cron-gates/boss-plan.mjs'
import { loadSkillConfig } from '../skill-config.mjs'
import { runCli } from './cli.mjs'
import { createLinearAdapter } from './linear.mjs'

function planningTransport(t, descriptions) {
  const cwd = tempDir('linear-plan-references-', t)
  writeFileSync(
    path.join(cwd, '.boss-skills.json'),
    JSON.stringify({
      trackerConfig: {
        linear: {
          mcpServer: 'demo-tracker',
          team: 'Demo',
          descriptionNormalization: { tolerated: [] },
        },
      },
    }),
  )
  const fetchImpl = async (_url, init) => {
    const { query } = JSON.parse(init.body)
    const fields = new Set(query.match(/[_A-Za-z][_0-9A-Za-z]*/g))
    const nodes = descriptions.map((description, index) => ({
      identifier: `APP-${index + 1}`,
      title: 'Planning candidate',
      createdAt: '2026-01-01T00:00:00Z',
      priority: 2,
      labels: { nodes: [{ name: 'agent-plan' }] },
      ...(fields.has('description') ? { description } : {}),
      ...(fields.has('comments') ? { comments: { nodes: [] } } : {}),
    }))
    return {
      ok: true,
      json: async () => ({ data: { issues: { nodes, pageInfo: { hasNextPage: false } } } }),
    }
  }
  return {
    config: loadSkillConfig({ cwd }),
    tracker: createLinearAdapter({ apiKey: 'test-key', fetchImpl, cwd }),
  }
}

test('list-unplanned filters descriptions returned by the real Linear planning query', async (t) => {
  const { config, tracker } = planningTransport(t, ['See #42.', 'A safe candidate.'])
  let out = '',
    err = ''
  const code = await runCli(['list-unplanned'], {
    resolveAdapter: () => tracker,
    loadConfig: () => config,
    env: {},
    write: (text) => {
      out += text
    },
    errWrite: (text) => {
      err += text
    },
  })
  assert.equal(code, 0, err)
  const candidates = JSON.parse(out)
  assert.deepEqual(
    candidates.map(({ identifier }) => identifier),
    ['APP-2'],
  )
  assert.equal(candidates[0].description, 'A safe candidate.')
  assert.match(err, /APP-1.*#42.*pull-request-reference-autolinking/)
})

test('boss-plan gate excludes references from the real Linear planning query', async (t) => {
  const { config, tracker } = planningTransport(t, ['See #42.'])
  const result = await evaluateBossPlanGate({ config, tracker })
  assert.equal(result.hasWork, false)
  assert.match(result.reason, /APP-1.*#42.*pull-request-reference-autolinking/)
})
