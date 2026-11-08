import crypto from 'node:crypto'
import { renameSync, rmSync, statSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { setTimeout as sleepTimer } from 'node:timers/promises'
import { parseClaimComments } from '../linear-claim.mjs'
import { settleRead } from './settle-read.mjs'

export const SETTLE_VERBS_USAGE = `
  read-claim-comments --id <issueId> --out-file <path> [--until-claim <token>] [--until-absent <commentId>]... [--settle-ms <n>] [--scope build|plan]
      Write the last comment array atomically; receipt reports settled, attempts, elapsedMs and ownClaimId.
  await-state --id <issueId> --state <name> [--settle-ms <n>]
      Read until the stored state matches; unsettled is a receipt, not a failure.
`

function parseFlags(rest, comments) {
  const names = comments
    ? ['id', 'out-file', 'until-claim', 'until-absent', 'settle-ms', 'scope']
    : ['id', 'state', 'settle-ms']
  const { values: raw, tokens } = parseArgs({
    args: rest,
    strict: true,
    tokens: true,
    options: Object.fromEntries(
      names.map((name) => [name, { type: 'string', multiple: name === 'until-absent' }]),
    ),
  })
  const values =
    /** @type {{id?: string, 'out-file'?: string, 'until-claim'?: string, 'until-absent'?: string[], 'settle-ms'?: string, scope?: string, state?: string}} */ (
      raw
    )
  const seen = new Set()
  for (const t of tokens) {
    if (t.kind !== 'option') continue
    if (seen.has(t.name) && t.name !== 'until-absent') throw new Error(`repeated --${t.name}`)
    seen.add(t.name)
    if (!t.value?.trim() || /^--/.test(t.value)) throw new Error(`--${t.name} requires a value`)
  }
  for (const name of comments ? ['id', 'out-file'] : ['id', 'state']) {
    if (!values[name]) throw new Error(`--${name} is required`)
  }
  if (values.scope && !['build', 'plan'].includes(values.scope)) throw new Error('invalid --scope')
  if (
    values['settle-ms'] !== undefined &&
    (!/^\d+$/.test(values['settle-ms']) ||
      Number(values['settle-ms']) < 1 ||
      Number(values['settle-ms']) > 300000)
  )
    throw new Error('--settle-ms must be a positive integer at most 300000')
  if (comments && values['settle-ms'] && !values['until-claim'] && !values['until-absent'])
    throw new Error('--settle-ms requires --until-claim or --until-absent')
  if (comments && !statSync(path.dirname(path.resolve(values['out-file']))).isDirectory())
    throw new Error('missing out-file parent directory')
  return values
}

function atomicComments(file, comments) {
  const temp = `${path.resolve(file)}.${process.pid}.${crypto.randomBytes(6).toString('hex')}.tmp`
  try {
    writeFileSync(temp, JSON.stringify(comments), { flag: 'wx' })
    renameSync(temp, file)
  } finally {
    rmSync(temp, { force: true })
  }
}

async function run(rest, io, comments) {
  const { write, errWrite, env, resolveAdapter, sleep = sleepTimer, now = Date.now } = io
  const verb = comments ? 'read-claim-comments' : 'await-state'
  const capability = comments ? 'readClaimComments' : 'readIssueState'
  const fallback = comments
    ? 'fall back to one readComments operation'
    : 'fall back to one getIssue operation'
  const fail = (error, code) => {
    errWrite(`${verb}: ${String(error?.message ?? error).replace(/[\r\n]+/g, ' ')}; ${fallback}\n`)
    return code
  }
  let flags
  try {
    flags = parseFlags(rest, comments)
  } catch (error) {
    return fail(error, 64)
  }
  try {
    const adapter = resolveAdapter({ env })
    if (typeof adapter?.[capability] !== 'function')
      throw new Error(`adapter has no ${capability} capability`)
    const settling = !comments || !!(flags['until-claim'] || flags['until-absent'])
    const ownClaim = (value) =>
      flags['until-claim']
        ? parseClaimComments(value.comments, { scope: flags.scope ?? 'build' }).find(
            (claim) => claim.token === flags['until-claim'],
          )
        : null
    const result = await settleRead({
      read: async () => {
        const value = await adapter[capability](flags.id)
        if (
          !value?.id ||
          !value?.identifier ||
          (comments ? !Array.isArray(value.comments) : typeof value.state?.name !== 'string')
        )
          throw new Error(`unreadable ${capability} result`)
        return value
      },
      isSettled: (value) =>
        !settling ||
        (comments
          ? (!flags['until-claim'] || !!ownClaim(value)) &&
            (flags['until-absent'] ?? []).every((id) => !value.comments.some((c) => c.id === id))
          : value.state.name === flags.state),
      budgetMs: Number(flags['settle-ms'] ?? (comments ? 30000 : 90000)),
      intervalMs: comments ? 3000 : 5000,
      sleep,
      now,
    })
    const { value, settled, attempts, elapsedMs } = result
    if (comments) atomicComments(flags['out-file'], value.comments)
    write(
      JSON.stringify({
        ...(comments
          ? { count: value.comments.length, ownClaimId: ownClaim(value)?.commentId ?? null }
          : { state: value.state.name }),
        settled,
        attempts,
        elapsedMs,
        id: value.id,
        identifier: value.identifier,
        outcome: comments ? 'comments-read' : 'state-read',
      }) + '\n',
    )
    return 0
  } catch (error) {
    return fail(error, 2)
  }
}

export const runReadClaimComments = (rest, io) => run(rest, io, true)
export const runAwaitState = (rest, io) => run(rest, io, false)
