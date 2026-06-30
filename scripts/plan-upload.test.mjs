#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'

import {
  generateToken,
  issueSlug,
  parsePlanUploadArgs,
  planObjectKey,
  planPublicUrl,
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
