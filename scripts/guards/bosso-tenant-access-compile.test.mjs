import assert from 'node:assert/strict'
import { test } from 'node:test'
import { fixture, runGuard, verifyDiagnostics } from './bosso-tenant-access-compile.mjs'

test('tenant access compile guard rejects a seeded permissive fixture', () => {
  assert.deepEqual(runGuard(), [], 'the real positive and negative controls must pass first')
  // Simulates one formerly raw call becoming accepted. The other raw calls still
  // fail, so accepting any compiler failure would incorrectly pass this seed.
  for (const method of [
    'orgs.Update',
    'triggers.Update',
    'configs.Get',
    'webhooks.GetDelivery',
    'sessions.GetSessionInOrganization',
  ]) {
    const negativeSource = fixture(true).source.replace(
      `${method}(ctx, raw,`,
      `${method}(ctx, org,`,
    )
    const failures = runGuard({ negativeSource })
    assert.equal(failures.length, 1, failures.join('\n'))
    assert.ok(failures[0].startsWith(`${method}: missing raw-string rejection`), failures[0])
  }
})

test('tenant access compile guard rejects unrelated compiler failures', () => {
  const expected = fixture(true).expected
  const diagnostics = expected
    .map(
      ({ line, type, method }) =>
        `internal/tenantaccessfixture/fixture.go:${line}:20: cannot use raw (variable of type string) as ${type} value in argument to ${method}`,
    )
    .join('\n')
  assert.deepEqual(
    verifyDiagnostics({ status: 1, stderr: '# command-line-arguments\n' + diagnostics }, expected),
    [],
  )
  const failures = verifyDiagnostics(
    {
      status: 1,
      stderr: diagnostics + '\ninternal/tenantaccessfixture/fixture.go:1:1: undefined: unrelated',
    },
    expected,
  )
  assert.deepEqual(failures, [
    'unexpected compiler output: internal/tenantaccessfixture/fixture.go:1:1: undefined: unrelated',
  ])
  assert.equal(verifyDiagnostics({ status: 0 }, expected).length, 1)
  assert.equal(verifyDiagnostics({ status: null, signal: 'SIGTERM' }, expected).length, 1)
})
