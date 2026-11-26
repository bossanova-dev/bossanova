import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { operatorMarkers } from './verify-gate.mjs'
import { loadSkillConfig } from '../skill-config.mjs'
import {
  discoverProofPublisher,
  proofPagesFor,
  readProofPages,
  proofNote,
  recordHeadProof,
  uploadProofPage,
} from './verify-proof-publish.mjs'
import { buildProofRecord } from './verify-proof-build.mjs'
import { parseProofLine, proofLine } from './verify-proof.mjs'
import { PAGE_OBJECT } from './verify-publish-hook.mjs'

const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)

test('every recorded proof note mirrors its lead and final page to a check run', async () => {
  for (const publisher of [null, { kind: 'hook', extension: { name: 'host' } }]) {
    const { result, notes, checkRuns } = await run({
      env: { BOSS_SOCKET: '/daemon.sock' },
      overrides: { discoverPublisher: () => ({ publisher, reason: 'not-configured' }) },
    })
    assert.equal(checkRuns.length, 1)
    const page = publisher ? `https://proof.example/acme/widgets/7/${HEAD}/index.html` : ''
    assert.deepEqual(checkRuns[0], {
      socket: '/daemon.sock',
      method: 'PostProofCheckRun',
      message: {
        repo: 'acme/widgets',
        headSha: HEAD,
        title: 'Review page: moderate',
        summary: notes[0].note.body.split('\n\n<!-- boss-proof:')[0],
        detailsUrl: page,
      },
      options: { timeoutMs: 15_000 },
    })
    assert.deepEqual(result.checkRun, {
      posted: true,
      url: 'https://github.com/acme/widgets/runs/1',
      created: true,
    })
  }
})

test('non-parked verdicts mirror their one-line proof when pages is parked', async () => {
  for (const verdict of ['pass', 'blocked', 'defect']) {
    for (const outcome of ['posted', 'updated', 'current']) {
      const { result, checkRuns } = await run({
        judgement: { ...judged, verdict },
        overrides: {
          recordHeadNote: () => ({ outcome }),
          trustedTree: () => ({ ok: true, root: '/base' }),
          loadConfig: () => ({ proofDefaults: { pages: 'parked' } }),
        },
      })
      assert.equal(result.note, outcome)
      assert.equal(checkRuns.length, 1)
      assert.match(
        checkRuns[0].message.summary,
        /^Verify's review note for `aaaaaaaaaaaa` is \*\*moderate\*\*\.$/,
      )
      assert.equal(checkRuns[0].message.detailsUrl, '')
    }
  }
})

test('proof checks honor only the trusted base opt-out and default on unreadable config', async () => {
  for (const fallback of ['none', 'check-run']) {
    const { result, checkRuns } = await run({
      overrides: {
        trustedTree: ({ baseSha }) => {
          assert.equal(baseSha, BASE)
          return { ok: true, root: '/base' }
        },
        loadConfig: (root) => {
          assert.equal(root, '/base', 'the head config cannot disable its own proof')
          return { proofDefaults: { fallback } }
        },
      },
    })
    assert.equal(checkRuns.length, fallback === 'none' ? 0 : 1)
    if (fallback === 'none')
      assert.deepEqual(result.checkRun, { posted: false, reason: 'fallback-disabled' })
  }
  const unreadable = await run({
    overrides: {
      trustedTree: () => ({ ok: true, root: '/base' }),
      loadConfig: () => {
        throw new Error('config unreadable')
      },
    },
  })
  assert.equal(unreadable.checkRuns.length, 1)
})

test('proof checks skip dry runs, failed notes, unreadable comments and repository proof', async () => {
  const dry = await run({ dryRun: true })
  assert.deepEqual(dry.result.checkRun, { posted: false, reason: 'dry-run' })
  assert.equal(dry.checkRuns.length, 0)
  for (const options of [
    { readable: false },
    { markers: [{ kind: 'repo-proof', head: HEAD }] },
    { overrides: { recordHeadNote: () => ({ outcome: 'failed' }) } },
  ]) {
    const result = await run(options)
    assert.equal(result.checkRuns.length, 0)
    assert.equal(result.result.recorded, false)
  }
})

test('proof check refusals and transport failures never undo a recorded note', async () => {
  const failures = [
    ...[
      'not-signed-in',
      'repo-not-registered',
      'no-installation',
      'checks-not-granted',
      'upload-failed',
    ].map((reason) => [{ reason, code: 'failed_precondition' }, reason]),
    [{ code: 'unimplemented' }, 'daemon-unsupported'],
    [new Error('socket closed'), 'check-run-failed'],
    [null, 'check-run-failed'],
  ]
  for (const [error, reason] of failures) {
    const { result, notes } = await run({
      overrides: {
        callDaemon: async () => {
          throw error
        },
      },
    })
    assert.equal(result.recorded, true)
    assert.equal(notes.length, 1)
    assert.deepEqual(result.checkRun, { posted: false, reason })
  }
})

test('access-put is selected only by trusted base config and hooks still win @sessions.AC256201', (t) => {
  const trusted = tempDir('access-trusted-', t)
  const checkout = tempDir('access-head-', t)
  fs.writeFileSync(
    path.join(checkout, '.boss-skills.json'),
    JSON.stringify({
      publishConfig: { proof: { kind: 'access-put', baseUrl: 'https://untrusted.example' } },
    }),
  )
  const common = { baseSha: BASE, cwd: checkout, trustedTree: () => ({ ok: true, root: trusted }) }
  const proof = { kind: 'access-put', baseUrl: 'https://proof.example', keychainService: 'my-host' }
  const loadConfig = (root) => {
    assert.equal(root, trusted)
    return { publishConfig: { proof } }
  }
  assert.deepEqual(discoverProofPublisher(common).publisher, { kind: 'hosted' })
  assert.deepEqual(discoverProofPublisher({ ...common, loadConfig }).publisher, proof)
  for (const baseUrl of [
    'http://proof.example',
    'https://user:pass@proof.example',
    'https://proof.example?key=value',
    'https://proof.example#fragment',
    'not-url',
  ]) {
    assert.deepEqual(
      discoverProofPublisher({
        ...common,
        loadConfig: () => ({ publishConfig: { proof: { ...proof, baseUrl } } }),
      }),
      { publisher: null, reason: 'invalid-publish-config' },
    )
  }
  for (const kind of [undefined, 'daemon']) {
    assert.deepEqual(
      discoverProofPublisher({
        ...common,
        loadConfig: () => ({
          publishConfig: { proof: { kind, baseUrl: 'https://proof.example', bucket: 'proof' } },
        }),
      }).publisher,
      { kind: 'daemon' },
    )
  }
  hook(trusted, 'boss-verify-base-hook')
  assert.equal(discoverProofPublisher({ ...common, loadConfig }).publisher.kind, 'hook')
})

test('proof page policy defaults to all, supports parked/none opt-out, and rejects deferred faults @sessions.AC254502', (t) => {
  for (const config of [
    undefined,
    {},
    { proofDefaults: { pages: 'all' } },
    { proofDefaults: { pages: true } },
  ])
    assert.equal(proofPagesFor(config), 'all')
  assert.equal(proofPagesFor({ proofDefaults: { pages: 'parked' } }), 'parked')
  assert.equal(proofPagesFor({ proofDefaults: { pages: 'none' } }), 'none')
  const root = tempDir('proof-policy-', t)
  fs.writeFileSync(
    path.join(root, '.boss-skills.json'),
    JSON.stringify({ proofDefaults: { pages: 'parked', builtins: false } }),
  )
  const config = loadSkillConfig({ cwd: root })
  assert.throws(() => proofPagesFor(config), /proofDefaults/)
  assert.equal(
    readProofPages({
      baseSha: BASE,
      trustedTree: () => ({ ok: true, root: '/base' }),
      loadConfig: () => config,
    }),
    'all',
  )
})

test('proof page policy reads only the trusted root and falls back on unavailable inputs @sessions.AC254502', () => {
  const input = {
    baseSha: BASE,
    cwd: '/head',
    read: () => {},
    trustedTree: ({ baseSha }) => {
      assert.equal(baseSha, BASE)
      return { ok: true, root: '/base' }
    },
    loadConfig: (root) => {
      assert.equal(root, '/base')
      return { proofDefaults: { pages: 'parked' } }
    },
  }
  assert.equal(readProofPages(input), 'parked')
  for (const override of [
    { trustedTree: undefined },
    { trustedTree: () => null },
    { trustedTree: () => ({ ok: false }) },
    {
      trustedTree: () => {
        throw new Error('tree unavailable')
      },
    },
    {
      loadConfig: () => {
        throw new Error('config unavailable')
      },
    },
  ])
    assert.equal(readProofPages({ ...input, ...override }), 'all')
})
const judged = {
  verdict: 'human',
  reasons: ['human-review:api-contract'],
  label: 'moderate: API contract',
  headSha: HEAD,
  baseSha: BASE,
  confidence: {
    level: 'moderate',
    asks: [{ area: 'api-contract', label: 'API contract', source: 'builtin' }],
    reasons: [],
    loweredBy: ['area-asks'],
    health: [],
  },
  facts: {
    areas: [
      { area: 'api-contract', label: 'API contract', paths: ['api.proto'], defaultAsk: 'always' },
    ],
    receipt: { state: 'success' },
  },
}
const DIFF = 'diff --git a/api.proto b/api.proto\n+message User {}\n'

/** A recordHeadProof run over fakes: what it uploaded, what it wrote, what it returned. */
async function run({
  markers = [],
  readable = true,
  upload,
  diff = DIFF,
  env = {},
  overrides = {},
  missing = new Set(),
  judgement = judged,
  dryRun = false,
  input = {},
} = {}) {
  const uploads = []
  const notes = []
  const calls = []
  const checkRuns = []
  const read = (command, args) => {
    if (command === 'git') calls.push(args.join(' '))
    if (command === 'git' && args[0] === 'cat-file')
      return { ok: !missing.has(args.at(-1).replace('^{commit}', '')) }
    if (command === 'git' && args[0] === 'fetch') {
      missing.delete(args.at(-1))
      return { ok: true, output: '' }
    }
    if (command === 'git' && args[0] === 'diff') return { ok: true, output: diff }
    if (command === 'git' && args[0] === 'log')
      return { ok: true, output: '2026-11-10T00:00:00Z\tAdd the user message' }
    if (command === 'git' && args[0] === 'rev-parse') return { ok: true, output: '/repo' }
    throw new Error(`unexpected ${command} ${args.join(' ')}`)
  }
  const result = await recordHeadProof(
    {
      read,
      repo: 'acme/widgets',
      pr: 7,
      headSha: HEAD,
      judged: judgement,
      cwd: '/repo',
      env,
      dryRun,
      ...input,
    },
    {
      readOperatorMarkers: () => (readable ? { readable: true, markers } : { readable: false }),
      callDaemon: async (socket, method, message, options) => {
        checkRuns.push({ socket, method, message, options })
        return { htmlUrl: 'https://github.com/acme/widgets/runs/1', isCreated: true }
      },
      recordHeadNote: (_read, note) => {
        notes.push(note)
        return { outcome: 'posted', url: 'https://github.com/acme/widgets/pull/7#c1' }
      },
      discoverPublisher: () => ({ publisher: { kind: 'hook', extension: { name: 'host' } } }),
      uploadPage: async (input) => {
        uploads.push(input)
        return upload
          ? upload(input)
          : { ok: true, url: `https://proof.example/acme/widgets/7/${HEAD}/index.html` }
      },
      ...overrides,
    },
  )
  return { result, uploads, notes, calls, checkRuns }
}

test('a parked verdict uploads the page and notes the proof line carrying its URL @sessions.AC248601', async () => {
  const { result, uploads, notes } = await run()
  const url = `https://proof.example/acme/widgets/7/${HEAD}/index.html`
  assert.deepEqual(result.page, { published: true, url, publisher: 'hook' })
  assert.equal(result.recorded, true)
  assert.equal(result.level, 'moderate')
  assert.match(uploads[0].html, /^<!doctype html>/)
  assert.ok(uploads[0].html.includes('+message User {}'), 'the asking area shows its diff')
  assert.deepEqual(uploads[0].target, { repo: 'acme/widgets', pr: 7, headSha: HEAD })
  assert.equal(notes[0].kind, 'proof-note')
  assert.deepEqual(parseProofLine(notes[0].note.body), { head: HEAD, page: url, level: 'moderate' })
})

test('a page with no spec titles itself from the PR and leads with its first paragraph', async () => {
  const prView = {
    title: 'Add the user message to the API',
    body: '## Summary\n\nAdds a `User` message so clients can read **who** signed in.\n\n## Tests\n\n- none',
  }
  const { uploads } = await run({ input: { prView } })
  const header = /<header class="pr">([\s\S]*?)<\/header>/.exec(uploads[0].html)?.[1] ?? ''
  assert.match(header, /<h1>Add the user message to the API<\/h1>/)
  assert.match(
    header,
    /<p class="lead">Adds a <code>User<\/code> message so clients can read who signed in\.<\/p>/,
  )
  const untitled = await run()
  assert.ok(!untitled.uploads[0].html.includes('<p class="lead">Adds'), 'no body, no lead')
})

test('proof recording forwards injected Access transport and keychain readers', async () => {
  const fetchImpl = () => {}
  const readKeychain = () => null
  const { uploads } = await run({ overrides: { fetchImpl, readKeychain } })
  assert.equal(uploads[0].fetchImpl, fetchImpl)
  assert.equal(uploads[0].readKeychain, readKeychain)
})

test('a parked page shows what review fixed and how the ticket criteria checked out', async () => {
  const fixed = {
    severity: 'P2',
    file: 'api.proto',
    line: 1,
    title: 'Field named twice',
    evidence: 'Renamed.',
  }
  const { uploads } = await run({
    judgement: {
      ...judged,
      ticket: { id: 'BOS-9', url: 'https://linear.app/acme/issue/BOS-9' },
      reviewFixed: [fixed],
      reviewAcceptance: [
        { id: 'AC1', text: 'Adds the User message', result: 'met', check: 'buf lint', head: HEAD },
      ],
    },
  })
  const html = uploads[0].html
  assert.ok(html.includes('<h2>Does it satisfy BOS-9?'), 'the ticket section names the ticket')
  assert.ok(
    html.includes('<span class="pill success">done</span>'),
    'a passed check on this head is done',
  )
  assert.ok(html.includes('Field named twice'), 'the fixed finding shows')
  assert.ok(html.includes('<span class="pill success">fixed</span>'))
})
test("a repo's own proof for the head wins: nothing is uploaded or written @sessions.AC248603", async () => {
  const markers = [{ kind: 'repo-proof', head: HEAD }]
  const { result, uploads, notes } = await run({ markers })
  assert.deepEqual(result, { recorded: false, reason: 'repo-proof-present' })
  assert.deepEqual([uploads.length, notes.length], [0, 0])
  // Another head's proof, or the core's own note, does not stop it.
  const others = [
    { kind: 'repo-proof', head: BASE },
    { kind: 'proof-note', head: HEAD },
  ]
  assert.equal((await run({ markers: others })).result.recorded, true)
  assert.equal((await run({ readable: false })).result.reason, 'comments-unreadable')
})

test('a standing note that says the same thing is reused without a second upload @sessions.AC248601', async () => {
  const first = await run()
  const standing = {
    kind: 'proof-note',
    head: HEAD,
    digest: first.notes[0].note.digest,
    page: first.result.page.url,
    at: 1,
    id: 1,
  }
  const again = await run({ markers: [standing] })
  assert.equal(again.uploads.length, 0)
  assert.equal(again.notes.length, 0)
  assert.equal(again.checkRuns.length, 1, 'a reused note still mirrors its proof to checks')
  assert.equal(again.checkRuns[0].message.detailsUrl, standing.page)
  assert.deepEqual(again.result.page, { published: true, url: standing.page, reused: true })
  const changed = await run({ markers: [standing], diff: `${DIFF}+message Team {}\n` })
  assert.equal(changed.uploads.length, 1, 'a changed page is uploaded again')
})

test('a failed upload still notes the level, with a null page @sessions.AC248602', async () => {
  const { result, notes } = await run({
    upload: () => ({ ok: false, reason: 'hook-failed:exit:3' }),
  })
  assert.deepEqual(result.page, { published: false, reason: 'hook-failed:exit:3' })
  assert.equal(result.recorded, true)
  assert.deepEqual(parseProofLine(notes[0].note.body), {
    head: HEAD,
    page: null,
    level: 'moderate',
  })
})

test('a failed upload reports its detail on the page, unless it carries an env credential', async () => {
  const detail = "EACCES: permission denied, mkdir '/repo/.verify-evidence'"
  const { result } = await run({ upload: () => ({ ok: false, reason: 'upload-failed', detail }) })
  assert.deepEqual(result.page, { published: false, reason: 'upload-failed', detail })
  const leaked = await run({
    env: { PROOF_UPLOAD_TOKEN: 'leaked-value-123' },
    upload: () => ({ ok: false, reason: 'upload-failed', detail: 'refused leaked-value-123' }),
  })
  assert.deepEqual(leaked.result.page, { published: false, reason: 'upload-failed' })
})

/** A proof-media dir holding two head clips: a.mp4 (100 bytes) and b.mp4 (300 bytes). */
function twoClips(t) {
  const dir = tempDir('proof-fit-media-', t)
  const clip = (id, bytes) => {
    fs.writeFileSync(path.join(dir, `${id}.mp4`), 'x'.repeat(bytes))
    return {
      id,
      side: 'head',
      title: `Clip ${id}`,
      file: `${id}.mp4`,
      durationMs: 5000,
      chapters: [],
      stills: [],
    }
  }
  const media = {
    schema: 'proof-media/1',
    head: HEAD,
    clips: [clip('a', 100), clip('b', 300)],
    transcripts: [],
  }
  fs.writeFileSync(path.join(dir, 'media.json'), JSON.stringify(media))
  return dir
}
const OVER = (kind, max) => ({ ok: false, reason: 'over-limit', limit: { kind, max } })
const PAGE_URL = `https://proof.example/acme/widgets/7/${HEAD}/index.html`
/** Upload answers in order, then success. */
const answers = (...list) => {
  let n = 0
  return () => list[n++] ?? { ok: true, url: PAGE_URL }
}

test('an over-limit refusal publishes the fitted page with its note and omitted count @sessions.AC261202', async (t) => {
  const mediaDir = twoClips(t)
  const { result, uploads, notes } = await run({
    input: { mediaDir },
    upload: answers(OVER('file-count', 3)),
  })
  assert.equal(uploads.length, 2)
  assert.deepEqual(
    uploads[0].assets.map((a) => a.name),
    ['media/a.mp4', 'media/b.mp4', 'player.js'],
  )
  assert.ok(!uploads[0].html.includes('data-omitted'))
  assert.deepEqual(
    uploads[1].assets.map((a) => a.name),
    ['media/a.mp4', 'player.js'],
  )
  assert.ok(uploads[1].html.includes('<div class="bnotice stacked" data-omitted="1">'))
  assert.deepEqual(result.page, {
    published: true,
    url: PAGE_URL,
    publisher: 'hook',
    omittedMedia: 1,
  })
  assert.equal(parseProofLine(notes[0].note.body).page, PAGE_URL)
})

const FELL = { kind: 'access-put', reason: 'credentials-missing', detail: 'PROOF_ACCESS_CLIENT_ID' }
const WITH_FALLBACK = {
  discoverPublisher: () => ({
    publisher: { kind: 'access-put', baseUrl: 'https://access.example' },
    fallback: { kind: 'hosted' },
  }),
}

test('after a fallback, a fitted retry goes straight to the hosted host and keeps the reason @sessions.AC278701', async (t) => {
  const mediaDir = twoClips(t)
  const { result, uploads, notes } = await run({
    input: { mediaDir },
    overrides: WITH_FALLBACK,
    upload: answers({ ...OVER('file-count', 3), fallbackFrom: FELL }),
  })
  assert.deepEqual(
    uploads.map((upload) => [upload.publisher.kind, upload.fallback]),
    [
      ['access-put', { kind: 'hosted' }],
      ['hosted', { kind: 'hosted' }],
    ],
  )
  assert.deepEqual(result.page, {
    published: true,
    url: PAGE_URL,
    publisher: 'hosted',
    fallbackFrom: FELL,
    omittedMedia: 1,
  })
  assert.ok(
    notes[0].note.body.includes('access-put: credentials missing (PROOF_ACCESS_CLIENT_ID);'),
  )
})

test('a fallback note is reused on a re-run, and a failed fallback says so @sessions.AC278701', async () => {
  const fell = () => ({ ok: true, url: PAGE_URL, fallbackFrom: FELL })
  const first = await run({ overrides: WITH_FALLBACK, upload: fell })
  const standing = {
    kind: 'proof-note',
    head: HEAD,
    digest: first.notes[0].note.digest,
    page: PAGE_URL,
    at: 1,
    id: 1,
  }
  const again = await run({ markers: [standing], overrides: WITH_FALLBACK, upload: fell })
  assert.equal(again.uploads.length, 0, 'the notice is outside the digest')
  const failed = await run({
    overrides: WITH_FALLBACK,
    upload: () => ({ ok: false, reason: 'quota-exhausted', fallbackFrom: FELL }),
  })
  assert.deepEqual(failed.result.page, {
    published: false,
    reason: 'quota-exhausted',
    fallbackFrom: FELL,
  })
  assert.ok(
    failed.notes[0].note.body.includes(
      'Bossanova hosting did not take the page either (quota-exhausted).',
    ),
  )
  assert.equal(parseProofLine(failed.notes[0].note.body).page, null)
})

test('a fit that drops every media file publishes a page with no media problem @sessions.AC261202', async (t) => {
  const { result, uploads } = await run({
    input: { mediaDir: twoClips(t) },
    upload: answers(OVER('file-count', 2)),
  })
  assert.deepEqual(
    uploads[1].assets.map((a) => a.name),
    ['player.js'],
  )
  assert.ok(uploads[1].html.includes('data-omitted="2"'))
  assert.ok(
    !uploads[1].html.includes('proof-media/1 manifest'),
    'diagnostics name no media problem',
  )
  assert.equal(result.page.omittedMedia, 2)
})

test('an over-limit refusal with no limit, or with no media to drop, keeps the null page and the note @sessions.AC261202', async (t) => {
  const bare = await run({
    input: { mediaDir: twoClips(t) },
    upload: answers({ ok: false, reason: 'over-limit' }),
  })
  assert.deepEqual(
    [bare.uploads.length, bare.result.page],
    [1, { published: false, reason: 'over-limit' }],
  )
  const none = await run({ upload: answers(OVER('file-count', 1)) })
  assert.deepEqual(
    [none.uploads.length, none.result.page],
    [1, { published: false, reason: 'over-limit' }],
  )
  for (const { result, notes } of [bare, none]) {
    assert.equal(result.recorded, true)
    assert.deepEqual(parseProofLine(notes[0].note.body), {
      head: HEAD,
      page: null,
      level: 'moderate',
    })
  }
})

test('a page that carries a credential is never uploaded and the note carries no excerpt @sessions.AC248602', async () => {
  const token = 'ghp_' + 'x'.repeat(36)
  const { result, uploads, notes } = await run({ diff: `${DIFF}+const key = "${token}"\n` })
  assert.deepEqual(result.page, { published: false, reason: 'page-contains-secret' })
  assert.equal(uploads.length, 0)
  assert.ok(!notes[0].note.body.includes(token))
  const named = await run({
    env: { DEPLOY_API_KEY: 'hunter2-hunter2' },
    diff: `${DIFF}+hunter2-hunter2\n`,
  })
  assert.equal(named.result.page.reason, 'page-contains-secret')
})

test('a refused keychain secret never reaches the proof note @sessions.AC256203', async () => {
  const token = 'private-publisher-token-abc'
  let fetchCalls = 0
  const { result, notes } = await run({
    diff: `${DIFF}+${token}\n`,
    overrides: {
      discoverPublisher: () => ({
        publisher: { kind: 'access-put', baseUrl: 'https://proof.example' },
      }),
      uploadPage: (input) => uploadProofPage({ ...input, platform: 'darwin' }),
      readKeychain: ({ account }) =>
        account === 'PROOF_ACCESS_CLIENT_SECRET' ? token : 'publisher-client',
      fetchImpl: () => {
        fetchCalls++
        throw new Error('a secret-bearing page must not be sent')
      },
    },
  })
  assert.deepEqual(result.page, { published: false, reason: 'page-contains-secret' })
  assert.equal(fetchCalls, 0)
  assert.equal(result.recorded, true)
  assert.equal(notes.length, 1)
  assert.ok(!notes[0].note.body.includes(token))
  assert.deepEqual(parseProofLine(notes[0].note.body), {
    head: HEAD,
    page: null,
    level: 'moderate',
  })
})

test('no failure inside the proof escapes it @sessions.AC248604', async () => {
  const boom = () => {
    throw new Error('boom')
  }
  for (const overrides of [
    { recordHeadNote: boom },
    { readOperatorMarkers: boom },
    { uploadPage: boom },
  ])
    assert.deepEqual((await run({ overrides })).result, { recorded: false, reason: 'proof-failed' })
  const moved = await recordHeadProof({ judged: { ...judged, headSha: BASE }, headSha: HEAD }, {})
  assert.deepEqual(moved, { recorded: false, reason: 'judge-other-head' })
})

function hook(root, name) {
  const dir = path.join(root, '.claude', 'skills', name)
  fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'SKILL.md'),
    `---\nname: ${name}\nx-boss-extension:\n  extends: boss-verify\n  role: proof-publish\n  publish: publish.mjs\n---\n\n# ${name}\n`,
  )
  fs.writeFileSync(path.join(dir, 'publish.mjs'), '')
}

test('the publisher is read from the trusted base tree, never the checkout @sessions.AC248601', (t) => {
  const trusted = tempDir('proof-trusted-', t)
  const checkout = tempDir('proof-checkout-', t)
  hook(checkout, 'boss-verify-head-hook')
  const trustedTree = ({ baseSha }) =>
    baseSha === BASE ? { ok: true, root: trusted } : { ok: false }
  const hosted = discoverProofPublisher({ baseSha: BASE, cwd: checkout, trustedTree })
  assert.deepEqual(hosted, { publisher: { kind: 'hosted' } })
  const daemon = discoverProofPublisher({
    baseSha: BASE,
    cwd: checkout,
    trustedTree,
    loadConfig: (root) => (root === trusted ? { publishConfig: { proof: {} } } : {}),
  })
  assert.deepEqual(daemon.publisher, { kind: 'daemon' })
  hook(trusted, 'boss-verify-base-hook')
  const found = discoverProofPublisher({ baseSha: BASE, cwd: checkout, trustedTree })
  assert.equal(found.publisher.kind, 'hook')
  assert.equal(found.publisher.extension.name, 'boss-verify-base-hook')
  const lost = discoverProofPublisher({ baseSha: HEAD, cwd: checkout, trustedTree })
  assert.equal(lost.reason, 'trusted-tree-unavailable')
})

test('the daemon uploads the page from the workspace evidence directory, then it is removed @sessions.AC248602', async (t) => {
  const workspace = tempDir('proof-workspace-', t)
  const calls = []
  const callDaemon = async (_socket, method, message) => {
    calls.push({
      method,
      message,
      html: fs.readFileSync(path.join(message.evidenceDir, PAGE_OBJECT), 'utf8'),
    })
    return { keyPrefix: `acme/widgets/7/${HEAD}/`, publicBaseUrl: 'https://proof.example/' }
  }
  const target = { repo: 'acme/widgets', pr: 7, headSha: HEAD }
  const common = {
    publisher: { kind: 'daemon' },
    html: '<p>page</p>',
    target,
    workspaceRoot: workspace,
  }
  const uploaded = await uploadProofPage({ ...common, callDaemon, env: { BOSS_SOCKET: '/s' } })
  assert.deepEqual(uploaded, {
    ok: true,
    url: `https://proof.example/acme/widgets/7/${HEAD}/index.html`,
  })
  assert.equal(calls[0].method, 'UploadEvidence')
  assert.equal(path.dirname(calls[0].message.evidenceDir), path.join(workspace, '.verify-evidence'))
  assert.deepEqual(
    [calls[0].message.prNumber, calls[0].message.headSha, calls[0].html],
    [7, HEAD, '<p>page</p>'],
  )
  assert.equal(fs.existsSync(path.join(workspace, '.verify-evidence')), false)
  const refused = async () => {
    throw Object.assign(new Error('pages off'), { code: 'failed_precondition' })
  }
  assert.deepEqual(await uploadProofPage({ ...common, callDaemon: refused }), {
    ok: false,
    reason: 'page-refused',
  })
})

test('comment markers tell a repo proof from the core proof note', () => {
  const built = buildProofRecord({ judged, repo: 'acme/widgets', pr: 7 })
  const note = proofNote({ headSha: HEAD, proof: built.proof, block: null })
  const comment = (id, body) => ({
    id,
    body,
    user: { login: 'verify' },
    author_association: 'MEMBER',
    created_at: '2026-11-10T00:00:00Z',
  })
  const markers = operatorMarkers([
    comment(1, `Evidence\n\n${proofLine(built.proof)}\n\n<!-- boss-evidence: {} -->`),
    comment(2, note.body),
  ])
  assert.deepEqual(
    markers.map((m) => [m.kind, m.head]),
    [
      ['repo-proof', HEAD],
      ['proof-note', HEAD],
    ],
  )
  assert.equal(markers[1].digest, note.digest)
})

test('media and the player go before the page, in daemon-sized calls, through either publisher @sessions.AC248607', async (t) => {
  const workspace = tempDir('proof-workspace-', t)
  const evidence = tempDir('proof-evidence-', t)
  const assets = Array.from({ length: 70 }, (_, i) => {
    fs.writeFileSync(path.join(evidence, `s${i}.png`), `still ${i}`)
    return { name: `media/s${i}.png`, from: path.join(evidence, `s${i}.png`) }
  })
  assets.push({ name: 'player.js', text: 'player()' })
  const target = { repo: 'acme/widgets', pr: 7, headSha: HEAD }
  const calls = []
  const callDaemon = async (_socket, _method, message, opts) => {
    const read = (name) => fs.readFileSync(path.join(message.evidenceDir, name), 'utf8')
    calls.push({
      names: message.objects.map((o) => o.name),
      first: read(message.objects[0].path),
      opts,
    })
    return { keyPrefix: `acme/widgets/7/${HEAD}/`, publicBaseUrl: 'https://proof.example' }
  }
  const daemon = await uploadProofPage({
    publisher: { kind: 'daemon' },
    html: '<p>page</p>',
    assets,
    target,
    workspaceRoot: workspace,
    callDaemon,
  })
  assert.equal(daemon.ok, true)
  assert.deepEqual(
    calls.map((c) => [c.names.length, c.names[0], c.names.at(-1)]),
    [
      [64, 'media/s0.png', 'media/s63.png'],
      [7, 'media/s64.png', 'player.js'],
      [1, PAGE_OBJECT, PAGE_OBJECT],
    ],
  )
  assert.deepEqual(
    calls.map((c) => c.first),
    ['still 0', 'still 64', '<p>page</p>'],
  )
  assert.ok(calls[0].opts.timeoutMs > calls[2].opts.timeoutMs, 'media get the long budget')
  assert.equal(fs.existsSync(path.join(workspace, '.verify-evidence')), false)

  const hooked = []
  const runHook = ({ envelope }) => {
    hooked.push(envelope.objects.map((o) => [o.name, o.contentType]))
    return { ok: true, baseUrl: 'https://proof.example' }
  }
  const hook = await uploadProofPage({
    publisher: { kind: 'hook', extension: { name: 'host' } },
    html: '<p>page</p>',
    assets: [assets[0], assets.at(-1)],
    target,
    runHook,
  })
  assert.deepEqual(hook, {
    ok: true,
    url: `https://proof.example/acme/widgets/7/${HEAD}/index.html`,
  })
  assert.deepEqual(hooked, [
    [
      ['media/s0.png', 'image/png'],
      ['player.js', 'text/javascript; charset=utf-8'],
    ],
    [[PAGE_OBJECT, 'text/html; charset=utf-8']],
  ])
  const failed = await uploadProofPage({
    publisher: { kind: 'hook', extension: { name: 'host' } },
    html: '<p>page</p>',
    assets: [assets[0]],
    target,
    runHook: ({ envelope }) =>
      envelope.objects[0].name === PAGE_OBJECT
        ? assert.fail('the page never goes up without its media')
        : { ok: false, reason: 'exit:2' },
  })
  assert.deepEqual(failed, { ok: false, reason: 'hook-failed:exit:2' })
})

test('a checkout without the base commit fetches it before any diff, so the page has its file list', async () => {
  const base = BASE
  const { result, calls } = await run({ missing: new Set([base]) })
  assert.equal(result.recorded, true)
  const fetch = calls.findIndex((c) => c === `fetch --no-tags --quiet origin ${base}`)
  const diff = calls.findIndex(
    (c) => c.startsWith('diff') || c.startsWith('--literal-pathspecs diff'),
  )
  assert.ok(fetch >= 0, `fetches the missing base: ${calls.join(' | ')}`)
  assert.ok(diff > fetch, 'the fetch comes before the first diff')
  assert.ok(
    !calls.includes(`fetch --no-tags --quiet origin ${HEAD}`),
    'a present head is not fetched',
  )
})

test('after the base moves, the page diffs and lists only the branch, on its merge base', async (t) => {
  const dir = tempDir('proof-merge-base-', t)
  const git = (...args) =>
    execFileSync(
      'git',
      ['-c', 'user.name=t', '-c', 'user.email=t@t', '-c', 'commit.gpgsign=false', ...args],
      { cwd: dir, encoding: 'utf8' },
    ).trim()
  git('init', '-q', '-b', 'trunk')
  fs.writeFileSync(path.join(dir, 'api.proto'), 'syntax = "proto3";\n\n\n\n\n\n\n\n')
  git('add', '.')
  git('commit', '-qm', 'fork')
  const fork = git('rev-parse', 'HEAD')
  git('checkout', '-qb', 'pr')
  fs.appendFileSync(path.join(dir, 'api.proto'), 'message User {}\n')
  git('commit', '-qam', 'Add the user message')
  const head = git('rev-parse', 'HEAD')
  git('checkout', '-q', 'trunk')
  fs.writeFileSync(path.join(dir, 'api.proto'), 'syntax = "proto3";\nmessage Main {}\n\n\n\n\n\n\n')
  fs.writeFileSync(path.join(dir, 'main.ts'), 'export const main = 1\n')
  git('add', '.')
  git('commit', '-qm', 'Trunk moves on')
  const base = git('rev-parse', 'HEAD')
  const read = (command, args) => {
    try {
      return { ok: true, output: execFileSync(command, args, { cwd: dir, encoding: 'utf8' }) }
    } catch {
      return { ok: false }
    }
  }
  const uploads = []
  await recordHeadProof(
    {
      read,
      repo: 'acme/widgets',
      pr: 7,
      headSha: head,
      judged: { ...judged, headSha: head, baseSha: base },
      cwd: dir,
    },
    {
      readOperatorMarkers: () => ({ readable: true, markers: [] }),
      recordHeadNote: () => ({
        outcome: 'posted',
        url: 'https://github.com/acme/widgets/pull/7#c1',
      }),
      readBranchContext: () => undefined,
      discoverPublisher: () => ({ publisher: { kind: 'hook', extension: { name: 'host' } } }),
      uploadPage: async (input) => {
        uploads.push(input)
        return { ok: true, url: `https://proof.example/acme/widgets/7/${head}/index.html` }
      },
    },
  )
  const { html } = uploads[0]
  assert.match(html, /message User \{\}/)
  assert.doesNotMatch(html, /message Main|main\.ts|Trunk moves on/)
  assert.match(html, new RegExp(`on <code>${fork.slice(0, 12)}</code>`))
  assert.ok(!html.includes(base.slice(0, 7)), 'the base tip is never shown')
})
