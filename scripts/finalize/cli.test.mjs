// scripts/finalize/cli.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { runCli } from './cli.mjs'

test('inject-pr-tag routes to the adapter injectPrTag with PR number + BASE_BRANCH', () => {
  const calls = []
  const resolve = () => ({ injectPrTag: (pr, opts) => calls.push({ pr, opts }) })
  const code = runCli(['inject-pr-tag', '1234'], { env: { BASE_BRANCH: 'main' }, resolve })
  assert.equal(code, 0)
  assert.equal(calls.length, 1)
  assert.equal(calls[0].pr, '1234')
  assert.equal(calls[0].opts.baseBranch, 'main')
})

test('inject-pr-tag without a PR number exits 2', () => {
  let err = ''
  const code = runCli(['inject-pr-tag'], { errWrite: (s) => (err += s), resolve: () => ({}) })
  assert.equal(code, 2)
  assert.match(err, /<pr-number> is required/)
})

test('an unknown finalize capability exits 2', () => {
  let err = ''
  const code = runCli(['bogus'], { errWrite: (s) => (err += s), resolve: () => ({}) })
  assert.equal(code, 2)
  assert.match(err, /unknown finalize capability: bogus/)
})

test('inject-pr-tag defaults resolve to the real finalize adapter factory', () => {
  // With no injected resolve, a fake runImpl proves the real factory is wired without
  // shelling out. resolveFinalizeAdapter(env, {runImpl}) is used by the default path.
  const calls = []
  const code = runCli(['inject-pr-tag', '9'], {
    env: { BASE_BRANCH: 'dev' },
    runImpl: (cmd, args, opts) => calls.push({ cmd, args, opts }),
  })
  assert.equal(code, 0)
  assert.equal(calls.length, 1)
  assert.match(calls[0].cmd, /add-pr-numbers\.sh$/)
  assert.deepEqual(calls[0].args, ['9'])
  assert.equal(calls[0].opts.env.BASE_BRANCH, 'dev')
})
