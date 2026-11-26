import { test } from 'node:test'
import assert from 'node:assert/strict'

import { gateSteps, inlineFullGateCheck, isFullGateCheck } from './acceptance-evidence.mjs'

const gates = (fullGateCommands) => ({ acceptance: { fullGateCommands } })

test('gateSteps follows cd and make -C into the directory each step runs in', () => {
  assert.deepEqual(gateSteps('cd apps/web && make test'), [
    { dir: 'apps/web', command: 'make test' },
  ])
  for (const form of [
    'make -C apps/web test',
    'make -Capps/web test',
    'make --directory apps/web test',
    'make --directory=apps/web test',
    'make test -C apps/web',
  ])
    assert.deepEqual(gateSteps(form), [{ dir: 'apps/web', command: 'make test' }], form)
  assert.deepEqual(gateSteps('cd apps && make -C web lint && cd .. && make test'), [
    { dir: 'apps/web', command: 'make lint' },
    { dir: '.', command: 'make test' },
  ])
  assert.deepEqual(gateSteps('cd ./apps/web/ && pnpm  test'), [
    { dir: 'apps/web', command: 'pnpm test' },
  ])
  for (const unfollowable of ['cd /tmp && make test', 'cd ~ && make test', 'cd .. && make test'])
    assert.equal(gateSteps(unfollowable), null, unfollowable)
  assert.equal(gateSteps('cd "$DIR" && make test'), null)
  assert.equal(gateSteps('make test &&'), null)
})

test('a plain gate matches in any directory, however the check reaches it', () => {
  const config = gates(['make test', 'make lint'])
  for (const check of [
    'make test',
    'cd apps/web && make test',
    'make -C apps/web test',
    'make lint && make -C apps/web test',
  ])
    assert.equal(isFullGateCheck(config, check), true, check)
})

test('a check with any step that is no gate fails the match', () => {
  const config = gates(['make test', { dir: 'apps/*', command: 'pnpm test' }])
  for (const check of [
    'make test && node --test foo.test.mjs',
    'cd apps/web && make test && rm -rf build',
    'cd apps/web && make test-e2e',
    'make -C apps/web -k test',
    'cd /abs && make test',
    'cd services/api && pnpm test',
    'pnpm test',
    'cd apps/web',
  ])
    assert.equal(isFullGateCheck(config, check), false, check)
})

test('a directory-scoped gate matches only in the directories its glob names', () => {
  const config = gates([
    { dir: 'apps/*', command: 'make test' },
    { dir: '.', command: 'make lint' },
  ])
  assert.equal(isFullGateCheck(config, 'cd apps/web && make test'), true)
  assert.equal(isFullGateCheck(config, 'make -C apps/api test'), true)
  assert.equal(isFullGateCheck(config, 'make test'), false)
  assert.equal(isFullGateCheck(config, 'cd apps/web/sub && make test'), false)
  assert.equal(isFullGateCheck(config, 'make lint'), true)
  assert.equal(isFullGateCheck(config, 'cd apps/web && make lint'), false)
})

test('a plain gate that moves directory itself counts only there', () => {
  const config = gates(['make -C apps/web test'])
  assert.equal(isFullGateCheck(config, 'cd apps/web && make test'), true)
  assert.equal(isFullGateCheck(config, 'make test'), false)
})

test('existing literal entries keep matching, bare cd entries included', () => {
  const config = gates(['cd apps/web', 'pnpm test'])
  assert.equal(isFullGateCheck(config, 'cd apps/web && pnpm test'), true)
  assert.equal(isFullGateCheck(config, 'cd apps/web'), true)
  assert.equal(isFullGateCheck({ commands: { test: 'make test' } }, 'make  test'), true)
  assert.equal(isFullGateCheck(gates([]), 'make test'), false)
})

test('an inline gate assertion is recovered through the same matcher', () => {
  const config = gates(['make test'])
  assert.equal(
    inlineFullGateCheck(config, '`make -C apps/web test` passes.'),
    'make -C apps/web test',
  )
  assert.equal(inlineFullGateCheck(config, '`make test && ./deploy` passes.'), null)
})
