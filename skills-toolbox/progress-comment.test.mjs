// progress-comment.test.mjs — pure unit tests for the single-comment
// epic-progress protocol helpers. node builtins only, styled after
// dag-scheduler.test.mjs: small behaviour-named tests, a fixture factory.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  PROGRESS_STATUSES,
  progressMarkerAnchor,
  validateProgressState,
  renderProgressComment,
  planProgressCommentUpsert,
} from './progress-comment.mjs'

// Fixture factories — a full valid state and a ticket builder, mirroring the
// `n()` abstract-node factory in dag-scheduler.test.mjs.
const ticket = (over = {}) => ({
  id: 'BOS-1',
  title: 'Do the thing',
  status: 'pending',
  ...over,
})

const state = (over = {}) => ({
  epicId: 'BOS-500',
  marker: 'boss-epic:progress:BOS-500',
  updatedAt: '2026-07-25T00:00:00Z',
  tickets: [ticket()],
  ...over,
})

// ---------------------------------------------------------------------------
// validateProgressState
// ---------------------------------------------------------------------------

test('validateProgressState: a valid full state passes', () => {
  const s = state({
    tickets: [
      ticket({
        id: 'BOS-1',
        title: 'Ship the thing',
        status: 'building',
        pr: 'https://github.com/o/r/pull/1',
        session: 'sess-1',
        rounds: 2,
        note: 'waiting on CI',
      }),
    ],
  })
  assert.deepEqual(validateProgressState(s), { ok: true, errors: [] })
})

test('validateProgressState: a valid minimal state (no optional ticket fields) passes', () => {
  const s = state({ tickets: [{ id: 'BOS-1', title: 'Do the thing', status: 'pending' }] })
  assert.deepEqual(validateProgressState(s), { ok: true, errors: [] })
})

test('validateProgressState: an empty tickets array is valid', () => {
  const s = state({ tickets: [] })
  assert.deepEqual(validateProgressState(s), { ok: true, errors: [] })
})

test('validateProgressState: a non-object state fails with a single top-level error', () => {
  assert.deepEqual(validateProgressState(null), { ok: false, errors: ['state: required object'] })
  assert.deepEqual(validateProgressState('nope'), {
    ok: false,
    errors: ['state: required object'],
  })
  assert.deepEqual(validateProgressState([1, 2]), {
    ok: false,
    errors: ['state: required object'],
  })
})

test('validateProgressState: missing epicId', () => {
  const s = state()
  delete s.epicId
  const result = validateProgressState(s)
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('epicId: required non-empty string'))
})

test('validateProgressState: blank/whitespace marker', () => {
  const result = validateProgressState(state({ marker: '   ' }))
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('marker: required non-empty string'))
})

test('validateProgressState: missing updatedAt', () => {
  const s = state()
  delete s.updatedAt
  const result = validateProgressState(s)
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('updatedAt: required non-empty string'))
})

test('validateProgressState: tickets not an array', () => {
  const result = validateProgressState(state({ tickets: 'nope' }))
  assert.deepEqual(result, { ok: false, errors: ['tickets: required array'] })
})

test('validateProgressState: a null ticket entry fails with "tickets[N]: required object"', () => {
  const s = state({ tickets: [null] })
  const result = validateProgressState(s)
  assert.deepEqual(result, { ok: false, errors: ['tickets[0]: required object'] })
})

test('validateProgressState: an array ticket entry fails with "tickets[N]: required object"', () => {
  const s = state({ tickets: [['not', 'a', 'ticket']] })
  const result = validateProgressState(s)
  assert.deepEqual(result, { ok: false, errors: ['tickets[0]: required object'] })
})

test('validateProgressState: ticket with status omitted entirely fails with "unknown status null"', () => {
  const s = state({ tickets: [{ id: 'BOS-1', title: 'Do the thing' }] })
  const result = validateProgressState(s)
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('tickets[0].status: unknown status null'))
})

test('validateProgressState: ticket missing id', () => {
  const s = state({ tickets: [ticket({ id: undefined })] })
  const result = validateProgressState(s)
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('tickets[0].id: required non-empty string'))
})

test('validateProgressState: ticket missing title', () => {
  const s = state({ tickets: [ticket({ title: undefined })] })
  const result = validateProgressState(s)
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('tickets[0].title: required non-empty string'))
})

test('validateProgressState: ticket with an unknown status', () => {
  const s = state({ tickets: [ticket(), ticket({ status: 'wip' })] })
  const result = validateProgressState(s)
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('tickets[1].status: unknown status "wip"'))
})

test('validateProgressState: an exotic status type is reported, not thrown on', () => {
  // The contract is {ok, errors}, never throw. JSON.stringify THROWS on a
  // BigInt, so formatting the offending value must not go through it.
  for (const status of [10n, Symbol('s'), 42, true, {}, () => {}]) {
    let result
    assert.doesNotThrow(
      () => {
        result = validateProgressState(state({ tickets: [ticket({ status })] }))
      },
      `validateProgressState must not throw for status of type ${typeof status}`,
    )
    assert.equal(result.ok, false)
    assert.ok(
      result.errors.some((e) => e.startsWith('tickets[0].status: unknown status')),
      `expected an unknown-status error for ${typeof status}; got ${result.errors.join(' | ')}`,
    )
  }
  // Pin one exact string so the formatter cannot silently regress to something
  // uninformative while still technically not throwing.
  assert.ok(
    validateProgressState(state({ tickets: [ticket({ status: 10n })] })).errors.includes(
      'tickets[0].status: unknown status (bigint) 10',
    ),
  )
})

test('validateProgressState: every declared status is accepted', () => {
  for (const status of PROGRESS_STATUSES) {
    const result = validateProgressState(state({ tickets: [ticket({ status })] }))
    assert.equal(result.ok, true, `expected status "${status}" to be valid`)
  }
})

test('validateProgressState: ticket pr of the wrong type', () => {
  const s = state({ tickets: [ticket({ pr: 42 })] })
  const result = validateProgressState(s)
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('tickets[0].pr: must be a string'))
})

test('validateProgressState: ticket rounds non-numeric', () => {
  const s = state({ tickets: [ticket({ rounds: 'two' })] })
  const result = validateProgressState(s)
  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('tickets[0].rounds: must be a finite number'))
})

test('validateProgressState: a marker already in `<!-- ... -->` form is rejected (it would double-wrap)', () => {
  // The exact trap the JSDoc calls out: renderProgressComment wraps the marker
  // itself, so an already-wrapped marker renders as
  // `<!-- <!-- token --> -->` — the inner `-->` closes the comment early and
  // leaks visible junk, and the written anchor no longer matches the anchor a
  // resuming driver looks for.
  const result = validateProgressState(state({ marker: '<!-- boss-epic:progress:BOS-500 -->' }))
  assert.equal(result.ok, false)
  assert.ok(
    result.errors.includes('marker: must not contain HTML comment delimiters ("<!--" or "-->")'),
  )
})

test('validateProgressState: a bare `-->` anywhere in the marker is rejected too', () => {
  const result = validateProgressState(state({ marker: 'progress --> here' }))
  assert.equal(result.ok, false)
  assert.ok(
    result.errors.includes('marker: must not contain HTML comment delimiters ("<!--" or "-->")'),
  )
})

test('validateProgressState: HTML comment delimiters are rejected in epicId and updatedAt too, not just marker', () => {
  // An unterminated `<!--` in either raw-interpolated field opens a comment
  // that swallows the table, legend and Updated line in an HTML-rendering
  // tracker — while the line-1 anchor still matches, so the driver keeps
  // updating a comment that renders blank.
  for (const field of ['epicId', 'marker', 'updatedAt']) {
    for (const value of ['<!-- swallow', 'trailing --> leak']) {
      const result = validateProgressState(state({ [field]: value }))
      assert.equal(result.ok, false, `expected ${field}=${JSON.stringify(value)} to fail`)
      assert.ok(
        result.errors.includes(
          `${field}: must not contain HTML comment delimiters ("<!--" or "-->")`,
        ),
        `expected the delimiter error for ${field}; got ${JSON.stringify(result.errors)}`,
      )
    }
  }
})

test('validateProgressState: a field failing BOTH render-safety checks reports them in the documented order', () => {
  // The per-field order the JSDoc promises is required -> line-break ->
  // delimiters. The loops above each exercise one check in isolation, so
  // nothing else pins their relative order on a single field.
  const result = validateProgressState(state({ epicId: 'a\n<!-- x' }))
  assert.deepEqual(
    result.errors.filter((e) => e.startsWith('epicId')),
    [
      'epicId: must not contain a line break',
      'epicId: must not contain HTML comment delimiters ("<!--" or "-->")',
    ],
  )
})

test('PROGRESS_STATUSES is frozen so the legend and the accepted vocabulary cannot drift', () => {
  // LEGEND is computed once at module load from this list; a caller pushing
  // onto it would make validateProgressState accept a status the renderer has
  // no icon for and the legend never mentions.
  assert.equal(Object.isFrozen(PROGRESS_STATUSES), true)
  assert.throws(() => PROGRESS_STATUSES.push('repairing'), TypeError)
})

test('validateProgressState: a line break in epicId, marker, or updatedAt is rejected', () => {
  // These three are interpolated RAW by the renderer (they are not table
  // cells, so they never pass through sanitizeCell) — a line break injects
  // arbitrary extra markdown into the body.
  for (const field of ['epicId', 'marker', 'updatedAt']) {
    const result = validateProgressState(state({ [field]: 'ok\n# injected heading' }))
    assert.equal(result.ok, false, `expected a line break in ${field} to fail validation`)
    assert.ok(
      result.errors.includes(`${field}: must not contain a line break`),
      `expected the line-break error for ${field}; got ${JSON.stringify(result.errors)}`,
    )
  }
})

test('validateProgressState: a missing top-level string reports exactly one error, not a line-break error too', () => {
  const s = state()
  delete s.epicId
  const result = validateProgressState(s)
  assert.deepEqual(
    result.errors.filter((e) => e.startsWith('epicId')),
    ['epicId: required non-empty string'],
  )
})

test('validateProgressState: multiple simultaneous problems produce the full deterministic errors array', () => {
  const result = validateProgressState({
    epicId: '',
    marker: 'boss-epic:progress:BOS-500',
    tickets: [{ id: '', title: 'ok', status: 'wip', rounds: 'two' }],
  })
  assert.deepEqual(result, {
    ok: false,
    errors: [
      'epicId: required non-empty string',
      'updatedAt: required non-empty string',
      'tickets[0].id: required non-empty string',
      'tickets[0].status: unknown status "wip"',
      'tickets[0].rounds: must be a finite number',
    ],
  })
})

// ---------------------------------------------------------------------------
// renderProgressComment
// ---------------------------------------------------------------------------

test('renderProgressComment: exact-bytes snapshot across several tickets and statuses', () => {
  const s = state({
    epicId: 'BOS-500',
    marker: 'boss-epic:progress:BOS-500',
    updatedAt: '2026-07-25T00:00:00Z',
    tickets: [
      ticket({
        id: 'BOS-501',
        title: 'Add the widget',
        status: 'merged',
        pr: 'https://github.com/o/r/pull/1',
        note: 'shipped clean',
      }),
      ticket({ id: 'BOS-502', title: 'Fix the thing', status: 'building' }),
      ticket({
        id: 'BOS-503',
        title: 'Investigate flake',
        status: 'failed',
        pr: 'https://github.com/o/r/pull/3',
      }),
    ],
  })
  const expected =
    '<!-- boss-epic:progress:BOS-500 -->\n' +
    '### Epic progress: BOS-500\n' +
    '\n' +
    '| Ticket | Status | PR | Note |\n' +
    '| --- | --- | --- | --- |\n' +
    '| BOS-501 — Add the widget | 🚢 merged | https://github.com/o/r/pull/1 | shipped clean |\n' +
    '| BOS-502 — Fix the thing | 🔨 building | — | — |\n' +
    '| BOS-503 — Investigate flake | ❌ failed | https://github.com/o/r/pull/3 | — |\n' +
    '\n' +
    'Legend: ⏳ pending · 🔨 building · ✅ green · 🚢 merged · ❌ failed · ⏭️ skipped\n' +
    '\n' +
    'Updated: 2026-07-25T00:00:00Z\n'
  assert.equal(renderProgressComment(s), expected)
})

test('renderProgressComment: byte-stable — two renders of the same state match, and the only ISO-8601-shaped substring is the caller-supplied updatedAt', () => {
  // The two-render comparison alone is near-tautological for a pure function
  // (even a smuggled Date.now()/new Date() would return the same value
  // across two calls made back-to-back). What actually pins byte-stability
  // is that nothing in the output is a live-generated timestamp: render with
  // a sentinel updatedAt and assert it is the ONLY ISO-8601-shaped substring
  // anywhere in the output — if renderProgressComment ever called
  // Date.now()/new Date() internally, a second, different timestamp would
  // show up and this would catch it even though the two renders would still
  // (deceptively) match each other.
  const sentinelUpdatedAt = '2031-02-03T04:05:06.789Z'
  const a = state({
    updatedAt: sentinelUpdatedAt,
    tickets: [ticket({ id: 'BOS-1', title: 'Do the thing', status: 'green' })],
  })
  const b = state({
    updatedAt: sentinelUpdatedAt,
    tickets: [ticket({ id: 'BOS-1', title: 'Do the thing', status: 'green' })],
  })
  const renderedA = renderProgressComment(a)
  const renderedB = renderProgressComment(b)
  assert.equal(renderedA, renderedB)

  const isoTimestamp = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z/g
  const found = renderedA.match(isoTimestamp) ?? []
  assert.deepEqual(found, [sentinelUpdatedAt])
})

test('renderProgressComment: empty tickets renders the placeholder in place of the table', () => {
  const s = state({ tickets: [] })
  const expected =
    '<!-- boss-epic:progress:BOS-500 -->\n' +
    '### Epic progress: BOS-500\n' +
    '\n' +
    '_No tickets yet._\n' +
    '\n' +
    'Legend: ⏳ pending · 🔨 building · ✅ green · 🚢 merged · ❌ failed · ⏭️ skipped\n' +
    '\n' +
    'Updated: 2026-07-25T00:00:00Z\n'
  assert.equal(renderProgressComment(s), expected)
})

test('renderProgressComment: pipe and newline in a cell are sanitised', () => {
  const s = state({
    tickets: [
      ticket({
        id: 'BOS-1',
        title: 'Has a | pipe',
        status: 'pending',
        note: 'line one\r\nline two',
      }),
    ],
  })
  const rendered = renderProgressComment(s)
  assert.ok(rendered.includes('BOS-1 — Has a \\| pipe'))
  assert.ok(rendered.includes('| line one line two |'))
  assert.equal(rendered.includes('\r'), false)
})

test('renderProgressComment: a literal backslash-pipe in a cell does not become a live pipe', () => {
  // Runtime value is the two characters BACKSLASH then PIPE (a\|b), as if a
  // human had typed an already-escaped pipe into a title. sanitizeCell must
  // escape the backslash FIRST, then the pipe, so the rendered cell reads as
  // an escaped backslash followed by an escaped pipe (three backslashes then
  // a pipe) — never an escaped backslash followed by a LIVE, unescaped pipe
  // (which markdown would read as a real column delimiter and break the
  // table).
  const s = state({
    tickets: [ticket({ id: 'BOS-1', title: 'a\\|b', status: 'pending' })],
  })
  const rendered = renderProgressComment(s)
  const correctlyEscaped = 'a' + '\\'.repeat(3) + '|b' // a\\\|b
  const doubleEscapeBug = 'a' + '\\'.repeat(2) + '|b' // a\\|b (live pipe)
  assert.ok(
    rendered.includes(correctlyEscaped),
    `expected the backslash-then-pipe escaped form; got: ${rendered}`,
  )
  assert.equal(
    rendered.includes(doubleEscapeBug),
    false,
    'must not contain the double-escape bug form (escaped backslash followed by a live pipe)',
  )
})

test('renderProgressComment: the marker anchor line is present and is the first line', () => {
  const rendered = renderProgressComment(state())
  assert.equal(rendered.split('\n')[0], '<!-- boss-epic:progress:BOS-500 -->')
})

// ---------------------------------------------------------------------------
// progressMarkerAnchor — the single source of truth for the anchor wrapping
// ---------------------------------------------------------------------------

test('progressMarkerAnchor wraps a bare token into the anchor form', () => {
  assert.equal(
    progressMarkerAnchor('boss-epic:progress:BOS-500'),
    '<!-- boss-epic:progress:BOS-500 -->',
  )
})

test('progressMarkerAnchor produces exactly the first line renderProgressComment emits', () => {
  // The property that makes the render half and the match half composable:
  // if these two ever drift, a resuming driver silently stops finding its own
  // comment and starts posting duplicates.
  const s = state()
  assert.equal(renderProgressComment(s).split('\n')[0], progressMarkerAnchor(s.marker))
})

test('render -> plan round-trip: ONE bare marker value drives both halves', () => {
  // End-to-end composition, using the single calling convention: the SAME bare
  // token goes into state.marker and into the planner. Render a body, hand it
  // back as an existing comment, and the planner must resolve to `update`
  // against that comment's id — not `create` (a duplicate).
  const s = state()
  const existing = { id: 'c1', body: renderProgressComment(s), createdAt: '2026-01-01T00:00:00Z' }
  const next = renderProgressComment(state({ updatedAt: '2026-07-26T00:00:00Z' }))
  const result = planProgressCommentUpsert({ comments: [existing], marker: s.marker, body: next })
  assert.deepEqual(result, { op: 'update', commentId: 'c1', body: next })
})

test('planProgressCommentUpsert matches the whole anchor line, not the bare token in prose', () => {
  // The hazard the internal wrapping makes unreachable: a human comment that
  // merely quotes the token must NOT be treated as the progress comment.
  const s = state()
  const humanComment = {
    id: 'human-1',
    body: `I think the marker is ${s.marker} — check it?`,
    createdAt: '2026-01-01T00:00:00Z',
  }
  const result = planProgressCommentUpsert({
    comments: [humanComment],
    marker: s.marker,
    body: 'BODY',
  })
  assert.deepEqual(result, { op: 'create', body: 'BODY' })
})

// ---------------------------------------------------------------------------
// planProgressCommentUpsert
// ---------------------------------------------------------------------------

// The BARE token — the module's single calling convention, and what these
// fixtures pass as `marker` (the copyable example a driver author reads).
// ANCHOR is what the renderer writes as line 1 and what the planner wraps to
// internally before matching, so the fixtures build comment bodies from it.
const MARKER = 'boss-epic:progress:BOS-500'
const ANCHOR = progressMarkerAnchor(MARKER)

test('planProgressCommentUpsert: no comments -> create', () => {
  const result = planProgressCommentUpsert({ comments: [], marker: MARKER, body: 'BODY' })
  assert.deepEqual(result, { op: 'create', body: 'BODY' })
})

test('planProgressCommentUpsert: no matching comment -> create', () => {
  const comments = [{ id: 'c1', body: 'unrelated comment', createdAt: '2026-01-01T00:00:00Z' }]
  const result = planProgressCommentUpsert({ comments, marker: MARKER, body: 'BODY' })
  assert.deepEqual(result, { op: 'create', body: 'BODY' })
})

test('planProgressCommentUpsert: exactly one match -> update with that id, no warning', () => {
  const comments = [{ id: 'c1', body: `intro\n${ANCHOR}\nmore`, createdAt: '2026-01-01T00:00:00Z' }]
  const result = planProgressCommentUpsert({ comments, marker: MARKER, body: 'BODY' })
  assert.deepEqual(result, { op: 'update', commentId: 'c1', body: 'BODY' })
  assert.equal('warning' in result, false)
})

test('planProgressCommentUpsert: multiple matches -> newest wins plus a warning naming the count', () => {
  const comments = [
    { id: 'c1', body: ANCHOR, createdAt: '2026-01-01T00:00:00Z' },
    { id: 'c2', body: ANCHOR, createdAt: '2026-01-03T00:00:00Z' },
    { id: 'c3', body: ANCHOR, createdAt: '2026-01-02T00:00:00Z' },
  ]
  const result = planProgressCommentUpsert({ comments, marker: MARKER, body: 'BODY' })
  assert.equal(result.op, 'update')
  assert.equal(result.commentId, 'c2')
  assert.equal(
    result.warning,
    '3 comments match the progress marker; updating the newest (c2) and ignoring 2 older duplicates',
  )
})

test('planProgressCommentUpsert: unparseable/missing createdAt sorts oldest', () => {
  const comments = [
    { id: 'c1', body: ANCHOR, createdAt: 'not-a-date' },
    { id: 'c2', body: ANCHOR }, // missing createdAt
    { id: 'c3', body: ANCHOR, createdAt: '2026-01-01T00:00:00Z' },
  ]
  const result = planProgressCommentUpsert({ comments, marker: MARKER, body: 'BODY' })
  assert.equal(result.commentId, 'c3')
})

test('planProgressCommentUpsert: an exact createdAt tie resolves to the last match in input order', () => {
  const comments = [
    { id: 'c1', body: ANCHOR, createdAt: '2026-01-01T00:00:00Z' },
    { id: 'c2', body: ANCHOR, createdAt: '2026-01-01T00:00:00Z' },
  ]
  const result = planProgressCommentUpsert({ comments, marker: MARKER, body: 'BODY' })
  assert.equal(result.commentId, 'c2')
})

test('planProgressCommentUpsert: when every matching comment has an unparseable createdAt, the last one in input order wins', () => {
  // parseCreatedAtForSort maps every unreadable timestamp to -Infinity, so
  // when NO match has a parseable createdAt the >= tie-break loop degrades
  // to last-in-input-order for every comparison, not just a genuine tie
  // between two real dates. That path is otherwise unexercised — every other
  // test here includes at least one parseable createdAt.
  const comments = [
    { id: 'c1', body: ANCHOR, createdAt: 'not-a-date' },
    { id: 'c2', body: ANCHOR }, // missing createdAt
    { id: 'c3', body: ANCHOR, createdAt: 'also-not-a-date' },
  ]
  const result = planProgressCommentUpsert({ comments, marker: MARKER, body: 'BODY' })
  assert.equal(result.commentId, 'c3')
})

test('planProgressCommentUpsert: comments undefined -> create', () => {
  const result = planProgressCommentUpsert({ marker: MARKER, body: 'BODY' })
  assert.deepEqual(result, { op: 'create', body: 'BODY' })
})

test('planProgressCommentUpsert: body is passed through verbatim', () => {
  const body = 'exact bytes to pass through'
  const result = planProgressCommentUpsert({ comments: [], marker: MARKER, body })
  assert.equal(result.body, body)
})

test('planProgressCommentUpsert: blank marker throws TypeError', () => {
  assert.throws(
    () => planProgressCommentUpsert({ comments: [], marker: '', body: 'BODY' }),
    TypeError,
  )
  assert.throws(
    () => planProgressCommentUpsert({ comments: [], marker: '   ', body: 'BODY' }),
    TypeError,
  )
})

test('planProgressCommentUpsert: absent marker throws TypeError', () => {
  assert.throws(() => planProgressCommentUpsert({ comments: [], body: 'BODY' }), TypeError)
})

test('planProgressCommentUpsert: an already-wrapped marker throws rather than matching nothing', () => {
  // The planner wraps internally, so a pre-wrapped marker would be wrapped
  // twice, match nothing, and post a duplicate on every run. Same rule
  // validateProgressState enforces on ProgressState.marker.
  for (const bad of [ANCHOR, '<!-- token', 'token -->']) {
    assert.throws(
      () => planProgressCommentUpsert({ comments: [], marker: bad, body: 'BODY' }),
      /must be the BARE token/,
      `expected a throw for marker = ${JSON.stringify(bad)}`,
    )
  }
})

test('planProgressCommentUpsert: a blank/absent body throws TypeError', () => {
  // Same failure class as the blank marker, reached from the other side: an
  // update carrying '' erases the existing comment INCLUDING its anchor line,
  // so the next run matches nothing and posts a duplicate.
  const comments = [{ id: 'c1', body: ANCHOR, createdAt: '2026-01-01T00:00:00Z' }]
  for (const body of ['', '   ', undefined, null, 42]) {
    assert.throws(
      () => planProgressCommentUpsert({ comments, marker: MARKER, body }),
      /body must be a non-empty string/,
      `expected a throw for body = ${JSON.stringify(body)}`,
    )
  }
})

test('planProgressCommentUpsert: a sole match with no usable id throws rather than returning commentId: undefined', () => {
  // The adapter-contract gap readComments is required to close. Returning
  // {op:'update', commentId: undefined} would hand an unexecutable op to the
  // driver; degrading to `create` would post a fresh duplicate every run.
  for (const bad of [{ body: ANCHOR }, { id: '', body: ANCHOR }, { id: 7, body: ANCHOR }]) {
    assert.throws(
      () => planProgressCommentUpsert({ comments: [bad], marker: MARKER, body: 'BODY' }),
      TypeError,
      `expected a throw for ${JSON.stringify(bad)}`,
    )
  }
})

test('planProgressCommentUpsert: a winning duplicate with no usable id throws too', () => {
  const comments = [
    { id: 'c1', body: ANCHOR, createdAt: '2026-01-01T00:00:00Z' },
    { body: ANCHOR, createdAt: '2026-01-03T00:00:00Z' }, // newest, but no id
  ]
  assert.throws(
    () => planProgressCommentUpsert({ comments, marker: MARKER, body: 'BODY' }),
    /no usable `id`/,
  )
})
