import assert from 'node:assert/strict'
import { test } from 'node:test'
import { checkRelationsAvailable } from './github-relations-preflight.mjs'

function fixture({
  missing,
  issues = [{ number: 12 }],
  errorAt,
  error = 'HTTP 403: forbidden',
} = {}) {
  const calls = []
  const ctx = {
    identity: { host: 'git.example.test', owner: 'acme', repo: 'tasks', repository: 'acme/tasks' },
    api(path, args = []) {
      calls.push({ path, args })
      if (path === errorAt) throw new Error(error)
      if (path === 'graphql')
        return {
          data: {
            __type: {
              fields: ['subIssues', 'parent', 'blockedBy', 'blocking']
                .filter((name) => name !== missing)
                .map((name) => ({ name })),
            },
          },
        }
      return []
    },
    issue(verb, args) {
      calls.push({ verb, args })
      if (errorAt === 'list') throw new Error(error)
      return issues
    },
  }
  const run = () =>
    checkRelationsAvailable(ctx, (status, remedy) => {
      throw new Error(`tracker preflight: ${status}: ${remedy}`)
    })
  return { run, calls, ctx }
}

test('each required schema field must exist before repository endpoints are probed', () => {
  for (const missing of ['subIssues', 'parent', 'blockedBy', 'blocking']) {
    const f = fixture({ missing })
    assert.throws(f.run, /tracker preflight: relations-unavailable:.*git.example.test/)
    assert.equal(f.calls.length, 1)
  }
})

test('empty repository passes with schema support alone', () => {
  const f = fixture({ issues: [] })
  f.run()
  assert.deepEqual(f.calls, [
    { path: 'graphql', args: ['-f', 'query={__type(name: "Issue") { fields { name } }}'] },
    { verb: 'list', args: ['--state', 'all', '--limit', '1', '--json', 'number'] },
  ])
})

test('nonempty repository probes both relation endpoints on its newest issue', () => {
  const f = fixture()
  f.run()
  assert.deepEqual(
    f.calls.slice(2).map((call) => call.path),
    ['repos/acme/tasks/issues/12/sub_issues', 'repos/acme/tasks/issues/12/dependencies/blocked_by'],
  )
})

test('404 and 410 on either relation endpoint name repository and feature remedy', () => {
  for (const endpoint of ['sub_issues', 'dependencies/blocked_by']) {
    for (const status of [404, 410]) {
      const f = fixture({
        errorAt: `repos/acme/tasks/issues/12/${endpoint}`,
        error: `Not Found (HTTP ${status})`,
      })
      assert.throws(
        f.run,
        /tracker preflight: relations-unavailable:.*acme\/tasks;.*epics and blocked-by scheduling/,
      )
    }
  }
})

test('permission and transport errors retain upstream status without claiming unsupported features', () => {
  for (const errorAt of ['graphql', 'list', 'repos/acme/tasks/issues/12/sub_issues']) {
    for (const status of [401, 403, 429, 500]) {
      const f = fixture({ errorAt, error: `HTTP ${status}: denied` })
      assert.throws(f.run, new RegExp(`tracker preflight: repository-unavailable:.*HTTP ${status}`))
    }
  }
})

test('malformed issue listing cannot pass as an empty repository', () => {
  for (const issues of [
    null,
    {},
    [{ number: 0 }],
    [{ number: '12' }],
    [{ number: 1 }, { number: 2 }],
  ])
    assert.throws(fixture({ issues }).run, /repository-unavailable/)
})

test('GraphQL errors and unreadable schema fail closed', () => {
  const f = fixture()
  f.ctx.api = () => ({ errors: [{ message: 'HTTP 403: schema denied' }] })
  assert.throws(f.run, /repository-unavailable:.*HTTP 403/)
  f.ctx.api = () => ({ data: { __type: null } })
  assert.throws(f.run, /relations-unavailable/)
})
