#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'
import { summarizeGoTestJson } from './test-profile-summary.mjs'

test('summarizes slow package test events', () => {
  const input = [
    JSON.stringify({ Action: 'pass', Package: 'example.com/a', Test: 'TestFast', Elapsed: 0.01 }),
    JSON.stringify({ Action: 'pass', Package: 'example.com/a', Test: 'TestSlow', Elapsed: 1.2 }),
    JSON.stringify({ Action: 'pass', Package: 'example.com/b', Elapsed: 2.5 }),
  ].join('\n')

  assert.equal(
    summarizeGoTestJson(input, 2),
    ['2.50s package example.com/b', '1.20s test example.com/a TestSlow'].join('\n'),
  )
})

test('ignores malformed JSON and pass events without packages', () => {
  const input = [
    '{not-json',
    JSON.stringify({ Action: 'pass', Test: 'TestMissingPackage', Elapsed: 3 }),
    JSON.stringify({ Action: 'pass', Elapsed: 2 }),
    JSON.stringify({ Action: 'pass', Package: 'example.com/a', Test: 'TestSlow', Elapsed: 1 }),
  ].join('\n')

  assert.equal(summarizeGoTestJson(input), '1.00s test example.com/a TestSlow')
})

test('returns an empty summary for empty input', () => {
  assert.equal(summarizeGoTestJson(''), '')
})

test('normalizes zero, negative, and NaN limits', () => {
  const input = [
    JSON.stringify({ Action: 'pass', Package: 'example.com/a', Test: 'TestFast', Elapsed: 0.1 }),
    JSON.stringify({ Action: 'pass', Package: 'example.com/a', Test: 'TestSlow', Elapsed: 1.2 }),
    JSON.stringify({ Action: 'pass', Package: 'example.com/b', Elapsed: 2.5 }),
  ].join('\n')

  assert.equal(summarizeGoTestJson(input, 0), '')
  assert.equal(summarizeGoTestJson(input, -1), '')
  assert.equal(
    summarizeGoTestJson(input, Number.NaN),
    [
      '2.50s package example.com/b',
      '1.20s test example.com/a TestSlow',
      '0.10s test example.com/a TestFast',
    ].join('\n'),
  )
})
