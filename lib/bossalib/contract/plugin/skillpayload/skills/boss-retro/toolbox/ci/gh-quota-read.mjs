import { spawnSync } from 'node:child_process'

export const GH_USAGE_LABEL_ENV = 'BOSS_GH_USAGE_LABEL'
export const FALLBACK_LABEL_PREFIX = 'fallback:'
export const FALLBACK_LABELS = Object.freeze({
  CI_WAIT: `${FALLBACK_LABEL_PREFIX}ci-wait`,
  PR_STATE: `${FALLBACK_LABEL_PREFIX}pr-state`,
})

/** @param {string[]} args */
export function quotaOf(args) {
  return (args[0] === 'api' && args[1] !== 'graphql') || args[0] === 'run' ? 'core' : 'graphql'
}

/** @param {string} stderr */
export function isRateLimitRefusal(stderr) {
  return /api rate limit|secondary rate limit|rate limit exceeded|was submitted too quickly|abuse detection/i.test(
    stderr,
  )
}

export function ghSpawn(args, { label, timeoutMs, deps }) {
  return (deps?.spawn ?? spawnSync)('gh', args, {
    ...(deps?.cwd ? { cwd: deps.cwd } : {}),
    encoding: 'utf8',
    timeout: Math.max(1, Math.floor(timeoutMs)),
    killSignal: 'SIGKILL',
    maxBuffer: 64 * 1024 * 1024,
    env: { ...(deps?.env ?? process.env), [GH_USAGE_LABEL_ENV]: label },
  })
}

function excerpt(value) {
  return String(value ?? '')
    .replace(/\b(?:gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+)\b/g, '[redacted]')
    .replace(/(bearer|token)\s+\S+/gi, '$1 [redacted]')
    .slice(0, 280)
}

/**
 * @template T
 * @param {{primary: {args: string[], normalize: (value: *) => T},
 * alternate: {args: string[], normalize: (value: *) => T},
 * label: string, timeoutMs: number, deps?: object}} options
 * @returns {{ok: true, value: T, transport: string,
 * degraded: {transport: string, reason: string}|null} |
 * {ok: false, error: {code: string, message: string, quotas?: string[]}}}
 */
export function readWithQuotaFallback({ primary, alternate, label, timeoutMs, deps }) {
  function read(request) {
    const res = ghSpawn(request.args, { label, timeoutMs, deps })
    if (res.error || res.status !== 0)
      return {
        ok: false,
        refused: !res.error && isRateLimitRefusal(String(res.stderr ?? '')),
        message: excerpt(
          res.error?.code ||
            res.error?.message ||
            String(res.stderr ?? '').trim() ||
            `gh exited ${res.status}`,
        ),
      }
    try {
      return { ok: true, value: request.normalize(JSON.parse(res.stdout)) }
    } catch {
      return { ok: false, refused: false, message: 'unparseable gh output' }
    }
  }
  const first = read(primary)
  const transport = quotaOf(primary.args)
  if (first.ok) return { ok: true, value: first.value, transport, degraded: null }
  if (!first.refused) return { ok: false, error: { code: 'UNREADABLE', message: first.message } }
  const alternateArgs = alternate.args
  const second = read({ args: alternateArgs, normalize: alternate.normalize })
  const fallbackTransport = quotaOf(alternateArgs)
  if (second.ok)
    return {
      ok: true,
      value: second.value,
      transport: fallbackTransport,
      degraded: { transport: fallbackTransport, reason: `${transport}-rate-limited` },
    }
  if (!second.refused) return { ok: false, error: { code: 'UNREADABLE', message: second.message } }
  return {
    ok: false,
    error: {
      code: 'RATE_LIMITED',
      message: `${first.message}; ${second.message}`,
      quotas: [transport, fallbackTransport],
    },
  }
}
