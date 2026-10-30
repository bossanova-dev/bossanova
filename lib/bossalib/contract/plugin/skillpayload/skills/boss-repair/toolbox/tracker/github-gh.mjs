import { spawnSync } from 'node:child_process'

export class GhError extends Error {
  /** @param {string} code @param {string} message */
  constructor(code, message) {
    super(message)
    this.name = 'GhError'
    this.code = code
  }
}

/**
 * Run gh as the caller's authenticated identity. Never starts a shell.
 * @param {string[]} args
 * @param {{spawn?: Function, env?: object, timeoutMs?: number, input?: string, json?: boolean}} [options]
 * @returns {any}
 */
export function runGh(
  args,
  { spawn = spawnSync, env = process.env, timeoutMs = 60_000, input, json = true } = {},
) {
  const result = spawn('gh', args, {
    env,
    timeout: timeoutMs,
    input,
    encoding: 'utf8',
    shell: false,
    maxBuffer: 32 * 1024 * 1024,
  })
  if (result.error?.code === 'ENOENT')
    throw new GhError(
      'gh-missing',
      'gh-missing: install the GitHub CLI (https://cli.github.com), then run gh auth login',
    )
  if (result.error || result.signal)
    throw new GhError('transport', 'gh transport timed out or terminated; request may have applied')
  if (result.status !== 0) {
    const stderr = String(result.stderr ?? '').trim()
    if (/rate.limit|abuse detection/i.test(stderr))
      throw new GhError('rate-limited', `HTTP 429: ${stderr}`)
    if (/HTTP 401|gh auth login|not logged/i.test(stderr))
      throw new GhError(
        'unauthenticated',
        'HTTP 401: unauthenticated; run gh auth login (or gh auth login --hostname <host>)',
      )
    throw new GhError('gh-failed', stderr.split('\n').at(-1) || `gh exited ${result.status}`)
  }
  const stdout = String(result.stdout ?? '')
  if (!json) return stdout
  try {
    return JSON.parse(stdout)
  } catch {
    throw new GhError('invalid-json', 'gh returned invalid JSON')
  }
}
