import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import { basename, dirname, relative, isAbsolute } from 'node:path'
import { parseArgs } from 'node:util'
import { generateClaimToken } from '../tracker/cli.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { loadClaimComments, loadClaimLiveness } from '../linear-claim.mjs'
import { runIssueLease } from '../issue-lease.mjs'
import { isMainModule } from '../main-module.mjs'
import { planScratchToken } from './plan-scratch-paths.mjs'

/** @typedef {{version: number, scope: string, issueId: string, token: string, commentId: string|null, released: boolean}} ClaimReceipt */

/** Validate a concrete receipt path before any filesystem access. */
export function assertClaimReceiptPath(path) {
  const candidate = isAbsolute(path) ? relative(process.cwd(), path) : path
  // Absolute paths are useful to callers outside their scratch checkout; classify the
  // declared suffix, while still rejecting nested or traversing artifact paths.
  const marker = '.linear-plans/'
  const index = candidate.indexOf(marker)
  const token = isAbsolute(path) && index >= 0 ? candidate.slice(index) : candidate
  const resolved = planScratchToken(token)
  if (
    !resolved.ok ||
    resolved.families.length !== 1 ||
    resolved.families[0] !== 'claim-receipt' ||
    /[<>]/.test(path)
  )
    throw new Error('expected a declared run-scoped claim-receipt path')
  return basename(path).slice(0, -'.claim-receipt.json'.length)
}

/** Read the shared planning ownership record. @returns {ClaimReceipt} */
export function readClaimReceipt(path) {
  const issueId = assertClaimReceiptPath(path)
  const receipt = JSON.parse(readFileSync(path, 'utf8'))
  if (
    receipt.version !== 1 ||
    receipt.scope !== 'plan' ||
    receipt.issueId !== issueId ||
    !/^[a-f0-9]{32}$/.test(receipt.token) ||
    typeof receipt.released !== 'boolean' ||
    !(
      receipt.commentId === null ||
      (typeof receipt.commentId === 'string' && /^\S+$/.test(receipt.commentId))
    )
  )
    throw new Error('invalid planning claim receipt')
  return receipt
}

function save(path, receipt) {
  mkdirSync(dirname(path), { recursive: true })
  const temporary = `${path}.${process.pid}.tmp`
  writeFileSync(temporary, `${JSON.stringify(receipt)}\n`, { mode: 0o600 })
  renameSync(temporary, path)
}

/** Execute a receipt-based planning claim verb with injectable transport and output.
 * @param {string[]} argv
 * @param {Parameters<typeof runIssueLease>[1] & {resolveAdapter?: typeof resolveTrackerAdapter, write?: (text: string) => unknown, errWrite?: (text: string) => unknown}} [deps]
 */
export function main(argv, deps = {}) {
  const write = deps.write ?? ((text) => process.stdout.write(text))
  const errWrite = deps.errWrite ?? ((text) => process.stderr.write(text))
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        receipt: { type: 'string' },
        issue: { type: 'string' },
        id: { type: 'string' },
        comments: { type: 'string' },
        liveness: { type: 'string' },
        'session-id': { type: 'string' },
        'ttl-seconds': { type: 'string' },
      },
    })
    const command = positionals[0]
    if (
      positionals.length !== 1 ||
      !['start', 'acquire', 'comment', 'record-comment', 'verdict', 'release'].includes(command) ||
      !values.receipt
    )
      throw new Error(
        'expected start|acquire|comment|record-comment|verdict|release --receipt <path>',
      )
    const path = values.receipt
    const issueId = assertClaimReceiptPath(path)
    const emit = (value) => write(`${typeof value === 'string' ? value : JSON.stringify(value)}\n`)
    if (command === 'start') {
      if (!values.issue || values.issue !== issueId)
        throw new Error('receipt issue id must equal --issue')
      if (existsSync(path) && !readClaimReceipt(path).released)
        throw new Error('release the held claim first')
      save(path, {
        version: 1,
        scope: 'plan',
        issueId,
        token: generateClaimToken(),
        commentId: null,
        released: false,
      })
      return 0
    }
    if (command === 'release' && !existsSync(path)) {
      emit({ lease: 'none', commentId: null })
      return 0
    }
    const receipt = readClaimReceipt(path)
    if (command === 'acquire' || command === 'release') {
      const ttlSeconds = Number(values['ttl-seconds'] ?? 7200)
      if (
        command === 'acquire' &&
        (!Number.isInteger(ttlSeconds) || ttlSeconds <= 0 || ttlSeconds > 2147483647)
      )
        throw new Error('invalid --ttl-seconds')
      const outcome = runIssueLease(
        { command, issue: receipt.issueId, scope: 'plan', token: receipt.token, ttlSeconds },
        deps,
      )
      if (command === 'release') {
        receipt.released = true
        try {
          save(path, receipt)
        } catch (error) {
          errWrite(`warning: release receipt update failed: ${error.message}\n`)
        }
        emit({
          lease: outcome.result ?? { verdict: 'unavailable', reason: outcome.error },
          commentId: receipt.commentId,
        })
        return 0
      }
      if (outcome.result) emit(outcome.result)
      if (outcome.error) errWrite(`${outcome.error}\n`)
      return outcome.status
    }
    if (command === 'record-comment') {
      if (!values.id || !/^\S+$/.test(values.id))
        throw new Error('expected --id <commentId> without whitespace')
      if (receipt.commentId !== null && receipt.commentId !== values.id)
        throw new Error('receipt already records a different comment id')
      receipt.commentId = values.id
      save(path, receipt)
      return 0
    }
    const adapter = (deps.resolveAdapter ?? resolveTrackerAdapter)(process.env)
    if (!adapter.claimScopes?.includes('plan'))
      throw new Error('tracker adapter missing claimScopes plan capability')
    if (command === 'comment') {
      emit(
        adapter.formatClaimComment(
          receipt.token,
          values['session-id'] ?? process.env.BOSS_SESSION_ID ?? null,
          { scope: 'plan' },
        ),
      )
      return 0
    }
    if (command === 'verdict') {
      if (!values.comments) throw new Error('verdict requires --comments <file|json>')
      const verdict = adapter.resolveClaim(loadClaimComments(values.comments), receipt.token, {
        ...(values.liveness === undefined ? {} : loadClaimLiveness(values.liveness)),
        scope: 'plan',
      })
      emit(verdict === null ? 'NO_WINNER' : verdict ? 'WON' : 'LOST')
      return verdict === null ? 4 : verdict ? 0 : 3
    }
  } catch (error) {
    errWrite(`plan-claim: ${error.message}\n`)
    return 2
  }
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
