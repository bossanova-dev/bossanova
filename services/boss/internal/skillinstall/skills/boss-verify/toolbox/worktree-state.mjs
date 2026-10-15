// Clean / dirty / unknown verdict for a worktree, read from git's own porcelain.
//
// A skill that decides "the worktree is clean" from `git status --porcelain` typed into an agent's
// shell trusts whatever that shell returns. A command-rewriting shell hook, alias or function can
// replace that output with a summary — a literal `ok`, an empty string, an exit 0 with no content —
// and every one of those reads as "clean" (or as one bogus dirty entry) to a caller that only asks
// "is the output empty?". This helper spawns git directly, with no shell, and VALIDATES THE SHAPE of
// what it reads, so anything that is not porcelain-v1 `-z` output becomes `unknown` instead of a
// verdict. Validation, not the absence of a shell, is the load-bearing part: it also catches a
// future wrapper that intercepts differently.
//
// Porcelain `-z` framing is the validator. Real output is either empty or a sequence of
// NUL-terminated `XY path` records, where a rename or copy carries one extra NUL-terminated field
// (its origin). Non-empty output that does not end in NUL, or any record whose first two bytes are
// not status characters followed by a space, is `unknown`. So is a git that exits non-zero, cannot
// be spawned, or is killed by a signal.
//
// The verdict is a PRINTED TOKEN, not only an exit code: every run prints a first line
// `verdict: clean|dirty|unknown`, and a caller acts on that token. An exit 0 without the literal
// `verdict: clean` line is unknown to the caller — otherwise a wrapper that swallows this helper's
// own output would reproduce the exit-0-and-empty shape the helper exists to reject.
//
// Exit codes: 0 clean, 1 dirty (entries follow, one per line), 2 unknown or usage error.
//
// Out of scope: an executable git shim on PATH that emits well-formed but FALSE porcelain.
//
// Node built-ins plus ./main-module.mjs only; no network, no worktree/ref mutation.

import { spawnSync } from 'node:child_process'

import { isMainModule } from './main-module.mjs'

export const CLEAN = 'clean'
export const DIRTY = 'dirty'
export const UNKNOWN = 'unknown'

export const EXIT_CODES = Object.freeze({ [CLEAN]: 0, [DIRTY]: 1, [UNKNOWN]: 2 })

// Porcelain v1 status characters (index X and worktree Y columns).
const STATUS_CHAR = /^[ MTADRCU?!]$/
const COMMITTED_PREFIX = 'committed '

/**
 * Default runner: spawn git with no shell. Injectable so tests never shell out.
 * @param {string} repo
 * @param {string[]} args
 * @returns {{status: number|null, signal: string|null, stdout: string, stderr: string, error?: string}}
 */
export function runGit(repo, args) {
  const res = spawnSync('git', ['-C', repo, ...args], {
    encoding: 'utf8',
    env: { ...process.env, GIT_OPTIONAL_LOCKS: '0' },
    maxBuffer: 64 * 1024 * 1024,
  })
  return {
    status: res.status,
    signal: res.signal ?? null,
    stdout: res.stdout || '',
    stderr: res.stderr || '',
    ...(res.error ? { error: String(res.error.message || res.error) } : {}),
  }
}

/**
 * Parse porcelain-v1 `-z` status output. Returns `{ok: true, entries}` or `{ok: false, reason}`.
 * @param {string} stdout
 */
export function parsePorcelainZ(stdout) {
  if (stdout === '') return { ok: true, entries: [] }
  if (!stdout.endsWith('\0'))
    return { ok: false, reason: 'status output is not NUL-terminated porcelain' }
  const fields = stdout.slice(0, -1).split('\0')
  const entries = []
  for (let i = 0; i < fields.length; i += 1) {
    const rec = fields[i]
    if (
      rec.length < 4 ||
      !STATUS_CHAR.test(rec[0]) ||
      !STATUS_CHAR.test(rec[1]) ||
      rec[2] !== ' '
    ) {
      return { ok: false, reason: `malformed porcelain record ${JSON.stringify(rec.slice(0, 40))}` }
    }
    entries.push(rec.slice(3))
    if (rec[0] === 'R' || rec[0] === 'C' || rec[1] === 'R' || rec[1] === 'C') {
      i += 1
      if (i >= fields.length || fields[i] === '') {
        return { ok: false, reason: 'rename/copy record is missing its origin field' }
      }
    }
  }
  return { ok: true, entries }
}

/**
 * Parse `git diff --name-only -z` output.
 * @param {string} stdout
 */
export function parseNameOnlyZ(stdout) {
  if (stdout === '') return { ok: true, entries: [] }
  if (!stdout.endsWith('\0')) return { ok: false, reason: 'diff output is not NUL-terminated' }
  const names = stdout.slice(0, -1).split('\0')
  if (names.some((n) => n === '')) return { ok: false, reason: 'diff output has an empty path' }
  return { ok: true, entries: names }
}

function failed(res, what) {
  if (res.error) return `${what}: could not run git (${res.error})`
  if (res.signal) return `${what}: git killed by ${res.signal}`
  if (res.status !== 0)
    return `${what}: git exited ${res.status}${res.stderr ? ` (${res.stderr.trim()})` : ''}`
  return ''
}

/**
 * Compute the verdict.
 * @param {{repo?: string, untracked?: 'all'|'no', pathspecs?: string[], excludes?: string[], base?: string}} opts
 * @param {(repo: string, args: string[]) => ReturnType<typeof runGit>} [run]
 * @returns {{verdict: string, entries: string[], reason: string}}
 */
export function worktreeState(opts = {}, run = runGit) {
  const repo = opts.repo || process.cwd()
  const pathspecs = [
    ...(opts.pathspecs || []),
    ...(opts.excludes || []).map((p) => `:(exclude)${p}`),
  ]
  const unknown = (reason) => ({ verdict: UNKNOWN, entries: [], reason })
  let res
  try {
    res = run(repo, [
      'status',
      '--porcelain=v1',
      '-z',
      `--untracked-files=${opts.untracked || 'all'}`,
      '--',
      ...pathspecs,
    ])
  } catch (err) {
    return unknown(`status: could not run git (${err?.message || err})`)
  }
  const statusFail = failed(res, 'status')
  if (statusFail) return unknown(statusFail)
  const status = parsePorcelainZ(res.stdout)
  if (!status.ok) return unknown(status.reason)
  const entries = [...status.entries]
  if (opts.base) {
    let diff
    try {
      diff = run(repo, ['diff', '--name-only', '-z', `${opts.base}...HEAD`, '--', ...pathspecs])
    } catch (err) {
      return unknown(`diff: could not run git (${err?.message || err})`)
    }
    const diffFail = failed(diff, 'diff')
    if (diffFail) return unknown(diffFail)
    const committed = parseNameOnlyZ(diff.stdout)
    if (!committed.ok) return unknown(committed.reason)
    entries.push(...committed.entries.map((p) => `${COMMITTED_PREFIX}${p}`))
  }
  return entries.length === 0
    ? { verdict: CLEAN, entries: [], reason: '' }
    : { verdict: DIRTY, entries, reason: '' }
}

export const USAGE = `usage: worktree-state.mjs [--repo <dir>] [--untracked all|no] [--exclude <pathspec>]...
                          [--base <ref>] [--json] [-- <pathspec>...]
Prints "verdict: clean|dirty|unknown" first, then dirty entries one per line.
--base <ref> also lists paths committed since <ref>...HEAD, prefixed "committed ".
Exit: 0 clean, 1 dirty, 2 unknown or usage error. Treat anything but clean/dirty as unknown.
`

/**
 * Parse CLI arguments. Returns `{opts}` or `{usage: reason}`.
 * @param {string[]} argv
 */
export function parseArgs(argv) {
  const opts = { repo: '', untracked: 'all', pathspecs: [], excludes: [], base: '', json: false }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--') {
      opts.pathspecs = argv.slice(i + 1)
      if (opts.pathspecs.some((p) => p === '')) return { usage: 'an empty pathspec is not allowed' }
      break
    }
    if (flag === '--json') {
      opts.json = true
      continue
    }
    if (!['--repo', '--untracked', '--exclude', '--base'].includes(flag)) {
      return { usage: `unknown argument ${JSON.stringify(flag)} (pathspecs follow --)` }
    }
    const value = argv[i + 1]
    i += 1
    if (value === undefined || value === '') {
      // An empty --exclude would become `:(exclude)`, which excludes EVERYTHING and reads a dirty
      // tree as clean — so it is a usage error, never passed through.
      return { usage: `${flag} needs a non-empty value` }
    }
    if (flag === '--repo') opts.repo = value
    else if (flag === '--base') opts.base = value
    else if (flag === '--exclude') opts.excludes.push(value)
    else if (value === 'all' || value === 'no') opts.untracked = value
    else return { usage: `--untracked must be all or no, got ${JSON.stringify(value)}` }
  }
  return { opts }
}

/**
 * Run the CLI. Returns the exit code.
 * @param {string[]} argv
 * @param {{run?: typeof runGit, out?: (s: string) => void}} [deps]
 */
export function runCli(argv, deps = {}) {
  const out = deps.out || ((s) => process.stdout.write(s))
  const dd = argv.indexOf('--')
  const flags = dd === -1 ? argv : argv.slice(0, dd)
  if (flags.includes('--help') || flags.includes('-h')) {
    out(USAGE)
    return 0
  }
  const parsed = parseArgs(argv)
  const json = flags.includes('--json')
  const result = parsed.usage
    ? { verdict: UNKNOWN, entries: [], reason: `usage: ${parsed.usage}` }
    : worktreeState(parsed.opts, deps.run || runGit)
  if (json) {
    out(`${JSON.stringify(result)}\n`)
  } else {
    let text = `verdict: ${result.verdict}\n`
    if (result.reason) text += `reason: ${result.reason}\n`
    for (const e of result.entries) text += `${e.replace(/[\0\n]/g, '?')}\n`
    if (parsed.usage) text += USAGE
    out(text)
  }
  return EXIT_CODES[result.verdict]
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
