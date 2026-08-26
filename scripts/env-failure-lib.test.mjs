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
  classifyEnvironmentFailure,
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
