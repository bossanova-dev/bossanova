import { test } from 'node:test'
import assert from 'node:assert/strict'

import { normalizeRepositoryIdentity, selectEpicCallbackTarget } from './epic-target.mjs'

const childPrRepo = 'acme/child-pr'
const matchingOrchestrator = { chatId: 'chat-orchestrator', repo: childPrRepo }
const matchingChild = { chatId: 'chat-child', repo: childPrRepo }

test('selectEpicCallbackTarget prefers a verified matching orchestrator target', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo,
      orchestrator: matchingOrchestrator,
      child: matchingChild,
    }),
    matchingOrchestrator,
  )
})

test('selectEpicCallbackTarget falls back to a verified matching child target', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo,
      orchestrator: { chatId: 'chat-orchestrator', repo: 'acme/other' },
      child: matchingChild,
    }),
    matchingChild,
  )
})

test('selectEpicCallbackTarget rejects an orchestrator without a repository identity', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo,
      orchestrator: { chatId: 'chat-orchestrator' },
      child: matchingChild,
    }),
    matchingChild,
  )
})

test('selectEpicCallbackTarget rejects a child without a repository identity', () => {
  assert.equal(
    selectEpicCallbackTarget({
      childPrRepo,
      orchestrator: null,
      child: { chatId: 'chat-child' },
    }),
    null,
  )
})

test('selectEpicCallbackTarget rejects a candidate without a chat id', () => {
  assert.equal(
    selectEpicCallbackTarget({
      childPrRepo,
      orchestrator: { repo: childPrRepo },
      child: { repo: childPrRepo },
    }),
    null,
  )
})

test('selectEpicCallbackTarget returns no target without a child PR repository identity', () => {
  assert.equal(
    selectEpicCallbackTarget({
      orchestrator: matchingOrchestrator,
      child: matchingChild,
    }),
    null,
  )
})

test('selectEpicCallbackTarget returns no target when neither candidate matches the child PR repository', () => {
  assert.equal(
    selectEpicCallbackTarget({
      childPrRepo,
      orchestrator: { chatId: 'chat-orchestrator', repo: 'acme/orchestrator' },
      child: { chatId: 'chat-child', repo: 'acme/child' },
    }),
    null,
  )
})

test('selectEpicCallbackTarget verifies a canonical HTTPS origin URL against an owner/repo slug', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo: 'acme/app',
      orchestrator: { chatId: 'chat-orchestrator', repo: 'https://github.com/acme/app' },
      child: { chatId: 'chat-child', repo: 'https://github.com/acme/app' },
    }),
    { chatId: 'chat-orchestrator', repo: 'acme/app' },
  )
})

test('selectEpicCallbackTarget verifies an scp-like git@host:owner/repo.git identity', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo: 'acme/app',
      orchestrator: { chatId: 'chat-orchestrator', repo: 'git@github.com:acme/app.git' },
    }),
    { chatId: 'chat-orchestrator', repo: 'acme/app' },
  )
})

test('selectEpicCallbackTarget verifies an ssh:// identity', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo: 'acme/app',
      orchestrator: { chatId: 'chat-orchestrator', repo: 'ssh://git@github.com/acme/app' },
    }),
    { chatId: 'chat-orchestrator', repo: 'acme/app' },
  )
})

test('selectEpicCallbackTarget verifies a trailing-slash identity', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo: 'acme/app',
      orchestrator: { chatId: 'chat-orchestrator', repo: 'https://github.com/acme/app/' },
    }),
    { chatId: 'chat-orchestrator', repo: 'acme/app' },
  )
})

test('selectEpicCallbackTarget verifies a .git identity against a .git child PR repository', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo: 'acme/app.git',
      orchestrator: { chatId: 'chat-orchestrator', repo: 'https://github.com/acme/app.git' },
    }),
    { chatId: 'chat-orchestrator', repo: 'acme/app' },
  )
})

test('selectEpicCallbackTarget compares repository identities case-insensitively', () => {
  assert.deepEqual(
    selectEpicCallbackTarget({
      childPrRepo: 'acme/app',
      orchestrator: { chatId: 'chat-orchestrator', repo: 'https://github.com/Acme/App' },
    }),
    { chatId: 'chat-orchestrator', repo: 'acme/app' },
  )
})

test('selectEpicCallbackTarget returns no target for a genuine repository mismatch', () => {
  assert.equal(
    selectEpicCallbackTarget({
      childPrRepo: 'acme/app',
      orchestrator: { chatId: 'chat-orchestrator', repo: 'https://github.com/acme/other' },
      child: { chatId: 'chat-child', repo: 'git@github.com:acme/other.git' },
    }),
    null,
  )
})

test('selectEpicCallbackTarget returns no target for malformed identities and never throws', () => {
  const malformed = ['', '   ', 'https://github.com', 'app', 'https://github.com/acme', 'acme/ap p']

  for (const value of malformed) {
    assert.doesNotThrow(() =>
      selectEpicCallbackTarget({
        childPrRepo: value,
        orchestrator: { chatId: 'chat-orchestrator', repo: 'https://github.com/acme/app' },
      }),
    )
    assert.equal(
      selectEpicCallbackTarget({
        childPrRepo: value,
        orchestrator: { chatId: 'chat-orchestrator', repo: 'https://github.com/acme/app' },
      }),
      null,
      `expected no target for child PR repository ${JSON.stringify(value)}`,
    )

    assert.doesNotThrow(() =>
      selectEpicCallbackTarget({
        childPrRepo: 'acme/app',
        orchestrator: { chatId: 'chat-orchestrator', repo: value },
      }),
    )
    assert.equal(
      selectEpicCallbackTarget({
        childPrRepo: 'acme/app',
        orchestrator: { chatId: 'chat-orchestrator', repo: value },
      }),
      null,
      `expected no target for candidate repository ${JSON.stringify(value)}`,
    )
  }
})

test('normalizeRepositoryIdentity normalizes every accepted repository form', () => {
  const accepted = [
    ['acme/app', 'acme/app'],
    ['acme/app/', 'acme/app'],
    ['acme/app.git', 'acme/app'],
    ['  acme/app  ', 'acme/app'],
    ['Acme/App', 'acme/app'],
    ['https://github.com/acme/app', 'acme/app'],
    ['https://github.com/acme/app/', 'acme/app'],
    ['https://github.com/acme/app.git', 'acme/app'],
    ['https://github.com/Acme/App', 'acme/app'],
    ['http://github.com/acme/app', 'acme/app'],
    ['https://user@github.com/acme/app', 'acme/app'],
    ['https://github.com/acme/app/pull/12', 'acme/app'],
    ['ssh://git@github.com/acme/app', 'acme/app'],
    ['ssh://git@github.com/acme/app.git', 'acme/app'],
    ['git@github.com:acme/app', 'acme/app'],
    ['git@github.com:acme/app.git', 'acme/app'],
    ['git@github.com:Acme/App.git', 'acme/app'],
  ]

  for (const [input, expected] of accepted) {
    assert.equal(normalizeRepositoryIdentity(input), expected, `input ${JSON.stringify(input)}`)
  }
})

test('normalizeRepositoryIdentity returns null for unusable input and never throws', () => {
  const rejected = [
    undefined,
    null,
    42,
    {},
    [],
    () => 'acme/app',
    '',
    '   ',
    '/',
    '///',
    '.git',
    'app',
    'app.git',
    'https://github.com',
    'https://github.com/',
    'https://github.com/acme',
    'git@github.com:',
    'git@github.com:acme',
    'acme/ap p',
    'ac me/app',
  ]

  for (const value of rejected) {
    assert.doesNotThrow(() => normalizeRepositoryIdentity(value))
    assert.equal(
      normalizeRepositoryIdentity(value),
      null,
      `expected null for ${JSON.stringify(value) ?? String(value)}`,
    )
  }
})
