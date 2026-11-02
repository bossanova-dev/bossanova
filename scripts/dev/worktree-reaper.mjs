// Removes linked git worktrees nobody is using, with the Bazel output bases built from them.
// Removing a worktree keeps its branch, so committed work survives; the holds below protect what
// would not: uncommitted changes, commits no ref reaches, and a tree something is still using.
// Once a worktree has sat idle long enough, the first two are salvaged instead: its changes,
// untracked files and loose commits are saved under the salvage folder, then it is removed.

import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import * as fs from 'node:fs'
import { homedir } from 'node:os'
import { basename, dirname, join, sep } from 'node:path'

const DAY = 24 * 60 * 60 * 1000
const MAX_DEPTH = 3
const MAX_SALVAGE_BYTES = 256 * 1024 ** 2
// Churn the repository never commits, so it is not work that removal could lose.
const IGNORED_CHANGES = new Set(['go.work.sum'])
const UPSTREAMS = [
  'refs/remotes/origin/HEAD',
  'refs/remotes/origin/main',
  'refs/remotes/origin/master',
  'refs/heads/main',
  'refs/heads/master',
]

/**
 * @typedef {{ commonDir: string, path: string, branch: string | null, locked: boolean, prunable: boolean }} Worktree
 * @typedef {{ path: string, workspace: string, alive: boolean }} OutputBase
 * @typedef {Worktree & {
 *   changes: number,
 *   headOnRef: boolean,
 *   looseLanded: boolean,
 *   lastActiveMs: number,
 *   inUse: boolean | null,
 *   liveSession: boolean | null,
 *   bases: OutputBase[],
 * }} WorktreeFacts
 */

/**
 * Why a worktree stays. A salvageable hold guards work that salvage can save before removal.
 * @type {Array<{ reason: string, salvageable?: boolean, holds: (facts: WorktreeFacts, ctx: { now: number, minIdleMs: number }) => boolean }>}
 */
const HOLDS = [
  { reason: 'locked', holds: (f) => f.locked },
  { reason: 'uncommitted changes', salvageable: true, holds: (f) => f.changes > 0 },
  {
    reason: 'commits on no branch, tag or remote that main lacks',
    salvageable: true,
    holds: (f) => !f.headOnRef && !f.looseLanded,
  },
  { reason: 'a process is working in it', holds: (f) => f.inUse === true },
  { reason: 'process working directories could not be listed', holds: (f) => f.inUse === null },
  { reason: 'its Bazel server is running', holds: (f) => f.bases.some((base) => base.alive) },
  { reason: 'a live boss session is on its branch', holds: (f) => f.liveSession === true },
  { reason: 'boss sessions could not be listed', holds: (f) => f.liveSession === null },
  {
    reason: 'used recently',
    holds: (f, { now, minIdleMs }) => now - f.lastActiveMs < minIdleMs,
  },
]

/** The holds that keep a worktree; an empty list means it can go. */
export function holdsOn(facts, ctx) {
  return HOLDS.filter(({ holds }) => holds(facts, ctx))
}

/**
 * True when every commit on `rev` that the `excluded` refs do not reach has a patch-identical
 * commit on the upstream branch. With no upstream branch, nothing has landed.
 * @param {string} path
 * @param {string[]} [excluded] rev-list ref selectors whose commits count as safe already
 * @param {string} [rev]
 */
export function looseCommitsLanded(
  path,
  excluded = ['--branches', '--remotes', '--tags'],
  rev = 'HEAD',
) {
  const upstream = UPSTREAMS.find((ref) => {
    try {
      git(['rev-parse', '--verify', '-q', ref], path)
      return true
    } catch {
      return false
    }
  })
  if (!upstream) return false
  const loose = git(['rev-list', rev, '--not', ...excluded], path)
    .split('\n')
    .filter(Boolean)
  const landed = new Set(
    git(['cherry', upstream, rev], path)
      .split('\n')
      .filter((line) => line.startsWith('- '))
      .map((line) => line.slice(2)),
  )
  return loose.every((sha) => landed.has(sha))
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

function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

/**
 * Branches of the boss sessions that are not archived, or null when boss cannot answer. A host
 * with bossd state but no boss on PATH (launchd's minimal PATH) cannot answer either. Attempts
 * are spaced so that a bossd restart, which refuses calls for a few seconds, does not end the run.
 */
export function liveSessionBranches({
  env = process.env,
  home = homedir(),
  attempts = 4,
  delayMs = 20 * 1000,
  sleep = sleepSync,
} = {}) {
  for (let attempt = 1; attempt <= attempts; attempt++) {
    const branches = sessionBranchesOnce(env, home)
    if (branches !== null) return branches
    if (attempt < attempts) sleep(delayMs)
  }
  return null
}

// Only the local bossd owns worktrees on this host, so its session list is the one that matters.
// Without BOSS_SOCKET, boss starts a daemon of its own when it finds none, so never call it bare;
// and run it from home, because boss refuses to run inside a checkout newer than its binary.
function sessionBranchesOnce(env, home) {
  const bossdHost = fs.existsSync(join(home, '.bossanova'))
  if (!env.BOSS_SOCKET) return bossdHost ? null : new Set()
  try {
    const out = execFileSync(env.BOSS_BIN || 'boss', ['ls', '--json'], {
      cwd: home,
      env,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      timeout: 60 * 1000,
    })
    const parsed = JSON.parse(out)
    const sessions = Array.isArray(parsed) ? parsed : parsed.sessions
    return new Set(sessions.map((session) => session.branch).filter(Boolean))
  } catch (error) {
    if (error.code === 'ENOENT' && !bossdHost) return new Set()
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
  const changed = git(
    ['status', '--porcelain', '--untracked-files=all', '--ignore-submodules=none'],
    path,
  )
    .split('\n')
    .filter((line) => line && !IGNORED_CHANGES.has(line.slice(3)))
    .map((line) => line.slice(3).split(' -> ').pop())
  const changes = changed.length
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
  const looseLanded = !headOnRef && looseCommitsLanded(path)
  const gitDir = git(['rev-parse', '--absolute-git-dir'], path).trim()
  const bases = outputBases.filter(
    (base) => base.workspace === worktree.path || base.workspace === path,
  )
  // Editing a file is activity too, so a fresh edit in an old checkout is never salvaged away.
  const lastActiveMs = [
    mtimeMs(join(gitDir, 'HEAD')),
    lastReflogMs(gitDir),
    ...bases.map((base) => mtimeMs(join(base.path, 'command.log'))),
    ...changed.map((file) => mtimeMs(join(path, file))),
  ].reduce((latest, ms) => Math.max(latest, ms), 0)
  return {
    ...worktree,
    path,
    changes,
    headOnRef,
    looseLanded,
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
 * Save what removing a worktree would lose: tracked changes as a binary patch, untracked files as
 * a tarball and loose commits as a bundle. Throws, leaving nothing behind, if any part fails.
 */
export function salvageWorktree(facts, salvageDir, now = Date.now()) {
  const stamp = new Date(now).toISOString().slice(0, 10)
  const head = git(['rev-parse', 'HEAD'], facts.path).trim()
  // Worktrees under different roots share folder names and commits, so key the folder by path,
  // and create it exclusively so one salvage can never overwrite another.
  const id = createHash('sha256').update(facts.path).digest('hex').slice(0, 8)
  const dest = join(salvageDir, `${stamp}-${basename(facts.path)}-${id}`)
  fs.mkdirSync(salvageDir, { recursive: true })
  fs.mkdirSync(dest)
  try {
    // A patch records a changed submodule only as a gitlink, never the edits inside it.
    const submodule = git(['status', '--porcelain=v2', '--ignore-submodules=none'], facts.path)
      .split('\n')
      .find((line) => /^[12] \S{2} S/.test(line))
    if (submodule)
      throw new Error(`a submodule has changes salvage cannot save: ${submodule.split(' ').pop()}`)
    const patch = git(['diff', 'HEAD', '--binary'], facts.path)
    if (patch) fs.writeFileSync(join(dest, 'changes.patch'), patch)
    const untracked = git(['ls-files', '--others', '--exclude-standard', '-z'], facts.path)
    const files = untracked.split('\0').filter(Boolean)
    if (files.length > 0) {
      const bytes = files.reduce(
        (sum, file) => sum + (fs.lstatSync(join(facts.path, file)).size || 0),
        0,
      )
      if (bytes > MAX_SALVAGE_BYTES) throw new Error(`untracked files hold ${bytes} bytes`)
      execFileSync('tar', ['-czf', join(dest, 'untracked.tar.gz'), '--null', '-T', '-'], {
        cwd: facts.path,
        input: untracked,
        stdio: ['pipe', 'ignore', 'pipe'],
      })
    }
    if (!facts.headOnRef) {
      git(
        [
          'bundle',
          'create',
          join(dest, 'commits.bundle'),
          'HEAD',
          '--not',
          '--branches',
          '--remotes',
          '--tags',
        ],
        facts.path,
      )
    }
    const info = {
      path: facts.path,
      branch: facts.branch,
      head,
      salvagedAt: new Date(now).toISOString(),
    }
    fs.writeFileSync(join(dest, 'info.json'), `${JSON.stringify(info, null, 2)}\n`)
    return dest
  } catch (error) {
    fs.rmSync(dest, { recursive: true, force: true })
    throw error
  }
}

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
  salvageAfterMs = 30 * DAY,
  salvageDir = join(homedir(), '.cache', 'bossanova', 'reaped'),
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
      const holds = holdsOn(facts, ctx)
      const salvage =
        holds.length > 0 &&
        holds.every((hold) => hold.salvageable) &&
        now - facts.lastActiveMs >= salvageAfterMs
      if (holds.length > 0 && !salvage) {
        kept++
        write(`worktrees: kept ${facts.path}: ${holds.map((hold) => hold.reason).join(', ')}`)
        continue
      }
      const size = dryRun
        ? sizeKiB(facts.path) + facts.bases.reduce((sum, base) => sum + sizeKiB(base.path), 0)
        : 0
      const idleDays = Math.floor((now - facts.lastActiveMs) / DAY)
      const label = `${facts.path} (${facts.branch ?? 'detached'}, idle ${idleDays}d, ${facts.bases.length} bazel base${facts.bases.length === 1 ? '' : 's'})`
      const verb = salvage ? 'salvage and remove' : 'remove'
      if (dryRun) {
        reaped++
        reapedKiB += size
        write(`worktrees: would ${verb} ${label}, ${gib(size)}`)
        continue
      }
      let saved = ''
      if (salvage) {
        try {
          saved = `; saved its work to ${salvageWorktree(facts, salvageDir, now)}`
        } catch (error) {
          kept++
          write(
            `worktrees: kept ${facts.path}: could not salvage it (${error.message.split('\n')[0]})`,
          )
          continue
        }
      }
      try {
        git(['--git-dir', commonDir, 'worktree', 'remove', '--force', facts.path])
        if (fs.existsSync(facts.path)) removeTree(facts.path)
        for (const base of facts.bases) removeTree(base.path)
        reaped++
        write(`worktrees: removed ${label}${saved}`)
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
