#!/usr/bin/env node
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'

import {
  changedGeneratedPackages,
  listTestPackages,
  resolveGeneratedConsumers,
} from './generated-consumers.mjs'
import { moduleRules, selectTargets } from './select-affected-tests.mjs'
import { tempDir } from './test-tmpdir.mjs'

const GEN = 'github.com/recurser/bossalib/gen/bossanova/v1'
const CONNECT = `${GEN}/bossanovav1connect`

// A three-module graph: bossalib owns the generated package, bossd and boss consume it.
// Each entry is one test variant: the package dir and its test build's transitive deps.
const fakeGraph = [
  { dir: 'lib/bossalib/gen/bossanova/v1', forTest: GEN, deps: ['google.golang.org/protobuf'] },
  { dir: 'lib/bossalib/client', forTest: 'x/client', deps: [GEN, CONNECT] },
  { dir: 'lib/bossalib/kit/safego', forTest: 'x/safego', deps: ['context'] },
  { dir: 'services/bossd/internal/session', forTest: 'x/session', deps: [GEN] },
  { dir: 'services/bossd/internal/server', forTest: 'x/server', deps: [`${CONNECT} [x.test]`] },
  { dir: 'services/bossd/internal/clock', forTest: 'x/clock', deps: ['time'] },
  { dir: 'services/boss', forTest: 'x/boss', deps: [GEN] },
]

/** @param {Array<{dir: string, forTest?: string, deps: string[]}>} graph */
function fakeResolver(graph) {
  /** @type {import('./generated-consumers.mjs').ConsumerResolver} */
  return (packages, options) =>
    resolveGeneratedConsumers(packages, { ...options, listPackages: () => graph })
}

/** @param {Array<{target: string, env: Record<string, string>}>} selected */
function byTarget(selected) {
  return Object.fromEntries(
    selected.map(({ target, env }) => [target, env.GO_TEST_PACKAGES ?? 'WHOLE']),
  )
}

const everyModuleWhole = Object.fromEntries(moduleRules.map(({ target }) => [target, 'WHOLE']))

test('a generated service file selects exactly the consumer packages, never web or warehouse', () => {
  const selected = byTarget(
    selectTargets(['lib/bossalib/gen/bossanova/v1/orchestrator.pb.go'], {
      generatedConsumers: fakeResolver(fakeGraph),
    }),
  )
  // internal/server links only the connect package, which this change does not touch.
  assert.deepEqual(selected, {
    'test-bossalib': './client ./gen/bossanova/v1',
    'test-bossd': './internal/session',
    'test-boss': '.',
  })
})

test('a proto change selects consumer packages plus the script and web gates', () => {
  const selected = byTarget(
    selectTargets(['proto/bossanova/v1/orchestrator.proto'], {
      generatedConsumers: fakeResolver(fakeGraph),
    }),
  )
  assert.deepEqual(selected, {
    'test-scripts': 'WHOLE',
    'test-web': 'WHOLE',
    'test-bossalib': './client ./gen/bossanova/v1',
    'test-bossd': './internal/server ./internal/session',
    'test-boss': '.',
  })
})

test('a generated-only change reaches consumers in other modules', () => {
  const selected = byTarget(
    selectTargets(['lib/bossalib/gen/bossanova/v1/bossanovav1connect/orchestrator.connect.go'], {
      generatedConsumers: fakeResolver(fakeGraph),
    }),
  )
  assert.deepEqual(selected, {
    'test-bossalib': './client ./gen/bossanova/v1/bossanovav1connect',
    'test-bossd': './internal/server',
  })
})

test('a whole-module selection elsewhere in the diff wins over package scoping', () => {
  const selected = byTarget(
    selectTargets(['services/bossd/Makefile', 'lib/bossalib/gen/bossanova/v1/models.pb.go'], {
      generatedConsumers: fakeResolver(fakeGraph),
    }),
  )
  assert.equal(selected['test-bossd'], 'WHOLE')
  assert.equal(selected['test-boss'], '.')
})

for (const [name, resolver] of [
  [
    'throws',
    () => {
      throw new Error('go list failed')
    },
  ],
  ['returns null', () => null],
  ['maps no package', fakeResolver([{ dir: 'services/bossd/x', deps: ['time'] }])],
]) {
  test(`a resolver that ${name} selects every Go module whole`, () => {
    for (const file of [
      'lib/bossalib/gen/bossanova/v1/models.pb.go',
      'proto/bossanova/v1/models.proto',
    ]) {
      const selected = byTarget(
        selectTargets([file], {
          generatedConsumers: /** @type {any} */ (resolver),
        }),
      )
      for (const [target, scope] of Object.entries(everyModuleWhole)) {
        assert.equal(selected[target], scope, `${file} ${target}`)
      }
    }
  })
}

test('a diff with no generated input never consults the resolver', () => {
  const selected = selectTargets(['services/bossd/internal/session/lifecycle.go'], {
    generatedConsumers: () => {
      throw new Error('must not be called')
    },
  })
  assert.deepEqual(byTarget(selected), { 'test-bossd': './internal/session' })
})

test('changedGeneratedPackages maps a proto to every generated package and a gen file to its own', () => {
  assert.deepEqual(changedGeneratedPackages(['proto/bossanova/v1/models.proto']), [
    'github.com/recurser/bossalib/gen/...',
  ])
  assert.deepEqual(
    changedGeneratedPackages([
      'lib/bossalib/gen/bossanova/v1/models.pb.go',
      'lib/bossalib/gen/bossanova/v1/BUILD.bazel',
      'services/bossd/main.go',
    ]),
    [GEN],
  )
  assert.deepEqual(changedGeneratedPackages(['proto/bossanova/v1/README.md']), [])
})

// The default lister must read the deps of test variants: a package whose only link to the
// generated code is a _test.go import (internal or external) is a consumer.
test('the go list reader counts imports that only test files make', (t) => {
  const root = tempDir('generated-consumers-', t)
  const write = (/** @type {string} */ rel, /** @type {string} */ body) => {
    fs.mkdirSync(path.dirname(path.join(root, rel)), { recursive: true })
    fs.writeFileSync(path.join(root, rel), body)
  }
  write('go.work', 'go 1.21\n\nuse ./mod\n')
  write('mod/go.mod', 'module example.com/fx\n\ngo 1.21\n')
  write('mod/gen/v1/v1.go', 'package v1\n\nconst Name = "v1"\n')
  write('mod/internaltest/a.go', 'package internaltest\n')
  write(
    'mod/internaltest/a_test.go',
    'package internaltest\n\nimport (\n\t"testing"\n\n\tv1 "example.com/fx/gen/v1"\n)\n\nfunc TestA(t *testing.T) { _ = v1.Name }\n',
  )
  write('mod/externaltest/b.go', 'package externaltest\n')
  write(
    'mod/externaltest/b_test.go',
    'package externaltest_test\n\nimport (\n\t"testing"\n\n\tv1 "example.com/fx/gen/v1"\n)\n\nfunc TestB(t *testing.T) { _ = v1.Name }\n',
  )
  write('mod/leaf/c.go', 'package leaf\n')
  write('mod/leaf/c_test.go', 'package leaf\n\nimport "testing"\n\nfunc TestC(t *testing.T) {}\n')

  const consumers = resolveGeneratedConsumers(['example.com/fx/gen/v1'], {
    modules: [{ root: 'mod/', target: 'test-mod' }],
    listPackages: (roots) => listTestPackages(roots, { repoRoot: root }),
  })
  assert.deepEqual(consumers?.get('mod/'), ['./externaltest', './internaltest'])
})

test('the default resolver finds the live tree consumers of bossanova/v1', () => {
  const selected = byTarget(selectTargets(['lib/bossalib/gen/bossanova/v1/models.pb.go']))
  const packagesOf = (/** @type {string} */ target) => (selected[target] ?? '').split(' ')
  // services/bossd/internal/session imports the generated package directly.
  assert.ok(packagesOf('test-bossd').includes('./internal/session'), selected['test-bossd'])
  // lib/bossalib/kit/safego never links it, even through its tests.
  assert.ok(packagesOf('test-bossalib').includes('./gen/bossanova/v1'), selected['test-bossalib'])
  assert.ok(!packagesOf('test-bossalib').includes('./kit/safego'), selected['test-bossalib'])
  // Every module imports the generated package, so every module is selected.
  for (const { target } of moduleRules) assert.ok(target in selected, target)
})
