import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { main, UsageError } from './verify-gate.mjs'
import { acceptMarker } from './verify-accept.mjs'
import { buildBlockers, encodeBlockerTrailer, operatorNoteBlocker } from './verify-blockers.mjs'

const head = 'a'.repeat(40)
const review = buildBlockers({
  reviewFindings: [
    {
      severity: 'P1',
      file: 'app.go',
      line: 12,
      title: 'Handle error',
      evidence: 'failure evidence',
      verified: true,
      introduced: true,
    },
  ],
})[0]
const standing = { version: 1, pr: 7, head, blockers: [review] }
const createdIssue = {
  id: 'id-12',
  identifier: 'TASK-12',
  url: 'https://example.com/issue/TASK-12',
}
const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload) })
function comment(body, login = 'writer', id = 1) {
  return {
    id,
    body,
    user: { login },
    author_association: 'MEMBER',
    created_at: `2026-10-01T00:00:0${id}Z`,
    html_url: `https://github.com/o/r/pull/7#issuecomment-${id}`,
  }
}
function world(options = {}) {
  const events = [],
    creates = [],
    writes = []
  const comments = options.comments ?? [comment(encodeBlockerTrailer(standing))]
  const read = (command, args) => {
    if (command === 'gh' && args[0] === 'pr')
      return options.prUnreadable
        ? { ok: false }
        : ok({
            state: options.state ?? 'OPEN',
            headRefOid: options.liveHead ?? head,
            url: 'https://github.com/o/r/pull/7',
            body: 'Tracker issue: https://example.com/issue/TASK-1',
          })
    if (command === 'gh' && args[0] === 'api') {
      if (args[1] === '--method') {
        const body = args.find((arg) => arg.startsWith('body='))?.slice(5)
        writes.push(body)
        events.push('comment')
        return options.failComment
          ? { ok: false }
          : ok({ html_url: 'https://github.com/o/r/pull/7#issuecomment-9' })
      }
      if (args[1].includes('/comments?'))
        return options.commentsUnreadable ? { ok: false } : ok([comments])
      if (args[1].includes('/permission'))
        return ok({ permission: args[1].includes('/reader/') ? 'read' : 'write' })
    }
    throw new Error(`Unexpected read ${command} ${args.join(' ')}`)
  }
  const adapter = options.noAdapter
    ? {}
    : {
        applyIssueCreate: async (issue) => {
          creates.push(issue)
          events.push('create')
          if (options.createThrow)
            throw Object.assign(new Error('credentials missing'), { code: 'credentials-missing' })
          return options.createResult ?? { ok: true, outcome: 'ok', issue: createdIssue }
        },
      }
  return {
    options,
    creates,
    writes,
    events,
    deps: {
      read,
      adapter,
      loadConfig: () => ({
        adapters: { tracker: 'demo' },
        trackerConfig: {
          demo: { states: { unplanned: 'Incoming' }, labels: { agentPlan: 'plan-me' } },
        },
      }),
    },
  }
}
async function run(
  t,
  w,
  {
    reason = 'Ship now, repair in a follow-up',
    blocker = review.id,
    targetHead = head,
    extra = [],
  } = {},
) {
  const dir = mkdtempSync(join(tmpdir(), 'verify-accept-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const path = join(dir, 'reason.txt')
  writeFileSync(path, reason)
  return main(
    [
      'accept',
      '--pr',
      '7',
      '--repo',
      'o/r',
      '--head',
      targetHead,
      '--blocker',
      blocker,
      '--reason-file',
      path,
      '--json',
      ...extra,
    ],
    w.deps,
  )
}

test('accept creates the queued follow-up before the pinned PR record @sessions.AC156', async (t) => {
  const w = world()
  const result = await run(t, w)
  assert.equal(result.accepted, true)
  assert.deepEqual(result.ticket, { identifier: createdIssue.identifier, url: createdIssue.url })
  assert.equal(result.commentUrl, 'https://github.com/o/r/pull/7#issuecomment-9')
  assert.deepEqual(w.events, ['create', 'comment'])
  assert.equal(w.creates[0].stateName, 'Incoming')
  assert.deepEqual(w.creates[0].labels, ['plan-me'])
  assert.equal(
    w.creates[0].marker,
    `<!-- boss-accept-followup repo=o/r pr=7 blocker=${review.id} -->`,
  )
  assert.ok(
    w.writes[0].endsWith(
      acceptMarker({ head, blocker: review.id, ticket: createdIssue.identifier }),
    ),
  )
})

test('read and eligibility refusals never write @sessions.AC157', async (t) => {
  const cases = [
    [{ prUnreadable: true }, 'pr-unreadable'],
    [{ state: 'CLOSED' }, 'not-open'],
    [{ liveHead: 'b'.repeat(40) }, 'head-moved'],
    [{ commentsUnreadable: true }, 'comments-unreadable'],
    [{ comments: [] }, 'no-blockers'],
    [{ comments: [comment(encodeBlockerTrailer(standing), 'reader')] }, 'no-blockers'],
    [{ comments: [comment(encodeBlockerTrailer({ ...standing, pr: 8 }))] }, 'no-blockers'],
    [
      {
        comments: [
          comment(
            encodeBlockerTrailer({ ...standing, blockers: buildBlockers({ conflict: true }) }),
          ),
        ],
      },
      'blocker-not-listed',
    ],
    [
      {
        comments: [
          comment(
            encodeBlockerTrailer({
              ...standing,
              blockers: [operatorNoteBlocker('Please repair'), review],
            }),
          ),
        ],
      },
      'sent-back',
    ],
    [{ noAdapter: true }, 'tracker-create-unavailable'],
  ]
  for (const [options, reason] of cases) {
    const w = world(options)
    assert.equal((await run(t, w)).reason, reason)
    assert.deepEqual(w.events, [])
  }
})

test('every non-review blocker kind is refused @sessions.AC157', async (t) => {
  for (const blocker of [
    ...buildBlockers({ failedChecks: ['unit'], conflict: true }),
    {
      id: 'no-review',
      kind: 'no-review',
      severity: null,
      file: null,
      line: null,
      summary: 'no review stands behind this head',
      text: '',
    },
    operatorNoteBlocker('Repair this'),
  ]) {
    const w = world({
      comments: [comment(encodeBlockerTrailer({ ...standing, blockers: [blocker] }))],
    })
    const result = await run(t, w, { blocker: blocker.id })
    assert.equal(result.reason, 'blocker-not-acceptable')
    assert.equal(result.detail, blocker.kind)
    assert.deepEqual(w.events, [])
  }
})

test('invalid reasons and head are usage errors before reads or writes', async (t) => {
  for (const reason of ['', 'x'.repeat(2001), 'invalid\u0000reason']) {
    const w = world()
    await assert.rejects(run(t, w, { reason }), UsageError)
    assert.deepEqual(w.events, [])
  }
  await assert.rejects(run(t, world(), { targetHead: 'bad' }), UsageError)
})

test('create absence, pre-network throws and uncertain outcomes never post acceptance', async (t) => {
  for (const [options, reason, detail] of [
    [{ noAdapter: true }, 'tracker-create-unavailable', undefined],
    [{ createThrow: true }, 'tracker-create-unavailable', 'credentials-missing'],
    [
      { createResult: { ok: false, outcome: 'failed', reason: 'denied' } },
      'tracker-create-failed',
      'denied',
    ],
    [
      { createResult: { ok: false, outcome: 'indeterminate', reason: 'timeout' } },
      'tracker-create-indeterminate',
      'timeout',
    ],
  ]) {
    const w = world(options),
      result = await run(t, w)
    assert.equal(result.reason, reason)
    assert.equal(result.detail, detail)
    assert.deepEqual(w.writes, [])
  }
})

test('failed PR post reruns reuse the created issue', async (t) => {
  const w = world({ failComment: true })
  assert.equal((await run(t, w)).reason, 'comment-failed')
  w.options.failComment = false
  w.options.createResult = { ok: true, outcome: 'ok', issue: createdIssue, existing: true }
  assert.equal((await run(t, w)).existingTicket, true)
  assert.equal(w.creates[0].marker, w.creates[1].marker)
})

test('already accepted heads write nothing, read-only acceptances do not stand', async (t) => {
  const acceptance = acceptMarker({ head, blocker: review.id, ticket: 'TASK-12' })
  const w = world({
    comments: [comment(encodeBlockerTrailer(standing)), comment(acceptance, 'writer', 2)],
  })
  assert.equal((await run(t, w)).already, true)
  assert.deepEqual(w.events, [])
  const untrusted = world({
    comments: [comment(encodeBlockerTrailer(standing)), comment(acceptance, 'reader', 2)],
  })
  assert.equal((await run(t, untrusted)).accepted, true)
  assert.deepEqual(untrusted.events, ['create', 'comment'])
})

test('later head acceptance reuses the head-independent follow-up marker', async (t) => {
  const later = 'b'.repeat(40)
  const w = world({
    liveHead: later,
    comments: [
      comment(encodeBlockerTrailer({ ...standing, head: later })),
      comment(acceptMarker({ head, blocker: review.id, ticket: 'TASK-12' }), 'writer', 2),
    ],
    createResult: { ok: true, outcome: 'ok', issue: createdIssue, existing: true },
  })
  assert.equal((await run(t, w, { targetHead: later })).existingTicket, true)
  assert.equal(
    w.creates[0].marker,
    `<!-- boss-accept-followup repo=o/r pr=7 blocker=${review.id} -->`,
  )
})

test('dry-run plans both writes but performs neither', async (t) => {
  const w = world(),
    result = await run(t, w, { extra: ['--dry-run'] })
  assert.equal(result.dryRun, true)
  assert.equal(result.accepted, false)
  assert.deepEqual(
    result.writes.map((write) => write.kind),
    ['tracker-issue', 'pr-comment'],
  )
  assert.deepEqual(w.events, [])
})
