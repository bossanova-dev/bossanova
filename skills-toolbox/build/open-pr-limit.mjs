#!/usr/bin/env node
// Opt-in open-PR limit for the build stage: with `--max-open-prs N`, no new build starts while the
// repository already has N or more open PRs (drafts included). Without the flag nothing is read.
//
//   node open-pr-limit.mjs -- <skill args...>
//
// Prints one JSON line. Exit 0 = under the limit or no limit, 1 = at or over it, 64 = invalid
// arguments, 69 = the count could not be read (fail closed). Every non-zero exit carries `reason`.

import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'
import { extractFlagValue } from '../selection.mjs'

const MAX_OPEN_PRS_FLAG = '--max-open-prs'
const LISTED_NUMBERS = 10
// gh pages at 100, so this reads an exact count in one call for any ordinary repository.
const MIN_FETCH = 100

/**
 * Take `--max-open-prs` out of an argv. Throws on a missing, repeated or non-positive-integer value.
 * @param {string[]} argv
 * @returns {{limit: number|null, rest: string[]}}
 */
export function readOpenPrLimit(argv) {
  const parsed = extractFlagValue(argv, MAX_OPEN_PRS_FLAG)
  if ('error' in parsed) throw new Error(parsed.error)
  if (parsed.value === null) return { limit: null, rest: parsed.rest }
  const limit = Number(parsed.value)
  if (!/^[1-9]\d*$/.test(parsed.value.trim()) || !Number.isSafeInteger(limit)) {
    throw new Error(
      `${MAX_OPEN_PRS_FLAG} ${JSON.stringify(parsed.value)} must be a positive integer`,
    )
  }
  return { limit, rest: parsed.rest }
}

/** @param {number} limit */
function fetchSizeFor(limit) {
  return Math.max(limit + 1, MIN_FETCH)
}

/**
 * @typedef {{prs: number[], truncated: boolean} | {error: string}} OpenPrList
 * @typedef {(fetch: number) => Promise<OpenPrList>} OpenPrLister
 */

/**
 * List open PR numbers in the current repository through `gh`. A failure is returned, never thrown.
 * @param {{run?: Function, timeoutMs?: number}} [options]
 * @returns {OpenPrLister}
 */
export function createOpenPrLister({ run = spawnSync, timeoutMs = 10_000 } = {}) {
  const timeout = Number.isSafeInteger(timeoutMs) && timeoutMs > 0 ? timeoutMs : 10_000
  return async (fetch) => {
    let timer
    try {
      const result = await Promise.race([
        Promise.resolve().then(() =>
          run(
            'gh',
            ['pr', 'list', '--state', 'open', '--json', 'number', '--limit', String(fetch)],
            {
              encoding: 'utf8',
              timeout,
              killSignal: 'SIGKILL',
              stdio: ['ignore', 'pipe', 'pipe'],
            },
          ),
        ),
        new Promise((resolve) => {
          timer = setTimeout(() => resolve({ error: 'timeout' }), timeout)
        }),
      ])
      if (result?.error) return { error: String(result.error.code ?? result.error) }
      if (result?.status !== 0) return { error: 'command-failed' }
      const rows = JSON.parse(String(result.stdout))
      if (
        !Array.isArray(rows) ||
        !rows.every((row) => Number.isSafeInteger(row?.number) && row.number > 0)
      ) {
        return { error: 'invalid-pr-list' }
      }
      return { prs: rows.map((row) => row.number), truncated: rows.length >= fetch }
    } catch {
      return { error: 'lookup-failed' }
    } finally {
      clearTimeout(timer)
    }
  }
}

/**
 * @param {{limit: number, prs: number[], truncated: boolean}} input
 * @returns {{blocked: boolean, reason: string|null}}
 */
export function decideOpenPrLimit({ limit, prs, truncated }) {
  if (prs.length < limit) return { blocked: false, reason: null }
  const plus = truncated ? '+' : ''
  const shown = prs.slice(0, LISTED_NUMBERS).map((n) => `#${n}`)
  const more = prs.length - shown.length
  const list = more > 0 ? `${shown.join(', ')} and ${more}${plus} more` : shown.join(', ')
  return {
    blocked: true,
    reason: `${prs.length}${plus} open PRs reach ${MAX_OPEN_PRS_FLAG} ${limit} (${list})`,
  }
}

/**
 * The limit check the gate and the skill share. No limit ⇒ the lister is never called.
 * @param {{limit: number|null, listOpenPrs?: OpenPrLister}} input
 * @returns {Promise<{blocked: boolean, reason: string|null, prs?: number[]}>}
 */
export async function checkOpenPrLimit({ limit, listOpenPrs = createOpenPrLister() }) {
  if (limit === null) return { blocked: false, reason: null }
  const listed = await listOpenPrs(fetchSizeFor(limit))
  if ('error' in listed) {
    throw new Error(
      `open PRs unreadable through gh (${listed.error}); ${MAX_OPEN_PRS_FLAG} fails closed`,
    )
  }
  return { ...decideOpenPrLimit({ limit, ...listed }), prs: listed.prs }
}

/**
 * @param {string[]} argv the CLI's own argv: `-- <skill args...>`
 * @param {{listOpenPrs?: OpenPrLister}} [deps]
 * @returns {Promise<{code: number, out: object}>}
 */
export async function runCli(argv, { listOpenPrs = createOpenPrLister() } = {}) {
  let limit
  try {
    const { positionals } = parseArgs({ args: argv, options: {}, allowPositionals: true })
    ;({ limit } = readOpenPrLimit(positionals))
  } catch (err) {
    return { code: 64, out: { reason: err.message } }
  }
  try {
    const result = await checkOpenPrLimit({ limit, listOpenPrs })
    return { code: result.blocked ? 1 : 0, out: { limit, ...result } }
  } catch (err) {
    return { code: 69, out: { limit, reason: err.message } }
  }
}

if (isMainModule(import.meta.url)) {
  const { code, out } = await runCli(process.argv.slice(2))
  process.stdout.write(`${JSON.stringify(out)}\n`)
  process.exit(code)
}
