import assert from 'node:assert/strict'
import { test } from 'node:test'
import { spawnSync } from 'node:child_process'
import { chmodSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { gitFixture } from './verify-git-fixture.mjs'
import { apiSchemaChange, dbtProjectRoots, treeAreaFacts } from './verify-tree-facts.mjs'
import { verifyPolicyFromConfigText } from '../skill-config.mjs'
import { NO_SIGN_OFF_AREA } from './verify-areas.mjs'

function change(t, before, after) {
  const repo = gitFixture(t)
  repo.write(before)
  const mergeBase = repo.commit('base')
  repo.write(after)
  const headSha = repo.commit('head')
  return { repo, cwd: repo.dir, mergeBase, headSha, paths: Object.keys(after), env: { PATH: '' } }
}

test('GraphQL trees classify additions, removals, required inputs and unreadable SDL', (t) => {
  for (const [before, after, expected] of [
    ['type Query { a: Int }', 'type Query { a: Int b: Int }', 'additive'],
    ['type Query { a: Int b: Int }', 'type Query { a: Int }', 'breaking'],
    ['input In { a: Int }', 'input In { a: Int! }', 'breaking'],
    ['type Query { a(x: Int): Int }', 'type Query { a(x: Int!): Int }', 'breaking'],
    ['type Query { a: Int }', 'type Query {', 'breaking'],
  ]) {
    const fixture = change(t, { 'schema.graphql': before }, { 'schema.graphql': after })
    assert.equal(apiSchemaChange(fixture).change, expected)
  }
})

test('GraphQL definitions moved across changed files cancel out', (t) => {
  const fixture = change(
    t,
    { 'a.gql': 'type Query { a: Int }', 'b.gql': 'scalar Date' },
    { 'a.gql': 'scalar Date', 'b.gql': 'type Query { a: Int }' },
  )
  assert.equal(apiSchemaChange(fixture).change, 'additive')
})

test('dbt roots come from either tree, while invalid trees contribute no roots', (t) => {
  const fixture = change(
    t,
    { 'old/dbt_project.yml': 'name: old', 'old/models/x.sql': 'select 1' },
    {
      'old/dbt_project.yml': null,
      'old/models/x.sql': null,
      'new/dbt_project.yml': 'name: new',
      'new/models/x.sql': 'select 2',
    },
  )
  assert.deepEqual(dbtProjectRoots({ ...fixture, shas: [fixture.mergeBase, fixture.headSha] }), [
    'new',
    'old',
  ])
  assert.deepEqual(dbtProjectRoots({ ...fixture, shas: ['unreadable'] }), [])
  assert.equal(
    apiSchemaChange({ ...fixture, mergeBase: 'unreadable', paths: ['schema.gql'] }).change,
    'breaking',
  )
  assert.equal(
    apiSchemaChange({ ...fixture, headSha: 'unreadable', paths: ['schema.gql'] }).change,
    'breaking',
  )
})

test('OpenAPI diff requires readable empty JSON results and handles new/deleted files', (t) => {
  const fixture = change(
    t,
    { 'openapi.yaml': 'openapi: 3.0.0' },
    { 'openapi.yaml': 'openapi: 3.0.1' },
  )
  const bin = path.join(fixture.cwd, 'oasdiff')
  writeFileSync(bin, '#!/bin/sh\nexit 0\n')
  chmodSync(bin, 0o755)
  for (const [stdout, status, expected] of [
    ['[]', 0, 'additive'],
    ['[{"warning":"removed"}]', 0, 'breaking'],
    ['oops', 0, 'breaking'],
    ['[]', 1, 'breaking'],
  ]) {
    const run = (command, args, options) =>
      command === bin ? { status, stdout } : spawnSync(command, args, options)
    assert.equal(apiSchemaChange({ ...fixture, run, env: { OASDIFF: bin } }).change, expected)
  }
  assert.equal(apiSchemaChange(fixture).change, 'breaking')
  const added = change(t, { README: 'base' }, { 'openapi.json': '{}' })
  assert.equal(apiSchemaChange(added).change, 'additive')
  const deleted = change(t, { 'openapi.json': '{}' }, { 'openapi.json': null })
  assert.equal(apiSchemaChange(deleted).change, 'breaking')
})

test('proto facts fail closed without buf and expose violations independently of describers', (t) => {
  const fixture = change(
    t,
    { 'proto/buf.yaml': 'version: v1', 'proto/a.proto': 'syntax = "proto3"; message A {}' },
    { 'proto/a.proto': 'syntax = "proto3"; message B {}' },
  )
  assert.equal(apiSchemaChange(fixture).change, 'breaking')
  const bin = path.join(fixture.cwd, 'buf')
  writeFileSync(bin, '#!/bin/sh\nexit 0\n')
  chmodSync(bin, 0o755)
  const run = (command, args, options) =>
    command === bin
      ? { status: 100, stdout: '{"message":"field removed"}' }
      : spawnSync(command, args, options)
  assert.equal(apiSchemaChange({ ...fixture, run, env: { BUF: bin } }).change, 'breaking')
  const added = change(
    t,
    { README: 'base' },
    { 'proto/buf.yaml': 'version: v1', 'proto/a.proto': 'syntax = "proto3"; message A {}' },
  )
  assert.equal(
    apiSchemaChange({ ...added, paths: ['proto/a.proto'], env: { BUF: bin } }).change,
    'additive',
  )
})

test('trusted describers monotonically classify tree facts and failures', (t) => {
  const fixture = change(
    t,
    { 'schema.gql': 'type Query { a: Int }' },
    { 'schema.gql': 'type Query { a: Int b: Int }' },
  )
  const script = path.join(fixture.cwd, 'describe.mjs')
  writeFileSync(script, '')
  const extension = {
    name: 'custom-api',
    dir: fixture.cwd,
    describe: 'describe.mjs',
    describeAreas: ['api-contract'],
    order: 1,
  }
  const policy = verifyPolicyFromConfigText('{}')
  for (const failure of [false, true]) {
    const run = (command, args, options) =>
      command === process.execPath
        ? {
            status: failure ? 1 : 0,
            stdout: JSON.stringify({
              ok: true,
              role: 'proof-describe',
              extension: extension.name,
              items: ['contract incompatible'],
              breaking: true,
              undisclosed: [],
            }),
          }
        : spawnSync(command, args, options)
    const result = treeAreaFacts(['schema.gql'], policy, {
      ...fixture,
      baseSha: fixture.mergeBase,
      run,
      trustedRoot: fixture.cwd,
      discover: () => ({ extensions: [extension], skipped: [] }),
    })
    const area = result.areas.find((a) => a.area === 'api-contract')
    assert.equal(area.change, 'breaking')
    assert.equal(area.defaultAsk, 'always')
    assert.ok(
      area.changeReasons.some(
        (reason) =>
          reason.includes(extension.name) && reason.includes(failure ? 'failed' : 'breaking'),
      ),
    )
  }
  const fork = treeAreaFacts(['schema.gql'], policy, {
    ...fixture,
    baseSha: fixture.mergeBase,
    trustedRoot: null,
  })
  assert.equal(fork.areas[0].change, 'breaking')
  assert.ok(fork.areas[0].changeReasons.includes('describers unavailable'))
})

test('describers run over the no-sign-off area and their items land on it without an ask', (t) => {
  const fixture = change(t, { 'dbt/models/a.sql': 'select 1' }, { 'dbt/models/a.sql': 'select 2' })
  const script = path.join(fixture.cwd, 'describe.mjs')
  writeFileSync(script, '')
  const extension = {
    name: 'custom-dbt',
    dir: fixture.cwd,
    describe: 'describe.mjs',
    describeAreas: [NO_SIGN_OFF_AREA],
    order: 1,
  }
  const policy = verifyPolicyFromConfigText(
    JSON.stringify({ verifyDefaults: { noSignOff: ['dbt/**'] } }),
  )
  const describers = []
  const run = (command, args, options) => {
    if (command !== process.execPath) return spawnSync(command, args, options)
    describers.push(JSON.parse(options.input).area.area)
    return {
      status: 0,
      stdout: JSON.stringify({
        ok: true,
        role: 'proof-describe',
        extension: extension.name,
        items: ['model a drops a column'],
        breaking: true,
        undisclosed: [],
      }),
    }
  }
  const result = treeAreaFacts(['dbt/models/a.sql'], policy, {
    ...fixture,
    baseSha: fixture.mergeBase,
    run,
    trustedRoot: fixture.cwd,
    discover: () => ({ extensions: [extension], skipped: [] }),
  })
  assert.deepEqual(describers, [NO_SIGN_OFF_AREA])
  assert.deepEqual(
    result.areas.map((a) => [a.area, a.defaultAsk, a.items, a.breaking, a.change]),
    [[NO_SIGN_OFF_AREA, 'never', ['model a drops a column'], true, undefined]],
  )
  const untrusted = treeAreaFacts(['dbt/models/a.sql'], policy, {
    ...fixture,
    baseSha: fixture.mergeBase,
    trustedRoot: null,
  })
  assert.deepEqual(untrusted.areas, [
    {
      area: NO_SIGN_OFF_AREA,
      label: 'No sign-off',
      paths: ['dbt/models/a.sql'],
      defaultAsk: 'never',
      source: 'config',
    },
  ])
})

test('an existing contract blob that cannot be read fails closed', (t) => {
  const fixture = change(
    t,
    { 'schema.gql': 'type Query { a: Int }' },
    { 'schema.gql': 'type Query { a: Int b: Int }' },
  )
  const run = (command, args, options) =>
    command === 'git' && args[0] === 'show'
      ? { status: 1, stdout: '' }
      : spawnSync(command, args, options)
  assert.equal(apiSchemaChange({ ...fixture, run }).change, 'breaking')
})

test('tree classification fetches remote commits before reading schema and dbt facts', (t) => {
  const remote = gitFixture(t)
  remote.write({
    'schema.gql': 'type Query { a: Int }',
    'warehouse/dbt_project.yml': 'name: warehouse',
    'warehouse/models/x.sql': 'select 1',
  })
  const baseSha = remote.commit('base')
  const local = gitFixture(t)
  local.git('remote', 'add', 'origin', remote.dir)
  // Neither commit exists locally when classification starts.
  remote.write({
    'schema.gql': 'type Query { a: Int b: Int }',
    'warehouse/models/x.sql': 'select 2',
  })
  const headSha = remote.commit('head')
  const blockedFetch = (command, args, options) =>
    command === 'git' && args[0] === 'fetch'
      ? { status: 1, stdout: '' }
      : spawnSync(command, args, options)
  const unavailable = treeAreaFacts(
    ['schema.gql', 'warehouse/models/x.sql'],
    verifyPolicyFromConfigText('{}'),
    { baseSha, headSha, cwd: local.dir, run: blockedFetch },
  )
  assert.deepEqual(unavailable.dbtRoots, [])
  assert.equal(
    unavailable.areas.some((area) => area.area === 'migrations'),
    true,
  )
  assert.equal(unavailable.areas.find((area) => area.area === 'api-contract').change, 'breaking')
  const facts = treeAreaFacts(
    ['schema.gql', 'warehouse/models/x.sql'],
    verifyPolicyFromConfigText('{}'),
    {
      baseSha,
      headSha,
      cwd: local.dir,
      trustedRoot: local.dir,
      discover: () => ({ extensions: [], skipped: [] }),
    },
  )
  assert.deepEqual(facts.dbtRoots, ['warehouse'])
  assert.equal(
    facts.areas.some((area) => area.area === 'migrations'),
    false,
  )
  assert.equal(facts.areas.find((area) => area.area === 'api-contract').change, 'additive')
})

test('an existing proto module with unreadable base extraction fails closed', (t) => {
  const fixture = change(
    t,
    { 'buf.yaml': 'version: v1', 'a.proto': 'syntax = "proto3"; message A {}' },
    { 'a.proto': 'syntax = "proto3"; message B {}' },
  )
  const bin = path.join(fixture.cwd, 'buf')
  writeFileSync(bin, '#!/bin/sh\nexit 0\n')
  chmodSync(bin, 0o755)
  const run = (command, args, options) =>
    command === 'git' && args[0] === 'archive'
      ? { status: 1, stdout: '' }
      : spawnSync(command, args, options)
  assert.equal(apiSchemaChange({ ...fixture, run, env: { BUF: bin } }).change, 'breaking')
})
