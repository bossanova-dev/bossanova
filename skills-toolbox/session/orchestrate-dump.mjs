// Reads every fact the orchestrator snapshot needs from one daemon in one round trip: a node
// script piped to `node -` on the daemon's machine (over SSH for a remote host).

import { execFileSync } from 'node:child_process'

// The tag a repair puts on the durable note it records when it stops short (boss-repair's
// RESIDUAL_NOTE_TAG); a stop on a red found on the base is one of them.
export const RESIDUAL_TAG = 'repair-residual'

// How long one collection may keep calling boss, below the budget an orchestrating loop gives
// one `collect` round. Past it the dump stops calling and returns what it read.
export const DEFAULT_DUMP_DEADLINE_MS = 4 * 60 * 1000
// How much longer than its deadline the caller waits for the dump (SSH setup, node start-up, the
// last in-flight call being stopped) before it kills the round trip.
const DUMP_GRACE_MS = 30 * 1000

// argv: boss, deadline in ms. Prints { sessions, residuals, errors, partial }; `residuals` are the
// host's repair-residual notes across every repo (`--repo ""`, since the notes list defaults to
// the current repo). A session that is a cron job's run carries that job as `cron`: its latest
// run, or a session titled with the job's name in the job's repo (a fire titles its session so; a
// branchless plan run keeps that title, and an overlapping or gated later fire moves the job's
// latest-run pointer).
//
// The dump never outlives its caller. It writes a space every second (the caller's JSON.parse
// skips leading whitespace), so once the caller or its SSH connection is gone a write fails and
// the dump stops its in-flight boss calls and exits. Past its deadline it issues no boss call,
// and each call is cut off where the deadline falls: the sessions it did not finish come back
// with errors, the ones it never started are left out and `partial` is set.
export const DAEMON_DUMP = `
const { execFile } = require('node:child_process')
const boss = process.argv[2] || 'boss'
const budget = Number(process.argv[3]) || ${DEFAULT_DUMP_DEADLINE_MS}
const deadline = Date.now() + budget
const CALL_TIMEOUT = 90000
const running = new Set()
function stop() {
  for (const child of running) child.kill()
  process.exit(1)
}
process.stdout.on('error', stop)
for (const signal of ['SIGHUP', 'SIGINT', 'SIGTERM']) process.on(signal, stop)
const heartbeat = setInterval(() => process.stdout.write(' '), 1000)
const run = (args) => new Promise((resolve, reject) => {
  const left = deadline - Date.now()
  if (left <= 0) return reject(new Error('deadline passed'))
  const child = execFile(boss, args, { encoding: 'utf8', maxBuffer: 64 << 20, timeout: Math.min(CALL_TIMEOUT, left) }, (err, out) => {
    running.delete(child)
    if (err) return reject(err.killed && Date.now() >= deadline ? new Error('deadline passed') : err)
    try { resolve(JSON.parse(out)) } catch (e) { reject(e) }
  })
  running.add(child)
})
const settle = (p) => p.then((value) => ({ status: 'fulfilled', value }), (reason) => ({ status: 'rejected', reason }))
const TERMINAL = new Set(['CLOSED', 'MERGED', 'ARCHIVED'])
// The cron job each session is a run of, from one cron list.
const cronBySession = new Map()
const cronByTitle = new Map()
const titleKey = (repoId, name) => repoId + '\\0' + name
async function one(s) {
  const entry = { session: s, chats: [], pr: null }
  if (TERMINAL.has(s.state)) return entry
  const cron = cronBySession.get(s.id) ?? cronByTitle.get(titleKey(s.repo_id, s.title))
  if (cron) entry.cron = cron
  const ref = /github\\.com\\/([^/]+\\/[^/]+)\\/pull\\/(\\d+)/.exec(s.pr_url || '')
  // Sequential per session and few workers: a burst of parallel calls makes bossd drop connections.
  const pr = await settle(ref ? run(['pr', 'state', ref[2], '--repo', ref[1], '--json']) : Promise.resolve(null))
  if (pr.status === 'fulfilled') entry.pr = pr.value
  else entry.errors = [...(entry.errors ?? []), 'pr state: ' + String(pr.reason.message).slice(0, 160)]
  if (ref) entry.repo = ref[1]
  const chats = await settle(run(['chats', '--json', s.id]))
  const shown = await settle(run(['show', '--json', s.id]))
  if (chats.status === 'fulfilled') entry.chats = chats.value.chats ?? []
  else entry.errors = [...(entry.errors ?? []), 'chats: ' + String(chats.reason.message).slice(0, 160)]
  if (shown.status === 'fulfilled') entry.session = shown.value.session ?? s
  return entry
}
;(async () => {
  const errors = []
  const crons = await settle(run(['cron', 'ls', '--json']))
  if (crons.status === 'fulfilled') {
    const jobs = Array.isArray(crons.value) ? crons.value : (crons.value.jobs ?? [])
    for (const job of jobs) {
      if (job.last_run_session_id) cronBySession.set(job.last_run_session_id, { id: job.id, name: job.name })
      if (job.repo_id && job.name) cronByTitle.set(titleKey(job.repo_id, job.name), { id: job.id, name: job.name })
    }
  } else errors.push('cron jobs: ' + String(crons.reason.message).slice(0, 160))
  const listed = await run(['ls', '--json'])
  const queue = [...(listed.sessions ?? listed)]
  const out = []
  await Promise.all(Array.from({ length: 4 }, async () => {
    while (queue.length && Date.now() < deadline) out.push(await one(queue.shift()))
  }))
  const partial = Date.now() >= deadline
  if (partial) errors.push('deadline: stopped after ' + Math.round(budget / 1000) + 's, ' + queue.length + ' session(s) not read')
  const notes = await settle(run(['notes', 'ls', '--tag', '${RESIDUAL_TAG}', '--repo', '', '--json']))
  let residuals = []
  if (notes.status === 'fulfilled') {
    const list = Array.isArray(notes.value) ? notes.value : (notes.value.notes ?? [])
    residuals = list.map((n) => ({ id: n.id, repo_id: n.repo_id, session_id: n.session_id,
      created_at: n.created_at, body: String(n.body ?? '').slice(0, 4000) }))
  } else errors.push('residual notes: ' + String(notes.reason.message).slice(0, 160))
  clearInterval(heartbeat)
  process.stdout.write(JSON.stringify({ sessions: out, residuals, errors, partial }))
})().catch((e) => { process.stderr.write(String(e.message)); stop() })
`

// A non-interactive SSH command gets a minimal PATH, and the host's login shell may not be POSIX,
// so remote commands run through `env` with a PATH covering the usual install locations.
export const DEFAULT_REMOTE_PATH = '/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin'

/**
 * Runs DAEMON_DUMP against the daemon on `host` ('' for this machine) and returns its result.
 * @param {string} host
 * @param {{ bossBin: string, remoteBoss: string, remotePath: string, deadlineMs?: number }} where
 */
export function dumpDaemon(
  host,
  { bossBin, remoteBoss, remotePath, deadlineMs = DEFAULT_DUMP_DEADLINE_MS },
) {
  /** @type {import('node:child_process').ExecFileSyncOptionsWithStringEncoding} */
  const opts = {
    input: DAEMON_DUMP,
    encoding: 'utf8',
    maxBuffer: 64 << 20,
    timeout: deadlineMs + DUMP_GRACE_MS,
  }
  const budget = String(deadlineMs)
  const raw = host
    ? execFileSync(
        'ssh',
        ['-o', 'BatchMode=yes', host, 'env', `PATH=${remotePath}`, 'node', '-', remoteBoss, budget],
        opts,
      )
    : execFileSync('node', ['-', bossBin, budget], opts)
  return JSON.parse(raw)
}
