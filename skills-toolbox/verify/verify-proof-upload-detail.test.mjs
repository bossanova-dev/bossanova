import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { failureDetail, FAILURE_DETAIL_MAX } from './verify-failure-detail.mjs'
import { fixedSize, publishWithinLimits } from './verify-proof-fit.mjs'
import { proofMissingBossNote, proofMissingNote, proofProblems } from './verify-proof-missing.mjs'
import { uploadProofPage } from './verify-proof-publish.mjs'

const HEAD = 'd'.repeat(40)
const TARGET = { repo: 'acme/widgets', pr: 7, headSha: HEAD }
const TOKEN = `ghp_${'A1'.repeat(20)}`
const PASSWORD = 'hunter2-correct-horse'
const SIGNATURE = 'q'.repeat(24)
const LEAKY = `PUT https://bot:${PASSWORD}@bucket.example/proof/index.html?token=${SIGNATURE} as ${TOKEN}`

/** A detail is one bounded line that carries none of the fixture's secrets. */
function assertSafe(detail) {
  assert.equal(typeof detail, 'string')
  assert.ok(detail.length > 0 && detail.length <= FAILURE_DETAIL_MAX, `bounded: ${detail}`)
  assert.doesNotMatch(detail, /[\n\r`]/)
  for (const secret of [TOKEN, PASSWORD, SIGNATURE])
    assert.ok(!detail.includes(secret), `no secret in: ${detail}`)
}

const daemonPage = (t, overrides) =>
  uploadProofPage({
    publisher: { kind: 'daemon' },
    html: '<p>page</p>',
    target: TARGET,
    workspaceRoot: tempDir('detail-workspace-', t),
    env: { BOSS_SOCKET: '/daemon.sock' },
    callDaemon: async () => ({ keyPrefix: `acme/widgets/7/${HEAD}/`, publicBaseUrl: 'https://p' }),
    ...overrides,
  })

test('a staging failure is upload-failed with a detail naming the filesystem error', async (t) => {
  const blocker = path.join(tempDir('detail-blocker-', t), 'workspace')
  fs.writeFileSync(blocker, 'a file where the workspace should be')
  const calls = []
  const unstageable = await daemonPage(t, {
    workspaceRoot: blocker,
    callDaemon: async () => calls.push('called'),
  })
  assert.equal(unstageable.ok, false)
  assert.equal(unstageable.reason, 'upload-failed')
  assertSafe(unstageable.detail)
  assert.match(unstageable.detail, /^ENOTDIR\b/)
  assert.deepEqual(calls, [], 'nothing is sent once staging fails')

  const missing = path.join(tempDir('detail-assets-', t), `${TOKEN}.png`)
  const lost = await daemonPage(t, { assets: [{ name: 'media/still.png', from: missing }] })
  assert.equal(lost.reason, 'upload-failed')
  assertSafe(lost.detail)
  assert.match(lost.detail, /^ENOENT: no such file or directory\b/)
})

test('a daemon failure keeps its reason code and carries the gRPC code and failing stage', async (t) => {
  const unavailable = Object.assign(new Error('UploadEvidence → HTTP 503'), {
    code: 'unavailable',
    reason: 'object-upload-failed',
  })
  const failed = await daemonPage(t, {
    callDaemon: async () => {
      throw unavailable
    },
  })
  assert.deepEqual(failed, {
    ok: false,
    reason: 'upload-failed',
    detail: 'unavailable (object-upload-failed): UploadEvidence → HTTP 503',
  })

  const leaky = await daemonPage(t, {
    callDaemon: async () => {
      throw Object.assign(new Error(LEAKY), { code: 'unavailable' })
    },
  })
  assert.equal(leaky.reason, 'upload-failed')
  assertSafe(leaky.detail)
  assert.match(leaky.detail, /^unavailable: PUT https:\/\/bucket\.example\b/)

  const noToken = Object.assign(
    new Error("ENOENT: no such file or directory, open '/x/bossd.token'"),
    {
      code: 'ENOENT',
    },
  )
  const unreachable = await daemonPage(t, {
    callDaemon: async () => {
      throw noToken
    },
  })
  assert.equal(unreachable.reason, 'daemon-unavailable')
  assert.match(unreachable.detail, /^ENOENT: .*bossd\.token/)

  const refused = await daemonPage(t, {
    callDaemon: async () => {
      throw Object.assign(new Error('pages off'), { code: 'failed_precondition' })
    },
  })
  assert.deepEqual(
    refused,
    { ok: false, reason: 'page-refused' },
    'a named refusal needs no detail',
  )
})

const OVER = { ok: false, reason: 'over-limit', limit: { kind: 'page-bytes', max: 10_000 } }
const MEDIA = {
  schema: 'proof-media/1',
  head: HEAD,
  transcripts: [],
  clips: [
    {
      id: 'a',
      side: 'head',
      title: 'Clip a',
      file: 'a.mp4',
      durationMs: 5_000,
      chapters: [],
      stills: [],
    },
  ],
}
const fit = (overrides) =>
  publishWithinLimits({
    upload: async () => OVER,
    render: () => '<p>fitted</p>',
    scan: () => false,
    html: '<p>page</p>',
    media: MEDIA,
    sizeOf: () => 100,
    head: HEAD,
    fixedFor: (html) => fixedSize(html, [{ text: 'player' }]),
    ...overrides,
  })

test('a fit, render or measure failure is upload-failed with a detail naming the cause', async () => {
  const rendered = await fit({
    render: () => {
      throw new TypeError(`cannot render ${LEAKY}`)
    },
  })
  assert.equal(rendered.uploaded.reason, 'upload-failed')
  assertSafe(rendered.uploaded.detail)
  assert.match(rendered.uploaded.detail, /^TypeError: cannot render PUT https:\/\/bucket\.example/)

  const measured = await fit({
    sizeOf: () => {
      throw Object.assign(new Error("EACCES: permission denied, stat '/media/a.mp4'"), {
        code: 'EACCES',
      })
    },
  })
  assert.equal(measured.uploaded.reason, 'upload-failed')
  assert.equal(measured.uploaded.detail, "EACCES: permission denied, stat '/media/a.mp4'")
})

test('the proof-missing comment and boss note show the detail, sanitised and bounded', () => {
  const problems = proofProblems({
    recorded: true,
    page: { published: false, reason: 'upload-failed', detail: 'EACCES: permission denied' },
  })
  assert.deepEqual(problems, [
    { part: 'page', reason: 'upload-failed', detail: 'EACCES: permission denied' },
  ])
  const note = proofMissingNote({ headSha: HEAD, pr: 7, repo: 'acme/widgets', problems })
  assert.match(note.body, /^- `page`: `upload-failed` \(`EACCES: permission denied`\)$/m)
  assert.equal(
    proofMissingBossNote({ pr: 7, headSha: HEAD, problems }),
    'PR #7 (dddddddddddd) review page incomplete: page: upload-failed (EACCES: permission denied)',
  )

  const raw = `first line\n\`${LEAKY}\` ${'x'.repeat(400)}`
  const [hostile] = proofProblems({
    page: { published: false, reason: 'upload-failed', detail: raw },
  })
  assertSafe(hostile.detail)
  const line = proofMissingNote({ headSha: HEAD, pr: 7, problems: [hostile] })
    .body.split('\n')
    .find((text) => text.startsWith('- `page`'))
  assert.ok(line && line.endsWith('`)') && line.length < FAILURE_DETAIL_MAX + 40, line)
  assert.ok(!line.includes(TOKEN) && !line.includes(PASSWORD))
})

test('failureDetail describes any thrown value without echoing a secret', () => {
  assert.equal(failureDetail(new RangeError('boom')), 'RangeError: boom')
  assert.equal(
    failureDetail(Object.assign(new Error('nope'), { code: 'unavailable' })),
    'unavailable: nope',
  )
  assert.equal(failureDetail(null), 'null')
  assert.equal(failureDetail('plain words'), 'plain words')
  assertSafe(failureDetail(new Error(LEAKY)))
  assert.ok(
    !failureDetail(new Error('the value supersecretvalue leaked'), {
      MY_TOKEN: 'supersecretvalue',
    }).includes('supersecretvalue'),
  )
})
