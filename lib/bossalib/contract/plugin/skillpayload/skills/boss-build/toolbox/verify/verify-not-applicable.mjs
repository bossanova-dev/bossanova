// The head-bound PR comment that records why verify extensions did not apply: one bullet per
// not-applicable extension that left a note, keyed by a digest so a re-judge of the same head
// never posts twice. Cosmetic: it never changes a verdict, a status or a merge.
import { createHash } from 'node:crypto'

const exactKeys = (value, keys) =>
  value !== null &&
  typeof value === 'object' &&
  !Array.isArray(value) &&
  Object.keys(value).length === keys.length &&
  keys.every((key) => Object.hasOwn(value, key))
const singleLine = (value) =>
  typeof value === 'string' && value.trim() !== '' && !/[\r\n]/.test(value)
const compare = (a, b) => (a < b ? -1 : a > b ? 1 : 0)
// The verify-notes escape set; it includes `<` and `>`, so a note can never forge a marker line.
const escapeMarkdown = (text) => text.replace(/([\\`*_{}\[\]()<>])/g, '\\$1')

/** The hidden last line of a not-applicable comment: the head it names and its notes' digest. */
export const NOT_APPLICABLE_MARKER =
  /^<!-- boss-verify-not-applicable head=([0-9a-fA-F]{40}) digest=([0-9a-f]{16}) -->$/

/**
 * Whether `value` is a not-applicable notes list, the `post --not-applicable` file schema: an
 * array of exactly `{extension, note}`, both single-line non-empty strings, the extension name
 * free of backticks so it renders as one code span.
 *
 * @param {unknown} value
 * @returns {value is {extension: string, note: string}[]}
 */
export function validNotApplicable(value) {
  return (
    Array.isArray(value) &&
    value.every(
      (entry) =>
        exactKeys(entry, ['extension', 'note']) &&
        singleLine(entry.extension) &&
        !entry.extension.includes('`') &&
        singleLine(entry.note),
    )
  )
}

/**
 * Renders the not-applicable comment for `head`: the notes sorted and de-duplicated by
 * extension, each note markdown-escaped, ending in the NOT_APPLICABLE_MARKER trailer whose
 * digest is sha256 over `{version: 1, notes}`, truncated to 16 hex characters.
 *
 * @param {{head: string, notes: {extension: string, note: string}[]}} input
 * @returns {{body: string, digest: string}}
 */
export function notApplicableComment({ head, notes }) {
  if (typeof head !== 'string' || !/^[0-9a-f]{40}$/i.test(head) || !validNotApplicable(notes))
    throw new TypeError('invalid not-applicable comment input')
  const sorted = notes
    .map(({ extension, note }) => ({ extension, note }))
    .sort((a, b) => compare(a.extension, b.extension))
    .filter((entry, index, all) => index === 0 || all[index - 1].extension !== entry.extension)
  const digest = createHash('sha256')
    .update(JSON.stringify({ version: 1, notes: sorted }))
    .digest('hex')
    .slice(0, 16)
  const body = [
    `Verify extensions not applicable on \`${head.slice(0, 12)}\`:`,
    '',
    ...sorted.map(({ extension, note }) => `- \`${extension}\`: ${escapeMarkdown(note)}`),
    '',
    `<!-- boss-verify-not-applicable head=${head} digest=${digest} -->`,
  ].join('\n')
  return { body, digest }
}

/**
 * Records `notes` as the head's not-applicable comment through `recordHeadNote` (kind
 * `not-applicable`): posted once per head, `current` when verify's own newest one has the same
 * digest, rewritten in place when it differs. Cosmetic: it never throws, and an unreadable marker
 * read returns 'unknown'.
 *
 * @param {{read: Function, repo: string, pr: number, headSha: string, dryRun: boolean, writes: object[]}} ctx
 * @param {{extension: string, note: string}[]} notes
 * @param {{readOperatorMarkers: Function, recordHeadNote: Function}} deps
 * @returns {string} the recordHeadNote outcome, 'unknown' or 'failed'
 */
export function recordNotApplicable(ctx, notes, { readOperatorMarkers, recordHeadNote }) {
  const { read, repo, pr, headSha, dryRun, writes } = ctx
  try {
    const markers = readOperatorMarkers(read, repo, pr)
    if (!markers.readable) return 'unknown'
    const note = notApplicableComment({ head: headSha, notes })
    return recordHeadNote(read, {
      kind: 'not-applicable',
      repo,
      pr,
      headSha,
      note,
      markers,
      dryRun,
      writes,
    })
  } catch {
    return 'failed'
  }
}
