import assert from 'node:assert/strict'
import test from 'node:test'

import { createR2PublishAdapter } from './r2.mjs'

test('r2 publish derives a plans/bossanova/<ID>/<token>.md key and returns url+key', async () => {
  const seen = []
  const adapter = createR2PublishAdapter({
    env: { BOSS_PROOF_R2_BUCKET: 'b', BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.bossanova.dev' },
    uploadImpl: ({ file, key, env, dryRun }) => {
      seen.push({ file, key, dryRun })
      return `${env.BOSS_PROOF_PUBLIC_BASE_URL}/${key}`
    },
  })
  const { url, key } = await adapter.publish({ issueId: 'bos-205', file: '/tmp/p.md' })
  assert.match(key, /^plans\/bossanova\/BOS-205\/[0-9a-f]{32}\.md$/)
  assert.equal(url, `https://proof.bossanova.dev/${key}`)
  assert.equal(seen[0].file, '/tmp/p.md')
})

test('r2 publish forwards dryRun to the upload impl', async () => {
  let sawDryRun
  const adapter = createR2PublishAdapter({
    env: { BOSS_PROOF_R2_BUCKET: 'b' },
    uploadImpl: ({ dryRun, key, env }) => {
      sawDryRun = dryRun
      return `${env.BOSS_PROOF_PUBLIC_BASE_URL || 'https://proof.bossanova.dev'}/${key}`
    },
  })
  await adapter.publish({ issueId: 'BOS-1', file: '/tmp/p.md', dryRun: true })
  assert.equal(sawDryRun, true)
})

test('r2 adapter reports its name', () => {
  const adapter = createR2PublishAdapter({ env: {} })
  assert.equal(adapter.name, 'r2')
})
