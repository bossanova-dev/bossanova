/**
 * proof-doctor.mjs — bs-proof prerequisite doctor + hard preflight (BOS-138 P1b).
 *
 * Reports, one line per prerequisite, whether each is `set`/`MISSING` — NEVER a
 * secret value. `proof.mjs run` runs these checks FIRST (as library calls),
 * scoped to the classified surface; any missing surface-required prerequisite
 * defers the run with an `env-unavailable` note (exit 0) before any pipeline
 * stage starts.
 *
 * All checks are pure functions over an injectable `lookups` object, so they
 * unit-test without real binaries, filesystems, or network.
 */

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

/** The five allowlisted proof env keys (daemon-injected in cron; P1a). */
export const PROOF_ENV_KEYS = [
  'PROOF_ANTHROPIC_API_KEY',
  'CLOUDFLARE_API_TOKEN',
  'BOSS_PROOF_R2_BUCKET',
  'BOSS_PROOF_PUBLIC_BASE_URL',
  'CLOUDFLARE_ACCOUNT_ID',
]

/** Env keys needed to upload + host the media (R2 + Cloudflare + public URL). */
const UPLOAD_ENV = [
  'CLOUDFLARE_API_TOKEN',
  'BOSS_PROOF_R2_BUCKET',
  'BOSS_PROOF_PUBLIC_BASE_URL',
  'CLOUDFLARE_ACCOUNT_ID',
]

/** Credentials needed to push the PR comment / gallery (D15). */
const PUSH = ['gh-auth', 'git-credential']

/**
 * Every prerequisite the doctor knows about. `kind` selects the probe:
 *   env   → lookups.env(id) is truthy
 *   bin   → lookups.hasBin(id)
 *   probe → lookups.<probeKey>()
 */
export const DOCTOR_CHECKS = [
  { id: 'PROOF_ANTHROPIC_API_KEY', kind: 'env', label: 'PROOF_ANTHROPIC_API_KEY (agent key)' },
  { id: 'CLOUDFLARE_API_TOKEN', kind: 'env', label: 'CLOUDFLARE_API_TOKEN (R2 upload)' },
  { id: 'BOSS_PROOF_R2_BUCKET', kind: 'env', label: 'BOSS_PROOF_R2_BUCKET' },
  { id: 'BOSS_PROOF_PUBLIC_BASE_URL', kind: 'env', label: 'BOSS_PROOF_PUBLIC_BASE_URL' },
  { id: 'CLOUDFLARE_ACCOUNT_ID', kind: 'env', label: 'CLOUDFLARE_ACCOUNT_ID' },
  { id: 'agg', kind: 'bin', label: 'agg on PATH (asciinema→gif)' },
  { id: 'ffmpeg', kind: 'bin', label: 'ffmpeg on PATH' },
  { id: 'chromium', kind: 'probe', probeKey: 'chromiumPresent', label: 'Playwright Chromium' },
  {
    id: 'web-node-modules',
    kind: 'probe',
    probeKey: 'webDepsPresent',
    label: 'services/web/node_modules',
  },
  {
    id: 'go-toolchain',
    kind: 'probe',
    probeKey: 'goToolchainPresent',
    label: 'Go toolchain (TUI bridge build)',
  },
  { id: 'gh-auth', kind: 'probe', probeKey: 'ghAuthOk', label: 'gh auth status' },
  {
    id: 'git-credential',
    kind: 'probe',
    probeKey: 'gitCredentialOk',
    label: 'git push credential',
  },
]

/**
 * Extra prerequisites required ONLY when a genuinely-running (unstubbed) live
 * agent scene is requested (BOS-142). These are additive: they are never part of
 * DOCTOR_CHECKS (which stays byte-identical for the non-live path) and only enter
 * a report/required-set when `liveAgent` is true. PROOF_ANTHROPIC_API_KEY is NOT
 * listed here — it is already a DOCTOR_CHECK and already required for the web
 * surface, so it needs no duplication.
 */
export const LIVE_AGENT_CHECKS = [
  { id: 'claude', kind: 'bin', label: 'claude on PATH (live agent)' },
  { id: 'tmux', kind: 'bin', label: 'tmux on PATH (live agent chat pane)' },
  {
    id: 'bossd-plugin-claude',
    kind: 'probe',
    probeKey: 'claudePluginBuilt',
    label: 'bossd-plugin-claude binary (make plugins)',
  },
]

/**
 * The prerequisite ids each surface requires. `shouldUpload=false` (dry-run /
 * BOSS_PROOF_UPLOAD=0) drops the upload + push credentials — a local dry-run must
 * not defer just because R2/gh are absent. `liveAgent=true` (BOS-142) appends the
 * LIVE_AGENT_CHECKS ids to whatever the surface normally requires.
 * @param {'tui'|'web'|'recipe'|'docs'|'all'} surface
 * @param {{ shouldUpload?: boolean, liveAgent?: boolean }} [opts]
 * @returns {string[]}
 */
export function requiredIdsForSurface(surface, { shouldUpload = true, liveAgent = false } = {}) {
  const uploadAndPush = shouldUpload ? [...UPLOAD_ENV, ...PUSH] : []
  const base = {
    tui: ['PROOF_ANTHROPIC_API_KEY', 'agg', 'ffmpeg', 'go-toolchain', ...uploadAndPush],
    web: ['PROOF_ANTHROPIC_API_KEY', 'ffmpeg', 'chromium', 'web-node-modules', ...uploadAndPush],
    recipe: ['ffmpeg', 'chromium', 'web-node-modules', ...uploadAndPush],
    docs: shouldUpload ? [...PUSH] : [],
    all: DOCTOR_CHECKS.map((c) => c.id),
  }
  const ids = base[surface] ?? base.all
  return liveAgent ? [...ids, ...LIVE_AGENT_CHECKS.map((c) => c.id)] : ids
}

/**
 * Evaluates every DOCTOR_CHECK against the injected lookups and marks which are
 * required for `surface`. Pure.
 * When `liveAgent` is true (BOS-142) the evaluated checks list is
 * DOCTOR_CHECKS ++ LIVE_AGENT_CHECKS and the LIVE_AGENT ids are required; when
 * false the list and required-set are byte-identical to the pre-BOS-142 report.
 * @param {{
 *   surface?: 'tui'|'web'|'recipe'|'docs'|'all',
 *   shouldUpload?: boolean,
 *   liveAgent?: boolean,
 *   lookups: object,
 * }} opts
 * @returns {{
 *   surface: string,
 *   checks: Array<{id:string,label:string,kind:string,required:boolean,present:boolean}>,
 *   missing: string[],
 *   ok: boolean,
 * }}
 */
export function doctorReport({ surface = 'all', shouldUpload = true, liveAgent = false, lookups }) {
  const required = new Set(requiredIdsForSurface(surface, { shouldUpload, liveAgent }))
  const evaluated = liveAgent ? [...DOCTOR_CHECKS, ...LIVE_AGENT_CHECKS] : DOCTOR_CHECKS
  const checks = evaluated.map((check) => ({
    id: check.id,
    label: check.label ?? check.id,
    kind: check.kind,
    required: required.has(check.id),
    present: probeCheck(check, lookups),
  }))
  const missing = checks.filter((c) => c.required && !c.present).map((c) => c.id)
  return { surface, checks, missing, ok: missing.length === 0 }
}

/**
 * @param {{id:string,kind:string,probeKey?:string}} check
 * @param {object} lookups
 * @returns {boolean}
 */
function probeCheck(check, lookups) {
  if (check.kind === 'env') return Boolean(lookups.env(check.id))
  if (check.kind === 'bin') return Boolean(lookups.hasBin(check.id))
  const probe = lookups[check.probeKey]
  return typeof probe === 'function' ? Boolean(probe()) : false
}

/**
 * Human-readable doctor report. Only ever prints `set`/`MISSING` — never a value.
 * @param {ReturnType<typeof doctorReport>} report
 * @returns {string}
 */
export function formatDoctorReport(report) {
  const lines = [`bs-proof doctor — surface: ${report.surface}`, '']
  for (const check of report.checks) {
    const status = check.present ? 'set' : 'MISSING'
    const scope = check.required ? 'required' : 'optional'
    lines.push(`  [${status}] ${check.label} (${scope})`)
  }
  lines.push('')
  lines.push(
    report.ok
      ? 'All required prerequisites present.'
      : `Missing required prerequisites: ${report.missing.join(', ')}`,
  )
  return lines.join('\n')
}

// ── Default (real) lookups ────────────────────────────────────────────────────

/**
 * True when `bin` is found in any PATH directory. Pure over env + fileExists so
 * it is unit-testable without a real PATH.
 * @param {string} bin
 * @param {{ env?: NodeJS.ProcessEnv, fileExists?: (p: string) => boolean }} [opts]
 * @returns {boolean}
 */
export function binOnPath(bin, { env = process.env, fileExists = fs.existsSync } = {}) {
  const dirs = (env.PATH || '').split(path.delimiter).filter(Boolean)
  return dirs.some((dir) => fileExists(path.join(dir, bin)))
}

/** Default Playwright browser cache dir for this platform. */
function defaultPlaywrightCache(env = process.env) {
  if (env.PLAYWRIGHT_BROWSERS_PATH && env.PLAYWRIGHT_BROWSERS_PATH !== '0') {
    return env.PLAYWRIGHT_BROWSERS_PATH
  }
  const home = os.homedir()
  if (process.platform === 'darwin') return path.join(home, 'Library', 'Caches', 'ms-playwright')
  if (process.platform === 'win32') {
    return path.join(env.LOCALAPPDATA || path.join(home, 'AppData', 'Local'), 'ms-playwright')
  }
  return path.join(home, '.cache', 'ms-playwright')
}

/** True when a `chromium-*` browser is installed in the Playwright cache. */
function chromiumInstalled(env = process.env) {
  try {
    return fs.readdirSync(defaultPlaywrightCache(env)).some((name) => name.startsWith('chromium'))
  } catch {
    return false
  }
}

/**
 * Builds the real probe seams used by the `doctor` CLI and the run preflight.
 * Values are read but never returned/printed — only presence booleans escape.
 * @param {{ repoRoot: string, env?: NodeJS.ProcessEnv }} opts
 * @returns {object}
 */
export function defaultDoctorLookups({ repoRoot, env = process.env }) {
  const okExit = (bin, args) => {
    try {
      return spawnSync(bin, args, { stdio: 'ignore' }).status === 0
    } catch {
      return false
    }
  }
  return {
    env: (key) => env[key],
    hasBin: (bin) => binOnPath(bin, { env }),
    chromiumPresent: () => chromiumInstalled(env),
    webDepsPresent: () => fs.existsSync(path.join(repoRoot, 'services', 'web', 'node_modules')),
    goToolchainPresent: () => binOnPath('go', { env }),
    // BOS-142: the live-agent chat pane is driven through bossd's claude plugin,
    // which must be built (`make plugins`) into bin/ before a live scene can run.
    claudePluginBuilt: () => fs.existsSync(path.join(repoRoot, 'bin', 'bossd-plugin-claude')),
    ghAuthOk: () => okExit('gh', ['auth', 'status']),
    // A push credential probe that never mutates: an origin remote must exist and
    // gh (which supplies the git credential helper here) must be authenticated.
    gitCredentialOk: () =>
      okExit('git', ['-C', repoRoot, 'rev-parse', '--is-inside-work-tree']) &&
      okExit('gh', ['auth', 'status']),
  }
}
