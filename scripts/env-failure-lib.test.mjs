import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import {
  ENV_FAILURE_EXIT_CODE,
  ENV_FAILURE_RULES,
  LOCK_CONTENTION_EXHAUSTED_SIGNATURE,
  LOCK_CONTENTION_SIGNATURE,
  TRANSPORT_FAILURE_RULES,
  classifyEnvironmentFailure,
  classifyGateFailure,
  environmentFailureBanner,
} from './env-failure-lib.mjs'

test('classifyEnvironmentFailure identifies environment failures by specific kind', () => {
  const cases = [
    ['disk-exhaustion', 'compile: write /tmp/link: No space left on device'],
    ['disk-exhaustion', 'open cache: ENOSPC'],
    ['gpg-signing-unavailable', 'error: gpg failed to sign the data'],
    ['gpg-memory-pressure', 'gpg: signing failed: Cannot allocate memory'],
    ['golangci-lock-contention', LOCK_CONTENTION_EXHAUSTED_SIGNATURE],
  ]

  for (const [kind, excerpt] of cases) {
    assert.equal(classifyEnvironmentFailure(excerpt)?.kind, kind, excerpt)
  }
})

test('the gate-boundary classifier ignores transient golangci lock contention tokens', () => {
  assert.equal(classifyEnvironmentFailure(`Error: ${LOCK_CONTENTION_SIGNATURE}`), null)
})

test('gpg memory pressure wins over generic gpg signing classification', () => {
  assert.equal(
    classifyEnvironmentFailure('error: gpg: signing failed: Cannot allocate memory')?.kind,
    'gpg-memory-pressure',
  )
})

test('classifyEnvironmentFailure does not relabel genuine code failures', () => {
  const cases = [
    'main.go:12:7: undefined: userID',
    'pkg/foo.go:33:2: ineffectual assignment to err (ineffassign)',
    '--- FAIL: TestWidget (0.00s)\n    widget_test.go:14: got false, want true',
    'panic: runtime error: invalid memory address or nil pointer dereference',
  ]

  for (const excerpt of cases) {
    assert.equal(classifyEnvironmentFailure(excerpt), null, excerpt)
  }
})

test('environmentFailureBanner documents the mechanical non-code-failure signal', () => {
  assert.equal(ENV_FAILURE_EXIT_CODE, 75)
  assert.ok(ENV_FAILURE_RULES.length >= 4)
  assert.match(
    environmentFailureBanner({
      kind: 'disk-exhaustion',
      remedy: 'free disk space',
      label: 'make test',
    }),
    /^ENVIRONMENT FAILURE \(not a code defect\): disk-exhaustion during make test - free disk space$/,
  )
})

test('CLAUDE.md documents the environment-failure banner and exit code verbatim', () => {
  const claudeMd = fs.readFileSync(
    path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'CLAUDE.md'),
    'utf8',
  )
  assert.ok(claudeMd.includes('ENVIRONMENT FAILURE (not a code defect)'))
  assert.ok(claudeMd.includes('exit code `75`'))
})

// --- BOS-1276: a gateway timeout fetching an external repository is not a red gate -------------

test('a 504 / Gateway Time-out on an external-repository fetch is an environment failure', () => {
  const bazelFetch = [
    "ERROR: An error occurred during the fetch of repository 'go_sdk':",
    '   Traceback (most recent call last):',
    '   Error downloading [https://dl.google.com/go/go1.25.darwin-arm64.tar.gz] to /private/var/tmp/x:',
    '   GET returned 504 Gateway Time-out',
    'ERROR: Analysis of target //services/bossd/... failed; build aborted',
  ].join('\n')
  assert.equal(classifyGateFailure(bazelFetch)?.kind, 'gateway-timeout-fetch')

  const curlFetch = [
    'Fetching https://proxy.golang.org/github.com/example/@v/list',
    'curl: (22) The requested URL returned error: 504',
  ].join('\n')
  assert.equal(classifyGateFailure(curlFetch)?.kind, 'gateway-timeout-fetch')

  const spacedSpelling = 'downloading module zip: HTTP/2 504 Gateway Timeout'
  assert.equal(classifyGateFailure(spacedSpelling)?.kind, 'gateway-timeout-fetch')

  assert.match(classifyGateFailure(bazelFetch).remedy, /re-run the gate/)
})

test('a genuine test failure that merely quotes the phrase stays a red gate', () => {
  // A test asserting on a proxy's 504 handling echoes the phrase in its own failure output. The
  // transport half is withheld from any log that already reports a test failure, so the red stands.
  const goFailure = [
    '=== RUN   TestProxyRetriesGatewayTimeout',
    '    proxy_test.go:88: got "504 Gateway Time-out", want a retry',
    '--- FAIL: TestProxyRetriesGatewayTimeout (0.01s)',
    'FAIL\tgithub.com/example/proxy\t0.041s',
  ].join('\n')
  assert.equal(classifyGateFailure(goFailure), null)

  const tapFailure = [
    'not ok 3 - surfaces a Gateway Time-out to the caller',
    '  ---',
    "  error: 'Expected values to be strictly equal'",
  ].join('\n')
  assert.equal(classifyGateFailure(tapFailure), null)
})

test('the gateway-timeout signature lives in the transport table, not the host table', () => {
  // The split is load-bearing: scripts/run-gate.mjs consults the HOST table only, so putting the
  // rule there would change the exit code of every wrapped make target.
  assert.equal(
    TRANSPORT_FAILURE_RULES.some((rule) => rule.kind === 'gateway-timeout-fetch'),
    true,
  )
  assert.equal(
    ENV_FAILURE_RULES.some((rule) => rule.kind === 'gateway-timeout-fetch'),
    false,
  )
  assert.equal(classifyEnvironmentFailure('GET returned 504 Gateway Time-out'), null)
})

test('a 504 from the branch own service in a non-test gate stays a red gate', () => {
  // classifyGateFailure withholds the transport half only from logs carrying a Go/TAP failure
  // marker. A curl smoke check or a Makefile recipe emits none, so without fetch context on the
  // line a bare 504 from the code under test would be reported as an environment failure and the
  // reader told to re-run a genuine red.
  const ownService = [
    'smoke: starting bosso on :8099',
    'smoke: POST /api/orders -> 504 Gateway Time-out',
    'smoke: expected 201, aborting',
  ].join('\n')
  assert.equal(classifyGateFailure(ownService), null)

  // The external-repository fetch it was written for still classifies.
  assert.equal(
    classifyGateFailure('   GET returned 504 Gateway Time-out')?.kind,
    'gateway-timeout-fetch',
  )
})
