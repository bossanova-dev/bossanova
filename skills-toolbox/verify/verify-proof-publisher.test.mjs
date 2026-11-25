import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { recordHeadProof, uploadProofPage } from './verify-proof-publish.mjs'
import { discoverProofPublisher } from './verify-proof-publisher.mjs'
import { parseProofLine } from './verify-proof.mjs'
import { PAGE_OBJECT, PUBLISH_HOOK_TIMEOUT_MS } from './verify-publish-hook.mjs'

const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)
const HOSTED = 'EVIDENCE_DESTINATION_HOSTED'
const LOCAL_OR_HOSTED = 'EVIDENCE_DESTINATION_LOCAL_OR_HOSTED'
const REVIEW_URL = `https://review.example/review/acme/widgets/7?head=${HEAD}`
const TARGET = { repo: 'acme/widgets', pr: 7, headSha: HEAD }
const LOCAL = { keyPrefix: `acme/widgets/7/${HEAD}/`, publicBaseUrl: 'https://proof.example/' }
const HOSTED_PAGE = { pageUrl: REVIEW_URL, destination: HOSTED }

function hook(root, name) {
  const dir = path.join(root, '.claude', 'skills', name)
  fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'SKILL.md'),
    `---\nname: ${name}\nx-boss-extension:\n  extends: boss-verify\n  role: proof-publish\n  publish: publish.mjs\n---\n\n# ${name}\n`,
  )
  fs.writeFileSync(path.join(dir, 'publish.mjs'), '')
}

const writeConfig = (root, config) =>
  fs.writeFileSync(path.join(root, '.boss-skills.json'), JSON.stringify(config))

test('the trusted base tree picks hook, then publishConfig.proof, then hosted @sessions.AC261101', (t) => {
  const trusted = tempDir('publisher-trusted-', t)
  const checkout = tempDir('publisher-checkout-', t)
  // The checkout opts out and supplies a hook; neither may change the choice.
  writeConfig(checkout, { publishConfig: { proof: false } })
  hook(checkout, 'boss-verify-head-hook')
  const common = { baseSha: BASE, cwd: checkout, trustedTree: () => ({ ok: true, root: trusted }) }
  const pick = (proof) =>
    discoverProofPublisher({
      ...common,
      loadConfig: (root) => {
        assert.equal(root, trusted, 'only the base tree is read')
        return proof === undefined ? {} : { publishConfig: { proof } }
      },
    })
  assert.deepEqual(discoverProofPublisher(common), { publisher: { kind: 'hosted' } })
  assert.deepEqual(pick(undefined), { publisher: { kind: 'hosted' } })
  const baseUrl = 'https://proof.example'
  assert.deepEqual(pick({ baseUrl, bucket: 'proof' }), { publisher: { kind: 'daemon' } })
  assert.deepEqual(pick({ kind: 'daemon', baseUrl, bucket: 'proof' }), {
    publisher: { kind: 'daemon' },
  })
  assert.deepEqual(pick({ kind: 'access-put', baseUrl }), {
    publisher: { kind: 'access-put', baseUrl, keychainService: undefined },
  })
  assert.deepEqual(pick({ kind: 'access-put', baseUrl: 'http://proof.example' }), {
    publisher: null,
    reason: 'invalid-publish-config',
  })
  assert.deepEqual(discoverProofPublisher({ ...common, trustedTree: () => ({ ok: false }) }), {
    publisher: null,
    reason: 'trusted-tree-unavailable',
  })
  hook(trusted, 'boss-verify-base-hook')
  assert.equal(pick({ baseUrl, bucket: 'proof' }).publisher.kind, 'hook')
})

test('a base opt-out or unreadable base config publishes nothing, unless a base hook exists @sessions.AC261102', (t) => {
  const trusted = tempDir('publisher-optout-', t)
  const common = { baseSha: BASE, cwd: trusted, trustedTree: () => ({ ok: true, root: trusted }) }
  writeConfig(trusted, { publishConfig: { proof: false } })
  assert.deepEqual(discoverProofPublisher(common), { publisher: null, reason: 'opted-out' })
  fs.writeFileSync(path.join(trusted, '.boss-skills.json'), '{ not json')
  assert.deepEqual(discoverProofPublisher(common), {
    publisher: null,
    reason: 'config-unreadable',
  })
  writeConfig(trusted, { publishConfig: { proof: true } })
  assert.deepEqual(discoverProofPublisher(common), {
    publisher: null,
    reason: 'config-unreadable',
  })
  const throwing = () => {
    throw new Error('schema fault')
  }
  assert.deepEqual(discoverProofPublisher({ ...common, loadConfig: throwing }), {
    publisher: null,
    reason: 'config-unreadable',
  })
  // A discovery fault with a readable config is not a config fault, and never falls through.
  assert.deepEqual(
    discoverProofPublisher({ ...common, discover: throwing, loadConfig: () => ({}) }),
    { publisher: null, reason: 'publisher-discovery-failed' },
  )
  hook(trusted, 'boss-verify-base-hook')
  assert.equal(discoverProofPublisher({ ...common, loadConfig: throwing }).publisher.kind, 'hook')
})

/** uploadProofPage through a recording daemon fake answering every call with `answer`. */
async function daemonUpload(t, { publisher, count = 2, answer = LOCAL }) {
  const workspace = tempDir('publisher-workspace-', t)
  const evidence = tempDir('publisher-evidence-', t)
  const assets = Array.from({ length: count }, (_, i) => {
    fs.writeFileSync(path.join(evidence, `s${i}.png`), `still ${i}`)
    return { name: `media/s${i}.png`, from: path.join(evidence, `s${i}.png`) }
  })
  const calls = []
  const callDaemon = async (_socket, method, message, opts) => {
    assert.equal(path.dirname(message.evidenceDir), path.join(workspace, '.verify-evidence'))
    calls.push({ method, message, opts, names: message.objects.map((o) => o.name) })
    if (answer instanceof Error) throw answer
    return answer
  }
  const result = await uploadProofPage({
    publisher: { kind: publisher },
    html: '<p>page</p>',
    assets,
    target: TARGET,
    workspaceRoot: workspace,
    callDaemon,
  })
  assert.equal(fs.existsSync(path.join(workspace, '.verify-evidence')), false)
  return { result, calls }
}

test('a page that fits one call goes up in one call, index.html last, on the media budget @sessions.AC261103', async (t) => {
  for (const [publisher, destination, answer] of [
    ['daemon', LOCAL_OR_HOSTED, LOCAL],
    ['hosted', HOSTED, HOSTED_PAGE],
  ]) {
    const { result, calls } = await daemonUpload(t, { publisher, count: 63, answer })
    assert.equal(result.ok, true, publisher)
    assert.equal(calls.length, 1, publisher)
    assert.equal(calls[0].method, 'UploadEvidence')
    assert.equal(calls[0].message.destination, destination)
    assert.deepEqual([calls[0].message.prNumber, calls[0].message.headSha], [7, HEAD])
    assert.deepEqual(
      [calls[0].names.length, calls[0].names[0], calls[0].names.at(-1)],
      [64, 'media/s0.png', PAGE_OBJECT],
    )
    assert.equal(calls[0].opts.timeoutMs, PUBLISH_HOOK_TIMEOUT_MS)
  }
  const bare = await daemonUpload(t, { publisher: 'daemon', count: 0 })
  assert.deepEqual(bare.calls[0].names, [PAGE_OBJECT])
  assert.ok(bare.calls[0].opts.timeoutMs < PUBLISH_HOOK_TIMEOUT_MS, 'a lone page keeps its budget')
})

test('a page over one call is sent as 64-object media calls, then index.html alone @sessions.AC261103', async (t) => {
  const { result, calls } = await daemonUpload(t, { publisher: 'daemon', count: 64 })
  assert.equal(result.ok, true)
  assert.deepEqual(
    calls.map((c) => [c.names.length, c.names.at(-1), c.message.destination]),
    [
      [64, 'media/s63.png', LOCAL_OR_HOSTED],
      [1, PAGE_OBJECT, LOCAL_OR_HOSTED],
    ],
  )
  assert.equal(calls[0].opts.timeoutMs, PUBLISH_HOOK_TIMEOUT_MS)
  assert.ok(calls[1].opts.timeoutMs < PUBLISH_HOOK_TIMEOUT_MS)
})

test('a returned pageUrl is the page; without one the local prefix check holds @sessions.AC261104', async (t) => {
  for (const publisher of ['daemon', 'hosted']) {
    const { result } = await daemonUpload(t, { publisher, answer: HOSTED_PAGE })
    assert.deepEqual(result, { ok: true, url: REVIEW_URL }, publisher)
  }
  const local = await daemonUpload(t, { publisher: 'daemon' })
  assert.deepEqual(local.result, {
    ok: true,
    url: `https://proof.example/acme/widgets/7/${HEAD}/${PAGE_OBJECT}`,
  })
  const elsewhere = { ...LOCAL, keyPrefix: 'acme/widgets/8/x/' }
  assert.deepEqual((await daemonUpload(t, { publisher: 'daemon', answer: elsewhere })).result, {
    ok: false,
    reason: 'page-url-mismatch',
  })
  // A daemon that ignores destination answers a hosted page as a local upload.
  for (const answer of [LOCAL, { pageUrl: REVIEW_URL }, { destination: HOSTED }]) {
    assert.deepEqual((await daemonUpload(t, { publisher: 'hosted', answer })).result, {
      ok: false,
      reason: 'daemon-unsupported',
    })
  }
})

test('hosted refusals record their reason; other refusals keep the code map @sessions.AC261105', async (t) => {
  const refusal = (code, reason) => Object.assign(new Error('refused'), { code, reason })
  for (const [publisher, error, reason, detail] of [
    ['hosted', refusal('failed_precondition', 'not-configured'), 'not-configured'],
    ['hosted', refusal('failed_precondition', 'quota-exhausted'), 'quota-exhausted'],
    ['hosted', refusal('resource_exhausted', 'over-limit'), 'over-limit'],
    ['daemon', refusal('failed_precondition', 'quota-exhausted'), 'quota-exhausted'],
    ['daemon', refusal('resource_exhausted', 'over-limit'), 'over-limit'],
    ['daemon', refusal('failed_precondition', 'not-configured'), 'page-refused'],
    ['daemon', refusal('failed_precondition', 'no-token'), 'page-refused'],
    ['hosted', refusal('failed_precondition', 'no-token'), 'page-refused'],
    ['daemon', refusal('resource_exhausted'), 'over-limit'],
    ['hosted', refusal('unimplemented'), 'daemon-unsupported'],
    ['daemon', refusal('unavailable'), 'upload-failed', 'unavailable: refused'],
    ['hosted', new Error('socket closed'), 'daemon-unavailable', 'Error: socket closed'],
  ]) {
    const { result, calls } = await daemonUpload(t, { publisher, answer: error })
    assert.deepEqual(
      result,
      { ok: false, reason, ...(detail ? { detail } : {}) },
      `${publisher} ${error.reason ?? error.code}`,
    )
    assert.equal(calls.length, 1, 'a refusal is never retried')
  }
})

test('an over-limit refusal carries its limit only when both headers are valid @sessions.AC261202', async (t) => {
  const refusal = (limit, limitMax, code = 'resource_exhausted', reason = 'over-limit') =>
    Object.assign(new Error('refused'), { code, reason, limit, limitMax })
  for (const [publisher, error, expected] of [
    ['hosted', refusal('file-count', '64'), { kind: 'file-count', max: 64 }],
    ['daemon', refusal('file-bytes', '5242880'), { kind: 'file-bytes', max: 5242880 }],
    ['hosted', refusal('page-bytes', '20971520'), { kind: 'page-bytes', max: 20971520 }],
    ['hosted', refusal(undefined, undefined), null],
    ['hosted', refusal('file-count', undefined), null],
    ['hosted', refusal(undefined, '64'), null],
    ['hosted', refusal('file-size', '64'), null],
    ['hosted', refusal('file-count', '0'), null],
    ['hosted', refusal('file-count', '-5'), null],
    ['hosted', refusal('file-count', '1.5'), null],
    ['hosted', refusal('file-count', '64, 64'), null],
    ['hosted', refusal('file-count', '9'.repeat(20)), null],
  ]) {
    const { result } = await daemonUpload(t, { publisher, answer: error })
    const label = `${publisher} ${error.limit} ${error.limitMax}`
    assert.deepEqual(
      result,
      { ok: false, reason: 'over-limit', ...(expected ? { limit: expected } : {}) },
      label,
    )
  }
  // Any other ResourceExhausted keeps the code map and names no limit.
  const other = await daemonUpload(t, {
    publisher: 'hosted',
    answer: refusal('file-count', '64', 'resource_exhausted', 'quota'),
  })
  assert.deepEqual(other.result, { ok: false, reason: 'over-limit' })
})

/** recordHeadProof for a parked head with real discovery over `trusted` and a recording daemon. */
async function recordParked(t, { trusted, answer = HOSTED_PAGE, loadConfig }) {
  const workspace = tempDir('publisher-record-', t)
  const daemon = []
  const notes = []
  const read = (command, args) => {
    if (command !== 'git') throw new Error(`unexpected ${command}`)
    if (args[0] === 'cat-file') return { ok: true }
    if (args[0] === 'diff') return { ok: true, output: 'diff --git a/a.proto b/a.proto\n+x\n' }
    if (args[0] === 'log') return { ok: true, output: '2026-11-10T00:00:00Z\tAdd x' }
    if (args[0] === 'rev-parse') return { ok: true, output: workspace }
    throw new Error(`unexpected git ${args.join(' ')}`)
  }
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
        { area: 'api-contract', label: 'API contract', paths: ['a.proto'], defaultAsk: 'always' },
      ],
      receipt: { state: 'success' },
    },
  }
  const result = await recordHeadProof(
    { read, repo: 'acme/widgets', pr: 7, headSha: HEAD, judged, cwd: workspace, env: {} },
    {
      trustedTree: () => ({ ok: true, root: trusted }),
      ...(loadConfig ? { loadConfig } : {}),
      readOperatorMarkers: () => ({ readable: true, markers: [] }),
      readBranchContext: () => undefined,
      recordHeadNote: (_read, note) => (notes.push(note), { outcome: 'posted' }),
      callDaemon: async (_socket, method, message) => {
        daemon.push({ method, message })
        if (method === 'UploadEvidence') return answer
        return { htmlUrl: 'https://github.com/acme/widgets/runs/1', isCreated: true }
      },
    },
  )
  return { result, daemon, notes }
}

test('an unconfigured base tree publishes to the hosted host and links its review page @sessions.AC261104', async (t) => {
  const { result, daemon, notes } = await recordParked(t, { trusted: tempDir('hosted-base-', t) })
  assert.deepEqual(result.page, { published: true, url: REVIEW_URL, publisher: 'hosted' })
  const upload = daemon.find((call) => call.method === 'UploadEvidence')
  assert.equal(upload.message.destination, HOSTED)
  assert.equal(upload.message.objects.at(-1).name, PAGE_OBJECT)
  assert.equal(parseProofLine(notes[0].note.body).page, REVIEW_URL)
  const check = daemon.find((call) => call.method === 'PostProofCheckRun')
  assert.equal(check.message.detailsUrl, REVIEW_URL)
})

test('an opted-out or unreadable base config never calls UploadEvidence @sessions.AC261102', async (t) => {
  for (const [loadConfig, reason] of [
    [() => ({ publishConfig: { proof: false } }), 'opted-out'],
    [
      () => {
        throw new Error('bad config')
      },
      'config-unreadable',
    ],
  ]) {
    const { result, daemon, notes } = await recordParked(t, {
      trusted: tempDir('optout-base-', t),
      loadConfig,
    })
    assert.deepEqual(result.page, { published: false, reason })
    assert.equal(notes.length, 1, 'the note still records the level')
    assert.ok(!daemon.some((call) => call.method === 'UploadEvidence'))
  }
})
