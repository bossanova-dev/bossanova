import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  closeSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { judge } from './verify-gate.mjs'
import { encodeJudgeRecord } from './verify-judge.mjs'
import { gitBlobSha } from './verify-merge-prose.mjs'
import { REPO, forge, headOf, judgeRecord } from './verify-replay-fixture.mjs'
import { main } from './verify-replay.mjs'
import { writeReplayEnvelope } from './verify-replay-prose.mjs'

const policy = 'docs/policy.md'
const quote = 'Allow documented widget changes with independent tests.'
const baseText = 'Hold changes to the public widget billing contract.'
const blob = gitBlobSha(Buffer.from(quote))
const path = 'src/widget.go'
const holdPath = 'api/widget.proto'
const merge = { policy, auto: [{ level: 'high' }, { area: 'api-contract' }] }
const stem = (pr, sha = blob) => `draft.pr${pr}.${sha.slice(0, 12)}`

function setup(t, prs = [3]) {
  const dir = mkdtempSync(join(tmpdir(), 'verify-prose-fixture-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const root = join(dir, 'base')
  const baseFile = join(root, policy)
  const draft = join(dir, 'draft.md')
  const config = join(dir, 'merge.json')
  const envelopes = join(dir, 'envelopes')
  const outputs = join(dir, 'outputs')
  mkdirSync(dirname(baseFile), { recursive: true })
  mkdirSync(outputs)
  writeFileSync(baseFile, baseText)
  writeFileSync(draft, quote)
  writeFileSync(config, JSON.stringify(merge))
  const world = forge({
    prs: prs.map((number) => ({
      number,
      paths: [number === 4 ? holdPath : path],
      comments: [judgeRecord(number)],
    })),
    baseConfig: { verifyDefaults: { merge } },
  })
  world.deps.trustedTree = () => ({ ok: true, root })
  const args = ['--repo', REPO, '--candidate', `draft=${config}`]
  const overlay = ['--policy', `draft=${draft}`]
  const roundTrip = ['--envelopes-dir', envelopes, '--judge-outputs', outputs]
  const output = (
    pr,
    {
      decision = 'auto',
      rule = quote,
      ask = 'Widget',
      areas = true,
      head = headOf(pr),
      sha = blob,
      fileStem = stem(pr, sha),
    } = {},
  ) =>
    writeFileSync(
      join(outputs, `${fileStem}.judge.json`),
      JSON.stringify({
        version: 1,
        head,
        policyBlob: sha,
        areas: areas ? [{ label: 'Widget', path, reason: 'Review the widget change.' }] : [],
        decisions: [
          { ask, decision, rule, reason: 'Apply the policy.', path: pr === 4 ? holdPath : path },
        ],
      }),
    )
  return { args, overlay, roundTrip, output, envelopes, baseFile, dir, ...world }
}

test('replays quoted decisions and missing outputs without writing to the forge or base tree', async (t) => {
  const f = setup(t, [3, 4, 5, 6, 7, 8])
  f.output(3)
  f.output(4, { decision: 'human', ask: 'api-contract', areas: false })
  f.output(5, { rule: 'This invented widget rule is absent from the policy.' })
  f.output(6, { sha: 'a'.repeat(40) })
  f.output(7, { head: headOf(3) })
  writeFileSync(join(f.dir, 'outputs', `${stem(8)}.judge.json`), '{')
  const argv = [...f.args, ...f.overlay, ...f.roundTrip]
  const scratch = join(f.dir, 'scratch')
  mkdirSync(scratch)
  const originalTmp = process.env.TMPDIR
  process.env.TMPDIR = scratch
  t.after(() => {
    if (originalTmp === undefined) delete process.env.TMPDIR
    else process.env.TMPDIR = originalTmp
  })
  const first = await main(argv, f.deps)
  const envelopeFile = join(f.envelopes, `${stem(6)}.envelope.json`)
  const envelopeBytes = readFileSync(envelopeFile, 'utf8')
  const [auto, hold, invented, missing, stale, malformed] = first.report.rows.filter(
    (r) => r.candidate === 'draft',
  )
  assert.equal(auto.outcome, 'merge')
  assert.equal(auto.asks[0].decidedBy.structured, null)
  assert.equal(auto.asks[0].decidedBy.prose.rule, quote)
  assert.equal(hold.outcome, 'park')
  assert.ok(hold.codes.includes('human-review:policy'))
  assert.deepEqual(hold.asks[0].decidedBy.structured, { index: 1, rule: { area: 'api-contract' } })
  assert.equal(hold.asks[0].decidedBy.prose.rule, quote)
  assert.equal(invented.asks[0].decidedBy.prose.quoteCheck, 'failed')
  assert.equal(invented.asks[0].decidedBy.prose.decision, 'human')
  assert.ok(invented.codes.includes('human-review:policy'))
  assert.equal(missing.outcome, 'unknown: judge-missing')
  assert.equal(missing.envelope, `${stem(6)}.envelope.json`)
  assert.equal(stale.outcome, 'unknown: judge-output-invalid')
  assert.equal(malformed.outcome, 'unknown: judge-output-invalid')
  const live = await judge(
    { pr: 6, repo: REPO },
    {
      ...f.deps,
      readMergePolicy: () => ({ state: 'ok', path: policy, text: quote, blob }),
    },
  )
  assert.deepEqual(JSON.parse(envelopeBytes), live.judgeEnvelope)
  assert.equal(JSON.parse(envelopeBytes).mergePolicy.text, quote)
  assert.equal(JSON.parse(envelopeBytes).mergePolicy.blob, blob)
  assert.deepEqual(first.report.candidates[1].policy, {
    state: 'ok',
    path: policy,
    blob,
    source: 'candidate',
  })
  assert.equal(first.report.summary.judgeMissing, 1)
  assert.equal(first.report.summary.judgeOutputInvalid, 2)
  assert.equal(first.report.summary.envelopesWritten, 1)
  assert.equal(first.report.summary.quoteCheckFailed, 1)
  assert.equal((await main(argv, f.deps)).text, first.text)
  const md = (await main([...argv, '--format', 'markdown'], f.deps)).text
  assert.equal((await main([...argv, '--format', 'markdown'], f.deps)).text, md)
  assert.ok(md.includes(quote) && md.includes('quote check failed'))
  assert.equal(readFileSync(envelopeFile, 'utf8'), envelopeBytes)
  assert.ok(![first.text, md, envelopeBytes].some((text) => text.includes(f.dir)))
  assert.equal(readFileSync(f.baseFile, 'utf8'), baseText)
  assert.deepEqual(readdirSync(scratch), [])
  assert.equal(first.report.summary.refusedWrites, 0)
  assert.deepEqual(f.writes, [])
  if (process.env.REPLAY_FIXTURE_REPORT) writeFileSync(process.env.REPLAY_FIXTURE_REPORT, md)
})

test('uses base policy text without an override and holds an unreadable base policy', async (t) => {
  const f = setup(t)
  const first = await main([...f.args, ...f.roundTrip], f.deps)
  const base = first.report.candidates[1]
  assert.equal(base.policy.source, 'base')
  assert.equal(base.policy.blob, gitBlobSha(Buffer.from(baseText)))
  assert.equal(first.report.rows[1].outcome, 'unknown: judge-missing')
  assert.notEqual(
    (await main([...f.args, ...f.overlay], f.deps)).report.candidates[1].digest,
    base.digest,
  )
  rmSync(f.baseFile)
  const row = (await main(f.args, f.deps)).report.rows[1]
  assert.ok(row.codes.includes('human-review:policy'))
  assert.deepEqual(row.policy, { state: 'unreadable', path: policy, why: 'missing' })
})

test('a draft keeps the review floor when the PR edits the base policy target', async (t) => {
  const f = setup(t)
  const root = join(f.dir, 'base')
  const target = 'docs/actual.md'
  writeFileSync(join(root, target), quote)
  rmSync(f.baseFile)
  symlinkSync('actual.md', f.baseFile)
  const record = judgeRecord(3)
  const areas = encodeJudgeRecord([], {
    policy: { state: 'ok', blob, path: policy, text: quote },
    decisions: [],
  })
  record.body = record.body.replace('areas=W10', `areas=${areas}`)
  const world = forge({ prs: [{ number: 3, paths: [target], comments: [record] }] })
  world.deps.trustedTree = () => ({ ok: true, root })
  const { report } = await main([...f.args, ...f.overlay], world.deps)
  const row = report.rows.find((entry) => entry.candidate === 'draft')
  assert.equal(row.outcome, 'park')
  assert.ok(row.asks.some((ask) => ask.area === 'verify-policy' && ask.floor))
  assert.equal(readFileSync(join(root, target), 'utf8'), quote)
})

test('envelopes use private permissions and replace the previous file atomically', (t) => {
  const { envelopes } = setup(t)
  const name = writeReplayEnvelope(envelopes, stem(3), '{"draft":1}\n')
  const file = join(envelopes, name)
  assert.equal(statSync(envelopes).mode & 0o077, 0)
  assert.equal(statSync(file).mode & 0o077, 0)
  const previous = openSync(file, 'r')
  try {
    assert.equal(writeReplayEnvelope(envelopes, stem(3), '{"draft":2}\n'), name)
    assert.equal(readFileSync(previous, 'utf8'), '{"draft":1}\n')
    assert.equal(readFileSync(file, 'utf8'), '{"draft":2}\n')
    assert.equal(statSync(file).mode & 0o077, 0)
    assert.deepEqual(readdirSync(envelopes), [name])
  } finally {
    closeSync(previous)
  }
})

test('preserves legacy structured-only names when replay uses no judge files', async (t) => {
  const f = setup(t)
  const config = join(f.dir, 'structured.json')
  writeFileSync(config, '{}')
  const name = `Legacy_Rules.${'a'.repeat(45)}`
  const { report } = await main(['--repo', REPO, '--candidate', `${name}=${config}`], f.deps)
  assert.equal(report.candidates[1].name, name)
})

test('gate-rejected outputs report invalid and keep the judge envelope available', async (t) => {
  const f = setup(t, [3, 5])
  f.output(3, { sha: 'a'.repeat(40), fileStem: stem(3) })
  writeFileSync(
    join(f.dir, 'outputs', `${stem(5)}.judge.json`),
    JSON.stringify({ version: 1, head: headOf(5), policyBlob: blob }),
  )
  const { report } = await main([...f.args, ...f.overlay, ...f.roundTrip], f.deps)
  const rows = report.rows.filter((row) => row.candidate === 'draft')
  assert.equal(report.summary.judgeOutputInvalid, 2)
  assert.equal(report.summary.envelopesWritten, 2)
  assert.equal(report.summary.perCandidate.draft.park, 0)
  for (const row of rows) {
    assert.equal(row.outcome, 'unknown: judge-output-invalid')
    assert.equal(row.envelope, `${stem(row.pr)}.envelope.json`)
    const envelope = JSON.parse(readFileSync(join(f.envelopes, row.envelope), 'utf8'))
    assert.equal(envelope.headSha, row.headSha)
    assert.equal(envelope.mergePolicy.blob, blob)
  }
})

test('rejects unsafe candidate names and unusable policy options with exit 2', (t) => {
  const f = setup(t)
  const script = fileURLToPath(new URL('./verify-replay.mjs', import.meta.url))
  const structured = join(f.dir, 'structured.json')
  writeFileSync(structured, '{}')
  for (const [args, message] of [
    [[...f.args, '--policy', `unknown=${f.baseFile}`], /unknown/],
    [['--candidate', `plain=${structured}`, '--policy', `plain=${f.baseFile}`], /merge.policy/],
    [[...f.args, '--policy', `draft=${f.dir}/missing.md`], /unreadable/],
    [['--candidate', `default=${structured}`], /invalid candidate name/],
    [
      ['--candidate', `Bad.Name=${structured}`, '--envelopes-dir', f.envelopes],
      /invalid candidate name/,
    ],
    [['--candidate', `Bad.Name=${structured}`, '--judge-outputs', f.dir], /invalid candidate name/],
    [['--candidate', `Bad.Name=${f.args.at(-1).slice('draft='.length)}`], /invalid candidate name/],
  ]) {
    const result = spawnSync(process.execPath, [script, ...args], { encoding: 'utf8' })
    assert.equal(result.status, 2)
    assert.match(result.stderr, message)
  }
})
