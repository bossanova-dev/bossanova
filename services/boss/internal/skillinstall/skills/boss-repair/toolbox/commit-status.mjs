#!/usr/bin/env node

// commit-status.mjs — the single owner of every GitHub commit-status write and read a skill makes.
//
// A commit status is attached to exactly one SHA and goes stale on the next push, which is what makes
// it the carrier for the `boss/build` receipt: "a boss actor produced and pushed this exact commit".
// Skill bodies name a verb and branch on its verdict; they never compose `gh api …/statuses` by hand.
// The receipt contract is documented in docs/skills/commit-status-receipts.md.
//
// Verbs:
//   post --context <name> --state success|failure|pending|error --sha <40-hex>
//        [--description <text>] [--target-url <http(s) url>] [--repo <owner/name>]
//     exit 0  {verdict:'posted', …}      the status was written
//     exit 1  {verdict:'failed', reason} gh could not be run or exited non-zero
//   read --sha <40-hex> [--context <name>] [--repo <owner/name>]
//     exit 0  {verdict:'present'|'absent'|'unknown', sha, contexts, …}
//             `unknown` (with a `reason`) means the read failed and is never `present`; `present`
//             means a status exists, not that it is `success` — compare `state` for that.
//   exit 2 on a usage or validation error, message on stderr, gh never spawned.
//
// Node builtins plus ./main-module.mjs only, so the vendoring closure is this one file.

import { spawnSync } from 'node:child_process'

import { isMainModule } from './main-module.mjs'

// The receipt context. `pr-check-state.mjs` keeps its own copy in PROVENANCE_CONTEXTS so its
// vendoring closure stays one file; the sibling test pins the two together.
export const RECEIPT_CONTEXT = 'boss/build'

export const STATUS_STATES = Object.freeze(['success', 'failure', 'pending', 'error'])

// GitHub rejects a commit-status description longer than 140 characters.
export const DESCRIPTION_LIMIT = 140

export const READ_VERDICTS = Object.freeze({
  PRESENT: 'present',
  ABSENT: 'absent',
  UNKNOWN: 'unknown',
})

const GH_TIMEOUT_MS = 30_000

export class UsageError extends Error {}

function normalizeSha(sha) {
  const value = typeof sha === 'string' ? sha.trim().toLowerCase() : ''
  if (!/^[0-9a-f]{40}$/.test(value)) {
    throw new UsageError(
      `commit-status: --sha must be a 40-character hex SHA, got ${JSON.stringify(sha)}`,
    )
  }
  return value
}

function validateContext(context) {
  if (typeof context !== 'string' || context.trim() === '') {
    throw new UsageError('commit-status: --context must be a non-empty name')
  }
  // Whitespace and control characters: a context is a machine name compared byte-for-byte.
  if (/[\s\p{Cc}]/u.test(context)) {
    throw new UsageError(
      `commit-status: --context must not contain whitespace or control characters, got ${JSON.stringify(context)}`,
    )
  }
  return context
}

function validateRepo(repo) {
  if (repo === undefined || repo === null || repo === '') return ''
  if (typeof repo !== 'string' || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo)) {
    throw new UsageError(`commit-status: --repo must be owner/name, got ${JSON.stringify(repo)}`)
  }
  return repo
}

// capDescription caps the text at DESCRIPTION_LIMIT Unicode code points, so a surrogate pair is never
// split. A truncated result ends in `…` and is still within the limit.
export function capDescription(text) {
  const value = typeof text === 'string' ? text : ''
  const points = Array.from(value)
  if (points.length <= DESCRIPTION_LIMIT) return value
  return `${points.slice(0, DESCRIPTION_LIMIT - 1).join('')}…`
}

// validatePostInput normalises and validates a post request, throwing UsageError on bad input.
export function validatePostInput({ context, state, sha, description, targetUrl, repo } = {}) {
  const normalized = {
    context: validateContext(context),
    state: '',
    sha: normalizeSha(sha),
    description: '',
    targetUrl: '',
    repo: validateRepo(repo),
  }
  if (!STATUS_STATES.includes(state)) {
    throw new UsageError(
      `commit-status: --state must be one of ${STATUS_STATES.join(' | ')}, got ${JSON.stringify(state)}`,
    )
  }
  normalized.state = state
  if (description !== undefined && description !== null && description !== '') {
    if (typeof description !== 'string') {
      throw new UsageError('commit-status: --description must be text')
    }
    normalized.description = capDescription(description)
  }
  if (targetUrl !== undefined && targetUrl !== null && targetUrl !== '') {
    if (typeof targetUrl !== 'string' || !/^https?:\/\/\S+$/.test(targetUrl)) {
      throw new UsageError(
        `commit-status: --target-url must be an http(s):// URL, got ${JSON.stringify(targetUrl)}`,
      )
    }
    normalized.targetUrl = targetUrl
  }
  return normalized
}

function repoPath(repo) {
  return repo === '' ? 'repos/{owner}/{repo}' : `repos/${repo}`
}

// postArgs builds the gh argv. Always `-f` (raw field), never `-F`: a typed field reads a value that
// begins with `@` as a file path, and a description is free text.
export function postArgs(input) {
  const v = validatePostInput(input)
  const args = [
    'api',
    '--method',
    'POST',
    `${repoPath(v.repo)}/statuses/${v.sha}`,
    '-f',
    `state=${v.state}`,
    '-f',
    `context=${v.context}`,
  ]
  if (v.description !== '') args.push('-f', `description=${v.description}`)
  if (v.targetUrl !== '') args.push('-f', `target_url=${v.targetUrl}`)
  return args
}

// readArgs lists every status on the commit. The list endpoint is reverse-chronological, so the
// first status per context is its latest; `--paginate --slurp` makes the result one JSON array of
// pages however many statuses the commit carries.
export function readArgs({ sha, repo } = {}) {
  const v = { sha: normalizeSha(sha), repo: validateRepo(repo) }
  return [
    'api',
    `${repoPath(v.repo)}/commits/${v.sha}/statuses?per_page=100`,
    '--paginate',
    '--slurp',
  ]
}

function epoch(value) {
  const ms = typeof value === 'string' ? Date.parse(value) : Number.NaN
  return Number.isFinite(ms) ? ms : null
}

// statusNodes flattens the accepted payload shapes into one list, or returns null for a wrong shape:
// a slurped page array (array of arrays), a single page (array), or a combined-status object
// (`{statuses: [...]}`).
function statusNodes(payload) {
  let nodes
  if (Array.isArray(payload)) {
    nodes = payload.every((page) => Array.isArray(page)) ? payload.flat() : payload
  } else if (payload && typeof payload === 'object' && Array.isArray(payload.statuses)) {
    nodes = payload.statuses
  } else {
    return null
  }
  for (const node of nodes) {
    if (!node || typeof node !== 'object' || Array.isArray(node)) return null
    if (typeof node.context !== 'string' || typeof node.state !== 'string') return null
  }
  return nodes
}

// classifyRead turns one gh read into a verdict. A failed or malformed read is `unknown` with a
// reason — never `absent`, so a caller can tell "no receipt" from "could not look".
export function classifyRead({ exit, stdout, error, context, sha = '' } = {}) {
  const base = { sha, contexts: {} }
  if (context !== undefined) base.context = context
  const unknown = (reason) => ({ verdict: READ_VERDICTS.UNKNOWN, ...base, reason })

  if (error) return unknown(`gh could not be run: ${error?.code ?? error?.message ?? error}`)
  if (exit !== 0) return unknown(`gh exited ${exit}`)
  let payload
  try {
    payload = JSON.parse(stdout)
  } catch {
    return unknown('unparseable gh output')
  }
  const nodes = statusNodes(payload)
  if (nodes === null) return unknown('unexpected payload shape')

  const contexts = {}
  for (const node of nodes) {
    const entry = {
      state: node.state,
      description: typeof node.description === 'string' ? node.description : '',
      targetUrl: typeof node.target_url === 'string' ? node.target_url : '',
      updatedAt: typeof node.updated_at === 'string' ? node.updated_at : '',
    }
    const prior = contexts[node.context]
    // Reverse-chronological list: the first status per context is the latest. A later entry only
    // displaces it when both timestamps parse and it is strictly newer.
    if (prior === undefined) {
      contexts[node.context] = entry
    } else {
      const a = epoch(prior.updatedAt)
      const b = epoch(entry.updatedAt)
      if (a !== null && b !== null && b > a) contexts[node.context] = entry
    }
  }

  const result = { ...base, contexts }
  if (context === undefined) {
    return {
      verdict: Object.keys(contexts).length > 0 ? READ_VERDICTS.PRESENT : READ_VERDICTS.ABSENT,
      ...result,
    }
  }
  const hit = contexts[context]
  if (hit === undefined) return { verdict: READ_VERDICTS.ABSENT, ...result }
  return { verdict: READ_VERDICTS.PRESENT, ...result, state: hit.state }
}

function runGh(spawn, args) {
  return spawn('gh', args, {
    encoding: 'utf8',
    timeout: GH_TIMEOUT_MS,
    killSignal: 'SIGKILL',
    maxBuffer: 64 * 1024 * 1024,
  })
}

// ---------------------------------------------------------------------------
// CLI.

const CLI_FLAGS = Object.freeze({
  post: ['context', 'state', 'sha', 'description', 'target-url', 'repo'],
  read: ['sha', 'context', 'repo'],
})

function parseFlags(verb, argv) {
  const accepted = CLI_FLAGS[verb]
  const shape = accepted.map((name) => `--${name}`).join(', ')
  const flags = {}
  for (let i = 0; i < argv.length; i += 1) {
    const key = argv[i]
    if (typeof key !== 'string' || !key.startsWith('--')) {
      throw new UsageError(
        `commit-status: ${verb}(${shape}) — unexpected argument ${JSON.stringify(key)}`,
      )
    }
    const name = key.slice(2)
    if (!accepted.includes(name)) {
      throw new UsageError(`commit-status: ${verb}(${shape}) — unrecognised flag --${name}`)
    }
    const next = argv[i + 1]
    if (typeof next !== 'string' || next.startsWith('--')) {
      throw new UsageError(`commit-status: ${verb}(${shape}) — --${name} needs a value`)
    }
    flags[name] = next
    i += 1
  }
  return flags
}

// main runs one verb and returns `{exitCode, result}`; it throws UsageError on bad input before gh is
// spawned. `deps.spawn` defaults to `spawnSync`.
export function main(argv, deps = {}) {
  const spawn = deps.spawn ?? spawnSync
  const [cmd, ...rest] = argv

  if (cmd === 'post') {
    const flags = parseFlags('post', rest)
    const input = {
      context: flags.context,
      state: flags.state,
      sha: flags.sha,
      description: flags.description,
      targetUrl: flags['target-url'],
      repo: flags.repo,
    }
    const args = postArgs(input)
    const v = validatePostInput(input)
    const res = runGh(spawn, args)
    const posted = { sha: v.sha, context: v.context, state: v.state }
    if (v.description !== '') posted.description = v.description
    if (v.targetUrl !== '') posted.targetUrl = v.targetUrl
    if (res?.error) {
      return {
        exitCode: 1,
        result: {
          verdict: 'failed',
          ...posted,
          reason: `gh could not be run: ${res.error.code ?? res.error.message}`,
        },
      }
    }
    if (res?.status !== 0) {
      const stderr = typeof res?.stderr === 'string' ? res.stderr.trim().split('\n')[0] : ''
      return {
        exitCode: 1,
        result: {
          verdict: 'failed',
          ...posted,
          reason: `gh exited ${res?.status}${stderr ? `: ${stderr}` : ''}`,
        },
      }
    }
    return { exitCode: 0, result: { verdict: 'posted', ...posted } }
  }

  if (cmd === 'read') {
    const flags = parseFlags('read', rest)
    if (flags.sha === undefined) throw new UsageError('commit-status: read needs --sha <40-hex>')
    const context = flags.context === undefined ? undefined : validateContext(flags.context)
    const args = readArgs({ sha: flags.sha, repo: flags.repo })
    const sha = normalizeSha(flags.sha)
    const res = runGh(spawn, args)
    return {
      exitCode: 0,
      result: classifyRead({
        exit: res?.status,
        stdout: res?.stdout,
        error: res?.error,
        context,
        sha,
      }),
    }
  }

  throw new UsageError(
    `commit-status: unknown command ${cmd ?? '(none)'} (expected "post" or "read")`,
  )
}

if (isMainModule(import.meta.url)) {
  try {
    const { exitCode, result } = main(process.argv.slice(2))
    process.stdout.write(`${JSON.stringify(result)}\n`)
    process.exitCode = exitCode
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
