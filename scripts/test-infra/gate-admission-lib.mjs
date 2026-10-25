import fs from 'node:fs'
import { randomUUID } from 'node:crypto'
import os from 'node:os'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

export const ADMISSION_TIMEOUT_SIGNATURE = 'gate-run admission: no host gate slot within'

const positive = (value, fallback) =>
  Number.isFinite(Number(value)) && Number(value) > 0 ? Number(value) : fallback

export function admissionConfig(env = process.env, cpus = os.availableParallelism()) {
  const configured = Number(env.BOSS_GATE_MAX_CONCURRENT)
  return {
    dir: path.resolve(
      env.BOSS_GATE_ADMISSION_DIR || path.join(os.homedir(), '.cache', 'boss', 'gate-admission'),
    ),
    slots:
      env.CI || env.BOSS_GATE_MAX_CONCURRENT === '0'
        ? 0
        : Number.isInteger(configured) && configured > 0
          ? configured
          : Math.max(1, Math.floor(cpus / 4)),
    timeoutMs: positive(env.BOSS_GATE_ADMISSION_TIMEOUT_MS, 3600000),
    pollMs: positive(env.BOSS_GATE_ADMISSION_POLL_MS, 1000),
  }
}

function alive(pid) {
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    return error.code === 'EPERM'
  }
}

function readRecord(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'))
  } catch {
    return null
  }
}

export function staleSlot(record, isAlive = alive) {
  return !Number.isInteger(record?.pid) || record.pid <= 0 || !isAlive(record.pid)
}

export function inheritedAdmission(env = process.env, readSlot = readRecord, isAlive = alive) {
  const file = env.BOSS_GATE_ADMITTED
  return file &&
    path.isAbsolute(file) &&
    path.dirname(file) === admissionConfig(env).dir &&
    /^slot-\d+$/.test(path.basename(file)) &&
    !staleSlot(readSlot(file), isAlive)
    ? file
    : null
}

function holderText(holders) {
  return holders
    .map(({ pid, cwd, label }) => `${pid} ${path.basename(cwd || '?')} ${label || 'gate'}`)
    .join(', ')
}

function* acquisitionSteps({
  dir,
  slots,
  timeoutMs,
  pollMs,
  label = '',
  log = (_line) => {},
  isAlive = alive,
  now = Date.now,
  signal = undefined,
  onWait = (_holders) => {},
}) {
  const started = now()
  fs.mkdirSync(dir, { recursive: true })
  const candidate = path.join(dir, `.candidate-${process.pid}-${randomUUID()}`)
  fs.writeFileSync(
    candidate,
    JSON.stringify({
      pid: process.pid,
      startedAt: new Date().toISOString(),
      cwd: process.cwd(),
      label,
    }),
    { flag: 'wx' },
  )
  let lastHolders = null
  let heldByAtAdmission = []
  try {
    for (;;) {
      signal?.throwIfAborted()
      const holders = []
      for (let index = 0; index < slots; index++) {
        const file = path.join(dir, `slot-${index}`)
        // Publish a complete record atomically. An open('wx') followed by write exposes an empty
        // record that another contender could reclaim as malformed before its owner writes it.
        try {
          fs.linkSync(candidate, file)
          const handle = {
            file,
            pid: process.pid,
            slots,
            waitedMs: lastHolders === null ? 0 : now() - started,
            heldByAtAdmission,
            inherited: false,
          }
          handle.onExit = () => releaseGateSlot(handle)
          process.once('exit', handle.onExit)
          return handle
        } catch (error) {
          if (error.code !== 'EEXIST') throw error
        }
        const record = readRecord(file)
        if (staleSlot(record, isAlive)) {
          // One reclaimer rechecks the owner and removes a dead slot. Without this
          // claim two waiters can both observe the dead inode and unlink a fresh holder.
          const claim = `${file}.reclaim`
          try {
            fs.linkSync(candidate, claim)
          } catch (error) {
            if (error.code !== 'EEXIST') throw error
            if (staleSlot(readRecord(claim), isAlive)) {
              try {
                fs.unlinkSync(claim)
              } catch (error) {
                if (error.code !== 'ENOENT') throw error
              }
            }
            continue
          }
          try {
            if (staleSlot(readRecord(file), isAlive)) {
              try {
                fs.unlinkSync(file)
              } catch (error) {
                if (error.code !== 'ENOENT') throw error
              }
            }
          } finally {
            fs.unlinkSync(claim)
          }
          index--
          continue
        }
        holders.push(record)
      }
      heldByAtAdmission = holders
      const text = holderText(holders)
      if (text !== lastHolders) {
        log(`gate-run admission: queued for a host gate slot - held by ${text}`)
        onWait(text)
        lastHolders = text
      }
      if (now() - started >= timeoutMs) {
        const error = Object.assign(
          new Error(`${ADMISSION_TIMEOUT_SIGNATURE} ${timeoutMs}ms (held by ${text})`),
          { code: 'GATE_ADMISSION_TIMEOUT' },
        )
        throw error
      }
      yield Math.min(pollMs, timeoutMs - (now() - started))
    }
  } finally {
    fs.rmSync(candidate, { force: true })
  }
}

// Both drivers share slot publication, stale-owner reclamation and release semantics.
export async function acquireGateSlot(options) {
  const steps = acquisitionSteps(options)
  try {
    for (;;) {
      const next = steps.next()
      if (next.done) return next.value
      await (options.sleep ?? delay)(next.value, undefined, { signal: options.signal })
    }
  } finally {
    steps.return(undefined)
  }
}

export function acquireGateSlotSync(options) {
  const steps = acquisitionSteps(options)
  try {
    for (;;) {
      const next = steps.next()
      if (next.done) return next.value
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, Number(next.value))
    }
  } finally {
    steps.return(undefined)
  }
}

export function releaseGateSlot(handle) {
  if (!handle || handle.inherited || !handle.file) return
  if (handle.onExit) process.removeListener('exit', handle.onExit)
  if (readRecord(handle.file)?.pid === handle.pid) fs.rmSync(handle.file, { force: true })
}

export async function admitGate(env = process.env, options = {}) {
  const config = admissionConfig(env)
  const inherited = inheritedAdmission(env)
  if (config.slots === 0 || inherited)
    return {
      ...config,
      file: inherited,
      inherited: Boolean(inherited),
      waitedMs: 0,
      heldByAtAdmission: [],
    }
  return acquireGateSlot({ ...config, ...options })
}

export function renderAdmissionLine(record) {
  return record
    ? `admission: waited ${(record.waitedMs / 1000).toFixed(1)}s for a host gate slot (${record.slots} slots)`
    : 'admission: unavailable'
}
