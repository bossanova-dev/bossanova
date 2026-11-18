import crypto from 'node:crypto'
import { readFileSync, renameSync, rmSync, statSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'

export const PREMISE_STATES_USAGE = `  premise-states --premises <file> --out-file <file>
      Write a fresh bare id-to-state map atomically; exits 2 with getIssue fallback, 64 on usage.
`
const fallback =
  'fall back to one fresh getIssue (MCP) read per premise id and write the bare {"<ID>": "<state name>"} map'
const nonEmpty = (value) => typeof value === 'string' && value.trim().length > 0
const plainObject = (value) =>
  value !== null &&
  typeof value === 'object' &&
  (Object.getPrototypeOf(value) === Object.prototype || Object.getPrototypeOf(value) === null)

/** @param {unknown} value @returns {{id: string, state: string}[]} */
export function parsePremises(value) {
  if (!Array.isArray(value)) throw new Error('premises must be an array')
  for (const [index, entry] of value.entries()) {
    if (!plainObject(entry) || !nonEmpty(entry.id) || !nonEmpty(entry.state))
      throw new Error(`premises[${index}] must be an object with non-empty string id and state`)
  }
  return value
}

/** @param {unknown} value @returns {Record<string, string>} */
export function parsePremiseStates(value) {
  if (!plainObject(value)) throw new Error('premise states must be a bare object map')
  for (const [key, state] of Object.entries(value)) {
    if (!nonEmpty(key) || !nonEmpty(state))
      throw new Error(
        `premise state ${JSON.stringify(key)} must be a non-empty string, got ${state === null ? 'null' : typeof state}`,
      )
  }
  return /** @type {Record<string, string>} */ (value)
}

export async function runPremiseStates(rest, { write, errWrite, env, resolveAdapter }) {
  const fail = (message, code) => {
    errWrite(`premise-states: ${String(message).replace(/[\r\n]+/g, ' ')}\n`)
    return code
  }
  let flags
  let premises
  try {
    const parsed = parseArgs({
      args: rest,
      strict: true,
      tokens: true,
      options: { premises: { type: 'string' }, 'out-file': { type: 'string' } },
    })
    const seen = new Set()
    for (const token of parsed.tokens) {
      if (token.kind !== 'option') continue
      if (seen.has(token.name) || !nonEmpty(token.value) || /^--/.test(token.value))
        throw new Error(`invalid or repeated --${token.name}`)
      seen.add(token.name)
    }
    flags = parsed.values
    if (!flags.premises || !flags['out-file'])
      throw new Error('--premises and --out-file are required')
    premises = parsePremises(JSON.parse(readFileSync(flags.premises, 'utf8')))
  } catch (error) {
    return fail(`usage: ${error.message}`, 64)
  }
  const target = path.resolve(flags['out-file'])
  let temporary
  try {
    const states = Object.create(null)
    const aliases = Object.create(null)
    const ids = [...new Set(premises.map(({ id }) => id))]
    if (ids.length) {
      const adapter = resolveAdapter({ env })
      if (typeof adapter?.readIssueState !== 'function')
        throw new Error('adapter has no readIssueState capability')
      for (const id of ids) {
        const result = await adapter.readIssueState(id)
        if (!nonEmpty(result?.state?.name)) throw new Error(`unreadable state for ${id}`)
        states[id] = result.state.name
        if (
          /^[A-Za-z][A-Za-z0-9]*-\d+$/.test(id) &&
          nonEmpty(result.identifier) &&
          result.identifier !== id
        )
          aliases[id] = result.identifier
      }
    }
    temporary = path.join(
      path.dirname(target),
      `.${path.basename(target)}.${crypto.randomBytes(8).toString('hex')}.tmp`,
    )
    writeFileSync(temporary, JSON.stringify(states), { encoding: 'utf8', flag: 'wx' })
    renameSync(temporary, target)
    const receipt = {
      outcome: 'premise-states-written',
      count: ids.length,
      bytes: statSync(target).size,
      out: flags['out-file'],
    }
    write(
      JSON.stringify({ ...receipt, ...(Object.keys(aliases).length ? { aliases } : {}) }) + '\n',
    )
    return 0
  } catch (error) {
    const cleanupErrors = []
    for (const file of [temporary, target].filter(Boolean)) {
      try {
        rmSync(file, { force: true })
      } catch (cleanupError) {
        cleanupErrors.push(cleanupError.message)
      }
    }
    const message =
      error.code === TRACKER_CREDENTIALS_MISSING ? 'tracker credentials missing' : error.message
    return fail(
      `${message}${cleanupErrors.length ? `; cleanup failed: ${cleanupErrors.join('; ')}` : ''}; ${fallback}`,
      2,
    )
  }
}
