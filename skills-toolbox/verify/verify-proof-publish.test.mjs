import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { operatorMarkers } from './verify-gate.mjs'
import {
  discoverProofPublisher,
  proofAfterPost,
  proofNote,
  recordHeadProof,
  uploadProofPage,
} from './verify-proof-publish.mjs'
import { buildProofRecord } from './verify-proof-build.mjs'
import { parseProofLine, proofLine } from './verify-proof.mjs'
import { PAGE_OBJECT } from './verify-publish-hook.mjs'

const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)
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
} = {}) {
  const uploads = []
  const notes = []
  const calls = []
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
    { read, repo: 'acme/widgets', pr: 7, headSha: HEAD, judged, cwd: '/repo', env },
    {
      readOperatorMarkers: () => (readable ? { readable: true, markers } : { readable: false }),
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
  return { result, uploads, notes, calls }
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

test('the proof follows only a landed verdict and only adds to its result @sessions.AC248604', async () => {
  const ctx = { headSha: HEAD, repo: 'acme/widgets', pr: 7 }
  const proof = { judged, detach: false }
  const actions = { readOperatorMarkers: () => ({ readable: false }) }
  const failed = { verdict: 'pass', posted: false, reason: 'status-post-failed' }
  assert.equal(await proofAfterPost(ctx, proof, failed, actions), failed)
  const landed = await proofAfterPost(ctx, proof, { verdict: 'pass', posted: true }, actions)
  assert.deepEqual(landed, {
    verdict: 'pass',
    posted: true,
    proof: { recorded: false, reason: 'comments-unreadable' },
  })
  const spawned = []
  const detached = await proofAfterPost(
    { ...ctx, pr: 7 },
    { ...proof, detach: true, files: { judge: '/tmp/judge.json' } },
    { verdict: 'pass', skipped: 'already-posted' },
    { spawnProof: (input) => (spawned.push(input), { detached: true, log: '/tmp/log' }) },
  )
  assert.deepEqual(detached.proof, { detached: true, log: '/tmp/log' })
  assert.equal(spawned[0].files.judge, '/tmp/judge.json')
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
  const none = discoverProofPublisher({ baseSha: BASE, cwd: checkout, trustedTree })
  assert.deepEqual(none, { publisher: null, reason: 'not-configured' })
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

test('after the base moves, the page diffs, lists and logs only the branch, on its merge base', async (t) => {
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
  assert.match(html, /Add the user message/)
  assert.match(html, new RegExp(`on <code>${fork.slice(0, 12)}</code>`))
  assert.ok(!html.includes(base.slice(0, 7)), 'the base tip is never shown')
})
