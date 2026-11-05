import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readAreaCaps } from './verify-area-caps-state.mjs'
const repo = 'owner/repo'
const now = Date.parse('2026-11-01T00:00:00Z')
const cap = {
  key: 'ui',
  label: 'ui',
  cappedAt: '2026-10-30T00:00:00Z',
  escapes: [{ original: 1, fix: 2, fixMergedAt: '2026-10-30T00:00:00Z' }],
}
const read = () => ({ ok: true, output: '/git/common\n' })
const state = (overrides = {}) => ({
  v: 1,
  repo,
  refreshedAt: new Date(now).toISOString(),
  caps: [cap],
  ...overrides,
})
const load = (value) => ({
  readFileSync: (path) => {
    assert.equal(path, '/git/common/boss-verify/area-caps.json')
    return JSON.stringify(value)
  },
})
test('state is local, repo-bound and freshness failures fail open', () => {
  assert.deepEqual(readAreaCaps({ read, repo, now, fs: load(state()) }), {
    caps: [cap],
    health: [],
  })
  assert.deepEqual(
    readAreaCaps({ read, repo, now, fs: load(state({ refreshedAt: '2026-10-29T00:00:00Z' })) }),
    { caps: [], health: ['area-caps-stale'] },
  )
  for (const value of [null, state({ repo: 'another/repo' }), state({ caps: [{}] })])
    assert.deepEqual(readAreaCaps({ read, repo, now, fs: load(value) }), {
      caps: [],
      health: ['area-caps-unknown'],
    })
  assert.deepEqual(
    readAreaCaps({
      read,
      repo,
      now,
      fs: {
        readFileSync() {
          throw Error('absent')
        },
      },
    }),
    { caps: [], health: ['area-caps-unknown'] },
  )
})

test('GitHub repository casing does not discard an active cap', () => {
  assert.deepEqual(readAreaCaps({ read, repo, now, fs: load(state({ repo: 'Owner/Repo' })) }), {
    caps: [cap],
    health: [],
  })
})
