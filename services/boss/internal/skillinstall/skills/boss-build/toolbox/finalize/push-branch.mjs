// skills-toolbox/finalize/push-branch.mjs
// Persist the current branch to its remote so finished work leaves the worktree. node builtins
// only (the cron worktree is dependency-free).
//
//   node toolbox/finalize/push-branch.mjs --branch <name> [--remote origin]
//       [--attempts 8] [--initial-delay 5]
//     -> prints {"pushed":"yes|rescue|no","attempts":N,"rescue":"<ref>|null","head":"<sha>",
//        "remoteBefore":"<sha>|null"} and exits 0 on "yes", 1 otherwise.
//
// Outcomes:
//   yes     the remote branch contains HEAD — already (nothing to send) or after a push.
//   rescue  the branch could not be pushed within the attempts, but HEAD was pushed to a fresh
//           `<branch>-blocked-<sha>` ref, which cannot be rejected as non-fast-forward.
//   no      neither the branch nor the rescue ref could be pushed; the work is still only local.
//
// A rejected push is reconciled with `git rebase --no-fork-point FETCH_HEAD`, never a merge and
// never `git pull --rebase`: pull's fork-point heuristic reads the old remote-tracking reflog and,
// after a server-side force-push, silently drops this run's commits. Never force-pushes.

import { execFileSync } from 'node:child_process'
import { isMainModule } from '../main-module.mjs'

function git(args, opts = {}) {
  return execFileSync('git', args, {
    cwd: opts.cwd,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', opts.quiet ? 'ignore' : 'pipe'],
  }).trim()
}

function tryGit(args, opts = {}) {
  try {
    return { ok: true, out: git(args, { ...opts, quiet: true }) }
  } catch {
    return { ok: false, out: '' }
  }
}

function sleepSeconds(seconds) {
  if (seconds > 0) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, seconds * 1000)
}

/**
 * The SHA the remote advertises for `branch`, or null when it advertises none or cannot be read.
 * @returns {{readable: boolean, sha: string|null}}
 */
export function remoteBranchSha({ remote, branch, cwd }) {
  const ref = `refs/heads/${branch}`
  const res = tryGit(['ls-remote', remote, ref], { cwd })
  if (!res.ok) return { readable: false, sha: null }
  for (const line of res.out.split('\n')) {
    const [sha, name] = line.split('\t')
    if (name === ref && /^[0-9a-f]{7,64}$/.test(sha ?? '')) return { readable: true, sha }
  }
  return { readable: true, sha: null }
}

/**
 * Whether the remote branch already contains HEAD. Containment, not equality: a remote that moved
 * ahead of HEAD (someone pushed on top) still holds every commit this run built. Fails closed — any
 * unreadable state is "not contained", which only costs a push.
 */
export function remoteContainsHead({ remote, branch, head, remoteSha, cwd }) {
  if (!head || !remoteSha) return false
  if (remoteSha === head) return true
  if (!tryGit(['fetch', '-q', remote, branch], { cwd }).ok) return false
  return tryGit(['merge-base', '--is-ancestor', head, 'FETCH_HEAD'], { cwd }).ok
}

/**
 * Push `branch` with bounded retries, reconciling a rejection by rebase, and fall back to a unique
 * rescue ref.
 * @returns {{pushed: 'yes'|'rescue'|'no', attempts: number, rescue: string|null, head: string|null, remoteBefore: string|null}}
 */
export function pushBranch({
  branch,
  remote = 'origin',
  attempts = 8,
  initialDelay = 5,
  cwd,
  sleep = sleepSeconds,
} = {}) {
  if (!branch) throw new Error('push-branch: --branch is required')
  const headOf = () => tryGit(['rev-parse', 'HEAD'], { cwd }).out || null
  const before = remoteBranchSha({ remote, branch, cwd })
  const head = headOf()
  if (remoteContainsHead({ remote, branch, head, remoteSha: before.sha, cwd })) {
    return { pushed: 'yes', attempts: 0, rescue: null, head, remoteBefore: before.sha }
  }

  let made = 0
  let delay = initialDelay
  while (made < attempts) {
    made += 1
    if (tryGit(['push', '-u', remote, branch], { cwd }).ok) {
      return {
        pushed: 'yes',
        attempts: made,
        rescue: null,
        head: headOf(),
        remoteBefore: before.sha,
      }
    }
    // Rejected (or unreachable). Reconcile onto what the remote holds and retry; a failed
    // reconcile is usually transient, so it never ends the loop — it only must not leave a
    // half-finished rebase for the next attempt.
    const fetched = tryGit(['fetch', '-q', remote, branch], { cwd }).ok
    if (fetched && !tryGit(['rebase', '--no-fork-point', 'FETCH_HEAD'], { cwd }).ok) {
      tryGit(['rebase', '--abort'], { cwd })
    }
    if (made < attempts) {
      sleep(delay)
      delay = Math.min(delay * 2, 60)
    }
  }

  const finalHead = headOf()
  const suffix = (finalHead || String(process.pid)).slice(0, 12)
  const rescue = `${branch}-blocked-${suffix}`
  if (tryGit(['push', remote, `HEAD:refs/heads/${rescue}`], { cwd }).ok) {
    return { pushed: 'rescue', attempts: made, rescue, head: finalHead, remoteBefore: before.sha }
  }
  return { pushed: 'no', attempts: made, rescue: null, head: finalHead, remoteBefore: before.sha }
}

function parseArgs(argv) {
  const opts = {}
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    const value = argv[i + 1]
    if (flag === '--branch') opts.branch = value
    else if (flag === '--remote') opts.remote = value
    else if (flag === '--attempts') opts.attempts = Number(value)
    else if (flag === '--initial-delay') opts.initialDelay = Number(value)
    else throw new Error(`push-branch: unknown argument ${flag}`)
    i += 1
  }
  if (opts.attempts !== undefined && !(Number.isInteger(opts.attempts) && opts.attempts > 0)) {
    throw new Error('push-branch: --attempts must be a positive integer')
  }
  if (opts.initialDelay !== undefined && !(opts.initialDelay >= 0)) {
    throw new Error('push-branch: --initial-delay must be a non-negative number')
  }
  return opts
}

if (isMainModule(import.meta.url)) {
  try {
    const result = pushBranch(parseArgs(process.argv.slice(2)))
    process.stdout.write(`${JSON.stringify(result)}\n`)
    process.exit(result.pushed === 'yes' ? 0 : 1)
  } catch (error) {
    process.stderr.write(`${error.message}\n`)
    process.exit(2)
  }
}
