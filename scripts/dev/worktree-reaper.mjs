// Removes linked git worktrees nobody is using, with the Bazel output bases built from them.
// Removing a worktree keeps its branch, so committed work survives; the holds below protect what
// would not: uncommitted changes, a HEAD no ref reaches, and a tree something is still using.

import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { homedir } from 'node:os'
import { dirname, join, sep } from 'node:path'

const DAY = 24 * 60 * 60 * 1000
const MAX_DEPTH = 3

/**
 * @typedef {{ commonDir: string, path: string, branch: string | null, locked: boolean, prunable: boolean }} Worktree
 * @typedef {{ path: string, workspace: string, alive: boolean }} OutputBase
 * @typedef {Worktree & {
 *   changes: number,
 *   headOnRef: boolean,
 *   lastActiveMs: number,
 *   inUse: boolean | null,
 *   liveSession: boolean | null,
 *   bases: OutputBase[],
 * }} WorktreeFacts
 */

/** @type {Array<[string, (facts: WorktreeFacts, ctx: { now: number, minIdleMs: number }) => boolean]>} */
const HOLDS = [
  ['locked', (f) => f.locked],
  ['uncommitted changes', (f) => f.changes > 0],
  ['HEAD is on no branch, tag or remote', (f) => !f.headOnRef],
  ['a process is working in it', (f) => f.inUse === true],
  ['process working directories could not be listed', (f) => f.inUse === null],
  ['its Bazel server is running', (f) => f.bases.some((base) => base.alive)],
  ['a live boss session is on its branch', (f) => f.liveSession === true],
  ['boss sessions could not be listed', (f) => f.liveSession === null],
  ['used recently', (f, { now, minIdleMs }) => now - f.lastActiveMs < minIdleMs],
]

/** The reasons to keep a worktree; an empty list means it can go. */
export function holdReasons(facts, ctx) {
  return HOLDS.filter(([, holds]) => holds(facts, ctx)).map(([reason]) => reason)
}

function git(args, cwd) {
  return execFileSync('git', args, {
    cwd,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'ignore'],
    maxBuffer: 64 << 20,
  })
}

/** When HEAD last moved, from its newest reflog entry; gc rewrites the file, so its mtime lies. */
function lastReflogMs(gitDir) {
  let log
  try {
    log = fs.readFileSync(join(gitDir, 'logs', 'HEAD'), 'utf8').trimEnd()
  } catch {
    return 0
  }
  const match = /> (\d+) [+-]\d{4}\t/.exec(log.slice(log.lastIndexOf('\n') + 1))
  return match ? Number(match[1]) * 1000 : 0
}

function mtimeMs(path) {
  try {
    return fs.statSync(path).mtimeMs
  } catch {
    return 0
  }
}

/** Git common dirs of every repository and worktree within MAX_DEPTH of each root. */
export function discoverRepos(roots) {
  const repos = new Set()
  const walk = (dir, depth) => {
    let names
    try {
      names = fs.readdirSync(dir)
    } catch {
      return
    }
    if (names.includes('.git')) {
      try {
        repos.add(
          fs.realpathSync(
            git(['rev-parse', '--path-format=absolute', '--git-common-dir'], dir).trim(),
          ),
        )
      } catch {
        // A worktree whose repository was deleted has nothing left to reap through git.
      }
      return
    }
    if (depth >= MAX_DEPTH) return
    for (const name of names) {
      if (name.startsWith('.') || name === 'node_modules') continue
      const path = join(dir, name)
      if (fs.lstatSync(path).isDirectory()) walk(path, depth + 1)
    }
  }
  for (const root of roots) walk(root, 0)
  return [...repos]
}

/** Linked worktrees of a repository, from `git worktree list --porcelain`; the main one is skipped. */
export function listWorktrees(commonDir) {
  const blocks = git(['--git-dir', commonDir, 'worktree', 'list', '--porcelain'])
    .trim()
    .split('\n\n')
  return blocks.slice(1).map((block) => {
    const fields = new Map(
      block.split('\n').map((line) => {
        const space = line.indexOf(' ')
        return space === -1 ? [line, ''] : [line.slice(0, space), line.slice(space + 1)]
      }),
    )
    const branch = fields.get('branch')
    return {
      commonDir,
      path: fields.get('worktree'),
      branch: branch ? branch.replace(/^refs\/heads\//, '') : null,
      locked: fields.has('locked'),
      prunable: fields.has('prunable'),
    }
  })
}

/** Working directories of every process this user can see, or null when lsof is unavailable. */
export function processCwds() {
  let out
  try {
    out = execFileSync('lsof', ['-nP', '-d', 'cwd', '-Fn'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      maxBuffer: 64 << 20,
    })
  } catch (error) {
    // lsof exits 1 when some processes are unreadable, after printing all the ones it could read.
    if (typeof error.stdout !== 'string' || error.stdout === '') return null
    out = error.stdout
  }
  return out
    .split('\n')
    .filter((line) => line.startsWith('n/'))
    .map((line) => line.slice(1))
}

/**
 * Branches of the boss sessions that are not archived, or null when boss cannot answer. A host
 * with bossd state but no boss on PATH (launchd's minimal PATH) cannot answer either.
 */
export function liveSessionBranches({ env = process.env, home = homedir(), attempts = 2 } = {}) {
  for (let attempt = 1; attempt <= attempts; attempt++) {
    const branches = sessionBranchesOnce(env, home)
    if (branches !== null) return branches
  }
  return null
}

function sessionBranchesOnce(env, home) {
  try {
    const out = execFileSync(env.BOSS_BIN || 'boss', ['ls', '--json', '--all-daemons'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      timeout: 60 * 1000,
    })
    const parsed = JSON.parse(out)
    const sessions = Array.isArray(parsed) ? parsed : parsed.sessions
    return new Set(sessions.map((session) => session.branch).filter(Boolean))
  } catch (error) {
    if (error.code === 'ENOENT' && !fs.existsSync(join(home, '.bossanova'))) return new Set()
    return null
  }
}

function isWithin(path, dir) {
  return path === dir || path.startsWith(dir + sep)
}

/**
 * @param {Worktree} worktree
 * @param {{ cwds: string[] | null, sessions: Set<string> | null, outputBases: OutputBase[] }} ctx
 * @returns {WorktreeFacts}
 */
export function worktreeFacts(worktree, { cwds, sessions, outputBases }) {
  const path = fs.realpathSync(worktree.path)
  const changes = git(
    ['status', '--porcelain', '--untracked-files=normal', '--ignore-submodules=none'],
    path,
  )
    .split('\n')
    .filter(Boolean).length
  const headOnRef =
    git(
      [
        'for-each-ref',
        '--contains',
        'HEAD',
        '--count=1',
        '--format=x',
        'refs/heads',
        'refs/remotes',
        'refs/tags',
      ],
      path,
    ).trim() !== ''
  const gitDir = git(['rev-parse', '--absolute-git-dir'], path).trim()
  const bases = outputBases.filter(
    (base) => base.workspace === worktree.path || base.workspace === path,
  )
  const lastActiveMs = Math.max(
    mtimeMs(join(gitDir, 'HEAD')),
    lastReflogMs(gitDir),
    ...bases.map((base) => mtimeMs(join(base.path, 'command.log'))),
  )
  return {
    ...worktree,
    path,
    changes,
    headOnRef,
    lastActiveMs,
    inUse: cwds === null ? null : cwds.some((cwd) => isWithin(cwd, path)),
    liveSession:
      sessions === null ? null : worktree.branch !== null && sessions.has(worktree.branch),
    bases,
  }
}

function kib(path) {
  try {
    return Number(
      execFileSync('du', ['-sk', path], {
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'ignore'],
      }).split('\t')[0],
    )
  } catch (error) {
    return Number(String(error.stdout ?? '').split('\t')[0]) || 0
  }
}

const gib = (kibibytes) => `${(kibibytes / 1024 ** 2).toFixed(1)} GiB`

/**
 * Remove idle, clean, unused worktrees under `roots` and their Bazel output bases. Every worktree
 * found is reported, kept ones with the reasons that held them.
 */
export function reapWorktrees({
  roots,
  outputBases,
  removeTree,
  dryRun = false,
  now = Date.now(),
  minIdleMs = 7 * DAY,
  cwds = processCwds(),
  sessions = liveSessionBranches(),
  sizeKiB = kib,
  write,
}) {
  const ctx = { now, minIdleMs }
  let reaped = 0
  let reapedKiB = 0
  let kept = 0
  for (const commonDir of discoverRepos(roots)) {
    for (const worktree of listWorktrees(commonDir)) {
      if (worktree.prunable) continue
      let facts
      try {
        facts = worktreeFacts(worktree, { cwds, sessions, outputBases })
      } catch (error) {
        kept++
        write(
          `worktrees: kept ${worktree.path}: could not inspect it (${error.message.split('\n')[0]})`,
        )
        continue
      }
      const reasons = holdReasons(facts, ctx)
      if (reasons.length > 0) {
        kept++
        write(`worktrees: kept ${facts.path}: ${reasons.join(', ')}`)
        continue
      }
      const size = dryRun
        ? sizeKiB(facts.path) + facts.bases.reduce((sum, base) => sum + sizeKiB(base.path), 0)
        : 0
      const idleDays = Math.floor((now - facts.lastActiveMs) / DAY)
      const label = `${facts.path} (${facts.branch ?? 'detached'}, idle ${idleDays}d, ${facts.bases.length} bazel base${facts.bases.length === 1 ? '' : 's'})`
      if (dryRun) {
        reaped++
        reapedKiB += size
        write(`worktrees: would remove ${label}, ${gib(size)}`)
        continue
      }
      try {
        git(['--git-dir', commonDir, 'worktree', 'remove', '--force', facts.path])
        if (fs.existsSync(facts.path)) removeTree(facts.path)
        for (const base of facts.bases) removeTree(base.path)
        reaped++
        write(`worktrees: removed ${label}`)
      } catch (error) {
        kept++
        write(`worktrees: could not remove ${facts.path}: ${error.message.split('\n')[0]}`)
      }
    }
    try {
      if (!dryRun) git(['--git-dir', commonDir, 'worktree', 'prune'])
    } catch {
      // Pruning only drops records of worktrees already gone; a failure leaves them for next time.
    }
  }
  const total = dryRun ? `, ${gib(reapedKiB)}` : ''
  write(`worktrees: ${dryRun ? 'would remove' : 'removed'} ${reaped}${total}; kept ${kept}`)
}

/** Repository roots to search: the folder holding this checkout's repository, and bossd's worktrees. */
export function defaultWorktreeRoots({
  env = process.env,
  home = homedir(),
  cwd = process.cwd(),
} = {}) {
  if (env.RECOVER_WORKTREE_ROOTS) return env.RECOVER_WORKTREE_ROOTS.split(':').filter(Boolean)
  const roots = [join(home, '.bossanova', 'worktrees')]
  try {
    const commonDir = git(['rev-parse', '--path-format=absolute', '--git-common-dir'], cwd).trim()
    roots.unshift(dirname(dirname(commonDir)))
  } catch {
    // Outside a repository there is no sibling folder to search.
  }
  return roots
}
