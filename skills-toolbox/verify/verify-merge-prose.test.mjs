import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, mkdirSync, symlinkSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import {
  gitBlobSha,
  readMergePolicy,
  verifyRule,
  validateDecisions,
  fingerprintBlock,
  touchesMergePolicy,
  proseFacts,
} from './verify-merge-prose.mjs'
import { verifyPolicyFromConfigText } from '../skill-config.mjs'

const text =
  '- Additive API changes may merge automatically.\n- Billing changes require human review.\n'
const policy = { state: 'ok', path: 'policy.md', blob: gitBlobSha(Buffer.from(text)), text }
const files = [{ path: 'src/a.go', previousPath: 'old/a.go' }]
const decision = {
  ask: 'api-contract',
  decision: 'auto',
  rule: 'Additive API changes may merge automatically.',
  reason: 'Adds an API field',
  path: 'src/a.go',
}
const validate = (raw, extra = {}) =>
  validateDecisions(raw, { policy, files, judgeLabels: [], ...extra })

test('blob SHA agrees with git hash-object', () => {
  const bytes = Buffer.from(text)
  const result = spawnSync('git', ['hash-object', '--stdin'], { input: bytes, encoding: 'utf8' })
  assert.equal(result.status, 0)
  assert.equal(gitBlobSha(bytes), result.stdout.trim())
})

test('policy reads are bounded to a regular UTF-8 file within the trusted tree', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'merge-policy-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  mkdirSync(join(root, 'base'))
  const base = join(root, 'base')
  const block = (path) => ({ merge: { policy: path } })
  assert.deepEqual(readMergePolicy({}, base), { state: 'none' })
  assert.equal(readMergePolicy(block('policy.md'), null).why, 'no-trusted-tree')
  for (const path of ['../policy.md', '/policy.md', './policy.md', 'x\\policy.md', 'policy.txt'])
    assert.equal(readMergePolicy(block(path), base).why, 'unsafe-path')
  assert.equal(readMergePolicy(block('policy.md'), base).why, 'missing')
  assert.equal(readMergePolicy(block('base.md'), root).why, 'missing')
  mkdirSync(join(base, 'folder.md'))
  assert.equal(readMergePolicy(block('folder.md'), base).why, 'not-a-file')
  for (const [bytes, why] of [
    [Buffer.alloc(32769), 'too-large'],
    [Buffer.from([255]), 'not-utf8'],
    [Buffer.from(' \n'), 'empty'],
  ]) {
    writeFileSync(join(base, 'policy.md'), bytes)
    assert.equal(readMergePolicy(block('policy.md'), base).why, why)
  }
  writeFileSync(join(base, 'policy.md'), text)
  assert.deepEqual(readMergePolicy(block('policy.md'), base), policy)
  writeFileSync(join(root, 'outside.md'), text)
  symlinkSync(join(root, 'outside.md'), join(base, 'escape.md'))
  assert.equal(readMergePolicy(block('escape.md'), base).why, 'outside-tree')
})

test('quotes retain markup and word boundaries while tolerating whitespace reflow', () => {
  assert.ok(verifyRule('Additive API\nchanges may merge automatically.', text))
  for (const quote of [
    'dditive API changes may merge automatically',
    'API',
    'Invented rule may merge anything.',
    'Additive API changes may merge automatic',
  ])
    assert.equal(verifyRule(quote, text), false, quote)
})

test('policy touches include symlink chains, directory aliases and rename sources', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'merge-policy-links-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  mkdirSync(join(root, 'rules'))
  writeFileSync(join(root, 'rules', 'actual.md'), text)
  symlinkSync('rules', join(root, 'alias'))
  symlinkSync('actual.md', join(root, 'rules', 'indirect.md'))
  symlinkSync('alias/indirect.md', join(root, 'policy.md'))
  const linked = readMergePolicy({ merge: { policy: 'policy.md' } }, root)
  assert.equal(linked.state, 'ok')
  assert.equal(linked.text, text)
  for (const path of ['policy.md', 'alias', 'rules/indirect.md', 'rules/actual.md']) {
    assert.equal(touchesMergePolicy([path], linked), true, path)
    assert.equal(
      touchesMergePolicy([{ path: 'moved.md', previousPath: path }], linked),
      true,
      `renamed ${path}`,
    )
  }
  assert.equal(touchesMergePolicy(['unrelated.md'], linked), false)
  symlinkSync(join(root, 'rules', 'indirect.md'), join(root, 'absolute.md'))
  const absolute = readMergePolicy({ merge: { policy: 'absolute.md' } }, root)
  assert.equal(absolute.state, 'ok')
  assert.equal(touchesMergePolicy(['rules/indirect.md'], absolute), true)
  assert.equal(touchesMergePolicy(['rules/actual.md'], absolute), true)
})

test('decisions reject malformed entries and ground quotes, paths, labels and counts', () => {
  assert.equal(validate([{ ...decision, path: 'old/a.go' }]).decisions[0].verified, true)
  assert.equal(
    validate([{ ...decision, ask: 'Domain risk' }], { judgeLabels: ['Domain risk'] }).decisions[0]
      .ask,
    'judge:domain-risk',
  )
  const invented = validate([{ ...decision, rule: 'This rule is completely invented.' }])
    .decisions[0]
  assert.equal(invented.decision, 'human')
  assert.equal(invented.verified, false)
  const { rule, ask, ...rest } = decision
  for (const value of [
    { ...decision, extra: true },
    { ...decision, path: 'absent.go' },
    { ...decision, rule: '' },
    { ...decision, reason: 'a\nb' },
    { ...rest, ask },
    { ...rest, rule },
  ]) {
    const result = validate([value])
    assert.deepEqual(result.decisions, [])
    assert.ok(result.health.includes('judge-decision-rejected'))
  }
  assert.equal(validate([{ ...rest, rule, decision: 'human' }]).decisions.length, 1)
  const many = validate(Array.from({ length: 17 }, () => decision))
  assert.equal(many.decisions.length, 16)
  assert.ok(many.health.includes('judge-decisions-truncated'))
  assert.ok(
    validate([decision], { policy: { state: 'none' } }).health.includes('judge-decisions-ignored'),
  )
})

test('fingerprints and policy path touches preserve no-policy behavior', () => {
  const block = { merge: { policy: 'policy.md', auto: [] } }
  assert.equal(fingerprintBlock(block, { state: 'none' }), block)
  assert.equal(fingerprintBlock(block, policy).merge.policyBlob, policy.blob)
  assert.equal(
    fingerprintBlock(block, { state: 'unreadable', why: 'missing' }).merge.policyBlob,
    'unreadable:missing',
  )
  assert.equal(touchesMergePolicy([{ path: 'new.md', previousPath: 'policy.md' }], policy), true)
  assert.equal(touchesMergePolicy(['policy.md'], { state: 'none' }), false)
  assert.deepEqual(proseFacts({ state: 'none' }, [], { files, headSha: 'a'.repeat(40) }), {
    state: 'none',
    decisions: [],
    stale: false,
  })
})

test('schema accepts a relative Markdown policy and rejects unsafe paths with the named error', () => {
  const check = (policy) =>
    verifyPolicyFromConfigText(JSON.stringify({ verifyDefaults: { merge: { policy } } }), 'test')
  assert.doesNotThrow(() => check('docs/merge-policy.md'))
  assert.doesNotThrow(() => check('.md'))
  for (const path of [
    '/a.md',
    '../a.md',
    'a/../b.md',
    'a//../b.md',
    'a//./b.md',
    './a.md',
    'a\\b.md',
    'a\0.md',
    'a.txt',
    7,
    '',
  ])
    assert.throws(
      () => check(path),
      /verifyDefaults\.merge\.policy must be a repo-relative Markdown \(\.md\) path when present/,
    )
  assert.throws(
    () => verifyPolicyFromConfigText('{"verifyDefaults":{"merge":{"other":true}}}', 'test'),
    /expected policy, auto/,
  )
})
