#!/usr/bin/env node
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import {
  readPr,
  readStatus,
  commandReader,
  claimState,
  parkOfStatus,
  CLAIM_STALE_AFTER_MS,
} from './verify-gate.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS } from './verify-vocabulary.mjs'

export const CEILING_MS = 570000
const DEFAULT_CHUNK_MS = 480000
const POLL_MS = 30000

export function verdictLine({ state, description = '', head, merged = false }) {
  const short = head.slice(0, 7)
  if (merged) return `Verified and merged ${short}`
  const park = parkOfStatus({ state, description })
  if (park) return `Parked for you on ${short}: ${park.label || park.codesText}`
  if (description.startsWith(VERIFY_DESCRIPTIONS.verified)) return `Verified ${short}`
  if (
    [VERIFY_DESCRIPTIONS.defect, VERIFY_DESCRIPTIONS.blocked].some((prefix) =>
      description.startsWith(prefix),
    )
  ) {
    return `Sent back for repair on ${short}: ${description}`
  }
  if (description.startsWith(VERIFY_DESCRIPTIONS.waiting)) {
    return `Waiting on ${short}: ${description.slice(VERIFY_DESCRIPTIONS.waiting.length).trim()}`
  }
  return ''
}

// The injected reader uses verify-gate's existing command/read contract. Production reads are
// bounded by the remaining chunk, including subprocess execution time.
export async function awaitVerdict({
  read,
  now = Date.now,
  delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
  options,
}) {
  const { pr, head, repo = '', chunkMs = DEFAULT_CHUNK_MS } = options
  const end = now() + chunkMs
  let token = '',
    deadline = 0
  const chunkEnded = () =>
    token
      ? { outcome: 'continue', token, deadline }
      : { outcome: 'unknown', why: 'chunk-exhausted-before-claim' }
  for (;;) {
    if (now() >= end) return chunkEnded()
    const view = readPr(read, pr, repo)
    if (!view.ok) return { outcome: 'unknown', why: 'pr-unreadable' }
    const p = view.payload
    if (p?.headRefOid && p.headRefOid !== head) {
      return {
        outcome: 'head-moved',
        newHead: p.headRefOid,
        line: `Head moved from ${head.slice(0, 7)} to ${p.headRefOid.slice(0, 7)}; the new head gets its own verify run`,
      }
    }
    if (p?.state === 'MERGED')
      return { outcome: 'merged', line: verdictLine({ state: 'success', head, merged: true }) }
    if (p?.state === 'CLOSED')
      return { outcome: 'closed', line: `Closed without merging ${head.slice(0, 7)}` }
    if (p?.state !== 'OPEN' || p.headRefOid !== head)
      return { outcome: 'unknown', why: 'pr-unreadable' }
    if (now() >= end) return chunkEnded()
    const reading = readStatus(read, head, repo, VERIFY_CONTEXT)
    if (reading.verdict === 'unknown') return { outcome: 'unknown', why: 'status-unreadable' }
    const status = reading.contexts?.[VERIFY_CONTEXT]
    const claim = claimState(status, { now: now() })
    if (claim.kind === 'stale') return { outcome: 'stale', token: claim.token }
    if (claim.kind === 'terminal' || claim.kind === 'waiting') {
      const line = verdictLine({ state: status.state, description: status.description, head })
      return line
        ? { outcome: 'verdict', state: status.state, description: status.description, line }
        : { outcome: 'unknown', why: 'unrecognized-status' }
    }
    if (claim.kind !== 'foreign' || !claim.token)
      return { outcome: 'unknown', why: 'no-followable-claim' }
    token = claim.token
    deadline = Date.parse(status.updatedAt) + CLAIM_STALE_AFTER_MS
    const remaining = end - now()
    if (remaining <= 0) return { outcome: 'continue', token, deadline }
    // claimState uses a strict age comparison; read once beyond the deadline to observe stale.
    await delay(Math.min(POLL_MS, remaining, Math.max(1, deadline - now() + 1)))
  }
}

export function parseOptions(argv) {
  const { values } = parseArgs({
    args: argv,
    strict: true,
    options: {
      pr: { type: 'string' },
      head: { type: 'string' },
      repo: { type: 'string' },
      'chunk-ms': { type: 'string' },
    },
  })
  const pr = Number(values.pr),
    chunkMs = Number(values['chunk-ms'] ?? DEFAULT_CHUNK_MS)
  if (!Number.isInteger(pr) || pr < 1) throw new Error('await needs --pr <positive integer>')
  if (!/^[a-f0-9]{40}$/i.test(values.head ?? ''))
    throw new Error('await needs --head <40-character SHA>')
  if (!Number.isInteger(chunkMs) || chunkMs < 1 || chunkMs > CEILING_MS)
    throw new Error(`chunk must fit the ${CEILING_MS}ms ceiling`)
  return { pr, head: values.head, repo: values.repo ?? '', chunkMs }
}

export async function main(argv) {
  const [command, ...rest] = argv
  if (command !== 'await') throw new Error('expected await --pr <n> --head <sha>')
  const options = parseOptions(rest)
  const end = Date.now() + options.chunkMs
  const read = commandReader({
    spawn: /** @type {typeof spawnSync} */ (
      (command, args, opts) =>
        spawnSync(command, args, {
          ...opts,
          timeout: Math.max(1, Math.min(opts.timeout, end - Date.now())),
        })
    ),
  })
  return awaitVerdict({ read, options })
}

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(`${JSON.stringify(await main(process.argv.slice(2)))}\n`)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 2
  }
}
