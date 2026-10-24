import { spawnSync } from 'node:child_process'
import path from 'node:path'

export const NOT_A_CHECKOUT_EXIT = 3

/** Resolve repository paths once, before repository-dependent work starts. */
export function resolveCheckout({ cwd = process.cwd(), run = spawnSync } = {}) {
  const result = run('git', ['rev-parse', '--show-toplevel', '--git-common-dir'], {
    cwd,
    encoding: 'utf8',
    timeout: 30000,
  })
  const [root, commonDir] = String(result.stdout ?? '')
    .trim()
    .split(/\r?\n/)
  if (result.error || result.status !== 0 || !root || !commonDir) {
    const reason =
      result.error && 'code' in result.error && result.error.code === 'ENOENT'
        ? 'git-unavailable'
        : 'not-a-checkout'
    throw Object.assign(new Error(`${reason}: ${path.resolve(cwd)}`), {
      reason,
      exitCode: NOT_A_CHECKOUT_EXIT,
    })
  }
  return { root: path.resolve(cwd, root), commonDir: path.resolve(cwd, commonDir) }
}
