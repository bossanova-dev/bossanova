export const ENV_FAILURE_EXIT_CODE = 75

export const LOCK_CONTENTION_SIGNATURE = 'parallel golangci-lint is running'
export const LOCK_CONTENTION_EXHAUSTED_SIGNATURE =
  'golangci-lint: lock contention exhausted after 3 attempts (not a lint finding)'

export const ENV_FAILURE_RULES = [
  {
    kind: 'gpg-memory-pressure',
    pattern: /gpg: signing failed: Cannot allocate memory|signing failed: Cannot allocate memory/i,
    remedy:
      'host GPG signing could not allocate memory; free host memory or disable signing for fixture repositories',
  },
  {
    kind: 'golangci-lock-contention',
    pattern: LOCK_CONTENTION_EXHAUSTED_SIGNATURE,
    remedy: 'another golangci-lint process holds the global lock; wait for that process to finish',
  },
  {
    kind: 'disk-exhaustion',
    pattern: /No space left on device|ENOSPC/i,
    remedy:
      'free disk space, especially /private/var/tmp/_bazel_dave and ~/.cache/bazel-bossanova-disk',
  },
  {
    kind: 'gpg-signing-unavailable',
    pattern: /gpg failed to sign the data|gpg: signing failed/i,
    remedy:
      'host Git commit signing failed; disable signing in temporary fixture repositories or verify the signing key',
  },
]

export function classifyEnvironmentFailure(text) {
  const input = String(text ?? '')
  for (const rule of ENV_FAILURE_RULES) {
    if (typeof rule.pattern === 'string') {
      if (input.includes(rule.pattern)) return rule
      continue
    }
    if (rule.pattern.test(input)) return rule
  }
  return null
}

export function environmentFailureBanner({ kind, remedy, label } = {}) {
  const gate = label ? ` during ${label}` : ''
  const advice = remedy ? ` - ${remedy}` : ''
  return `ENVIRONMENT FAILURE (not a code defect): ${kind ?? 'unknown'}${gate}${advice}`
}
