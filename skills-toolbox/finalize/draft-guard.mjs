// Read-only draft intent check. A once-ready PR's draft is a hold to preserve.
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'

const QUERY = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      isDraft
      state
      timelineItems(last: 1, itemTypes: [READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT]) {
        totalCount
        nodes { __typename ... on ReadyForReviewEvent { createdAt } ... on ConvertToDraftEvent { createdAt } }
      }
    }
  }
}`

/** @param {string} reason */
function unknown(reason) {
  return { verdict: 'unknown', reason, isDraft: null, readyOnceEvents: null }
}

/**
 * readyOnceEvents is bounded presence evidence (0 or 1), not a total event count.
 * GitHub's totalCount includes unrelated events despite the itemTypes filter;
 * only the filtered nodes establish whether a ready/draft transition exists.
 * @param {unknown} payload
 */
export function classifyReadyIntent(payload) {
  if (!payload || typeof payload !== 'object' || !('data' in payload))
    return unknown('missing GraphQL data')
  if ('errors' in payload && (!Array.isArray(payload.errors) || payload.errors.length > 0))
    return unknown('GraphQL errors returned')
  const data = record(payload.data)
  const pr = record(record(data?.repository)?.pullRequest)
  if (
    !pr ||
    typeof pr.isDraft !== 'boolean' ||
    typeof pr.state !== 'string' ||
    !['OPEN', 'CLOSED', 'MERGED'].includes(pr.state)
  )
    return unknown('missing or malformed pullRequest')
  const timeline = record(pr.timelineItems)
  if (
    !timeline ||
    typeof timeline.totalCount !== 'number' ||
    !Number.isSafeInteger(timeline.totalCount) ||
    timeline.totalCount < 0 ||
    !Array.isArray(timeline.nodes)
  )
    return unknown('missing or malformed timelineItems')
  const event = timeline.nodes[0]
  if (
    timeline.nodes.length > 1 ||
    (timeline.nodes.length === 1 &&
      (!event ||
        !['ReadyForReviewEvent', 'ConvertToDraftEvent'].includes(event.__typename) ||
        typeof event.createdAt !== 'string' ||
        !Number.isFinite(Date.parse(event.createdAt))))
  )
    return unknown('malformed draft history event')
  const result = { isDraft: pr.isDraft, readyOnceEvents: timeline.nodes.length }
  if (pr.state !== 'OPEN')
    return { ...result, verdict: 'not-open', reason: `PR state is ${pr.state}` }
  if (!pr.isDraft)
    return { ...result, verdict: 'already-ready', reason: 'open PR is already ready' }
  if (timeline.nodes.length === 0) {
    return { ...result, verdict: 'ready', reason: 'draft now; no ready or convert-to-draft events' }
  }
  return {
    ...result,
    verdict: 'keep-draft',
    reason: `draft now; ${event.__typename} at ${event.createdAt}`,
  }
}

/** @param {unknown} value @returns {Record<string, unknown> | null} */
function record(value) {
  return value && typeof value === 'object' ? /** @type {Record<string, unknown>} */ (value) : null
}

/**
 * @param {string[]} argv
 * @param {{run?: typeof spawnSync, write?: (text: string) => void}} deps
 */
export function main(argv, deps = {}) {
  const run = deps.run ?? spawnSync
  const write = deps.write ?? ((text) => process.stdout.write(text))
  let json = false
  let result
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: { pr: { type: 'string' }, json: { type: 'boolean', default: false } },
    })
    json = values.json
    const [command] = positionals
    if (command !== 'decide' || positionals.length !== 1 || !values.pr)
      throw new Error('usage: draft-guard.mjs decide --pr <PR URL> [--json]')
    const url = new URL(values.pr)
    const match = /^\/([^/]+)\/([^/]+)\/pull\/([1-9]\d*)\/?$/.exec(url.pathname)
    if (
      url.protocol !== 'https:' ||
      url.username ||
      url.password ||
      url.port ||
      url.search ||
      url.hash ||
      !match ||
      !Number.isSafeInteger(Number(match[3]))
    )
      throw new Error('invalid PR URL')
    const [, owner, name, number] = match
    const args = [
      'api',
      'graphql',
      '-f',
      `query=${QUERY}`,
      '-f',
      `owner=${owner}`,
      '-f',
      `name=${name}`,
      '-F',
      `number=${number}`,
    ]
    if (url.hostname !== 'github.com') args.push('--hostname', url.hostname)
    const response = run('gh', args, { encoding: 'utf8', timeout: 30_000, maxBuffer: 1024 * 1024 })
    if (response.error || response.signal || response.status !== 0) {
      result = unknown(
        `gh read failed: ${response.error?.message || response.signal || String(response.stderr || '').trim() || `exit ${response.status}`}`,
      )
    } else {
      result = classifyReadyIntent(JSON.parse(String(response.stdout)))
    }
  } catch (error) {
    result = unknown(error instanceof Error ? error.message : String(error))
  }
  write(
    json ? `${JSON.stringify(result)}\n` : `verdict: ${result.verdict}\nreason: ${result.reason}\n`,
  )
  return result.verdict === 'unknown' ? 2 : result.verdict === 'not-open' ? 1 : 0
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
