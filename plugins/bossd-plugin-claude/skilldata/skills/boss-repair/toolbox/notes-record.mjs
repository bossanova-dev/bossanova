#!/usr/bin/env node
// Buffer unexpected outcomes under the worktree git dir; one terminal flush records at most
// three per-run-idempotent improvement notes. Neither verb can change the caller's outcome.
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import defaultFs from 'node:fs'
import path from 'node:path'
import { isSecretBearing, formatRunLine, resolveRunId, sanitizeRunId } from './bs-record-notes.mjs'
import { resolveBossBinary } from './boss-binary.mjs'
import { isMainModule } from './main-module.mjs'

export const MAX_TRIGGER_NOTES = 3
export const MAX_CATEGORY_NOTES = 3
const STALE_MS = 7 * 24 * 60 * 60 * 1000
export const TRIGGERS = Object.freeze(
  Object.fromEntries(
    Object.entries({
      'review-must-fix': {
        statement: 'review raised a must-fix finding in this category',
        why: 'the same defect class keeps reaching review instead of being prevented earlier',
        fix: 'unknown',
      },
      'evidence-unknown': {
        statement: 'Required evidence could not be established.',
        why: 'Unknown evidence prevents an honest verification claim.',
        fix: 'Make the evidence source readable and classify its result explicitly.',
      },
      'extension-failed': {
        statement: 'A configured extension failed unexpectedly.',
        why: 'The run loses the capability the extension was meant to provide.',
        fix: 'Repair the extension or its result contract.',
      },
      'repair-exhausted': {
        statement: 'Repair stopped without resolving the failure.',
        why: 'Repeated attempts spent the repair budget without restoring a green gate.',
        fix: 'Investigate the persistent failure before another repair attempt.',
      },
      'human-push-on-agent-pr': {
        statement: 'Another contributor moved the agent PR head.',
        why: 'Continuing with stale ownership could overwrite another contributor’s work.',
        fix: 'Reconcile the remote head and re-establish ownership before pushing.',
      },
      'stale-claim-taken-over': {
        statement: 'A stale worktree claim had to be taken over.',
        why: 'A previous run left ownership behind without completing cleanup.',
        fix: 'Check why the previous run failed to release its claim.',
      },
      'verify-failed-after-clean-review': {
        statement: 'Verification failed after a clean review.',
        why: 'The review did not detect a failure found by the verification gate.',
        fix: 'Add the missed verification evidence to the review path.',
      },
      'dispatch-failed': {
        statement: 'A required agent dispatch failed.',
        why: 'The dispatched work did not produce a usable result.',
        fix: 'Inspect the dispatch transport and its completion evidence.',
      },
      'gate-unknown': {
        statement: 'A required gate ended with an unknown result.',
        why: 'An unknown gate cannot establish that the branch passed verification.',
        fix: 'Restore gate visibility and obtain a terminal verdict.',
      },
    }).map(([name, value]) => [name, Object.freeze(value)]),
  ),
)

const singleLine = (value) =>
  String(value ?? '')
    .replace(/[\r\n]+/g, ' ')
    .slice(0, 200)
const failureText = (error) =>
  isSecretBearing(String(error?.message ?? error))
    ? 'operation failed (secret-bearing diagnostic omitted)'
    : String(error?.message ?? error)
const currentTime = (deps) => new Date((deps.now ?? (() => new Date()))())

function defaultGitDir() {
  const result = spawnSync('git', ['rev-parse', '--absolute-git-dir'], {
    encoding: 'utf8',
    timeout: 10000,
  })
  if (result.error) throw result.error
  if (result.status !== 0 || !result.stdout.trim())
    throw new Error('cannot resolve worktree git directory')
  return result.stdout.trim()
}

function bufferPath(options, deps) {
  if (options.buffer) return options.buffer
  const dir = typeof deps.gitDir === 'function' ? deps.gitDir() : (deps.gitDir ?? defaultGitDir())
  if (typeof dir !== 'string' || dir === '')
    throw new Error('cannot resolve worktree git directory')
  return path.join(dir, 'boss-notes-record.jsonl')
}

function addBatch(options, deps) {
  try {
    if (Object.hasOwn(options, 'where'))
      throw new Error('--from and --where are mutually exclusive')
    if (!Object.hasOwn(TRIGGERS, options.trigger))
      throw new Error(`unknown trigger: ${singleLine(options.trigger)}`)
    if (typeof options.core !== 'string' || !options.core) throw new Error('add requires --core')
    const entries = JSON.parse((deps.fs ?? defaultFs).readFileSync(options.from, 'utf8'))
    if (!Array.isArray(entries)) throw new Error('--from requires a JSON array')
    const result = { ok: true, buffered: 0, skipped: [], failures: [] }
    for (const [index, entry] of entries.entries()) {
      const where = typeof entry?.where === 'string' ? singleLine(entry.where).trim() : ''
      if (!where) {
        result.skipped.push({ index, reason: 'blank where' })
        continue
      }
      const added = addTrigger({ ...options, from: undefined, where, detail: entry.detail }, deps)
      if (added.ok) result.buffered += 1
      else {
        result.ok = false
        result.failures.push(...added.failures)
      }
    }
    return result
  } catch (error) {
    const message = failureText(error)
    return { ok: false, error: message, failures: [message] }
  }
}

export function addTrigger(options, deps = {}) {
  if (options.from !== undefined) return addBatch(options, deps)
  const failures = []
  try {
    if (!Object.hasOwn(TRIGGERS, options.trigger)) {
      return { ok: false, error: `unknown trigger: ${singleLine(options.trigger)}`, failures }
    }
    if (
      typeof options.core !== 'string' ||
      !options.core ||
      typeof options.where !== 'string' ||
      !options.where
    ) {
      throw new Error('add requires --core and --where')
    }
    const fs = deps.fs ?? defaultFs
    const buffer = bufferPath(options, deps)
    const env = deps.env ?? process.env
    let runId =
      sanitizeRunId(options.runId) ||
      sanitizeRunId(env.BOSS_AGENT_SESSION_ID) ||
      sanitizeRunId(env.BOSS_SESSION_ID)
    if (!runId) {
      try {
        for (const raw of fs.readFileSync(buffer, 'utf8').split('\n')) {
          try {
            const id = sanitizeRunId(JSON.parse(raw).runId)
            if (id?.startsWith('adhoc-')) {
              runId = id
              break
            }
          } catch {
            /* Ignore malformed lines; flush reports them. */
          }
        }
      } catch (error) {
        if (error.code !== 'ENOENT') throw error
      }
    }
    runId ||= resolveRunId({ env })
    const line = {
      v: 1,
      runId,
      at: currentTime(deps).toISOString(),
      core: singleLine(options.core),
      trigger: options.trigger,
      where: singleLine(options.where),
      detail: singleLine(options.detail),
    }
    fs.appendFileSync(buffer, `${JSON.stringify(line)}\n`, {
      mode: 0o600,
    })
    return { ok: true, failures }
  } catch (error) {
    failures.push(failureText(error))
    return { ok: false, error: failures[0], failures }
  }
}

export function triggerIdempotencyKey(line) {
  const hash = createHash('sha256')
    .update([line.core, line.trigger, line.where, line.runId].join('|'))
    .digest('hex')
    .slice(0, 32)
  return `notes-record:${hash}`
}

function defaultAddNote({ body, idempotencyKey }, deps) {
  const env = deps.env ?? process.env
  const binary = resolveBossBinary(env, { fs: deps.fs ?? defaultFs })
  if (!binary.ok) throw new Error(binary.reason)
  const result = spawnSync(
    binary.path,
    [
      'notes',
      'add',
      '--tag',
      'improvement',
      '--json',
      '--idempotency-key',
      idempotencyKey,
      '--',
      body,
    ],
    { encoding: 'utf8', env, timeout: 10000 },
  )
  if (result.error) throw result.error
  if (result.status !== 0)
    throw new Error(`boss notes add exited ${result.status}: ${(result.stderr || '').trim()}`)
  const id = JSON.parse(result.stdout)?.id
  if (typeof id !== 'string' || !id) throw new Error('boss notes add returned no id')
  return id
}

export function flushTriggers(options, deps = {}) {
  const result = { recorded: [], dropped: [], failures: [] }
  if ((deps.env ?? process.env).BOSS_NOTES_SUPPRESSED === '1') return result
  const fs = deps.fs ?? defaultFs
  let claimed
  try {
    const buffer = bufferPath(options, deps)
    const claimPath = `${buffer}.flushing-${process.pid}`
    try {
      fs.renameSync(buffer, claimPath)
    } catch (error) {
      if (error.code === 'ENOENT') return result
      throw error
    }
    claimed = claimPath
    const text = fs.readFileSync(claimed, 'utf8')
    const now = currentTime(deps)
    const seen = new Set()
    let attempted = 0
    let categoryAttempted = 0
    for (const raw of text.split('\n').filter((line) => line.trim())) {
      let line
      try {
        line = JSON.parse(raw)
      } catch {
        result.dropped.push({ trigger: 'unknown', reason: 'malformed' })
        result.failures.push('malformed JSONL line')
        continue
      }
      if (
        !line ||
        line.v !== 1 ||
        !Object.hasOwn(TRIGGERS, line.trigger) ||
        typeof line.core !== 'string' ||
        !line.core ||
        typeof line.where !== 'string' ||
        !line.where ||
        typeof line.detail !== 'string' ||
        typeof line.at !== 'string' ||
        !Number.isFinite(Date.parse(line.at))
      ) {
        result.dropped.push({
          trigger: typeof line?.trigger === 'string' ? line.trigger : 'unknown',
          reason: 'malformed',
        })
        result.failures.push('malformed buffered trigger record')
        continue
      }
      if (now - new Date(line.at) > STALE_MS) {
        result.dropped.push({ trigger: line.trigger, reason: 'stale' })
        continue
      }
      line = {
        ...line,
        runId: resolveRunId({ runId: line.runId, env: deps.env ?? process.env }),
        core: singleLine(line.core),
        where: singleLine(line.where),
        detail: singleLine(line.detail),
      }
      const tuple = JSON.stringify([line.core, line.trigger, line.where, line.runId])
      if (seen.has(tuple)) continue
      seen.add(tuple)
      const trigger = TRIGGERS[line.trigger]
      const body = [
        `${line.core}: ${trigger.statement}`,
        `Where: ${line.where}`,
        `Why it matters: ${trigger.why}${line.detail ? ` Detail: ${line.detail}` : ''}`,
        `Suggested fix: ${trigger.fix}`,
        formatRunLine(
          [
            line.core,
            singleLine(options.outcome || 'unknown'),
            singleLine(options.mode || 'unknown'),
            `trigger:${line.trigger}`,
          ],
          line.runId,
        ),
      ].join('\n')
      if (isSecretBearing(body)) {
        result.dropped.push({ trigger: line.trigger, reason: 'secret-shape' })
        continue
      }
      const category = line.trigger === 'review-must-fix'
      if (
        (category ? categoryAttempted : attempted) >=
        (category ? MAX_CATEGORY_NOTES : MAX_TRIGGER_NOTES)
      ) {
        result.dropped.push(
          category
            ? { trigger: line.trigger, where: line.where, reason: 'over-category-cap' }
            : { trigger: line.trigger, reason: 'over-cap' },
        )
        continue
      }
      if (category) categoryAttempted += 1
      else attempted += 1
      try {
        const note = { body, tag: 'improvement', idempotencyKey: triggerIdempotencyKey(line) }
        const noteId = deps.addNote ? deps.addNote(note) : defaultAddNote(note, deps)
        if (typeof noteId !== 'string' || !noteId) throw new Error('boss notes add returned no id')
        result.recorded.push({ trigger: line.trigger, where: line.where, noteId })
      } catch (error) {
        result.failures.push(failureText(error))
      }
    }
  } catch (error) {
    result.failures.push(failureText(error))
  } finally {
    if (claimed) {
      try {
        fs.unlinkSync(claimed)
      } catch (error) {
        if (error.code !== 'ENOENT') result.failures.push(failureText(error))
      }
    }
  }
  return result
}

export function main(argv, deps = {}) {
  let result
  try {
    const [command, ...flags] = argv
    const options = {}
    const allowed = new Set(
      command === 'add'
        ? ['core', 'trigger', 'where', 'detail', 'buffer', 'run-id', 'from']
        : ['core', 'outcome', 'mode', 'buffer'],
    )
    for (let index = 0; index < flags.length; index += 2) {
      const flag = flags[index]
      if (!flag.startsWith('--') || !allowed.has(flag.slice(2)) || flags[index + 1] === undefined)
        throw new Error('invalid notes-record arguments')
      options[flag === '--run-id' ? 'runId' : flag.slice(2)] = flags[index + 1]
    }
    if (command === 'add') result = addTrigger(options, deps)
    else if (command === 'flush' && options.core && options.outcome)
      result = flushTriggers(options, deps)
    else
      throw new Error(
        'usage: notes-record.mjs add --core <core> --trigger <trigger> (--where <pointer> | --from <json-file>) | flush --core <core> --outcome <outcome>',
      )
  } catch (error) {
    const message = failureText(error)
    result = { ok: false, error: message, failures: [message] }
  }
  const stdout = deps.stdout ?? ((text) => process.stdout.write(text))
  const stderr = deps.stderr ?? ((text) => process.stderr.write(text))
  for (const message of result.failures?.length
    ? result.failures
    : result.error
      ? [result.error]
      : []) {
    try {
      stderr(`notes-record: ${message}\n`)
    } catch {
      /* Reporting must also be non-fatal. */
    }
  }
  try {
    stdout(`${JSON.stringify(result)}\n`)
  } catch {
    /* Closed output cannot fail the run. */
  }
  return 0
}

if (isMainModule(import.meta.url, { warn: () => {} })) process.exit(main(process.argv.slice(2)))
