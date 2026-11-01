import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { MAX_OPEN_PRS, runCappedBossBuildGate } from './boss-build-capped.mjs'
import { DEFAULT_CONFIG, mergeConfig, validateConfig } from '../skill-config.mjs'

function demoConfig() {
  const config = mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { planned: 'Planned' },
        labels: { agentBuild: 'agent-build', needsHuman: 'needs-human' },
      },
    },
  })
  validateConfig(config, 'test')
  return config
}

/** Run the wrapper with `openPrs` open PRs and a tracker that always has work. */
async function runWithOpenPrs(openPrs, argv = []) {
  const queries = []
  const fetches = []
  const exits = []
  let stderr = ''
  await runCappedBossBuildGate(argv, {
    config: demoConfig(),
    tracker: {
      hasUnblockedWork(query) {
        queries.push(query)
        return Promise.resolve(true)
      },
    },
    listOpenPrs: async (fetch) => {
      fetches.push(fetch)
      return { prs: Array.from({ length: openPrs }, (_, i) => 100 + i), truncated: false }
    },
    exit: (code) => exits.push(code),
    stderr: { write: (text) => (stderr += text) },
  })
  return { queries, fetches, exits, stderr }
}

test('the capped gate blocks a build once the repository has 8 open PRs', async () => {
  assert.equal(MAX_OPEN_PRS, 8)
  const run = await runWithOpenPrs(8)
  assert.deepEqual(run.exits, [1])
  assert.match(run.stderr, /^boss-build gate: 8 open PRs reach --max-open-prs 8 /)
  assert.deepEqual(run.queries, [])
})

test('under 8 open PRs the capped gate scans the tracker with the passed-through flags', async () => {
  const run = await runWithOpenPrs(7, ['--exclude-label', 'infra'])
  assert.deepEqual(run.exits, [0])
  assert.equal(run.stderr, '')
  assert.equal(run.fetches.length, 1)
  assert.ok(run.fetches[0] > 8, 'the read must be able to see past the limit')
  assert.equal(run.queries.length, 1)
  assert.deepEqual(run.queries[0].selection.labels, { include: [], exclude: ['infra'] })
})

// Process-level pin: the real entry point reads the open-PR count before any tracker read, which
// only a gate carrying --max-open-prs does, and fails closed when `gh` cannot be run.
test('the capped entry point applies the open-PR limit and fails closed without gh', (t) => {
  const dir = tempDir('boss-build-capped-', t)
  const result = spawnSync(
    process.execPath,
    [fileURLToPath(new URL('./boss-build-capped.mjs', import.meta.url))],
    {
      cwd: dir,
      env: { PATH: join(dir, 'empty-path'), HOME: dir, LINEAR_API_KEY: 'lin_test' },
      encoding: 'utf8',
      timeout: 30_000,
    },
  )
  assert.equal(result.status, 69, result.stderr)
  assert.match(result.stderr, /^boss-build gate: open PRs unreadable through gh .*--max-open-prs/)
})
