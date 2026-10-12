#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { buildManifest, renderManifest } from './generate-test-command-manifest.mjs'

test('manifest includes the Web Targets section heading', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bosso', target: 'test-bosso' }],
  })

  assert.match(manifest, /## Web Targets \(`services\/web`\)/)
})

// BOS-1083 retired the BOS-33 pointer this used to assert. That reference said
// the real-stack specs were `test.fixme` pending repo-seeding, which stopped
// being true at BOS-374; the row's live prerequisite is now the database, and
// that is what a reader who skips the prose most needs to see.
test('manifest includes pnpm run test:e2e:real row and its database prerequisite', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bosso', target: 'test-bosso' }],
  })

  // Anchored to the ROW, not just the document: the trailing prose also names
  // the variable, so a bare /BOSSO_TEST_DATABASE_URL/ stays green even if the
  // row loses its prerequisite entirely.
  assert.match(manifest, /\|\s*`pnpm run test:e2e:real`\s*\|[^|]*BOSSO_TEST_DATABASE_URL[^|]*\|/)
})

test('manifest includes trailing prose line for Web Targets', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bosso', target: 'test-bosso' }],
  })

  assert.match(manifest, /Run from `services\/web\/`\./)
})

test('manifest links to frontend lint gate guidance', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bosso', target: 'test-bosso' }],
  })

  assert.match(manifest, /\[`docs\/testing\/frontend-lint-gates\.md`\]\(frontend-lint-gates\.md\)/)
})

test('Web Targets heading appears before Go Module Targets heading', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bosso', target: 'test-bosso' }],
  })

  const webIndex = manifest.indexOf('## Web Targets')
  const goIndex = manifest.indexOf('## Go Module Targets')

  assert.ok(webIndex !== -1, 'Web Targets heading not found')
  assert.ok(goIndex !== -1, 'Go Module Targets heading not found')
  assert.ok(webIndex < goIndex, 'Web Targets must appear before Go Module Targets')
})

test('manifest omits the volatile Go test-file count column', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bosso', target: 'test-bosso' }],
  })

  assert.doesNotMatch(manifest, /Test files/)
  assert.match(manifest, /\| Module\s+\| Target\s+\|/)
  assert.doesNotMatch(manifest, /\| `services\/bosso` \| `make test-bosso` \| \d+ \|/)
})

test('buildManifest is invariant to Go test files but changes for new modules', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'test-command-manifest-invariant-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))

  const moduleDirs = ['lib/bossalib', 'services/bossd', 'plugins/bossd-plugin-alpha']
  for (const moduleDir of moduleDirs) {
    fs.mkdirSync(path.join(root, moduleDir), { recursive: true })
    fs.writeFileSync(path.join(root, moduleDir, 'go.mod'), `module example.com/${moduleDir}\n`)
  }

  const before = buildManifest({ root })

  const testFiles = [
    'lib/bossalib/foo_test.go',
    'services/bossd/internal/server/bar_test.go',
    'plugins/bossd-plugin-alpha/pkg/nested/baz_test.go',
  ]
  for (const testFile of testFiles) {
    const absolutePath = path.join(root, testFile)
    fs.mkdirSync(path.dirname(absolutePath), { recursive: true })
    fs.writeFileSync(absolutePath, 'package fixture\n')
  }

  assert.equal(buildManifest({ root }), before)

  fs.mkdirSync(path.join(root, 'services/newmod'), { recursive: true })
  fs.writeFileSync(path.join(root, 'services/newmod/go.mod'), 'module example.com/newmod\n')

  assert.notEqual(buildManifest({ root }), before)
})

test('manifest states where -race runs and points at the budget ratchet', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bossd', target: 'test-bossd' }],
  })

  // The prose is the only place the three-tier race policy is written down for
  // agents; losing it silently is exactly the failure this asserts against.
  assert.match(manifest, /### Where `-race` actually runs \(BOS-1022\)/)
  assert.match(manifest, /`bazel-linux-smoke\.yml`/)
  assert.match(manifest, /`make check-race-budget`/)
  assert.match(manifest, /summed, never maxed/)
  assert.match(manifest, /`services\/bossd\/internal\/dbtest`/)
})
