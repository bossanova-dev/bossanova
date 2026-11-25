import { test } from 'node:test'
import assert from 'node:assert/strict'
import { proofAfterPost } from './verify-proof-after-post.mjs'
import { PROOF_MISSING_MARKER } from './verify-proof-missing.mjs'
import { parseProofLine } from './verify-proof.mjs'

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
    proofMissing: {
      problems: [{ part: 'record', reason: 'comments-unreadable' }],
      comment: 'unknown',
      note: false,
    },
  })
  const spawned = []
  let detachedMarkerReads = 0
  const detached = await proofAfterPost(
    { ...ctx, pr: 7 },
    { ...proof, detach: true, files: { judge: '/tmp/judge.json' } },
    { verdict: 'pass', skipped: 'already-posted' },
    {
      spawnProof: (input) => (spawned.push(input), { detached: true, log: '/tmp/log' }),
      readOperatorMarkers: () => (++detachedMarkerReads, { readable: true, markers: [] }),
    },
  )
  assert.deepEqual(detached.proof, { detached: true, log: '/tmp/log' })
  assert.equal(detached.proofMissing, undefined)
  assert.equal(detachedMarkerReads, 0)
  assert.equal(spawned[0].files.judge, '/tmp/judge.json')

  const spawnFailNotes = []
  const spawnFailComments = []
  const spawnFailed = await proofAfterPost(
    { ...ctx, pr: 7 },
    { ...proof, detach: true, files: { judge: '/tmp/judge.json' } },
    { verdict: 'pass', posted: true },
    {
      spawnProof: () => ({ recorded: false, reason: 'spawn-failed' }),
      readOperatorMarkers: () => ({ readable: true, markers: [] }),
      recordHeadNote: (_read, opts) => {
        spawnFailComments.push(opts)
        return { outcome: 'posted', url: 'https://github.com/acme/widgets/pull/7#issuecomment-1' }
      },
      recordNote: (input) => {
        spawnFailNotes.push(input)
        return { recorded: true, via: 'boss-notes' }
      },
    },
  )
  assert.deepEqual(spawnFailed.proof, { recorded: false, reason: 'spawn-failed' })
  assert.deepEqual(spawnFailed.proofMissing, {
    problems: [{ part: 'record', reason: 'spawn-failed' }],
    comment: 'posted',
    note: true,
  })
  assert.equal(spawnFailComments.length, 1)
  assert.equal(spawnFailComments[0].kind, 'proof-missing')
  assert.match(spawnFailComments[0].note.body, /`record`: `spawn-failed`/)
  assert.equal(spawnFailNotes.length, 1)
  assert.equal(spawnFailNotes[0].trigger, 'proof-missing')
  assert.match(spawnFailNotes[0].body, /record: spawn-failed/)
})

test('dry-run proofAfterPost plans the proof-missing comment in writes and skips recordNote', async () => {
  const writes = []
  let noteCalls = 0
  const ctx = {
    headSha: HEAD,
    repo: '',
    pr: 7,
    dryRun: true,
    writes,
    read: () => ({ ok: true, output: '' }),
  }
  const actions = {
    readOperatorMarkers: () => ({ readable: true, markers: [] }),
    recordHeadNote: (_read, opts) => {
      opts.writes.push({ kind: 'pr-comment', body: opts.note.body })
      return { outcome: 'planned', url: '' }
    },
    recordNote: () => {
      noteCalls++
      return { recorded: true, via: 'boss-notes' }
    },
  }
  const result = await proofAfterPost(
    ctx,
    { judged, detach: false },
    { verdict: 'human', posted: true },
    actions,
  )
  assert.equal(result.posted, true)
  assert.deepEqual(result.proof, { recorded: false, reason: 'record-invalid' })
  assert.deepEqual(result.proofMissing, {
    problems: [{ part: 'record', reason: 'record-invalid' }],
    comment: 'planned',
    note: false,
  })
  assert.equal(noteCalls, 0)
  assert.equal(writes.length, 1)
  assert.equal(writes[0].kind, 'pr-comment')
  const last = writes[0].body
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .at(-1)
  assert.match(last, PROOF_MISSING_MARKER)
  assert.equal(parseProofLine(writes[0].body), null)
})

test('proofAfterPost reuses operator markers read by recordHeadProof without a second read', async () => {
  const passJudged = { ...judged, verdict: 'pass', reason: 'verified' }
  const runCase = async ({ callDaemon, markers = [] }) => {
    let markerReads = 0
    const recordedNotes = []
    const res = await proofAfterPost(
      {
        headSha: HEAD,
        repo: 'acme/widgets',
        pr: 7,
        read: () => ({ ok: true, output: '' }),
        writes: [],
      },
      { judged: passJudged, detach: false },
      { verdict: 'pass', posted: true },
      {
        readOperatorMarkers: () => (++markerReads, { readable: true, markers }),
        recordHeadNote: (_read, opts) => {
          recordedNotes.push(opts)
          return { outcome: 'posted', url: 'https://github.com/acme/widgets/pull/7#issuecomment-1' }
        },
        trustedTree: () => ({ ok: true, root: '/trusted' }),
        loadConfig: () => ({ proofDefaults: { pages: 'parked' } }),
        callDaemon,
        recordNote: () => ({ recorded: true, via: 'boss-notes' }),
      },
    )
    return { res, markerReads, recordedNotes }
  }

  const clean = await runCase({
    callDaemon: async () => ({
      htmlUrl: 'https://github.com/acme/widgets/runs/1',
      isCreated: true,
    }),
  })
  assert.equal(clean.res.proof.recorded, true)
  assert.equal(clean.res.proofMissing, undefined)
  assert.equal(clean.markerReads, 1, 'clean proof reads operator markers only once')

  const checkFailed = await runCase({
    callDaemon: async () => {
      throw new Error('daemon down')
    },
  })
  assert.deepEqual(checkFailed.res.proof.checkRun, { posted: false, reason: 'check-run-failed' })
  assert.equal(checkFailed.res.proofMissing.comment, 'posted')
  assert.equal(checkFailed.markerReads, 1, 'post-note failure reuses operator markers read')

  const resolved = await runCase({
    callDaemon: async () => ({
      htmlUrl: 'https://github.com/acme/widgets/runs/2',
      isCreated: true,
    }),
    markers: [
      {
        kind: 'proof-missing',
        head: HEAD,
        digest: '0123456789abcdef',
        resolved: false,
        id: 5,
        at: 1,
      },
    ],
  })
  assert.equal(resolved.res.proofMissing, undefined)
  assert.equal(resolved.markerReads, 1, 'resolving standing proof-missing reuses markers read')
  assert.equal(
    resolved.recordedNotes.some((n) => n.kind === 'proof-missing'),
    true,
    'still resolves standing proof-missing comment from reused markers',
  )
})
