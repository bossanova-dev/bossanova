// Cross-process port allocation and lease registry for `scripts/dev/serve.mjs`.
//
// Multiple worktrees can start `make serve` at the same time. Because `bosso`
// compiles before binding its TCP port (and Docusaurus/Astro warm up before
// listening, and Tailscale Serve binds only after readiness), probing sockets
// alone has a wide TOCTOU window. A lock-guarded lease file in `os.tmpdir()`
// reserves both local loopback ports and Tailscale HTTPS ports across live
// `serve.mjs` processes until shutdown (or process exit, pruned via PID check).

import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'

const DEFAULT_LEASE_FILE = path.join(os.tmpdir(), 'bossanova-serve-leases.json')
const LOCK_STALE_MS = 15_000
const LOCK_TIMEOUT_MS = 10_000
const STEP_ATTEMPTS = 50

/**
 * @typedef {{
 *   pid: number,
 *   worktree: string,
 *   localPorts: Record<string, number>,
 *   httpsPorts: Record<string, number>,
 *   startedAt: string,
 * }} PortLease
 */

/**
 * Check whether a POSIX PID is currently alive.
 * @param {number} pid
 * @returns {boolean}
 */
export function isPidAlive(pid) {
  if (!Number.isInteger(pid) || pid <= 0) return false
  try {
    process.kill(pid, 0)
    return true
  } catch (err) {
    return /** @type {NodeJS.ErrnoException} */ (err).code === 'EPERM'
  }
}

/**
 * Test whether a TCP port is free on a specific bind host.
 * Treats IPv6-unavailable errors (`EADDRNOTAVAIL`, `EAFNOSUPPORT`) as free.
 * @param {number} port
 * @param {string} host
 * @returns {Promise<boolean>}
 */
function isHostPortFree(port, host) {
  return new Promise((resolve) => {
    const srv = net.createServer()
    srv.once('error', (err) => {
      const code = /** @type {NodeJS.ErrnoException} */ (err).code
      if (host === '::1' && (code === 'EADDRNOTAVAIL' || code === 'EAFNOSUPPORT')) {
        resolve(true)
        return
      }
      resolve(false)
    })
    srv.listen(port, host, () => srv.close(() => resolve(true)))
  })
}

/**
 * Check whether a loopback TCP port is free on both IPv4 (`127.0.0.1`) and
 * IPv6 (`::1`) so `localhost` and `127.0.0.1` never collide with another
 * listener.
 * @param {number} port
 * @returns {Promise<boolean>}
 */
export async function isLoopbackPortFree(port) {
  const [v4, v6] = await Promise.all([
    isHostPortFree(port, '127.0.0.1'),
    isHostPortFree(port, '::1'),
  ])
  return v4 && v6
}

/**
 * Allocate an ephemeral free loopback TCP port.
 * @returns {Promise<number>}
 */
function allocateEphemeralPort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.once('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address()
      const port = typeof addr === 'object' && addr ? addr.port : 0
      srv.close(() => resolve(port))
    })
  })
}

/**
 * Allocate a free local loopback port, trying `preferred` first, then stepping
 * sequentially (`preferred + 1`, `preferred + 2`, ...) before falling back to
 * an OS ephemeral port.
 * @param {number} preferred
 * @param {Set<number>} reserved
 * @param {(port: number) => Promise<boolean>} [probePort]
 * @returns {Promise<number>}
 */
export async function allocateLocalPort(preferred, reserved, probePort = isLoopbackPortFree) {
  for (let offset = 0; offset < STEP_ATTEMPTS; offset++) {
    const candidate = preferred + offset
    if (candidate > 65_535) break
    if (!reserved.has(candidate) && (await probePort(candidate))) {
      reserved.add(candidate)
      return candidate
    }
  }
  for (let attempt = 0; attempt < 20; attempt++) {
    const port = await allocateEphemeralPort()
    if (!reserved.has(port) && (await probePort(port))) {
      reserved.add(port)
      return port
    }
  }
  throw new Error(`unable to allocate a free local TCP port near ${preferred}`)
}

/**
 * Parse `tailscale status --json` output into a small status object.
 * @param {string} stdout
 */
export function parseTailscaleStatus(stdout) {
  try {
    const data = JSON.parse(stdout)
    const rawDns = typeof data?.Self?.DNSName === 'string' ? data.Self.DNSName : ''
    const dnsName = rawDns.replace(/\.$/, '')
    const certDomains = Array.isArray(data?.CertDomains) ? data.CertDomains.filter(Boolean) : []
    return {
      running: data?.BackendState === 'Running' && dnsName.length > 0,
      dnsName,
      httpsEnabled: certDomains.length > 0,
    }
  } catch {
    return { running: false, dnsName: '', httpsEnabled: false }
  }
}

/**
 * Parse `tailscale serve status --json` to find TCP ports already bound by
 * other sessions on this machine.
 * @param {string} stdout
 * @returns {Set<number>}
 */
export function parseTailscaleServePorts(stdout) {
  /** @type {Set<number>} */
  const ports = new Set()
  try {
    const data = JSON.parse(stdout)
    if (data?.TCP && typeof data.TCP === 'object') {
      for (const key of Object.keys(data.TCP)) {
        const port = Number.parseInt(key, 10)
        if (Number.isInteger(port) && port > 0) ports.add(port)
      }
    }
    if (data?.Web && typeof data.Web === 'object') {
      for (const hostPort of Object.keys(data.Web)) {
        const idx = hostPort.lastIndexOf(':')
        if (idx >= 0) {
          const port = Number.parseInt(hostPort.slice(idx + 1), 10)
          if (Number.isInteger(port) && port > 0) ports.add(port)
        }
      }
    }
  } catch {
    // Empty or non-JSON status means no active routes.
  }
  return ports
}

/**
 * Pick a free Tailscale Serve HTTPS port that does not collide with `usedPorts`.
 * @param {number} preferred
 * @param {Set<number>} usedPorts
 * @returns {number}
 */
export function pickTailscalePort(preferred, usedPorts) {
  let port = preferred
  while (usedPorts.has(port) && port < 65_000) {
    port += 10
  }
  usedPorts.add(port)
  return port
}

/**
 * Acquire an atomic directory lock next to `leaseFile`, recovering stale locks.
 * @param {string} leaseFile
 * @returns {Promise<() => void>}
 */
async function acquireLeaseLock(leaseFile) {
  const lockDir = `${leaseFile}.lock`
  fs.mkdirSync(path.dirname(leaseFile), { recursive: true })
  const deadline = Date.now() + LOCK_TIMEOUT_MS
  while (true) {
    try {
      fs.mkdirSync(lockDir)
      return () => {
        fs.rmSync(lockDir, { recursive: true, force: true, maxRetries: 3 })
      }
    } catch (err) {
      const code = /** @type {NodeJS.ErrnoException} */ (err).code
      if (code !== 'EEXIST') throw err
      try {
        const stat = fs.statSync(lockDir)
        if (Date.now() - stat.mtimeMs > LOCK_STALE_MS) {
          fs.rmSync(lockDir, { recursive: true, force: true, maxRetries: 3 })
          continue
        }
      } catch {
        // Lock disappeared between mkdir and stat; retry immediately.
        continue
      }
      if (Date.now() > deadline) {
        throw new Error(`timed out waiting for serve port lock: ${lockDir}`)
      }
      await new Promise((resolve) => setTimeout(resolve, 25))
    }
  }
}

/**
 * Read and prune live port leases from `leaseFile`.
 * @param {string} leaseFile
 * @param {(pid: number) => boolean} [pidAlive]
 * @returns {PortLease[]}
 */
function readLiveLeases(leaseFile, pidAlive = isPidAlive) {
  if (!fs.existsSync(leaseFile)) return []
  try {
    const raw = JSON.parse(fs.readFileSync(leaseFile, 'utf8'))
    if (!Array.isArray(raw)) return []
    return raw.filter(
      (entry) =>
        entry && typeof entry === 'object' && typeof entry.pid === 'number' && pidAlive(entry.pid),
    )
  } catch {
    return []
  }
}

/**
 * Atomically write the lease array to `leaseFile`.
 * @param {string} leaseFile
 * @param {PortLease[]} leases
 */
function writeLeasesAtomic(leaseFile, leases) {
  const dir = path.dirname(leaseFile)
  fs.mkdirSync(dir, { recursive: true })
  const tmp = path.join(dir, `.${path.basename(leaseFile)}.tmp-${process.pid}-${Date.now()}`)
  try {
    fs.writeFileSync(tmp, `${JSON.stringify(leases, null, 2)}\n`, { mode: 0o600, flag: 'wx' })
    fs.renameSync(tmp, leaseFile)
  } catch (err) {
    fs.rmSync(tmp, { force: true })
    throw err
  }
}

/**
 * Reserve non-conflicting local and (optional) Tailscale HTTPS ports across
 * concurrent `make serve` runs in different worktrees.
 *
 * Returns the allocated maps plus a `release()` function to remove this PID's
 * lease on shutdown.
 *
 * @param {{
 *   services: string[],
 *   serviceMeta: Record<string, { localPort: number, httpsPort: number }>,
 *   useTailscale: boolean,
 *   tailscaleHost: string,
 *   usedTsPorts?: Iterable<number>,
 *   worktree: string,
 *   pid?: number,
 *   leaseFile?: string,
 *   pidAlive?: (pid: number) => boolean,
 *   probePort?: (port: number) => Promise<boolean>,
 * }} opts
 */
export async function reserveServicePorts({
  services,
  serviceMeta,
  useTailscale,
  tailscaleHost,
  usedTsPorts = [],
  worktree,
  pid = process.pid,
  leaseFile = process.env.BOSS_SERVE_LEASE_FILE || DEFAULT_LEASE_FILE,
  pidAlive = isPidAlive,
  probePort = isLoopbackPortFree,
}) {
  const unlock = await acquireLeaseLock(leaseFile)
  try {
    const existing = readLiveLeases(leaseFile, pidAlive).filter((entry) => entry.pid !== pid)
    /** @type {Set<number>} */
    const reservedLocal = new Set()
    /** @type {Set<number>} */
    const reservedHttps = new Set(usedTsPorts)

    for (const lease of existing) {
      for (const port of Object.values(lease.localPorts ?? {})) {
        if (typeof port === 'number') reservedLocal.add(port)
      }
      for (const port of Object.values(lease.httpsPorts ?? {})) {
        if (typeof port === 'number') reservedHttps.add(port)
      }
    }

    /** @type {Record<string, number>} */
    const localPorts = {}
    /** @type {Record<string, number>} */
    const httpsPorts = {}
    /** @type {Record<string, string>} */
    const publicUrls = {}

    for (const name of services) {
      const meta = serviceMeta[name]
      localPorts[name] = await allocateLocalPort(meta.localPort, reservedLocal, probePort)
      if (useTailscale) {
        httpsPorts[name] = pickTailscalePort(meta.httpsPort, reservedHttps)
        publicUrls[name] = `https://${tailscaleHost}:${httpsPorts[name]}`
      } else {
        publicUrls[name] = `http://127.0.0.1:${localPorts[name]}`
      }
    }

    existing.push({
      pid,
      worktree,
      localPorts,
      httpsPorts,
      startedAt: new Date().toISOString(),
    })
    writeLeasesAtomic(leaseFile, existing)

    let released = false
    const release = async () => {
      if (released) return
      released = true
      try {
        const unlockRelease = await acquireLeaseLock(leaseFile)
        try {
          const remaining = readLiveLeases(leaseFile, pidAlive).filter((entry) => entry.pid !== pid)
          writeLeasesAtomic(leaseFile, remaining)
        } finally {
          unlockRelease()
        }
      } catch {
        // Best-effort cleanup on shutdown; dead PIDs are pruned on next start.
      }
    }

    return { localPorts, httpsPorts, publicUrls, release }
  } finally {
    unlock()
  }
}
