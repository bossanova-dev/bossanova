import { test } from 'node:test'
import assert from 'node:assert/strict'
import { wakePlan, rearmArgv } from './verify-wake.mjs'

for (const [name, input, owner, waitingOn, trigger] of [
  [
    'conflict takes precedence',
    { verdict: 'blocked', prView: { mergeStateStatus: 'DIRTY' } },
    'repair',
    'conflict',
    'checks_passed_ready',
  ],
  [
    'draft waits for a person',
    { verdict: 'wait', reason: 'pr-draft', prView: { isDraft: true } },
    'person',
    'draft',
    'checks_passed_ready',
  ],
  [
    'draft findings belong to repair',
    { verdict: 'defect', prView: { isDraft: true } },
    'repair',
    'findings',
    'checks_passed',
  ],
  [
    'blocked draft belongs to repair',
    { verdict: 'blocked', reason: 'pr-draft', prView: { isDraft: true } },
    'repair',
    'blockers',
    'checks_passed',
  ],
  [
    'ready CI waits belong to verify',
    { verdict: 'wait', reason: 'ci-not-settled', prView: { isDraft: false } },
    'verify',
    'ci',
    'checks_passed_ready',
  ],
]) {
  test(name, () => {
    assert.deepEqual(wakePlan(input), { owner, waitingOn, trigger, onTransition: true })
    const argv = rearmArgv({ pr: 7, chat: 'verify', repo: 'owner/repo', trigger })
    assert.equal(argv[3], trigger)
    assert.equal(argv[4], '--on-transition')
  })
}
