import { createHash } from 'node:crypto'
import { findingHash, isNoteFinding } from './verify-blockers.mjs'

const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
const exactKeys = (value, keys) =>
  object(value) &&
  Object.keys(value).length === keys.length &&
  keys.every((key) => Object.hasOwn(value, key))
const oneLine = (value) =>
  String(value ?? '')
    .replace(/[\r\n]+/g, ' ')
    .trim()
const compare = (a, b) => (a < b ? -1 : a > b ? 1 : 0)
const noteOrder = (a, b) =>
  compare(a.source, b.source) ||
  compare(a.file ?? '', b.file ?? '') ||
  (a.line ?? 0) - (b.line ?? 0) ||
  compare(a.summary, b.summary) ||
  compare(a.id, b.id)
const digestOf = (notes, tickets) =>
  createHash('sha256')
    .update(JSON.stringify({ version: 1, notes, tickets }))
    .digest('hex')
    .slice(0, 16)
const singleLine = (value) =>
  typeof value === 'string' && value.trim() !== '' && !/[\r\n]/.test(value)
const noteId = (value) => typeof value === 'string' && /^note:[a-f0-9]{12}$/.test(value)

/** Normalize the two finding producers into stable, order-independent notes. */
export function buildNotes(findingNotes = []) {
  const notes = findingNotes.filter(isNoteFinding).map((finding) => ({
    id: `note:${findingHash(finding)}`,
    source: oneLine(finding.extension) ? `extension:${oneLine(finding.extension)}` : 'review',
    severity:
      String(finding.severity ?? '')
        .trim()
        .toUpperCase() === 'P3'
        ? 'P3'
        : 'style',
    file: singleLine(finding.file) ? finding.file : null,
    line:
      singleLine(finding.file) && Number.isSafeInteger(finding.line) && finding.line > 0
        ? finding.line
        : null,
    summary: oneLine(finding.title),
    text: oneLine(finding.evidence ?? finding.detail),
  }))
  notes.sort(
    (a, b) => noteOrder(a, b) || compare(a.text, b.text) || compare(a.severity, b.severity),
  )
  return notes.filter((note, index) => notes.findIndex((other) => other.id === note.id) === index)
}

function validNotes(notes) {
  if (!Array.isArray(notes)) return false
  const ids = new Set()
  return notes.every((note, index) => {
    if (
      !exactKeys(note, ['id', 'source', 'severity', 'file', 'line', 'summary', 'text']) ||
      !noteId(note.id) ||
      ids.has(note.id) ||
      !(note.source === 'review' || /^extension:[^\r\n]+$/.test(note.source)) ||
      !['P3', 'style'].includes(note.severity) ||
      !(note.file === null || singleLine(note.file)) ||
      !(
        note.line === null ||
        (note.file !== null && Number.isSafeInteger(note.line) && note.line > 0)
      ) ||
      !singleLine(note.summary) ||
      typeof note.text !== 'string' ||
      /[\r\n]/.test(note.text) ||
      (index > 0 && noteOrder(notes[index - 1], note) > 0)
    )
      return false
    ids.add(note.id)
    return true
  })
}

function validTickets(tickets, notes) {
  return (
    object(tickets) &&
    Object.entries(tickets).every(
      ([id, ticket]) =>
        notes.some((note) => note.id === id) &&
        exactKeys(ticket, ['identifier', 'url']) &&
        singleLine(ticket.identifier) &&
        singleLine(ticket.url) &&
        /^https?:\/\/[^\s]+$/.test(ticket.url),
    )
  )
}

/** Keep previously recorded links only while their note survives. Author trust is the caller's job. */
export function carryTickets(previous, notes) {
  return Object.fromEntries(
    Object.entries(previous?.tickets ?? {})
      .filter(([id]) => notes.some((note) => note.id === id))
      .sort(([a], [b]) => compare(a, b)),
  )
}

const shorten = (text, limit) => (text.length > limit ? text.slice(0, limit) + '…' : text)
const escapeMarkdown = (text) => text.replace(/([\\`*_{}\[\]()<>])/g, '\\$1')

/** Render a digest-keyed folded comment, preserving every identity and recorded ticket. */
export function notesComment({ head, notes, tickets = {} }) {
  if (
    typeof head !== 'string' ||
    !/^[a-f0-9]{40}$/i.test(head) ||
    !validNotes(notes) ||
    !validTickets(tickets, notes)
  )
    throw new TypeError('invalid notes comment input')
  tickets = Object.fromEntries(Object.entries(tickets).sort(([a], [b]) => compare(a, b)))
  const render = (limit) => {
    const bounded = notes.map((note) => ({ ...note, text: shorten(note.text, limit) }))
    const digest = digestOf(bounded, tickets)
    const payload = Buffer.from(JSON.stringify({ version: 1, notes: bounded, tickets })).toString(
      'base64url',
    )
    const bullets = bounded.map((note) => {
      const location = note.file
        ? ` ${escapeMarkdown(note.file)}${note.line === null ? '' : `:${note.line}`}`
        : ''
      const ticket = tickets[note.id]
      const link = ticket
        ? ` → [${escapeMarkdown(ticket.identifier)}](${ticket.url.replaceAll('(', '%28').replaceAll(')', '%29')})`
        : ''
      return `- ${note.id} **${note.severity}**${location} — ${escapeMarkdown(note.summary)}${note.text ? ` — ${escapeMarkdown(note.text)}` : ''}${link}`
    })
    const body = [
      `**${notes.length} note(s)** on \`${head.slice(0, 12)}\` — notes never block this PR.`,
      '',
      '<details><summary>Notes</summary>',
      '',
      ...bullets,
      '',
      '</details>',
      '',
      `<!-- boss-verify-notes head=${head} digest=${digest} ${payload} -->`,
    ].join('\n')
    return { body, digest }
  }
  let low = 0,
    high = Math.max(0, ...notes.map((note) => note.text.length))
  if (render(0).body.length > 65536)
    throw new RangeError('note identities and tickets exceed comment limit')
  while (low < high) {
    const mid = Math.ceil((low + high) / 2)
    if (render(mid).body.length <= 65536) low = mid
    else high = mid - 1
  }
  return render(low)
}

/** Accept only a canonical, schema-checked final trailer whose digest matches its payload. */
export function parseNotesTrailer(body) {
  if (typeof body !== 'string') return null
  const match = body
    .trimEnd()
    .match(
      /(?:^|\n)<!-- boss-verify-notes head=([a-f0-9]{40}) digest=([a-f0-9]{16}) ([A-Za-z0-9_-]+) -->$/i,
    )
  if (!match) return null
  try {
    const bytes = Buffer.from(match[3], 'base64url')
    if (bytes.toString('base64url') !== match[3]) return null
    const payload = JSON.parse(bytes.toString('utf8'))
    if (
      !exactKeys(payload, ['version', 'notes', 'tickets']) ||
      payload.version !== 1 ||
      !validNotes(payload.notes) ||
      !validTickets(payload.tickets, payload.notes) ||
      digestOf(payload.notes, payload.tickets) !== match[2]
    )
      return null
    return { head: match[1], digest: match[2], notes: payload.notes, tickets: payload.tickets }
  } catch {
    return null
  }
}

/** Build an operator-selected follow-up without performing any tracker writes. */
export function followUpIssue({ repo, pr, prUrl, note }) {
  const marker = `<!-- boss-follow-up repo=${repo} pr=${pr} note=${note.id} -->`
  const title = `Follow-up: ${note.summary}`
  return {
    // gate-region-ok: cap the tracker title length; this is not source-region extraction.
    title: title.length > 256 ? title.slice(0, 255) + '…' : title,
    description: [
      `Follow-up from ${prUrl} (pr=${pr} note=${note.id}).`,
      '',
      `Severity: ${note.severity}; source: ${note.source}.`,
      ...(note.file ? [`Location: ${note.file}${note.line === null ? '' : `:${note.line}`}`] : []),
      '',
      note.text,
      '',
      marker,
    ].join('\n'),
    marker,
  }
}
