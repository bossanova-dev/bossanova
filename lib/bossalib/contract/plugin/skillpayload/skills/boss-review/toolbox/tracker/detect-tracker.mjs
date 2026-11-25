// Pure tracker detection with injectable I/O so committed configs pay nothing and tests stay
// independent of the host environment, HOME, and git state.
import { execFileSync } from 'node:child_process'
import { existsSync, readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { dirname, join, parse as parsePath, resolve } from 'node:path'
import { githubRepositoryFromOrigin } from './github-config.mjs'

const LINEAR_MCP_NAME = /linear/i

/**
 * @typedef {{
 *   tracker: string,
 *   reason: 'explicit' | 'declared-block' | 'multiple-blocks-default' | 'env-override' | 'linear-api-key' | 'linear-mcp' | 'github-origin' | 'default',
 *   block?: { repository: string },
 * }} TrackerDetection
 */

/**
 * @typedef {{
 *   user?: { adapters?: { tracker?: unknown }, trackerConfig?: Record<string, unknown> } | null,
 *   env?: NodeJS.ProcessEnv | Record<string, string | undefined>,
 *   cwd?: string,
 *   home?: string | null,
 *   readOrigin?: (dir: string) => string | null,
 *   readFile?: (filePath: string) => string | null,
 * }} DetectTrackerOptions
 */

/** @param {string} filePath @returns {string | null} */
function defaultReadFile(filePath) {
  try {
    return readFileSync(filePath, 'utf8')
  } catch {
    return null
  }
}

/** @param {string} dir @returns {string | null} */
function defaultReadOrigin(dir) {
  if (typeof dir !== 'string' || !dir.trim()) return null
  try {
    const out = execFileSync('git', ['remote', 'get-url', 'origin'], {
      cwd: dir,
      encoding: 'utf8',
      timeout: 3000,
      stdio: ['ignore', 'pipe', 'ignore'],
    })
    const trimmed = typeof out === 'string' ? out.trim() : ''
    return trimmed || null
  } catch {
    return null
  }
}

/** @param {string | null} text @returns {boolean} */
function hasLinearJsonMcp(text) {
  if (typeof text !== 'string' || !text.trim()) return false
  let parsed
  try {
    parsed = JSON.parse(text)
  } catch {
    return false
  }
  const servers = parsed?.mcpServers
  if (!servers || typeof servers !== 'object' || Array.isArray(servers)) return false
  return Object.keys(servers).some((name) => LINEAR_MCP_NAME.test(name))
}

/**
 * Parse a single TOML `[a.b."c"]` table header line into its key segments, or `null`.
 * @param {string} line
 * @returns {string[] | null}
 */
function parseTomlTableKeys(line) {
  let i = 0
  while (i < line.length && (line[i] === ' ' || line[i] === '\t')) i += 1
  if (line[i] !== '[' || line[i + 1] === '[') return null
  i += 1
  /** @type {string[]} */
  const keys = []
  while (i < line.length) {
    while (i < line.length && (line[i] === ' ' || line[i] === '\t')) i += 1
    if (i >= line.length) return null
    let key = ''
    if (line[i] === '"') {
      i += 1
      let closed = false
      while (i < line.length) {
        if (line[i] === '\\') {
          if (i + 1 >= line.length) return null
          key += line[i + 1]
          i += 2
          continue
        }
        if (line[i] === '"') {
          i += 1
          closed = true
          break
        }
        key += line[i]
        i += 1
      }
      if (!closed) return null
    } else if (line[i] === "'") {
      i += 1
      const end = line.indexOf("'", i)
      if (end < 0) return null
      key = line.slice(i, end)
      i = end + 1
    } else {
      const start = i
      while (i < line.length && /[A-Za-z0-9_-]/.test(line[i])) i += 1
      if (i === start) return null
      key = line.slice(start, i)
    }
    keys.push(key)
    while (i < line.length && (line[i] === ' ' || line[i] === '\t')) i += 1
    if (line[i] === '.') {
      i += 1
      continue
    }
    if (line[i] === ']') {
      i += 1
      const trailing = line.slice(i).trim()
      return trailing === '' || trailing.startsWith('#') ? keys : null
    }
    return null
  }
  return null
}

/** @param {string | null} text @returns {boolean} */
function hasLinearTomlMcp(text) {
  if (typeof text !== 'string' || !text.trim()) return false
  for (const line of text.split(/\r?\n/)) {
    const keys = parseTomlTableKeys(line)
    if (keys && keys[0] === 'mcp_servers' && keys.length >= 2 && LINEAR_MCP_NAME.test(keys[1])) {
      return true
    }
  }
  return false
}

/**
 * @param {(filePath: string) => string | null} readFile
 * @param {string} filePath
 * @returns {string | null}
 */
function safeReadFile(readFile, filePath) {
  try {
    const content = readFile(filePath)
    return typeof content === 'string' ? content : null
  } catch {
    return null
  }
}

/**
 * Walk upward from `startDir` looking for a `.git` entry (directory or worktree file) and return
 * the enclosing repository root, or `null` when none exists on disk.
 * @param {string} startDir
 * @returns {string | null}
 */
function findGitRoot(startDir) {
  let dir = resolve(startDir)
  const { root } = parsePath(dir)
  for (;;) {
    try {
      if (existsSync(join(dir, '.git'))) return dir
    } catch {
      return null
    }
    if (dir === root) return null
    const parent = dirname(dir)
    if (parent === dir) return null
    dir = parent
  }
}

/**
 * Detect the active tracker from user config, environment, harness MCP configs, and git origin.
 * Precedence: explicit selection > single/multiple declared trackerConfig blocks > explicit TRACKER
 * env override (`linear` or `github`) > Linear API key or Linear MCP server > GitHub origin >
 * Linear fallback.
 * @param {DetectTrackerOptions} [options]
 * @returns {TrackerDetection}
 */
export function detectTracker({
  user,
  env = process.env,
  cwd = process.cwd(),
  home = homedir(),
  readOrigin = defaultReadOrigin,
  readFile = defaultReadFile,
} = {}) {
  const explicit = user?.adapters?.tracker
  if (typeof explicit === 'string' && explicit.trim() !== '') {
    return { tracker: explicit, reason: 'explicit' }
  }

  const tc = user?.trackerConfig
  if (tc && typeof tc === 'object' && !Array.isArray(tc)) {
    const keys = Object.keys(tc)
    if (keys.length === 1) return { tracker: keys[0], reason: 'declared-block' }
    if (keys.length > 1) return { tracker: 'linear', reason: 'multiple-blocks-default' }
  }

  const envTracker = typeof env?.TRACKER === 'string' ? env.TRACKER.trim() : ''
  if (envTracker === 'linear') {
    return { tracker: 'linear', reason: 'env-override' }
  }

  if (envTracker !== 'github') {
    if (typeof env?.LINEAR_API_KEY === 'string' && env.LINEAR_API_KEY.trim() !== '') {
      return { tracker: 'linear', reason: 'linear-api-key' }
    }

    /** @type {Array<[string, 'json' | 'toml']>} */
    const harnessFiles = []
    if (typeof cwd === 'string' && cwd.trim() !== '') {
      harnessFiles.push([join(cwd, '.mcp.json'), 'json'])
      harnessFiles.push([join(cwd, '.codex', 'config.toml'), 'toml'])
      const repoRoot = findGitRoot(cwd)
      if (repoRoot && repoRoot !== resolve(cwd)) {
        harnessFiles.push([join(repoRoot, '.mcp.json'), 'json'])
        harnessFiles.push([join(repoRoot, '.codex', 'config.toml'), 'toml'])
      }
    }
    if (typeof home === 'string' && home.trim() !== '') {
      harnessFiles.push([join(home, '.claude.json'), 'json'])
      harnessFiles.push([join(home, '.codex', 'config.toml'), 'toml'])
    }
    for (const [filePath, format] of harnessFiles) {
      const content = safeReadFile(readFile, filePath)
      if (format === 'json' ? hasLinearJsonMcp(content) : hasLinearTomlMcp(content)) {
        return { tracker: 'linear', reason: 'linear-mcp' }
      }
    }
  }

  let origin = null
  if (typeof cwd === 'string' && cwd.trim() !== '') {
    try {
      const value = readOrigin(cwd)
      origin = typeof value === 'string' ? value : null
    } catch {
      origin = null
    }
  }
  const repository = githubRepositoryFromOrigin(origin)
  if (repository) {
    return { tracker: 'github', reason: 'github-origin', block: { repository } }
  }

  return { tracker: 'linear', reason: 'default' }
}

/**
 * Convert a tracker detection result into a partial config layer merged between defaults and user.
 * @param {TrackerDetection} detection
 * @param {string} [defaultTracker]
 * @returns {{ adapters?: { tracker: string }, trackerConfig?: Record<string, { repository: string }> }}
 */
export function trackerDetectionLayer(detection, defaultTracker = 'linear') {
  if (detection?.block) {
    return {
      adapters: { tracker: detection.tracker },
      trackerConfig: { [detection.tracker]: detection.block },
    }
  }
  if (detection?.reason === 'declared-block' && detection.tracker !== defaultTracker) {
    return { adapters: { tracker: detection.tracker } }
  }
  return {}
}
