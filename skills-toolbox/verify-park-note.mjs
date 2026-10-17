// verify-park-note.mjs — the operator-facing explanation a verify park leaves on its PR: each park
// code's issue in plain words and the evidence verify saw for it. Also the
// note a pass leaves listing the PR's `## After merge` items, which nothing gates on.
//
// Pure: no subprocess, no filesystem, no `process` reads. verify-gate.mjs `post` gathers the facts
// (the PR body, the derived park label, the extension results, whether the PR has a ticket) and
// upserts the rendered comment, one per head, by the digest its marker carries.

import { createHash } from 'node:crypto'

import { checklist, isPostMergeItem, sections } from './merge-eligibility.mjs'
import { classifyVerifyOutcome } from './skill-extensions.mjs'

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
  },
  'repair-exhausted': {
    plain: 'automatic repair gave up on this PR',
    category: 'repair exhausted',
    signOff: false,
  },
  'always-human-path': {
    plain: "touches a path that always needs a person's sign-off",
    category: 'security',
    signOff: true,
  },
  'ledger-open': {
    plain:
      'the PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section',
    category: 'open checklist items',
    signOff: false,
  },
  'extension-failed': {
    plain: 'a verify extension failed to run',
    category: 'verify extension failed',
    signOff: false,
  },
  'no-session': {
    plain: 'verify found no session behind this PR',
    category: 'no session',
    signOff: false,
  },
  'worktree-diverged': {
    plain: 'the session worktree is off the PR head and holds work the PR lacks',
    category: 'worktree diverged',
    signOff: false,
  },
  'worktree-unsynced': {
    plain: 'verify could not move the session worktree to the PR head',
    category: 'worktree unsynced',
    signOff: false,
  },
  'human-review:database': {
    plain: 'a database migration for a person to review',
    category: 'database migration',
    signOff: true,
  },
  'human-review:security': {
    plain: 'a security-sensitive change for a person to review',
    category: 'security',
    signOff: true,
  },
  'human-review:ui': {
    plain: 'a UI change',
    category: 'UI change',
    signOff: true,
  },
  'human-review:unproven': {
    plain: 'some checks have no evidence that they pass',
    category: 'unproven checks',
    signOff: true,
  },
})

export const UNKNOWN_CODE_WORDS = Object.freeze({
  plain: 'an issue this version of boss does not recognise',
  category: 'an issue this version of boss does not recognise',
  signOff: false,
})

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

// The ledger sections `splitEligibility` reads, with the reason each raises when open or unreadable.
const LEDGER_SECTIONS = Object.freeze([
  { heading: 'acceptance criteria', allowNone: false, open: 'Unmet acceptance criterion' },
  {
    heading: 'human follow-up',
    allowNone: true,
    exempt: isPostMergeItem,
    open: 'Open human follow-up',
  },
  { heading: 'open questions', allowNone: true, open: 'Open question' },
])

/**
 * What makes `ledger-open` fire on this PR, one line each: every ledger section that is missing,
 * duplicated or not a checklist, every unchecked item in one, and a do-not-merge marker. These are
 * exactly the ledger reasons verify counts (merge-eligibility.mjs criteria and follow-up checks plus
 * the do-not-merge marker); no other section, `## After merge` and an unapplied review finding
 * included, is counted, nor is a follow-up item that marks itself post-merge (isPostMergeItem).
 */
export function ledgerEvidence({ title = '', body = '' } = {}) {
  const parsed = sections(body)
  const out = []
  for (const { heading, allowNone, exempt, open } of LEDGER_SECTIONS) {
    const name = `\`## ${heading.replace(/^./, (c) => c.toUpperCase())}\``
    if (!parsed.has(heading)) {
      out.push(`${name} is missing.`)
      continue
    }
    const lines = parsed.get(heading)
    if (lines === null) {
      out.push(`${name} appears more than once, so verify cannot read it.`)
      continue
    }
    const list = checklist(lines, allowNone, exempt)
    if (list.status !== 'ok') {
      out.push(
        `${name} is not a checklist of \`- [ ]\` / \`- [x]\` items${allowNone ? ' (or `- none`)' : ''}.`,
      )
      continue
    }
    for (const line of lines) {
      const item = line.trim().match(/^- \[ \]\s+(\S.*)$/)
      if (item && !exempt?.(item[1])) out.push(`${open}: ${quote(item[1])}`)
    }
  }
  if (/do not merge/i.test(`${title}\n${body}`) || /\(partial\s/i.test(title))
    out.push('The title or body carries a do-not-merge marker.')
  return out
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
  if (code === 'ledger-open') return ledgerEvidence(facts)
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

/**
 * The fingerprint of a PR's ledger as verify counts it (ledgerEvidence): equal for two bodies that
 * differ only outside the ledger, different once an item is ticked or a section repaired. A
 * `ledger-open` park records the one it was judged on, so a ledger fixed after the park re-judges it.
 */
export function ledgerFingerprint({ title = '', body = '' } = {}) {
  return createHash('sha256')
    .update(JSON.stringify(ledgerEvidence({ title, body })))
    .digest('hex')
    .slice(0, 16)
}

// GitHub refuses a comment over 65,536 characters; the note stays under NOTE_CHAR_LIMIT, with room
// left for the marker line.
export const NOTE_CHAR_LIMIT = 60_000
const MARKER_RESERVE = 160
const MORE_RESERVE = 32

// The code lines and as much evidence as fits in what `used` leaves of NOTE_CHAR_LIMIT, in order;
// each code whose evidence was cut ends with how many lines it lost. A code with no line of its own
// lists its evidence at the top level.
function fitEvidence(blocks, used) {
  let room =
    NOTE_CHAR_LIMIT -
    used -
    blocks.reduce((sum, block) => sum + (block.line?.length ?? 0) + 1 + MORE_RESERVE, 0)
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

// The most open items the machine-readable line carries; the note itself lists them all.
export const OPEN_ITEMS_MAX = 20

/**
 * The machine-readable line naming what keeps the PR checklist open on `headSha`, for clients that
 * show it without reading the PR (lib/bossalib/evidence ParseOpenItems): one plain line per item,
 * backticks dropped, at most OPEN_ITEMS_MAX. `<` and `>` are escaped so no item can close the
 * comment early. '' when there is nothing open.
 */
export function openItemsLine(headSha, items) {
  const plain = items.slice(0, OPEN_ITEMS_MAX).map((item) => item.replace(/`/g, ''))
  if (plain.length === 0) return ''
  const json = JSON.stringify({ head: headSha, items: plain })
    .replace(/</g, '\\u003c')
    .replace(/>/g, '\\u003e')
  return `<!-- boss-open-items: ${json} -->`
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

export function parkNoteMarker(headSha, digest, ledger = null) {
  return `<!-- boss-park-note head=${headSha} digest=${digest}${ledger ? ` ledger=${ledger}` : ''} -->`
}

export const PARK_NOTE_MARKER =
  /^<!-- boss-park-note head=([0-9a-fA-F]{40}) digest=([0-9a-f]{16})(?: ledger=([0-9a-f]{16}))? -->$/

/**
 * The park note for `codes` on `headSha`: `{body, digest}`. `facts` carries `title`, `body` (the
 * PR's), `label` (the derived park label), `extensionResults` (the parsed results array, or
 * undefined when the post was not given one) and `ticketless` (no tracker ticket behind the PR).
 * `ledger` is the ledgerFingerprint the park was judged on, carried in the marker when known. The
 * digest covers the note and that fingerprint, so an unchanged note is never rewritten.
 */
export function parkNote({ codes, headSha, facts = {}, ledger = null }) {
  const wordsOf = (code) => PARK_CODE_WORDS[code] ?? UNKNOWN_CODE_WORDS
  const categories = parkCategories(codes)
  const head = [
    `**This PR is waiting for a person** on \`${headSha.slice(0, 12)}\`: ${categories.join(', ')}.`,
    '',
  ]
  const blocks = labelBlocks(codes, facts).concat(
    codes.map((code) => {
      const words = wordsOf(code)
      return {
        line: restatesCategory(words) ? null : `- ${capitalize(words.plain)}.`,
        evidence: evidenceFor(code, facts),
      }
    }),
  )
  const tail = ['', ...afterMergeLines(facts.body)]
  if (facts.ticketless === true)
    tail.push('This PR has no tracker ticket, so no ticket was updated.', '')
  tail.push(
    `An approval waives exactly what is listed above, on this commit only. A new push is judged afresh${codes.includes('ledger-open') ? ', and a fixed checklist is judged again at the next check' : ''}.`,
  )
  const items = codes.includes('ledger-open') ? openItemsLine(headSha, ledgerEvidence(facts)) : ''
  tail.push('', parkCodesLine(codes))
  if (items) tail.push(items)
  const content = [
    ...head,
    ...fitEvidence(blocks, [...head, ...tail].join('\n').length + MARKER_RESERVE),
    ...tail,
  ].join('\n')
  const digest = createHash('sha256')
    .update(`${content}\n${ledger ?? ''}`)
    .digest('hex')
    .slice(0, 16)
  return { body: `${content}\n\n${parkNoteMarker(headSha, digest, ledger)}`, digest }
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
