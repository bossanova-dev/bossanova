// skills-toolbox/finalize/push-branch.mjs
// Persist the current branch to its remote so finished work leaves the worktree. node builtins
// only (the cron worktree is dependency-free).
//
//   node toolbox/finalize/push-branch.mjs --branch <name> [--remote origin]
//       [--base <ref>] [--expect-remote <full-sha>] [--attempts 8] [--initial-delay 5]
//     -> prints {pushed: "yes|rescue|no", attempts, rescue, head, remoteBefore,
//        refused: null|"superseded"|"replays-base"|"lease-stale", remoteTip}
//        and exits 0 on "yes", 1 otherwise (usage errors exit 2).
//
// Outcomes: yes means the remote contains HEAD; rescue means HEAD reached a unique
// `<branch>-blocked-<sha>` ref; no means no push succeeded or superseded history needs a lease.
// Default mode retries transient failures, refuses superseded remote history, and (with --base)
// rescues rather than replaying base commits. Otherwise it reconciles genuine peer work by rebase.
// Never use `git pull --rebase`: its fork-point heuristic can silently drop this run's commits
// after a server-side force-push. Reconcile with `git rebase --no-fork-point FETCH_HEAD` instead.
// Force only with an exact caller-supplied lease: --expect-remote never rebase-reconciles.
// A squash with new content cannot be proven superseded; its owner should supply a lease.

import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
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

// Remote commits are replaceable only when their content is already in HEAD. Empty commits
// need an explicit check: cherry-mark reports them as unmatched even when they add no content.
function supersededRemote(remoteTip, cwd) {
  if (
    git(['rev-parse', `${remoteTip}^{tree}`], { cwd }) ===
    git(['rev-parse', 'HEAD^{tree}'], { cwd })
  )
    return true
  const equivalent = new Set(
    git(['rev-list', '--right-only', '--cherry-mark', `HEAD...${remoteTip}`], { cwd })
      .split('\n')
      .filter((line) => line.startsWith('='))
      .map((line) => line.slice(1)),
  )
  return git(['rev-list', `HEAD..${remoteTip}`], { cwd })
    .split('\n')
    .filter(Boolean)
    .every((sha) => {
      if (equivalent.has(sha)) return true
      const parentTree = tryGit(['rev-parse', `${sha}^1^{tree}`], { cwd })
      return parentTree.ok && parentTree.out === git(['rev-parse', `${sha}^{tree}`], { cwd })
    })
}

/**
 * Push with bounded retries, classifying remote history before reconciliation or leasing an
 * explicit remote tip. Failed pushes fall back to a unique rescue ref except superseded history.
 * @param {{branch?: string, remote?: string, attempts?: number, initialDelay?: number,
 *   base?: string, expectRemote?: string, cwd?: string, sleep?: (seconds: number) => void}} [options]
 * @returns {{pushed: 'yes'|'rescue'|'no', attempts: number, rescue: string|null, head: string|null, remoteBefore: string|null,
 *   refused: null|'superseded'|'replays-base'|'lease-stale', remoteTip: string|null}}
 */
export function pushBranch({
  branch,
  remote = 'origin',
  attempts = 8,
  initialDelay = 5,
  base,
  expectRemote,
  cwd,
  sleep = sleepSeconds,
} = {}) {
  if (!branch) throw new Error('push-branch: --branch is required')
  if (expectRemote !== undefined && !/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/i.test(expectRemote)) {
    throw new Error('push-branch: --expect-remote must be a full 40- or 64-hex SHA')
  }
  expectRemote = expectRemote?.toLowerCase()
  /** @type {null|'superseded'|'replays-base'|'lease-stale'} */
  let refused = null
  let remoteTip = expectRemote || null
  const headOf = () => tryGit(['rev-parse', 'HEAD'], { cwd }).out || null
  const before = remoteBranchSha({ remote, branch, cwd })
  const head = headOf()
  if (remoteContainsHead({ remote, branch, head, remoteSha: before.sha, cwd })) {
    return {
      pushed: 'yes',
      attempts: 0,
      rescue: null,
      head,
      remoteBefore: before.sha,
      refused,
      remoteTip,
    }
  }

  let made = 0
  let delay = initialDelay
  while (made < attempts) {
    made += 1
    const pushArgs = expectRemote
      ? ['push', `--force-with-lease=refs/heads/${branch}:${expectRemote}`, '-u', remote, branch]
      : ['push', '-u', remote, branch]
    if (tryGit(pushArgs, { cwd }).ok) {
      return {
        pushed: 'yes',
        attempts: made,
        rescue: null,
        head: headOf(),
        remoteBefore: before.sha,
        refused,
        remoteTip,
      }
    }
    if (expectRemote) {
      const observed = remoteBranchSha({ remote, branch, cwd })
      if (observed.readable) {
        remoteTip = observed.sha
        if (observed.sha !== expectRemote) {
          refused = 'lease-stale'
          break
        }
      }
    } else if (tryGit(['fetch', '-q', remote, branch], { cwd }).ok) {
      remoteTip = git(['rev-parse', 'FETCH_HEAD'], { cwd })
      if (!tryGit(['merge-base', '--is-ancestor', remoteTip, 'HEAD'], { cwd }).ok) {
        if (supersededRemote(remoteTip, cwd)) {
          return {
            pushed: 'no',
            attempts: made,
            rescue: null,
            head: headOf(),
            remoteBefore: before.sha,
            refused: 'superseded',
            remoteTip,
          }
        }
        if (
          base &&
          git(['rev-list', '--count', `${remoteTip}..HEAD`], { cwd }) !==
            git(['rev-list', '--count', `${remoteTip}..HEAD`, `^${base}`], { cwd })
        ) {
          refused = 'replays-base'
          break
        }
        if (!tryGit(['rebase', '--no-fork-point', 'FETCH_HEAD'], { cwd }).ok) {
          tryGit(['rebase', '--abort'], { cwd })
        }
      }
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
    return {
      pushed: 'rescue',
      attempts: made,
      rescue,
      head: finalHead,
      remoteBefore: before.sha,
      refused,
      remoteTip,
    }
  }
  return {
    pushed: 'no',
    attempts: made,
    rescue: null,
    head: finalHead,
    remoteBefore: before.sha,
    refused,
    remoteTip,
  }
}

function parseOptions(argv) {
  const { values } = parseArgs({
    args: argv,
    strict: true,
    options: {
      branch: { type: 'string' },
      remote: { type: 'string' },
      base: { type: 'string' },
      'expect-remote': { type: 'string' },
      attempts: { type: 'string' },
      'initial-delay': { type: 'string' },
    },
  })
  const opts = {
    branch: values.branch,
    remote: values.remote,
    base: values.base,
    expectRemote: values['expect-remote'],
    attempts: values.attempts === undefined ? undefined : Number(values.attempts),
    initialDelay:
      values['initial-delay'] === undefined ? undefined : Number(values['initial-delay']),
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
    const result = pushBranch(parseOptions(process.argv.slice(2)))
    process.stdout.write(`${JSON.stringify(result)}\n`)
    process.exit(result.pushed === 'yes' ? 0 : 1)
  } catch (error) {
    process.stderr.write(`${error.message}\n`)
    process.exit(2)
  }
}
