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

// Network transport signatures. Deliberately a SEPARATE list from ENV_FAILURE_RULES: widening that
// table would silently change the exit code of every `scripts/run-gate.mjs`-wrapped make target and
// of `scripts/lint-affected.mjs`, in CI as well as locally. Only `scripts/gate-run.mjs` consults
// these, through classifyGateFailure.
export const TRANSPORT_FAILURE_RULES = [
  {
    kind: 'github-graphql-transport',
    pattern: 'Post "https://api.github.com/graphql"',
    remedy:
      'the GitHub GraphQL endpoint did not answer; re-run the gate rather than reading the exit code as a red gate',
  },
  {
    kind: 'network-transport-timeout',
    pattern: /operation timed out|read tcp .*: i\/o timeout|TLS handshake timeout/i,
    remedy:
      'a network read timed out during the gate; re-run the gate before treating it as a defect',
  },
  {
    // A gateway timeout fetching an EXTERNAL REPOSITORY (BOS-1276). Bazel renders it
    // `ERROR: An error occurred during the fetch of repository '<name>'` followed by
    // `GET returned 504 Gateway Time-out`; curl renders the same upstream answer as
    // `The requested URL returned error: 504`. A CDN or module proxy answering 504 says nothing
    // about the branch's code.
    //
    // Deliberately in THIS table rather than ENV_FAILURE_RULES, which means `scripts/run-gate.mjs`
    // — the wrapper that consults the host table only — still returns a red gate for this case.
    // That fork is intentional: as the comment above this list records, widening the host table
    // silently changes the exit code of every wrapped make target, in CI as well as locally, and
    // that blast radius is worse than the single case it would fix. `scripts/gate-run.mjs` is the
    // surface that classifies it.
    kind: 'gateway-timeout-fetch',
    // The 504 token must share its LINE with fetch context. A bare 504 anywhere in the log is not
    // evidence of a fetch: a non-test gate (a curl smoke check, a Makefile recipe) that fails
    // because the branch's OWN service answered 504 would otherwise be reported as an environment
    // failure and the reader told to re-run it. The test-failure withholding in classifyGateFailure
    // does not cover those - it keys on Go/TAP markers this gate never emits. This narrows the rule
    // to what its kind and remedy actually claim; it does not make the claim exact, because a smoke
    // check fetching a URL is still indistinguishable from a dependency fetch by text alone.
    pattern:
      /^.*(?:fetch|download|GET |requested URL|proxy\.golang|https?:\/\/).*(?:Gateway Time-?out|returned (?:error: )?504\b|HTTP(?:\/[\d.]+)? 504\b)/im,
    remedy:
      'an upstream gateway returned 504 while fetching an external repository; re-run the gate rather than reading the exit code as a red gate',
  },
  {
    kind: 'network-connection-reset',
    pattern: /connection reset by peer/i,
    remedy: 'the remote peer dropped the connection mid-gate; re-run the gate',
  },
]

// A genuine test failure whose own output quotes a transport phrase must stay a red gate. This repo
// really does carry such fixtures — services/bossd/internal/upstream/terminal_stream_test.go and
// services/bossd/internal/server/merge_session_live_test.go both use the literal string
// `connection reset by peer` — and Go echoes a fixture back in its failure output, so an unbounded
// transport scan would report a real red as exit 75 whose stated remedy is "re-run it, not a red
// gate". That is the same class of lying result the gate verdicts exist to remove.
export const TEST_FAILURE_MARKERS = [
  /^--- FAIL/m,
  /^\s*--- FAIL/m,
  /^FAIL\b/m,
  /^not ok /m,
  /^FAILED: /m,
]

function hasTestFailureMarker(input) {
  return TEST_FAILURE_MARKERS.some((marker) => marker.test(input))
}

function matchRules(input, rules) {
  for (const rule of rules) {
    if (typeof rule.pattern === 'string') {
      if (input.includes(rule.pattern)) return rule
      continue
    }
    if (rule.pattern.test(input)) return rule
  }
  return null
}

export function classifyEnvironmentFailure(text) {
  return matchRules(String(text ?? ''), ENV_FAILURE_RULES)
}

// Host-environment rules first, then transport: a disk-exhaustion or GPG failure is a better
// description of the same output than "a socket timed out" when both happen to appear.
//
// Only the TRANSPORT half is withheld from a log that already reports a test failure. The host
// rules are deliberately NOT gated that way: a disk that filled up or a GPG key that failed during
// a test run really is a host-environment failure, and classifying it as one is the shipped
// behaviour scripts/run-gate.mjs already depends on.
export function classifyGateFailure(text) {
  const input = String(text ?? '')
  const hostFailure = matchRules(input, ENV_FAILURE_RULES)
  if (hostFailure) return hostFailure
  if (hasTestFailureMarker(input)) return null
  return matchRules(input, TRANSPORT_FAILURE_RULES)
}

export function environmentFailureBanner({ kind, remedy, label } = {}) {
  const gate = label ? ` during ${label}` : ''
  const advice = remedy ? ` - ${remedy}` : ''
  return `ENVIRONMENT FAILURE (not a code defect): ${kind ?? 'unknown'}${gate}${advice}`
}
