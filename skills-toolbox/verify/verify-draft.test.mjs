import assert from 'node:assert/strict'
import { test } from 'node:test'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  main,
  draftMarker,
  hasDraftMarker,
  withDraftMarker,
  fileRecovery,
} from './verify-draft.mjs'
import { encodeOutcomesTrailer } from './verify-blockers.mjs'

const head = 'a'.repeat(40)
const marker = `boss-verify draft ${head}`
const headMarker = `boss-verify head ${head}`
function world(overrides = {}) {
  const values = {
    pr: { state: 'OPEN', isDraft: true, headRefOid: 'b'.repeat(40) },
    statuses: [{ context: 'boss/verify', state: 'failure', description: 'defect: 1 finding(s)' }],
    comments: [[{ body: `${marker}\n${headMarker}`, user: { login: 'writer' } }]],
    permission: { permission: 'write' },
    ...overrides,
  }
  const calls = []
  const read = (command, args) => {
    calls.push(args)
    if (command === 'git')
      return {
        ok: args[0] !== 'merge-base' || values.ancestor !== false,
        output: values.localHead ?? 'b'.repeat(40),
      }
    let payload
    if (args[0] === 'pr' && args[1] === 'ready') return { ok: values.ready !== false }
    if (args[0] === 'pr') payload = values.pr
    else if (args[1] === 'graphql') payload = values.timeline
    else if (args[1].includes('/statuses'))
      payload = values.statusBySha?.[args[1].match(/commits\/([a-f0-9]{40})/)[1]] ?? values.statuses
    else if (args[1].includes('/comments?')) payload = values.comments
    else if (args[1].includes('/permission'))
      payload =
        values.permissionByLogin?.[decodeURIComponent(args[1].split('/').at(-2))] ??
        values.permission
    else throw new Error(`Unexpected ${command}: ${args}`)
    return payload === null
      ? { ok: false, output: '' }
      : { ok: true, payload, output: JSON.stringify(payload) }
  }
  let saved = null
  const recovery = {
    load: () => saved,
    save: (value) => {
      saved = value
    },
    clear: () => {
      saved = null
    },
  }
  return { read, calls, values, recovery, pause: async () => {} }
}
const args = ['ready', '--pr', '7', '--repo', 'o/r', '--start-sha', head]

test('draft markers are whole trimmed lines and preserve the head and blocker trailer', () => {
  assert.equal(draftMarker(head.toUpperCase()), marker)
  const body = `Findings\n${headMarker}\n<!-- blockers -->`
  assert.equal(withDraftMarker(body, head), `Findings\n${marker}\n${headMarker}\n<!-- blockers -->`)
  assert.equal(hasDraftMarker(` \t${marker} \n`, head), true)
  assert.equal(hasDraftMarker(`prefix ${marker}`, head), false)
  assert.equal(hasDraftMarker(`${marker} extra`, head), false)
})

test('ready accepts only a writer marker and newest unparked send-back for the start head', async () => {
  for (const description of ['defect: 1 finding(s)', 'blocked: 1 P1']) {
    const w = world({
      statuses: [
        {
          context: 'boss/verify',
          state: 'success',
          description: 'verified',
          updated_at: '2026-10-01T00:00:00Z',
        },
        {
          context: 'boss/verify',
          state: 'failure',
          description,
          updated_at: '2026-10-02T00:00:00Z',
        },
      ],
    })
    assert.deepEqual(await main(args, w), { outcome: 'readied' })
    assert.deepEqual(w.calls.at(-1), ['pr', 'ready', '7', '--repo', 'o/r'])
  }
})

test('ready preserves human drafts, stale or untrusted markers, parks and non-drafts', async () => {
  for (const [overrides, outcome, reason] of [
    [{ pr: { state: 'OPEN', isDraft: false } }, 'not-draft'],
    [{ pr: { state: 'CLOSED', isDraft: true } }, 'not-verify-drafted', 'no-send-back'],
    [{ statuses: [] }, 'not-verify-drafted', 'no-send-back'],
    [
      { statuses: [{ context: 'boss/verify', state: 'success', description: 'defect: 1' }] },
      'not-verify-drafted',
      'no-send-back',
    ],
    [
      {
        statuses: [
          { context: 'boss/verify', state: 'failure', description: 'blocked: 1 P1, needs human' },
        ],
      },
      'not-verify-drafted',
      'parked',
    ],
    [
      { comments: [[{ body: headMarker, user: { login: 'writer' } }]] },
      'not-verify-drafted',
      'no-marker',
    ],
    [
      {
        comments: [
          [
            {
              body: `${marker}\n${`boss-verify head ${'b'.repeat(40)}`}`,
              user: { login: 'writer' },
            },
          ],
        ],
      },
      'not-verify-drafted',
      'no-marker',
    ],
    [{ permission: { permission: 'read' } }, 'not-verify-drafted', 'no-marker'],
  ]) {
    const w = world(overrides)
    assert.deepEqual(await main(args, w), { outcome, ...(reason ? { reason } : {}) })
    assert.equal(
      w.calls.some((c) => c[1] === 'ready'),
      false,
    )
  }
})

// The #4093 shape: verify sent back `sendBack` and drafted the PR; a repair pushed `repaired`
// without readying it and posted outcomes; a later repair starts from `repaired`, which has no
// verify status, and pushes `pushed`.
const sendBack = head
const repaired = 'c'.repeat(40)
const pushed = 'd'.repeat(40)
const outcomes = (from, to, login = 'writer') => ({
  body: `### Repair blocker outcomes\n\n${encodeOutcomesTrailer({
    version: 1,
    blockersHead: from,
    pushedHead: to,
    outcomes: [{ id: 'B1', outcome: 'fixed', commit: to }],
  })}`,
  user: { login },
  created_at: '2026-11-06T11:36:19Z',
})
const event = (__typename, createdAt) => ({ __typename, createdAt })
const timeline = (...nodes) => ({
  data: { repository: { pullRequest: { timelineItems: { nodes } } } },
})
function chainWorld(overrides = {}) {
  return world({
    pr: { state: 'OPEN', isDraft: true, headRefOid: pushed },
    localHead: pushed,
    statusBySha: {
      [repaired]: [],
      [sendBack]: [{ context: 'boss/verify', state: 'failure', description: 'blocked: 4 P2' }],
    },
    comments: [
      [
        {
          body: `### Verification found 4 blocker(s)\n${marker}\n${headMarker}`,
          user: { login: 'writer' },
          created_at: '2026-11-06T09:25:43Z',
        },
        outcomes(sendBack, repaired),
      ],
    ],
    timeline: timeline(
      event('ReadyForReviewEvent', '2026-11-06T08:42:43Z'),
      event('ConvertToDraftEvent', '2026-11-06T09:25:45Z'),
    ),
    ...overrides,
  })
}
const chainArgs = [...args.slice(0, -1), repaired]

test('ready owns verify draft through writer repair outcomes from a send-back head', async () => {
  const w = chainWorld()
  assert.deepEqual(await main(chainArgs, w), { outcome: 'readied' })
  assert.deepEqual(w.calls.at(-1), ['pr', 'ready', '7', '--repo', 'o/r'])

  const hop = 'e'.repeat(40)
  const twoHops = chainWorld({
    statusBySha: { ...w.values.statusBySha, [hop]: [] },
    comments: [[...w.values.comments[0], outcomes(repaired, hop)]],
  })
  assert.deepEqual(await main([...args.slice(0, -1), hop], twoHops), { outcome: 'readied' })
})

test('ready keeps a chained draft a person re-drafted or an untrusted hop links', async () => {
  for (const [overrides, expected] of [
    [
      {
        timeline: timeline(
          event('ReadyForReviewEvent', '2026-11-06T12:00:00Z'),
          event('ConvertToDraftEvent', '2026-11-06T12:05:00Z'),
        ),
      },
      { outcome: 'not-verify-drafted', reason: 'redrafted' },
    ],
    [
      { permissionByLogin: { outsider: { permission: 'read' } } },
      { outcome: 'not-verify-drafted', reason: 'no-send-back' },
    ],
    [{ timeline: null }, { outcome: 'unreadable' }],
  ]) {
    const w = chainWorld(overrides)
    if (overrides.permissionByLogin)
      w.values.comments = [[w.values.comments[0][0], outcomes(sendBack, repaired, 'outsider')]]
    assert.deepEqual(await main(chainArgs, w), expected)
    assert.equal(
      w.calls.some((c) => c[1] === 'ready'),
      false,
    )
  }
})

test('ready fails closed for every unreadable input and a failed mutation', async () => {
  for (const overrides of [
    { pr: null },
    { pr: {} },
    { statuses: null },
    { comments: null },
    { comments: {} },
    { permission: null },
  ]) {
    const w = world(overrides)
    assert.deepEqual(await main(args, w), { outcome: 'unreadable' })
    assert.equal(
      w.calls.some((c) => c[1] === 'ready'),
      false,
    )
  }
  assert.deepEqual(await main(args, world({ ready: false })), { outcome: 'failed' })
})

test('ready dry-run reads all evidence and plans without a write', async () => {
  const w = world()
  assert.deepEqual(await main([...args, '--dry-run'], w), { outcome: 'planned' })
  assert.equal(
    w.calls.some((c) => c[1] === 'ready'),
    false,
  )
  assert.equal(w.recovery.load(), null)
})

test('recovery checkpoints survive new readers, clear after success and reject damaged data', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'verify-draft-recovery-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const path = join(dir, 'nested', 'recovery.json')
  assert.equal(fileRecovery(path).load(), null)
  const record = { repo: 'o/r', pr: 7, startSha: head, headSha: 'b'.repeat(40) }
  fileRecovery(path).save(record)
  assert.deepEqual(fileRecovery(path).load(), record)
  fileRecovery(path).clear()
  assert.equal(fileRecovery(path).load(), null)
  fileRecovery(path).clear()
  writeFileSync(path, '{')
  assert.throws(() => fileRecovery(path).load())
})

test('ready retries transient mutations with the original ownership head', async () => {
  const w = world({ ready: false })
  w.pause = async () => {
    w.values.ready = true
  }
  assert.deepEqual(await main(args, w), { outcome: 'readied' })
  assert.equal(w.calls.filter((c) => c[1] === 'ready').length, 2)
  assert.equal(w.recovery.load(), null)
})

test('later repair recovers the original draft ownership after failed ready attempts', async () => {
  const w = world({ ready: false })
  assert.deepEqual(await main(args, w), { outcome: 'failed' })
  assert.equal(w.recovery.load().startSha, head)
  w.values.ready = true
  w.calls.length = 0
  assert.deepEqual(await main([...args.slice(0, -1), 'b'.repeat(40)], w), { outcome: 'readied' })
  assert.ok(w.calls.some((c) => c[1]?.includes(`/commits/${head}/statuses`)))
  assert.equal(w.recovery.load(), null)
})

test('recovery still rejects revoked writer permission and leaves personal drafts untouched', async () => {
  const w = world({ ready: false })
  await main(args, w)
  w.values.permission = { permission: 'read' }
  w.values.ready = true
  w.calls.length = 0
  assert.deepEqual(await main([...args.slice(0, -1), 'b'.repeat(40)], w), {
    outcome: 'not-verify-drafted',
    reason: 'no-marker',
  })
  assert.equal(
    w.calls.some((c) => c[1] === 'ready'),
    false,
  )
  assert.equal(w.recovery.load(), null)
})

test('recovery survives unreadable PR reads and a later repair push', async () => {
  const w = world({ pr: null })
  assert.deepEqual(await main(args, w), { outcome: 'unreadable' })
  w.values.localHead = 'c'.repeat(40)
  w.values.pr = { state: 'OPEN', isDraft: true, headRefOid: w.values.localHead }
  w.calls.length = 0
  assert.deepEqual(await main([...args.slice(0, -1), 'b'.repeat(40)], w), { outcome: 'readied' })
  assert.ok(w.calls.some((c) => c[1]?.includes(`/commits/${head}/statuses`)))
})

test('another failed repair push retains ownership across its own retries', async () => {
  const w = world({ ready: false })
  await main(args, w)
  w.values.localHead = 'c'.repeat(40)
  w.values.pr.headRefOid = w.values.localHead
  assert.deepEqual(await main([...args.slice(0, -1), 'b'.repeat(40)], w), { outcome: 'failed' })
  assert.equal(w.recovery.load().startSha, head)
  w.values.ready = true
  assert.deepEqual(await main([...args.slice(0, -1), 'c'.repeat(40)], w), { outcome: 'readied' })
})

test('recovery follows a captured repair head across rebase and refuses a moved PR head', async () => {
  const w = world({ ready: false })
  await main(args, w)
  w.values.localHead = 'c'.repeat(40)
  w.values.ancestor = false
  w.values.pr.headRefOid = w.values.localHead
  w.values.ready = true
  w.calls.length = 0
  assert.deepEqual(await main([...args.slice(0, -1), 'b'.repeat(40)], w), { outcome: 'readied' })
  assert.ok(w.calls.some((c) => c[1]?.includes(`/commits/${head}/statuses`)))
  const moved = world({ pr: { state: 'OPEN', isDraft: true, headRefOid: 'c'.repeat(40) } })
  assert.deepEqual(await main(args, moved), { outcome: 'unreadable' })
  assert.equal(
    moved.calls.some((c) => c[1] === 'ready'),
    false,
  )
})

test('ready rejects unknown flags, invalid SHAs and missing arguments as usage errors', async () => {
  for (const argv of [[...args, '--unknown'], [...args.slice(0, -1), 'nope'], ['ready']])
    await assert.rejects(main(argv, world()), (error) => error.exitCode === 2)
})

test('ready CLI exits 2 for usage errors before accessing GitHub', () => {
  for (const argv of [[...args, '--unknown'], [...args.slice(0, -1), 'nope'], ['ready']]) {
    const result = spawnSync(
      process.execPath,
      [fileURLToPath(new URL('./verify-draft.mjs', import.meta.url)), ...argv],
      { encoding: 'utf8' },
    )
    assert.equal(result.status, 2)
    assert.equal(result.stdout, '')
    assert.match(result.stderr, /verify-draft: ready needs/)
  }
})
