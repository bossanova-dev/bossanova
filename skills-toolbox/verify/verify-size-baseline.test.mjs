import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, rm, readdir, readFile, writeFile, stat, symlink } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { readSizeBaseline } from './verify-size-baseline.mjs'
import { confidence } from './verify-confidence.mjs'

const history = Array.from({ length: 100 }, (_, i) => ({ additions: i, deletions: 1 }))
const failed = () => ({ ok: false, error: 'offline' })
async function withRoot(t) {
  const root = await mkdtemp(join(tmpdir(), 'confidence-cache-test-'))
  t.after(() => rm(root, { recursive: true, force: true, maxRetries: 3 }))
  return root
}
async function cachePath(root) {
  const [parent] = await readdir(root)
  return join(root, parent, 'size-baseline-owner-repo.json')
}

test('fetch computes thresholds in a private cache and fresh reads avoid gh', async (t) => {
  const cacheRoot = await withRoot(t)
  let calls = 0
  const read = (command, args, options) => {
    calls++
    assert.equal(command, 'gh')
    assert.deepEqual(args, [
      'pr',
      'list',
      '--repo',
      'owner/repo',
      '--state',
      'merged',
      '--limit',
      '500',
      '--json',
      'additions,deletions',
    ])
    assert.deepEqual(options, { json: true })
    return { ok: true, payload: history }
  }
  const first = readSizeBaseline({ read, repo: 'owner/repo', now: 1000, cacheRoot })
  assert.equal(first.ok, true)
  assert.deepEqual(first.thresholds, { p90: 90, p99: 99, sample: 100 })
  assert.equal(first.sample, 100)
  assert.equal(first.fetchedAt, 1000)
  assert.equal(first.stale, false)
  const parent = join(cacheRoot, (await readdir(cacheRoot))[0])
  assert.equal((await stat(parent)).mode & 0o777, 0o700)
  assert.deepEqual(readSizeBaseline({ read, repo: 'owner/repo', now: 1001, cacheRoot }), first)
  assert.equal(calls, 1)
})

test('expired cache refetches and a failed fetch uses stale cache', async (t) => {
  const cacheRoot = await withRoot(t)
  const read = () => ({ ok: true, payload: history })
  const first = readSizeBaseline({ read, repo: 'owner/repo', now: 1000, cacheRoot })
  const second = readSizeBaseline({ read, repo: 'owner/repo', now: 3000, ttlMs: 1000, cacheRoot })
  assert.equal(second.fetchedAt, 3000)
  const stale = readSizeBaseline({
    read: failed,
    repo: 'owner/repo',
    now: 5000,
    ttlMs: 1000,
    cacheRoot,
  })
  assert.deepEqual(stale, { ...second, stale: true })
  assert.equal(first.stale, false)
})

test('fresh evidence survives a cache destination that cannot be replaced', async (t) => {
  const cacheRoot = await withRoot(t)
  const options = { repo: 'owner/repo', cacheRoot, read: () => ({ ok: true, payload: history }) }
  const expected = readSizeBaseline({ ...options, now: 1000 })
  const path = await cachePath(cacheRoot)
  await rm(path)
  await mkdir(path)

  const fresh = readSizeBaseline({ ...options, now: 2000 })

  assert.deepEqual(fresh, { ...expected, fetchedAt: 2000 })
  const parent = join(cacheRoot, (await readdir(cacheRoot))[0])
  assert.deepEqual(await readdir(parent), ['size-baseline-owner-repo.json'])
})

test('corrupt, mismatched and malformed cache data cannot supply evidence', async (t) => {
  const cacheRoot = await withRoot(t)
  const options = { repo: 'owner/repo', now: 1000, cacheRoot }
  readSizeBaseline({ ...options, read: () => ({ ok: true, payload: history }) })
  const path = await cachePath(cacheRoot)
  const cache = JSON.parse(await readFile(path, 'utf8'))
  for (const value of [
    '{broken',
    { ...cache, repo: 'other/repo' },
    { ...cache, limit: 1 },
    { ...cache, thresholds: { p90: 'bad', p99: 9 } },
    { ...cache, sample: -1 },
  ]) {
    await writeFile(path, typeof value === 'string' ? value : JSON.stringify(value))
    assert.equal(readSizeBaseline({ ...options, read: failed }).ok, false)
  }
  await writeFile(path, '{broken')
  assert.equal(
    readSizeBaseline({ ...options, read: () => ({ ok: true, payload: history }) }).ok,
    true,
  )
})

test('no cache fails closed, malformed history fails closed, thin history skips sizes', async (t) => {
  const cacheRoot = await withRoot(t)
  const options = { repo: 'owner/repo', cacheRoot }
  assert.equal(readSizeBaseline({ ...options, read: failed }).ok, false)
  assert.equal(readSizeBaseline({ ...options, read: () => ({ ok: true, payload: {} }) }).ok, false)
  assert.equal(
    readSizeBaseline({
      ...options,
      read: () => ({ ok: true, payload: [{ additions: '3', deletions: 0 }] }),
    }).ok,
    false,
  )
  const thin = readSizeBaseline({
    ...options,
    read: () => ({ ok: true, payload: history.slice(0, 29) }),
  })
  assert.equal(thin.ok, true)
  assert.equal(thin.thresholds, null)
  assert.equal(thin.sample, 29)
})

test('a symlinked cache parent is refused without fetching or writing', async (t) => {
  const cacheRoot = await withRoot(t)
  const options = { repo: 'owner/repo', cacheRoot, read: () => ({ ok: true, payload: history }) }
  readSizeBaseline(options)
  const parent = join(cacheRoot, (await readdir(cacheRoot))[0])
  await rm(parent, { recursive: true, force: true, maxRetries: 3 })
  const target = await withRoot(t)
  await symlink(target, parent)
  let calls = 0
  const result = readSizeBaseline({
    ...options,
    read: () => {
      calls++
      return { ok: true, payload: history }
    },
  })
  assert.equal(result.ok, false)
  assert.equal(calls, 0)
  assert.deepEqual(await readdir(target), [])
})

test('a size baseline that could not be read leaves confidence cannot-assess', async (t) => {
  const cacheRoot = await withRoot(t)
  const assess = (read) =>
    confidence({
      evidence: { review: 'ran', collectors: [] },
      size: {
        changedLines: 10,
        baseline: readSizeBaseline({ read, repo: 'owner/repo', cacheRoot }),
      },
    }).level
  assert.equal(assess(failed), 'cannot-assess')
  assert.equal(
    assess(() => ({ ok: true, payload: history })),
    'high',
  )
})
