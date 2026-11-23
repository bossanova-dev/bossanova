#!/usr/bin/env node

// Dev supervisor for `make serve` and `boss session serve`.
// Runs bosso (with Go hot reload), web (Vite), marketing (Astro) and docs
// (Docusaurus) in parallel, wires web to the running bosso instance, allocates
// collision-free ports across concurrent worktrees, exposes HTTPS links via
// foreground `tailscale serve` proxies into `$BOSS_SERVICES_FILE`, and tears
// down every child process group cleanly on SIGINT/SIGTERM.

import { execFileSync, spawn } from 'node:child_process'
import fs from 'node:fs'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { startDevBossd } from './serve-bossd.mjs'
import {
  parseTailscaleServePorts,
  parseTailscaleStatus,
  pickTailscalePort,
  reserveServicePorts,
} from './serve-ports.mjs'

export { parseTailscaleServePorts, parseTailscaleStatus, pickTailscalePort }

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

export const ALL_SERVICES = /** @type {const} */ (['bosso', 'web', 'marketing', 'docs'])
const VALID_SERVICES = new Set(ALL_SERVICES)
const VALID_TUNNELS = new Set(['auto', 'tailscale', 'none'])
const VALID_AUTH_MODES = new Set(['dev', 'workos'])

/** @type {Record<string, { dir: string, localPort: number, httpsPort: number }>} */
const SERVICE_META = {
  bosso: { dir: 'services/bosso', localPort: 8080, httpsPort: 18080 },
  web: { dir: 'services/web', localPort: 5173, httpsPort: 15173 },
  marketing: { dir: 'services/marketing', localPort: 4321, httpsPort: 14321 },
  docs: { dir: 'services/docs', localPort: 3001, httpsPort: 13001 },
}

const HELP = `Usage: node scripts/dev/serve.mjs [options]

Options:
  --services <csv>   Comma-separated subset of: bosso,web,marketing,docs (default: all present)
  --tunnel <mode>    HTTPS exposure: auto (default), tailscale, or none
  --auth <mode>      Auth wiring for web+bosso: dev (default, auto-login) or workos
  --no-watch         Disable Go file-watcher hot reload for bosso
  --help             Show this help
`

/**
 * Parse and validate CLI arguments.
 * @param {string[]} [argv]
 */
export function parseServeArgs(argv = process.argv.slice(2)) {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: false,
    strict: true,
    options: {
      services: { type: 'string' },
      tunnel: { type: 'string', default: 'auto' },
      auth: { type: 'string', default: 'dev' },
      'no-watch': { type: 'boolean', default: false },
      help: { type: 'boolean', default: false },
    },
  })
  if (positionals.length > 0) {
    throw new Error(`unexpected positional arguments: ${positionals.join(' ')}`)
  }
  const tunnel = values.tunnel ?? 'auto'
  if (!VALID_TUNNELS.has(tunnel)) {
    throw new Error(`invalid --tunnel "${tunnel}" (expected auto, tailscale, or none)`)
  }
  const auth = values.auth ?? 'dev'
  if (!VALID_AUTH_MODES.has(auth)) {
    throw new Error(`invalid --auth "${auth}" (expected dev or workos)`)
  }
  /** @type {string[]} */
  let services = [...ALL_SERVICES]
  if (values.services !== undefined) {
    services = []
    for (const raw of values.services.split(',')) {
      const name = raw.trim()
      if (!name) continue
      if (!VALID_SERVICES.has(/** @type {any} */ (name))) {
        throw new Error(`unknown service "${name}" (expected: ${ALL_SERVICES.join(', ')})`)
      }
      if (!services.includes(name)) services.push(name)
    }
    if (services.length === 0) {
      throw new Error('--services must name at least one service')
    }
  }
  return {
    services,
    tunnel,
    auth,
    watch: !values['no-watch'],
    help: Boolean(values.help),
  }
}

/**
 * Filter requested services down to directories that exist in `repoRoot`.
 * @param {string} repoRoot
 * @param {string[]} requested
 * @returns {string[]}
 */
export function availableServices(repoRoot, requested) {
  return requested.filter((name) => {
    const meta = SERVICE_META[name]
    return Boolean(meta && fs.existsSync(path.join(repoRoot, meta.dir)))
  })
}

/**
 * Build child-process environment variables for a given service.
 * @param {{
 *   service: string,
 *   localPorts: Record<string, number>,
 *   publicUrls: Record<string, string>,
 *   authMode: string,
 *   databaseUrl?: string,
 *   migrationsDatabaseUrl?: string,
 *   tailscaleHost?: string,
 *   baseEnv?: NodeJS.ProcessEnv,
 * }} opts
 * @returns {NodeJS.ProcessEnv}
 */
export function buildServiceEnv({
  service,
  localPorts,
  publicUrls,
  authMode,
  databaseUrl = '',
  migrationsDatabaseUrl = '',
  tailscaleHost = '',
  baseEnv = process.env,
}) {
  /** @type {NodeJS.ProcessEnv} */
  const env = { ...baseEnv }
  const allowedHosts = tailscaleHost ? `.ts.net,${tailscaleHost}` : ''

  if (service === 'bosso') {
    const webOrigins = []
    if (localPorts.web) {
      webOrigins.push(`http://127.0.0.1:${localPorts.web}`, `http://localhost:${localPorts.web}`)
    }
    if (publicUrls.web && !webOrigins.includes(publicUrls.web)) {
      webOrigins.push(publicUrls.web)
    }
    if (webOrigins.length === 0) {
      webOrigins.push('http://127.0.0.1:5173', 'http://localhost:5173')
    }
    const wsHosts = webOrigins
      .map((origin) => {
        try {
          return new URL(origin).host
        } catch {
          return ''
        }
      })
      .filter(Boolean)

    env.BOSSO_ADDR = `127.0.0.1:${localPorts.bosso}`
    env.BOSSO_DB_DRIVER = 'postgres'
    env.BOSSO_DATABASE_URL = databaseUrl
    env.BOSSO_MIGRATIONS_DATABASE_URL = migrationsDatabaseUrl || databaseUrl
    env.BOSSO_MULTI_INSTANCE = ''
    env.BOSSO_CORS_ORIGINS = webOrigins.join(',')
    env.BOSSO_WS_ALLOWED_ORIGINS = wsHosts.join(',')
    env.BOSSO_SENTRY_DSN = ''
    if (authMode === 'dev') {
      env.BOSS_ENV = 'development'
      env.BOSS_E2E_TEST_AUTH = '1'
      // Clear WorkOS + Stripe credentials in dev-auth mode so the periodic
      // WorkOS reconciler never queries real WorkOS for the synthetic `e2e-org`
      // and evicts `e2e-user` / revokes the local dev daemon token (and bosso
      // does not require BOSSO_WORKOS_API_KEY for cloud billing).
      env.BOSSO_WORKOS_API_KEY = ''
      env.BOSSO_WORKOS_CLIENT_ID = ''
      env.BOSSO_WORKOS_WEBHOOK_SECRET = ''
      env.BOSSO_STRIPE_SECRET_KEY = ''
      env.BOSSO_STRIPE_CLOUD_PRICE_ID = ''
    } else {
      env.BOSS_ENV = baseEnv.BOSS_ENV || 'development'
      delete env.BOSS_E2E_TEST_AUTH
    }
    return env
  }

  if (service === 'web') {
    env.BOSS_WEB_PORT = String(localPorts.web)
    if (allowedHosts) {
      env.BOSS_WEB_ALLOWED_HOSTS = allowedHosts
    }
    if (publicUrls.bosso) {
      env.VITE_API_BASE_URL = publicUrls.bosso
    }
    if (authMode === 'dev') {
      env.VITE_E2E_REAL = '1'
    } else {
      delete env.VITE_E2E_REAL
    }
    return env
  }

  if (service === 'marketing') {
    if (allowedHosts) {
      env.BOSS_MARKETING_ALLOWED_HOSTS = allowedHosts
    }
    if (publicUrls.marketing) {
      env.PUBLIC_SITE_URL = publicUrls.marketing
    }
    return env
  }

  if (service === 'docs') {
    if (publicUrls.docs) {
      env.PUBLIC_DOCS_URL = publicUrls.docs
    }
    return env
  }

  return env
}

/**
 * Atomically write `$BOSS_SERVICES_FILE` as a sorted JSON object of HTTPS URLs.
 * @param {string} filePath
 * @param {Record<string, string>} entries
 */
export function writeServicesFileAtomic(filePath, entries) {
  /** @type {Record<string, string>} */
  const sorted = {}
  for (const key of Object.keys(entries).sort()) {
    const href = entries[key]
    if (typeof href === 'string' && href.startsWith('https://')) {
      sorted[key] = href
    }
  }
  const dir = path.dirname(filePath)
  fs.mkdirSync(dir, { recursive: true })
  const tmp = path.join(dir, `.${path.basename(filePath)}.tmp-${process.pid}-${Date.now()}`)
  const payload = `${JSON.stringify(sorted, null, 2)}\n`
  try {
    fs.writeFileSync(tmp, payload, { mode: 0o600, flag: 'wx' })
    fs.renameSync(tmp, filePath)
  } catch (err) {
    fs.rmSync(tmp, { force: true })
    throw err
  }
}

/**
 * Send a signal to a detached child's entire POSIX process group.
 * @param {number} pid
 * @param {NodeJS.Signals} signal
 */
export function signalProcessGroup(pid, signal) {
  try {
    process.kill(-pid, signal)
  } catch (err) {
    if (/** @type {NodeJS.ErrnoException} */ (err).code !== 'ESRCH') {
      try {
        process.kill(pid, signal)
      } catch {
        // Process already gone.
      }
    }
  }
}

/**
 * Manages detached child process groups with tagged logs and clean shutdown.
 */
export class ProcessSupervisor {
  /** @param {{ write?: (line: string) => void }} [io] */
  constructor(io = {}) {
    this.write = io.write ?? ((line) => process.stdout.write(line))
    /** @type {Map<string, { child: import('node:child_process').ChildProcess, exited: Promise<void> }>} */
    this.entries = new Map()
    this.stopping = false
  }

  /**
   * Prefix and forward a readable stream line-by-line.
   * @param {string} label
   * @param {import('node:stream').Readable | null} stream
   */
  pipeTagged(label, stream) {
    if (!stream) return
    let buffer = ''
    stream.setEncoding('utf8')
    stream.on('data', (chunk) => {
      buffer += chunk
      const lines = buffer.split(/\r?\n/)
      buffer = lines.pop() ?? ''
      for (const line of lines) {
        if (line.length > 0) this.write(`[${label}] ${line}\n`)
      }
    })
    stream.on('end', () => {
      if (buffer.length > 0) this.write(`[${label}] ${buffer}\n`)
    })
  }

  /**
   * Spawn a managed command in its own process group.
   * @param {string} key
   * @param {string} label
   * @param {string} command
   * @param {string[]} args
   * @param {{ cwd?: string, env?: NodeJS.ProcessEnv, onUnexpectedExit?: (code: number | null, signal: NodeJS.Signals | null) => void }} [opts]
   */
  spawn(key, label, command, args, opts = {}) {
    const child = spawn(command, args, {
      cwd: opts.cwd,
      env: opts.env,
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    this.pipeTagged(label, child.stdout)
    this.pipeTagged(label, child.stderr)

    const exited = new Promise((resolve) => {
      child.once('exit', (code, signal) => {
        if (this.entries.get(key)?.child === child) {
          this.entries.delete(key)
        }
        resolve(undefined)
        if (!this.stopping && opts.onUnexpectedExit) {
          opts.onUnexpectedExit(code, signal)
        }
      })
      child.once('error', (err) => {
        this.write(`[${label}] failed to start: ${err.message}\n`)
        if (this.entries.get(key)?.child === child) {
          this.entries.delete(key)
        }
        resolve(undefined)
      })
    })

    this.entries.set(key, { child, exited })
    return { child, exited }
  }

  /**
   * Stop a single managed process group gracefully, escalating to SIGKILL.
   * @param {string} key
   * @param {number} [graceMs]
   */
  async stopOne(key, graceMs = 3000) {
    const entry = this.entries.get(key)
    if (!entry) return
    const { child, exited } = entry
    if (child.pid && child.exitCode === null && child.signalCode === null) {
      signalProcessGroup(child.pid, 'SIGTERM')
      const timer = setTimeout(() => {
        if (child.pid && child.exitCode === null && child.signalCode === null) {
          signalProcessGroup(child.pid, 'SIGKILL')
        }
      }, graceMs)
      timer.unref()
      await exited
      clearTimeout(timer)
    }
  }

  /**
   * Terminate every managed process group in parallel.
   * @param {number} [graceMs]
   */
  async stopAll(graceMs = 3000) {
    this.stopping = true
    const keys = [...this.entries.keys()]
    await Promise.all(keys.map((key) => this.stopOne(key, graceMs)))
  }
}

/**
 * Probe a local HTTP endpoint until it responds (any HTTP status < 500 or TCP open).
 * @param {number} port
 * @param {{ path?: string, timeoutMs?: number, shouldStop?: () => boolean }} [opts]
 * @returns {Promise<boolean>}
 */
function waitForHttp(port, opts = {}) {
  const probePath = opts.path ?? '/'
  const timeoutMs = opts.timeoutMs ?? 120_000
  const shouldStop = opts.shouldStop ?? (() => false)
  const deadline = Date.now() + timeoutMs

  return new Promise((resolve) => {
    const attempt = () => {
      if (shouldStop() || Date.now() > deadline) {
        resolve(false)
        return
      }
      const req = http.get({ host: '127.0.0.1', port, path: probePath, timeout: 1500 }, (res) => {
        res.resume()
        if ((res.statusCode ?? 500) < 500) {
          resolve(true)
        } else {
          setTimeout(attempt, 400)
        }
      })
      req.on('error', () => setTimeout(attempt, 400))
      req.on('timeout', () => {
        req.destroy()
        setTimeout(attempt, 400)
      })
    }
    attempt()
  })
}

/**
 * Query local Tailscale status and currently bound Serve ports.
 */
function probeTailscale() {
  try {
    const statusOut = execFileSync('tailscale', ['status', '--json'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      timeout: 5000,
    })
    const status = parseTailscaleStatus(statusOut)
    let usedPorts = new Set()
    if (status.running) {
      try {
        const serveOut = execFileSync('tailscale', ['serve', 'status', '--json'], {
          encoding: 'utf8',
          stdio: ['ignore', 'pipe', 'ignore'],
          timeout: 5000,
        })
        usedPorts = parseTailscaleServePorts(serveOut)
      } catch {
        // No serve config yet.
      }
    }
    return { installed: true, ...status, usedPorts }
  } catch {
    return {
      installed: false,
      running: false,
      dnsName: '',
      httpsEnabled: false,
      usedPorts: new Set(),
    }
  }
}

/**
 * Resolve Postgres connection URLs for bosso (respecting BOSSO_DATABASE_URL or
 * provisioning this worktree's own local Postgres via test-postgres.mjs).
 * @param {string} repoRoot
 * @param {NodeJS.ProcessEnv} baseEnv
 */
function resolveBossoDatabase(repoRoot, baseEnv) {
  const existing = (baseEnv.BOSSO_DATABASE_URL ?? '').trim()
  if (existing) {
    const migrations = (baseEnv.BOSSO_MIGRATIONS_DATABASE_URL ?? '').trim() || existing
    return { databaseUrl: existing, migrationsDatabaseUrl: migrations }
  }
  process.stdout.write('[serve] Ensuring local Postgres for bosso…\n')
  const url = execFileSync(
    process.execPath,
    [path.join(repoRoot, 'scripts/test-infra/test-postgres.mjs'), 'ensure', '--print', 'bosso-url'],
    { cwd: repoRoot, encoding: 'utf8', env: baseEnv, stdio: ['ignore', 'pipe', 'inherit'] },
  ).trim()
  if (!url) {
    throw new Error('test-postgres.mjs did not return a Postgres URL for bosso')
  }
  return { databaseUrl: url, migrationsDatabaseUrl: url }
}

/**
 * Build the bosso Go binary into `outPath`.
 * @param {string} repoRoot
 * @param {string} outPath
 * @param {NodeJS.ProcessEnv} env
 * @returns {Promise<boolean>}
 */
function buildBossoBinary(repoRoot, outPath, env) {
  return new Promise((resolve) => {
    const child = spawn('go', ['build', '-mod=readonly', '-o', outPath, './cmd'], {
      cwd: path.join(repoRoot, 'services/bosso'),
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    child.stdout?.on('data', (chunk) => process.stdout.write(`[bosso:build] ${chunk}`))
    child.stderr?.on('data', (chunk) => process.stderr.write(`[bosso:build] ${chunk}`))
    child.once('exit', (code) => resolve(code === 0))
    child.once('error', (err) => {
      process.stderr.write(`[bosso:build] failed to run go build: ${err.message}\n`)
      resolve(false)
    })
  })
}

/**
 * Watch Go sources for bosso and rebuild+restart on edits.
 * @param {{
 *   repoRoot: string,
 *   binPath: string,
 *   env: NodeJS.ProcessEnv,
 *   supervisor: ProcessSupervisor,
 *   isShuttingDown: () => boolean,
 * }} opts
 */
function startBossoWatcher({ repoRoot, binPath, env, supervisor, isShuttingDown }) {
  const watchDirs = ['services/bosso', 'lib/bossalib']
    .map((rel) => path.join(repoRoot, rel))
    .filter((dir) => fs.existsSync(dir))

  /** @type {fs.FSWatcher[]} */
  const watchers = []
  /** @type {ReturnType<typeof setTimeout> | null} */
  let debounceTimer = null
  let rebuilding = false
  let pendingRebuild = false

  const triggerRebuild = async () => {
    if (isShuttingDown()) return
    if (rebuilding) {
      pendingRebuild = true
      return
    }
    rebuilding = true
    pendingRebuild = false
    try {
      process.stdout.write('[bosso] Source change detected — rebuilding…\n')
      const nextBin = `${binPath}.next`
      const ok = await buildBossoBinary(repoRoot, nextBin, env)
      if (!ok || isShuttingDown()) {
        fs.rmSync(nextBin, { force: true })
        if (!isShuttingDown()) {
          process.stderr.write('[bosso] Build failed; keeping previous instance running\n')
        }
        return
      }
      await supervisor.stopOne('bosso')
      if (isShuttingDown()) {
        fs.rmSync(nextBin, { force: true })
        return
      }
      fs.renameSync(nextBin, binPath)
      supervisor.spawn('bosso', 'bosso', binPath, [], {
        cwd: path.join(repoRoot, 'services/bosso'),
        env,
      })
      process.stdout.write('[bosso] Reloaded\n')
    } finally {
      rebuilding = false
      if (pendingRebuild && !isShuttingDown()) {
        void triggerRebuild()
      }
    }
  }

  for (const dir of watchDirs) {
    try {
      const watcher = fs.watch(dir, { recursive: true }, (_event, filename) => {
        if (!filename) return
        const name = String(filename)
        if (!(name.endsWith('.go') || name.endsWith('.sql')) || name.endsWith('_test.go')) {
          return
        }
        if (debounceTimer) clearTimeout(debounceTimer)
        debounceTimer = setTimeout(() => {
          debounceTimer = null
          void triggerRebuild()
        }, 300)
      })
      watchers.push(watcher)
    } catch (err) {
      process.stderr.write(
        `[bosso] watch unavailable for ${dir}: ${/** @type {Error} */ (err).message}\n`,
      )
    }
  }

  return () => {
    if (debounceTimer) clearTimeout(debounceTimer)
    for (const watcher of watchers) watcher.close()
  }
}

/**
 * Resolve a local service binary inside `<serviceDir>/node_modules/.bin/<name>`.
 * @param {string} serviceDir
 * @param {string} binName
 */
function serviceBin(serviceDir, binName) {
  const local = path.join(serviceDir, 'node_modules', '.bin', binName)
  if (!fs.existsSync(local)) {
    throw new Error(
      `missing ${local} — run \`pnpm install\` (and \`make -C services/docs deps\` for docs)`,
    )
  }
  return local
}

/**
 * Return true when semver `version` (e.g. "22.23.4" or "v26.13.0") is >= `min`.
 * @param {string} version
 * @param {[number, number, number]} min
 */
export function meetsMinNodeVersion(version, min) {
  const parts = version
    .replace(/^v/, '')
    .split('.')
    .map((n) => Number.parseInt(n, 10))
  if (parts.length < 3 || parts.some((n) => !Number.isInteger(n))) return false
  for (let i = 0; i < 3; i++) {
    if (parts[i] !== min[i]) return parts[i] > min[i]
  }
  return true
}

/**
 * Astro 7 requires Node >= 22.23.4; if the repo's `.node-version` shim is
 * older, prefer a newer system Node (e.g. Homebrew) on PATH for marketing.
 * @param {NodeJS.ProcessEnv} env
 * @returns {NodeJS.ProcessEnv}
 */
function ensureAstroNodeInPath(env) {
  if (meetsMinNodeVersion(process.versions.node, [22, 23, 4])) {
    return env
  }
  for (const binDir of ['/opt/homebrew/bin', '/usr/local/bin']) {
    const candidate = path.join(binDir, 'node')
    if (!fs.existsSync(candidate)) continue
    try {
      const ver = execFileSync(candidate, ['-v'], { encoding: 'utf8', timeout: 5000 }).trim()
      if (meetsMinNodeVersion(ver, [22, 23, 4])) {
        return { ...env, PATH: `${binDir}:${env.PATH ?? ''}` }
      }
    } catch {
      // Ignore unusable candidate.
    }
  }
  return env
}

/**
 * Run pre-dev scripts for marketing (robots.txt + install.sh copy).
 * @param {string} marketingDir
 * @param {NodeJS.ProcessEnv} env
 */
function prepareMarketing(marketingDir, env) {
  for (const script of ['scripts/generate-robots.mjs', 'scripts/copy-install-sh.mjs']) {
    const full = path.join(marketingDir, script)
    if (fs.existsSync(full)) {
      execFileSync(process.execPath, [full], { cwd: marketingDir, env, stdio: 'inherit' })
    }
  }
}

/**
 * Main entrypoint.
 * @param {string[]} [argv]
 */
export async function main(argv = process.argv.slice(2)) {
  let opts
  try {
    opts = parseServeArgs(argv)
  } catch (err) {
    process.stderr.write(`serve: ${/** @type {Error} */ (err).message}\n${HELP}`)
    return 2
  }
  if (opts.help) {
    process.stdout.write(HELP)
    return 0
  }

  const services = availableServices(REPO_ROOT, opts.services)
  if (services.length === 0) {
    process.stderr.write(
      'serve: none of the requested service directories exist in this checkout\n',
    )
    return 1
  }

  const servicesFile = (process.env.BOSS_SERVICES_FILE ?? '').trim()
  const ts = opts.tunnel === 'none' ? null : probeTailscale()
  let useTailscale = false
  if (opts.tunnel === 'tailscale') {
    if (!ts?.running || !ts.httpsEnabled) {
      process.stderr.write(
        'serve: --tunnel=tailscale requires Tailscale running with HTTPS Certificates enabled.\n' +
          'Enable MagicDNS + HTTPS Certificates in Tailscale Admin -> DNS, then retry.\n',
      )
      return 1
    }
    useTailscale = true
  } else if (opts.tunnel === 'auto') {
    if (ts?.running && ts.httpsEnabled) {
      useTailscale = true
    } else if (servicesFile) {
      const reason = !ts?.installed
        ? 'tailscale CLI not found'
        : !ts.running
          ? 'tailscale is not connected'
          : `HTTPS Certificates are not enabled for ${ts.dnsName}`
      process.stderr.write(
        `serve: BOSS_SERVICES_FILE requires https:// links, but ${reason}.\n` +
          'Enable MagicDNS + HTTPS Certificates in Tailscale Admin -> DNS (https://login.tailscale.com/admin/dns), then retry.\n',
      )
      return 1
    } else if (ts?.running && !ts.httpsEnabled) {
      process.stdout.write(
        `[serve] Tailscale is running (${ts.dnsName}), but HTTPS Certificates are off — serving on localhost.\n` +
          '[serve] Enable HTTPS Certificates in Tailscale Admin -> DNS for iOS / remote HTTPS previews.\n',
      )
    }
  }

  const tailscaleHost = useTailscale && ts ? ts.dnsName : ''
  const {
    localPorts,
    httpsPorts,
    publicUrls,
    release: releasePorts,
  } = await reserveServicePorts({
    services,
    serviceMeta: SERVICE_META,
    useTailscale,
    tailscaleHost,
    usedTsPorts: ts?.usedPorts ?? [],
    worktree: REPO_ROOT,
  })

  let databaseUrl = ''
  let migrationsDatabaseUrl = ''
  if (services.includes('bosso')) {
    try {
      const db = resolveBossoDatabase(REPO_ROOT, process.env)
      databaseUrl = db.databaseUrl
      migrationsDatabaseUrl = db.migrationsDatabaseUrl
    } catch (err) {
      await releasePorts()
      throw err
    }
  }

  const supervisor = new ProcessSupervisor()
  let shuttingDown = false
  let stopWatcher = () => {}
  const buildDir = fs.mkdtempSync(path.join(os.tmpdir(), 'bossanova-serve-'))
  /** @type {Record<string, string>} */
  const publishedLinks = {}

  /** @param {string} reason @param {number} exitCode */
  const shutdown = async (reason, exitCode) => {
    if (shuttingDown) return
    shuttingDown = true
    process.stdout.write(`\n[serve] Stopping (${reason})…\n`)
    stopWatcher()
    await supervisor.stopAll()
    await releasePorts()
    fs.rmSync(buildDir, { recursive: true, force: true, maxRetries: 3 })
    process.exit(exitCode)
  }

  process.on('SIGINT', () => void shutdown('SIGINT', 0))
  process.on('SIGTERM', () => void shutdown('SIGTERM', 0))
  process.on('SIGHUP', () => void shutdown('SIGHUP', 0))

  const publishReady = (/** @type {string} */ name) => {
    if (shuttingDown) return
    const url = publicUrls[name]
    process.stdout.write(
      `[serve] READY ${name.padEnd(10)} ${url} (local http://127.0.0.1:${localPorts[name]})\n`,
    )
    if (useTailscale && servicesFile) {
      publishedLinks[name] = url
      writeServicesFileAtomic(servicesFile, publishedLinks)
    }
  }

  const startTunnel = (/** @type {string} */ name) => {
    if (!useTailscale) return
    supervisor.spawn(
      `ts:${name}`,
      `tailscale:${name}`,
      'tailscale',
      ['serve', `--https=${httpsPorts[name]}`, `http://127.0.0.1:${localPorts[name]}`],
      { cwd: REPO_ROOT, env: process.env },
    )
  }

  const portSummary = services.map((name) => `${name}=${localPorts[name]}`).join(', ')
  process.stdout.write(
    `[serve] Starting ${services.join(', ')} (auth=${opts.auth}, tunnel=${useTailscale ? `tailscale:${tailscaleHost}` : 'local'}, ports: ${portSummary})\n`,
  )

  try {
    /** @type {Promise<void>[]} */
    const bootTasks = []

    if (services.includes('bosso')) {
      const bossoEnv = buildServiceEnv({
        service: 'bosso',
        localPorts,
        publicUrls,
        authMode: opts.auth,
        databaseUrl,
        migrationsDatabaseUrl,
        tailscaleHost,
      })
      const bossoBin = path.join(buildDir, 'bosso')
      bootTasks.push(
        (async () => {
          process.stdout.write('[bosso] Building…\n')
          const ok = await buildBossoBinary(REPO_ROOT, bossoBin, bossoEnv)
          if (!ok) {
            throw new Error('initial bosso build failed')
          }
          if (shuttingDown) return
          supervisor.spawn('bosso', 'bosso', bossoBin, [], {
            cwd: path.join(REPO_ROOT, 'services/bosso'),
            env: bossoEnv,
          })
          if (opts.watch) {
            stopWatcher = startBossoWatcher({
              repoRoot: REPO_ROOT,
              binPath: bossoBin,
              env: bossoEnv,
              supervisor,
              isShuttingDown: () => shuttingDown,
            })
          }
          const ready = await waitForHttp(localPorts.bosso, {
            path: '/healthz',
            shouldStop: () => shuttingDown,
          })
          if (ready && !shuttingDown) {
            startTunnel('bosso')
            publishReady('bosso')
            if (opts.auth === 'dev') {
              await startDevBossd({
                repoRoot: REPO_ROOT,
                buildDir,
                bossoPort: localPorts.bosso,
                supervisor,
                isShuttingDown: () => shuttingDown,
              })
            }
          }
        })(),
      )
    }

    if (services.includes('web')) {
      const webDir = path.join(REPO_ROOT, 'services/web')
      const webEnv = buildServiceEnv({
        service: 'web',
        localPorts,
        publicUrls,
        authMode: opts.auth,
        tailscaleHost,
      })
      const viteBin = serviceBin(webDir, 'vite')
      supervisor.spawn(
        'web',
        'web',
        viteBin,
        ['--host', '127.0.0.1', '--port', String(localPorts.web), '--strictPort'],
        { cwd: webDir, env: webEnv },
      )
      bootTasks.push(
        (async () => {
          const ready = await waitForHttp(localPorts.web, { shouldStop: () => shuttingDown })
          if (ready && !shuttingDown) {
            startTunnel('web')
            publishReady('web')
          }
        })(),
      )
    }

    if (services.includes('marketing')) {
      const marketingDir = path.join(REPO_ROOT, 'services/marketing')
      const marketingEnv = ensureAstroNodeInPath(
        buildServiceEnv({
          service: 'marketing',
          localPorts,
          publicUrls,
          authMode: opts.auth,
          tailscaleHost,
        }),
      )
      prepareMarketing(marketingDir, marketingEnv)
      const astroBin = serviceBin(marketingDir, 'astro')
      // `--ignore-lock` keeps Astro 7 in the foreground (disables agent
      // auto-backgrounding) so supervisor teardown stops it cleanly.
      supervisor.spawn(
        'marketing',
        'marketing',
        astroBin,
        ['dev', '--ignore-lock', '--host', '127.0.0.1', '--port', String(localPorts.marketing)],
        { cwd: marketingDir, env: marketingEnv },
      )
      bootTasks.push(
        (async () => {
          const ready = await waitForHttp(localPorts.marketing, {
            shouldStop: () => shuttingDown,
          })
          if (ready && !shuttingDown) {
            startTunnel('marketing')
            publishReady('marketing')
          }
        })(),
      )
    }

    if (services.includes('docs')) {
      const docsDir = path.join(REPO_ROOT, 'services/docs')
      const docsEnv = buildServiceEnv({
        service: 'docs',
        localPorts,
        publicUrls,
        authMode: opts.auth,
        tailscaleHost,
      })
      const docusaurusBin = serviceBin(docsDir, 'docusaurus')
      supervisor.spawn(
        'docs',
        'docs',
        docusaurusBin,
        ['start', '--host', '127.0.0.1', '--port', String(localPorts.docs), '--no-open'],
        { cwd: docsDir, env: docsEnv },
      )
      bootTasks.push(
        (async () => {
          const ready = await waitForHttp(localPorts.docs, { shouldStop: () => shuttingDown })
          if (ready && !shuttingDown) {
            startTunnel('docs')
            publishReady('docs')
          }
        })(),
      )
    }

    await Promise.all(bootTasks)
    if (!shuttingDown) {
      process.stdout.write('[serve] All services ready. Press Ctrl+C to stop.\n')
    }
    // Keep alive until signaled.
    await new Promise(() => {})
    return 0
  } catch (err) {
    process.stderr.write(`[serve] Fatal: ${/** @type {Error} */ (err).message}\n`)
    await shutdown('error', 1)
    return 1
  }
}

if (isMainModule(import.meta.url)) {
  main().then((code) => {
    if (typeof code === 'number' && code !== 0) process.exitCode = code
  })
}
