import fs from 'node:fs'
import { randomUUID } from 'node:crypto'
import os from 'node:os'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

export const ADMISSION_TIMEOUT_SIGNATURE = 'gate-run admission: no host gate slot within'

const positive = (value, fallback) =>
  Number.isFinite(Number(value)) && Number(value) > 0 ? Number(value) : fallback

const MAKE_VALUE_OPTIONS = new Set(['-C', '-f', '-o', '-W', '-I'])
const MAKE_NUMERIC_OPTIONS = new Set(['-j', '-l'])
const LIGHT_MAKE_TARGET = /^(?:(?:lint|format|build|generate)(?:-.+)?|bin\/.+|.+-check)$/

function makeTargets(args) {
  const targets = []
  for (let index = 0; index < args.length; index++) {
    const arg = args[index]
    if (MAKE_VALUE_OPTIONS.has(arg)) index++
    else if (MAKE_NUMERIC_OPTIONS.has(arg)) {
      if (/^\d+(?:\.\d+)?$/.test(args[index + 1] ?? '')) index++
    } else if (!arg.startsWith('-') && !arg.includes('=')) targets.push(arg)
  }
  return targets
}

/**
 * Classifies a gate command: lint, format, build and codegen-check commands are 'light';
 * everything else, including anything unrecognised, is 'heavy'.
 * @param {readonly string[] | undefined} argv
 * @returns {'light' | 'heavy'}
 */
export function gateClass(argv) {
  const [command = '', ...args] = argv ?? []
  const program = path.basename(command)
  if (program === 'make' || program === 'gmake') {
    const targets = makeTargets(args)
    return targets.length > 0 &&
      targets.every((target) => LIGHT_MAKE_TARGET.test(target) && !target.includes('test'))
      ? 'light'
      : 'heavy'
  }
  if (program === 'buf') return ['lint', 'format', 'breaking'].includes(args[0]) ? 'light' : 'heavy'
  if (/^node(?:\.exe)?$/.test(program))
    return /(?:^|\/)scripts\/lint\/[^/]+\.mjs$/.test((args[0] ?? '').replaceAll('\\', '/'))
      ? 'light'
      : 'heavy'
  return 'heavy'
}

const width = (value) => {
  const number = Number(value)
  return Number.isInteger(number) && number > 0 ? number : null
}

/**
 * Light gates share `<dir>/light`, so the heavy pool's slot scan never sees them and one
 * `BOSS_GATE_ADMISSION_DIR` override moves both pools.
 * @param {NodeJS.ProcessEnv} [env]
 * @param {number} [cpus]
 * @param {'light' | 'heavy'} [pool]
 */
export function admissionConfig(
  env = process.env,
  cpus = os.availableParallelism(),
  pool = 'heavy',
) {
  const base = path.resolve(
    env.BOSS_GATE_ADMISSION_DIR || path.join(os.homedir(), '.cache', 'boss', 'gate-admission'),
  )
  const bypassed = Boolean(env.CI) || env.BOSS_GATE_MAX_CONCURRENT === '0'
  const light = pool === 'light'
  return {
    pool: /** @type {'light' | 'heavy'} */ (light ? 'light' : 'heavy'),
    dir: light ? path.join(base, 'light') : base,
    slots: bypassed
      ? 0
      : light
        ? env.BOSS_GATE_LIGHT_MAX_CONCURRENT === '0'
          ? 0
          : (width(env.BOSS_GATE_LIGHT_MAX_CONCURRENT) ?? Math.max(2, Math.floor(cpus / 4)))
        : (width(env.BOSS_GATE_MAX_CONCURRENT) ?? Math.max(1, Math.floor(cpus / 4))),
    timeoutMs: positive(env.BOSS_GATE_ADMISSION_TIMEOUT_MS, 3600000),
    pollMs: positive(env.BOSS_GATE_ADMISSION_POLL_MS, 1000),
  }
}

export function bazelResourceFlags({ slots, cpus }) {
  if (!Number.isInteger(slots) || slots <= 0) return []
  const share = Math.max(1, Math.floor(cpus / slots))
  return [`--local_resources=cpu=${share}`, `--local_test_jobs=${share}`]
}

// Bazel shares always divide the host by the heavy pool's width: a light gate that runs Bazel
// must not claim a bigger share than the heavy gates it runs beside.
export function gateBazelEnv(admission, cpus = os.availableParallelism()) {
  return {
    BOSS_GATE_BAZEL_FLAGS: bazelResourceFlags({
      slots: admission.bazelSlots ?? admission.slots,
      cpus,
    }).join(' '),
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

/** @returns {'light' | 'heavy' | null} the pool whose directory holds a slot token, if any */
function tokenPool(file, env) {
  if (!file || !path.isAbsolute(file) || !/^slot-\d+$/.test(path.basename(file))) return null
  for (const pool of /** @type {const} */ (['heavy', 'light']))
    if (path.dirname(file) === admissionConfig(env, 1, pool).dir) return pool
  return null
}

// A nested wrapper inherits a live token from either pool, so the outermost wrapper's class decides.
export function inheritedAdmission(env = process.env, readSlot = readRecord, isAlive = alive) {
  const file = env.BOSS_GATE_ADMITTED
  return tokenPool(file, env) && !staleSlot(readSlot(file), isAlive) ? file : null
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
  pool = 'host',
  log = (_line) => {},
  isAlive = alive,
  now = Date.now,
  signal = undefined,
  onWait = (_holders, _pool) => {},
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
            pool,
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
        log(`gate-run admission: queued for a ${pool} gate slot - held by ${text}`)
        onWait(text, pool)
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

/**
 * Admits a gate to its class's pool, or inherits a live token from either pool.
 * @param {NodeJS.ProcessEnv} [env]
 * @param {{ argv?: readonly string[], gateClass?: 'light' | 'heavy', [key: string]: unknown }} [options]
 */
export async function admitGate(env = process.env, options = {}) {
  const { argv, gateClass: requested, ...acquire } = options
  const cpus = os.availableParallelism()
  const config = admissionConfig(env, cpus, requested ?? gateClass(argv))
  const bazelSlots = admissionConfig(env, cpus).slots
  const inherited = inheritedAdmission(env)
  if (config.slots === 0 || inherited) {
    const held = inherited ? tokenPool(inherited, env) : config.pool
    return {
      ...admissionConfig(env, cpus, held ?? config.pool),
      file: inherited,
      inherited: Boolean(inherited),
      waitedMs: 0,
      heldByAtAdmission: [],
      bazelSlots,
    }
  }
  return Object.assign(await acquireGateSlot({ ...config, ...acquire }), { bazelSlots })
}

export function renderAdmissionLine(record) {
  const pool = record?.pool ?? 'heavy'
  return record
    ? `admission: waited ${(record.waitedMs / 1000).toFixed(1)}s for a ${pool} gate slot (${pool} pool, ${record.slots} slots)`
    : 'admission: unavailable'
}
