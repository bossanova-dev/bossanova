import { spawnSync } from 'node:child_process'
import { realpathSync, statSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { MAX_ITEMS, MAX_TEXT_CHARS, MAX_DESCRIBE_LINKS, isProofUrl } from './verify-proof.mjs'

export { MAX_DESCRIBE_LINKS } from './verify-proof.mjs'
export const DESCRIBE_TIMEOUT_MS = 60_000
export const MAX_DESCRIBE_OUTPUT_BYTES = 64 * 1024
const keys = new Set([
  'ok',
  'extension',
  'role',
  'items',
  'links',
  'breaking',
  'undisclosed',
  'error',
])
const singleLine = (value) =>
  typeof value === 'string' &&
  value.trim().length > 0 &&
  value.length <= MAX_TEXT_CHARS &&
  !/[\r\n\u2028\u2029]/.test(value)

export function refineDescription(result) {
  const errors = []
  if (!result || typeof result !== 'object' || Array.isArray(result))
    return ['expected description object']
  for (const key of Object.keys(result)) if (!keys.has(key)) errors.push(`unknown key: ${key}`)
  if (result.ok !== true) errors.push('ok must be true')
  if (!singleLine(result.extension)) errors.push('extension must be non-empty text')
  if (result.role !== 'proof-describe') errors.push('role must be proof-describe')
  for (const key of ['items', 'undisclosed']) {
    if (!Array.isArray(result[key]) || result[key].length > MAX_ITEMS)
      errors.push(`${key} must be an array of at most ${MAX_ITEMS} entries`)
    if (Array.isArray(result[key]))
      for (const item of result[key])
        if (!singleLine(item))
          errors.push(
            `${key} entries must be single-line text of at most ${MAX_TEXT_CHARS} characters`,
          )
  }
  if (typeof result.breaking !== 'boolean') errors.push('breaking must be boolean')
  if (result.links !== undefined) {
    if (!Array.isArray(result.links) || result.links.length > MAX_DESCRIBE_LINKS)
      errors.push(`links must be an array of at most ${MAX_DESCRIBE_LINKS} entries`)
    if (Array.isArray(result.links))
      for (const link of result.links) {
        if (
          !link ||
          typeof link !== 'object' ||
          Array.isArray(link) ||
          Object.keys(link).some((key) => !['text', 'url'].includes(key)) ||
          !singleLine(link.text) ||
          !isProofUrl(link.url)
        )
          errors.push('links require text and an absolute HTTPS URL without userinfo')
      }
  }
  return errors
}

/** Parse extension-relative paths and the scalar area list; invalid markers are visible skips. */
export function parseDescribeMarker(marker) {
  if (typeof marker.describe !== 'string' || typeof marker.areas !== 'string') return null
  const raw = marker.describe.trim().replace(/\\/g, '/')
  if (!raw || raw.startsWith('/') || /^[A-Za-z]:/.test(raw)) return null
  const segments = raw.split('/').filter((segment) => segment && segment !== '.')
  const areas = marker.areas.split(',').map((area) => area.trim())
  if (
    !segments.length ||
    segments.includes('..') ||
    areas.some((area) => !/^[a-z0-9][a-z0-9:-]*$/.test(area))
  )
    return null
  return { describe: segments.join('/'), describeAreas: [...new Set(areas)] }
}

function scriptPath(extension) {
  try {
    const script = realpathSync(path.resolve(extension.dir, extension.describe))
    const relative = path.relative(realpathSync(extension.dir), script)
    if (relative.startsWith('..') || path.isAbsolute(relative) || !statSync(script).isFile())
      return null
    return script
  } catch {
    return null
  }
}

/** Run every matching pair serially; failures become facts, never exceptions. `env` is the
 * describer process environment (the caller's own when omitted). */
export function runDescribers({
  extensions,
  areas,
  baseSha,
  headSha,
  pr,
  cwd,
  env,
  run = spawnSync,
  timeoutMs = DESCRIBE_TIMEOUT_MS,
}) {
  const outcomes = []
  for (const [index, extension] of extensions.entries())
    for (const area of areas) {
      if (!extension.describeAreas?.includes(area.area)) continue
      const identity = {
        extension: extension.name,
        area: area.area,
        order: extension.order ?? 100,
        index,
      }
      const fail = (reason) => outcomes.push({ ...identity, ok: false, reason })
      const script = scriptPath(extension)
      if (!script) {
        fail('script-missing')
        continue
      }
      try {
        const envelope = {
          role: 'proof-describe',
          extension: extension.name,
          area: { area: area.area, label: area.label, paths: area.paths },
          baseSha,
          headSha,
          pr: { number: pr?.number, title: pr?.title ?? '', body: pr?.body ?? '' },
        }
        const result = run(process.execPath, [script], {
          input: JSON.stringify(envelope),
          cwd,
          env,
          timeout: timeoutMs,
          maxBuffer: MAX_DESCRIBE_OUTPUT_BYTES,
          encoding: 'utf8',
        })
        const errorCode = result.error && 'code' in result.error ? result.error.code : undefined
        if (errorCode === 'ETIMEDOUT') {
          fail('timeout')
          continue
        }
        if (
          errorCode === 'ENOBUFS' ||
          Buffer.byteLength(result.stdout ?? '') > MAX_DESCRIBE_OUTPUT_BYTES
        ) {
          fail('oversize')
          continue
        }
        if (result.signal) {
          fail('signal')
          continue
        }
        if (result.status !== 0) {
          fail(`exit:${result.status ?? 1}`)
          continue
        }
        let description
        try {
          description = JSON.parse(String(result.stdout))
        } catch {
          fail('not-json')
          continue
        }
        const errors = refineDescription(description)
        if (description?.extension !== extension.name) errors.push('extension mismatch')
        if (errors.length) {
          fail(`invalid:${errors[0]}`)
          continue
        }
        outcomes.push({ ...identity, ok: true, description })
      } catch (error) {
        fail(
          error?.code === 'ETIMEDOUT'
            ? 'timeout'
            : error?.code === 'ENOBUFS'
              ? 'oversize'
              : 'exit:1',
        )
      }
    }
  return outcomes
}

/** Merge in descriptor order without mutating inputs or relaxing their path-based policy. */
export function describeAreas(areas, outcomes) {
  const health = [],
    collectors = [],
    undisclosed = {}
  const ordered = [...outcomes].sort(
    (a, b) =>
      (a.order ?? 100) - (b.order ?? 100) ||
      (a.index ?? 0) - (b.index ?? 0) ||
      a.extension.localeCompare(b.extension) ||
      a.area.localeCompare(b.area),
  )
  const described = areas.map((area) => {
    const items = [],
      links = [],
      hidden = []
    let breaking = false
    for (const outcome of ordered.filter((entry) => entry.area === area.area)) {
      collectors.push({ name: `describe:${outcome.extension}:${area.area}`, ok: outcome.ok })
      if (!outcome.ok) {
        health.push({
          code: 'describer-failed',
          text: `${outcome.extension}: ${area.area} (${outcome.reason})`.slice(0, MAX_TEXT_CHARS),
        })
        continue
      }
      items.push(...outcome.description.items)
      links.push(...(outcome.description.links ?? []))
      hidden.push(...outcome.description.undisclosed)
      breaking ||= outcome.description.breaking
    }
    const uniqueItems = [...new Set(items)],
      uniqueLinks = [
        ...new Map(links.map((link) => [JSON.stringify([link.text, link.url]), link])).values(),
      ],
      uniqueHidden = [...new Set(hidden)]
    if (uniqueItems.length > MAX_ITEMS || uniqueLinks.length > MAX_DESCRIBE_LINKS)
      health.push({
        code: 'describer-items-truncated',
        text: `${area.area}: description lists truncated`,
      })
    undisclosed[area.area] = uniqueHidden
    return {
      ...area,
      defaultAsk: breaking ? 'always' : area.defaultAsk,
      items: uniqueItems.slice(0, MAX_ITEMS),
      links: uniqueLinks.slice(0, MAX_DESCRIBE_LINKS),
      breaking,
      undisclosed: uniqueHidden,
    }
  })
  return { areas: described, health, collectors, undisclosed }
}

/** One line of at most MAX_TEXT_CHARS characters, or '' when nothing is left. */
function oneLine(text) {
  const line = String(text)
    .replace(/[\r\n\u2028\u2029]+/g, ' ')
    .trim()
  return line.length > MAX_TEXT_CHARS ? `${line.slice(0, MAX_TEXT_CHARS - 1)}…` : line
}
const boundList = (list = []) => [...new Set(list.map(oneLine).filter(Boolean))].slice(0, MAX_ITEMS)

/**
 * A describer's raw facts as a valid, bounded proof-describe result.
 * @param {string} extension
 * @param {{items?: string[], breaking?: boolean, undisclosed?: string[]}} [facts]
 */
export function boundDescription(extension, { items, breaking = false, undisclosed } = {}) {
  return {
    ok: true,
    extension,
    role: 'proof-describe',
    items: boundList(items),
    breaking: breaking === true,
    undisclosed: boundList(undisclosed),
  }
}

/**
 * The shared entry point of a describer script: no flags, the envelope on stdin, one bounded
 * result on stdout. `describe(envelope, {cwd})` returns `{items, breaking, undisclosed?}`; a throw
 * exits non-zero, which the core records as a failed describer.
 * @param {(envelope: any, options: {cwd: string}) => any} describe
 * @param {{argv?: string[], stdin?: AsyncIterable<string | Buffer>,
 *   stdout?: {write: (text: string) => unknown}, cwd?: string}} [io]
 */
export async function describerMain(
  describe,
  { argv = process.argv.slice(2), stdin = process.stdin, stdout = process.stdout, cwd } = {},
) {
  parseArgs({ args: argv, options: {}, strict: true, allowPositionals: false })
  let input = ''
  for await (const chunk of stdin) input += chunk
  const envelope = JSON.parse(input)
  const facts = await describe(envelope, { cwd: cwd ?? process.cwd() })
  stdout.write(JSON.stringify(boundDescription(envelope.extension, facts)))
}
