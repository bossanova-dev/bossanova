// The agent CLI basics shared by the review helpers and the headless proof author: which binary
// to run and which output means its login is stale.
import fs from 'node:fs'
import path from 'node:path'

/**
 * The agent binary to run, or null. A non-empty `env[overrideVar]` must be an absolute executable
 * path: a relative one is refused rather than resolved, because a cron or daemon cwd is not
 * predictable, and it never falls back to PATH, which would hide the bad setting. Otherwise the
 * first executable `binName` on `env.PATH`.
 * @param {Record<string, string | undefined> | undefined} env
 * @param {{overrideVar?: string, binName?: string}} [options]
 * @returns {string | null}
 */
export function resolveAgentBin(env, { overrideVar, binName } = {}) {
  const rawOverride = overrideVar ? env?.[overrideVar] : undefined
  const override = typeof rawOverride === 'string' ? rawOverride : ''
  if (override !== '') {
    if (!path.isAbsolute(override)) return null
    try {
      fs.accessSync(override, fs.constants.X_OK)
      return override
    } catch {
      return null
    }
  }
  const pathVar = typeof env?.PATH === 'string' ? env.PATH : ''
  for (const dir of pathVar.split(path.delimiter).filter(Boolean)) {
    const candidate = path.join(dir, String(binName))
    try {
      fs.accessSync(candidate, fs.constants.X_OK)
      return candidate
    } catch {
      /* not in this directory */
    }
  }
  return null
}

/** The claude binary: `BOSS_CLAUDE_BIN`, else `claude` on PATH. @param {Record<string, string | undefined> | undefined} env */
export const resolveClaudeBin = (env) =>
  resolveAgentBin(env, { overrideVar: 'BOSS_CLAUDE_BIN', binName: 'claude' })

/** Output that means the claude login is missing or expired. */
export const CLAUDE_AUTH_SIGNATURES = [
  /OAuth (?:token|session)(?: has)? expired/i,
  /authentication_error/i,
  /Please run \/login|Invalid API key/i,
  /401[^\n]*(?:Unauthorized|authentication)|(?:Unauthorized|authentication)[^\n]*401/i,
]
