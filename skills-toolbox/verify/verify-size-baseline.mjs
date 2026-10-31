import { lstatSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomUUID } from 'node:crypto'
import { sizeThresholds } from './verify-confidence.mjs'

/**
 * @typedef {{ok: true, thresholds: ReturnType<typeof sizeThresholds>, sample: number, fetchedAt: number, stale: boolean} | {ok: false, error: string}} SizeBaselineResult
 * @typedef {(command: string, args: string[], options: {json: boolean}) => {ok: boolean, payload?: unknown, error?: string}} Reader
 * @typedef {{repo: string, limit: number, changedLines: number[], fetchedAt: number}} Cache
 */

/** @param {string} root */
function privateParent(root) {
  const uid = typeof process.getuid === 'function' ? process.getuid() : 'user'
  const parent = join(root, `verify-size-baselines-${uid}`)
  mkdirSync(root, { recursive: true })
  try {
    mkdirSync(parent, { mode: 0o700 })
  } catch (error) {
    if (error.code !== 'EEXIST') throw error
  }
  const st = lstatSync(parent)
  if (
    !st.isDirectory() ||
    st.isSymbolicLink() ||
    (st.mode & 0o777) !== 0o700 ||
    (typeof process.getuid === 'function' && st.uid !== process.getuid())
  ) {
    throw Error('Size baseline cache parent is not a private owned directory')
  }
  return parent
}

/** @param {string} path @param {string} repo @param {number} limit @returns {Cache|null} */
function readCache(path, repo, limit) {
  try {
    if (!lstatSync(path).isFile()) return null
    const value = JSON.parse(readFileSync(path, 'utf8'))
    if (
      value.repo !== repo ||
      value.limit !== limit ||
      !Number.isFinite(value.fetchedAt) ||
      !Array.isArray(value.changedLines) ||
      !value.changedLines.every((n) => Number.isSafeInteger(n) && n >= 0)
    )
      return null
    if (
      value.sample !== value.changedLines.length ||
      JSON.stringify(value.thresholds) !== JSON.stringify(sizeThresholds(value.changedLines))
    )
      return null
    return value
  } catch {
    return null
  }
}

/** Fetch this repo's merged-PR size distribution, reusing a daily cache.
 * @param {{read: Reader, repo: string, now?: number, cacheRoot?: string, limit?: number, ttlMs?: number}} options
 * @returns {SizeBaselineResult}
 */
export function readSizeBaseline({
  read,
  repo,
  now = Date.now(),
  cacheRoot = tmpdir(),
  limit = 500,
  ttlMs = 24 * 60 * 60 * 1000,
}) {
  /** @type {Cache|null} */
  let cached = null
  /** @param {Cache} value @param {boolean} stale @returns {SizeBaselineResult} */
  const result = (value, stale) => ({
    ok: true,
    thresholds: sizeThresholds(value.changedLines),
    sample: value.changedLines.length,
    fetchedAt: value.fetchedAt,
    stale,
  })
  try {
    if (!/^[a-z\d][a-z\d-]*\/[a-z\d_.-]+$/i.test(repo)) throw Error('Invalid repository name')
    const parent = privateParent(cacheRoot)
    const path = join(parent, `size-baseline-${repo.replace('/', '-')}.json`)
    cached = readCache(path, repo, limit)
    if (cached && now >= cached.fetchedAt && now - cached.fetchedAt < ttlMs)
      return result(cached, false)
    const response = read(
      'gh',
      [
        'pr',
        'list',
        '--repo',
        repo,
        '--state',
        'merged',
        '--limit',
        String(limit),
        '--json',
        'additions,deletions',
      ],
      { json: true },
    )
    if (!response.ok) throw Error(response.error ?? 'Merged PR history could not be read')
    const rows = response.payload
    if (
      !Array.isArray(rows) ||
      !rows.every(
        (row) =>
          row &&
          Number.isSafeInteger(row.additions) &&
          row.additions >= 0 &&
          Number.isSafeInteger(row.deletions) &&
          row.deletions >= 0 &&
          Number.isSafeInteger(row.additions + row.deletions),
      )
    ) {
      throw Error('Merged PR history has invalid changed-line counts')
    }
    const value = {
      repo,
      limit,
      changedLines: rows.map((row) => row.additions + row.deletions),
      fetchedAt: now,
    }
    const baseline = result(value, false)
    const temporary = join(parent, `.size-baseline-${randomUUID()}.tmp`)
    try {
      writeFileSync(
        temporary,
        JSON.stringify({
          ...value,
          sample: rows.length,
          thresholds: sizeThresholds(value.changedLines),
        }),
        { flag: 'wx', mode: 0o600 },
      )
      renameSync(temporary, path)
    } catch {
      // Cache persistence must not discard successfully collected evidence.
    } finally {
      try {
        unlinkSync(temporary)
      } catch {}
    }
    return baseline
  } catch (error) {
    return cached
      ? result(cached, true)
      : { ok: false, error: error instanceof Error ? error.message : String(error) }
  }
}
