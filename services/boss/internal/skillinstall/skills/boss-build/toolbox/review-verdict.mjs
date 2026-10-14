#!/usr/bin/env node
// review-verdict.mjs — the one contract between a review and everything that consumes it.
//
// A review (boss-review, or a repo skill that replaces it) writes `<outDir>/review-verdict.json`.
// boss-build, the ready gate's verdict note, PR publishing and follow-up routing read ONLY this
// file, through the verbs below. The stated `status` is never trusted: it is recomputed from the
// findings, and a contradiction is corrected and warned about.
//
//   validate  <file>                         exit 0 valid, 2 invalid (errors on stderr)
//   status    <file>                         prints pass|blocked|error; exit 2 when unreadable
//   render    <file> [--pr-section|--report] markdown; the report's first line is <!-- bs-review -->
//   followups <file>                         JSON candidates for followup-route.mjs
//   merge --out <file> <file>...             one verdict from a chain or a re-review; exit 2 on error
//
// Pure apart from the CLI wrapper. Node builtins only.
import defaultFs from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from './main-module.mjs'

export const VERDICT_FILENAME = 'review-verdict.json'
export const VERDICT_VERSION = 1
export const STATUSES = Object.freeze(['pass', 'blocked', 'error'])
export const SEVERITIES = Object.freeze(['P0', 'P1', 'P2', 'P3'])
export const SECOND_MODEL_AGENTS = Object.freeze(['codex', 'claude'])
export const REPORT_MARKER = '<!-- bs-review -->'

const SHA = /^[0-9a-f]{7,40}$/i
const SERIOUS = new Set(['P0', 'P1'])

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)
const text = (v) => typeof v === 'string' && v.trim() !== ''
const sha = (v) => typeof v === 'string' && SHA.test(v)

/**
 * Check and normalise a verdict. Cosmetic problems (severity case, a numeric-string line, a missing
 * `fixed`/`introduced`) are repaired toward the blocking reading and warned about; a structural
 * problem that cannot be repaired safely is an error.
 * @returns {{ok: boolean, errors: string[], warnings: string[], verdict: object|null}}
 */
export function validateVerdict(input) {
  const errors = []
  const warnings = []
  if (!isObject(input))
    return { ok: false, errors: ['verdict is not a JSON object'], warnings, verdict: null }
  const verdict = structuredClone(input)

  if (verdict.version !== VERDICT_VERSION) errors.push(`version must be ${VERDICT_VERSION}`)
  if (!STATUSES.includes(verdict.status)) errors.push(`status must be one of ${STATUSES.join('|')}`)
  if (verdict.status === 'error') {
    if (!text(verdict.error)) errors.push('an error verdict needs a non-empty error')
  } else {
    if (!sha(verdict.base)) errors.push('base must be a commit sha')
    if (!sha(verdict.head)) errors.push('head must be a commit sha')
  }

  if (verdict.findings === undefined && verdict.status === 'error') verdict.findings = []
  if (!Array.isArray(verdict.findings)) {
    errors.push('findings must be an array')
    verdict.findings = []
  }
  const ids = new Set()
  verdict.findings.forEach((finding, index) => {
    const at = `findings[${index}]`
    if (!isObject(finding)) {
      errors.push(`${at} is not an object`)
      return
    }
    if (!text(finding.id)) errors.push(`${at}.id must be a non-empty string`)
    else if (ids.has(finding.id)) errors.push(`${at}.id "${finding.id}" is not unique`)
    else ids.add(finding.id)
    if (
      typeof finding.severity === 'string' &&
      SEVERITIES.includes(finding.severity.trim().toUpperCase())
    ) {
      const normal = finding.severity.trim().toUpperCase()
      if (normal !== finding.severity)
        warnings.push(`${at}.severity "${finding.severity}" read as ${normal}`)
      finding.severity = normal
    } else errors.push(`${at}.severity must be one of ${SEVERITIES.join('|')}`)
    if (typeof finding.line === 'string' && /^\d+$/.test(finding.line.trim())) {
      warnings.push(`${at}.line "${finding.line}" read as a number`)
      finding.line = Number(finding.line.trim())
    }
    if (!Number.isInteger(finding.line) || finding.line < 1)
      errors.push(`${at}.line must be a positive integer`)
    for (const key of ['file', 'title', 'evidence'])
      if (!text(finding[key])) errors.push(`${at}.${key} must be a non-empty string`)
    // Provenance only: nothing decides on it, so a gap is labelled, never fatal.
    if (!text(finding.source)) {
      warnings.push(`${at}.source missing; read as "unknown"`)
      finding.source = 'unknown'
    }
    if (typeof finding.verified !== 'boolean') errors.push(`${at}.verified must be a boolean`)
    // Unknown means the blocking reading: introduced by this change, not yet fixed.
    if (typeof finding.introduced !== 'boolean') {
      warnings.push(`${at}.introduced missing; read as true`)
      finding.introduced = true
    }
    if (typeof finding.fixed !== 'boolean') {
      if (finding.fixed !== undefined) warnings.push(`${at}.fixed is not a boolean; read as false`)
      finding.fixed = false
    }
    if (finding.fixed && !sha(finding.fixCommit))
      warnings.push(`${at} is fixed but names no fixCommit`)
  })

  if (verdict.fixPass === undefined) verdict.fixPass = { ran: false }
  if (!isObject(verdict.fixPass) || typeof verdict.fixPass.ran !== 'boolean')
    errors.push('fixPass.ran must be a boolean')
  else if (verdict.fixPass.ran && !sha(verdict.fixPass.deltaBase))
    warnings.push('fixPass ran but names no deltaBase')

  if (verdict.secondModel === undefined) {
    warnings.push('secondModel missing; read as not run')
    verdict.secondModel = { ran: false, reason: 'not recorded' }
  }
  if (!isObject(verdict.secondModel) || typeof verdict.secondModel.ran !== 'boolean')
    errors.push('secondModel.ran must be a boolean')
  else if (verdict.secondModel.ran && !SECOND_MODEL_AGENTS.includes(verdict.secondModel.agent))
    warnings.push(`secondModel.agent should be one of ${SECOND_MODEL_AGENTS.join('|')}`)
  else if (!verdict.secondModel.ran && !text(verdict.secondModel.reason))
    warnings.push('secondModel did not run and names no reason')

  return { ok: errors.length === 0, errors, warnings, verdict: errors.length ? null : verdict }
}

/** A finding blocks only when it is verified, introduced by this change, P0/P1, and not fixed. */
export const isBlocking = (f) =>
  f.verified === true && f.introduced === true && SERIOUS.has(f.severity) && f.fixed !== true

/** Serious, verified, unfixed, but not this change's doing: a follow-up, never a blocker. */
export const isSeriousPreexisting = (f) =>
  f.verified === true && f.introduced === false && SERIOUS.has(f.severity) && f.fixed !== true

/** Verified, unfixed, and neither blocking nor a serious pre-existing follow-up. */
export const isUnapplied = (f) =>
  f.verified === true && f.fixed !== true && !SERIOUS.has(f.severity)

/**
 * The status the findings support. An `error` verdict stays an error; anything else is recomputed,
 * and a stated status that disagrees is corrected with a warning.
 * @returns {{status: 'pass'|'blocked'|'error', warnings: string[], errors: string[], verdict: object|null}}
 */
export function verdictStatus(input) {
  const checked = validateVerdict(input)
  if (!checked.ok)
    return { status: 'error', warnings: checked.warnings, errors: checked.errors, verdict: null }
  const { verdict } = checked
  if (verdict.status === 'error')
    return { status: 'error', warnings: checked.warnings, errors: [], verdict }
  const status = verdict.findings.some(isBlocking) ? 'blocked' : 'pass'
  const warnings = [...checked.warnings]
  if (status !== verdict.status)
    warnings.push(`stated status "${verdict.status}" contradicts the findings; using "${status}"`)
  return { status, warnings, errors: [], verdict: { ...verdict, status } }
}

// `do not merge` is reserved for boss-build's PARTIAL marker, so a finding may never spell it.
const oneLine = (value) =>
  String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim()
    .replace(/do not merge/gi, 'do-not-merge')
const where = (f) => `\`${f.file}:${f.line}\``
const short = (s) => (typeof s === 'string' ? s.slice(0, 12) : '?')
const item = (f) => `**${f.severity}** ${where(f)} — ${oneLine(f.title)}`

function secondModelLine(second) {
  if (second?.ran) return `ran (${second.agent ?? 'unknown agent'})`
  return `skipped: ${oneLine(second?.reason) || 'not recorded'}`
}

/**
 * PR-body sections. `## Review findings` appears only when something blocks (verify-gate reads its
 * presence as "not clean"); verified P2/P3 findings go to an `## Unapplied review findings`
 * checklist; `## Cross-model review` and `## Review coverage` always carry the token the review
 * earned. Serious pre-existing findings are not rendered here — `followups` routes them.
 */
export function renderPrSection(result) {
  const lines = []
  if (result.status === 'error') {
    const reason =
      oneLine(result.verdict?.error ?? result.errors.join('; ')) || 'unreadable verdict'
    lines.push('## Review findings', `- [ ] review did not produce a usable verdict: ${reason}`, '')
    lines.push('## Cross-model review', `error: ${reason}`, '')
    lines.push('## Review coverage', `none: review verdict unusable (${reason})`, '')
    return lines.join('\n')
  }
  const { findings, secondModel } = result.verdict
  const blocking = findings.filter(isBlocking)
  if (blocking.length) {
    lines.push('## Review findings')
    for (const f of blocking) lines.push(`- [ ] ${item(f)}: ${oneLine(f.evidence)}`)
    lines.push('')
  }
  const unapplied = findings.filter(isUnapplied)
  if (unapplied.length) {
    lines.push('## Unapplied review findings')
    for (const f of unapplied)
      lines.push(`- [ ] ${item(f)}${f.introduced ? '' : ' (pre-existing)'}: ${oneLine(f.evidence)}`)
    lines.push('')
  }
  lines.push('## Cross-model review', secondModelLine(secondModel), '')
  lines.push('## Review coverage', 'full', '')
  return lines.join('\n')
}

/**
 * One verdict from several (a review chain, or a later re-review of new commits). Any unusable
 * input makes the whole an error; otherwise findings are pooled (ids prefixed by position), the
 * range runs from the first base to the last head, and the status is recomputed over the pool.
 */
export function mergeVerdicts(results) {
  const list = Array.isArray(results) ? results : []
  if (list.length === 0)
    return { version: VERDICT_VERSION, status: 'error', error: 'no verdicts to merge' }
  const broken = list.filter((r) => r.status === 'error')
  if (broken.length) {
    const why = broken.map((r) => r.verdict?.error ?? r.errors.join('; ')).join('; ')
    return { version: VERDICT_VERSION, status: 'error', error: oneLine(why) || 'unusable verdict' }
  }
  const verdicts = list.map((r) => r.verdict)
  const findings = verdicts.flatMap((v, i) =>
    v.findings.map((f) => (list.length > 1 ? { ...f, id: `${i + 1}:${f.id}` } : f)),
  )
  const fixPass = verdicts.find((v) => v.fixPass?.ran)?.fixPass ?? { ran: false }
  const secondModel =
    verdicts.find((v) => v.secondModel?.ran)?.secondModel ?? verdicts[0].secondModel
  const merged = {
    version: VERDICT_VERSION,
    status: 'pass',
    base: verdicts[0].base,
    head: verdicts.at(-1).head,
    findings,
    fixPass,
    secondModel,
  }
  return { ...merged, status: merged.findings.some(isBlocking) ? 'blocked' : 'pass' }
}

/** The `<!-- bs-review -->` comment body. */
export function renderReport(result) {
  const lines = [REPORT_MARKER, `## boss-review: ${result.status}`, '']
  if (result.status === 'error') {
    const reason =
      oneLine(result.verdict?.error ?? result.errors.join('; ')) || 'unreadable verdict'
    lines.push(`The review did not produce a usable verdict: ${reason}`, '')
    return lines.join('\n')
  }
  const v = result.verdict
  const blocking = v.findings.filter(isBlocking)
  const fixed = v.findings.filter((f) => f.verified && f.fixed)
  const preexisting = v.findings.filter(isSeriousPreexisting)
  const unapplied = v.findings.filter(isUnapplied)
  lines.push(
    `Reviewed \`${short(v.base)}...${short(v.head)}\`: ${blocking.length} blocking, ${fixed.length} fixed, ` +
      `${preexisting.length} pre-existing, ${unapplied.length} unapplied.`,
    `Second model: ${secondModelLine(v.secondModel)}.`,
    `Fix pass: ${v.fixPass?.ran ? `ran; delta re-reviewed from \`${short(v.fixPass.deltaBase)}\`` : 'not run'}.`,
    '',
  )
  const section = (heading, list, render) => {
    if (!list.length) return
    lines.push(`### ${heading}`, ...list.map(render), '')
  }
  section('Blocking', blocking, (f) => `- ${item(f)}\n  - ${oneLine(f.evidence)}`)
  section(
    'Fixed in review',
    fixed,
    (f) => `- ${item(f)}${sha(f.fixCommit) ? ` (fixed in \`${short(f.fixCommit)}\`)` : ''}`,
  )
  section(
    'Pre-existing (not introduced by this change)',
    preexisting,
    (f) => `- ${item(f)}\n  - ${oneLine(f.evidence)}`,
  )
  section('Unapplied review findings', unapplied, (f) => `- [ ] ${item(f)}: ${oneLine(f.evidence)}`)
  return lines.join('\n')
}

/** Candidates for `followup-route.mjs route`: serious pre-existing findings, never nits. */
export function followUpCandidates(result) {
  if (result.status === 'error') return []
  return result.verdict.findings.filter(isSeriousPreexisting).map((f) => ({
    text: `${f.severity} ${f.file}:${f.line} — ${oneLine(f.title)}: ${oneLine(f.evidence)}`,
    source: 'review-finding',
    where: `${f.file}:${f.line}`,
  }))
}

export const USAGE =
  'usage: review-verdict.mjs validate|status|followups <file> | render <file> [--pr-section|--report] | merge --out <file> <file>...'

function readVerdict(fs, file) {
  try {
    return { input: JSON.parse(fs.readFileSync(file, 'utf8')) }
  } catch (error) {
    return { readError: `cannot read ${file}: ${error.message}` }
  }
}

/**
 * @param {string[]} argv
 * @param {{fs?: typeof defaultFs, stdout?: (s: string) => void, stderr?: (s: string) => void}} [deps]
 * @returns {number} exit code: 0, or 2 for a usage error or an unusable verdict
 */
export function main(argv, deps = {}) {
  const fs = deps.fs ?? defaultFs
  const stdout = deps.stdout ?? ((s) => process.stdout.write(s))
  const stderr = deps.stderr ?? ((s) => process.stderr.write(s))
  let parsed
  try {
    parsed = parseArgs({
      args: argv,
      options: {
        'pr-section': { type: 'boolean' },
        report: { type: 'boolean' },
        out: { type: 'string' },
      },
      allowPositionals: true,
    })
  } catch (error) {
    stderr(`review-verdict: ${error.message}\n${USAGE}\n`)
    return 2
  }
  const [command, file, ...extra] = parsed.positionals
  const load = (path) => {
    const { input, readError } = readVerdict(fs, path)
    return readError
      ? { status: 'error', warnings: [], errors: [readError], verdict: null }
      : verdictStatus(input)
  }
  if (command === 'merge') {
    if (!parsed.values.out || !file || parsed.values['pr-section'] || parsed.values.report) {
      stderr(`review-verdict: bad arguments\n${USAGE}\n`)
      return 2
    }
    const merged = mergeVerdicts([file, ...extra].map(load))
    try {
      fs.writeFileSync(parsed.values.out, `${JSON.stringify(merged, null, 2)}\n`)
    } catch (error) {
      stderr(`review-verdict: cannot write --out: ${error.message}\n`)
      return 2
    }
    stdout(`${merged.status}\n`)
    return merged.status === 'error' ? 2 : 0
  }
  const known = ['validate', 'status', 'render', 'followups']
  const renderFlags = parsed.values['pr-section'] || parsed.values.report
  if (
    !known.includes(command) ||
    !file ||
    extra.length ||
    parsed.values.out !== undefined ||
    (renderFlags && command !== 'render') ||
    (parsed.values['pr-section'] && parsed.values.report)
  ) {
    stderr(`review-verdict: bad arguments\n${USAGE}\n`)
    return 2
  }

  const result = load(file)
  for (const warning of result.warnings) stderr(`review-verdict: warning: ${warning}\n`)
  for (const error of result.errors) stderr(`review-verdict: ${error}\n`)
  const usable = result.errors.length === 0
  const code = usable ? 0 : 2

  if (command === 'validate') {
    if (usable) stdout('valid\n')
    return code
  }
  if (command === 'status') {
    stdout(`${result.status}\n`)
    return code
  }
  if (command === 'render') {
    stdout(`${parsed.values['pr-section'] ? renderPrSection(result) : renderReport(result)}\n`)
    return code
  }
  if (command === 'followups' && usable) stdout(`${JSON.stringify(followUpCandidates(result))}\n`)
  return code
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
