import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { proofAfterPost, spawnProof } from './verify-proof-after-post.mjs'
import { PROOF_AUTHOR_ENV } from './verify-proof-author.mjs'
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

const FAKE_AUTHOR = `#!${process.execPath}
const fs = require('node:fs')
fs.writeFileSync(process.env.FAKE_LOG, process.cwd())
const diffAt = process.argv.at(-1).match(/merge-base diff of the head: (\\S+)/)
if (process.env.FAKE_DIFF_LOG && diffAt && fs.existsSync(diffAt[1]))
  fs.writeFileSync(process.env.FAKE_DIFF_LOG, fs.readFileSync(diffAt[1]))
if (process.env.FAKE_AUTHOR === 'prose') process.stdout.write('No spec this time.\\n')
else process.stdout.write(JSON.stringify({ schema: 'proof-spec/1', head: process.env.FAKE_HEAD }))
`

/** A detached proof process's world: a fake claude, a fake bundle verb and a capturing recorder. */
function authorWorld(
  t,
  { mode = 'spec', author, pages, verdict = 'human', markers = [], env } = {},
) {
  const dir = tempDir('verify-after-post-author-', t)
  const bin = path.join(dir, 'claude')
  fs.writeFileSync(bin, FAKE_AUTHOR, { mode: 0o755 })
  const log = path.join(dir, 'cwd.txt')
  const recorded = []
  const bundles = []
  const gitCalls = []
  const checkouts = []
  const actions = {
    cwd: dir,
    env: env ?? {
      PATH: process.env.PATH,
      BOSS_CLAUDE_BIN: bin,
      FAKE_AUTHOR: mode,
      FAKE_HEAD: HEAD,
      FAKE_LOG: log,
      [PROOF_AUTHOR_ENV]: '1',
    },
    readOperatorMarkers: () => ({ readable: true, markers }),
    trustedTree: () => ({ ok: true, root: '/trusted' }),
    loadConfig: () => ({
      proofDefaults: { ...(author ? { author } : {}), ...(pages ? { pages } : {}) },
    }),
    buildProofBundle: async (options) => {
      bundles.push(options)
      return {
        written: true,
        bundle: path.join(options.out, 'bundle.json'),
        media: null,
        proofMedia: options.out,
        spec: path.join(options.out, 'spec.json'),
      }
    },
    recordHeadProof: async (input) => {
      recorded.push({
        ...input,
        specText:
          input.spec && fs.existsSync(input.spec) ? fs.readFileSync(input.spec, 'utf8') : null,
      })
      return { recorded: true, level: 'moderate', note: 'posted', page: { published: true } }
    },
  }
  const read = (command, args) => {
    if (command === 'git') gitCalls.push(args)
    if (command === 'git' && args[0] === 'merge-base')
      return { ok: true, output: `${'b'.repeat(40)}\n` }
    if (command === 'git' && args[0] === 'diff')
      return { ok: true, output: 'diff --git a/app.js b/app.js\n+export const changed = true\n' }
    if (command === 'git' && args.includes('worktree') && args.includes('add')) {
      fs.mkdirSync(args.at(-2), { recursive: true })
      checkouts.push({ path: args.at(-2), real: fs.realpathSync(args.at(-2)) })
    }
    return { ok: true, output: '' }
  }
  const ctx = { headSha: HEAD, repo: 'acme/widgets', pr: 7, read, writes: [] }
  const proof = { judged: { ...judged, verdict }, detach: false, files: { judge: '/tmp/j.json' } }
  return { ctx, proof, actions, recorded, bundles, gitCalls, checkouts, log }
}

test('the detached proof process is the one marked to author', (t) => {
  const spawned = []
  const logDir = tempDir('verify-after-post-log-', t)
  spawnProof({
    pr: 7,
    headSha: HEAD,
    files: { judge: '/tmp/j.json' },
    cwd: logDir,
    env: { PATH: '/bin' },
    logDir,
    spawn: (_bin, args, options) => {
      spawned.push({ args, options })
      return { unref() {} }
    },
  })
  assert.equal(spawned[0].options.env[PROOF_AUTHOR_ENV], '1')
  assert.equal(spawned[0].options.env.PATH, '/bin')
})

test('an authored spec and the bundle media reach the recorded page from a head checkout', async (t) => {
  const w = authorWorld(t)
  const result = await proofAfterPost(w.ctx, w.proof, { verdict: 'human', posted: true }, w.actions)
  assert.equal(result.verdict, 'human')
  assert.deepEqual(result.proof.author, { authored: true })
  assert.equal(result.proofMissing, undefined)
  assert.equal(w.recorded.length, 1)
  assert.deepEqual(JSON.parse(w.recorded[0].specText), { schema: 'proof-spec/1', head: HEAD })
  assert.equal(w.recorded[0].mediaDir, w.bundles[0].out)
  assert.equal(w.bundles[0].judge, '/tmp/j.json')
  const [checkout] = w.checkouts
  assert.ok(w.gitCalls.some((args) => args.includes('add') && args.at(-1) === HEAD))
  assert.equal(fs.readFileSync(w.log, 'utf8'), checkout.real, 'the author ran in the head checkout')
  assert.ok(
    w.gitCalls.some((args) => args.includes('remove') && args.at(-1) === checkout.path),
    'the head checkout is removed',
  )
  assert.equal(fs.existsSync(w.bundles[0].out), false, 'the run-temp bundle is removed')
})

test('the read-only author gets the merge-base diff of the head as a file', async (t) => {
  const w = authorWorld(t)
  const diffLog = path.join(w.actions.cwd, 'diff-seen.txt')
  w.actions.env.FAKE_DIFF_LOG = diffLog
  await proofAfterPost(w.ctx, w.proof, { verdict: 'human', posted: true }, w.actions)
  assert.match(fs.readFileSync(diffLog, 'utf8'), /\+export const changed = true/)
  assert.ok(
    w.gitCalls.some(
      (args) => args[0] === 'diff' && args.at(-1) === HEAD && args.includes('b'.repeat(40)),
    ),
    'the diff runs from the merge base to the head',
  )
})

test('a failed author still records the deterministic page and is no proof loss', async (t) => {
  for (const [world, skipped] of [
    [{ mode: 'prose' }, 'no-json'],
    [
      { env: { PATH: process.env.PATH, BOSS_CLAUDE_BIN: '/nonexistent', [PROOF_AUTHOR_ENV]: '1' } },
      'no-agent',
    ],
  ]) {
    const w = authorWorld(t, world)
    const result = await proofAfterPost(
      w.ctx,
      w.proof,
      { verdict: 'human', posted: true },
      w.actions,
    )
    assert.equal(result.verdict, 'human')
    assert.deepEqual(result.proof.author, { skipped })
    assert.equal(result.proofMissing, undefined)
    assert.equal(w.recorded.length, 1)
    assert.equal(w.recorded[0].spec, undefined, `${skipped} records without a spec`)
    assert.equal(w.recorded[0].mediaDir, w.bundles[0].out)
  }
  const unbundled = authorWorld(t)
  unbundled.actions.buildProofBundle = async () => ({ written: false, reason: 'head-moved' })
  const result = await proofAfterPost(
    unbundled.ctx,
    unbundled.proof,
    { verdict: 'human', posted: true },
    unbundled.actions,
  )
  assert.deepEqual(result.proof.author, { skipped: 'bundle-head-moved' })
  assert.equal(unbundled.recorded[0].spec, undefined)
  assert.equal(unbundled.recorded[0].mediaDir, undefined)
})

test('the author runs only where both policies cover the verdict', async (t) => {
  for (const [world, runs] of [
    [{ author: 'none' }, false],
    [{ author: 'parked', verdict: 'pass' }, false],
    [{ author: 'parked', verdict: 'human' }, true],
    [{ verdict: 'pass' }, true],
    [{ pages: 'parked', verdict: 'pass' }, false],
    [{ pages: 'none' }, false],
  ]) {
    const w = authorWorld(t, world)
    const result = await proofAfterPost(
      w.ctx,
      w.proof,
      { verdict: w.proof.judged.verdict, posted: true },
      w.actions,
    )
    const label = JSON.stringify(world)
    assert.equal(w.bundles.length > 0, runs, label)
    assert.equal(fs.existsSync(w.log), runs, label)
    assert.equal(result.proof.author !== undefined, runs, label)
    assert.equal(w.recorded.length, 1, `${label} still records`)
  }
})

test('the author stays out of in-process posts, given specs, dry runs and standing pages', async (t) => {
  const cases = [
    (w) => delete w.actions.env[PROOF_AUTHOR_ENV],
    (w) => (w.proof.spec = '/tmp/spec.json'),
    (w) => (w.ctx.dryRun = true),
    (w) => (w.actions.readOperatorMarkers = () => ({ readable: false })),
  ]
  for (const kind of ['proof-note', 'repo-proof'])
    cases.push((w) => {
      const marker = { kind, head: HEAD, page: 'https://proof.example/p', at: 1, id: 1 }
      w.actions.readOperatorMarkers = () => ({ readable: true, markers: [marker] })
    })
  for (const change of cases) {
    const w = authorWorld(t)
    change(w)
    const result = await proofAfterPost(
      w.ctx,
      w.proof,
      { verdict: 'human', posted: true },
      w.actions,
    )
    assert.equal(w.bundles.length, 0, change.toString())
    assert.equal(result.proof.author, undefined, change.toString())
    assert.equal(w.recorded.length, 1)
  }
})
