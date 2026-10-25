import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { verifyLocations } from './bs-security-findings.mjs'
import { scanRepository } from './bs-security-tenant-scan.mjs'

const scanner = fileURLToPath(new URL('./bs-security-tenant-scan.mjs', import.meta.url))
const paths = {
  calls: 'services/bosso/internal/server/calls.go',
  store: 'services/bosso/internal/db/store.go',
  proto: 'proto/bossanova/v1/orchestrator.proto',
  suite: 'services/bosso/api/orchestrator/cross_tenant_denial_test.go',
  dispatcher: 'services/bossd/api/upstream/command_dispatcher.go',
  table: 'docs/security/daemon-commands.md',
}
const rpc = (
  name = 'Good',
  options = ['authz_level) = AUTHZ_LEVEL_USER', 'entitlement) = ENTITLEMENT_FREE'],
) =>
  `rpc ${name}(Request) returns (Response) { ${options.map((value) => `option (bossanova.v1.${value};`).join('\n')} }`
const emptyAllowlist = () => ({
  schemaVersion: 1,
  systemAccess: [],
  rawOrganizationID: [],
  crossTenantExceptions: [],
})
const reviewedAllowlist = () => ({
  ...emptyAllowlist(),
  systemAccess: [
    {
      path: paths.calls,
      function: 'Run',
      reason: 'reviewed',
      count: 1,
      review: 'Global maintenance operation.',
    },
  ],
})

function fixture(t) {
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), 'tenant-scan-test-'))
  t.after(() => fs.rmSync(repo, { recursive: true, force: true, maxRetries: 5 }))
  const write = (name, content) => {
    const dest = path.join(repo, name)
    fs.mkdirSync(path.dirname(dest), { recursive: true })
    fs.writeFileSync(dest, content)
  }
  write(paths.calls, 'package server\nfunc Run() { policy.SystemAccess("reviewed") }\n')
  write(
    paths.store,
    'package db\nfunc (s *Store) Get(ctx context.Context, access policy.OrgAccess) {}\n',
  )
  write(paths.proto, `syntax = "proto3";\nservice OrchestratorService {\n${rpc()}\n}\n`)
  write(
    paths.suite,
    'package orchestrator\nvar crossTenantExceptions = map[string]string{}\nfunc TestCrossTenantDenial_EveryOrchestratorMethod(t *testing.T) {}\n',
  )
  write(
    paths.dispatcher,
    'package upstream\nfunc dispatch(cmd any) { switch cmd.(type) {\ncase *pb.OrchestratorCommand_Stop:\n} }\n',
  )
  write(
    paths.table,
    '# Commands\n## Command inventory\n\n| Command | Class |\n| --- | --- |\n| stop | Justified |\n',
  )
  return {
    repo,
    write,
    scan: (allowlist = reviewedAllowlist()) => scanRepository({ repo, allowlist }),
  }
}

function assertRecords(result, count, severity) {
  assert.equal(result.records.length, count, JSON.stringify(result.records, null, 2))
  for (const record of result.records) {
    assert.equal(record.source, 'tenant-scan')
    assert.equal(record.lens, 'tenant-access')
    if (severity) assert.equal(record.severity, severity)
  }
}

test('reviewed fixture has nonempty inventories and no findings', async (t) => {
  const result = await fixture(t).scan()
  assert.equal(result.schemaVersion, 1)
  assertRecords(result, 0)
  assert.equal(result.inventory.systemAccess.length, 1)
  assert.equal(result.inventory.declarations.length, 1)
  assert.equal(result.inventory.upstreamCommands.length, 1)
})

test('SystemAccess inventories literals, aliases, multiline calls and expressions while ignoring comments and tests', async (t) => {
  const f = fixture(t)
  f.write(
    paths.calls,
    `package server
func Run() {
 policy.SystemAccess("reviewed")
 // policy.SystemAccess("comment only")
 /* policy.SystemAccess("block comment") */
 alias.SystemAccess(
   "new literal",
 )
 policy.SystemAccess(reasonConstant)
 policy.SystemAccess(\x60raw reason\x60)
}
`,
  )
  f.write(
    'services/bosso/internal/server/calls_test.go',
    'func TestX() { policy.SystemAccess("test only") }',
  )
  const result = await f.scan()
  assertRecords(result, 3, 'medium')
  const calls = result.inventory.systemAccess
  assert.equal(calls.length, 4)
  assert.equal(calls.find((call) => call.reason === 'reasonConstant').reasonKind, 'expression')
  assert.ok(calls.some((call) => call.reason === 'new literal'))
  assert.ok(calls.some((call) => call.reason === 'raw reason'))
})

test('reusing an allowed reason above its reviewed count produces a finding', async (t) => {
  const f = fixture(t)
  f.write(
    paths.calls,
    'package server\nfunc Run() { policy.SystemAccess("reviewed"); policy.SystemAccess("reviewed") }\n',
  )
  const result = await f.scan()
  assertRecords(result, 1, 'medium')
  assert.equal(result.inventory.systemAccess[0].count, 2)
})

test('raw organization parameters in multiline methods are flagged, access-bearing methods are ignored', async (t) => {
  const f = fixture(t)
  f.write(
    paths.store,
    `package db
func (s *Store) Get(
 ctx context.Context,
 organizationID string,
) {}
func (s *Store) Safe(ctx context.Context, orgID string, access policy.OrgAccess) {}
func (s *Store) System(ctx context.Context, orgIDs []string, access policy.System) {}
func (s *Store) Batch(ctx context.Context, orgIDs []string) {}
`,
  )
  const result = await f.scan()
  assertRecords(result, 2, 'high')
  assert.deepEqual(result.inventory.rawOrganizationID.map((item) => item.symbol).sort(), [
    'Store.Batch',
    'Store.Get',
  ])
})

test('raw store methods can be reviewed explicitly', async (t) => {
  const f = fixture(t)
  f.write(
    paths.store,
    'package db\nfunc (s *Store) Resolve(ctx context.Context, workOSOrganizationID string) {}\n',
  )
  const allowlist = reviewedAllowlist()
  allowlist.rawOrganizationID.push({
    path: paths.store,
    symbol: 'Store.Resolve',
    review: 'Resolves an external identity before selecting a tenant.',
  })
  assertRecords(await f.scan(allowlist), 0)
})

test('sqlcgen imports and raw tenant SQL outside stores are findings including cmd and helpers', async (t) => {
  const f = fixture(t)
  f.write(
    'services/bosso/internal/server/bypass.go',
    'package server\nimport queries "github.com/recurser/bosso/internal/db/sqlcgen"\n',
  )
  f.write(
    'services/bosso/cmd/bypass.go',
    'package main\nconst query = `select * from sessions where organization_id = ?`\n',
  )
  f.write(
    'services/bosso/internal/testharness/bypass.go',
    'package testharness\nconst query = "update sessions set organization_id = ?"\n',
  )
  f.write(
    'services/bosso/internal/server/ignored_test.go',
    'const query = "delete from sessions where organization_id = ?"',
  )
  const result = await f.scan()
  assertRecords(result, 3, 'high')
})

for (const [name, options] of [
  ['authz', ['entitlement) = ENTITLEMENT_FREE']],
  ['entitlement', ['authz_level) = AUTHZ_LEVEL_USER']],
  [
    'unspecified authz',
    ['authz_level) = AUTHZ_LEVEL_UNSPECIFIED', 'entitlement) = ENTITLEMENT_FREE'],
  ],
  [
    'unspecified entitlement',
    ['authz_level) = AUTHZ_LEVEL_USER', 'entitlement) = ENTITLEMENT_UNSPECIFIED'],
  ],
]) {
  test(`missing or invalid ${name} declaration is a finding`, async (t) => {
    const f = fixture(t)
    f.write(paths.proto, `service OrchestratorService { ${rpc('Good', options)} }`)
    assertRecords(await f.scan(), 1, 'high')
  })
}

test('semicolon RPCs are inventoried and both missing declarations are reported', async (t) => {
  const f = fixture(t)
  f.write(paths.proto, 'service OrchestratorService { rpc Good(Request) returns (Response); }')
  const result = await f.scan()
  assert.equal(result.inventory.declarations.length, 1)
  assertRecords(result, 2, 'high')
})

test('unreviewed cross-tenant exceptions are findings and reviewed exceptions are suppressed', async (t) => {
  const f = fixture(t)
  f.write(
    paths.suite,
    'package orchestrator\nvar crossTenantExceptions = map[string]string{ "Good": "stream" }\nfunc TestCrossTenantDenial_EveryOrchestratorMethod(t *testing.T) {}\n',
  )
  assertRecords(await f.scan(), 1, 'medium')
  const allowlist = reviewedAllowlist()
  allowlist.crossTenantExceptions.push({
    method: 'Good',
    review: 'Stream covered by separate fixture.',
  })
  assertRecords(await f.scan(allowlist), 0)
})

test('a missing suite entry point produces a high severity finding', async (t) => {
  const f = fixture(t)
  f.write(paths.suite, 'package orchestrator\nvar crossTenantExceptions = map[string]string{}\n')
  assertRecords(await f.scan(), 1, 'high')
})

test('new upstream commands and invalid classes are findings while stale rows are counted', async (t) => {
  const f = fixture(t)
  f.write(
    paths.dispatcher,
    'package upstream\nfunc dispatch(cmd any) { switch cmd.(type) {\ncase *pb.OrchestratorCommand_Stop:\ncase *pb.OrchestratorCommand_NewCommand:\n} }\n',
  )
  f.write(
    paths.table,
    '## Command inventory\n| Command | Class |\n| --- | --- |\n| stop | |\n| retired_command | Gap |\n',
  )
  const result = await f.scan()
  assertRecords(result, 2, 'medium')
  assert.equal(result.counts.staleCommandRows, 1)
})

test('stale allowlist entries are counted without findings', async (t) => {
  const f = fixture(t)
  const allowlist = reviewedAllowlist()
  allowlist.systemAccess.push({
    path: paths.calls,
    function: 'Gone',
    reason: 'removed',
    count: 1,
    review: 'Previously reviewed grant.',
  })
  allowlist.rawOrganizationID.push({
    path: paths.store,
    symbol: 'Store.Gone',
    review: 'Retired lookup.',
  })
  allowlist.crossTenantExceptions.push({ method: 'Gone', review: 'Retired stream.' })
  const result = await f.scan(allowlist)
  assertRecords(result, 0)
  assert.equal(result.counts.staleAllowlistEntries, 3)
})

test('fingerprints survive line shifts and distinguish reasons, and all finding locations verify', async (t) => {
  const f = fixture(t)
  const source =
    'package server\nfunc Run() { policy.SystemAccess("reviewed"); policy.SystemAccess("alpha access reason"); policy.SystemAccess("beta access reason") }\n'
  f.write(paths.calls, source)
  const before = await f.scan()
  assertRecords(before, 2)
  assert.equal(new Set(before.records.map((record) => record.fingerprint)).size, 2)
  f.write(paths.calls, `\n\n${source}`)
  const after = await f.scan()
  assert.deepEqual(
    after.records.map((record) => record.fingerprint).sort(),
    before.records.map((record) => record.fingerprint).sort(),
  )
  for (const record of verifyLocations(after.records, { root: f.repo })) {
    assert.equal(record.locationStatus, 'location-verified', JSON.stringify(record.locations))
  }
})

function runCli(f, allowlist = reviewedAllowlist()) {
  const allowlistFile = path.join(f.repo, 'allowlist.json')
  const outputFile = path.join(f.repo, 'output', 'findings.json')
  fs.writeFileSync(allowlistFile, JSON.stringify(allowlist))
  const process = spawnSync(
    processExecPath,
    [scanner, 'scan', '--repo', f.repo, '--out', outputFile, '--allowlist', allowlistFile],
    { encoding: 'utf8' },
  )
  return { process, outputFile }
}
const processExecPath = process.execPath

for (const [name, mutate] of [
  [
    'unknown keys',
    (allowlist) => {
      allowlist.unrecognized = []
    },
  ],
  [
    'empty review',
    (allowlist) => {
      allowlist.systemAccess[0].review = ''
    },
  ],
  [
    'duplicate keys',
    (allowlist) => {
      allowlist.systemAccess.push({ ...allowlist.systemAccess[0] })
    },
  ],
  [
    'invalid count',
    (allowlist) => {
      allowlist.systemAccess[0].count = 0
    },
  ],
]) {
  test(`invalid allowlist (${name}) exits 2 and writes no output`, (t) => {
    const f = fixture(t)
    const allowlist = reviewedAllowlist()
    mutate(allowlist)
    const { process: result, outputFile } = runCli(f, allowlist)
    assert.equal(result.status, 2, result.stderr)
    assert.equal(fs.existsSync(outputFile), false)
  })
}

for (const [name, filename, replacement] of [
  ['SystemAccess calls', paths.calls, 'package server\n'],
  ['store files', paths.store, null],
  ['RPCs', paths.proto, 'service OrchestratorService {}'],
  ['cross-tenant map', paths.suite, 'package orchestrator\n'],
  ['dispatcher arms', paths.dispatcher, 'package upstream\n'],
  ['command rows', paths.table, '## Command inventory\n| Command | Class |\n| --- | --- |\n'],
]) {
  test(`empty ${name} inventory exits 2 and writes no output`, (t) => {
    const f = fixture(t)
    if (replacement === null) fs.unlinkSync(path.join(f.repo, filename))
    else f.write(filename, replacement)
    const { process: result, outputFile } = runCli(f)
    assert.equal(result.status, 2, result.stderr)
    assert.equal(fs.existsSync(outputFile), false)
  })
}

test('CLI writes a complete JSON report for a valid tree', (t) => {
  const f = fixture(t)
  const { process: result, outputFile } = runCli(f)
  assert.equal(result.status, 0, result.stderr)
  const report = JSON.parse(fs.readFileSync(outputFile, 'utf8'))
  assert.equal(report.schemaVersion, 1)
  assertRecords(report, 0)
})
