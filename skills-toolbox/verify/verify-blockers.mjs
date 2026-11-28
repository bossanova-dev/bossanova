import { createHash } from 'node:crypto'
import { parseArgs } from 'node:util'

export const BLOCKER_KINDS = Object.freeze([
  'operator-note',
  'review-finding',
  'check',
  'conflict',
  'no-review',
])
export const BLOCKING_SEVERITIES = Object.freeze(['P0', 'P1', 'P2'])
const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
const text = (value) => typeof value === 'string' && value.trim() !== ''
const line = (value) => typeof value === 'string' && value.trim() !== '' && !/[\r\n]/.test(value)
const sha = (value) => typeof value === 'string' && /^[a-f0-9]{40}$/i.test(value)
const normalized = (value) => (typeof value === 'string' ? value.trim().toUpperCase() : '')
const oneLine = (value) =>
  String(value ?? '')
    .replace(/[\r\n]+/g, ' ')
    .trim()
const compare = (a, b) => (a < b ? -1 : a > b ? 1 : 0)

export const MAX_OPERATOR_NOTE_CHARS = 2000

export const isValidOperatorText = (note) =>
  text(note) &&
  Array.from(note).length <= MAX_OPERATOR_NOTE_CHARS &&
  !/[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/u.test(note)

/** Preserve the operator's text verbatim as evidence, never as executable instructions. */
export function operatorNoteBlocker(note) {
  if (!isValidOperatorText(note)) throw new TypeError('invalid operator note')
  return {
    id: 'operator-note',
    kind: 'operator-note',
    severity: null,
    file: null,
    line: null,
    summary: 'the operator sent this head back',
    text: note,
  }
}

/** Explicit P3 or style findings are notes, regardless of which producer emitted them. */
export const isNoteFinding = (finding) =>
  ['P3', 'STYLE'].includes(normalized(finding?.severity)) ||
  normalized(finding?.category) === 'STYLE'

/** The single blocking rule for review findings, shared by build and verify. */
export const isBlockingFinding = (finding) =>
  finding?.verified === true &&
  finding?.introduced === true &&
  finding?.fixed !== true &&
  !isNoteFinding(finding) &&
  BLOCKING_SEVERITIES.includes(normalized(finding?.severity))

function blockerOrder(a, b) {
  return (
    BLOCKER_KINDS.indexOf(a.kind) - BLOCKER_KINDS.indexOf(b.kind) ||
    compare(a.severity ?? '', b.severity ?? '') ||
    compare(a.file ?? '', b.file ?? '') ||
    (a.line ?? 0) - (b.line ?? 0) ||
    compare(a.summary, b.summary) ||
    compare(a.id, b.id)
  )
}

export const findingHash = (finding) =>
  createHash('sha256')
    .update([finding.file ?? '', finding.line ?? '', finding.title].join('\0'))
    .digest('hex')
    .slice(0, 12)

const reviewId = (finding) => 'review:' + findingHash(finding)

/** The caller supplies settled failed checks, classified by the shared CI classifier. */
export function buildBlockers({
  reviewFindings = [],
  failedChecks = [],
  conflict = false,
  partial = false,
} = {}) {
  const blockers = []
  if (!partial)
    for (const finding of reviewFindings) {
      const severity = normalized(finding?.severity)
      if (!isBlockingFinding(finding)) continue
      blockers.push({
        id: reviewId(finding),
        kind: 'review-finding',
        severity,
        file: text(finding.file) ? finding.file : null,
        line:
          text(finding.file) && Number.isSafeInteger(finding.line) && finding.line > 0
            ? finding.line
            : null,
        summary: oneLine(finding.title),
        text: String(finding.evidence ?? ''),
      })
    }
  for (const check of failedChecks) {
    const name = oneLine(typeof check === 'string' ? check : check?.name)
    if (!name || name.startsWith('boss/')) continue
    blockers.push({
      id: `check:${name}`,
      kind: 'check',
      severity: null,
      file: null,
      line: null,
      summary: name,
      text: typeof check === 'string' ? name : String(check.text ?? name),
    })
  }
  if (conflict)
    blockers.push({
      id: 'conflict',
      kind: 'conflict',
      severity: null,
      file: null,
      line: null,
      summary: 'the head conflicts with its base',
      text: 'Rebase the head onto its base and resolve the merge conflicts.',
    })
  // Sorting before deduplication makes repeated input order irrelevant, including differing evidence.
  blockers.sort((a, b) => blockerOrder(a, b) || compare(a.text, b.text))
  return blockers.filter(
    (blocker, index) => blockers.findIndex((b) => b.id === blocker.id) === index,
  )
}

const exactKeys = (value, keys) =>
  object(value) &&
  Object.keys(value).length === keys.length &&
  keys.every((key) => Object.hasOwn(value, key))

/** Reject malformed files at both writing and reading boundaries; never partially accept a list. */
export function validateBlockerFile(input) {
  const errors = []
  if (!exactKeys(input, ['version', 'pr', 'head', 'blockers']))
    return { ok: false, errors: ['expected version, pr, head and blockers'] }
  if (input.version !== 1) errors.push('version must be 1')
  if (!Number.isSafeInteger(input.pr) || input.pr < 1) errors.push('pr must be a positive integer')
  if (!sha(input.head)) errors.push('head must be a 40-hex SHA')
  if (!Array.isArray(input.blockers) || input.blockers.length === 0)
    return { ok: false, errors: [...errors, 'blockers must be a non-empty array'] }
  const ids = new Set()
  input.blockers.forEach((b, index) => {
    const at = `blockers[${index}]`
    if (!exactKeys(b, ['id', 'kind', 'severity', 'file', 'line', 'summary', 'text'])) {
      errors.push(`${at} has invalid fields`)
      return
    }
    if (!BLOCKER_KINDS.includes(b.kind)) errors.push(`${at} has invalid kind`)
    if (!text(b.id) || ids.has(b.id)) errors.push(`${at} has invalid or duplicate id`)
    ids.add(b.id)
    if (!text(b.summary) || /[\r\n]/.test(b.summary)) errors.push(`${at}.summary must be one line`)
    if (typeof b.text !== 'string') errors.push(`${at}.text must be a string`)
    if (b.kind === 'operator-note' && !isValidOperatorText(b.text))
      errors.push(`${at} has invalid operator note`)
    if (b.kind === 'review-finding') {
      if (typeof b.id !== 'string' || !/^review:[a-f0-9]{12}$/.test(b.id))
        errors.push(`${at} has invalid review id`)
      if (!BLOCKING_SEVERITIES.includes(b.severity)) errors.push(`${at} has invalid severity`)
      if (b.file !== null && !text(b.file)) errors.push(`${at} has invalid file`)
      if (b.line !== null && (!Number.isSafeInteger(b.line) || b.line < 1 || b.file === null))
        errors.push(`${at} has invalid line`)
    } else {
      if (b.severity !== null || b.file !== null || b.line !== null)
        errors.push(`${at} has unexpected location or severity`)
      if (
        b.kind === 'check'
          ? b.id !== `check:${b.summary}` || !text(b.summary) || b.summary.startsWith('boss/')
          : b.id !== b.kind
      )
        errors.push(`${at} has invalid id`)
    }
    const previous = input.blockers[index - 1]
    if (index && object(previous) && blockerOrder(previous, b) > 0)
      errors.push(`${at} is out of order`)
  })
  return { ok: errors.length === 0, errors }
}

export function blockedDescription(blockers, parked = false) {
  const parts = blockers.some((b) => b.kind === 'operator-note') ? ['operator note'] : []
  parts.push(
    ...BLOCKING_SEVERITIES.flatMap((severity) => {
      const count = blockers.filter((b) => b.severity === severity).length
      return count ? [`${count} ${severity}`] : []
    }),
  )
  const checks = blockers.filter((b) => b.kind === 'check').length
  if (checks) parts.push(`${checks} failing check${checks === 1 ? '' : 's'}`)
  if (blockers.some((b) => b.kind === 'conflict')) parts.push('conflict')
  if (blockers.some((b) => b.kind === 'no-review')) parts.push('no review')
  return `blocked: ${parts.join(', ')}${parked ? ', needs human' : ''}`
}

export function encodeBlockerTrailer(file) {
  const checked = validateBlockerFile(file)
  if (!checked.ok) throw new Error(checked.errors.join('; '))
  const encoded = Buffer.from(JSON.stringify(file)).toString('base64url')
  return `<!-- boss-verify-blockers head=${file.head} v1 ${encoded} -->`
}

/** Only a well-formed final trailer can stand for a comment; trust its author separately. */
export function parseBlockerTrailer(body) {
  if (typeof body !== 'string') return null
  const match = body
    .trimEnd()
    .match(/(?:^|\n)<!-- boss-verify-blockers head=([a-f0-9]{40}) v1 ([A-Za-z0-9_-]+) -->$/i)
  if (!match) return null
  try {
    const bytes = Buffer.from(match[2], 'base64url')
    if (bytes.toString('base64url') !== match[2]) return null
    const file = JSON.parse(bytes.toString('utf8'))
    return validateBlockerFile(file).ok && file.head === match[1] ? file : null
  } catch {
    return null
  }
}

/** Validate repair outcomes independently of Git state or the originating blocker file. */
export function validateOutcomeFile(input) {
  const errors = []
  if (
    !input ||
    input.version !== 1 ||
    !sha(input.blockersHead) ||
    !sha(input.pushedHead) ||
    !Array.isArray(input.outcomes)
  )
    return { ok: false, errors: ['expected version 1, blockersHead, pushedHead and outcomes'] }
  const ids = new Set()
  for (const o of input.outcomes) {
    if (!o || !line(o.id) || ids.has(o.id)) {
      errors.push('invalid or duplicate id')
      continue
    }
    ids.add(o.id)
    if (o.outcome === 'fixed') {
      if (!sha(o.commit)) errors.push(`${o.id}: fixed requires a 40-hex commit`)
    } else if (o.outcome === 'not-fixable') {
      if (!line(o.reason)) errors.push(`${o.id}: reason must be one non-empty line`)
    } else errors.push(`${o.id}: invalid outcome`)
  }
  return { ok: errors.length === 0, errors }
}

export function encodeOutcomesTrailer(file) {
  const checked = validateOutcomeFile(file)
  if (!checked.ok) throw new Error(checked.errors.join('; '))
  const encoded = Buffer.from(JSON.stringify(file)).toString('base64url')
  return `<!-- boss-repair-outcomes blockers-head=${file.blockersHead} head=${file.pushedHead} v1 ${encoded} -->`
}

/** Only accept a validated final trailer; the caller must establish author trust separately. */
export function parseOutcomesTrailer(body) {
  if (typeof body !== 'string') return null
  const match = body
    .trimEnd()
    .match(
      /(?:^|\n)<!-- boss-repair-outcomes blockers-head=([a-f0-9]{40}) head=([a-f0-9]{40}) v1 ([A-Za-z0-9_-]+) -->$/i,
    )
  if (!match) return null
  try {
    const bytes = Buffer.from(match[3], 'base64url')
    if (bytes.toString('base64url') !== match[3]) return null
    const file = JSON.parse(bytes.toString('utf8'))
    return validateOutcomeFile(file).ok &&
      file.blockersHead === match[1] &&
      file.pushedHead === match[2]
      ? file
      : null
  } catch {
    return null
  }
}

function blockerLabel(blocker, location) {
  switch (blocker.kind) {
    case 'operator-note':
      return '**operator note**'
    case 'review-finding':
      return `**${blocker.severity}**${location} — ${blocker.summary}`
    case 'check':
      return `**check** ${blocker.summary}`
    case 'conflict':
      return '**conflict**'
    default:
      return '**no review**'
  }
}

export function blockedComment(
  file,
  marker = `boss-verify head ${file.head}`,
  acceptedSection = [],
  mention = '',
) {
  const trailer = encodeBlockerTrailer(file)
  const render = (limit) => {
    const lines = [
      ...(mention ? [mention, ''] : []),
      `### Verification found ${file.blockers.length} blocker(s) on \`${file.head.slice(0, 12)}\``,
      '',
    ]
    for (const b of file.blockers) {
      const location = b.file ? ` \`${b.file}${b.line === null ? '' : `:${b.line}`}\`` : ''
      const label = blockerLabel(b, location)
      const visible = b.text.length > limit ? b.text.slice(0, limit) + '…' : b.text
      const longest = Math.max(0, ...Array.from(b.text.matchAll(/`+/g), (m) => m[0].length))
      const fence = '`'.repeat(Math.max(3, longest + 1))
      const evidence = b.kind === 'operator-note' ? `${fence}\n${visible}\n${fence}` : visible
      lines.push(
        `- ${label} (\`${b.id}\`)`,
        '',
        '  <details>',
        '  <summary>Details</summary>',
        '',
        ...evidence.split('\n').map((line) => `  ${line}`),
        '',
        '  </details>',
        '',
      )
    }
    if (acceptedSection.length)
      lines.push('#### Accepted by the operator', '', ...acceptedSection, '')
    lines.push(marker, trailer)
    return lines.join('\n')
  }
  // Keep the complete file in the trailer; only its duplicate visible evidence is shortened.
  let low = 0,
    high = Math.max(0, ...file.blockers.map((b) => b.text.length))
  while (low < high) {
    const mid = Math.ceil((low + high) / 2)
    if (render(mid).length <= 65536) low = mid
    else high = mid - 1
  }
  return render(low)
}

export function defectDescription(count, parked = false) {
  return `defect: ${count} finding(s)${parked ? ', needs human' : ''}`
}

export function defectTrackerComment({ count, pr, url, commentUrl }) {
  return [
    `Verification found ${count} finding(s) on PR #${pr}${url ? ` (${url})` : ''}, and automatic repair is off for this repository.`,
    commentUrl ? `Findings: ${commentUrl}` : '',
  ]
    .filter(Boolean)
    .join('\n\n')
}

export function findingsComment(findings, headSha, mention = '') {
  const lines = [
    ...(mention ? [mention, ''] : []),
    `### Verification found ${findings.length} finding(s) on \`${headSha.slice(0, 12)}\``,
    '',
  ]
  for (const f of findings) {
    const where = text(f.file) ? ` (${f.file}${Number.isInteger(f.line) ? `:${f.line}` : ''})` : ''
    lines.push(`- **${String(f.title)}**${where}${text(f.detail) ? ` — ${f.detail}` : ''}`)
  }
  lines.push('', `boss-verify head ${headSha}`)
  return lines.join('\n')
}

export function parseVerifyFlags(verb, argv, FLAGS, UsageError) {
  const spec = FLAGS[verb]
  let flags
  try {
    flags = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: false,
      options: Object.fromEntries([
        ...spec.values.map((name) => [name, { type: 'string' }]),
        ...spec.bools.map((name) => [name, { type: 'boolean' }]),
      ]),
    }).values
  } catch (error) {
    throw new UsageError(`verify-gate: ${verb}: ${error.message}`)
  }
  const pr = Number(flags.pr)
  if (!Number.isInteger(pr) || pr < 1)
    throw new UsageError(`verify-gate: ${verb} needs --pr <number>`)
  return { ...flags, pr }
}

export function aggregateBlockerExtensions(
  results,
  installed,
  classifyVerifyOutcome,
  HUMAN_REASONS,
  { depsMissing = false } = {},
) {
  const declaredReasons = new Map()
  for (const ext of Array.isArray(installed) ? installed : [])
    if (text(ext?.name) && ext.optional !== true && HUMAN_REASONS.includes(ext.malfunctionReason))
      declaredReasons.set(ext.name, ext.malfunctionReason)
  if (!Array.isArray(results))
    return {
      verdict: 'human',
      reason: 'extension-failed',
      outcomes: [],
      failed: ['(results unreadable)'],
      findings: [],
      humanReasons: HUMAN_REASONS.filter((code) => [...declaredReasons.values()].includes(code)),
      waitReason: null,
    }
  const entries = results.filter((entry) => entry && typeof entry === 'object')
  const seen = new Set(entries.map((entry) => entry.extension).filter(text))
  for (const ext of installed)
    if (text(ext?.name) && !seen.has(ext.name))
      entries.push({ extension: ext.name, optional: ext.optional === true, crashed: true })
  const outcomes = entries.map((entry) => ({
    extension: text(entry.extension) ? entry.extension : '(unnamed)',
    ...classifyVerifyOutcome({
      depsMissing,
      result: entry.result,
      timedOut: entry.timedOut === true,
      crashed: entry.crashed === true,
      optional: entry.optional === true,
      dirtied: entry.dirtied === true,
      humanReasons: HUMAN_REASONS,
    }),
  }))
  const failed = outcomes.filter((o) => o.outcome === 'failed').map((o) => o.extension)
  const malfunctionReasons = outcomes
    .filter((o) => o.outcome === 'failed')
    .map((o) => declaredReasons.get(o.extension))
  const humanReasons = HUMAN_REASONS.filter(
    (code) =>
      malfunctionReasons.includes(code) ||
      outcomes.some((o) => o.outcome === 'ok' && o.verdict === 'human' && o.humanReason === code),
  )
  const findings = []
  const findingNotes = []
  entries.forEach((entry, index) => {
    if (outcomes[index].outcome === 'ok' && outcomes[index].verdict === 'fail')
      for (const finding of entry.result.findings)
        (isNoteFinding(finding) ? findingNotes : findings).push({
          extension: outcomes[index].extension,
          ...finding,
        })
  })
  // `waitReason` is returned whatever the verdict, like `humanReasons`, so a caller that ignores a
  // malfunction under a waiver still waits on the extensions that did run.
  const waitReason = outcomes.find((o) => o.outcome === 'ok' && o.verdict === 'wait')?.waitReason
  const out = {
    outcomes,
    failed,
    findings,
    findingNotes,
    humanReasons,
    waitReason: waitReason ?? null,
  }
  if (failed.length > 0) return { verdict: 'human', reason: 'extension-failed', ...out }
  if (findings.length > 0) return { verdict: 'defect', reason: 'findings', ...out }
  if (waitReason) return { verdict: 'wait', reason: waitReason, ...out }
  if (humanReasons.length > 0) return { verdict: 'human', reason: humanReasons.join(','), ...out }
  return { verdict: 'pass', reason: 'verified', ...out }
}

// The park code of a head verify judged that GitHub blocks until a person approves the PR.
export const APPROVAL_REQUIRED = 'approval-required'

// The review decisions under which GitHub blocks a merge until a person approves.
const PENDING_REVIEW_DECISIONS = ['REVIEW_REQUIRED', 'CHANGES_REQUESTED']

/**
 * Why a recorded approval cannot lift a park with these codes, or null. Only GitHub lifts
 * `approval-required`, once its review decision reads APPROVED; a boss approval never stands in.
 * @param {string[]} codes @param {string | undefined} reviewDecision
 */
export const githubApprovalRefusal = (codes, reviewDecision) =>
  codes.includes(APPROVAL_REQUIRED) && normalized(reviewDecision) !== 'APPROVED'
    ? APPROVAL_REQUIRED
    : null

/**
 * Whether a BLOCKED head may be judged and parked for an approval instead of waiting. GitHub's
 * BLOCKED names no cause, so it is read off the PR: the review decision says a required review is
 * missing, and green, settled CI on the head (the classifier over statusCheckRollup, `boss/*`
 * excluded) rules out a required check. A block reviews cannot explain (an approved or
 * review-free PR) stays a wait. A rule the PR does not show, such as conversation resolution, can
 * still hide behind a missing review; that only mislabels a park, because merge still waits for a
 * clean merge state.
 * @param {{reviewDecision?: string, ciGreen?: boolean}} facts
 */
export const approvalCanUnblock = ({ reviewDecision, ciGreen }) =>
  ciGreen === true && PENDING_REVIEW_DECISIONS.includes(normalized(reviewDecision))

// Merge-state reasons that only a merge needs cleared. Dirty is a conflict, judged as a blocker;
// unknown may hide a conflict and draft is a draft, so both still wait.
const MERGE_ONLY_REASONS = [
  'merge-state-clean',
  'merge-state-has_hooks',
  'merge-state-unstable',
  'merge-state-blocked',
  'merge-state-behind',
]

// Splits shared-eligibility reasons into `{wait, partial, approvalRequired}`: merge state, PR shape
// and head reasons resolve by waiting (the CI reasons are judged separately, with `boss/*`
// excluded); boss-build's PARTIAL marker is the PR itself saying it is not finished. A BLOCKED head
// an approval can unblock is judged and parked `approval-required` instead, and a run that never
// merges (`mergeless`) waits on no merge-only reason.
export function splitEligibility(reasons, { approvalOnly = false, mergeless = false } = {}) {
  const wait = []
  let partial = false
  let approvalRequired = false
  for (const reason of reasons ?? []) {
    if (reason === 'partial-marker') partial = true
    else if (['ci-not-settled', 'ci-not-green-on-head', 'merge-state-dirty'].includes(reason))
      continue
    else if (mergeless && MERGE_ONLY_REASONS.includes(reason)) continue
    else if (reason === 'merge-state-blocked' && approvalOnly) approvalRequired = true
    else wait.push(reason)
  }
  return { wait, partial, approvalRequired }
}

// The split for a judge's facts: `trial` sessions never merge.
export const judgeEligibility = (f) =>
  splitEligibility(f.eligibilityReasons, {
    approvalOnly: approvalCanUnblock({ reviewDecision: f.reviewDecision, ciGreen: f.ci?.green }),
    mergeless: f.trial === true,
  })
