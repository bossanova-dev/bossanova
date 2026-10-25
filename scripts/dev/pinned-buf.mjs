import { spawnSync } from 'node:child_process'
import path from 'node:path'

const makePlumbing = ['MAKEFLAGS', 'MFLAGS', 'MAKEOVERRIDES', 'GNUMAKEFLAGS', 'MAKELEVEL']

/** @param {NodeJS.ProcessEnv} env */
function withoutMakePlumbing(env) {
  const clean = { ...env }
  for (const key of makePlumbing) delete clean[key]
  return clean
}

/** @param {NodeJS.ProcessEnv} env */
export function withoutMakeOverrides(env) {
  const clean = withoutMakePlumbing(env)
  for (const key of ['BUF', 'BUF_CACHE_DIR', 'BUF_BREAKING_BASE']) delete clean[key]
  return clean
}

/** @typedef {{buf: string} | {error: string} | {skip: string}} BufResolution */
/** @type {Map<string, BufResolution>} */
const resolutions = new Map()

/**
 * Ask Make to provision and validate the same buf used by its proto recipes.
 * @param {{repoRoot: string, env?: NodeJS.ProcessEnv}} options
 * @returns {BufResolution}
 */
export function resolvePinnedBuf({ repoRoot, env = process.env }) {
  const childEnv = withoutMakePlumbing(env)
  const key = JSON.stringify([path.resolve(repoRoot), childEnv.BUF, childEnv.BUF_CACHE_DIR])
  const cached = resolutions.get(key)
  if (cached) return cached
  const result = spawnSync('make', ['-s', '--no-print-directory', 'buf-path'], {
    cwd: repoRoot,
    env: childEnv,
    encoding: 'utf8',
  })
  /** @type {BufResolution} */
  let resolution
  if (result.status === 0 && result.stdout.trim()) {
    const lastLine = result.stdout.trim().split('\n').at(-1)
    resolution = { buf: path.resolve(repoRoot, lastLine) }
  } else if (/buf version mismatch/.test(result.stderr || '')) {
    resolution = { error: result.stderr }
  } else {
    resolution = {
      skip: `make buf-check-version could not provision buf: ${result.stderr || result.error?.message || 'no buf path returned'}`,
    }
  }
  resolutions.set(key, resolution)
  return resolution
}
