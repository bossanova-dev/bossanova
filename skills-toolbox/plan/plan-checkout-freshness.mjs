import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'

import { countBehind, resolveCommit } from '../base-drift.mjs'
import { isMainModule } from '../main-module.mjs'

/** @typedef {{status: number, stdout: string, stderr: string}} GitResult */
/** @typedef {(repo: string, args: string[]) => GitResult} GitRunner */

/** @type {GitRunner} */
function runGit(repo, args) {
  const r = spawnSync('git', args, {
    cwd: repo,
    encoding: 'utf8',
    timeout: 60_000,
    env: { ...process.env, GIT_TERMINAL_PROMPT: '0' },
  })
  return { status: r.error ? -1 : (r.status ?? -1), stdout: r.stdout || '', stderr: r.stderr || '' }
}

/**
 * Refresh a clean base checkout, or tell recon which fetched ref to read.
 * @param {{repo?: string, base?: string, remote?: string, run?: GitRunner}} [options]
 */
export function checkoutFreshness({
  repo = process.cwd(),
  base,
  remote = 'origin',
  run = runGit,
} = {}) {
  if (!base) {
    const symbolic = run(repo, ['symbolic-ref', '--short', `refs/remotes/${remote}/HEAD`])
    if (symbolic.status === 0 && symbolic.stdout.trim().startsWith(`${remote}/`)) {
      base = symbolic.stdout.trim().slice(remote.length + 1)
    }
  }
  const remoteRef = base ? `${remote}/${base}` : null
  const result = {
    verdict: 'unevaluated',
    base: base || null,
    remote,
    remoteRef,
    fetchFailed: false,
    behindBefore: /** @type {number | 'unevaluated'} */ ('unevaluated'),
    behind: /** @type {number | 'unevaluated'} */ ('unevaluated'),
    ahead: /** @type {number | 'unevaluated'} */ ('unevaluated'),
    fastForward: { attempted: false, outcome: 'none', reason: /** @type {string|null} */ (null) },
    reconRef: 'HEAD',
    warning: /** @type {string|null} */ (null),
  }
  if (!base || !remoteRef) {
    result.fastForward.reason = 'base-unresolved'
    result.warning = 'Checkout freshness could not be evaluated: base-unresolved; supply --base.'
    return result
  }
  const fetch = run(repo, [
    'fetch',
    '--quiet',
    remote,
    `+refs/heads/${base}:refs/remotes/${remote}/${base}`,
  ])
  result.fetchFailed = fetch.status !== 0
  result.behindBefore = countBehind({ repo, base: remoteRef, run })
  result.behind = result.behindBefore
  result.ahead = countBehind({ repo, base: 'HEAD', head: remoteRef, run })
  if (
    typeof result.behind === 'number' &&
    result.behind > 0 &&
    resolveCommit({ repo, rev: remoteRef, run })
  ) {
    result.reconRef = remoteRef
  }
  if (result.fetchFailed || typeof result.behind !== 'number' || typeof result.ahead !== 'number') {
    result.warning = `Checkout freshness could not be evaluated: ${result.fetchFailed ? 'fetch failed' : 'count unreadable'}.`
    return result
  }
  if (result.behind === 0) {
    result.verdict = 'fresh'
    return result
  }
  const branch = run(repo, ['symbolic-ref', '--short', '-q', 'HEAD'])
  const status = run(repo, ['status', '--porcelain=v1', '-z', '--untracked-files=no'])
  if (status.status !== 0) {
    result.warning = 'Checkout freshness could not be evaluated: tracked changes unreadable.'
    return result
  }
  let reason = null
  if (result.ahead > 0) reason = 'diverged'
  else if (branch.status !== 0) reason = 'detached'
  else if (branch.stdout.trim() !== base) reason = 'off-branch'
  else if (status.stdout !== '') reason = 'dirty'
  if (reason) {
    result.fastForward.outcome = 'skipped'
    result.fastForward.reason = reason
  } else {
    result.fastForward.attempted = true
    const ff = run(repo, ['merge', '--ff-only', '--quiet', remoteRef])
    result.fastForward.outcome = ff.status === 0 ? 'fast-forwarded' : 'skipped'
    result.fastForward.reason = ff.status === 0 ? null : 'ff-failed'
    result.behind = countBehind({ repo, base: remoteRef, run })
    if (typeof result.behind !== 'number') {
      result.warning = 'Checkout freshness could not be evaluated: post-refresh count unreadable.'
      return result
    }
    if (ff.status === 0 && result.behind === 0) {
      result.verdict = 'refreshed'
      result.reconRef = 'HEAD'
      return result
    }
  }
  result.verdict = 'stale'
  result.warning = `Checkout is ${result.behind} commit(s) behind ${remoteRef}; fast-forward ${result.fastForward.reason || 'incomplete'}. Read code at ${remoteRef}.`
  return result
}

const usage =
  'Usage: plan-checkout-freshness.mjs check [--base <branch>] [--remote <name>] [--repo <dir>]'
if (isMainModule(import.meta.url)) {
  try {
    const { values, positionals } = parseArgs({
      strict: true,
      allowPositionals: true,
      options: {
        base: { type: 'string' },
        remote: { type: 'string' },
        repo: { type: 'string' },
        help: { type: 'boolean', short: 'h' },
      },
    })
    const [cmd] = positionals
    if (values.help) console.log(usage)
    else if (positionals.length !== 1 || cmd !== 'check') {
      console.error(usage)
      process.exitCode = 2
    } else {
      const result = checkoutFreshness(values)
      console.log(JSON.stringify(result))
      console.error(
        result.warning || `Checkout freshness: ${result.verdict} (${result.remoteRef}).`,
      )
    }
  } catch (error) {
    console.error(`${error instanceof Error ? error.message : error}\n${usage}`)
    process.exitCode = 2
  }
}
