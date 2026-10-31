import assert from 'node:assert/strict'
import { test } from 'node:test'
import { classifyReadyIntent, main } from './draft-guard.mjs'

function payload(isDraft = true, state = 'OPEN', totalCount = 0, nodes = []) {
  return {
    data: { repository: { pullRequest: { isDraft, state, timelineItems: { totalCount, nodes } } } },
  }
}

for (const [name, input, verdict] of [
  ['new draft', payload(), 'ready'],
  ['ready PR', payload(false), 'already-ready'],
  ['merged PR', payload(false, 'MERGED'), 'not-open'],
  ['closed PR', payload(true, 'CLOSED'), 'not-open'],
  [
    'converted draft',
    payload(true, 'OPEN', 1, [
      { __typename: 'ConvertToDraftEvent', createdAt: '2026-10-30T12:00:00Z' },
    ]),
    'keep-draft',
  ],
  [
    'previously readied draft',
    payload(true, 'OPEN', 1, [
      { __typename: 'ReadyForReviewEvent', createdAt: '2026-10-29T12:00:00Z' },
    ]),
    'keep-draft',
  ],
  ['GraphQL errors', { ...payload(), errors: [{ message: 'denied' }] }, 'unknown'],
  ['missing PR', { data: { repository: { pullRequest: null } } }, 'unknown'],
  ['invalid count', payload(true, 'OPEN', '1'), 'unknown'],
  ['draft with unrelated timeline events', payload(true, 'OPEN', 3), 'ready'],
  ['malformed event', payload(true, 'OPEN', 3, [null]), 'unknown'],
  ['unexpected event', payload(true, 'OPEN', 3, [{ __typename: 'IssueComment' }]), 'unknown'],
]) {
  test(name, () => assert.equal(classifyReadyIntent(input).verdict, verdict))
}

test('bounded evidence count ignores unrelated timeline events', () => {
  assert.equal(classifyReadyIntent(payload(true, 'OPEN', 3)).readyOnceEvents, 0)
  const result = classifyReadyIntent(
    payload(true, 'OPEN', 20, [
      { __typename: 'ReadyForReviewEvent', createdAt: '2026-10-29T12:00:00Z' },
    ]),
  )
  assert.equal(result.verdict, 'keep-draft')
  assert.equal(result.readyOnceEvents, 1)
})

test('keep-draft reason identifies the event evidence', () => {
  const result = classifyReadyIntent(
    payload(true, 'OPEN', 1, [
      { __typename: 'ConvertToDraftEvent', createdAt: '2026-10-30T12:00:00Z' },
    ]),
  )
  assert.match(result.reason, /ConvertToDraftEvent at 2026-10-30T12:00:00Z/)
  assert.equal(result.isDraft, true)
  assert.equal(result.readyOnceEvents, 1)
})

function invoke(
  args,
  response = {
    status: 0,
    stdout: JSON.stringify(
      payload(true, 'OPEN', 1, [
        { __typename: 'ConvertToDraftEvent', createdAt: '2026-10-30T12:00:00Z' },
      ]),
    ),
  },
) {
  let output = ''
  let calls = []
  const exitCode = main(args, {
    run: (command, argv, options) => {
      calls.push({ command, argv, options })
      return response
    },
    write: (text) => {
      output += text
    },
  })
  return { exitCode, output, calls }
}

test('CLI queries the URL repository and prints keep-draft', () => {
  const result = invoke(['decide', '--pr', 'https://github.com/acme/widget/pull/42'])
  assert.equal(result.exitCode, 0)
  assert.match(result.output, /^verdict: keep-draft\nreason: /)
  assert.equal(result.calls.length, 1)
  assert.equal(result.calls[0].command, 'gh')
  assert.deepEqual(result.calls[0].argv.slice(0, 2), ['api', 'graphql'])
  for (const value of ['owner=acme', 'name=widget', 'number=42'])
    assert.ok(result.calls[0].argv.includes(value))
  assert.ok(result.calls[0].options.timeout > 0)
})

test('Enterprise URL selects its hostname', () => {
  const { calls } = invoke(['decide', '--pr', 'https://code.example.com/acme/widget/pull/42'])
  const index = calls[0].argv.indexOf('--hostname')
  assert.ok(index >= 0)
  assert.equal(calls[0].argv[index + 1], 'code.example.com')
})

for (const response of [
  { status: 1, stderr: 'not authenticated' },
  { status: 0, stdout: 'not json' },
  { status: null, error: new Error('timed out') },
]) {
  test(`runner failure: ${JSON.stringify(response)}`, () => {
    const result = invoke(['decide', '--pr', 'https://github.com/a/b/pull/1'], response)
    assert.equal(result.exitCode, 2)
    assert.match(result.output, /^verdict: unknown/)
  })
}

for (const args of [
  ['decide', '--unexpected'],
  ['decide', '--pr', 'invalid'],
  ['other'],
  ['decide', '--pr', 'https://github.com/a/b/pull/0'],
]) {
  test(`invalid invocation: ${args.join(' ')}`, () => {
    const result = invoke(args)
    assert.equal(result.exitCode, 2)
    assert.equal(result.calls.length, 0)
    assert.match(result.output, /^verdict: unknown/)
  })
}

test('JSON output is one verdict object', () => {
  const result = invoke(['decide', '--pr', 'https://github.com/a/b/pull/1', '--json'])
  assert.equal(JSON.parse(result.output).verdict, 'keep-draft')
  assert.equal(result.output.trim().split('\n').length, 1)
})

test('closed PR exits 1', () => {
  const result = invoke(['decide', '--pr', 'https://github.com/a/b/pull/1'], {
    status: 0,
    stdout: JSON.stringify(payload(false, 'CLOSED')),
  })
  assert.equal(result.exitCode, 1)
  assert.match(result.output, /^verdict: not-open/)
})
