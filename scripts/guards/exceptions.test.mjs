import assert from 'node:assert/strict'
import test from 'node:test'

import { expiredExceptions, isExcepted, parseExceptions } from './exceptions.mjs'

test('parseExceptions covers the line a trailing marker sits on', () => {
  const text = [
    'const a = 1',
    'const b = 2 // guard-exception(lines-per-file): generated table; expires 2026-12-31',
    'const c = 3',
  ].join('\n')
  const parsed = parseExceptions(text, { guardId: 'lines-per-file' })
  assert.deepEqual(parsed.errors, [])
  assert.deepEqual(parsed.exceptions, [
    {
      guardId: 'lines-per-file',
      line: 2,
      coversLine: 2,
      reason: 'generated table',
      expires: '2026-12-31',
    },
  ])
  assert.equal(isExcepted(parsed, 2), true)
  assert.equal(isExcepted(parsed, 3), false)
})

test('parseExceptions covers the next non-blank line for a marker alone on its line', () => {
  const text = [
    '# guard-exception(naming): legacy name; expires 2026-12-31',
    '',
    'bad_name: 1',
    '<!-- guard-exception(naming): doc anchor; expires 2027-01-02 -->',
    'Bad Heading',
    '    /* guard-exception(naming): vendored; expires 2027-01-03 */',
    '  var X',
  ].join('\n')
  const parsed = parseExceptions(text, { guardId: 'naming' })
  assert.deepEqual(parsed.errors, [])
  assert.deepEqual(
    parsed.exceptions.map(({ line, coversLine, reason, expires }) => [
      line,
      coversLine,
      reason,
      expires,
    ]),
    [
      [1, 3, 'legacy name', '2026-12-31'],
      [4, 5, 'doc anchor', '2027-01-02'],
      [6, 7, 'vendored', '2027-01-03'],
    ],
  )
  assert.equal(isExcepted(parsed, 3), true)
  assert.equal(isExcepted(parsed, 1), false)
})

test('parseExceptions ignores markers for other guards', () => {
  const text = 'x // guard-exception(other): reason; expires 2026-12-31'
  assert.deepEqual(parseExceptions(text, { guardId: 'mine' }), { exceptions: [], errors: [] })
  assert.equal(parseExceptions(text).exceptions.length, 1)
})

test('parseExceptions reports an empty reason as an error', () => {
  const parsed = parseExceptions('x // guard-exception(g):  ; expires 2026-12-31', {
    guardId: 'g',
  })
  assert.deepEqual(parsed.exceptions, [])
  assert.equal(parsed.errors.length, 1)
  assert.match(parsed.errors[0], /^line 1: .*empty reason/)
})

test('parseExceptions reports an invalid date as an error', () => {
  for (const date of ['2026-13-01', '2026-02-30', 'tomorrow', '']) {
    const parsed = parseExceptions(`x // guard-exception(g): reason; expires ${date}`, {
      guardId: 'g',
    })
    assert.deepEqual(parsed.exceptions, [], date)
    assert.equal(parsed.errors.length, 1, date)
    assert.match(parsed.errors[0], /expiry|expires/, date)
  }
})

test('parseExceptions reports a missing expiry and a marker that covers nothing', () => {
  assert.match(
    parseExceptions('x // guard-exception(g): reason only', { guardId: 'g' }).errors[0],
    /expires YYYY-MM-DD/,
  )
  assert.match(
    parseExceptions('x\n// guard-exception(g): reason; expires 2026-12-31\n\n', { guardId: 'g' })
      .errors[0],
    /covers no line/,
  )
})

test('expiredExceptions returns only the entries past today', () => {
  const text = [
    'a // guard-exception(g): old; expires 2026-10-19',
    'b // guard-exception(g): today; expires 2026-10-20',
    'c // guard-exception(g): future; expires 2026-10-21',
  ].join('\n')
  const parsed = parseExceptions(text, { guardId: 'g' })
  assert.deepEqual(
    expiredExceptions(parsed, '2026-10-20').map((entry) => entry.reason),
    ['old'],
  )
  assert.deepEqual(
    expiredExceptions(parsed, new Date('2026-10-22T00:00:00Z')).map((entry) => entry.reason),
    ['old', 'today', 'future'],
  )
})
