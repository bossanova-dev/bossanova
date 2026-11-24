// Ephemeral dev `bossd` bootstrap and repo seeder for `make serve`.
//
// In `--auth=dev` mode, `make serve` runs an isolated `bossd` connected to the
// local `bosso` instance (using the same `e2e-test-token` accepted by bosso's
// `BOSS_E2E_TEST_AUTH=1` validator) and registers the current `bossanova`
// repository over `bossd`'s unix socket so the web UI immediately has an
// online daemon and a selectable repository for live testing.

import { execFileSync, spawn } from 'node:child_process'
import fs from 'node:fs'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'

export const DEV_AUTH_TOKEN = 'e2e-test-token'

/** Known local agent-runner plugin binaries and their `GetInfo` runner names. */
const AGENT_PLUGINS = [
  { name: 'claude', bin: 'bossd-plugin-claude' },
  { name: 'codex', bin: 'bossd-plugin-codex' },
  { name: 'opencode', bin: 'bossd-plugin-opencode' },
  { name: 'antigravity', bin: 'bossd-plugin-antigravity' },
  { name: 'stub', bin: 'bossd-plugin-stub-runner' },
]

/**
 * Discover already-built agent runner plugins in `<repoRoot>/bin/`.
 * @param {string} repoRoot
 * @returns {Array<{ name: string, path: string, enabled: boolean }>}
 */
export function discoverBuiltAgentPlugins(repoRoot) {
  const binDir = path.join(repoRoot, 'bin')
  /** @type {Array<{ name: string, path: string, enabled: boolean }>} */
  const plugins = []
  for (const entry of AGENT_PLUGINS) {
    const full = path.join(binDir, entry.bin)
    if (fs.existsSync(full)) {
      plugins.push({ name: entry.name, path: full, enabled: true })
    }
  }
  return plugins
}

/**
 * Derive a clean display name for the seeded repository (e.g. `bossanova` even
 * when running from a worktree directory with a branch-based folder name).
 * @param {string} repoRoot
 * @returns {string}
 */
export function resolveRepoDisplayName(repoRoot) {
  try {
    const origin = execFileSync('git', ['-C', repoRoot, 'remote', 'get-url', 'origin'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      timeout: 3000,
    }).trim()
    const match = origin.match(/\/([^/]+?)(?:\.git)?$/)
    if (match?.[1]) return match[1]
  } catch {
    // Fall back to directory basename when origin is unavailable.
  }
  return path.basename(repoRoot) || 'bossanova'
}

/**
 * Build a Go binary from `moduleDir` into `outPath`.
 * @param {string} moduleDir
 * @param {string} outPath
 * @param {NodeJS.ProcessEnv} env
 * @param {string} label
 * @returns {Promise<boolean>}
 */
function buildGoBinary(moduleDir, outPath, env, label) {
  return new Promise((resolve) => {
    const child = spawn('go', ['build', '-mod=readonly', '-o', outPath, '.'], {
      cwd: moduleDir,
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    child.stdout?.on('data', (chunk) => process.stdout.write(`[${label}:build] ${chunk}`))
    child.stderr?.on('data', (chunk) => process.stderr.write(`[${label}:build] ${chunk}`))
    child.once('exit', (code) => resolve(code === 0))
    child.once('error', (err) => {
      process.stderr.write(`[${label}:build] failed to run go build: ${err.message}\n`)
      resolve(false)
    })
  })
}

/**
 * The GitHub token the developer's own `gh` holds, read with their real HOME
 * before `bossd` gets an isolated one. Null when `gh` is missing or logged out.
 * @param {NodeJS.ProcessEnv} [baseEnv]
 * @param {typeof execFileSync} [run]
 * @returns {string | null}
 */
export function resolveGitHubToken(baseEnv = process.env, run = execFileSync) {
  const given = (baseEnv.GH_TOKEN || baseEnv.GITHUB_TOKEN || '').trim()
  if (given) {
    return given
  }
  try {
    const out = run('gh', ['auth', 'token'], {
      env: baseEnv,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      timeout: 15_000,
    })
    return String(out).trim() || null
  } catch {
    return null
  }
}

/**
 * The developer's git name and email, read with their real HOME, so commits in
 * the dev stack's sessions keep their author. Only the identity crosses: the
 * rest of their global config stays out, because signing (commit.gpgsign with
 * a gpg key) cannot work under the isolated HOME and would fail every commit.
 * @param {NodeJS.ProcessEnv} [baseEnv]
 * @param {typeof execFileSync} [run]
 * @returns {{ name: string, email: string } | null}
 */
export function resolveGitIdentity(baseEnv = process.env, run = execFileSync) {
  const read = (/** @type {string} */ key) => {
    try {
      return String(
        run('git', ['config', '--get', key], {
          env: baseEnv,
          encoding: 'utf8',
          stdio: ['ignore', 'pipe', 'ignore'],
          timeout: 10_000,
        }),
      ).trim()
    } catch {
      return ''
    }
  }
  const name = read('user.name')
  const email = read('user.email')
  return name && email ? { name, email } : null
}

// A github.com credential helper that answers from GH_TOKEN. The empty entry
// before it drops the macOS keychain helper, which an isolated HOME cannot
// reach and which would otherwise leave git prompting for a username.
const GITHUB_TOKEN_HELPER =
  '!f() { test "$1" = get && echo username=x-access-token && echo "password=$GH_TOKEN"; }; f'

/**
 * Appends config entries through git's GIT_CONFIG_COUNT/KEY/VALUE variables,
 * after any the caller's environment already set.
 * @param {NodeJS.ProcessEnv} env
 * @param {readonly [string, string][]} entries
 */
function appendGitConfig(env, entries) {
  const start = Number.parseInt(env.GIT_CONFIG_COUNT ?? '0', 10) || 0
  entries.forEach(([key, value], index) => {
    env[`GIT_CONFIG_KEY_${start + index}`] = key
    env[`GIT_CONFIG_VALUE_${start + index}`] = value
  })
  env.GIT_CONFIG_COUNT = String(start + entries.length)
}

/**
 * Build child-process env for the ephemeral dev `bossd`.
 *
 * HOME is isolated so bossd keeps its own state and never reads a production
 * token, but git and gh must still reach GitHub as the developer: without that
 * every session create fails its base-branch check ("could not read Username
 * for 'https://github.com'") and no clone, fetch, push or PR call can work.
 * The token goes in as GH_TOKEN (read by gh, and by git through the helper
 * above), commits carry the developer's name and email, and git never waits
 * on a terminal prompt.
 * @param {{
 *   homeDir: string,
 *   settingsPath: string,
 *   socketPath?: string,
 *   bossoPort: number,
 *   githubToken?: string | null,
 *   gitIdentity?: { name: string, email: string } | null,
 *   baseEnv?: NodeJS.ProcessEnv,
 * }} opts
 * @returns {NodeJS.ProcessEnv}
 */
export function buildDevBossdEnv({
  homeDir,
  settingsPath,
  socketPath,
  bossoPort,
  githubToken = null,
  gitIdentity = null,
  baseEnv = process.env,
}) {
  /** @type {NodeJS.ProcessEnv} */
  const env = {
    ...baseEnv,
    // Isolate HOME + force file keyring so bossd uses BOSSD_USER_JWT instead
    // of picking up a production token from the macOS Keychain.
    HOME: homeDir,
    BOSS_KEYRING_BACKEND: 'file',
    BOSS_SETTINGS_PATH: settingsPath,
    BOSSD_ORCHESTRATOR_URL: `http://127.0.0.1:${bossoPort}`,
    BOSSD_USER_JWT: DEV_AUTH_TOKEN,
    BOSSD_SENTRY_DSN: '',
    BOSS_ENV: 'development',
    BOSS_REFUSE_DEFAULT_SETTINGS: '1',
    GIT_TERMINAL_PROMPT: '0',
  }
  if (gitIdentity) {
    env.GIT_AUTHOR_NAME = gitIdentity.name
    env.GIT_AUTHOR_EMAIL = gitIdentity.email
    env.GIT_COMMITTER_NAME = gitIdentity.name
    env.GIT_COMMITTER_EMAIL = gitIdentity.email
  }
  if (githubToken) {
    env.GH_TOKEN = githubToken
    appendGitConfig(env, [
      ['credential.https://github.com.helper', ''],
      ['credential.https://github.com.helper', GITHUB_TOKEN_HELPER],
    ])
  }
  if (socketPath) {
    env.BOSS_SOCKET = socketPath
  } else {
    delete env.BOSS_SOCKET
  }
  return env
}

/**
 * Pick the Unix socket path for `bossd`, falling back to `fallbackDir` when
 * `<configDir>/bossd.sock` would exceed macOS's 104-byte `sun_path` limit.
 * @param {string} configDir
 * @param {string} fallbackDir
 * @returns {string}
 */
export function resolveBossdSocketPath(configDir, fallbackDir) {
  const preferred = path.join(configDir, 'bossd.sock')
  if (Buffer.byteLength(preferred, 'utf8') < 96) {
    return preferred
  }
  return path.join(fallbackDir, 'bossd.sock')
}

/**
 * Make a Connect JSON unary call over bossd's unix socket.
 * @param {string} socketPath
 * @param {string} token
 * @param {string} method
 * @param {Record<string, unknown>} message
 * @returns {Promise<unknown>}
 */
function callDaemonSocket(socketPath, token, method, message) {
  return new Promise((resolve, reject) => {
    const body = JSON.stringify(message)
    const req = http.request(
      {
        socketPath,
        method: 'POST',
        path: `/bossanova.v1.DaemonService/${method}`,
        timeout: 5000,
        headers: {
          'content-type': 'application/json',
          'connect-protocol-version': '1',
          'content-length': Buffer.byteLength(body),
          authorization: `Bearer ${token}`,
        },
      },
      (res) => {
        /** @type {Buffer[]} */
        const chunks = []
        res.on('data', (chunk) => chunks.push(chunk))
        res.on('end', () => {
          const text = Buffer.concat(chunks).toString('utf8')
          if (res.statusCode === 200) {
            resolve(text ? JSON.parse(text) : {})
            return
          }
          reject(new Error(`DaemonService/${method} -> HTTP ${res.statusCode}: ${text}`))
        })
      },
    )
    req.on('error', reject)
    req.on('timeout', () => req.destroy(new Error(`DaemonService/${method} timed out`)))
    req.write(body)
    req.end()
  })
}

/**
 * Register a repository with bossd over its unix socket, retrying until the
 * socket and bearer token file are ready.
 * @param {{
 *   socketPath: string,
 *   repoPath: string,
 *   displayName: string,
 *   worktreeBaseDir: string,
 *   timeoutMs?: number,
 *   shouldStop?: () => boolean,
 * }} opts
 * @returns {Promise<boolean>}
 */
async function registerRepoOverSocket({
  socketPath,
  repoPath,
  displayName,
  worktreeBaseDir,
  timeoutMs = 30_000,
  shouldStop = () => false,
}) {
  const tokenPath = path.join(path.dirname(socketPath), 'bossd.token')
  const deadline = Date.now() + timeoutMs
  let lastErr = ''

  while (Date.now() < deadline) {
    if (shouldStop()) return false
    try {
      const token = fs.readFileSync(tokenPath, 'utf8').trim()
      if (token) {
        await callDaemonSocket(socketPath, token, 'RegisterRepo', {
          localPath: repoPath,
          displayName,
          defaultBaseBranch: 'main',
          worktreeBaseDir,
        })
        return true
      }
    } catch (err) {
      const msg = /** @type {Error} */ (err).message
      if (msg.includes('already_exists') || msg.includes('HTTP 409')) {
        return true
      }
      lastErr = msg
    }
    await new Promise((resolve) => setTimeout(resolve, 300))
  }
  process.stderr.write(`[bossd] failed to seed repo ${displayName}: ${lastErr}\n`)
  return false
}

/**
 * Build, start, and seed a standalone dev `bossd` under `<repoRoot>/.config/serve`
 * connected to the local `bosso` instance.
 * @param {{
 *   repoRoot: string,
 *   buildDir: string,
 *   bossoPort: number,
 *   supervisor: import('./serve.mjs').ProcessSupervisor,
 *   isShuttingDown: () => boolean,
 * }} opts
 */
export async function startDevBossd({ repoRoot, buildDir, bossoPort, supervisor, isShuttingDown }) {
  const bossdDir = path.join(repoRoot, 'services/bossd')
  if (!fs.existsSync(bossdDir)) return

  // Keep standalone per-worktree daemon state under `<repoRoot>/.config/serve`
  // (already covered by `/.config/` in `.gitignore`) so each worktree runs
  // independently without clobbering a live `.config/bossd.sock` in main.
  const configDir = path.join(repoRoot, '.config', 'serve')
  const worktreeDir = path.join(configDir, 'worktrees')
  const homeDir = path.join(configDir, 'home')
  fs.mkdirSync(configDir, { recursive: true })
  fs.mkdirSync(worktreeDir, { recursive: true })
  fs.mkdirSync(homeDir, { recursive: true })

  const bossdBin = path.join(buildDir, 'bossd')
  process.stdout.write('[bossd] Building…\n')
  const ok = await buildGoBinary(path.join(bossdDir, 'cmd'), bossdBin, process.env, 'bossd')
  if (!ok || isShuttingDown()) {
    if (!isShuttingDown()) {
      process.stderr.write('[bossd] Build failed; continuing without seeded daemon\n')
    }
    return
  }

  let plugins = discoverBuiltAgentPlugins(repoRoot)
  if (plugins.length === 0) {
    const stubDir = path.join(repoRoot, 'plugins/bossd-plugin-stub-runner')
    if (fs.existsSync(stubDir)) {
      const stubBin = path.join(buildDir, 'bossd-plugin-stub-runner')
      process.stdout.write('[bossd] Building stub agent runner…\n')
      const stubOk = await buildGoBinary(stubDir, stubBin, process.env, 'bossd-stub')
      if (stubOk) {
        plugins = [{ name: 'stub', path: stubBin, enabled: true }]
      }
    }
  }

  const socketPath = resolveBossdSocketPath(configDir, buildDir)
  const settingsPath = path.join(configDir, 'settings.json')
  const settings = {
    cloud_control: 'full',
    app_data_dir: configDir,
    socket_path: socketPath,
    worktree_base_dir: worktreeDir,
    plugins,
  }
  fs.writeFileSync(settingsPath, `${JSON.stringify(settings, null, 2)}\n`, { mode: 0o600 })

  const githubToken = resolveGitHubToken()
  if (!githubToken) {
    process.stderr.write(
      '[bossd] no GitHub token from `gh auth token`; sessions in the dev stack cannot reach GitHub. Run `gh auth login`.\n',
    )
  }
  const env = buildDevBossdEnv({
    homeDir,
    settingsPath,
    socketPath,
    bossoPort,
    githubToken,
    gitIdentity: resolveGitIdentity(),
  })
  supervisor.spawn('bossd', 'bossd', bossdBin, [], { cwd: repoRoot, env })

  const displayName = resolveRepoDisplayName(repoRoot)
  const seeded = await registerRepoOverSocket({
    socketPath,
    repoPath: repoRoot,
    displayName,
    worktreeBaseDir: worktreeDir,
    shouldStop: isShuttingDown,
  })
  if (seeded && !isShuttingDown()) {
    process.stdout.write(
      `[serve] READY bossd      seeded repo "${displayName}" (config ${configDir})\n`,
    )
  }
}
