import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { checkGolangciCaps } from './golangci-caps.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')

const config = (issues) =>
  [
    'version: "2"',
    '',
    'run:',
    '  timeout: 5m',
    '',
    ...issues,
    '',
    'linters:',
    '  enable:',
    '    - govet',
    '',
  ].join('\n')

const UNCAPPED = [
  'issues:',
  '  max-issues-per-linter: 0',
  '  max-same-issues: 0',
  '  uniq-by-line: false',
]

test('checkGolangciCaps passes when both caps are 0', () => {
  assert.deepEqual(checkGolangciCaps(config(UNCAPPED)).failures, [])
})

test('checkGolangciCaps fails a seeded max-same-issues: 3', () => {
  const { failures } = checkGolangciCaps(
    config([
      'issues:',
      '  max-issues-per-linter: 0',
      '  max-same-issues: 3',
      '  uniq-by-line: false',
    ]),
  )
  assert.equal(failures.length, 1)
  assert.match(failures[0], /max-same-issues is 3/)
})

test('checkGolangciCaps fails a seeded max-issues-per-linter: 50', () => {
  const { failures } = checkGolangciCaps(
    config([
      'issues:',
      '  max-issues-per-linter: 50 # too low',
      '  max-same-issues: 0',
      '  uniq-by-line: false',
    ]),
  )
  assert.equal(failures.length, 1)
  assert.match(failures[0], /max-issues-per-linter is 50/)
})

test('checkGolangciCaps fails a missing issues: block', () => {
  const { failures } = checkGolangciCaps(config([]))
  assert.equal(failures.length, 1)
  assert.match(failures[0], /no top-level issues: block/)
})

test('checkGolangciCaps fails a missing key, because golangci then applies its default cap', () => {
  const { failures } = checkGolangciCaps(
    config(['issues:', '  max-same-issues: 0', '  uniq-by-line: false']),
  )
  assert.equal(failures.length, 1)
  assert.match(failures[0], /max-issues-per-linter is not set/)
})

test('checkGolangciCaps ignores a nested issues: key and caps under another block', () => {
  const text = config([
    'output:',
    '  issues:',
    '    max-issues-per-linter: 0',
    '    max-same-issues: 0',
  ])
  assert.match(checkGolangciCaps(text).failures[0], /no top-level issues: block/)
  const deeper = config([
    'issues:',
    '  exclude-rules:',
    '    - max-same-issues: 0',
    '  max-issues-per-linter: 0',
  ])
  assert.match(checkGolangciCaps(deeper).failures[0], /max-same-issues is not set/)
})

test('checkGolangciCaps passes on the real .golangci.yml', () => {
  const text = fs.readFileSync(path.join(repoRoot, '.golangci.yml'), 'utf8')
  assert.deepEqual(checkGolangciCaps(text).failures, [])
})

test('checkGolangciCaps fails a seeded uniq-by-line: true', () => {
  const { failures } = checkGolangciCaps(
    config(UNCAPPED.map((line) => line.replace('uniq-by-line: false', 'uniq-by-line: true'))),
  )
  assert.equal(failures.length, 1)
  assert.match(failures[0], /uniq-by-line is true/)
})
