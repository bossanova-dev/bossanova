import os from 'node:os'
import { spawn } from 'node:child_process'

const TERM_GRACE_MS = 2000

export function gateTimeoutMs(env = process.env) {
  const configured = Number(env.BOSS_GATE_TIMEOUT_MS)
  return Number.isFinite(configured) && configured > 0 ? configured : 3600000
}

// Gates select the persistent admission-slot pool; explicit caches remain caller-owned.
// Nested gates inherit the marker. Cache lifetime belongs to admission and disk recovery.
export function gateEnvironment(env = process.env) {
  return {
    env: {
      ...env,
      ...(!env.GOLANGCI_LINT_CACHE ? { BOSS_GATE_LINT_CACHE_POOL: '1' } : {}),
    },
  }
}

// The outer owner supervises the whole group. Nested wrappers stay in that group so stop cannot
// strand a detached grandchild. Its deadline includes pipe-holding descendants after leader exit.
export function spawnGate(argv, { stdio, env = process.env, timeoutMs = gateTimeoutMs(env) }) {
  const ownsGroup = env.BOSS_GATE_PROCESS_GROUP !== '1'
  const child = spawn(argv[0], argv.slice(1), {
    stdio,
    detached: ownsGroup,
    env: { ...env, BOSS_GATE_PROCESS_GROUP: '1' },
  })
  let timedOut = false
  let stopping = false
  let cancellationStatus = null
  let killTimer = null
  let timer = null
  let closed = false
  let escalation = null
  const signalGroup = (signal) => {
    if (!child.pid) return
    try {
      process.kill(-child.pid, signal)
    } catch (error) {
      if (error.code !== 'ESRCH') throw error
    }
  }
  const terminate = (signal = 'SIGTERM') => {
    if (stopping || !ownsGroup || closed) return
    stopping = true
    cancellationStatus = 128 + (os.constants.signals[signal] ?? os.constants.signals.SIGTERM)
    signalGroup(signal)
    // Do not cancel escalation when the leader exits: resistant descendants can still own pipes
    // or cache files. Complete escalation before cleanup or a terminal status is written.
    escalation = new Promise((resolve) => {
      killTimer = setTimeout(() => {
        signalGroup('SIGKILL')
        resolve()
      }, TERM_GRACE_MS)
    })
  }
  const listeners = ['SIGINT', 'SIGTERM'].map((signal) => {
    const listener = () => terminate(signal)
    process.on(signal, listener)
    return { signal, listener }
  })
  if (ownsGroup && timeoutMs !== null) {
    timer = setTimeout(() => {
      timedOut = true
      process.stderr.write(`gate wall-clock deadline exceeded (${timeoutMs}ms)\n`)
      terminate()
    }, timeoutMs)
  }
  const result = new Promise((resolve) => {
    let error = null
    child.once('error', (cause) => {
      error = cause
    })
    child.once('close', async (status, signal) => {
      if (ownsGroup && child.pid) {
        // File-descriptor stdio (gate-run logs) closes with the leader, unlike a pipe. Keep the
        // deadline armed until its descendants are gone even on that path.
        for (;;) {
          // After SIGKILL no descendant can retain a cache or lock. Some container init processes
          // leave adopted zombies unreaped, so group existence alone must not extend the deadline.
          if (escalation) {
            await escalation
            break
          }
          try {
            process.kill(-child.pid, 0)
          } catch (cause) {
            if (cause.code === 'ESRCH') break
            throw cause
          }
          await new Promise((done) => setTimeout(done, 25))
        }
      }
      clearTimeout(timer)
      if (escalation) await escalation
      clearTimeout(killTimer)
      closed = true
      for (const { signal, listener } of listeners) process.removeListener(signal, listener)
      resolve({ status: timedOut ? 124 : (cancellationStatus ?? status), signal, error, timedOut })
    })
  })
  return { child, result, terminate }
}
