// Converge a session worktree onto its PR's pushed head without discarding unpushed work.
//
// A repair can rebase and force-push from a side checkout, leaving the session worktree on the
// pre-rebase commit: diverged from the PR head, so a plain fast-forward refuses and every later
// stage that needs "worktree HEAD == PR head" waits forever. `sync` moves the worktree to the
// target when that loses nothing: every commit only the local HEAD has is either patch-equivalent
// to one in the target, or was on the remote before (reachable from a past value of the
// remote-tracking ref's reflog, or from a caller-supplied `--previous-head`). Incidental dirty
// files (default `go.work.sum`) are carried across byte-for-byte unless the target changes them;
// any other tracked modification blocks. A worktree already on a newer pushed head is never moved
// back. Every unsynced outcome is counted per (head, target) pair in the worktree's git dir so a
// caller can escalate instead of waiting silently, and `--remove-worktree` removes a detached side
// checkout whose HEAD the target already contains. Concurrent runs are serialised by a lock file.
//
// Prints one JSON line. Exit: 0 synced, 1 not synced, 2 unknown or usage error.

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs as parseArgv } from 'node:util'

import { isMainModule } from './main-module.mjs'
import { parsePorcelainZ } from './worktree-state.mjs'

export const VERDICTS = Object.freeze({
  IN_SYNC: 'in-sync',
  FAST_FORWARDED: 'fast-forwarded',
  RESET: 'reset',
  AHEAD: 'ahead',
  UNSAFE: 'unsafe',
  DIRTY: 'dirty',
  UNKNOWN: 'unknown',
})

const SYNCED = new Set([VERDICTS.IN_SYNC, VERDICTS.FAST_FORWARDED, VERDICTS.RESET])

export const EXIT_CODES = Object.freeze({
  [VERDICTS.IN_SYNC]: 0,
  [VERDICTS.FAST_FORWARDED]: 0,
  [VERDICTS.RESET]: 0,
  [VERDICTS.AHEAD]: 1,
  [VERDICTS.UNSAFE]: 1,
  [VERDICTS.DIRTY]: 1,
  [VERDICTS.UNKNOWN]: 2,
})

// The verify park code an escalated outcome posts: the worktree holds commits or changes the
// target lacks, or the helper could not establish the state at all.
export const PARK_REASONS = Object.freeze({
  [VERDICTS.AHEAD]: 'worktree-diverged',
  [VERDICTS.UNSAFE]: 'worktree-diverged',
  [VERDICTS.DIRTY]: 'worktree-diverged',
  [VERDICTS.UNKNOWN]: 'worktree-unsynced',
})

export const DEFAULT_INCIDENTAL = Object.freeze(['go.work.sum'])
export const DEFAULT_ESCALATE_AFTER = 6
export const STATE_FILE = 'boss-worktree-sync.json'
export const LOCK_FILE = 'boss-worktree-sync.lock'
export const BACKUP_DIR = 'boss-worktree-sync-backup'

const SHA = /^[0-9a-f]{40}$/

function git(repo, args) {
  const res = spawnSync('git', ['-C', repo, ...args], {
    encoding: 'utf8',
    env: { ...process.env, GIT_OPTIONAL_LOCKS: '0', GIT_TERMINAL_PROMPT: '0' },
    maxBuffer: 64 * 1024 * 1024,
  })
  return {
    ok: res.status === 0 && !res.error,
    status: res.status,
    stdout: res.stdout || '',
    stderr: (res.stderr || '').trim() || (res.error ? String(res.error.message) : ''),
  }
}

const lines = (s) => s.split('\n').filter(Boolean)

function revParse(repo, ref) {
  const r = git(repo, ['rev-parse', '--verify', '--quiet', `${ref}^{commit}`])
  const sha = r.stdout.trim().toLowerCase()
  return r.ok && SHA.test(sha) ? sha : ''
}

function isAncestor(repo, a, b) {
  return git(repo, ['merge-base', '--is-ancestor', a, b]).status === 0
}

function readState(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'))
  } catch {
    return null
  }
}

/**
 * Commits reachable from `head` that the remote never held: not reachable from `target` or any
 * known-pushed commit, and with no patch-equivalent commit in `target`.
 */
export function unpushedCommits(repo, { head, target, pushed }) {
  const known = pushed.filter((sha) => revParse(repo, sha))
  const only = git(repo, ['rev-list', head, '--not', target, ...known])
  if (!only.ok) return { ok: false, reason: `rev-list: ${only.stderr}` }
  const candidates = lines(only.stdout)
  if (candidates.length === 0) return { ok: true, commits: [] }
  const cherry = git(repo, ['cherry', target, head])
  if (!cherry.ok) return { ok: false, reason: `cherry: ${cherry.stderr}` }
  const equivalent = new Set(
    lines(cherry.stdout)
      .filter((l) => l.startsWith('- '))
      .map((l) => l.slice(2).trim()),
  )
  return { ok: true, commits: candidates.filter((sha) => !equivalent.has(sha)) }
}

function remoteReflog(repo, ref) {
  const r = git(repo, ['log', '-g', '--format=%H', ref, '--'])
  return r.ok ? lines(r.stdout).filter((s) => SHA.test(s)) : []
}

function dirtyEntries(repo) {
  const r = git(repo, ['status', '--porcelain=v1', '-z', '--untracked-files=all'])
  if (!r.ok) return { ok: false, reason: `status: ${r.stderr}` }
  if (r.stdout === '') return { ok: true, tracked: [], untracked: [] }
  const parsed = parsePorcelainZ(r.stdout)
  if (!parsed.ok) return { ok: false, reason: parsed.reason }
  const records = r.stdout.slice(0, -1).split('\0')
  const tracked = []
  const untracked = []
  let i = 0
  for (const entry of parsed.entries) {
    const rec = records[i]
    ;(rec.startsWith('??') ? untracked : tracked).push(entry)
    i += /^[RC]|^.[RC]/.test(rec) ? 2 : 1
  }
  return { ok: true, tracked, untracked }
}

function saveFiles(repo, paths) {
  return paths.map((p) => {
    const abs = path.join(repo, p)
    if (!fs.existsSync(abs)) return { path: p, exists: false }
    return { path: p, exists: true, bytes: fs.readFileSync(abs), mode: fs.statSync(abs).mode }
  })
}

function writeFiles(root, saved) {
  for (const f of saved) {
    const abs = path.join(root, f.path)
    if (!f.exists) {
      fs.rmSync(abs, { force: true })
      continue
    }
    fs.mkdirSync(path.dirname(abs), { recursive: true })
    fs.writeFileSync(abs, f.bytes)
    fs.chmodSync(abs, f.mode)
  }
}

function blobAt(repo, rev, file) {
  const r = git(repo, ['rev-parse', '--verify', '--quiet', `${rev}:${file}`])
  return r.ok ? r.stdout.trim() : ''
}

// An incidental file the target itself changes takes the target's version: carrying the local
// bytes over would silently revert the PR's change. Those bytes are copied to `backupDir` first.
function moveTo(repo, { head, target, incidental, backupDir }) {
  const saved = saveFiles(repo, incidental)
  const carried = saved.filter((f) => blobAt(repo, head, f.path) === blobAt(repo, target, f.path))
  const superseded = saved.filter((f) => !carried.includes(f))
  writeFiles(
    backupDir,
    superseded.filter((f) => f.exists),
  )
  for (const f of incidental) {
    if (blobAt(repo, head, f)) git(repo, ['checkout', head, '--', f])
    else fs.rmSync(path.join(repo, f), { force: true })
  }
  const reset = git(repo, ['reset', '--keep', target])
  writeFiles(repo, reset.ok ? carried : saved)
  return {
    reset,
    preserved: carried.map((f) => f.path),
    superseded: superseded.map((f) => ({
      path: f.path,
      backup: f.exists ? path.join(backupDir, f.path) : '',
    })),
  }
}

function listWorktrees(repo) {
  const list = git(repo, ['worktree', 'list', '--porcelain'])
  const entries = new Map()
  let main = ''
  let current = null
  for (const line of list.ok ? list.stdout.split('\n') : []) {
    if (line.startsWith('worktree ')) {
      const dir = line.slice(9)
      current = { head: '', branch: '' }
      const real = fs.existsSync(dir) ? fs.realpathSync(dir) : dir
      if (!main) main = real
      entries.set(real, current)
    } else if (current && line.startsWith('HEAD ')) current.head = line.slice(5).trim()
    else if (current && line.startsWith('branch ')) current.branch = line.slice(7).trim()
  }
  return { main, entries }
}

// Only a detached side checkout is removable: never this worktree, the main checkout, or any
// worktree with a branch checked out (a session worktree; repair's side checkouts are --detach).
function removeWorktrees(repo, { paths, target, incidental, self }) {
  const removed = []
  const kept = []
  if (paths.length === 0) return { removed, kept }
  const { main, entries } = listWorktrees(repo)
  for (const raw of paths) {
    const abs = fs.existsSync(raw) ? fs.realpathSync(raw) : path.resolve(raw)
    const keep = (reason) => kept.push({ path: raw, reason })
    const entry = entries.get(abs)
    if (!target) keep('no-target')
    else if (abs === self) keep('current-worktree')
    else if (abs === main) keep('main-checkout')
    else if (!entry) keep('not-a-worktree')
    else if (entry.branch) keep(`branch-checked-out: ${entry.branch}`)
    else if (!SHA.test(entry.head) || !isAncestor(repo, entry.head, target))
      keep('head-not-in-target')
    else {
      const dirt = dirtyEntries(abs)
      const extra = dirt.ok
        ? [...dirt.tracked, ...dirt.untracked].filter((p) => !incidental.includes(p))
        : []
      if (!dirt.ok) keep(`unreadable: ${dirt.reason}`)
      else if (extra.length > 0) keep(`dirty: ${extra.join(', ')}`)
      else {
        const rm = git(repo, ['worktree', 'remove', '--force', abs])
        if (rm.ok) removed.push(raw)
        else keep(`remove failed: ${rm.stderr}`)
      }
    }
  }
  return { removed, kept }
}

/**
 * `firstSeenAt` is set only on an outcome that is not synced (see syncLocked's `finish`).
 * @returns {{synced: boolean, head: string, previousHead: string, target: string, unpushed: any[],
 *   preserved: any[], superseded: any[], blocking: any[], removedWorktrees: any[],
 *   keptWorktrees: any[], attempts: number, escalate: boolean, parkReason: string,
 *   reason: string, firstSeenAt?: string}}
 */
function emptyResult() {
  return {
    synced: false,
    head: '',
    previousHead: '',
    target: '',
    unpushed: [],
    preserved: [],
    superseded: [],
    blocking: [],
    removedWorktrees: [],
    keptWorktrees: [],
    attempts: 0,
    escalate: false,
    parkReason: '',
    reason: '',
  }
}

// A crashed run leaves its lock behind; past this age the lock is taken over.
export const LOCK_STALE_MS = 15 * 60 * 1000

function acquireLock(file) {
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      fs.writeFileSync(file, `${process.pid}\n`, { flag: 'wx' })
      return { ok: true }
    } catch (err) {
      if (err.code !== 'EEXIST') return { ok: false, reason: `lock ${file}: ${err.message}` }
      let age = 0
      try {
        age = Date.now() - fs.statSync(file).mtimeMs
      } catch {
        continue
      }
      if (age < LOCK_STALE_MS) return { ok: false, reason: `another sync holds ${file}` }
      fs.rmSync(file, { force: true })
    }
  }
  return { ok: false, reason: `another sync holds ${file}` }
}

/**
 * @param {{repo?: string, remote?: string, branch?: string, head?: string, previousHeads?: string[],
 *   incidental?: string[], removeWorktrees?: string[], escalateAfter?: number, now?: () => number}} opts
 */
export function sync(opts = {}) {
  const top = git(opts.repo || process.cwd(), ['rev-parse', '--show-toplevel'])
  if (!top.ok)
    return {
      verdict: VERDICTS.UNKNOWN,
      ...emptyResult(),
      reason: `not a git worktree: ${top.stderr}`,
    }
  const repo = fs.realpathSync(top.stdout.trim())
  const gitPath = (name) =>
    path.resolve(repo, git(repo, ['rev-parse', '--git-path', name]).stdout.trim())
  const lockFile = gitPath(LOCK_FILE)
  const lock = acquireLock(lockFile)
  if (!lock.ok) return { verdict: VERDICTS.UNKNOWN, ...emptyResult(), reason: lock.reason }
  try {
    return syncLocked(repo, opts, {
      stateFile: gitPath(STATE_FILE),
      backupDir: gitPath(BACKUP_DIR),
    })
  } finally {
    fs.rmSync(lockFile, { force: true })
  }
}

function syncLocked(repo, opts, { stateFile, backupDir }) {
  const remote = opts.remote || 'origin'
  const incidental = opts.incidental?.length ? opts.incidental : [...DEFAULT_INCIDENTAL]
  const escalateAfter = opts.escalateAfter ?? DEFAULT_ESCALATE_AFTER
  const now = opts.now ?? Date.now
  const branch =
    opts.branch || git(repo, ['symbolic-ref', '--short', '-q', 'HEAD']).stdout.trim() || ''
  const trackingRef = branch ? `refs/remotes/${remote}/${branch}` : ''
  const head = revParse(repo, 'HEAD')
  const base = emptyResult()
  base.head = head
  base.previousHead = head
  let target = ''

  // Every outcome that is not synced is counted per (head, target), whatever its verdict, so no
  // refusal or failure can make a caller wait forever without escalating.
  const finish = (verdict, extra = {}) => {
    const out = { verdict, ...base, ...extra }
    out.synced = SYNCED.has(verdict)
    const cleanup = removeWorktrees(repo, {
      paths: opts.removeWorktrees ?? [],
      target,
      incidental,
      self: repo,
    })
    out.removedWorktrees = cleanup.removed
    out.keptWorktrees = cleanup.kept
    if (out.synced) {
      fs.rmSync(stateFile, { force: true })
      return out
    }
    const key = target || opts.head?.toLowerCase() || trackingRef
    const prior = readState(stateFile)
    const same = prior && prior.head === head && prior.target === key
    out.attempts = same && Number.isInteger(prior.attempts) ? prior.attempts + 1 : 1
    out.firstSeenAt = same && prior.firstSeenAt ? prior.firstSeenAt : new Date(now()).toISOString()
    out.escalate = out.attempts >= escalateAfter
    out.parkReason = PARK_REASONS[verdict]
    try {
      fs.writeFileSync(
        stateFile,
        `${JSON.stringify({ head, target: key, verdict, attempts: out.attempts, firstSeenAt: out.firstSeenAt })}\n`,
      )
    } catch (err) {
      out.reason = `${out.reason}; state not recorded (${err.message})`.replace(/^; /, '')
    }
    return out
  }

  let fetchError = ''
  if (branch) {
    const fetched = git(repo, ['fetch', '--quiet', remote, `+refs/heads/${branch}:${trackingRef}`])
    if (!fetched.ok) fetchError = `fetch ${remote} ${branch}: ${fetched.stderr}`
  }
  if (fetchError && !opts.head) return finish(VERDICTS.UNKNOWN, { reason: fetchError })
  target = opts.head
    ? revParse(repo, opts.head.toLowerCase())
    : trackingRef
      ? revParse(repo, trackingRef)
      : ''
  base.target = target
  if (!target)
    return finish(VERDICTS.UNKNOWN, {
      reason: opts.head
        ? `target ${opts.head} is not present after fetch${fetchError ? ` (${fetchError})` : ''}`
        : 'no branch to sync against',
    })
  if (!head) return finish(VERDICTS.UNKNOWN, { reason: 'HEAD does not resolve' })

  if (head === target) return finish(VERDICTS.IN_SYNC)

  const dirt = dirtyEntries(repo)
  if (!dirt.ok) return finish(VERDICTS.UNKNOWN, { reason: dirt.reason })
  const blocking = dirt.tracked.filter((p) => !incidental.includes(p))
  if (blocking.length > 0)
    return finish(VERDICTS.DIRTY, {
      blocking,
      reason: 'tracked changes outside the incidental set',
    })

  const fastForward = isAncestor(repo, head, target)
  if (!fastForward) {
    const remoteTip = trackingRef ? revParse(repo, trackingRef) : ''
    const pushed = [
      ...new Set([
        ...(trackingRef ? remoteReflog(repo, trackingRef) : []),
        ...(opts.previousHeads ?? []),
      ]),
    ]
    const unpushed = unpushedCommits(repo, { head, target, pushed })
    if (!unpushed.ok) return finish(VERDICTS.UNKNOWN, { reason: unpushed.reason })
    if (unpushed.commits.length > 0)
      return finish(VERDICTS.UNSAFE, {
        unpushed: unpushed.commits,
        reason: 'local HEAD has commits the remote never held',
      })
    if (
      isAncestor(repo, target, head) ||
      (remoteTip && remoteTip !== target && isAncestor(repo, head, remoteTip))
    )
      return finish(VERDICTS.AHEAD, {
        reason: 'the worktree is on a newer pushed head than the target; it is never moved back',
      })
  }

  const incidentalDirt = [...dirt.tracked, ...dirt.untracked].filter((p) => incidental.includes(p))
  const moved = moveTo(repo, { head, target, incidental: incidentalDirt, backupDir })
  if (!moved.reset.ok)
    return finish(VERDICTS.DIRTY, { reason: `reset --keep refused: ${moved.reset.stderr}` })
  base.head = revParse(repo, 'HEAD')
  if (base.head !== target)
    return finish(VERDICTS.UNKNOWN, { reason: 'HEAD did not reach the target' })
  return finish(fastForward ? VERDICTS.FAST_FORWARDED : VERDICTS.RESET, {
    preserved: moved.preserved,
    superseded: moved.superseded,
  })
}

export const USAGE = `usage: worktree-sync.mjs sync [--repo <dir>] [--remote <name>] [--branch <name>] [--head <sha>]
         [--previous-head <sha>]... [--incidental <path>]... [--remove-worktree <dir>]...
         [--escalate-after <n>]
Moves the worktree to the PR head when no unpushed work would be lost. Prints one JSON line.
Exit: 0 synced (in-sync|fast-forwarded|reset), 1 not synced (ahead|unsafe|dirty), 2 unknown or
usage error. When escalate is true, post the park code named by parkReason.
`

export function runCli(argv, deps = {}) {
  const out = deps.out ?? ((s) => process.stdout.write(s))
  let parsed
  try {
    parsed = parseArgv({
      args: argv,
      allowPositionals: true,
      strict: true,
      options: {
        repo: { type: 'string' },
        remote: { type: 'string' },
        branch: { type: 'string' },
        head: { type: 'string' },
        'previous-head': { type: 'string', multiple: true },
        incidental: { type: 'string', multiple: true },
        'remove-worktree': { type: 'string', multiple: true },
        'escalate-after': { type: 'string' },
        help: { type: 'boolean', short: 'h' },
      },
    })
  } catch (err) {
    out(
      `${JSON.stringify({ verdict: VERDICTS.UNKNOWN, reason: `usage: ${err.message}` })}\n${USAGE}`,
    )
    return 2
  }
  const { values, positionals } = parsed
  if (values.help) {
    out(USAGE)
    return 0
  }
  const [command] = positionals
  const escalateAfter =
    values['escalate-after'] === undefined ? undefined : Number(values['escalate-after'])
  if (
    positionals.length !== 1 ||
    command !== 'sync' ||
    (escalateAfter !== undefined && !(Number.isInteger(escalateAfter) && escalateAfter > 0))
  ) {
    out(
      `${JSON.stringify({ verdict: VERDICTS.UNKNOWN, reason: 'usage: sync is the only verb; --escalate-after takes a positive integer' })}\n${USAGE}`,
    )
    return 2
  }
  const result = sync({
    repo: values.repo,
    remote: values.remote,
    branch: values.branch,
    head: values.head,
    previousHeads: values['previous-head'],
    incidental: values.incidental,
    removeWorktrees: values['remove-worktree'],
    escalateAfter,
    now: deps.now,
  })
  out(`${JSON.stringify(result)}\n`)
  return EXIT_CODES[result.verdict]
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
