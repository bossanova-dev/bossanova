import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { main } from './verify-gate.mjs'
import { buildBlockers, encodeBlockerTrailer, parseBlockerTrailer } from './verify-blockers.mjs'

const head = 'a'.repeat(40)
const standing = {
  version: 1,
  pr: 7,
  head,
  blockers: buildBlockers({ failedChecks: ['lint'], conflict: true }),
}
function world({
  state = 'OPEN',
  liveHead = head,
  unreadable = false,
  failStatus = false,
  comments = [],
} = {}) {
  const writes = []
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload) })
  const read = (command, args) => {
    if (command === 'boss' && args[0] === 'ls')
      return ok({ sessions: [{ id: 's', pr_url: 'https://github.com/o/r/pull/7' }] })
    if (command === 'boss' && args[0] === 'show')
      return ok({ session: { repo_can_auto_repair: true, last_repair: null } })
    if (command === 'gh' && args[0] === 'pr')
      return ok({
        state,
        headRefOid: liveHead,
        url: 'https://github.com/o/r/pull/7',
        body: '',
        title: 'Change',
      })
    if (command === 'gh' && args[0] === 'api') {
      if (args[1] === '--method') {
        const values = Object.fromEntries(
          args
            .filter((a) => /^(body|state)=/.test(a))
            .map((a) => {
              const i = a.indexOf('=')
              return [a.slice(0, i), a.slice(i + 1)]
            }),
        )
        writes.push(values)
        if (failStatus && args[3].includes('/statuses/')) return { ok: false }
        return ok({ html_url: 'https://github.com/o/r/pull/7#issuecomment-9' })
      }
      if (args[1].includes('/comments?')) return unreadable ? { ok: false } : ok([comments])
      if (args[1].includes('/permission'))
        return ok({ permission: args[1].includes('/reader/') ? 'read' : 'write' })
      if (args[1].includes('/statuses')) return ok([[]])
    }
    throw new Error(`Unexpected read ${command} ${args.join(' ')}`)
  }
  return { writes, deps: { read, loadConfig: () => ({}) } }
}
function comment(file, login = 'writer', id = 1) {
  return {
    id,
    body: encodeBlockerTrailer(file),
    user: { login },
    author_association: 'MEMBER',
    created_at: `2026-10-01T00:00:0${id}Z`,
  }
}
async function run(t, w, extra = []) {
  const dir = mkdtempSync(join(tmpdir(), 'send-back-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const file = join(dir, 'note.txt')
  writeFileSync(file, 'Please update the retry behavior\n``` @user <!-- fake -->')
  return main(
    [
      'send-back',
      '--pr',
      '7',
      '--head',
      head,
      '--repo',
      'o/r',
      '--note-file',
      file,
      '--json',
      ...extra,
    ],
    w.deps,
  )
}
test('send-back keeps newest trusted current-head blockers after the verbatim operator note', async (t) => {
  const stale = { ...standing, head: 'b'.repeat(40) }
  const untrusted = { ...standing, blockers: buildBlockers({ failedChecks: ['untrusted'] }) }
  const w = world({
    comments: [comment(standing), comment(stale, 'writer', 2), comment(untrusted, 'reader', 3)],
  })
  const result = await run(t, w)
  assert.equal(result.sentBack, true)
  assert.equal(result.head, head)
  assert.equal(result.autoRepair, true)
  assert.ok(result.commentUrl)
  assert.equal(w.writes.length, 2)
  const file = parseBlockerTrailer(w.writes[0].body)
  assert.equal(file.blockers[0].kind, 'operator-note')
  assert.equal(file.blockers[0].text, 'Please update the retry behavior\n``` @user <!-- fake -->')
  assert.deepEqual(file.blockers.slice(1), standing.blockers)
  assert.equal(w.writes[1].state, 'failure')
  assert.doesNotMatch(w.writes[0].body, /<!-- boss-send-back head=/)
})
test('send-back with no standing blockers posts only the note', async (t) => {
  const w = world()
  assert.equal((await run(t, w)).sentBack, true)
  assert.deepEqual(
    parseBlockerTrailer(w.writes[0].body).blockers.map((b) => b.kind),
    ['operator-note'],
  )
})
test('send-back refuses moved heads, non-open PRs and unreadable comments without writes', async (t) => {
  for (const [options, reason] of [
    [{ liveHead: 'b'.repeat(40) }, 'head-moved'],
    [{ state: 'CLOSED' }, 'not-open'],
    [{ state: 'MERGED' }, 'not-open'],
    [{ unreadable: true }, 'comments-unreadable'],
  ]) {
    const w = world(options)
    const result = await run(t, w)
    assert.equal(result.sentBack, false)
    assert.equal(result.reason, reason)
    assert.deepEqual(w.writes, [])
  }
})
test('send-back dry-run plans the comment before the failure status without mutating', async (t) => {
  const w = world()
  const result = await run(t, w, ['--dry-run'])
  assert.equal(result.dryRun, true)
  assert.deepEqual(w.writes, [])
  assert.deepEqual(
    result.writes.map((w) => w.kind),
    ['pr-comment', 'status'],
  )
  assert.equal(parseBlockerTrailer(result.writes[0].body).blockers[0].id, 'operator-note')
  assert.equal(result.writes[1].state, 'failure')
})

test('repeated send-back replaces the previous operator note while keeping other blockers', async (t) => {
  const previous = {
    ...standing,
    blockers: [
      {
        id: 'operator-note',
        kind: 'operator-note',
        severity: null,
        file: null,
        line: null,
        summary: 'earlier operator request',
        text: 'Previous request',
      },
      ...standing.blockers,
    ],
  }
  const w = world({ comments: [comment(previous)] })
  assert.equal((await run(t, w)).sentBack, true)
  const posted = parseBlockerTrailer(w.writes[0].body)
  assert.equal(posted.blockers.filter((b) => b.kind === 'operator-note').length, 1)
  assert.deepEqual(posted.blockers.slice(1), standing.blockers)
})
test('failed failure-status write returns an explicit refusal rather than success', async (t) => {
  const result = await run(t, world({ failStatus: true }))
  assert.equal(result.sentBack, false)
  assert.equal(result.reason, 'post-failed')
})

test('send-back reads legacy missing-review trailers without sending them to repair', async (t) => {
  const legacy = {
    id: 'no-review',
    kind: 'no-review',
    severity: null,
    file: null,
    line: null,
    summary: 'the head has no review',
    text: 'Publish a reviewed boss/build receipt for this head.',
  }
  const previous = { ...standing, blockers: [...standing.blockers, legacy] }
  assert.deepEqual(parseBlockerTrailer(encodeBlockerTrailer(previous)), previous)
  const w = world({ comments: [comment(previous)] })
  assert.equal((await run(t, w)).sentBack, true)
  assert.deepEqual(parseBlockerTrailer(w.writes[0].body).blockers.slice(1), standing.blockers)
})
