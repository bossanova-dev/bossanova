// verify-park-note.mjs — the operator-facing explanation a verify park leaves on its PR: each park
// code's issue in plain words and the evidence verify saw for it. Also the
// note a pass leaves listing the PR's `## After merge` items, which nothing gates on.
//
// Pure: no subprocess, no filesystem, no `process` reads. verify-gate.mjs `post` gathers the facts
// (the PR body, the derived park label, the extension results, whether the PR has a ticket) and
// upserts the rendered comment, one per head, by the digest its marker carries.

import { createHash } from 'node:crypto'
import { levelNoteLead } from './verify-level.mjs'

import { sectionToken, sections } from '../epic/merge-eligibility.mjs'
import { classifyVerifyOutcome } from '../skill-extensions.mjs'

// The issue each park code names in plain words, the category a person reads it as, and whether it
// only asks a person to look. They are the `plain` words, the category and the sign-off flag of the
// same code in lib/bossalib/attention/templates.go (knownParkCodes), so the PR
// comment and the TUI describe a park the same way. Both sides are tested against
// lib/bossalib/attention/testdata/park_code_words.json, which the Go table generates.
export const PARK_CODE_WORDS = Object.freeze({
  'no-receipt': {
    plain:
      'the latest commit has not had an automated build and review, or changed after its review',
    category: 'latest commit not reviewed',
    signOff: true,
    decidingPriority: true,
  },
  'repair-exhausted': {
    plain: 'automatic repair gave up on this PR',
    category: 'repair exhausted',
    signOff: false,
    decidingPriority: false,
  },
  'always-human-path': {
    plain: "touches a path that always needs a person's sign-off",
    category: 'protected path',
    signOff: true,
    decidingPriority: true,
  },
  'partial-build': {
    plain: 'only part of the ticket was finished, so the PR is held',
    category: 'partial build',
    signOff: false,
    decidingPriority: true,
  },
  // Retired with the PR checklist gate: verify no longer raises it, but parks recorded before then
  // still name it.
  'ledger-open': {
    plain:
      'the PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section',
    category: 'open checklist items',
    signOff: false,
    decidingPriority: true,
  },
  'extension-failed': {
    plain: 'a verify extension failed to run',
    category: 'verify extension failed',
    signOff: false,
    decidingPriority: false,
  },
  'cannot-assess': {
    plain: 'verify could not gather the evidence to judge the PR',
    category: 'cannot assess',
    signOff: false,
    decidingPriority: false,
  },
  'no-session': {
    plain: 'verify found no session behind this PR',
    category: 'no session',
    signOff: false,
    decidingPriority: false,
  },
  'worktree-diverged': {
    plain: 'the session worktree is off the PR head and holds work the PR lacks',
    category: 'worktree diverged',
    signOff: false,
    decidingPriority: false,
  },
  'worktree-unsynced': {
    plain: 'verify could not move the session worktree to the PR head',
    category: 'worktree unsynced',
    signOff: false,
    decidingPriority: false,
  },
  'human-merge': {
    plain: "verify passed; this PR's policy leaves the merge to a person",
    category: 'merge by a person',
    signOff: true,
    decidingPriority: true,
  },
  'verify-post-failed': {
    plain: 'verify could not record its verdict on this commit',
    category: 'verify could not post',
    signOff: false,
    decidingPriority: false,
  },
  'human-review:database': {
    plain: 'a database migration for a person to review',
    category: 'database migration',
    signOff: true,
    decidingPriority: true,
  },
  'human-review:security': {
    plain: 'a security-sensitive change for a person to review',
    category: 'security',
    signOff: true,
    decidingPriority: true,
  },
  'human-review:ui': {
    plain: 'a UI change',
    category: 'UI change',
    signOff: true,
    decidingPriority: true,
  },
  'human-review:unproven': {
    plain: 'some checks have no evidence that they pass',
    category: 'unproven checks',
    signOff: true,
    decidingPriority: true,
  },

  'ticket-needs-person': {
    plain:
      "the ticket is labelled for a person's answer, so verify will not take this PR until it is answered or labelled for build",
    category: 'ticket waits on a person',
    signOff: false,
    decidingPriority: false,
  },
  'ticket-not-selected': {
    plain:
      "the ticket's labels keep it out of verify's selection, so verify will not take this PR until it is relabelled or merged by hand",
    category: 'ticket not selected',
    signOff: false,
    decidingPriority: false,
  },
})

export const UNKNOWN_CODE_WORDS = Object.freeze({
  plain: 'an issue this version of boss does not recognise',
  category: 'an issue this version of boss does not recognise',
  signOff: false,
  decidingPriority: false,
})

// Admission-only legacy codes are known words, but follow reported codes in first-seen order.
const reportOrder = Object.keys(PARK_CODE_WORDS).filter((code) => !code.startsWith('ticket-'))
const wordsOf = (code) =>
  Object.hasOwn(PARK_CODE_WORDS, code) ? PARK_CODE_WORDS[code] : UNKNOWN_CODE_WORDS
function normalizeCodes(raw) {
  const seen = [
    ...new Set(
      raw
        .flatMap((entry) => entry.split(','))
        .map((code) => code.trim())
        .filter(Boolean),
    ),
  ]
  return [
    ...reportOrder.filter((code) => seen.includes(code)),
    ...seen.filter((code) => !reportOrder.includes(code)),
  ]
}
function decidingCode(codes) {
  const failures = codes.filter((code) => !wordsOf(code).signOff)
  const candidates = failures.length ? failures : codes
  return (
    candidates.find((code) => wordsOf(code).decidingPriority && code.startsWith('human-review:')) ??
    candidates.find((code) => wordsOf(code).decidingPriority) ??
    candidates[0]
  )
}

/**
 * The verdict attention.ParkVerdict derives, or null for no code. Inputs are operator park codes:
 * parseHumanReasonList already excludes waiting codes; this port does not model waiting labels.
 * @param {string[]} codes
 * @param {{label?: string, evidence?: {url?: string, videoSeconds?: number}|null, openItems?: number}} [options]
 */
export function parkVerdict(codes, { label = '', evidence = null, openItems = 0 } = {}) {
  const code = decidingCode(normalizeCodes(codes))
  if (!code) return null
  const words = wordsOf(code)
  const state = code === 'human-merge' ? 'Verified' : 'Not verified'
  const wait = words.signOff ? 'waiting for you' : 'blocked'
  const reason =
    label && labelled(code)
      ? `this PR changes ${label}`
      : code === 'ledger-open'
        ? `the PR checklist has ${openItems === 0 ? 'open items' : `${openItems} open item${openItems === 1 ? '' : 's'}`}`
        : words.plain
  const seconds = Math.round(evidence?.videoSeconds ?? 0)
  const shownEvidence = evidence?.url
    ? `evidence video${(evidence.videoSeconds ?? 0) > 0 ? ` (${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')})` : ''}`
    : ''
  return {
    state,
    wait,
    reason,
    evidence: shownEvidence,
    sentence: [state, wait, reason, shownEvidence].filter(Boolean).join(' · '),
  }
}

/** The categories `codes` fall in, deduped in code order, as a person reads them. */
export function parkCategories(codes) {
  return [...new Set(codes.map((code) => (PARK_CODE_WORDS[code] ?? UNKNOWN_CODE_WORDS).category))]
}

// The words of a phrase that carry meaning: lower-cased, articles dropped.
const meaningOf = (phrase) =>
  phrase
    .toLowerCase()
    .split(/\s+/)
    .filter((word) => word && !['a', 'an', 'the'].includes(word))
    .join(' ')

/**
 * Whether a code's plain words only repeat its category, ignoring case and articles, so a line
 * naming the category already says them. lib/bossalib/attention ParkCodeRestatesCategory is the same
 * rule; both are tested against park_code_words.json.
 */
export const restatesCategory = ({ plain, category }) => meaningOf(plain) === meaningOf(category)

const text = (value) => typeof value === 'string' && value.trim() !== ''
const capitalize = (value) => value.charAt(0).toUpperCase() + value.slice(1)
const LINE_CAP = 300

// One line of quoted PR or extension text: whitespace collapsed, capped, and unable to end the
// comment with a forged marker or to mention anyone.
function quote(value) {
  const flat = String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim()
    .replace(/<!--/g, '&lt;!--')
    .replace(/@/g, '@​')
  return flat.length > LINE_CAP ? `${flat.slice(0, LINE_CAP - 1).trimEnd()}…` : flat
}

const AFTER_MERGE_SHOWN = 20

/**
 * The unchecked `## After merge` items, quoted. Verify never gates on them; it lists them in its
 * notes so a person sees them, and so a pre-merge task filed there by mistake shows before merge.
 */
export function afterMergeItems(body = '') {
  const lines = sections(body).get('after merge')
  if (!Array.isArray(lines)) return []
  return lines.flatMap((line) => {
    const item = line.trim().match(/^- \[ \]\s+(\S.*)$/)
    return item ? [quote(item[1])] : []
  })
}

function afterMergeLines(body) {
  const items = afterMergeItems(body)
  if (!items.length) return []
  const more = items.length - AFTER_MERGE_SHOWN
  return [
    '**After merge, check:**',
    ...items.slice(0, AFTER_MERGE_SHOWN).map((item) => `- ${item}`),
    ...(more > 0 ? [`- …and ${more} more in the PR description.`] : []),
    '',
  ]
}

const FAILURE_WORDS = Object.freeze({
  'timed-out': 'timed out',
  crashed: 'crashed or returned nothing',
  'reported-failure': 'reported a failure',
  'pass-without-evidence': 'passed without citing any evidence',
  'malformed-result': 'returned a result verify cannot read',
})

/**
 * The malfunctioning extensions in an `--extension-results` array, one line each: its name, how it
 * failed, and the error text it reported, if any. Null when the results are not an array.
 */
export function extensionFailures(results) {
  if (!Array.isArray(results)) return null
  const out = []
  for (const entry of results) {
    if (!entry || typeof entry !== 'object') continue
    const outcome = classifyVerifyOutcome({
      result: entry.result,
      timedOut: entry.timedOut === true,
      crashed: entry.crashed === true,
      optional: entry.optional === true,
    })
    if (outcome.outcome !== 'failed') continue
    const name = text(entry.extension) ? entry.extension : '(unnamed)'
    const error = text(entry.result?.error) ? `: ${quote(entry.result.error)}` : ''
    out.push(`\`${quote(name)}\` ${FAILURE_WORDS[outcome.reason] ?? quote(outcome.reason)}${error}`)
  }
  return out
}

/** The evidence lines for one park code. */
function evidenceFor(code, facts) {
  if (code === 'verify-post-failed')
    return (facts.postFailureNotes ?? []).map((note) =>
      quote(note.detail ?? note.evidence ?? note.title),
    )
  if (code === 'repair-exhausted' && facts.repairRounds) {
    const describe = (blocker) => {
      const severity = text(blocker.severity) ? `${quote(blocker.severity)} ` : ''
      const location = text(blocker.file)
        ? `${quote(blocker.file)}${blocker.line == null ? '' : `:${quote(blocker.line)}`} `
        : ''
      return `${severity}${location}${quote(blocker.summary ?? blocker.id)}`
    }
    return [
      ...(facts.repairRounds.repeatNotFixable ?? []).map(
        (blocker) => `${describe(blocker)} — not fixable twice: ${quote(blocker.reason)}.`,
      ),
      ...(facts.repairRounds.notFixable ?? []).map(
        (blocker) =>
          `${describe(blocker)} — not fixable: ${quote(blocker.reason)} (round ${quote(blocker.round)}).`,
      ),
      ...(facts.blockers?.blockers ?? []).map((blocker) => `Still blocking: ${describe(blocker)}.`),
    ]
  }
  if (code === 'partial-build') {
    const line = sectionToken(facts.body, 'partial')
    return [
      line ? `\`## Partial\` reads: ${quote(line)}` : `The title reads: ${quote(facts.title)}`,
    ]
  }
  if (code === 'no-receipt') return ['No automated build and review has passed on this commit.']
  if (code === 'extension-failed') {
    const failures = extensionFailures(facts.extensionResults)
    if (failures === null)
      return ["The extensions' results were not available when this comment was written."]
    return failures.length > 0
      ? failures
      : ['No extension in the results failed; one that ran but left no result counts as failed.']
  }
  return []
}

// Whether a code's evidence is the park label: the operator's words for the paths it matched.
const labelled = (code) => code === 'always-human-path' || code.startsWith('human-review:')

/**
 * The park label as one top-level line, or none. Every labelled code shares the one label, so it
 * is said once, before the codes, and never under a code whose own line was dropped.
 */
function labelBlocks(codes, facts) {
  if (!text(facts.label) || !codes.some(labelled)) return []
  return [{ line: null, evidence: [`The change touches ${quote(facts.label)}.`] }]
}

// GitHub refuses a comment over 65,536 characters; the note stays under NOTE_CHAR_LIMIT, with room
// left for the marker line.
export const NOTE_CHAR_LIMIT = 60_000
const MARKER_RESERVE = 160
const MORE_RESERVE = 32
const blockReserve = (blocks) =>
  blocks.reduce((sum, block) => sum + (block.line?.length ?? 0) + 1 + MORE_RESERVE, 0)

// Keep the level and reasons first, then as many Low asks as the rest of the note leaves room for.
function fitConfidenceLead(confidence, room) {
  const lines = levelNoteLead(confidence).map(quote)
  const askCount = confidence.level === 'low' ? confidence.asks.length : 0
  const out = lines.slice(0, lines.length - askCount)
  room -= out.reduce((sum, line) => sum + line.length + 1, 0)
  const asks = lines.slice(lines.length - askCount)
  for (const [index, line] of asks.entries()) {
    const remaining = asks.length - index - 1
    const more = remaining ? `- …and ${remaining} more review asks.` : ''
    if (line.length + 1 + (more ? more.length + 1 : 0) > room) {
      out.push(`- …and ${asks.length - index} more review asks.`)
      break
    }
    out.push(line)
    room -= line.length + 1
  }
  return out
}

// The code lines and as much evidence as fits in what `used` leaves of NOTE_CHAR_LIMIT, in order;
// each code whose evidence was cut ends with how many lines it lost. A code with no line of its own
// lists its evidence at the top level.
function fitEvidence(blocks, used) {
  let room = NOTE_CHAR_LIMIT - used - blockReserve(blocks)
  const out = []
  for (const { line, evidence } of blocks) {
    const indent = line ? '  - ' : '- '
    if (line) out.push(line)
    let kept = 0
    for (const entry of evidence) {
      if (indent.length + entry.length + 1 > room) break
      room -= indent.length + entry.length + 1
      out.push(`${indent}${entry}`)
      kept++
    }
    if (kept < evidence.length) {
      room = 0
      out.push(`${indent}…and ${evidence.length - kept} more.`)
    }
  }
  return out
}

/**
 * The machine-readable line naming the park's codes exactly, for readers of the comment that need
 * them; the note itself names each code by its category. `<` and `>` are escaped so no code can
 * close the comment early.
 */
export function parkCodesLine(codes) {
  const json = JSON.stringify(codes).replace(/</g, '\\u003c').replace(/>/g, '\\u003e')
  return `<!-- boss-park-codes: ${json} -->`
}

export const PARK_CODES_LINE = /^<!-- boss-park-codes: (\[.*\]) -->$/

export function parkNoteMarker(headSha, digest) {
  return `<!-- boss-park-note head=${headSha} digest=${digest} -->`
}

// A note written while the PR checklist gate existed may carry a `ledger=` fingerprint; it still
// reads as that head's note.
export const PARK_NOTE_MARKER =
  /^<!-- boss-park-note head=([0-9a-fA-F]{40}) digest=([0-9a-f]{16})(?: ledger=[0-9a-f]{16})? -->$/

/**
 * The park note for `codes` on `headSha`: `{body, digest}`. `facts` carries `title`, `body` (the
 * PR's), `label` (the derived park label), `extensionResults` (the parsed results array, or
 * undefined when the post was not given one), `ticketless` (no tracker ticket behind the PR),
 * `repairRounds` (counted rounds and not-fixable outcomes) and `blockers` (the validated blocker
 * file for the parked head).
 * The digest covers the note, so an unchanged note is never rewritten.
 */
export function parkNote({ codes, headSha, facts = {} }) {
  codes = normalizeCodes(codes)
  const verdict = parkVerdict(codes, { label: facts.label })
  const deciding = decidingCode(codes)
  const head = [`**${quote(verdict?.sentence)}** on \`${headSha.slice(0, 12)}\`.`, '']
  const blocks = (
    verdict?.reason === `this PR changes ${facts.label}` ? [] : labelBlocks(codes, facts)
  ).concat(
    codes.map((code) => {
      const words = wordsOf(code)
      return {
        line:
          code === deciding && verdict?.reason === words.plain
            ? null
            : `- ${capitalize(words.plain)}.`,
        evidence: evidenceFor(code, facts),
      }
    }),
  )
  const tail = ['', ...afterMergeLines(facts.body)]
  if (facts.ticketless === true)
    tail.push('This PR has no tracker ticket, so no ticket was updated.', '')
  tail.push(
    'An approval waives exactly what is listed above, on this commit only. A new push is judged afresh.',
  )
  tail.push('', parkCodesLine(codes))
  if (facts.confidence) {
    const room =
      NOTE_CHAR_LIMIT -
      [...head, ...tail].join('\n').length -
      MARKER_RESERVE -
      blockReserve(blocks) -
      1
    head.unshift(...fitConfidenceLead(facts.confidence, room), '')
  }
  const content = [
    ...head,
    ...fitEvidence(blocks, [...head, ...tail].join('\n').length + MARKER_RESERVE),
    ...tail,
  ].join('\n')
  const digest = createHash('sha256').update(content).digest('hex').slice(0, 16)
  return { body: `${content}\n\n${parkNoteMarker(headSha, digest)}`, digest }
}

function afterMergeMarker(headSha, digest) {
  return `<!-- boss-after-merge head=${headSha} digest=${digest} -->`
}

export const AFTER_MERGE_MARKER =
  /^<!-- boss-after-merge head=([0-9a-fA-F]{40}) digest=([0-9a-f]{16}) -->$/

/**
 * The note a pass on `headSha` leaves when the PR body lists `## After merge` items: `{body,
 * digest}`, or null when it lists none. The digest covers the note, so an unchanged one is never
 * rewritten.
 */
export function afterMergeNote({ headSha, body = '' }) {
  const lines = afterMergeLines(body)
  if (!lines.length) return null
  const content = [
    `**This PR passed verification** on \`${headSha.slice(0, 12)}\`. Nothing below holds the merge.`,
    '',
    ...lines,
  ]
    .join('\n')
    .trimEnd()
  const digest = createHash('sha256').update(content).digest('hex').slice(0, 16)
  return { body: `${content}\n\n${afterMergeMarker(headSha, digest)}`, digest }
}
