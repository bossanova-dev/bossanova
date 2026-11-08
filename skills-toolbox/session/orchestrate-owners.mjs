// Which daemons an orchestrator round acts on. By default one attended orchestrator covers every
// daemon. With `respectOwners`, each daemon has one orchestrator: a daemon whose own cron list runs
// the orchestrate skill `--unattended` belongs to that worker cron. orchestrate-snapshot.mjs
// applies it to every round's targets.

import { execFileSync } from 'node:child_process'

/** `boss cron ls --json` on the daemon's own host. */
export function readCrons(host, { bossBin, remoteBoss, remotePath }) {
  /** @type {import('node:child_process').ExecFileSyncOptionsWithStringEncoding} */
  const opts = { encoding: 'utf8', maxBuffer: 16 << 20, timeout: 60 * 1000 }
  const args = ['cron', 'ls', '--json']
  const raw = host
    ? execFileSync(
        'ssh',
        ['-o', 'BatchMode=yes', host, 'env', `PATH=${remotePath}`, remoteBoss, ...args],
        opts,
      )
    : execFileSync(bossBin, args, opts)
  const jobs = JSON.parse(raw)
  if (!Array.isArray(jobs)) throw new Error('boss cron ls --json did not return a list')
  return jobs
}

/** The name of the enabled cron job in `jobs` that runs `core` with `--unattended`, or ''. */
export function workerCron(jobs, core) {
  const name = core.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const skill = new RegExp(`(^|\\s)[/$]${name}(?=\\s|$)`)
  const worker = jobs.find(
    (job) =>
      job.enabled === true &&
      skill.test(job.prompt ?? '') &&
      /(^|\s)--unattended(?=\s|$)/.test(job.prompt ?? ''),
  )
  return worker?.name ?? ''
}

/**
 * Without `respectOwners` every target is this round's, and no cron list is read.
 *
 * With it, one orchestrator per daemon. A daemon whose own cron list has a worker cron (`workerCron`)
 * belongs to that worker, so this round leaves it out and lists it under `owners`; the attended
 * chat reads its queue instead. A worker round (`unattended`) keeps its own daemon, the local one.
 *
 * A cron list that cannot be read fails closed: the daemon is left out, named in `errors` and
 * `unread`. Covering a daemon that does have a worker would give its sessions two orchestrators
 * and can start two repairs on one PR, which no later round can undo, while leaving out a daemon
 * with no worker only delays it one round, and the error tells the operator. A daemon whose cron
 * list is unreadable is usually unreadable as a whole anyway.
 */
export function claimTargets({ targets, unattended, crons, where, core, respectOwners = false }) {
  if (!respectOwners) return { mine: [...targets], owners: [], errors: [], unread: [] }
  const mine = []
  const owners = []
  const errors = []
  const unread = []
  for (const target of targets) {
    if (unattended && !target.host) {
      mine.push(target)
      continue
    }
    let cron
    try {
      cron = workerCron(crons(target.host, where), core)
    } catch (error) {
      errors.push(
        `${target.label}: cron list unreadable, so its sessions are left out to avoid a second orchestrator (${String(error.message).split('\n')[0]})`,
      )
      unread.push(target.label)
      continue
    }
    if (cron) owners.push({ host: target.label, cron })
    else mine.push(target)
  }
  return { mine, owners, errors, unread }
}
