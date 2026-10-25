import { spawnSync } from 'node:child_process'
import os from 'node:os'
import path from 'node:path'
import { acquireGateSlotSync, releaseGateSlot } from './gate-admission-lib.mjs'

export const GOLANGCI_ADMISSION_TIMEOUT_SIGNATURE =
  'golangci-lint: host admission wait exceeded after'

/** @param {NodeJS.ProcessEnv} [env] @param {number} [cpus] */
export function golangciAdmissionConfig(env = process.env, cpus = os.availableParallelism()) {
  const positive = (value, fallback) =>
    Number.isInteger(Number(value)) && Number(value) > 0 ? Number(value) : fallback
  return {
    dir: path.resolve(
      env.BOSS_GOLANGCI_SLOT_DIR || path.join(os.homedir(), '.cache', 'bossanova-golangci-slots'),
    ),
    slots: positive(env.BOSS_GOLANGCI_SLOTS, Math.max(2, Math.floor(cpus / 4))),
    timeoutMs: positive(env.BOSS_GOLANGCI_ADMISSION_WAIT_MS, 1800000),
    pollMs: 250 + Math.floor(Math.random() * 751),
    cpus,
  }
}

/** Admission waits are outside the child's timeout. Runner flags belong here alone.
 * @param {string[]} args
 * @param {import('node:child_process').SpawnSyncOptionsWithStringEncoding & {privateCache?: boolean}} [options]
 */
export function runGolangciSync(args, options = { encoding: 'utf8' }) {
  if (args.some((arg) => /^--allow-(?:parallel|serial)-runners(?:=|$)/.test(arg)))
    throw new Error('golangci runner flags are owned by host admission')
  const { privateCache: explicitPrivate = false, ...spawnOptions } = options
  const env = options.env ?? process.env
  const config = golangciAdmissionConfig(env)
  const privateCache =
    explicitPrivate ||
    Boolean(env.GOLANGCI_LINT_CACHE && env.GOLANGCI_LINT_CACHE === env.BOSS_GATE_PRIVATE_LINT_CACHE)
  const started = Date.now()
  const handles = []
  const acquire = (dir, slots) => {
    handles.push(
      acquireGateSlotSync({
        ...config,
        dir,
        slots,
        timeoutMs: Math.max(1, config.timeoutMs - (Date.now() - started)),
        label: 'golangci-lint',
        log: (line) =>
          process.stderr.write(
            `${line.replace('gate-run admission:', 'golangci-lint admission:')} (not a lint finding)\n`,
          ),
      }),
    )
  }
  try {
    // Shared-cache callers serialize before consuming a host analysis slot.
    if (!privateCache) acquire(path.join(config.dir, 'shared'), 1)
    acquire(config.dir, config.slots)
    const hasWidth = args.some((arg) => /^-j(?:=|\d|$)|^--concurrency(?:=|$)/.test(arg))
    return spawnSync(
      'golangci-lint',
      [
        ...args,
        privateCache ? '--allow-parallel-runners' : '--allow-serial-runners',
        ...(!hasWidth
          ? ['--concurrency', String(Math.max(1, Math.floor(config.cpus / config.slots)))]
          : []),
      ],
      { ...spawnOptions, env, encoding: 'utf8' },
    )
  } catch (error) {
    if (error.code !== 'GATE_ADMISSION_TIMEOUT') throw error
    const stderr = `${GOLANGCI_ADMISSION_TIMEOUT_SIGNATURE} ${config.timeoutMs}ms (not a lint finding)\n`
    process.stderr.write(stderr)
    return {
      status: 1,
      signal: null,
      stdout: '',
      stderr,
      error: undefined,
      pid: 0,
      output: [null, '', stderr],
    }
  } finally {
    for (const handle of handles.reverse()) releaseGateSlot(handle)
  }
}
