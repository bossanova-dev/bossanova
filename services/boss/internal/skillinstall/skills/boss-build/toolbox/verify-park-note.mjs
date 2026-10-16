// verify-park-note.mjs — the operator-facing explanation a verify park leaves on its PR: each park
// code, what it means in plain words, the evidence verify saw for it, and what to do next. Also the
// note a pass leaves listing the PR's `## After merge` items, which nothing gates on.
//
// Pure: no subprocess, no filesystem, no `process` reads. verify-gate.mjs `post` gathers the facts
// (the PR body, the derived park label, the extension results, whether the PR has a ticket) and
// upserts the rendered comment, one per head, by the digest its marker carries.

import { createHash } from 'node:crypto'

import { checklist, isPostMergeItem, sections } from './merge-eligibility.mjs'
import { classifyVerifyOutcome } from './skill-extensions.mjs'

// What each park code means and the issue it names. `plain` and `next` mirror the
// `plain` words and the park sentence of the same code in lib/bossalib/attention/templates.go
// (knownParkCodes), so the PR comment and the TUI describe a park the same way; keep them in step.
export const PARK_CODE_WORDS = Object.freeze({
  'no-receipt': {
    plain: "no build receipt: built by hand, without the factory's build and review",
    next: 'Built by hand, no factory review',
  },
  'repair-exhausted': {
    plain: 'automatic repair gave up on this PR',
    next: 'Verify is blocked',
  },
  'always-human-path': {
    plain: "touches a path that always needs a person's sign-off",
    next: 'Security change',
  },
  'ledger-open': {
    plain:
      'the PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section',
    next: 'The PR checklist has open items',
  },
  'extension-failed': {
    plain: 'a verify extension failed to run',
    next: 'Verify is blocked',
  },
  'no-session': {
    plain: 'verify found no session behind this PR',
    next: 'Verify is blocked',
  },
  'worktree-diverged': {
    plain: 'the session worktree is off the PR head and holds work the PR lacks',
    next: 'Verify is blocked',
  },
  'worktree-unsynced': {
    plain: 'verify could not move the session worktree to the PR head',
    next: 'Verify is blocked',
  },
  'human-review:database': {
    plain: 'a database migration for a person to review',
    next: 'Database migration',
  },
  'human-review:security': {
    plain: 'a security-sensitive change for a person to review',
    next: 'Security change',
  },
  'human-review:ui': {
    plain: 'a UI change; watch the evidence before it merges',
    next: 'UI change · evidence video',
  },
  'human-review:unproven': {
    plain: 'some checks have no evidence that they pass',
    next: 'Some checks have no evidence',
  },
})

const UNKNOWN_CODE_WORDS = Object.freeze({
  plain: 'a park code this version of boss does not know',
  next: 'Verify is blocked',
})

const text = (value) => typeof value === 'string' && value.trim() !== ''
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
  if (code === 'no-receipt') return ['This head has no successful `boss/build` status.']
  if (code === 'extension-failed') {
    const failures = extensionFailures(facts.extensionResults)
    if (failures === null)
      return ["This post was not given the extensions' results; the verify chat has them."]
    return failures.length > 0
      ? failures
      : ['No extension in the results failed; one that ran but left no result counts as failed.']
  }
  if (code === 'always-human-path' || code.startsWith('human-review:'))
    return text(facts.label) ? [`Matched: ${quote(facts.label)}`] : []
  return []
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
// each code whose evidence was cut ends with how many lines it lost.
function fitEvidence(blocks, used) {
  let room =
    NOTE_CHAR_LIMIT -
    used -
    blocks.reduce((sum, block) => sum + block.line.length + 1 + MORE_RESERVE, 0)
  const out = []
  for (const { line, evidence } of blocks) {
    out.push(line)
    let kept = 0
    for (const entry of evidence) {
      if (entry.length + 1 > room) break
      room -= entry.length + 1
      out.push(entry)
      kept++
    }
    if (kept < evidence.length) {
      room = 0
      out.push(`  - …and ${evidence.length - kept} more.`)
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
export function parkNote({ codes, headSha, pr, facts = {}, ledger = null }) {
  const head = [
    `**boss-verify parked this PR for a person** on \`${headSha.slice(0, 12)}\`: ${codes.map((c) => `\`${c}\``).join(', ')}.`,
    '',
  ]
  const blocks = codes.map((code) => {
    const words = PARK_CODE_WORDS[code] ?? UNKNOWN_CODE_WORDS
    return {
      line: `- **\`${code}\`**: ${words.plain}. ${words.next}.`,
      evidence: evidenceFor(code, facts).map((evidence) => `  - ${evidence}`),
    }
  })
  const tail = ['', ...afterMergeLines(facts.body)]
  if (facts.ticketless === true)
    tail.push(
      'This PR has no tracker ticket, so verify made no tracker writes: this comment and the `boss/verify` status are the whole record.',
      '',
    )
  tail.push(
    `**Next:** \`boss pr approve ${pr}\` (or approve the PR on GitHub) waives exactly these codes on this head; to change the PR instead, open a chat in its session and push a fix. A new push is judged afresh${codes.includes('ledger-open') ? ', and a fixed ledger is re-judged at the next verify sweep' : ''}.`,
  )
  const items = codes.includes('ledger-open') ? openItemsLine(headSha, ledgerEvidence(facts)) : ''
  if (items) tail.push('', items)
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
    `**boss-verify passed this PR** on \`${headSha.slice(0, 12)}\`. Nothing below holds the merge; it is listed so a person sees it.`,
    '',
    ...lines,
  ]
    .join('\n')
    .trimEnd()
  const digest = createHash('sha256').update(content).digest('hex').slice(0, 16)
  return { body: `${content}\n\n${afterMergeMarker(headSha, digest)}`, digest }
}
