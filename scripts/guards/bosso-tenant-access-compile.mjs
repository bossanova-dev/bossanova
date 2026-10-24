#!/usr/bin/env node

// guard-id: bosso-tenant-access-compile

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

// Each entry owns one diagnostic line, so a permissive method cannot hide behind
// another method's expected compiler failure. Keep every tenant-scoped store method covered.
export const METHODS = [
  ['triggers.Get', 'org', '"id"'],
  ['triggers.Update', 'org', 'domain.UpdateTriggerParams{}'],
  ['triggers.Delete', 'org', '"id"'],
  ['triggers.SetSecretHash', 'org', '"id", "hash", time.Time{}'],
  ['triggers.ListForCreator', 'org', '"user"'],
  ['triggers.ListInOrganization', 'org', ''],
  ['triggers.SetDisabledForCreator', 'org', '"user"'],
  ['triggers.SetDisabledForRepo', 'org', '"repo"'],
  ['triggers.GetForSystem', 'system', '"id"'],
  ['triggers.GetByPublicID', 'system', '"public"'],
  ['configs.Get', 'org', '"id"'],
  ['configs.List', 'org', ''],
  ['configs.Delete', 'org', '"id"'],
  ['configs.GetByRepo', 'system', '"repo", "github"'],
  ['webhooks.Get', 'org', '"id"'],
  ['webhooks.List', 'org', ''],
  ['webhooks.Update', 'org', '"id", domain.UpdateSessionWebhookParams{}'],
  ['webhooks.SetSecret', 'org', '"id", "secret"'],
  ['webhooks.Delete', 'org', '"id"'],
  ['webhooks.Count', 'org', ''],
  ['webhooks.SetPendingDeliveriesCancelled', 'org', '"id"'],
  ['webhooks.ListSubscribedWebhooks', 'org', '[]string{"session.closed"}'],
  ['webhooks.ListDeliveries', 'org', '"id", domain.SessionWebhookDeliveryFilter{}, "", 1'],
  ['webhooks.GetDelivery', 'org', '"id"'],

  ['sessions.UpsertSessionInOrganization', 'org', '"daemon", domain.SessionRecord{}'],
  ['sessions.DeleteSessionInOrganization', 'org', '"id"'],
  ['sessions.DeleteUserSessionInOrganization', 'org', '"id"'],
  ['sessions.ReplaceDaemonSessionsInOrganization', 'org', '"daemon", nil'],
  ['sessions.DeleteDaemonSessionsInOrganization', 'org', '"daemon", time.Time{}'],
  ['sessions.ListSessionsInOrganization', 'org', 'db.SessionRecordFilter{}'],
  ['sessions.ListSessionRowsInOrganization', 'org', 'db.SessionRecordFilter{}'],
  ['sessions.CountSessionsInOrganization', 'org', ''],
  ['sessions.GetSessionInOrganization', 'org', '"id"'],
  ['sessions.ListSessionRefsInOrganization', 'org', ''],

  ['orgs.GetForMember', 'org', ''],
  ['orgs.Update', 'org', 'db.UpdateOrganizationParams{}'],
  ['orgs.SetWorkOSID', 'org', '"workos"'],
  ['orgs.Delete', 'org', ''],
  ['orgs.ListMembers', 'org', ''],
  ['orgs.DeleteMember', 'org', '"user"'],
  ['orgs.SetMemberRoleUnlessLastOwner', 'org', '"user", "member", nil'],
  ['orgs.DeleteMemberUnlessLastOwner', 'org', '"user", nil'],
  ['orgs.CountMembers', 'org', ''],
  ['orgs.Get', 'system', '"id"'],
  ['repos.Delete', 'org', '"origin"'],
  ['repos.ListInOrganization', 'org', ''],
  ['cloud.Create', 'org', 'cloudsession.Session{}, "key"'],
  ['cloud.Get', 'org', '"id"'],
  ['cloud.List', 'org', ''],
  ['cloud.CreateOperation', 'org', '"id", "key", cloudsession.Create'],
  ['cloud.UpdateProgress', 'org', '"id", "op", int64(1), cloudsession.Progress{}'],
  ['cloud.GetIsCloud', 'system', '"id"'],
  ['cloud.ListPending', 'system', ''],
  ['notes.CreateAPINote', 'org', 'domain.CreateOrganizationNoteParams{}'],
  ['notes.CreateIdempotentAPINote', 'org', '"key", domain.CreateOrganizationNoteParams{}'],
  ['notes.GetAPINoteByIdempotencyKey', 'org', '"user", "key", time.Time{}'],
  ['notes.UpsertSyncedNote', 'org', 'domain.UpsertSyncedOrganizationNoteParams{}'],
  [
    'notes.MarkSyncedNoteDeleted',
    'org',
    'domain.UpsertSyncedOrganizationNoteParams{}, time.Time{}',
  ],
  ['notes.GetSyncedNoteBySource', 'org', '"daemon", "note"'],
  ['notes.Get', 'org', '"id", time.Time{}'],
  ['notes.List', 'org', 'domain.ListOrganizationNotesParams{}, time.Time{}'],
  ['notes.UpdateAPINote', 'org', '"id", domain.UpdateOrganizationNoteParams{}, time.Time{}'],
  ['notes.DeleteNote', 'org', '"id", time.Time{}'],
  ['notes.MarkIngestionReserved', 'org', '1, 10, time.Time{}'],
  ['notes.MarkIngestionReleased', 'org', '1, time.Time{}'],
  ['notes.GetIngestionUsage', 'org', 'time.Time{}'],
  ['notes.DeleteExpired', 'system', 'time.Time{}, 10'],
  ['notes.DeleteIngestionWindowsBefore', 'system', 'time.Time{}'],
]

export function fixture(negative = false) {
  const lines = [
    'package tenantaccessfixture',
    'import (',
    '"context"',
    '"time"',
    '_ "github.com/recurser/bosso/api/orchestrator/notes"',
    '_ "github.com/recurser/bosso/api/orchestrator/triggers"',
    '_ "github.com/recurser/bosso/api/orchestrator/webhooks"',
    '"github.com/recurser/bosso/internal/db"',
    '"github.com/recurser/bosso/internal/domain"',
    '"github.com/recurser/bosso/internal/domain/cloudsession"',
    '"github.com/recurser/bosso/internal/domain/policy"',
    ')',
    'func check(ctx context.Context, sessions db.SessionRecordStore, cloud *db.SQLCloudSessionStore, notes *db.SQLOrganizationNoteStore, orgs *db.SQLOrganizationStore, repos *db.SQLRepoOrganizationStore, triggers *db.SQLTriggerStore, configs *db.SQLWebhookConfigStore, webhooks *db.SQLSessionWebhookStore) {',
    'org, _ := policy.DecideMember(nil, "org", false)',
    'system := policy.SystemAccess("compile guard positive control")',
    'var raw string = "org"',
    '_ = org; _ = system; _ = raw',
  ]
  const expected = []
  for (const [method, kind, tail] of METHODS) {
    lines.push(`${method}(ctx, ${negative ? 'raw' : kind}${tail ? ', ' + tail : ''})`)
    expected.push({
      line: lines.length,
      method,
      type: kind === 'org' ? 'policy.OrgAccess' : 'policy.System',
    })
  }
  lines.push(`_ = policy.SystemAccess(${negative ? 'raw' : '"constant reason"'})`)
  expected.push({ line: lines.length, method: 'policy.SystemAccess', type: 'policy.systemReason' })
  lines.push('}')
  return { source: lines.join('\n') + '\n', expected }
}

export function verifyDiagnostics(result, expected) {
  if (result.error || result.signal || result.status === null) {
    return [
      `compiler did not finish: ${result.error?.message || result.signal || 'unknown status'}`,
    ]
  }
  if (result.status === 0) return ['raw access fixture compiled successfully']
  const diagnostics = `${result.stdout || ''}\n${result.stderr || ''}`.split('\n')
  const failures = []
  const matched = new Set()
  for (const item of expected) {
    const index = diagnostics.findIndex(
      (line) =>
        line.includes(`fixture.go:${item.line}:`) &&
        line.includes('cannot use raw (variable of type string)') &&
        line.includes(`as ${item.type} value in argument to ${item.method}`),
    )
    if (index < 0)
      failures.push(
        `${item.method}: missing raw-string rejection as ${item.type} on line ${item.line}`,
      )
    else matched.add(index)
  }
  for (const [index, line] of diagnostics.entries()) {
    if (line.trim() && !line.startsWith('# ') && !matched.has(index)) {
      failures.push(`unexpected compiler output: ${line}`)
    }
  }
  return failures
}

/** @param {{ repoRoot?: string, negativeSource?: string }} [options] */
export function runGuard({ repoRoot = ROOT, negativeSource } = {}) {
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'bosso-tenant-access-'))
  try {
    const moduleRoot = path.join(repoRoot, 'services/bosso')
    const virtual = path.join(moduleRoot, 'internal/tenantaccessfixture/fixture.go')
    const replacement = path.join(temporary, 'fixture.go')
    const overlay = path.join(temporary, 'overlay.json')
    fs.writeFileSync(overlay, JSON.stringify({ Replace: { [virtual]: replacement } }))
    function compile(source) {
      fs.writeFileSync(replacement, source)
      return spawnSync('go', ['build', '-gcflags=-e', '-overlay', overlay, virtual], {
        cwd: moduleRoot,
        encoding: 'utf8',
        timeout: 180_000,
        maxBuffer: 8 * 1024 * 1024,
      })
    }
    const positive = compile(fixture().source)
    if (positive.status !== 0 || positive.error || positive.signal) {
      return [
        `positive control failed: ${positive.error?.message || positive.signal || positive.stderr || positive.stdout}`,
      ]
    }
    const negative = fixture(true)
    return verifyDiagnostics(compile(negativeSource ?? negative.source), negative.expected)
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true })
  }
}

if (isMainModule(import.meta.url)) {
  parseArgs({ options: {} })
  const failures = runGuard()
  if (failures.length) {
    console.error(failures.join('\n'))
    process.exitCode = 1
  } else {
    console.log(
      `bosso tenant access: positive control compiled; ${METHODS.length} raw store calls and nonconstant reason rejected`,
    )
  }
}
