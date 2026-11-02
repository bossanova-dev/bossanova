// The toolbox against the `boss pr state --json` CLI's real bytes: the golden file is written and
// pinned by services/boss/api/cli/session/pr_state_golden_test.go, so a field the CLI drops (as
// it once dropped `workflow_name`) fails here instead of hiding behind a hand-built envelope.

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import { CHECK_STATES, classifyChecks } from '../pr-check-state.mjs'
import { readPRView } from '../pr-state.mjs'
import { rollupFromEnvelope } from './pr-state-envelope.mjs'

const GOLDEN = new URL(
  '../../services/boss/api/cli/session/testdata/pr_state_workflow.golden.json',
  import.meta.url,
)
const goldenText = readFileSync(GOLDEN, 'utf8')
const golden = JSON.parse(goldenText)
const HEAD = golden.head_sha

test('the CLI golden: every Actions check keeps its workflow, a commit status has none', () => {
  const byName = Object.fromEntries(rollupFromEnvelope(golden).map((node) => [node.name, node]))
  assert.equal(byName.build.workflowName, 'CI')
  assert.equal(byName['docs-lint'].workflowName, 'Docs')
  assert.equal(Object.hasOwn(byName['Terraform Cloud'], 'workflowName'), false)
})

test('the CLI golden: a prior saved through view lets notTriggered drop a path-filtered workflow', () => {
  const deps = {
    resolveBoss: () => ({ ok: true, path: '/fake/boss' }),
    spawn: (cmd, args) => {
      assert.equal(cmd, '/fake/boss', `the daemon path spawned ${cmd} ${args.join(' ')}`)
      assert.deepEqual(args.slice(0, 2), ['pr', 'state'])
      return { status: 0, stdout: goldenText }
    },
  }
  const prior = readPRView({ pr: 7, timeoutMs: 60_000, deps })
  assert.equal(prior.out.source, 'daemon')
  // The next head's push touched no docs, so the Docs workflow did not run on it.
  const verdict = classifyChecks({
    headSHA: HEAD,
    observedSHA: HEAD,
    rollup: {
      statusCheckRollup: [
        {
          __typename: 'CheckRun',
          name: 'build',
          status: 'COMPLETED',
          conclusion: 'SUCCESS',
          workflowName: 'CI',
        },
        { __typename: 'StatusContext', context: 'Terraform Cloud', state: 'SUCCESS' },
      ],
    },
    priorContexts: prior.out,
    workflowRuns: [
      {
        name: 'CI',
        workflowName: 'CI',
        status: 'completed',
        conclusion: 'success',
        headSha: HEAD,
        event: 'pull_request',
      },
    ],
  })
  assert.deepEqual(verdict.notTriggered, ['docs-lint'])
  assert.equal(verdict.state, CHECK_STATES.GREEN)
})
