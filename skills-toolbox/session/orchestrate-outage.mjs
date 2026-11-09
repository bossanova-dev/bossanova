#!/usr/bin/env node

// Outage mode for the orchestrator. When one infrastructure failure (a CI provider outage, a
// download host returning 5xx) reds many sessions at once, the orchestrator pauses the factory
// instead of letting each session burn tokens on a red it cannot fix: it disables the crons that
// would start new runs into the outage, tells the affected sessions' live chats to hold, and keeps
// one durable record of everything it changed. When the outage is over, `resume` undoes exactly
// that: it re-enables the crons it disabled, reruns the failed runs it recorded, tells the held
// sessions to continue, and clears the record. A fresh orchestrator that finds the record resumes
// it, so nothing stays paused when the orchestrator that paused it is gone.
//
//   node orchestrate-outage.mjs pause --reason <text> [--host <h>]... [--no-local]
//        [--cron-match <regex>] [--session <id>[@<host>]]... [--run <owner/repo>#<run-id>]...
//        [--probe <url>]...
//   node orchestrate-outage.mjs status          the record, and whether every probe answers
//   node orchestrate-outage.mjs resume [--force]  refuses while a probe still fails, unless --force
// Each verb takes [--state-dir <dir>] [--boss <boss>].

import { execFileSync } from 'node:child_process'
import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'

/** @typedef {(args: string[]) => string} BossRunner */
/** @typedef {(args: string[]) => string} GhRunner */
/** @typedef {(url: string) => { url: string, status: number, ok: boolean }} Prober */

export const DEFAULT_STATE_DIR = path.join(homedir(), '.cache', 'boss-orchestrate')
export const outageFile = (dir = DEFAULT_STATE_DIR) => path.join(dir, 'outage.json')

// The crons that start new work: build and plan runs. Verify, release and maintenance crons keep
// running; they only read, or they wait on green checks by themselves.
export const DEFAULT_CRON_MATCH = '\\b(?:build|plan)\\b'
const RUN_REF = /^([\w.-]+\/[\w.-]+)#(\d+)$/
const SESSION_REF = /^([A-Za-z0-9][A-Za-z0-9-]*)(?:@([\w.-]+))?$/

const HOLD_MESSAGE = (reason) =>
  `Operator: an infrastructure outage (${reason}) is failing CI across sessions. Hold: do not repair, review or rerun this red, and do not mark the run blocked on it. The orchestrator will tell you when it has recovered.`
const CONTINUE_MESSAGE = (reason) =>
  `Operator: the outage (${reason}) has recovered and its failed jobs are rerunning. Continue toward the hand-off, and treat any red that remains as your own.`

/** The open outage record in `dir`, or null. */
export function readOutage(dir = DEFAULT_STATE_DIR) {
  try {
    const record = JSON.parse(readFileSync(outageFile(dir), 'utf8'))
    return record && typeof record === 'object' && !Array.isArray(record) ? record : null
  } catch {
    return null
  }
}

function saveOutage(dir, record) {
  mkdirSync(dir, { recursive: true })
  const file = outageFile(dir)
  writeFileSync(`${file}.tmp`, `${JSON.stringify(record, null, 2)}\n`)
  renameSync(`${file}.tmp`, file)
}

const onHost = (host) => (host ? ['--host', host] : [])
const sameHost = (a, b) => (a || '') === (b || '')

function liveChats(runBoss, host, session) {
  const listed = JSON.parse(runBoss([...onHost(host), 'chats', '--json', session]))
  return (listed?.chats ?? []).filter(
    (chat) => chat.spinner_present === true || chat.status === 'WORKING',
  )
}

function newestChat(runBoss, host, session) {
  const listed = JSON.parse(runBoss([...onHost(host), 'chats', '--json', session]))
  const chats = [...(listed?.chats ?? [])]
  chats.sort((a, b) => Date.parse(b.created_at ?? '') - Date.parse(a.created_at ?? ''))
  return chats[0] ?? null
}

/** `--session <id>[@<host>]` and `--run <owner/repo>#<run-id>` arguments, parsed. */
export function parseRefs({ sessions = [], runs = [] }) {
  return {
    sessions: sessions.map((ref) => {
      const match = SESSION_REF.exec(ref)
      if (!match) throw new Error(`--session wants <id>[@<host>], not ${JSON.stringify(ref)}`)
      return { session: match[1], host: match[2] ?? '' }
    }),
    runs: runs.map((ref) => {
      const match = RUN_REF.exec(ref)
      if (!match) throw new Error(`--run wants <owner/repo>#<run-id>, not ${JSON.stringify(ref)}`)
      return { repo: match[1], run: match[2] }
    }),
  }
}

/**
 * Pauses the factory for an outage, or extends the pause already recorded: disables every enabled
 * cron on `hosts` whose name or prompt matches `cronMatch`, tells the live chats of each newly held
 * session to hold, and records the crons, sessions, failed runs and probe URLs. The record is saved
 * after every change, so a pause that dies halfway still resumes everything it did.
 */
export function pause({
  dir = DEFAULT_STATE_DIR,
  reason,
  hosts = [''],
  cronMatch = DEFAULT_CRON_MATCH,
  sessions = [],
  runs = [],
  probes = [],
  runBoss,
  now = new Date(),
}) {
  const record = readOutage(dir) ?? {
    reason: String(reason ?? '').trim(),
    since: now.toISOString(),
    crons: [],
    sessions: [],
    runs: [],
    probes: [],
  }
  if (!record.reason) throw new Error('pause needs --reason <what is down>')
  const save = () => saveOutage(dir, record)
  for (const url of probes) if (!record.probes.includes(url)) record.probes.push(url)
  for (const run of runs)
    if (!record.runs.some((r) => r.repo === run.repo && r.run === run.run)) record.runs.push(run)
  save()
  const match = new RegExp(cronMatch, 'i')
  const disabled = []
  for (const host of hosts) {
    const listed = JSON.parse(runBoss([...onHost(host), 'cron', 'ls', '--json']))
    for (const cron of Array.isArray(listed) ? listed : (listed?.crons ?? [])) {
      const ours = record.crons.some((c) => c.id === cron.id && sameHost(c.host, host))
      // A cron we disabled that came back on (a config reconcile, say) is disabled again.
      if (!cron.enabled || !(ours || match.test(`${cron.name}\n${cron.prompt ?? ''}`))) continue
      runBoss([...onHost(host), 'cron', 'disable', cron.id])
      if (!ours) record.crons.push({ host, id: cron.id, name: cron.name })
      disabled.push(`${host || 'local'}/${cron.name}`)
      save()
    }
  }
  const held = []
  for (const { session, host } of sessions) {
    if (record.sessions.some((s) => s.session === session && sameHost(s.host, host))) continue
    const told = liveChats(runBoss, host, session).map((chat) => chat.agent_session_id)
    for (const chat of told)
      runBoss([...onHost(host), 'chat', 'send', '--json', chat, HOLD_MESSAGE(record.reason)])
    record.sessions.push({ host, session, told })
    held.push(`${host || 'local'}/${session}`)
    save()
  }
  return { record, disabled, held }
}

/** GETs the first byte of `url` and reports its HTTP status; 2xx and 3xx count as up. */
export function curlProbe(url) {
  let status = 0
  try {
    const out = execFileSync(
      'curl',
      ['-sS', '-L', '-o', '/dev/null', '-r', '0-0', '--max-time', '20', '-w', '%{http_code}', url],
      { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] },
    )
    status = Number(out.trim()) || 0
  } catch {
    status = 0
  }
  return { url, status, ok: status >= 200 && status < 400 }
}

/** The open record with every probe's answer; `recovered` is true when every probe is up. */
export function status({ dir = DEFAULT_STATE_DIR, probe = curlProbe }) {
  const record = readOutage(dir)
  if (!record) return { open: false }
  const probes = record.probes.map((url) => probe(url))
  return { open: true, record, probes, recovered: probes.every((p) => p.ok) }
}

// A run that is already re-running was rerun by an earlier resume that died before saving.
const alreadyRerun = (error) =>
  /already (?:running|in progress|been re-?run)|is still running/i.test(
    `${error?.message ?? ''}\n${error?.stderr ?? ''}`,
  )

/**
 * Ends the outage: re-enables exactly the recorded crons, reruns the recorded failed runs, tells
 * every held session's newest chat to continue, and clears the record. Each item leaves the record
 * as it is done, so a resume that fails partway can simply be run again. Refuses while a recorded
 * probe still fails, unless `force`.
 */
export function resume({
  dir = DEFAULT_STATE_DIR,
  force = false,
  runBoss,
  runGh,
  probe = curlProbe,
}) {
  const checked = status({ dir, probe })
  if (!checked.open) return { resumed: false, reason: 'no outage is recorded' }
  if (!checked.recovered && !force) {
    const down = checked.probes.filter((p) => !p.ok).map((p) => `${p.url} (${p.status})`)
    return { resumed: false, reason: `still down: ${down.join(', ')}` }
  }
  const record = checked.record
  const errors = []
  const done = { crons: [], runs: [], sessions: [] }
  const drain = (key, act) => {
    for (const item of [...record[key]]) {
      try {
        act(item)
      } catch (error) {
        if (key !== 'runs' || !alreadyRerun(error)) {
          errors.push(`${key} ${JSON.stringify(item)}: ${String(error.message).split('\n')[0]}`)
          continue
        }
      }
      record[key] = record[key].filter((other) => other !== item)
      done[key].push(item)
      saveOutage(dir, record)
    }
  }
  drain('crons', ({ host, id }) => runBoss([...onHost(host), 'cron', 'enable', id]))
  drain('runs', ({ repo, run }) => runGh(['run', 'rerun', run, '--failed', '-R', repo]))
  drain('sessions', ({ host, session }) => {
    const chat = newestChat(runBoss, host, session)
    if (!chat) return
    runBoss([
      ...onHost(host),
      'chat',
      'send',
      '--json',
      chat.agent_session_id,
      CONTINUE_MESSAGE(record.reason),
    ])
  })
  if (errors.length) return { resumed: false, done, errors, record }
  rmSync(outageFile(dir), { force: true })
  return { resumed: true, done }
}

/**
 * The snapshot rows of held sessions, set to `held` while the outage lasts so no one repairs or
 * nudges them. `label` names a row's host the way the snapshot does ('local' for this machine).
 */
export function holdRows(rows, record) {
  if (!record) return rows
  const settled = new Set(['skip', 'done', 'awaiting-operator', 'stale-session'])
  for (const row of rows) {
    const held = (record.sessions ?? []).some(
      (s) => s.session === row.session && (s.host || 'local') === row.host,
    )
    if (!held || settled.has(row.action)) continue
    row.why = `outage hold (${record.reason}); was ${row.action}: ${row.why}`
    row.action = 'held'
    row.next = ''
  }
  return rows
}

function main(argv, io) {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    strict: true,
    options: {
      reason: { type: 'string' },
      host: { type: 'string', multiple: true, default: [] },
      'no-local': { type: 'boolean', default: false },
      'cron-match': { type: 'string', default: DEFAULT_CRON_MATCH },
      session: { type: 'string', multiple: true, default: [] },
      run: { type: 'string', multiple: true, default: [] },
      probe: { type: 'string', multiple: true, default: [] },
      force: { type: 'boolean', default: false },
      'state-dir': { type: 'string', default: DEFAULT_STATE_DIR },
      boss: { type: 'string', default: process.env.BOSS_BIN || 'boss' },
    },
  })
  const runBoss =
    io.runBoss ??
    ((args) => execFileSync(values.boss, args, { encoding: 'utf8', maxBuffer: 64 << 20 }))
  const runGh = io.runGh ?? ((args) => execFileSync('gh', args, { encoding: 'utf8' }))
  const dir = values['state-dir']
  const print = (value) => io.write(`${JSON.stringify(value, null, 2)}\n`)
  const [command] = positionals
  if (command === 'pause') {
    const refs = parseRefs({ sessions: values.session, runs: values.run })
    const hosts = [...(values['no-local'] ? [] : ['']), ...values.host]
    const result = pause({
      dir,
      reason: values.reason,
      hosts,
      cronMatch: values['cron-match'],
      ...refs,
      probes: values.probe,
      runBoss,
    })
    print({
      file: outageFile(dir),
      disabled: result.disabled,
      held: result.held,
      record: result.record,
    })
    return 0
  }
  if (command === 'status') {
    print({ file: outageFile(dir), ...status({ dir, probe: io.probe }) })
    return 0
  }
  if (command === 'resume') {
    const result = resume({ dir, force: values.force, runBoss, runGh, probe: io.probe })
    print(result)
    return result.resumed ? 0 : result.errors ? 2 : 3
  }
  process.stderr.write('usage: orchestrate-outage.mjs pause|status|resume [options]\n')
  return 64
}

/** Runs one verb and returns its exit code: 0 done, 1 failed, 2 partly resumed, 3 not resumed. */
export function run(argv, io = {}) {
  try {
    return main(argv, { write: (text) => process.stdout.write(text), ...io })
  } catch (error) {
    process.stderr.write(`orchestrate-outage: ${String(error.message).split('\n')[0]}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url)) process.exitCode = run(process.argv.slice(2))
