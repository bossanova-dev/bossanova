#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'

import {
  generateToken,
  issueSlug,
  parsePlanUploadArgs,
  planObjectKey,
  planPublicUrl,
  uploadPlanFile,
} from './plan-upload.mjs'

test('issueSlug lowercases, hyphenates, and upcases the id', () => {
  assert.equal(
    issueSlug('bos-5', 'Add an unsubscribe mechanism!'),
    'BOS-5-add-an-unsubscribe-mechanism',
  )
})

test('issueSlug collapses runs of non-alphanumerics and trims edges', () => {
  assert.equal(issueSlug('BOS-12', '  Review   the   DB schema  '), 'BOS-12-review-the-db-schema')
})

test('generateToken returns 32 lowercase hex chars', () => {
  const token = generateToken()
  assert.match(token, /^[0-9a-f]{32}$/)
})

test('generateToken is non-deterministic', () => {
  assert.notEqual(generateToken(), generateToken())
})

test('planObjectKey nests under plans/bossanova/<ID>/<token>.md', () => {
  assert.equal(
    planObjectKey('bos-5', 'deadbeefdeadbeefdeadbeefdeadbeef'),
    'plans/bossanova/BOS-5/deadbeefdeadbeefdeadbeefdeadbeef.md',
  )
})

test('planPublicUrl joins base + key and strips a trailing slash on the base', () => {
  assert.equal(
    planPublicUrl('https://proof.bossanova.dev/', 'plans/bossanova/BOS-5/abc.md'),
    'https://proof.bossanova.dev/plans/bossanova/BOS-5/abc.md',
  )
})

test('planPublicUrl defaults the base when undefined', () => {
  assert.equal(
    planPublicUrl(undefined, 'plans/bossanova/BOS-5/abc.md'),
    'https://proof.bossanova.dev/plans/bossanova/BOS-5/abc.md',
  )
})

test('parsePlanUploadArgs reads --file, --key, and --dry-run', () => {
  assert.deepEqual(
    parsePlanUploadArgs([
      '--file',
      '/tmp/p.md',
      '--key',
      'plans/bossanova/BOS-5/abc.md',
      '--dry-run',
    ]),
    { file: '/tmp/p.md', key: 'plans/bossanova/BOS-5/abc.md', dryRun: true },
  )
})

test('parsePlanUploadArgs throws when --file is missing', () => {
  assert.throws(() => parsePlanUploadArgs(['--key', 'k']), /--file/)
})

test('parsePlanUploadArgs throws when --key is missing', () => {
  assert.throws(() => parsePlanUploadArgs(['--file', '/tmp/p.md']), /--key/)
})

test('uploadPlanFile returns the public URL and invokes the runner with the r2 command', () => {
  const calls = []
  const url = uploadPlanFile({
    file: '/tmp/p.md',
    key: 'plans/bossanova/BOS-5/abc.md',
    env: {
      BOSS_PROOF_R2_BUCKET: 'bossanova-proof-production',
      BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.bossanova.dev',
    },
    runImpl: (cmd) => calls.push(cmd),
  })
  assert.equal(url, 'https://proof.bossanova.dev/plans/bossanova/BOS-5/abc.md')
  assert.equal(calls.length, 1)
  const [command, args] = calls[0]
  assert.equal(command, 'pnpm')
  assert.ok(args.includes('--remote'))
  assert.ok(args.includes('bossanova-proof-production/plans/bossanova/BOS-5/abc.md'))
  // Lock the load-bearing content-type (the public URL 404s / renders wrong without it).
  assert.deepEqual(args.slice(args.indexOf('--content-type'), args.indexOf('--content-type') + 2), [
    '--content-type',
    'text/markdown; charset=utf-8',
  ])
})

test('uploadPlanFile in dryRun mode does not invoke the runner', () => {
  const calls = []
  const url = uploadPlanFile({
    file: '/tmp/p.md',
    key: 'plans/bossanova/BOS-5/abc.md',
    env: { BOSS_PROOF_R2_BUCKET: 'b' },
    dryRun: true,
    runImpl: (cmd) => calls.push(cmd),
  })
  assert.match(url, /\/plans\/bossanova\/BOS-5\/abc\.md$/)
  assert.equal(calls.length, 0)
})

test('uploadPlanFile throws when the bucket is missing', () => {
  assert.throws(
    () => uploadPlanFile({ file: '/tmp/p.md', key: 'k', env: {}, runImpl: () => {} }),
    /BOSS_PROOF_R2_BUCKET is required/,
  )
})
