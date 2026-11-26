// verify-extension-leg.mjs — one `role: verify` extension leg, held to its deadline.
//   start  records the leg: its deadline (the descriptor's `timeoutMinutes`, else the default leg
//          timeout), its output paths and a snapshot of the worktree.
//   wait   one bounded in-turn hold until the result file lands or the deadline passes, re-posting
//          this run's claim every CLAIM_HEARTBEAT_EVERY_MS so a long leg never reads as abandoned.
//          A landed result is reported once; a later wait holds on until the deadline, because the
//          subagent may still be writing until it returns.
//   finish compares the worktree with the snapshot and writes the leg's `--extension-results`
//          entry. A tracked or unignored change, a moved HEAD, or an ignored change the extension
//          did not declare with `writes: ignored` marks the entry `dirtied`. Each listed path is
//          fingerprinted, so rewriting a file that was already modified, untracked or ignored
//          counts as a change too. HARNESS_STATE_PATHS are left out of every snapshot.

import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  readlinkSync,
  renameSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'
import { parseArgs } from 'node:util'
import {
  DEFAULT_POLL_INTERVAL_MS,
  DEFAULT_WAIT_BUDGET_MS,
  WAIT_EXIT_CODES,
  legDeadlineAt,
  legTimeoutMsFromEnv,
} from '../bs-dispatch-await.mjs'
import { isMainModule } from '../main-module.mjs'
import { CLAIM_HEARTBEAT_EVERY_MS } from './verify-vocabulary.mjs'

export const LEG_EXIT_CODES = Object.freeze({ ...WAIT_EXIT_CODES, 'claim-lost': 94 })
// A failed heartbeat post is retried this soon rather than a whole heartbeat interval later.
export const HEARTBEAT_RETRY_MS = 60_000
export const MAX_CHANGED_PATHS = 20
// Filesystem entries an ignored directory's fingerprint visits before it stops; a write past the
// bound inside an ignored directory that was already present goes unseen.
export const IGNORED_WALK_LIMIT = 100_000
// Worktree files the agent harness and daemon rewrite while a leg runs (Claude Code's scheduler
// lock, the daemon-owned hook settings). A change there is not the leg's, so no snapshot reads them.
export const HARNESS_STATE_PATHS = Object.freeze([
  '.claude/scheduled_tasks.lock',
  '.claude/settings.local.json',
])
const HARNESS_STATE = new Set(HARNESS_STATE_PATHS)
const GIT_TIMEOUT_MS = 120_000
const GATE_TIMEOUT_MS = 60_000
const NAME = /^[a-z0-9][a-z0-9._-]*$/
const HERE = dirname(fileURLToPath(import.meta.url))

const legPath = (runDir, name) => join(runDir, 'legs', `${name}.leg.json`)

function writeJson(path, value) {
  const tmp = `${path}.tmp`
  writeFileSync(tmp, `${JSON.stringify(value, null, 2)}\n`)
  renameSync(tmp, path)
}

function readJson(path) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'))
  } catch {
    return null
  }
}

function readLeg(runDir, name) {
  const leg = readJson(legPath(runDir, name))
  if (!leg || leg.version !== 2) throw new Error(`no started leg ${name}; run start first`)
  return leg
}

/** @param {string} cwd @returns {(args: string[]) => string} */
export function gitIn(cwd) {
  return (args) => {
    const r = spawnSync('git', args, { cwd, encoding: 'utf8', timeout: GIT_TIMEOUT_MS })
    if (r.status !== 0) throw new Error(`git ${args[0]} failed: ${(r.stderr || '').trim()}`)
    return r.stdout
  }
}

/**
 * One path's fingerprint: a content hash for a file or symlink, `-` when it is absent, and for a
 * directory a digest of every entry below it. `content: false` (ignored paths) uses size and
 * mtime instead of reading files, and `budget` bounds the walk. `rel` is the root-relative path;
 * a directory walk skips HARNESS_STATE_PATHS below it.
 * @param {string} abs
 * @param {string} rel
 * @param {{content: boolean, budget: {left: number}}} opts
 * @returns {string}
 */
function fingerprint(abs, rel, opts) {
  let st
  try {
    st = lstatSync(abs)
  } catch {
    return '-'
  }
  if (st.isSymbolicLink()) return `l:${readlinkSync(abs)}`
  if (st.isFile())
    return opts.content
      ? `f:${createHash('sha256').update(readFileSync(abs)).digest('hex')}`
      : `s:${st.size}:${st.mtimeMs}`
  if (!st.isDirectory()) return `o:${st.mode}:${st.mtimeMs}`
  const hash = createHash('sha256')
  for (const name of readdirSync(abs).sort()) {
    const child = `${rel.replace(/\/$/, '')}/${name}`
    if (HARNESS_STATE.has(child)) continue
    if (opts.budget.left <= 0) {
      hash.update('\0capped')
      break
    }
    opts.budget.left -= 1
    hash.update(`${name}\0${fingerprint(join(abs, name), child, opts)}\0`)
  }
  return `d:${hash.digest('hex')}`
}

/**
 * HEAD plus every `git status` entry, untracked files listed one by one and ignored directories
 * collapsed, each as `{entry: 'XY path', print}`. `print` fingerprints the path's current
 * contents; ignored entries are fingerprinted only when `ignored` is true.
 * @param {(args: string[]) => string} git
 * @param {{ignored?: boolean}} [opts]
 */
export function worktreeSnapshot(git, { ignored = true } = {}) {
  const head = git(['rev-parse', 'HEAD']).trim()
  const root = git(['rev-parse', '--show-toplevel']).trim()
  const fields = git([
    ...['status', '--porcelain=v1', '-z'],
    ...['--untracked-files=all', '--ignored=matching'],
  ]).split('\0')
  const budget = { left: IGNORED_WALK_LIMIT }
  const entries = []
  for (let i = 0; i < fields.length; i += 1) {
    const entry = fields[i]
    if (entry === '') continue
    // A rename or copy record carries its source path as the next field.
    if (/^[RC]/.test(entry)) i += 1
    const path = entry.slice(3)
    if (HARNESS_STATE.has(path)) continue
    const isIgnored = entry.startsWith('!!')
    const print =
      isIgnored && !ignored
        ? ''
        : fingerprint(join(root, path), path, { content: !isIgnored, budget })
    entries.push({ entry, print })
  }
  return {
    head,
    entries: entries.sort((a, b) => (a.entry < b.entry ? -1 : a.entry > b.entry ? 1 : 0)),
  }
}

/**
 * What changed between two snapshots, and whether that dirtied the worktree for a leg that
 * declared `writes` (`ignored` or `none`). An entry whose status or fingerprint differs changed.
 * @param {{head: string, entries: {entry: string, print: string}[]}} before
 * @param {{head: string, entries: {entry: string, print: string}[]}} after
 * @param {string} writes
 */
export function worktreeChanges(before, after, writes) {
  const key = (e) => `${e.entry}\0${e.print}`
  const was = new Set(before.entries.map(key))
  const now = new Set(after.entries.map(key))
  const byPath = new Map()
  for (const e of [
    ...after.entries.filter((e) => !was.has(key(e))),
    ...before.entries.filter((e) => !now.has(key(e))),
  ]) {
    const path = e.entry.slice(3)
    const ignored = e.entry.startsWith('!!') && byPath.get(path)?.ignored !== false
    byPath.set(path, { path, ignored })
  }
  const changed = [...byPath.values()]
  const headMoved = before.head !== after.head
  const dirtied = headMoved || changed.some((change) => !change.ignored || writes !== 'ignored')
  return { dirtied, headMoved, changed }
}

/**
 * Record one leg before its dispatch. Re-running start for a leg replaces it.
 * @param {{runDir: string, discovery: any, name: string, git: (args: string[]) => string,
 *   now?: number, env?: NodeJS.ProcessEnv}} input
 */
export function startLeg({ runDir, discovery, name, git, now = Date.now(), env = process.env }) {
  if (!NAME.test(name)) throw new Error(`invalid extension name ${JSON.stringify(name)}`)
  const descriptor = (discovery?.extensions ?? []).find((ext) => ext?.name === name)
  if (!descriptor) throw new Error(`extension ${name} is not in the discovery output`)
  const timeoutMs = Number.isInteger(descriptor.timeoutMinutes)
    ? descriptor.timeoutMinutes * 60_000
    : legTimeoutMsFromEnv(env)
  const dir = join(runDir, 'legs')
  const runTmp = join(dir, `${name}.tmp`)
  mkdirSync(runTmp, { recursive: true })
  const writes = descriptor.writes === 'ignored' ? 'ignored' : 'none'
  const leg = {
    version: 2,
    extension: name,
    optional: descriptor.optional === true,
    writes,
    timeoutMs,
    dispatchedAt: now,
    deadlineAt: legDeadlineAt(now, timeoutMs),
    outPath: join(dir, `${name}.result.json`),
    runTmp,
    baseline: worktreeSnapshot(git, { ignored: writes !== 'ignored' }),
    lastBeatAt: 0,
    lastBeatAttemptAt: 0,
  }
  rmSync(leg.outPath, { force: true })
  writeJson(legPath(runDir, name), leg)
  const { baseline: _baseline, ...shown } = leg
  return shown
}

/**
 * One bounded hold. `beat` re-posts the claim and returns the gate's `{won, reason}`. Outcomes:
 * `completed` (the result file first parses), `timed-out`, `claim-lost`, or `budget` (re-arm).
 * A wait after `completed` never completes again: it beats until the deadline or its budget,
 * and its `timed-out` only stops the subagent; the leg keeps its `completed` result.
 * @param {{runDir: string, name: string, beat: () => {won?: boolean, reason?: string},
 *   budgetMs?: number, intervalMs?: number, now?: () => number,
 *   sleep?: (ms: number) => Promise<unknown>}} input
 */
export async function waitLeg({
  runDir,
  name,
  beat,
  budgetMs = DEFAULT_WAIT_BUDGET_MS,
  intervalMs = DEFAULT_POLL_INTERVAL_MS,
  now = Date.now,
  sleep = delay,
}) {
  const leg = readLeg(runDir, name)
  const start = now()
  const settle = (outcome, extra = {}) => {
    if (outcome !== 'budget' && leg.outcome !== 'completed') leg.outcome = outcome
    writeJson(legPath(runDir, name), leg)
    return { outcome, extension: name, waitedMs: now() - start, ...extra }
  }
  for (;;) {
    const at = now()
    if (leg.outcome !== 'completed' && readJson(leg.outPath) !== null) return settle('completed')
    if (at >= leg.deadlineAt) return settle('timed-out', { timeoutMs: leg.timeoutMs })
    if (
      at - leg.lastBeatAt >= CLAIM_HEARTBEAT_EVERY_MS &&
      at - leg.lastBeatAttemptAt >= HEARTBEAT_RETRY_MS
    ) {
      leg.lastBeatAttemptAt = at
      const r = beat()
      if (r?.won === true) leg.lastBeatAt = at
      else if (r?.reason === 'claim-lost') return settle('claim-lost')
      writeJson(legPath(runDir, name), leg)
    }
    const left = budgetMs - (now() - start)
    if (left <= 0) return settle('budget', { deadlineAt: leg.deadlineAt })
    await sleep(Math.min(intervalMs, left, Math.max(1, leg.deadlineAt - now())))
  }
}

/**
 * The leg's `--extension-results` entry, upserted into `resultsPath` by extension name.
 * @param {{runDir: string, name: string, resultsPath: string, git: (args: string[]) => string}} input
 */
export function finishLeg({ runDir, name, resultsPath, git }) {
  const leg = readLeg(runDir, name)
  const timedOut = leg.outcome === 'timed-out'
  const result = timedOut ? null : readJson(leg.outPath)
  const after = worktreeSnapshot(git, { ignored: leg.writes !== 'ignored' })
  const changes = worktreeChanges(leg.baseline, after, leg.writes)
  const entry = {
    extension: name,
    optional: leg.optional,
    timedOut,
    crashed: !timedOut && result === null,
    result,
    ...(changes.dirtied
      ? {
          dirtied: true,
          changed: [
            ...(changes.headMoved ? ['HEAD'] : []),
            ...changes.changed.map((change) => change.path),
          ].slice(0, MAX_CHANGED_PATHS),
        }
      : {}),
  }
  const prior = readJson(resultsPath)
  const entries = Array.isArray(prior) ? prior.filter((e) => e?.extension !== name) : []
  writeJson(resultsPath, [...entries, entry])
  return entry
}

/** The claim heartbeat through `verify-gate.mjs post --verdict claim --heartbeat`. */
export function gateHeartbeat({ pr, head, token, repo, dryRun }) {
  return () => {
    const r = spawnSync(
      process.execPath,
      [
        join(HERE, 'verify-gate.mjs'),
        ...['post', '--pr', pr, '--head', head, '--verdict', 'claim', '--heartbeat'],
        ...['--token', token, ...(repo ? ['--repo', repo] : []), ...(dryRun ? ['--dry-run'] : [])],
      ],
      { encoding: 'utf8', timeout: GATE_TIMEOUT_MS },
    )
    const line = String(r.stdout ?? '')
      .trim()
      .split('\n')
      .pop()
    try {
      return JSON.parse(line ?? '')
    } catch {
      return { won: false, reason: 'gate-unreadable' }
    }
  }
}

const USAGE = [
  'usage: verify-extension-leg.mjs start --run-dir <dir> --discovery <file> --name <ext>',
  '  | wait --run-dir <dir> --name <ext> --pr <n> --head <sha> --token <t> [--repo o/r] [--dry-run]',
  '         [--budget <ms>] [--interval <ms>]',
  '         (exit 0 completed | 96 timed-out | 94 claim-lost | 98 budget spent: re-arm)',
  '  | finish --run-dir <dir> --name <ext> --results <file>',
].join('\n')

const positive = (raw, flag) => {
  if (raw === undefined) return undefined
  const value = Number(raw)
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${flag} must be a positive number`)
  return value
}

export async function main(argv = process.argv.slice(2), { cwd = process.cwd() } = {}) {
  const { values, positionals } = parseArgs({
    args: argv,
    strict: true,
    allowPositionals: true,
    options: {
      'run-dir': { type: 'string' },
      discovery: { type: 'string' },
      name: { type: 'string' },
      pr: { type: 'string' },
      head: { type: 'string' },
      token: { type: 'string' },
      repo: { type: 'string' },
      'dry-run': { type: 'boolean', default: false },
      budget: { type: 'string' },
      interval: { type: 'string' },
      results: { type: 'string' },
    },
  })
  const [command, ...extra] = positionals
  const runDir = values['run-dir']
  const name = values.name
  const need = (keys) => {
    if (extra.length > 0 || !runDir || !name || keys.some((key) => !values[key]))
      throw new Error(USAGE)
  }
  const git = gitIn(cwd)
  if (command === 'start') {
    need(['discovery'])
    const discovery = JSON.parse(readFileSync(values.discovery, 'utf8'))
    return { code: 0, out: startLeg({ runDir, discovery, name, git }) }
  }
  if (command === 'wait') {
    need(['pr', 'head', 'token'])
    const beat = gateHeartbeat({ ...values, dryRun: values['dry-run'] })
    const budgetMs = positive(values.budget, '--budget')
    const intervalMs = positive(values.interval, '--interval')
    const out = await waitLeg({ runDir, name, beat, budgetMs, intervalMs })
    return { code: LEG_EXIT_CODES[out.outcome], out }
  }
  if (command === 'finish') {
    need(['results'])
    return { code: 0, out: finishLeg({ runDir, name, resultsPath: values.results, git }) }
  }
  throw new Error(USAGE)
}

if (isMainModule(import.meta.url)) {
  main().then(
    ({ code, out }) => {
      process.stdout.write(`${JSON.stringify(out)}\n`)
      process.exitCode = code
    },
    (error) => {
      process.stderr.write(`${error.message}\n`)
      process.exitCode = 2
    },
  )
}
