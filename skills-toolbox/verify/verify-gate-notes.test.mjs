import { test } from 'node:test'
import assert from 'node:assert/strict'
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  post,
  main,
  merge,
  decideVerdict,
  aggregateExtensions,
  readOperatorMarkers,
} from './verify-gate.mjs'
import { buildBlockers } from './verify-blockers.mjs'

const HEAD = 'a'.repeat(40)
const OTHER = 'b'.repeat(40)
const URL = 'https://github.com/o/r/pull/7'
const finding = {
  severity: 'P3',
  verified: true,
  introduced: true,
  fixed: false,
  title: 'Polish naming',
  file: 'src/a.mjs',
  line: 2,
  evidence: 'Readable but inconsistent',
}
const inputs = [
  finding,
  { ...finding, severity: 'style' },
  { ...finding, severity: 'P1', category: 'style' },
]
function inputFile(value) {
  const file = join(tempDir('verify-notes-input-'), 'input.json')
  writeFileSync(file, JSON.stringify(value))
  return file
}
function world({ comments = [], failNotes = false, failPermission = false, status = null } = {}) {
  const writes = []
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const read = (command, args) => {
    if (command === 'boss' && args[0] === 'ls') return ok({ sessions: [{ id: 's1', pr_url: URL }] })
    if (command === 'boss' && args[0] === 'show')
      return ok({ session: { repo_can_auto_repair: true, last_repair: null } })
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view')
      return ok({
        headRefOid: HEAD,
        state: 'OPEN',
        isDraft: false,
        mergeable: 'MERGEABLE',
        mergeStateStatus: 'CLEAN',
        title: 'Notes',
        body: 'Linear issue: https://linear.app/acme/issue/TASK-1/title',
        url: URL,
        baseRefName: 'main',
        statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'SUCCESS' }],
      })
    if (command === 'gh' && args[0] === 'api') {
      const path = args[1]
      if (path === 'graphql')
        return ok({ data: { repository: { pullRequest: { timelineItems: { nodes: [] } } } } })
      if (path.includes('/actions/runs?'))
        return ok([
          {
            workflow_runs: [
              { name: 'CI', status: 'completed', conclusion: 'success', head_sha: HEAD },
            ],
          },
        ])
      if (path === 'user') return ok({ login: 'verify' })
      if (path === 'repos/o/r') return ok({ owner: { login: 'o', type: 'Organization' } })
      // An unread base tip leaves the park's review request to the owner.
      if (path === 'repos/o/r/commits/main') return { ok: false, exit: 1, output: '' }
      if (path.includes('/commits/') && path.includes('/statuses'))
        return ok([status ? [status] : []])
      if (path.includes('/comments?')) return ok([comments])
      if (path.includes('/permission'))
        return failPermission ? { ok: false, exit: 1 } : ok({ permission: 'write' })
      if (path === '--method') {
        const fields = Object.fromEntries(
          args
            .filter((arg) => /^(body|state|description)=/.test(arg))
            .map((arg) => {
              const at = arg.indexOf('=')
              return [arg.slice(0, at), arg.slice(at + 1)]
            }),
        )
        writes.push({ method: args[2], path: args[3], ...fields })
        if (failNotes && fields.body?.includes('boss-verify-notes')) return { ok: false, exit: 1 }
        return ok({ html_url: URL + '#issuecomment-99' })
      }
    }
    throw new Error(`Unexpected read: ${command} ${args.join(' ')}`)
  }
  return {
    writes,
    deps: {
      read,
      now: () => Date.UTC(2026, 9, 2),
      loadConfig: () => ({}),
      adapter: { applyIssueWrites: async () => ({ outcome: 'ok', issue: { labels: [] } }) },
    },
  }
}
const options = { pr: 7, head: HEAD, repo: 'o/r' }
const status = {
  context: 'boss/verify',
  state: 'success',
  description: 'verified',
  updated_at: '2026-10-01T00:00:00Z',
}

test('notes are rejected for claim and wait and require an array', async () => {
  for (const verdict of ['claim', 'wait'])
    await assert.rejects(
      post({ ...options, verdict, reason: 'waiting', notes: inputFile([finding]) }, world().deps),
      /notes/,
    )
  await assert.rejects(
    post({ ...options, verdict: 'pass', notes: inputFile({ bad: true }) }, world().deps),
    /notes/,
  )
})
test('each verdict appends cosmetic notes after its own writes', async () => {
  for (const verdict of ['pass', 'blocked', 'defect', 'human']) {
    const w = world()
    const blockers = {
      version: 1,
      pr: 7,
      head: HEAD,
      blockers: buildBlockers({ reviewFindings: [{ ...finding, severity: 'P2' }] }),
    }
    const result = await main(
      [
        'post',
        '--pr',
        '7',
        '--head',
        HEAD,
        '--repo',
        'o/r',
        '--verdict',
        verdict,
        '--notes',
        inputFile([finding]),
        ...(verdict === 'blocked' ? ['--blockers', inputFile(blockers)] : []),
        ...(verdict === 'defect'
          ? ['--findings', inputFile([{ ...finding, severity: 'P2' }])]
          : []),
        ...(verdict === 'human' ? ['--reason', 'partial-build'] : []),
      ],
      w.deps,
    )
    const baseline = await post(
      {
        ...options,
        verdict,
        ...(verdict === 'blocked' ? { blockers: inputFile(blockers) } : {}),
        ...(verdict === 'defect' ? { findings: inputFile([{ ...finding, severity: 'P2' }]) } : {}),
        ...(verdict === 'human' ? { reason: 'partial-build' } : {}),
      },
      world().deps,
    )
    assert.deepEqual(result.writes.slice(0, -1), baseline.writes)
    assert.equal(result.posted, true, JSON.stringify(result))
    assert.equal(result.notes, 'posted')
    assert.match(w.writes.at(-1).body, /boss-verify-notes/)
    assert.equal(w.writes.filter((x) => x.body?.includes('boss-verify-notes')).length, 1)
    assert.ok(w.writes.findIndex((x) => x.state) < w.writes.length - 1)
    if (verdict === 'pass') assert.equal(w.writes.find((x) => x.state).description, 'verified')
    if (verdict === 'blocked')
      assert.equal(w.writes.find((x) => x.state).description, 'blocked: 1 P2')
  }
})
test('a failed notes write preserves successful verdict, and an empty array writes nothing extra', async () => {
  const w = world({ failNotes: true })
  const result = await post({ ...options, verdict: 'pass', notes: inputFile([finding]) }, w.deps)
  assert.equal(result.posted, true, JSON.stringify(result))
  assert.equal(result.notes, 'failed')
  assert.equal(w.writes[0].state, 'success')
  assert.equal(w.writes[0].description, 'verified')
  const empty = world()
  assert.equal(
    (await post({ ...options, verdict: 'pass', notes: inputFile([]) }, empty.deps)).notes,
    undefined,
  )
  assert.equal(empty.writes.length, 1)
})
test('note severities never hold judge or post and remain visible', async () => {
  for (const note of inputs) {
    const extensions = [{ name: 'proof' }]
    const rows = [
      {
        extension: 'proof',
        result: {
          ok: true,
          extension: 'proof',
          role: 'verify',
          verdict: 'fail',
          findings: [note],
          evidence: [],
        },
      },
    ]
    const agg = aggregateExtensions(rows, extensions)
    assert.equal(agg.verdict, 'pass')
    const judged = decideVerdict({
      prState: 'OPEN',
      isDraft: false,
      headSha: HEAD,
      receipt: 'success',
      ci: { settled: true, green: true },
      extensions: [],
      reviewFindings: [note],
    })
    assert.equal(judged.verdict, 'pass')
    assert.equal(judged.findingNotes.length, 1)
    const w = world()
    const result = await post({ ...options, verdict: 'pass', notes: inputFile([note]) }, w.deps)
    assert.equal(result.verdict, 'pass')
    assert.equal(w.writes[0].description, 'verified')
  }
})
test('repeat notes are current; older head tickets carry forward only from the verify account', async () => {
  const { buildNotes, notesComment, parseNotesTrailer } = await import('./verify-notes.mjs')
  const notes = buildNotes([finding])
  const tickets = { [notes[0].id]: { identifier: 'TASK-7', url: 'https://tracker.example/task/7' } }
  const comment = (head, login, id, linked = tickets) => ({
    id,
    user: { login },
    author_association: 'OWNER',
    created_at: '2026-10-01T00:00:00Z',
    body: notesComment({ head, notes, tickets: linked }).body,
  })
  const current = world({ comments: [comment(HEAD, 'verify', 1)], status })
  const result = await post(
    { ...options, verdict: 'pass', notes: inputFile([finding]) },
    current.deps,
  )
  assert.equal(result.notes, 'current')
  assert.equal(current.writes.length, 0)
  const changed = world({ comments: [comment(HEAD, 'verify', 1)], status })
  assert.equal(
    (
      await post(
        {
          ...options,
          verdict: 'pass',
          notes: inputFile([{ ...finding, evidence: 'new evidence' }]),
        },
        changed.deps,
      )
    ).notes,
    'updated',
  )
  assert.equal(changed.writes.at(-1).method, 'PATCH')
  const carried = world({
    comments: [comment(OTHER, 'verify', 1), comment(HEAD, 'someone', 2, {})],
    failPermission: true,
  })
  assert.equal(
    (await post({ ...options, verdict: 'pass', notes: inputFile([finding]) }, carried.deps)).notes,
    'posted',
  )
  assert.equal(carried.writes.at(-1).method, 'POST')
  assert.deepEqual(parseNotesTrailer(carried.writes.at(-1).body).tickets, tickets)
  const foreign = world({ comments: [comment(OTHER, 'someone', 2)] })
  await post({ ...options, verdict: 'pass', notes: inputFile([finding]) }, foreign.deps)
  assert.deepEqual(parseNotesTrailer(foreign.writes.at(-1).body).tickets, {})
})

test('notes comments never change merge eligibility', async () => {
  const { buildNotes, notesComment } = await import('./verify-notes.mjs')
  for (const note of inputs) {
    const comments = [
      {
        id: 1,
        user: { login: 'verify' },
        author_association: 'OWNER',
        created_at: '2026-10-01T00:00:00Z',
        body: notesComment({ head: HEAD, notes: buildNotes([note]), tickets: {} }).body,
      },
    ]
    const w = world({ comments, status })
    const result = await merge({ ...options, dryRun: true }, w.deps)
    assert.equal(result.planned, true, JSON.stringify(result))
    assert.equal(
      result.writes.some((x) => x.kind === 'merge'),
      true,
    )
  }
})

test('cosmetic notes never require permission reads, but operator approvals still do', async () => {
  const { buildNotes, notesComment } = await import('./verify-notes.mjs')
  for (const head of [HEAD, OTHER]) {
    const comment = {
      id: 1,
      user: { login: 'other-author' },
      author_association: 'OWNER',
      created_at: '2026-10-01T00:00:00Z',
      body: notesComment({ head, notes: buildNotes([finding]), tickets: {} }).body,
    }
    const w = world({ comments: [comment], status, failPermission: true })
    const result = await merge({ ...options, dryRun: true }, w.deps)
    assert.equal(result.planned, true, JSON.stringify(result))
    assert.equal(
      result.writes.some((x) => x.kind === 'merge'),
      true,
    )
    const approval = world({
      comments: [{ ...comment, body: `<!-- boss-approve head=${HEAD} -->` }],
      failPermission: true,
    })
    assert.deepEqual(readOperatorMarkers(approval.deps.read, 'o/r', 7), { readable: false })
  }
})
