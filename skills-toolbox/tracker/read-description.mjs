import crypto from 'node:crypto'
import { existsSync, readFileSync, renameSync, rmSync, statSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { setTimeout as sleepTimer } from 'node:timers/promises'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { settleRead } from './settle-read.mjs'

const EX_USAGE = 64
const oneLine = (message) => String(message).replace(/[\r\n]+/g, ' ')
const READ_DESCRIPTION_FLAGS = ['id', 'out-file', 'expect-file', 'stale-file', 'settle-ms']
const READ_DESCRIPTION_FALLBACK =
  "fall back to the tracker adapter's getIssue operation (MCP) byte-copy"

export function parseReadDescriptionFlags(rest) {
  try {
    const { values: flags, tokens } = parseArgs({
      args: rest,
      strict: true,
      tokens: true,
      options: Object.fromEntries(READ_DESCRIPTION_FLAGS.map((name) => [name, { type: 'string' }])),
    })
    const seen = new Set()
    for (const token of tokens) {
      if (token.kind !== 'option') continue
      if (seen.has(token.name)) throw new Error(`--${token.name} given more than once`)
      seen.add(token.name)
      if (!token.value?.trim() || /^--/.test(token.value))
        throw new Error(`--${token.name} requires a non-empty value`)
    }
    for (const name of ['id', 'out-file']) {
      if (!flags[name]) throw new Error(`--${name} is required`)
    }
    if (
      flags['settle-ms'] !== undefined &&
      (!/^\d+$/.test(flags['settle-ms']) ||
        Number(flags['settle-ms']) < 1 ||
        Number(flags['settle-ms']) > 300000)
    ) {
      throw new Error('--settle-ms must be a positive integer at most 300000')
    }
    if (flags['settle-ms'] && !flags['expect-file'] && !flags['stale-file'])
      throw new Error('--settle-ms requires --expect-file or --stale-file')
    return { flags }
  } catch (error) {
    return { error: error.message }
  }
}

export async function runReadDescription(
  rest,
  { write, errWrite, env, resolveAdapter, sleep = sleepTimer, now = Date.now },
) {
  const fail = (message, code = 2) => {
    errWrite(`read-description: ${oneLine(message)}\n`)
    return code
  }
  const parsed = parseReadDescriptionFlags(rest)
  if (parsed.error) {
    return fail(
      `usage: ${parsed.error}; expected read-description --id <issueId> --out-file <path>`,
      EX_USAGE,
    )
  }
  const { id, 'out-file': outFile } = parsed.flags
  let expected
  let stale
  try {
    if (parsed.flags['expect-file']) expected = readFileSync(parsed.flags['expect-file'])
    if (parsed.flags['stale-file']) stale = readFileSync(parsed.flags['stale-file'])
  } catch (error) {
    return fail(`usage: cannot read settle input: ${error.message}`, EX_USAGE)
  }
  const settling = expected !== undefined || stale !== undefined
  let adapter
  try {
    adapter = resolveAdapter({ env })
  } catch (err) {
    return fail(`could not resolve the tracker adapter: ${err?.message ?? err}`)
  }
  if (typeof adapter?.readDescription !== 'function') {
    return fail(
      `resolved tracker adapter has no readDescription capability; ${READ_DESCRIPTION_FALLBACK}`,
    )
  }
  // Checked BEFORE the read, so a write that cannot land never spends a tracker request.
  const parent = path.dirname(path.resolve(outFile))
  if (!existsSync(parent) || !statSync(parent).isDirectory()) {
    return fail(`--out-file parent directory ${parent} does not exist`)
  }
  let result
  let receipt
  try {
    if (settling) {
      const polled = await settleRead({
        read: async () => {
          const value = await adapter.readDescription(id)
          if (
            !value ||
            typeof value.id !== 'string' ||
            typeof value.identifier !== 'string' ||
            typeof value.description !== 'string' ||
            !value.description.isWellFormed()
          ) {
            throw new Error(
              'readDescription returned an unreadable result (need well-formed string id, identifier, description)',
            )
          }
          return value
        },
        isSettled: (value) => {
          const bytes = Buffer.from(value.description, 'utf8')
          return (
            (expected !== undefined && bytes.equals(expected)) ||
            (stale !== undefined && !bytes.equals(stale))
          )
        },
        budgetMs: Number(parsed.flags['settle-ms'] ?? 120000),
        intervalMs: 5000,
        sleep,
        now,
      })
      result = polled.value
      receipt = { settled: polled.settled, attempts: polled.attempts, elapsedMs: polled.elapsedMs }
    } else {
      result = await adapter.readDescription(id)
    }
  } catch (err) {
    const message = String(err?.message ?? err)
    if (err?.code === TRACKER_CREDENTIALS_MISSING) {
      return fail(
        `tracker credential missing (${message}), so the stored description cannot be read directly; ${READ_DESCRIPTION_FALLBACK}`,
      )
    }
    return fail(`readDescription failed: ${message}`)
  }
  if (
    !result ||
    typeof result !== 'object' ||
    typeof result.id !== 'string' ||
    typeof result.identifier !== 'string' ||
    typeof result.description !== 'string'
  ) {
    return fail(
      'readDescription returned an unreadable result (need string id, identifier, description)',
    )
  }
  const { description } = result
  // A lone surrogate would be written as U+FFFD, so the file would silently differ from the stored
  // description it attests to. Refuse rather than write a lossy copy.
  if (!description.isWellFormed()) {
    return fail(
      'the stored description is not well-formed UTF-16, so it cannot be written verbatim',
    )
  }
  const target = path.resolve(outFile)
  let bytes
  try {
    bytes = writeFileAtomically(target, description)
  } catch (err) {
    return fail(`could not write --out-file ${outFile}: ${err?.message ?? err}`)
  }
  write(
    JSON.stringify({
      bytes,
      outcome: description === '' ? 'stored-description-empty' : 'stored-description-written',
      id: result.id,
      identifier: result.identifier,
      ...receipt,
    }) + '\n',
  )
  return 0
}

// A sibling rename exposes either the old complete file or the new complete file.
export function writeFileAtomically(target, text) {
  const temp = path.join(
    path.dirname(target),
    `.${path.basename(target)}.${process.pid}.${crypto.randomBytes(6).toString('hex')}.tmp`,
  )
  try {
    writeFileSync(temp, text, { encoding: 'utf8', flag: 'wx' })
    renameSync(temp, target)
    return statSync(target).size
  } finally {
    rmSync(temp, { force: true })
  }
}
