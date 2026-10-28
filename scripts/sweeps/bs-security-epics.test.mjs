import assert from 'node:assert/strict'
import { test } from 'node:test'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { scanText } from '../../skills-toolbox/plan/plan-secret-scan.mjs'
import {
  validateGroups,
  tierAndPartition,
  priorityFor,
  epicTitle,
  renderParentDescription,
  serializeFindingsAttachment,
  matchChildToGroup,
  selectForVerification,
  applyVerdicts,
} from './bs-security-epics.mjs'

const finding = (n, severity = 'low', status = 'new') => ({
  fingerprint: `sf-${n.toString(16).padStart(12, '0')}`,
  severity,
  title: `Synthetic defect ${n}`,
  defect: 'Unchecked input',
  impact: 'Unexpected access',
  fix: 'Validate input',
  source: 'claude',
  lens: 'auth',
  confidence: 'likely',
  locations: [{ path: 'src/auth.js', line: n }],
  locationStatus: 'location-verified',
  ledger: { status, issues: status === 'regression-candidate' ? ['TEST-7'] : [] },
})
const group = (f, extra = {}) => ({
  key: `g${f.fingerprint.slice(-4)}`,
  title: 'Validate session ownership',
  goal: 'Check ownership',
  keyPaths: ['src/auth.js'],
  fingerprints: [f.fingerprint],
  blockedByKeys: [],
  ...extra,
})
const many = (n, severity = 'low') => {
  const findings = Array.from({ length: n }, (_, i) => finding(i + 1, severity))
  return { findings, groups: findings.map((f) => group(f)) }
}
const run = (args) =>
  spawnSync(process.execPath, ['scripts/sweeps/bs-security-epics.mjs', ...args], {
    encoding: 'utf8',
  })

test('validateGroups reports unassigned, doubly assigned and dangling references together', () => {
  const a = finding(1),
    b = finding(2)
  const result = validateGroups(
    [
      group(a),
      group(a, {
        key: 'other',
        fingerprints: [a.fingerprint, 'sf-ffffffffffff'],
        blockedByKeys: ['missing'],
      }),
      group(a, { fingerprints: [] }),
    ],
    [a, b],
  )
  assert.equal(result.ok, false)
  for (const text of [
    'unassigned',
    'multiple',
    'unknown finding',
    'unknown group',
    'duplicate',
    'empty',
  ])
    assert.ok(
      result.errors.some((e) => e.includes(text)),
      text,
    )
})
test('validateGroups rejects a blockedBy cycle via assertAcyclic', () => {
  const { groups, findings } = many(2)
  groups[0].blockedByKeys = [groups[1].key]
  groups[1].blockedByKeys = [groups[0].key]
  assert.match(validateGroups(groups, findings).errors.join(), /cycle.*g0001.*g0002/)
})
test('tierAndPartition splits 13 independent groups into 12 + 1', () => {
  const { groups, findings } = many(13)
  assert.deepEqual(
    tierAndPartition(groups, findings).epics.map((e) => e.groups.length),
    [12, 1],
  )
})
test('tierAndPartition never splits a connected component', () => {
  const { groups, findings } = many(5)
  groups[4].blockedByKeys = [groups[0].key]
  const { epics } = tierAndPartition(groups, findings, { cap: 3 })
  assert.ok(epics.some((e) => e.groups.includes(groups[0]) && e.groups.includes(groups[4])))
})
test('tierAndPartition throws on a component larger than the cap', () => {
  const { groups, findings } = many(3)
  groups[0].blockedByKeys = [groups[1].key]
  groups[1].blockedByKeys = [groups[2].key]
  assert.throws(() => tierAndPartition(groups, findings, { cap: 2 }), /g0001.*g0002.*g0003/)
})
test('tierAndPartition keeps urgent and hardening groups in separate epics', () => {
  const findings = [finding(1, 'high'), finding(2)]
  const { epics } = tierAndPartition(
    findings.map((f) => group(f)),
    findings,
  )
  assert.deepEqual(
    epics.map((e) => e.tier),
    ['urgent', 'hardening'],
  )
})
test('tierAndPartition moves a cross-tier component to urgent', () => {
  const findings = [finding(1, 'high'), finding(2)]
  const groups = findings.map((f) => group(f))
  groups[1].blockedByKeys = [groups[0].key]
  const { epics } = tierAndPartition(groups, findings)
  assert.equal(epics.length, 1)
  assert.equal(epics[0].tier, 'urgent')
})
test('tierAndPartition returns overflow beyond maxEpicsPerTier as deferred', () => {
  const { groups, findings } = many(25)
  const r = tierAndPartition(groups, findings)
  assert.equal(r.epics.length, 2)
  assert.deepEqual(r.deferred, [groups[24]])
})
test('priorityFor maps severities and epic priority is the minimum', () => {
  for (const [severity, expected] of [
    ['critical', 1],
    ['high', 1],
    ['medium', 2],
    ['low', 3],
    ['info', 3],
  ]) {
    const f = finding(1, severity)
    assert.equal(priorityFor(group(f), [f]), expected)
  }
  const findings = [finding(1, 'low'), finding(2, 'medium')]
  assert.equal(
    tierAndPartition(
      findings.map((f) => group(f)),
      findings,
    ).epics[0].priority,
    2,
  )
})
test('epicTitle appends i/n only when there are siblings', () => {
  assert.equal(epicTitle('2026-10-13', 'urgent', 1, 1), 'Security review 2026-10-13: urgent fixes')
  assert.equal(
    epicTitle('2026-10-13', 'hardening', 1, 2),
    'Security review 2026-10-13: hardening (1/2)',
  )
})
test('renderParentDescription has one marker per finding and passes scanText', () => {
  const { groups, findings } = many(2)
  const text = renderParentDescription({ groups }, findings, {
    codexOutcome: 'ok',
    runDate: '2026-10-13',
    incompleteLenses: [{ lens: 'auth', reason: 'timeout' }],
  })
  assert.equal(text.split('SecurityFinding: ').length - 1, 2)
  assert.match(text, /Codex cross-review: ok/)
  assert.match(text, /lens auth: incomplete \(timeout\)/)
  assert.match(text, /location-verified/)
  assert.match(text, /uncertain|not-selected/)
  assert.deepEqual(scanText(text), [])
  assert.equal(text.includes('## Original notes'), false)
})
test('renderParentDescription redacts a credential-shaped line', () => {
  const { groups, findings } = many(1)
  const secret = 'gh' + 'p_' + 'A'.repeat(36)
  findings[0].defect = 'quoted ' + secret
  groups[0].goal = secret
  groups[0].keyPaths = [secret]
  const text = renderParentDescription({ groups }, findings, {})
  assert.ok(!text.includes(secret))
  assert.match(text, /REDACTED/)
  assert.deepEqual(scanText(text), [])
  const attachment = serializeFindingsAttachment({ groups }, findings, {})
  assert.ok(!attachment.includes(secret))
  assert.deepEqual(scanText(attachment), [])
})
test('serializeFindingsAttachment is byte-stable across runs', () => {
  const { groups, findings } = many(2)
  const options = { runDate: '2026-10-13', codexOutcome: 'ok' }
  const a = serializeFindingsAttachment({ groups }, findings, options)
  const b = serializeFindingsAttachment({ groups }, [...findings].reverse(), options)
  assert.equal(a, b)
  const doc = JSON.parse(a)
  assert.equal(doc.schemaVersion, 1)
  assert.deepEqual(doc.groups, groups)
  assert.equal(doc.findings.length, 2)
})
test('matchChildToGroup picks the best overlap and returns null below 0.5', () => {
  const groups = [{ title: 'Validate session ownership' }, { title: 'Validate input' }]
  assert.equal(matchChildToGroup('Validate the session ownership', groups), groups[0])
  assert.equal(matchChildToGroup('Replace build cache', groups), null)
})
test('selectForVerification picks every new finding and regression candidate, excluding leads', () => {
  const findings = [
    finding(1, 'critical'),
    finding(2, 'high'),
    finding(3),
    finding(4, 'low', 'regression-candidate'),
    finding(5, 'high', 'known'),
    finding(6, 'medium'),
    finding(7, 'info'),
    { ...finding(8), outcome: 'needs_validation', severity: null },
    { ...finding(9), ledger: undefined },
  ]
  assert.deepEqual(selectForVerification(findings), [
    findings[0].fingerprint,
    findings[1].fingerprint,
    findings[2].fingerprint,
    findings[3].fingerprint,
    findings[5].fingerprint,
    findings[6].fingerprint,
    findings[8].fingerprint,
  ])
})
test('applyVerdicts drops refuted, keeps uncertain and unverdicted, and files regressions only when confirmed', () => {
  const findings = [
    finding(1, 'high'),
    finding(2, 'high'),
    finding(3, 'high'),
    finding(4, 'low', 'regression-candidate'),
    finding(5, 'high', 'regression-candidate'),
    finding(6, 'low', 'known'),
  ]
  const verdicts = {
    [findings[0].fingerprint]: 'refuted',
    [findings[1].fingerprint]: 'uncertain',
    [findings[3].fingerprint]: 'confirmed',
  }
  const result = applyVerdicts(findings, verdicts)
  assert.deepEqual(
    result.findings.map((f) => f.fingerprint),
    [findings[1].fingerprint, findings[2].fingerprint, findings[3].fingerprint],
  )
  assert.equal(result.findings[1].verifierStatus, 'uncertain')
  assert.match(result.findings[2].title, /regression of TEST-7/i)
  assert.equal(result.refuted.length, 1)
  assert.equal(result.regressionsNotConfirmed.length, 1)
  assert.equal(result.suppressed, 1)
})
test('verify-markers fails when a read-back description lost a marker', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'security-epics-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const { groups, findings } = many(2)
  const expected = path.join(dir, 'expected.json'),
    description = path.join(dir, 'description.md')
  fs.writeFileSync(expected, serializeFindingsAttachment({ groups }, findings, {}))
  fs.writeFileSync(description, `SecurityFinding: ${findings[0].fingerprint}`)
  assert.equal(
    run(['verify-markers', '--expected', expected, '--description', description]).status,
    1,
  )
  fs.writeFileSync(
    description,
    findings
      .map((f) => `SecurityFinding: ${f.fingerprint}`)
      .reverse()
      .join('\n'),
  )
  assert.equal(
    run(['verify-markers', '--expected', expected, '--description', description]).status,
    0,
  )
})
test('plan CLI writes epics.json and one description per epic', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'security-epics-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const { groups, findings } = many(13)
  fs.writeFileSync(path.join(dir, 'groups.json'), JSON.stringify(groups))
  fs.writeFileSync(path.join(dir, 'merged.json'), JSON.stringify({ records: findings }))
  const r = run([
    'plan',
    '--groups',
    path.join(dir, 'groups.json'),
    '--merged',
    path.join(dir, 'merged.json'),
    '--date',
    '2026-10-13',
    '--out-dir',
    path.join(dir, 'out'),
  ])
  assert.equal(r.status, 0, r.stderr)
  const summary = JSON.parse(fs.readFileSync(path.join(dir, 'out/epics.json')))
  assert.equal(summary.epics.length, 2)
  for (const n of [1, 2]) {
    assert.ok(fs.existsSync(path.join(dir, `out/epic-${n}.md`)))
    assert.ok(fs.existsSync(path.join(dir, `out/epic-${n}.findings.json`)))
  }
})
test('plan removes excluded findings and empty groups before validation', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'security-epics-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const findings = [finding(1, 'high', 'known'), finding(2, 'high')],
    groups = findings.map((f) => group(f))
  fs.writeFileSync(path.join(dir, 'groups.json'), JSON.stringify(groups))
  fs.writeFileSync(path.join(dir, 'merged.json'), JSON.stringify(findings))
  fs.writeFileSync(
    path.join(dir, 'verdicts.json'),
    JSON.stringify({ [findings[1].fingerprint]: 'refuted' }),
  )
  const r = run([
    'plan',
    '--groups',
    path.join(dir, 'groups.json'),
    '--merged',
    path.join(dir, 'merged.json'),
    '--verdicts',
    path.join(dir, 'verdicts.json'),
    '--date',
    '2026-10-13',
    '--out-dir',
    path.join(dir, 'out'),
  ])
  assert.equal(r.status, 0, r.stderr)
  assert.equal(JSON.parse(r.stdout).epics.length, 0)
})
test('CLI distinguishes usage from invalid grouping', () => {
  assert.equal(run(['plan', '--unknown']).status, 2)
  assert.equal(run(['verify-queue']).status, 2)
})

test('redaction removes multiline private-key bodies from attachments and descriptions', () => {
  const { groups, findings } = many(1)
  const body = 'Q'.repeat(80)
  findings[0].evidence =
    '-----BEGIN ' + 'PRIVATE KEY-----\n' + body + '\n-----END ' + 'PRIVATE KEY-----'
  findings[0].impact = findings[0].evidence
  for (const output of [
    renderParentDescription({ groups }, findings, {}),
    serializeFindingsAttachment({ groups }, findings, {}),
  ]) {
    assert.ok(!output.includes(body))
    assert.deepEqual(scanText(output), [])
  }
})
test('rendered summaries truncate text after redaction while attachments retain safe full text', () => {
  const { groups, findings } = many(1)
  findings[0].defect = 'D'.repeat(450)
  const text = renderParentDescription({ groups }, findings, {})
  assert.ok(text.includes('Defect: ' + 'D'.repeat(400)))
  assert.ok(!text.includes('D'.repeat(401)))
  assert.equal(
    JSON.parse(serializeFindingsAttachment({ groups }, findings, {})).findings[0].defect.length,
    450,
  )
})
test('CLI rejects invalid groups with exit 1 and emits the verification queue', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'security-epics-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const findings = [finding(1, 'high')]
  fs.writeFileSync(path.join(dir, 'merged.json'), JSON.stringify({ records: findings }))
  fs.writeFileSync(path.join(dir, 'groups.json'), '[]')
  const r = run([
    'plan',
    '--groups',
    path.join(dir, 'groups.json'),
    '--merged',
    path.join(dir, 'merged.json'),
    '--date',
    '2026-10-13',
    '--out-dir',
    path.join(dir, 'out'),
  ])
  assert.equal(r.status, 1)
  assert.match(r.stderr, /unassigned finding/)
  const queue = run(['verify-queue', '--merged', path.join(dir, 'merged.json')])
  assert.equal(queue.status, 0)
  assert.deepEqual(JSON.parse(queue.stdout), [findings[0].fingerprint])
})

test('verdicts keep all unverified severities uncertain and never promote reviewer leads', () => {
  const records = ['critical', 'high', 'medium', 'low', 'info'].map((s, i) => finding(i + 1, s))
  const applied = applyVerdicts(records)
  assert.ok(applied.findings.every((f) => f.verifierStatus === 'uncertain'))
  const lead = {
    ...finding(10),
    outcome: 'needs_validation',
    severity: null,
    unresolved: 'Proxy config',
  }
  for (const verdict of [
    undefined,
    'confirmed',
    'uncertain',
    'needs_validation',
    { status: 'confirmed', severity: 'high' },
  ]) {
    const result = applyVerdicts([lead], { [lead.fingerprint]: verdict })
    assert.equal(result.findings.length, 0)
    assert.equal(result.needsValidation.length, 1)
  }
  assert.equal(applyVerdicts([lead], { [lead.fingerprint]: 'refuted' }).refuted.length, 1)
  assert.equal(
    applyVerdicts([finding(1)], { [finding(1).fingerprint]: 'needs_validation' }).needsValidation
      .length,
    1,
  )
})

test('object verdict lowers severity and priority, never raises severity', () => {
  const f = finding(1, 'high')
  const lowered = applyVerdicts([f], {
    [f.fingerprint]: { status: 'confirmed', severity: 'medium' },
  }).findings[0]
  assert.equal(lowered.severity, 'medium')
  assert.equal(lowered.severityLoweredFrom, 'high')
  assert.equal(priorityFor(group(lowered), [lowered]), 2)
  assert.match(
    renderParentDescription({ groups: [group(lowered)] }, [lowered]),
    /medium \(reviewer: high\)/,
  )
  const higher = applyVerdicts([finding(2)], {
    [finding(2).fingerprint]: { status: 'confirmed', severity: 'critical' },
  }).findings[0]
  assert.equal(higher.severity, 'low')
  assert.equal(higher.severityLoweredFrom, undefined)
  const regression = finding(3, 'low', 'regression-candidate')
  assert.equal(
    applyVerdicts([regression], { [regression.fingerprint]: { status: 'confirmed' } }).findings
      .length,
    1,
  )
})

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'security-verdicts-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  return {
    dir,
    write(name, value) {
      const file = path.join(dir, name)
      fs.writeFileSync(file, JSON.stringify(value))
      return file
    },
  }
}

test('plan reports severity-less leads and excludes them from descriptions and attachments', (t) => {
  const { dir, write } = fixture(t)
  const records = [
    finding(1),
    finding(2, 'high'),
    {
      ...finding(3),
      outcome: 'needs_validation',
      severity: null,
      unresolved: 'Deployment setting',
    },
  ]
  const verdicts = { [records[1].fingerprint]: 'needs_validation' }
  const args = [
    'plan',
    '--groups',
    write(
      'groups.json',
      records.map((f) => group(f)),
    ),
    '--merged',
    write('merged.json', records),
    '--verdicts',
    write('verdicts.json', verdicts),
    '--evidence',
    write('evidence.json', { [records[1].fingerprint]: 'Provider policy unknown' }),
    '--date',
    '2026-10-27',
    '--out-dir',
    path.join(dir, 'out'),
  ]
  const result = run(args)
  assert.equal(result.status, 0, result.stderr)
  const summary = JSON.parse(result.stdout)
  assert.equal(summary.needsValidation.length, 2)
  assert.deepEqual(summary, JSON.parse(fs.readFileSync(path.join(dir, 'out/epics.json'))))
  assert.ok(summary.needsValidation.every((lead) => !Object.hasOwn(lead, 'severity')))
  assert.deepEqual(
    summary.needsValidation.map((lead) => lead.unresolved),
    ['Provider policy unknown', 'Deployment setting'],
  )
  for (const file of ['epic-1.md', 'epic-1.findings.json']) {
    const text = fs.readFileSync(path.join(dir, 'out', file), 'utf8')
    for (const lead of records.slice(1)) assert.ok(!text.includes(lead.fingerprint))
  }
  for (const invalid of [
    'bad',
    { status: 'bad' },
    { status: 'confirmed', severity: 'extreme' },
    { status: 'confirmed', severity: null },
  ]) {
    write('verdicts.json', { [records[0].fingerprint]: invalid })
    assert.equal(run(args).status, 2)
  }
})

test('queue batches preserve connected pairs and merged order, including oversized clusters', (t) => {
  const { write } = fixture(t)
  const records = Array.from({ length: 7 }, (_, i) => finding(i + 1))
  const fps = records.map((f) => f.fingerprint)
  const merged = write('merged.json', {
    records,
    nearDuplicates: [{ fingerprints: [fps[0], fps[3]] }, { fingerprints: [fps[3], fps[5]] }],
  })
  const pairs = write('pairs.json', [[fps[1], fps[4]]])
  const flat = run(['verify-queue', '--merged', merged])
  assert.equal(flat.stdout, JSON.stringify(fps, null, 2) + '\n')
  const result = run(['verify-queue', '--merged', merged, '--batch-size', '2', '--pairs', pairs])
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(result.stdout), [
    [fps[0], fps[3], fps[5]],
    [fps[1], fps[4]],
    [fps[2], fps[6]],
  ])
  assert.equal(run(['verify-queue', '--merged', merged, '--batch-size', '0']).status, 2)
})

test('merge-verdicts isolates missing and invalid batches and discards off-batch evidence', (t) => {
  const { dir, write } = fixture(t)
  const fps = Array.from({ length: 4 }, (_, i) => finding(i + 1).fingerprint)
  const batches = write(
    'batches.json',
    fps.map((fp) => [fp]),
  )
  write('verdicts-0.json', {
    [fps[0]]: { status: 'confirmed', severity: 'low' },
    [fps[1]]: 'confirmed',
  })
  write('evidence-0.json', { [fps[0]]: 'Current code checked', [fps[1]]: 'Off batch' })
  write('verdicts-2.json', { [fps[2]]: 'invalid' })
  write('verdicts-3.json', { [fps[3]]: 'needs_validation' })
  write('evidence-3.json', { [fps[3]]: 'Deployment not visible' })
  const out = path.join(dir, 'verdicts.json'),
    evidence = path.join(dir, 'evidence.json')
  const result = run([
    'merge-verdicts',
    '--batches',
    batches,
    '--dir',
    dir,
    '--out',
    out,
    '--evidence-out',
    evidence,
  ])
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(fs.readFileSync(out)), {
    [fps[0]]: { status: 'confirmed', severity: 'low' },
    [fps[3]]: 'needs_validation',
  })
  assert.deepEqual(JSON.parse(fs.readFileSync(evidence)), {
    [fps[0]]: 'Current code checked',
    [fps[3]]: 'Deployment not visible',
  })
  const summary = JSON.parse(result.stdout)
  assert.equal(summary.ignoredOffBatch, 2)
  assert.equal(summary.incompleteBatches.length, 2)
})
