#!/usr/bin/env node
import { spawnSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import {
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  utimesSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join, relative, isAbsolute } from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'

const isSha = (sha) => typeof sha === 'string' && /^[0-9a-f]{40}$/i.test(sha)
const gitReader = (cwd) => (bin, args) => {
  const r = spawnSync(bin, args, { cwd, encoding: 'utf8', timeout: 60000 })
  return {
    ok: !r.error && r.status === 0,
    output: r.stdout ?? '',
    error: String(r.error?.message ?? r.stderr ?? ''),
  }
}
const ensureCommit = (git, sha) => {
  const check = () => git('git', ['cat-file', '-e', `${sha}^{commit}`]).ok
  if (check()) return true
  git('git', ['fetch', '--no-tags', '--quiet', 'origin', sha])
  return check()
}
function extractTree(git, baseSha, staging) {
  const archive = join(staging, 'tree.tar')
  const root = join(staging, 'tree')
  mkdirSync(root)
  const archived = git('git', ['archive', '--format=tar', '-o', archive, baseSha])
  if (!archived.ok) return { ok: false, why: `archive-failed:${archived.error}` }
  const extracted = git('tar', ['-xf', archive, '-C', root])
  return extracted.ok ? { ok: true, root } : { ok: false, why: `extract-failed:${extracted.error}` }
}

// The trusted tree: a read-only snapshot of the repository at the base revision the judge already
// trusts for policy. Verify extensions — their SKILL.md, resources and `applies` checks — are read
// from it, never from the PR worktree, so a head cannot change how its own verdict is decided. It
// is a plain extraction, not a `git worktree add`, so it registers nothing in the worktree list.

export const TRUSTED_TREE_PREFIX = 'boss-verify-base-'
// Trees for other base revisions older than this are reaped; a reused tree is touched on every use.
export const TRUSTED_TREE_STALE_MS = 2 * 60 * 60 * 1000

// Trees live in a private per-user parent (0700), so another local user on a shared temp dir cannot
// plant one at the predictable path. Same-user tampering is out of scope: code running as this user
// can rewrite any snapshot it can read.
export function trustedTreeParent(tmpRoot = tmpdir()) {
  const uid = typeof process.getuid === 'function' ? process.getuid() : 'user'
  return join(tmpRoot, `boss-verify-trusted-${uid}`)
}

export function trustedTreePath(baseSha, tmpRoot = tmpdir()) {
  return join(trustedTreeParent(tmpRoot), `${TRUSTED_TREE_PREFIX}${String(baseSha).toLowerCase()}`)
}

// Creates the private parent if absent, then accepts it only as a real directory (not a symlink)
// owned by this user and not group/world-writable.
export function ensureTrustedParent(tmpRoot = tmpdir()) {
  const parent = trustedTreeParent(tmpRoot)
  try {
    mkdirSync(tmpRoot, { recursive: true })
    mkdirSync(parent, { mode: 0o700 })
  } catch (error) {
    if (error?.code !== 'EEXIST') return false
  }
  try {
    const st = lstatSync(parent)
    if (st.isSymbolicLink() || !st.isDirectory()) return false
    if (typeof process.getuid === 'function' && st.uid !== process.getuid()) return false
    return (st.mode & 0o022) === 0
  } catch {
    return false
  }
}

export const isDirectory = (path) => {
  try {
    return statSync(path).isDirectory()
  } catch {
    return false
  }
}

// Removes every trusted tree and abandoned staging directory older than `staleMs`, except `keep`.
// Best-effort: a reap that fails leaves a directory for the next call, never fails this one.
/** @param {{tmpRoot?: string, keep?: string|string[], now?: number, staleMs?: number}} options */
export function reapTrustedTrees({ tmpRoot = tmpdir(), keep, now = Date.now(), staleMs } = {}) {
  const reaped = []
  const parent = trustedTreeParent(tmpRoot)
  let names = []
  try {
    names = readdirSync(parent)
  } catch {
    return reaped
  }
  for (const name of names) {
    if (!name.startsWith(TRUSTED_TREE_PREFIX)) continue
    const path = join(parent, name)
    if ((Array.isArray(keep) ? keep : [keep]).includes(path)) continue
    try {
      if (now - statSync(path).mtimeMs < (staleMs ?? TRUSTED_TREE_STALE_MS)) continue
      // Rename out of the prefixed namespace first, so a concurrent judge never reuses a
      // half-deleted tree: it either sees the whole tree or no tree.
      const doomed = join(parent, `.reaping-${randomBytes(6).toString('hex')}`)
      renameSync(path, doomed)
      rmSync(doomed, { recursive: true, force: true })
      reaped.push(path)
    } catch {
      /* Left for the next reap. */
    }
  }
  return reaped
}

/**
 * Produces the trusted tree for `baseSha` from the checkout at `cwd`: `{ok: true, root, reused}` or
 * `{ok: false, why}`. The path is content-addressed by `baseSha`, so concurrent judges of PRs on one
 * base share a tree; extraction happens in a sibling staging directory that is renamed into place,
 * so a partial tree is never observable, and an existing tree is reused, never written. A commit
 * that is not local is fetched first. A tree that cannot be produced is unknown, never the
 * working tree.
 */
/** @param {{baseSha?: string, cwd?: string, read?: Function, tmpRoot?: string, now?: number, staleMs?: number}} options */
export function materializeTrustedTree({
  baseSha,
  cwd = process.cwd(),
  read,
  tmpRoot = tmpdir(),
  now = Date.now(),
  staleMs,
} = {}) {
  if (!isSha(baseSha)) return { ok: false, why: 'no-base-sha' }
  const git = read ?? gitReader(cwd)
  if (!ensureTrustedParent(tmpRoot)) return { ok: false, why: 'untrusted-parent' }
  const root = trustedTreePath(baseSha, tmpRoot)
  const done = (result) => {
    reapTrustedTrees({ tmpRoot, keep: [root, `${root}-deps`], now, staleMs })
    return result
  }
  const touch = () => {
    try {
      const at = new Date(now)
      utimesSync(root, at, at)
    } catch {
      /* Touching only defers the reap. */
    }
  }
  if (isDirectory(root)) {
    touch()
    // A reaper may have renamed it away between the check and the touch: extract afresh then.
    if (isDirectory(root)) return done({ ok: true, root, reused: true })
  }
  if (!ensureCommit(git, baseSha)) return done({ ok: false, why: 'commit-unavailable' })
  let staging
  try {
    staging = mkdtempSync(`${root}.partial-`)
    const extracted = extractTree(git, baseSha, staging)
    if (!extracted.ok) return done(extracted)
    const tree = extracted.root
    try {
      renameSync(tree, root)
    } catch (error) {
      // A concurrent judge renamed its own complete tree into place first: reuse that one.
      if (!isDirectory(root)) return done({ ok: false, why: `rename-failed:${error?.code}` })
      touch()
      return done({ ok: true, root, reused: true })
    }
    return done({ ok: true, root, reused: false })
  } catch (error) {
    return done({ ok: false, why: `materialize-failed:${error?.code ?? 'error'}` })
  } finally {
    if (staging) rmSync(staging, { recursive: true, force: true })
  }
}

export const TRUSTED_DEPS_INSTALL_TIMEOUT_MS = 5 * 60 * 1000
export const trustedDepsPath = (baseSha, tmpRoot = tmpdir()) =>
  `${trustedTreePath(baseSha, tmpRoot)}-deps`

export function depsPlan(root) {
  const exists = (file) => {
    try {
      return statSync(join(root, file)).isFile()
    } catch {
      return false
    }
  }
  if (exists('pnpm-lock.yaml'))
    return {
      needed: true,
      manager: 'pnpm',
      argv: [
        'install',
        '--frozen-lockfile',
        '--ignore-scripts',
        '--prefer-offline',
        '--config.strict-dep-builds=false',
      ],
    }
  if (exists('package-lock.json') || exists('npm-shrinkwrap.json'))
    return {
      needed: true,
      manager: 'npm',
      argv: ['ci', '--ignore-scripts', '--prefer-offline', '--no-audit', '--no-fund'],
    }
  if (['yarn.lock', 'bun.lock', 'bun.lockb'].some(exists))
    return { needed: true, manager: null, why: 'unsupported-lockfile' }
  return { needed: false }
}

/** @param {{root?: string, extensions?: {dir?: string, builtin?: boolean}[]}} options */
export function needsTrustedDeps({ root, extensions } = {}) {
  return (
    typeof root === 'string' &&
    root.length > 0 &&
    Array.isArray(extensions) &&
    extensions.some((ext) => {
      if (ext?.builtin === true || typeof ext?.dir !== 'string') return false
      const rel = relative(root, ext.dir)
      return rel !== '..' && !rel.startsWith('../') && !rel.startsWith('..\\') && !isAbsolute(rel)
    }) &&
    depsPlan(root).needed
  )
}

export function trustedDepsMissing(options) {
  return needsTrustedDeps(options) && !isDirectory(`${options.root}-deps`)
}

/** @param {{baseSha?: string, cwd?: string, read?: Function, tmpRoot?: string, now?: number, staleMs?: number, install?: Function, timeoutMs?: number}} options */
export function ensureTrustedDeps({
  baseSha,
  cwd = process.cwd(),
  read,
  tmpRoot = tmpdir(),
  now = Date.now(),
  staleMs,
  install = spawnSync,
  timeoutMs = TRUSTED_DEPS_INSTALL_TIMEOUT_MS,
} = {}) {
  if (!isSha(baseSha)) return { ok: false, why: 'no-base-sha' }
  if (!ensureTrustedParent(tmpRoot)) return { ok: false, why: 'untrusted-parent' }
  const plain = trustedTreePath(baseSha, tmpRoot)
  const root = trustedDepsPath(baseSha, tmpRoot)
  const done = (result) => {
    reapTrustedTrees({ tmpRoot, keep: [plain, root], now, staleMs })
    return result
  }
  const reuse = () => {
    try {
      const at = new Date(now)
      utimesSync(root, at, at)
    } catch {}
    return done({ ok: true, root, reused: true })
  }
  if (isDirectory(root)) return reuse()
  const git = read ?? gitReader(cwd)
  if (!ensureCommit(git, baseSha)) return done({ ok: false, why: 'commit-unavailable' })
  let staging
  try {
    staging = mkdtempSync(`${root}.partial-`)
    const tree = extractTree(git, baseSha, staging)
    if (!tree.ok) return done(tree)
    const plan = depsPlan(tree.root)
    if (!plan.needed) return done({ ok: false, why: 'no-lockfile' })
    if (!plan.manager) return done({ ok: false, why: plan.why })
    const r = install(plan.manager, plan.argv, {
      cwd: tree.root,
      env: { ...process.env, CI: 'true', COREPACK_ENABLE_DOWNLOAD_PROMPT: '0' },
      encoding: 'utf8',
      timeout: timeoutMs,
      killSignal: 'SIGKILL',
    })
    if (r.error?.code === 'ETIMEDOUT') return done({ ok: false, why: 'deps-timeout' })
    if (r.error || r.status !== 0)
      return done({
        ok: false,
        why: `deps-install-failed:${r.error?.code ?? r.status ?? r.signal ?? 'error'}`,
      })
    try {
      renameSync(tree.root, root)
    } catch (error) {
      if (isDirectory(root)) return reuse()
      return done({ ok: false, why: `rename-failed:${error?.code}` })
    }
    return done({ ok: true, root, reused: false })
  } catch (error) {
    return done({ ok: false, why: `deps-install-failed:${error?.code ?? 'error'}` })
  } finally {
    if (staging) rmSync(staging, { recursive: true, force: true, maxRetries: 3 })
  }
}

export function runCli(argv, deps = {}) {
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: { judge: { type: 'string' } },
    })
    const [cmd] = positionals
    if (cmd === 'deps' && positionals.length === 1 && values.judge) {
      const { facts } = JSON.parse(readFileSync(values.judge, 'utf8'))
      const root = facts?.trustedRoot
      if (typeof root !== 'string' || !isSha(facts?.base?.sha))
        throw new Error('judge has no trusted root or base SHA')
      const out = (deps, why) => ({ code: 0, out: { root, deps, ...(why ? { why } : {}) } })
      if (root !== trustedTreePath(facts.base.sha, deps.tmpRoot))
        return out('missing', 'root-mismatch')
      if (!needsTrustedDeps({ root, extensions: facts.extensionPaths }))
        return out('not-needed', null)
      const r = ensureTrustedDeps({ ...deps, baseSha: facts.base.sha })
      return r.ok
        ? { code: 0, out: { root: r.root, deps: r.reused ? 'reused' : 'installed' } }
        : out('missing', r.why)
    }
    throw new Error('usage: verify-trusted-tree.mjs deps --judge <file>')
  } catch (error) {
    return { code: 1, error: error.message }
  }
}
if (isMainModule(import.meta.url)) {
  const r = runCli(process.argv.slice(2))
  if (r.out) process.stdout.write(`${JSON.stringify(r.out)}\n`)
  else process.stderr.write(`${r.error}\n`)
  process.exitCode = r.code
}
