import { test } from 'node:test'
import assert from 'node:assert/strict'

import { cronBranchSlug, hasOpenCronPR } from './cron-open-pr.mjs'

test('cronBranchSlug mirrors cronBranchSlugRE behavior', () => {
  assert.equal(cronBranchSlug('bs-sweep-debt'), 'bs-sweep-debt')
  assert.equal(cronBranchSlug(' BS Sweep Mutation! '), 'bs-sweep-mutation')
  assert.equal(cronBranchSlug('***'), 'job')
  assert.equal(cronBranchSlug('A'.repeat(50)), 'a'.repeat(40))
})

test('hasOpenCronPR matches open PR head refs by cron slug prefix', () => {
  const prs = [
    { number: 10, headRefName: 'feature/cron-bs-sweep-debt-1780000000' },
    { number: 11, headRefName: 'cron-bs-sweep-debt-1780000000' },
    { number: 12, headRefName: 'cron-bs-sweep-mutation-1780000001' },
    { number: 13, headRefName: 'cron-bossanova-sweep-debt-1780000002' },
    { number: 14, headRefName: 'cron-bossanova-sweep-mutation-1780000003' },
  ]

  assert.equal(hasOpenCronPR(prs, 'bs-sweep-debt'), true)
  assert.equal(hasOpenCronPR(prs, 'BS Sweep Mutation!'), true)
  assert.equal(hasOpenCronPR(prs, 'Bossanova sweep debt'), true)
  assert.equal(hasOpenCronPR(prs, ['Bossanova sweep mutation', 'bs-sweep-mutation']), true)
  assert.equal(hasOpenCronPR(prs, 'bs-sweep-security'), false)
})
