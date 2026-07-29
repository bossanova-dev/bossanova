import { test } from 'node:test'
import assert from 'node:assert/strict'

import { selectEpicCallbackTarget } from './epic-target.mjs'

const childPrRepo = 'repo-child-pr'
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
      orchestrator: { chatId: 'chat-orchestrator', repo: 'repo-other' },
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
      orchestrator: { chatId: 'chat-orchestrator', repo: 'repo-orchestrator' },
      child: { chatId: 'chat-child', repo: 'repo-child' },
    }),
    null,
  )
})
