#!/usr/bin/env node
// Pure grouping and filing artifacts for bs-security. This module never writes to a tracker.
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { assertAcyclic, EPIC_MAX_CHILDREN } from '../../skills-toolbox/plan/plan-epic-lib.mjs'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scanText } from '../../skills-toolbox/plan/plan-secret-scan.mjs'
import {
  markerLine,
  mergeFindings,
  parseSecurityMarkers,
  titleStem,
} from './bs-security-findings.mjs'

export function validateGroups(groups, findings) {
  const errors = []
  const keys = new Set()
  const assignments = new Map(findings.map((f) => [f.fingerprint, []]))
  for (const group of groups) {
    if (keys.has(group.key)) errors.push(`duplicate group key: ${group.key}`)
    keys.add(group.key)
    if (!group.fingerprints.length) errors.push(`empty group: ${group.key}`)
    for (const fp of group.fingerprints) {
      if (!assignments.has(fp)) errors.push(`unknown finding ${fp} in ${group.key}`)
      else assignments.get(fp).push(group.key)
    }
  }
  for (const [fp, assigned] of assignments) {
    if (!assigned.length) errors.push(`unassigned finding: ${fp}`)
    if (assigned.length > 1)
      errors.push(`finding ${fp} assigned multiple times: ${assigned.join(', ')}`)
  }
  for (const group of groups) {
    for (const key of group.blockedByKeys ?? []) {
      if (!keys.has(key)) errors.push(`unknown group ${key} referenced by ${group.key}`)
    }
  }
  try {
    assertAcyclic({ children: groups })
  } catch (err) {
    errors.push(err.message)
  }
  return { ok: errors.length === 0, errors }
}

const findingsFor = (groups, findings) => {
  const fps = new Set(groups.flatMap((g) => g.fingerprints))
  return findings.filter((f) => fps.has(f.fingerprint))
}

export function priorityFor(group, findings) {
  return findingsFor([group], findings).reduce(
    (priority, f) =>
      Math.min(priority, { critical: 1, high: 1, medium: 2, low: 3, info: 3 }[f.severity] ?? 1),
    3,
  )
}

export function tierAndPartition(
  groups,
  findings,
  { cap = EPIC_MAX_CHILDREN, maxEpicsPerTier = 2 } = {},
) {
  if (
    !Number.isInteger(cap) ||
    cap < 1 ||
    !Number.isInteger(maxEpicsPerTier) ||
    maxEpicsPerTier < 1
  ) {
    throw new Error('cap and maxEpicsPerTier must be positive integers')
  }
  const validation = validateGroups(groups, findings)
  if (!validation.ok) throw new Error(validation.errors.join('; '))
  const byKey = new Map(groups.map((g) => [g.key, g]))
  const adjacency = new Map(groups.map((g) => [g.key, new Set()]))
  for (const group of groups) {
    for (const key of group.blockedByKeys ?? []) {
      adjacency.get(group.key).add(key)
      adjacency.get(key).add(group.key)
    }
  }
  const seen = new Set(),
    components = []
  for (const key of [...byKey.keys()].sort()) {
    if (seen.has(key)) continue
    const pending = [key],
      component = []
    while (pending.length) {
      const next = pending.pop()
      if (seen.has(next)) continue
      seen.add(next)
      component.push(byKey.get(next))
      pending.push(...adjacency.get(next))
    }
    component.sort((a, b) => a.key.localeCompare(b.key))
    if (component.length > cap)
      throw new Error(`component exceeds cap ${cap}: ${component.map((g) => g.key).join(', ')}`)
    const priority = Math.min(...component.map((g) => priorityFor(g, findings)))
    components.push({ groups: component, priority, tier: priority === 1 ? 'urgent' : 'hardening' })
  }
  components.sort(
    (a, b) =>
      a.priority - b.priority ||
      b.groups.length - a.groups.length ||
      a.groups[0].key.localeCompare(b.groups[0].key),
  )
  const epics = [],
    deferred = []
  for (const tier of ['urgent', 'hardening']) {
    const packed = []
    for (const component of components.filter((c) => c.tier === tier)) {
      let epic = packed.find((e) => e.groups.length + component.groups.length <= cap)
      if (!epic && packed.length < maxEpicsPerTier) {
        epic = { tier, priority: component.priority, groups: [] }
        packed.push(epic)
      }
      if (!epic) deferred.push(...component.groups)
      else {
        epic.groups.push(...component.groups)
        epic.priority = Math.min(epic.priority, component.priority)
      }
    }
    epics.push(...packed.map((e, i) => ({ ...e, index: i + 1, total: packed.length })))
  }
  return { epics, deferred }
}

export function epicTitle(date, tier, index, total) {
  return `Security review ${date}: ${tier === 'urgent' ? 'urgent fixes' : 'hardening'}${total > 1 ? ` (${index}/${total})` : ''}`
}

const REDACTED = '[REDACTED: credential-shaped text removed by bs-security]'
function redactText(value) {
  const text = String(value)
  const hits = scanText(text)
  // A multiline private key is one credential: discard its whole field, not just its header.
  if (hits.some((h) => h.kind === 'private-key')) return REDACTED
  const lines = new Set(hits.map((h) => h.line))
  return text
    .split('\n')
    .map((line, i) => (lines.has(i + 1) ? REDACTED : line))
    .join('\n')
}
function redact(value) {
  if (typeof value === 'string') return redactText(value)
  if (Array.isArray(value)) return value.map(redact)
  if (value && typeof value === 'object')
    return Object.fromEntries(Object.entries(value).map(([k, v]) => [redactText(k), redact(v)]))
  return value
}
const oneLine = (value) =>
  String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim()
const short = (value) => oneLine(value).slice(0, 400)

export function renderParentDescription(
  epic,
  findings,
  { codexOutcome = 'unknown', incompleteLenses = [], runDate = '' } = {},
) {
  const safe = redact({
    epic,
    findings: findingsFor(epic.groups, findings),
    codexOutcome,
    incompleteLenses,
    runDate,
  })
  const records = mergeFindings(safe.findings)
  const lines = [
    '## Summary',
    '',
    `Security review ${oneLine(safe.runDate)}`,
    ['critical', 'high', 'medium', 'low', 'info']
      .map((s) => `${s}: ${records.filter((f) => f.severity === s).length}`)
      .join(', '),
    `Codex cross-review: ${oneLine(safe.codexOutcome)}`,
  ]
  for (const lens of safe.incompleteLenses)
    lines.push(`lens ${oneLine(lens.lens ?? lens.id)}: incomplete (${oneLine(lens.reason)})`)
  lines.push('', '## Proposed decomposition')
  for (const group of safe.epic.groups) {
    lines.push(
      '',
      `### ${oneLine(group.title)}`,
      '',
      `Goal: ${oneLine(group.goal)}`,
      `Key paths: ${(group.keyPaths ?? []).map(oneLine).join(', ') || 'none'}`,
      `blockedBy: ${(group.blockedByKeys ?? []).map(oneLine).join(', ') || 'none'}`,
    )
    for (const f of findingsFor([group], records)) {
      const loc = f.locations?.[0]
      const where = `${loc?.path ?? f.primaryPath ?? 'unknown'}${loc?.line ? `:${loc.line}` : ''}`
      lines.push(
        '',
        `- ${oneLine(f.severity)}: ${oneLine(f.title)}`,
        `  - Location: ${oneLine(where)} (${oneLine(f.locationStatus ?? 'location-unverified')})`,
        `  - Verifier: ${oneLine(f.verifierStatus ?? 'not-selected')}`,
        `  - Defect: ${short(f.defect)}`,
        `  - Impact: ${short(f.impact)}`,
        `  - Fix: ${short(f.fix)}`,
      )
    }
  }
  lines.push('', '## Finding markers', '', ...records.map((f) => markerLine(f.fingerprint)))
  return `${lines.join('\n')}\n`
}

function sortedKeys(value) {
  if (Array.isArray(value)) return value.map(sortedKeys)
  if (value && typeof value === 'object')
    return Object.fromEntries(
      Object.keys(value)
        .sort()
        .map((k) => [k, sortedKeys(value[k])]),
    )
  return value
}
const stableJson = (value) => `${JSON.stringify(sortedKeys(redact(value)), null, 2)}\n`

export function serializeFindingsAttachment(
  epic,
  findings,
  { runDate = '', codexOutcome = 'unknown' } = {},
) {
  return stableJson({
    schemaVersion: 1,
    runDate,
    codexOutcome,
    groups: epic.groups,
    findings: mergeFindings(findingsFor(epic.groups, findings)),
  })
}

export function matchChildToGroup(childTitle, groups) {
  const child = new Set(titleStem(childTitle).split(' ').filter(Boolean))
  let best = null,
    score = 0
  for (const group of groups) {
    const tokens = new Set(titleStem(group.title).split(' ').filter(Boolean))
    const union = new Set([...child, ...tokens]).size
    const overlap = union ? [...tokens].filter((t) => child.has(t)).length / union : 0
    if (overlap >= 0.5 && overlap > score) {
      best = group
      score = overlap
    }
  }
  return best
}

export function selectForVerification(merged) {
  return merged
    .filter(
      (f) =>
        f.ledger?.status === 'regression-candidate' ||
        ((f.ledger?.status ?? 'new') === 'new' && ['critical', 'high'].includes(f.severity)),
    )
    .map((f) => f.fingerprint)
}

export function applyVerdicts(merged, verdicts = {}) {
  const selected = new Set(selectForVerification(merged))
  const findings = [],
    refuted = [],
    regressionsNotConfirmed = []
  let suppressed = 0
  for (const f of merged) {
    if (f.ledger?.status === 'known') {
      suppressed++
      continue
    }
    const verdict = verdicts[f.fingerprint]
    if (f.ledger?.status === 'regression-candidate') {
      if (verdict !== 'confirmed') {
        regressionsNotConfirmed.push(f)
        continue
      }
      findings.push({
        ...f,
        title: `Regression of ${(f.ledger.issues ?? []).join(', ')}: ${f.title}`,
        verifierStatus: 'confirmed',
      })
    } else if (verdict === 'refuted') refuted.push(f)
    else
      findings.push({
        ...f,
        verifierStatus: verdict ?? (selected.has(f.fingerprint) ? 'uncertain' : 'not-selected'),
      })
  }
  return { findings, refuted, regressionsNotConfirmed, suppressed }
}

class UsageError extends Error {}
class ValidationError extends Error {}
const COMMANDS = {
  'verify-queue': { required: ['merged'], optional: [] },
  plan: {
    required: ['groups', 'merged', 'date', 'out-dir'],
    optional: ['verdicts', 'codex-outcome', 'incomplete'],
  },
  'verify-markers': { required: ['expected', 'description'], optional: [] },
}
function parseCommand(argv) {
  const [cmd, ...args] = argv,
    spec = COMMANDS[cmd]
  if (!spec) throw new UsageError('expected verify-queue, plan or verify-markers')
  let values
  try {
    ;({ values } = parseArgs({
      args,
      strict: true,
      allowPositionals: false,
      options: Object.fromEntries(
        [...spec.required, ...spec.optional].map((k) => [k, { type: 'string' }]),
      ),
    }))
  } catch (err) {
    throw new UsageError(err.message)
  }
  for (const key of spec.required) if (!values[key]) throw new UsageError(`--${key} is required`)
  return { cmd, values }
}
const readJson = (file) => JSON.parse(fs.readFileSync(file, 'utf8'))
function recordsOf(doc) {
  const records = Array.isArray(doc) ? doc : doc?.records
  if (!Array.isArray(records))
    throw new UsageError('merged findings must be an array or {records: [...]}')
  return records
}

export function runCli(argv) {
  const { cmd, values } = parseCommand(argv)
  if (cmd === 'verify-queue') return selectForVerification(recordsOf(readJson(values.merged)))
  if (cmd === 'verify-markers') {
    const expected = new Set(readJson(values.expected).findings.map((f) => f.fingerprint))
    const actual = new Set(parseSecurityMarkers(fs.readFileSync(values.description, 'utf8')))
    if (expected.size !== actual.size || [...expected].some((fp) => !actual.has(fp)))
      throw new ValidationError('finding marker sets differ')
    return { ok: true }
  }
  const original = recordsOf(readJson(values.merged))
  const verdicts = values.verdicts ? readJson(values.verdicts) : {}
  for (const verdict of Object.values(verdicts)) {
    if (!['confirmed', 'refuted', 'uncertain'].includes(verdict))
      throw new UsageError('verdict must be confirmed, refuted or uncertain')
  }
  const applied = applyVerdicts(original, verdicts)
  const kept = new Set(applied.findings.map((f) => f.fingerprint)),
    all = new Set(original.map((f) => f.fingerprint))
  const inputGroups = readJson(values.groups)
  // Remove only findings the verifier/ledger excluded; unknown fingerprints still fail validation.
  const groups = inputGroups
    .map((g) => ({
      ...g,
      fingerprints: g.fingerprints.filter((fp) => kept.has(fp) || !all.has(fp)),
    }))
    .filter((g, i) => g.fingerprints.length || !inputGroups[i].fingerprints.length)
  const remainingKeys = new Set(groups.map((g) => g.key)),
    originalKeys = new Set(inputGroups.map((g) => g.key))
  for (const group of groups)
    group.blockedByKeys = (group.blockedByKeys ?? []).filter(
      (k) => remainingKeys.has(k) || !originalKeys.has(k),
    )
  const validation = validateGroups(groups, applied.findings)
  if (!validation.ok) throw new ValidationError(validation.errors.join('; '))
  let partition
  try {
    partition = tierAndPartition(groups, applied.findings)
  } catch (err) {
    throw new ValidationError(err.message)
  }
  const { epics, deferred } = partition
  const options = {
    runDate: values.date,
    codexOutcome: values['codex-outcome'] ?? 'unknown',
    incompleteLenses: values.incomplete ? readJson(values.incomplete) : [],
  }
  const summary = {
    epics: epics.map((epic, i) => ({
      title: epicTitle(values.date, epic.tier, epic.index, epic.total),
      priority: epic.priority,
      tier: epic.tier,
      groupKeys: epic.groups.map((g) => g.key),
      description: `epic-${i + 1}.md`,
      attachment: `epic-${i + 1}.findings.json`,
    })),
    deferred: deferred.map((g) => g.key),
    refuted: applied.refuted.length,
    suppressed: applied.suppressed,
    regressionsNotConfirmed: applied.regressionsNotConfirmed.length,
  }
  fs.mkdirSync(values['out-dir'], { recursive: true })
  epics.forEach((epic, i) => {
    fs.writeFileSync(
      path.join(values['out-dir'], `epic-${i + 1}.md`),
      renderParentDescription(epic, applied.findings, options),
    )
    fs.writeFileSync(
      path.join(values['out-dir'], `epic-${i + 1}.findings.json`),
      serializeFindingsAttachment(epic, applied.findings, options),
    )
  })
  fs.writeFileSync(path.join(values['out-dir'], 'epics.json'), stableJson(summary))
  return redact(summary)
}

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(`${JSON.stringify(runCli(process.argv.slice(2)), null, 2)}\n`)
  } catch (err) {
    // Validation errors can include reviewer-controlled group keys; redact diagnostics too.
    process.stderr.write(`bs-security-epics: ${redactText(err.message)}\n`)
    process.exitCode = err instanceof ValidationError ? 1 : 2
  }
}
