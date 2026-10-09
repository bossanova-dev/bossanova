// Repo-scoped retro lock, immutable note snapshot, and snapshot-guarded retirement.
import fs from 'node:fs'
import path from 'node:path'
import { randomUUID } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { resolveBossBinary } from './boss-binary.mjs'
import { isMainModule } from './main-module.mjs'

function staleSeconds(env) {
  for (const candidate of [env.BOSS_RETRO_LOCK_STALE_SECS]) {
    if (candidate === undefined || candidate === null || String(candidate).trim() === '') continue
    const value = Number(candidate)
    if (Number.isFinite(value) && value >= 0) return value
  }
  return 3600
}

// Publish an initialized, nonempty directory, so another transition can never
// replace the guard while its owner runs. Recovery unlinks only the dead owner's
// unique marker and uses rmdir: a successor's different marker prevents removal.
function transitionGuard(location, io, seconds, env) {
  const guard = location + '.reclaim'
  const marker = `${process.pid}.${randomUUID()}`
  const privateDir = `${guard}.${marker}`
  const busy = () => {
    let entries
    let age
    try {
      entries = io.readdirSync(guard)
      age = seconds - io.statSync(guard).mtimeMs / 1000
    } catch (error) {
      if (error.code === 'ENOENT') return false
      throw error
    }
    if (age < staleSeconds(env)) return true
    if (entries.length > 1) return true
    if (entries.length === 1) {
      const pid = /^([1-9][0-9]*)\./.exec(entries[0])?.[1]
      if (!pid) return true
      try {
        process.kill(Number(pid), 0)
        return true
      } catch (error) {
        if (error.code !== 'ESRCH') return true
      }
      try {
        io.unlinkSync(path.join(guard, entries[0]))
      } catch (error) {
        if (error.code !== 'ENOENT') throw error
      }
    }
    try {
      io.rmdirSync(guard)
    } catch (error) {
      if (['ENOTEMPTY', 'EEXIST'].includes(error.code)) return true
      if (error.code !== 'ENOENT') throw error
    }
    return false
  }
  // Check existing empty legacy guards explicitly; rename must not bypass their
  // stale threshold merely because POSIX permits replacing an empty directory.
  if (busy()) return null
  io.mkdirSync(privateDir)
  try {
    io.writeFileSync(path.join(privateDir, marker), '')
    try {
      io.renameSync(privateDir, guard)
    } catch (error) {
      if (['ENOTEMPTY', 'EEXIST'].includes(error.code)) return null
      throw error
    }
  } finally {
    io.rmSync(privateDir, { recursive: true, force: true })
  }
  return () => {
    try {
      io.unlinkSync(path.join(guard, marker))
    } catch (error) {
      if (error.code !== 'ENOENT') throw error
    }
    try {
      io.rmdirSync(guard)
    } catch (error) {
      if (!['ENOENT', 'ENOTEMPTY', 'EEXIST'].includes(error.code)) throw error
    }
  }
}

/** Atomic single-flight lock under the repository's git common directory. */
export function lock(
  action,
  { dir, token = randomUUID(), fs: io = fs, now = Date.now, env = process.env } = {},
) {
  if (typeof dir !== 'string' || !dir) throw new Error('lock requires --dir')
  if (!['acquire', 'touch', 'release'].includes(action)) throw new Error('unknown lock action')
  const location = path.join(dir, 'boss-retro.lock')
  const ownerFile = path.join(location, 'owner')
  const heartbeatFile = path.join(location, 'heartbeat')
  const seconds = Math.floor(now() / 1000)
  const releaseGuard = transitionGuard(location, io, seconds, env)
  if (!releaseGuard) return { verdict: action === 'acquire' ? 'held' : 'not-owner', token }
  try {
    if (action !== 'acquire') {
      let owner
      try {
        owner = io.readFileSync(ownerFile, 'utf8').trim()
      } catch (error) {
        if (error.code !== 'ENOENT') throw error
      }
      if (owner !== token) return { verdict: 'not-owner', token }
      if (action === 'touch') io.writeFileSync(heartbeatFile, String(seconds) + '\n')
      else io.rmSync(location, { recursive: true })
      return { verdict: action === 'touch' ? 'touched' : 'released', token }
    }
    try {
      io.mkdirSync(location)
    } catch (error) {
      if (error.code !== 'EEXIST') throw error
      let heartbeat
      try {
        const raw = io.readFileSync(heartbeatFile, 'utf8').trim()
        heartbeat = raw ? Number(raw) : NaN
      } catch {
        heartbeat = NaN
      }
      if (!Number.isFinite(heartbeat) || seconds - heartbeat < staleSeconds(env))
        return { verdict: 'held', token }
      io.rmSync(location, { recursive: true })
      io.mkdirSync(location)
    }
    io.writeFileSync(ownerFile, token + '\n')
    io.writeFileSync(heartbeatFile, String(seconds) + '\n')
    return { verdict: 'acquired', token }
  } finally {
    releaseGuard()
  }
}

/** Validate the whole notes list before publishing the immutable ID snapshot. */
export function snapshot(notes, idsOut, { fs: io = fs } = {}) {
  const valid = (note) =>
    note !== null &&
    typeof note === 'object' &&
    !Array.isArray(note) &&
    typeof note.id === 'string' &&
    note.id.length > 0 &&
    typeof note.body === 'string' &&
    typeof note.created_at === 'string' &&
    note.created_at.length > 0 &&
    Array.isArray(note.tags) &&
    note.tags.includes('improvement')
  if (
    !Array.isArray(notes) ||
    !notes.every(valid) ||
    new Set(notes.map((note) => note.id)).size !== notes.length
  ) {
    throw new Error('snapshot requires unique, well-formed improvement notes')
  }
  io.writeFileSync(idsOut, notes.map((note) => note.id).join('\n') + (notes.length ? '\n' : ''))
  return { count: notes.length }
}

function checkedPlan(plan, ids) {
  if (!plan || typeof plan !== 'object' || Array.isArray(plan))
    throw new Error('malformed retire plan')
  for (const op of ['delete', 'retag']) {
    if (
      !Array.isArray(plan[op]) ||
      !plan[op].every((id) => typeof id === 'string' && id.length > 0 && !/[\r\n]/.test(id))
    ) {
      throw new Error('malformed retire plan IDs')
    }
  }
  const counts = plan.counts
  if (
    !counts ||
    counts.delete !== plan.delete.length ||
    counts.retag !== plan.retag.length ||
    counts.drain !== counts.delete + counts.retag
  ) {
    throw new Error('malformed retire plan counts')
  }
  for (const id of [...plan.delete, ...plan.retag]) {
    if (!ids.has(id)) throw new Error(`retire ID outside snapshot: ${id}`)
  }
  const deletions = [...new Set(plan.delete)]
  const deleting = new Set(deletions)
  return [
    ...deletions.map((id) => ({ op: 'delete', id })),
    ...[...new Set(plan.retag)]
      .filter((id) => !deleting.has(id))
      .map((id) => ({ op: 'retag', id })),
  ]
}

/** Execute only snapshot IDs, recording each successful operation for resume. */
export function retire(
  plan,
  snapshotIds,
  { progress, dryRun = false, fs: io = fs, run = spawnSync, env = process.env, cwd } = {},
) {
  if (typeof progress !== 'string' || !progress) throw new Error('retire requires --progress')
  const ids = new Set(io.readFileSync(snapshotIds, 'utf8').split('\n').filter(Boolean))
  const ops = checkedPlan(plan, ids)
  if (dryRun) {
    const deleting = ops.filter(({ op }) => op === 'delete').length
    return { delete: deleting, retag: ops.length - deleting, drain: ops.length }
  }
  let previous = ''
  try {
    previous = io.readFileSync(progress, 'utf8')
  } catch (error) {
    if (error.code !== 'ENOENT') throw error
  }
  const completed = new Set(previous.split('\n').filter(Boolean))
  const remaining = ops.filter(({ op, id }) => !completed.has(`${op} ${id}`))
  if (!remaining.length) return { done: 0, failed: null, remaining: 0 }
  const binary = resolveBossBinary(env, { fs: io, cwd })
  if (!binary.ok) throw new Error(binary.reason)
  let done = 0
  for (const { op, id } of remaining) {
    let result
    try {
      result = run(
        binary.path,
        op === 'delete' ? ['notes', 'rm', id] : ['notes', 'edit', id, '--tag', 'stale'],
        { encoding: 'utf8', env },
      )
    } catch {
      return { done, failed: { op, id }, remaining: remaining.length - done }
    }
    if (result?.error || result?.status !== 0)
      return { done, failed: { op, id }, remaining: remaining.length - done }
    io.appendFileSync(progress, `${op} ${id}\n`)
    done++
  }
  return { done, failed: null, remaining: 0 }
}

export function runCli(argv, deps = {}) {
  const [command, ...args] = argv
  const io = deps.fs ?? fs
  if (command === 'snapshot') {
    if (args.length !== 2) throw new Error('usage: snapshot <notes.json> <ids-out>')
    return {
      output: snapshot(JSON.parse(io.readFileSync(args[0], 'utf8')), args[1], deps),
      exitCode: 0,
    }
  }
  if (command === 'lock') {
    const [action, ...flags] = args
    const options = { ...deps }
    for (let i = 0; i < flags.length; i += 2) {
      if (!['--dir', '--token'].includes(flags[i]) || !flags[i + 1])
        throw new Error('usage: lock acquire|touch|release --dir <dir> [--token <token>]')
      options[flags[i].slice(2)] = flags[i + 1]
    }
    if (action !== 'acquire' && !options.token)
      throw new Error('lock touch/release requires --token')
    const output = lock(action, options)
    return { output, exitCode: ['held', 'not-owner'].includes(output.verdict) ? 3 : 0 }
  }
  if (command === 'retire') {
    const [planFile, idsFile, ...flags] = args
    if (!planFile || !idsFile)
      throw new Error('usage: retire <plan.json> <snapshot-ids> --progress <file> [--dry-run]')
    const options = { ...deps }
    for (let i = 0; i < flags.length; i++) {
      if (flags[i] === '--dry-run') options.dryRun = true
      else if (flags[i] === '--progress' && flags[i + 1]) options.progress = flags[++i]
      else throw new Error('unknown retire flag')
    }
    const output = retire(JSON.parse(io.readFileSync(planFile, 'utf8')), idsFile, options)
    return { output, exitCode: output.failed ? 1 : 0 }
  }
  throw new Error('unknown retro-write command')
}

if (isMainModule(import.meta.url)) {
  try {
    const { output, exitCode } = runCli(process.argv.slice(2))
    process.stdout.write(JSON.stringify(output) + '\n')
    process.exitCode = exitCode
  } catch (error) {
    process.stderr.write(`retro-write: ${error.message}\n`)
    process.exitCode = 1
  }
}
