#!/usr/bin/env node

// Removes Claude Code subagent worktrees (`isolation: "worktree"`) once their work has landed.
// Claude Code removes such a worktree only when the subagent changed nothing; one that committed
// stays at <repo>/.claude/worktrees/agent-<id> on branch worktree-agent-<id> (or detached from it)
// for good. This pass removes the tree, its Bazel output bases and that branch once every commit on
// them is on a remote ref or patch-identical upstream, nothing uses the tree, and both it and the
// session that spawned
// it have been quiet for RECOVER_AGENT_WORKTREE_GRACE_MINUTES (60). The session comes from the
// subagent's meta file under Claude Code's projects folder; without one the tree is held, and the
// general reaper (worktree-reaper.mjs) stays its backstop. Removal is forced, so any doubt keeps the
// tree: an ignored file that is not a regenerable cache, a subagent record without finished: true,
// or another worktree inside it.
// `--detach` is the hook form: rate-limited, silent, always exit 0, with the pass in a detached
// child that appends to ~/.cache/bossanova/agent-worktree-reaper.log. `--dry-run` removes nothing.

import { execFileSync, spawn as spawnProcess } from 'node:child_process'
import { createHash } from 'node:crypto'
import * as fs from 'node:fs'
import { homedir } from 'node:os'
import { basename, dirname, join, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { bazelOutputUserRoots, readOutputBases, removeTree } from './recover-disk-space.mjs'
import {
  listWorktrees,
  looseCommitsLanded,
  processCwds,
  worktreeFacts,
} from './worktree-reaper.mjs'

const MINUTE = 60 * 1000
const DEFAULT_GRACE_MINUTES = 60
const RATE_LIMIT_MS = 10 * MINUTE
const STALE_CLAIM_MS = 30 * MINUTE
const MAX_LOG_BYTES = 1024 ** 2
const CLAIM_ENV = 'AGENT_WORKTREE_REAPER_CLAIM'
const AGENT_DIR = /^agent-([A-Za-z0-9_-]+)$/
const META_FILE = /^agent-([A-Za-z0-9_-]+)\.meta\.json$/
// Ignored content any checkout regenerates (dependencies, build and test outputs, generated hooks),
// so a forced removal loses nothing. Every other ignored file, such as verification evidence,
// handoff notes, plans or a .env, may be the only copy of a subagent's work.
const REGENERABLE_DIRS = new Set([
  'node_modules',
  '.turbo',
  'bin',
  'build',
  'dist',
  'coverage',
  'test-results',
  'playwright-report',
])
const REGENERABLE_FILES = new Set(['.generate.stamp', '.DS_Store'])

/**
 * @typedef {import('./worktree-reaper.mjs').WorktreeFacts} WorktreeFacts
 * @typedef {import('./worktree-reaper.mjs').OutputBase} OutputBase
 * @typedef {{ metaPath: string, worktreePath: string, branch: string, unfinished: boolean, parentLastActiveMs: number }} AgentMeta
 * @typedef {WorktreeFacts & {
 *   landed: boolean,
 *   ignoredWork: string[],
 *   holdsWorktree: boolean,
 *   meta: AgentMeta | null,
 *   metaNamesIt: boolean,
 *   doomedBranch: string | null,
 * }} AgentFacts
 */

function git(args, cwd) {
  return execFileSync('git', args, {
    cwd,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'ignore'],
    maxBuffer: 64 << 20,
  })
}

function mtimeMs(path) {
  try {
    return fs.statSync(path).mtimeMs
  } catch {
    return 0
  }
}

/**
 * Whether an ignored path, as `git status --ignored=matching` names it (a folder ends in a slash),
 * is a regenerable cache: it lies in a REGENERABLE_DIRS folder, is husky's generated hook folder, or
 * is one of REGENERABLE_FILES. Bazel's top-level links are checked on disk, in ignoredWork.
 */
export function isRegenerable(entry) {
  const parts = entry.replace(/\/$/, '').split('/')
  const folders = entry.endsWith('/') ? parts : parts.slice(0, -1)
  if (folders.some((part) => REGENERABLE_DIRS.has(part))) return true
  if (parts[0] === '.husky' && parts[1] === '_') return true
  return REGENERABLE_FILES.has(parts[parts.length - 1])
}

/** Ignored files and folders in the worktree at `path` that are not regenerable caches. */
export function ignoredWork(path) {
  // `matching` names each path an ignore rule matched, a matched folder once, and nothing else.
  return git(
    ['status', '--porcelain', '-z', '--ignored=matching', '--untracked-files=normal'],
    path,
  )
    .split('\0')
    .filter((line) => line.startsWith('!! '))
    .map((line) => line.slice(3))
    .filter((entry) => !isRegenerable(entry) && !isBazelLink(path, entry))
}

/** A top-level bazel-* symlink; a real file or folder of that name may hold work. */
function isBazelLink(root, entry) {
  return /^bazel-[^/]+$/.test(entry) && fs.lstatSync(join(root, entry)).isSymbolicLink()
}

function isWithin(path, dir) {
  return path === dir || path.startsWith(dir + sep)
}

function realpathOr(path) {
  try {
    return fs.realpathSync(path)
  } catch {
    return path
  }
}

/** RECOVER_AGENT_WORKTREE_GRACE_MINUTES, or 60 when it is unset, blank, not a number or negative. */
export function graceMinutesFrom(env) {
  const raw = (env.RECOVER_AGENT_WORKTREE_GRACE_MINUTES ?? '').trim()
  const minutes = raw === '' ? Number.NaN : Number(raw)
  return Number.isFinite(minutes) && minutes >= 0 ? minutes : DEFAULT_GRACE_MINUTES
}

function readdir(path) {
  try {
    return fs.readdirSync(path)
  } catch {
    return []
  }
}

/**
 * The subagent id when the worktree is a Claude Code isolation worktree, else null: its folder is
 * .claude/worktrees/agent-<id> and it is on branch worktree-agent-<id> or detached, as a subagent
 * that rebased or checked out a commit leaves it.
 */
export function agentId(worktree) {
  const match = AGENT_DIR.exec(basename(worktree.path))
  if (!match || !dirname(worktree.path).endsWith(join('.claude', 'worktrees'))) return null
  const own = worktree.branch === null || worktree.branch === `worktree-agent-${match[1]}`
  return own ? match[1] : null
}

/**
 * The worktree-agent branch to delete with the tree, or null to keep it. A checked-out branch is
 * HEAD, which the landed hold already proved; a detached tree's branch must prove it on its own.
 */
function branchToDelete(path, branch, meta, id) {
  if (meta === null || meta.branch !== `worktree-agent-${id}`) return null
  if (branch !== null) return branch === meta.branch ? branch : null
  try {
    git(['rev-parse', '--verify', '-q', `refs/heads/${meta.branch}`], path)
  } catch {
    return null
  }
  return looseCommitsLanded(path, ['--remotes'], `refs/heads/${meta.branch}`) ? meta.branch : null
}

/**
 * Meta files of the subagents named by `ids`, from one readdir per session `subagents/` folder.
 * A parent's last activity is the newest mtime of its `<session>.jsonl` and of every file in its
 * `subagents/` folder, which covers a nested subagent that spawned this one. When two meta files
 * claim one id, the one with the most recent parent wins, so the hold errs towards keeping.
 * @param {{ projectsRoots: string[], ids: Set<string> }} options
 * @returns {Map<string, AgentMeta>}
 */
export function findAgentMeta({ projectsRoots, ids }) {
  /** @type {Map<string, AgentMeta>} */
  const found = new Map()
  if (ids.size === 0) return found
  for (const root of projectsRoots) {
    for (const slug of readdir(root)) {
      for (const session of readdir(join(root, slug))) {
        if (session.endsWith('.jsonl')) continue
        const subagents = join(root, slug, session, 'subagents')
        const names = readdir(subagents)
        const wanted = names.filter((name) => ids.has(META_FILE.exec(name)?.[1] ?? ''))
        if (wanted.length === 0) continue
        const parentLastActiveMs = Math.max(
          mtimeMs(join(root, slug, `${session}.jsonl`)),
          ...names.map((name) => mtimeMs(join(subagents, name))),
        )
        for (const name of wanted) {
          const metaPath = join(subagents, name)
          let meta
          try {
            meta = JSON.parse(fs.readFileSync(metaPath, 'utf8'))
          } catch {
            continue
          }
          if (meta?.spawnedWithWorktree !== true || typeof meta.worktreePath !== 'string') continue
          const id = META_FILE.exec(name)?.[1] ?? ''
          const prior = found.get(id)
          if (prior && prior.parentLastActiveMs >= parentLastActiveMs) continue
          found.set(id, {
            metaPath,
            worktreePath: meta.worktreePath,
            branch: typeof meta.worktreeBranch === 'string' ? meta.worktreeBranch : '',
            // Claude Code writes finished: true when the subagent ends and omits it while it runs;
            // a record from before the field existed also lacks it, so that tree waits too.
            unfinished: meta.finished !== true,
            parentLastActiveMs,
          })
        }
      }
    }
  }
  return found
}

/**
 * Why an agent worktree stays.
 * @type {Array<{ reason: string, holds: (facts: AgentFacts, ctx: { now: number, graceMs: number }) => boolean }>}
 */
const HOLDS = [
  { reason: 'locked', holds: (f) => f.locked },
  { reason: 'uncommitted changes', holds: (f) => f.changes > 0 },
  {
    reason: 'ignored files that are not regenerable caches',
    holds: (f) => f.ignoredWork.length > 0,
  },
  { reason: 'another worktree is inside it', holds: (f) => f.holdsWorktree },
  { reason: 'commits not on a remote or patch-identical upstream', holds: (f) => !f.landed },
  { reason: 'a process is working in it', holds: (f) => f.inUse === true },
  { reason: 'process working directories could not be listed', holds: (f) => f.inUse === null },
  { reason: 'its Bazel server is running', holds: (f) => f.bases.some((base) => base.alive) },
  { reason: 'used recently', holds: (f, { now, graceMs }) => now - f.lastActiveMs < graceMs },
  { reason: 'parent session unknown', holds: (f) => f.meta === null },
  { reason: 'its subagent has not finished', holds: (f) => f.meta?.unfinished === true },
  {
    reason: 'its subagent record names another worktree',
    holds: (f) => f.meta !== null && !f.metaNamesIt,
  },
  {
    reason: 'parent session active recently',
    holds: (f, { now, graceMs }) => f.meta !== null && now - f.meta.parentLastActiveMs < graceMs,
  },
]

/** The reasons an agent worktree stays; an empty list means it can go. */
export function agentWorktreeHolds(facts, ctx) {
  return HOLDS.filter(({ holds }) => holds(facts, ctx)).map(({ reason }) => reason)
}

/** Default Claude Code projects folders: the configured one and the default one. */
export function defaultProjectsRoots({ env = process.env, home = homedir() } = {}) {
  if (env.RECOVER_CLAUDE_PROJECTS_DIRS) {
    return env.RECOVER_CLAUDE_PROJECTS_DIRS.split(':').filter(Boolean)
  }
  const roots = [join(home, '.claude', 'projects')]
  if (env.CLAUDE_CONFIG_DIR) roots.unshift(join(env.CLAUDE_CONFIG_DIR, 'projects'))
  return [...new Set(roots)]
}

/**
 * Remove the landed agent worktrees of the repository at `repoRoot`. Only agent worktrees are
 * reported; every other worktree is the general reaper's.
 * @param {{
 *   repoRoot: string,
 *   projectsRoots: string[],
 *   outputBases?: OutputBase[],
 *   removeTree: (path: string) => void,
 *   cwds?: string[] | null | (() => string[] | null),
 *   now?: number,
 *   graceMs?: number,
 *   dryRun?: boolean,
 *   write: (line: string) => void,
 * }} options
 */
export function reapAgentWorktrees({
  repoRoot,
  projectsRoots,
  outputBases,
  removeTree,
  cwds,
  now = Date.now(),
  graceMs = DEFAULT_GRACE_MINUTES * MINUTE,
  dryRun = false,
  write,
}) {
  const commonDir = git(
    ['rev-parse', '--path-format=absolute', '--git-common-dir'],
    repoRoot,
  ).trim()
  const agents = listWorktrees(commonDir)
    .filter((worktree) => !worktree.prunable)
    .map((worktree) => ({ worktree, id: agentId(worktree) }))
    .filter(({ id }) => id !== null)
  if (agents.length === 0) return
  const ids = new Set(agents.map(({ id }) => id ?? ''))
  const listCwds = typeof cwds === 'function' ? cwds : cwds === undefined ? processCwds : () => cwds
  const bases = outputBases ?? bazelOutputUserRoots().flatMap((root) => readOutputBases(root))
  /** Inspect one agent worktree against a fresh process listing and fresh subagent records. */
  const inspect = (worktree, id, ctx, metas) => {
    const base = worktreeFacts(worktree, ctx)
    const meta = metas.get(id ?? '') ?? null
    const inside = (path) => isWithin(path, worktree.path) || isWithin(realpathOr(path), base.path)
    return {
      ...base,
      landed: looseCommitsLanded(base.path, ['--remotes']),
      ignoredWork: ignoredWork(base.path),
      // Fresh each look, prunable records included: a nested tree goes with a forced removal.
      holdsWorktree: listWorktrees(commonDir).some(
        (other) => other.path !== worktree.path && inside(other.path),
      ),
      meta,
      metaNamesIt: meta !== null && samePath(meta.worktreePath, base.path),
      doomedBranch: branchToDelete(base.path, base.branch, meta, id),
    }
  }
  const metas = findAgentMeta({ projectsRoots, ids })
  const ctx = { cwds: listCwds(), sessions: new Set(), outputBases: bases }
  let removed = 0
  let kept = 0
  for (const { worktree, id } of agents) {
    /** @type {AgentFacts} */
    let facts
    try {
      facts = inspect(worktree, id, ctx, metas)
    } catch (error) {
      kept++
      write(`agent worktrees: kept ${worktree.path}: could not inspect it (${firstLine(error)})`)
      continue
    }
    let holds = agentWorktreeHolds(facts, { now, graceMs })
    if (holds.length === 0 && !dryRun) {
      // The first look is a snapshot taken before the loop; re-inspect this tree immediately before
      // the forced removal so a write, a new process or parent activity since then keeps it.
      try {
        const fresh = { cwds: listCwds(), sessions: new Set(), outputBases: bases }
        facts = inspect(
          worktree,
          id,
          fresh,
          findAgentMeta({ projectsRoots, ids: new Set([id ?? '']) }),
        )
        holds = agentWorktreeHolds(facts, { now, graceMs })
      } catch (error) {
        holds = [`could not re-inspect it (${firstLine(error)})`]
      }
    }
    if (holds.length > 0) {
      kept++
      write(`agent worktrees: kept ${facts.path}: ${holds.join(', ')}`)
      continue
    }
    const baseCount = `${facts.bases.length} bazel base${facts.bases.length === 1 ? '' : 's'}`
    const branch = facts.doomedBranch ? `branch ${facts.doomedBranch}` : 'no branch'
    const label = `${facts.path} (${facts.branch ?? 'detached'}; with ${branch}, ${baseCount})`
    if (dryRun) {
      removed++
      write(`agent worktrees: would remove ${label}`)
      continue
    }
    try {
      git(['--git-dir', commonDir, 'worktree', 'remove', '--force', facts.path])
      if (fs.existsSync(facts.path)) removeTree(facts.path)
      for (const base of facts.bases) removeTree(base.path)
    } catch (error) {
      kept++
      write(`agent worktrees: could not remove ${facts.path}: ${firstLine(error)}`)
      continue
    }
    removed++
    try {
      // Only after the tree is gone; the landed checks proved every commit on it safe elsewhere.
      if (facts.doomedBranch) git(['--git-dir', commonDir, 'branch', '-D', facts.doomedBranch])
      write(`agent worktrees: removed ${label}`)
    } catch (error) {
      write(`agent worktrees: removed ${label}, but kept the branch: ${firstLine(error)}`)
    }
  }
  try {
    if (!dryRun) git(['--git-dir', commonDir, 'worktree', 'prune'])
  } catch {
    // Pruning only drops records of worktrees already gone; a failure leaves them for next time.
  }
  write(`agent worktrees: ${dryRun ? 'would remove' : 'removed'} ${removed}; kept ${kept}`)
}

function samePath(path, realPath) {
  try {
    return fs.realpathSync(path) === realPath
  } catch {
    return path === realPath
  }
}

function firstLine(error) {
  return String(error?.message ?? error).split('\n')[0]
}

/** The stamp and claim files that rate-limit and serialise passes over one git common dir. */
export function passFiles(commonDir, stateDir) {
  const key = createHash('sha256').update(commonDir).digest('hex').slice(0, 12)
  return {
    stamp: join(stateDir, `agent-worktree-reaper-${key}.last`),
    claim: join(stateDir, `agent-worktree-reaper-${key}.claim`),
  }
}

/** Create the claim exclusively; a claim older than STALE_CLAIM_MS is from a dead pass. */
export function takeClaim(claim, now = Date.now()) {
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      fs.writeFileSync(claim, `${process.pid}\n`, { flag: 'wx' })
      return true
    } catch (error) {
      if (error.code !== 'EEXIST' || now - mtimeMs(claim) < STALE_CLAIM_MS) return false
      fs.rmSync(claim, { force: true })
    }
  }
  return false
}

function trimLog(log) {
  let size
  try {
    size = fs.statSync(log).size
  } catch {
    return
  }
  if (size <= MAX_LOG_BYTES) return
  const text = fs.readFileSync(log, 'utf8')
  // Keep the newest half, from its first whole line.
  const tail = text.slice(text.length - Math.floor(MAX_LOG_BYTES / 2))
  fs.writeFileSync(log, tail.replace(/^[^\n]*\n/, ''))
}

/**
 * The hook form: unless a pass started within RATE_LIMIT_MS or one still holds the claim, claim
 * it and start the pass as a detached child logging to `log`. Returns whether it spawned.
 * @param {{
 *   commonDir: string,
 *   stateDir: string,
 *   log: string,
 *   script: string,
 *   cwd: string,
 *   now?: number,
 *   spawn?: typeof spawnProcess,
 * }} options
 */
export function detachPass({
  commonDir,
  stateDir,
  log,
  script,
  cwd,
  now = Date.now(),
  spawn = spawnProcess,
}) {
  const { stamp, claim } = passFiles(commonDir, stateDir)
  if (now - mtimeMs(stamp) < RATE_LIMIT_MS) return false
  fs.mkdirSync(stateDir, { recursive: true })
  if (!takeClaim(claim, now)) return false
  fs.writeFileSync(stamp, '')
  const when = new Date(now)
  fs.utimesSync(stamp, when, when)
  trimLog(log)
  const fd = fs.openSync(log, 'a')
  try {
    const child = spawn(process.execPath, [script], {
      cwd,
      detached: true,
      stdio: ['ignore', fd, fd],
      env: { ...process.env, [CLAIM_ENV]: claim },
    })
    child.unref()
  } catch (error) {
    fs.rmSync(claim, { force: true })
    throw error
  } finally {
    fs.closeSync(fd)
  }
  return true
}

function main() {
  const { values } = parseArgs({
    options: { detach: { type: 'boolean' }, 'dry-run': { type: 'boolean' } },
    strict: true,
  })
  const env = process.env
  const home = homedir()
  const script = fileURLToPath(import.meta.url)
  const repoRoot = resolve(dirname(script), '..', '..')
  const stateDir = join(home, '.cache', 'bossanova')
  if (values.detach) {
    // A hook: SessionStart stdout reaches the model and a SubagentStop exit 2 blocks the
    // subagent, so this prints nothing and always exits 0.
    try {
      const commonDir = git(['rev-parse', '--path-format=absolute', '--git-common-dir'], repoRoot)
      detachPass({
        commonDir: commonDir.trim(),
        stateDir,
        log: join(stateDir, 'agent-worktree-reaper.log'),
        script,
        cwd: repoRoot,
      })
    } catch {
      // The next hook or disk-guard wake tries again.
    }
    return
  }
  // A blank or invalid value falls back to the default, never to 0, which would drop both
  // activity holds.
  const graceMinutes = graceMinutesFrom(env)
  const write = (line) => process.stdout.write(`${new Date().toISOString()} ${line}\n`)
  const dryRun = values['dry-run'] === true
  const commonDir = git(
    ['rev-parse', '--path-format=absolute', '--git-common-dir'],
    repoRoot,
  ).trim()
  const { claim } = passFiles(commonDir, stateDir)
  const owned = env[CLAIM_ENV] === claim
  if (!dryRun && !owned) {
    fs.mkdirSync(stateDir, { recursive: true })
    if (!takeClaim(claim)) {
      write('agent worktrees: another pass is running; skipping')
      return
    }
  }
  try {
    reapAgentWorktrees({
      repoRoot,
      projectsRoots: defaultProjectsRoots({ env, home }),
      outputBases: bazelOutputUserRoots({ env, home }).flatMap((root) => readOutputBases(root)),
      removeTree,
      graceMs: graceMinutes * MINUTE,
      dryRun,
      write,
    })
  } finally {
    if (!dryRun) fs.rmSync(claim, { force: true })
  }
}

if (isMainModule(import.meta.url)) main()
