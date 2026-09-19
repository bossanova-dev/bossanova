// progress-comment.mjs
// Pure helpers for boss-epic's single-comment epic-progress protocol: the
// epic reports its ticket-by-ticket progress as ONE tracker comment, edited
// in place, using a hidden marker line as both the anchor a human recognizes
// and the resume point a re-run driver finds. ZERO tracker/I-O knowledge —
// no Linear client, no MCP calls, no Date.now()/timezone dependence. node
// builtins only (mirrors the dependency-free cron worktree; this file also
// ships standalone inside the published boss-epic skill payload, so it must
// not import anything outside this module).
//
// Contract (Task C): the three functions below decide WHAT the comment
// should look like and WHICH tracker op applies it; they never call a
// tracker themselves. The driver executes that decision through the tracker
// adapter's declarative operationMap — readComments, writeComment (create),
// and updateComment (update) — defined in tracker/adapter.mjs
// (REQUIRED_TRACKER_OPERATIONS). Each configured tracker adapter (e.g.
// tracker/linear.mjs) maps those three capability names onto its
// own concrete tool call; this module never names one. Concretely: the
// driver calls readComments, feeds the result plus the marker and a body from
// renderProgressComment into planProgressCommentUpsert, and executes
// the returned `op` verbatim — `create` -> writeComment({issueId, body}),
// `update` -> updateComment({id: commentId, body}). Both `issueId` (the
// ticket the comment lives on) and `commentId`/`id` (which existing comment
// to update) come from the driver, not from this module — planProgressCommentUpsert
// only ever returns `body` (and, on update, `commentId`); the driver supplies
// `issueId` itself when it calls writeComment. That is the entire
// single-comment progress protocol, expressible with no raw GraphQL in any
// driver.

/**
 * @typedef {Object} ProgressTicket
 * @property {string} id       Tracker issue id/identifier (e.g. "PROJ-501").
 * @property {string} title    Human-readable ticket title.
 * @property {'pending'|'building'|'green'|'merged'|'failed'|'skipped'} status
 *           One of PROGRESS_STATUSES.
 * @property {string} [pr]     PR reference — a URL or a number, always as a
 *           string (validateProgressState rejects a non-string `pr`).
 * @property {string} [session] Driver-internal session id. Bookkeeping only:
 *           not rendered.
 * @property {number} [rounds] Driver-internal round counter. Bookkeeping only:
 *           not rendered.
 * @property {string} [note]   Free-text status note (e.g. a failure reason).
 *           The escape hatch for anything the four-column rendered table
 *           cannot otherwise express — see PROGRESS_STATUSES.
 */

/**
 * @typedef {Object} ProgressState
 * @property {string} epicId     Epic identifier, rendered in the heading.
 * @property {string} marker     The BARE resume-anchor token, without the
 *           `<!-- ... -->` delimiters — renderProgressComment and
 *           planProgressCommentUpsert both wrap it via progressMarkerAnchor,
 *           so an already-wrapped value would double-wrap. Pass the same bare
 *           token to both; validateProgressState rejects one carrying `<!--`
 *           or `-->`.
 * @property {string} updatedAt  Caller-supplied timestamp string, rendered
 *           verbatim — never generated here (byte-stability contract).
 * @property {ProgressRun} [run]  OPTIONAL driver-owned run metadata. Absent ⇒
 *           the rendered body is byte-identical to what it was before this block
 *           existed, so every pre-existing caller is unaffected.
 * @property {ProgressTicket[]} tickets  Rendered in this exact array order;
 *           the driver owns ordering, this module never sorts them.
 */

/**
 * @typedef {Object} ProgressRun
 * @property {string} runId   The run this comment belongs to.
 * @property {string} phase   Current driver phase.
 * @property {string} status  RUNNING / RUNNING_BUT_UNWATCHED / BLOCKED / DONE.
 * @property {string} [lastReconciledAt] Last authoritative reconciliation.
 * @property {string} [nextWake] Next durable wake mechanism/time, when non-terminal.
 */

// The only ticket.status values validateProgressState accepts. This order is
// also the display order of the legend LEGEND builds from them below.
//
// Deliberately six values, no `repairing`: a driver maps its own extra
// lifecycle states onto these six rather than this list growing to match every
// driver (a queued state -> `pending`; an in-flight or repairing state ->
// `building`, with any round number carried in `rounds` and surfaced via
// `note` if it needs to be visible). Widening the list is a schema change and
// belongs in its own ticket.
//
// Frozen, like PROGRESS_STATUS_ICONS: LEGEND is computed once at module load,
// so pushing onto the exported array would make validateProgressState accept a
// status the renderer has no icon for and the legend never mentions.
export const PROGRESS_STATUSES = Object.freeze([
  'pending',
  'building',
  'green',
  'merged',
  'failed',
  'skipped',
])

// Read by both the per-row status cell and LEGEND, so the two can never drift.
// Frozen so a caller can't mutate the shared icon set under the renderer.
export const PROGRESS_STATUS_ICONS = Object.freeze({
  pending: '⏳',
  building: '🔨',
  green: '✅',
  merged: '🚢',
  failed: '❌',
  skipped: '⏭️',
})

// The run-metadata block's fields, in render order. The first three are required whenever `run` is
// present; the last two render as an em dash when blank. This ordered list is the single source of
// truth for BOTH the renderer and parseProgressRunMetadata, so the write and the read-back cannot
// drift — which is what makes the block a resume join rather than decoration.
export const PROGRESS_RUN_FIELDS = Object.freeze([
  'runId',
  'phase',
  'status',
  'lastReconciledAt',
  'nextWake',
])

const PROGRESS_RUN_REQUIRED = Object.freeze(['runId', 'phase', 'status'])

// The block's lead line. Matched by parseProgressRunMetadata, so it is a structural token rather
// than prose: rewording it is a schema change.
export const PROGRESS_RUN_HEADING = 'Run state:'

function isNonEmptyString(value) {
  return typeof value === 'string' && value.trim().length > 0
}

/**
 * The exact anchor line renderProgressComment emits as the comment's first
 * line, built from the bare marker token. The single source of truth for that
 * wrapping: the renderer writes it and planProgressCommentUpsert matches on
 * it, both through this one function, so the two cannot drift.
 *
 * Exported — despite both in-module callers wrapping for you — so a driver can
 * name the anchor it is looking for: logging which line it resumed on, or
 * asserting in its own tests that a comment body starts with it. Callers never
 * need it to CALL this module: pass the bare token everywhere.
 * @param {string} marker bare token (no `<!--`/`-->`; see ProgressState.marker)
 * @returns {string}
 */
export function progressMarkerAnchor(marker) {
  return `<!-- ${marker} -->`
}

function isBlankOrAbsent(value) {
  return value === undefined || value === null || (typeof value === 'string' && value.trim() === '')
}

function pushRequiredString(errors, path, value) {
  if (!isNonEmptyString(value)) errors.push(`${path}: required non-empty string`)
}

// Single-line check for the three top-level strings the renderer interpolates
// RAW (they are not table cells, so they never pass through sanitizeCell): a
// line break in any of them injects arbitrary extra markdown lines into the
// rendered body — an epicId of "E-1\n# oops" grows a heading. Only checked
// once the value is already a non-empty string, so a missing field reports
// exactly one error, not two.
function pushSingleLine(errors, path, value) {
  if (isNonEmptyString(value) && /[\r\n]/.test(value)) {
    errors.push(`${path}: must not contain a line break`)
  }
}

const HTML_COMMENT_DELIMITER = /<!--|-->/

// Unsafe in all three raw-interpolated fields: in `marker` it double-wraps the
// anchor, and in `epicId`/`updatedAt` an unterminated `<!--` opens a comment
// that swallows the table, legend and Updated line in any HTML-rendering
// tracker — while the line-1 anchor still matches, so the driver keeps
// updating a comment that renders blank.
function pushNoCommentDelimiters(errors, path, value) {
  if (isNonEmptyString(value) && HTML_COMMENT_DELIMITER.test(value)) {
    errors.push(`${path}: must not contain HTML comment delimiters ("<!--" or "-->")`)
  }
}

// Render an out-of-vocabulary status for an error message WITHOUT throwing.
// Deliberately not a bare JSON.stringify: that throws on a BigInt (and on an
// object with a throwing toJSON), which would break this validator's
// never-throws contract on exactly the malformed input it exists to report.
function describeStatus(value) {
  if (typeof value === 'string') return JSON.stringify(value)
  if (value === undefined || value === null) return 'null'
  if (typeof value === 'object' || typeof value === 'function') return `(${typeof value})`
  return `(${typeof value}) ${String(value)}`
}

/**
 * Validate a ProgressState. Repo house style: return `{ok, errors}`, never
 * throw — see validatePlanDescription in skill-config.mjs for the sibling
 * convention this follows. `errors` is stable and deterministic: top-level
 * fields (epicId, marker, updatedAt) in that order — each reporting its
 * required-non-empty-string error first, then its render-safety errors in the
 * order line-break, HTML-comment-delimiters — then `tickets` itself, then each
 * ticket in array-index order, then each ticket's own fields in the order id,
 * title, status, pr, session, note, rounds.
 *
 * Scope: this checks the shape AND the render-safety of the values the
 * renderer interpolates raw (epicId, marker, updatedAt). It does NOT promise
 * that every free-text ticket cell renders inertly — cell values go through
 * sanitizeCell, which neutralises the markdown table delimiters (`\` and `|`)
 * and line breaks but deliberately leaves other markdown/HTML in a `title` or
 * `note` alone.
 * @param {unknown} state
 * @returns {{ok: boolean, errors: string[]}}
 */
export function validateProgressState(state) {
  if (state === null || typeof state !== 'object' || Array.isArray(state)) {
    return { ok: false, errors: ['state: required object'] }
  }

  const errors = []
  for (const field of ['epicId', 'marker', 'updatedAt']) {
    pushRequiredString(errors, field, state[field])
    pushSingleLine(errors, field, state[field])
    pushNoCommentDelimiters(errors, field, state[field])
  }

  // `run` is optional, but a PRESENT run block is validated to the same standard as the three raw
  // top-level strings: every value is interpolated raw into the block, so a line break or an
  // unterminated `<!--` in any of them wrecks the body while the line-1 anchor still matches — the
  // driver would then keep updating a comment that renders blank and carries no resume data.
  if (state.run !== undefined && state.run !== null) {
    if (typeof state.run !== 'object' || Array.isArray(state.run)) {
      errors.push('run: required object when present')
    } else {
      for (const field of PROGRESS_RUN_FIELDS) {
        const value = state.run[field]
        const path = `run.${field}`
        if (PROGRESS_RUN_REQUIRED.includes(field)) {
          pushRequiredString(errors, path, value)
        } else if (value !== undefined && value !== null && typeof value !== 'string') {
          errors.push(`${path}: must be a string`)
          continue
        }
        pushSingleLine(errors, path, value)
        pushNoCommentDelimiters(errors, path, value)
      }
    }
  }

  if (!Array.isArray(state.tickets)) {
    errors.push('tickets: required array')
  } else {
    state.tickets.forEach((ticket, index) => {
      const prefix = `tickets[${index}]`
      if (!ticket || typeof ticket !== 'object' || Array.isArray(ticket)) {
        errors.push(`${prefix}: required object`)
        return
      }
      pushRequiredString(errors, `${prefix}.id`, ticket.id)
      pushRequiredString(errors, `${prefix}.title`, ticket.title)
      if (typeof ticket.status !== 'string' || !PROGRESS_STATUSES.includes(ticket.status)) {
        errors.push(`${prefix}.status: unknown status ${describeStatus(ticket.status)}`)
      }
      for (const field of ['pr', 'session', 'note']) {
        const value = ticket[field]
        if (value !== undefined && value !== null && typeof value !== 'string') {
          errors.push(`${prefix}.${field}: must be a string`)
        }
      }
      if (
        ticket.rounds !== undefined &&
        ticket.rounds !== null &&
        !Number.isFinite(ticket.rounds)
      ) {
        errors.push(`${prefix}.rounds: must be a finite number`)
      }
    })
  }

  return { ok: errors.length === 0, errors }
}

// Escape `\` and `|` and collapse CR/LF so a title or note containing a
// backslash, a pipe, or a newline cannot break the markdown table. The
// backslash MUST be escaped before the pipe: escaping `|` -> `\|` first and
// then `\` -> `\\` would double-escape an already-literal `\|` into `\\|`,
// which markdown reads as an escaped backslash followed by a LIVE,
// unescaped pipe — reintroducing the exact table break this function exists
// to prevent. Escaping the backslash first makes that same input round-trip
// as `\\\|` (escaped backslash, escaped pipe) instead. The single helper
// every cell value passes through (id, title, pr, note) — correctness, not
// cosmetics.
function sanitizeCell(value) {
  return String(value)
    .replace(/\\/g, '\\\\')
    .replace(/\|/g, '\\|')
    .replace(/[\r\n]+/g, ' ')
}

// Absent/blank optional cells (pr, note) render as an em dash; present ones
// are sanitized like every other cell.
function cellOrDash(value) {
  return isBlankOrAbsent(value) ? '—' : sanitizeCell(value)
}

function renderTicketRow(ticket) {
  const idTitle = `${sanitizeCell(ticket.id)} — ${sanitizeCell(ticket.title)}`
  const status = `${PROGRESS_STATUS_ICONS[ticket.status]} ${ticket.status}`
  return `| ${idTitle} | ${status} | ${cellOrDash(ticket.pr)} | ${cellOrDash(ticket.note)} |`
}

// Built FROM PROGRESS_STATUS_ICONS (iterated in PROGRESS_STATUSES order) so
// the legend can never drift from the per-row icon mapping.
const LEGEND = PROGRESS_STATUSES.map((status) => `${PROGRESS_STATUS_ICONS[status]} ${status}`).join(
  ' · ',
)

/**
 * Render a ProgressState to the exact markdown byte contract boss-epic
 * upserts as its single progress comment. Byte-stable: never calls
 * Date.now()/new Date()/toLocaleString or anything locale/timezone
 * dependent — `updatedAt` is rendered verbatim from `state`. Tickets render
 * in `state.tickets`' input array order and are never sorted here; the
 * driver owns ordering, and preserving it is what makes equal state render
 * to equal bytes (the idempotence contract other callers rely on).
 *
 * `ticket.session` and `ticket.rounds` are bookkeeping fields and are not
 * rendered; `note` is the escape hatch for anything the four columns cannot
 * express. Changing the column set is a schema decision for the ticket that
 * adopts this renderer — made once, reflected in the snapshot test — not an
 * ad-hoc widening.
 *
 * Does not validate `state`: run validateProgressState first. Its scope is
 * shape plus the render-safety of the raw-interpolated top-level fields, not
 * the markdown/HTML content of free-text ticket cells (see there).
 * @param {ProgressState} state
 * @returns {string} markdown, ending in exactly one trailing "\n"
 */
export function renderProgressComment(state) {
  const { epicId, marker, updatedAt, tickets, run } = state
  const lines = [progressMarkerAnchor(marker), `### Epic progress: ${epicId}`, '']

  if (tickets.length === 0) {
    lines.push('_No tickets yet._')
  } else {
    lines.push('| Ticket | Status | PR | Note |')
    lines.push('| --- | --- | --- | --- |')
    for (const ticket of tickets) lines.push(renderTicketRow(ticket))
  }

  lines.push('', `Legend: ${LEGEND}`)
  if (run !== undefined && run !== null) {
    lines.push('', PROGRESS_RUN_HEADING, '')
    for (const field of PROGRESS_RUN_FIELDS) {
      lines.push(`- ${field}: ${isNonEmptyString(run[field]) ? run[field] : '—'}`)
    }
  }
  lines.push('', `Updated: ${updatedAt}`)
  return lines.join('\n') + '\n'
}

/**
 * Read the run-metadata block back out of a rendered comment body — the resume JOIN. A fresh
 * driver process finds the progress comment by its marker, reads `runId` from here, and can then
 * locate its own persisted state file without any turn history. That is the only reason the block
 * is machine-readable rather than prose.
 *
 * Returns `null` when the body carries no block (a pre-block comment, or a caller that never passed
 * `run`), which is a clean "no join available", not an error. An em dash reads back as `''`, so a
 * render/parse round trip is lossless for every value the renderer accepts.
 * @param {unknown} body A comment body as `renderProgressComment` wrote it.
 * @returns {{runId: string, phase: string, status: string, lastReconciledAt: string, nextWake: string} | null}
 */
export function parseProgressRunMetadata(body) {
  if (typeof body !== 'string' || !body.includes(PROGRESS_RUN_HEADING)) return null
  const out = {}
  let found = false
  for (const field of PROGRESS_RUN_FIELDS) {
    // Anchored at line start so a field name quoted inside a ticket `note` cell cannot be read as
    // the block's own value.
    const match = new RegExp(`^- ${field}: (.*)$`, 'm').exec(body)
    if (!match) {
      out[field] = ''
      continue
    }
    found = true
    const value = match[1].trim()
    out[field] = value === '—' ? '' : value
  }
  return found ? out : null
}

// The comment the upsert resolved to must carry a usable id or the `update`
// op this module returns is unexecutable. Throwing beats returning
// `commentId: undefined` (which the driver would hand to updateComment) and
// beats falling back to `create` (which would silently post a duplicate
// comment on every run).
function requireCommentId(comment) {
  if (!isNonEmptyString(comment?.id)) {
    throw new TypeError(
      'planProgressCommentUpsert: the matching comment has no usable `id` — the tracker ' +
        "adapter's readComments must surface each comment's id, otherwise the update op has " +
        'nothing to target',
    )
  }
  return comment.id
}

function parseCreatedAtForSort(createdAt) {
  // Missing/unparseable createdAt sorts as oldest (documented tie-break),
  // rather than throwing or treating it as "newest" — a comment with no
  // timestamp we can read should never win over one we can.
  if (createdAt === undefined || createdAt === null) return -Infinity
  const time = new Date(createdAt).getTime()
  return Number.isNaN(time) ? -Infinity : time
}

/**
 * Decide whether boss-epic's next progress-comment write should create a
 * new tracker comment or update an existing one — a pure decision function,
 * no tracker access, no I/O. `comments` is a `readComments` result: an array
 * of `{id, body, createdAt}`.
 *
 * `marker` is the same BARE token carried in `ProgressState.marker`. A comment
 * matches when its `body` contains `progressMarkerAnchor(marker)` — the full
 * anchor LINE the renderer writes, not the bare token. Matching the whole
 * anchor is what keeps a human comment that merely quotes the token in prose
 * from being silently overwritten; wrapping here rather than asking the caller
 * to wrap makes that unreachable by construction, and keeps one marker
 * spelling across the whole protocol.
 *   - No match (including `comments` missing/not an array, treated as
 *     empty) -> `{op: 'create', body}`.
 *   - Exactly one match -> `{op: 'update', commentId, body}`.
 *   - Multiple matches -> the newest one wins (greatest `createdAt`, parsed
 *     as a date; a missing/unparseable `createdAt` sorts oldest; an exact
 *     tie is broken by last-in-input-order, i.e. the tracker's own
 *     ordering), plus a deterministic `warning` string naming the count and
 *     the winning id — surfaced so a driver can log/alert on the duplicate
 *     rather than silently discarding it.
 *
 * `body` is passed through onto the result verbatim, so the driver hands
 * one object straight to the adapter op this result names (writeComment for
 * `create`, updateComment for `update` — see the module header).
 *
 * Four caller/adapter errors throw a TypeError rather than returning
 * `{ok: false}`, because none is a normal validation failure and each fails
 * silently and destructively if tolerated:
 *   - a blank/absent `marker` — matching on `<!--  -->` would match every
 *     comment this module has ever rendered, silently updating an unrelated
 *     epic's progress comment;
 *   - a `marker` already carrying `<!--`/`-->` — it would be wrapped a second
 *     time here and match nothing, so every run would post a duplicate. Same
 *     rule validateProgressState enforces on `ProgressState.marker`, applied
 *     here too so a driver that skipped validation still fails loudly;
 *   - a blank/absent `body` — an `update` carrying `''` blanks the existing
 *     comment INCLUDING its anchor line, so the next run matches nothing and
 *     posts a fresh comment: the same unbounded-duplicates outcome, reached
 *     from the other direction. (renderProgressComment can never return empty,
 *     so this only ever catches a driver bug — which is exactly when a loud
 *     failure is worth most.);
 *   - a winning match whose `id` is not a non-empty string — updateComment
 *     would have nothing to target. This is the exact gap the adapter
 *     contract warns about (readComments MUST surface each comment's id);
 *     degrading to `create` instead would post a fresh duplicate comment on
 *     every single run, which is the unbounded-duplicates failure the
 *     single-comment protocol exists to prevent.
 * @param {{comments?: {id: string, body: string, createdAt?: string}[], marker: string, body: string}} args
 *        `marker` is the bare token (same value as `ProgressState.marker`).
 * @returns {{op: 'create'|'update', body: string, commentId?: string, warning?: string}}
 */
export function planProgressCommentUpsert({ comments, marker, body } = {}) {
  if (!isNonEmptyString(marker)) {
    throw new TypeError(
      'planProgressCommentUpsert: marker must be a non-empty string (a blank marker would ' +
        'match every progress comment, silently overwriting an unrelated one)',
    )
  }
  if (HTML_COMMENT_DELIMITER.test(marker)) {
    throw new TypeError(
      'planProgressCommentUpsert: marker must be the BARE token, without `<!--`/`-->` — it is ' +
        'wrapped here via progressMarkerAnchor, so an already-wrapped marker matches nothing ' +
        'and every run would post a duplicate comment',
    )
  }
  if (!isNonEmptyString(body)) {
    throw new TypeError(
      'planProgressCommentUpsert: body must be a non-empty string (a blank body would erase ' +
        'the existing comment along with its anchor line, so the next run would post a duplicate)',
    )
  }

  const anchor = progressMarkerAnchor(marker)
  const list = Array.isArray(comments) ? comments : []
  const matches = list.filter(
    (comment) => typeof comment?.body === 'string' && comment.body.includes(anchor),
  )

  if (matches.length === 0) return { op: 'create', body }
  if (matches.length === 1) return { op: 'update', commentId: requireCommentId(matches[0]), body }

  let winner = matches[0]
  let winnerTime = parseCreatedAtForSort(winner.createdAt)
  for (let i = 1; i < matches.length; i++) {
    const candidate = matches[i]
    const candidateTime = parseCreatedAtForSort(candidate.createdAt)
    // >= (not >) so an exact tie keeps advancing to the later candidate,
    // resolving to last-in-input-order as documented.
    if (candidateTime >= winnerTime) {
      winner = candidate
      winnerTime = candidateTime
    }
  }

  const winnerId = requireCommentId(winner)
  const warning =
    `${matches.length} comments match the progress marker; updating the newest (${winnerId}) ` +
    `and ignoring ${matches.length - 1} older duplicates`
  return { op: 'update', commentId: winnerId, body, warning }
}

/**
 * Project ONE coordinator ProgressState into one per-parent ProgressState — the
 * multi-root reporting contract: a combined run keeps a single state covering the
 * deduplicated child universe, and each selected epic parent gets its own comment
 * showing only the children that parent contains.
 *
 * Returns `[{parentId, state}, ...]` in `parentIds` order. Each `state` is a full
 * ProgressState the driver feeds straight to `renderProgressComment` +
 * `planProgressCommentUpsert`, so every parent keeps the unchanged single-comment
 * marker/upsert contract — the driver only varies the issue it writes to.
 *
 * What is projected, and why each way round:
 *   - `epicId` becomes the PARENT's id, because that comment's heading names the
 *     epic a reader is looking at, not the combined run.
 *   - `marker` and `updatedAt` are carried through VERBATIM. The marker is the
 *     per-comment resume anchor and the comments live on different issues, so one
 *     shared token cannot collide; varying it per parent would instead strand the
 *     comment a previous run created. `updatedAt` stays byte-stable, as ever.
 *   - `tickets` are filtered to that parent's membership and kept in the
 *     COORDINATOR's row order, never the membership array's. One ordering
 *     authority is what makes a child shared by two parents render identical
 *     bytes in both projections — the row objects are the very same objects.
 *
 * A membership id with no row in `state.tickets` is simply absent from the
 * projection: the coordinator's table is the single authority on what is
 * reportable, and inventing a row here would report a ticket nobody classified.
 * A parent with no membership at all projects an empty — still valid — table.
 *
 * Pure: no tracker access, no clock, and the input state is never mutated.
 * Throws (rather than returning `{ok:false}`) on a state that would not validate
 * or a blank parent id, matching `planProgressCommentUpsert`: both are driver
 * wiring errors whose silent forms are destructive — an unvalidated state
 * renders a broken comment on EVERY parent at once.
 *
 * @param {{state: ProgressState, parentIds: string[], childrenByParent?: Record<string, string[]>}} args
 * @returns {{parentId: string, state: ProgressState}[]}
 */
export function projectProgressByParent({ state, parentIds, childrenByParent } = {}) {
  const validity = validateProgressState(state)
  if (!validity.ok) {
    throw new TypeError(
      'projectProgressByParent: the coordinator state is invalid, so it cannot be projected — ' +
        `it would render a broken comment on every selected parent at once: ${validity.errors.join('; ')}`,
    )
  }
  if (!Array.isArray(parentIds) || parentIds.length === 0) {
    throw new TypeError(
      'projectProgressByParent: parentIds must be a non-empty array of selected parent ids',
    )
  }
  for (const parentId of parentIds) {
    if (!isNonEmptyString(parentId)) {
      throw new TypeError(
        `projectProgressByParent: parentIds contains a blank/non-string parent id (${String(parentId)})`,
      )
    }
  }

  const membership = childrenByParent ?? {}
  return parentIds.map((parentId) => {
    const members = new Set(membership[parentId] ?? [])
    return {
      parentId,
      state: {
        epicId: parentId,
        marker: state.marker,
        updatedAt: state.updatedAt,
        tickets: state.tickets.filter((ticket) => members.has(ticket.id)),
      },
    }
  })
}
