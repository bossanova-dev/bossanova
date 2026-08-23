#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import {
  deriveModuleTargets,
  discoverModuleTargets,
  renderManifest,
} from './generate-test-command-manifest.mjs'
import { renderManifestDrift } from './check-test-command-manifest.mjs'

test('manifest includes the agent command ladder', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke', 'test-affected', 'test-full', 'test-race', 'test-profile'],
    modules: [{ path: 'services/bossd', target: 'test-bossd', testFiles: 42 }],
  })

  assert.match(manifest, /make test-smoke/)
  assert.match(manifest, /make test-affected/)
  assert.match(manifest, /make test-bossd/)
  assert.match(manifest, /services\/bossd/)
  assert.doesNotMatch(manifest, /Test files/)
})

test('module targets derive plugin entries from go.mod layout', () => {
  assert.deepEqual(
    deriveModuleTargets([
      'services/bossd/go.mod',
      'plugins/bossd-plugin-example/go.mod',
      'lib/bossalib/go.mod',
    ]),
    [
      { path: 'lib/bossalib', target: 'test-bossalib' },
      { path: 'services/bossd', target: 'test-bossd' },
      { path: 'plugins/bossd-plugin-example', target: 'test-example' },
    ],
  )
})

test('module target discovery scans Makefile module roots', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'test-command-manifest-'))
  for (const modulePath of [
    'plugins/bossd-plugin-zeta',
    'services/bossd',
    'lib/bossalib',
    'plugins/bossd-plugin-alpha',
  ]) {
    fs.mkdirSync(path.join(repoRoot, modulePath), { recursive: true })
    fs.writeFileSync(path.join(repoRoot, modulePath, 'go.mod'), 'module example\n')
  }

  assert.deepEqual(discoverModuleTargets(repoRoot), [
    { path: 'lib/bossalib', target: 'test-bossalib' },
    { path: 'services/bossd', target: 'test-bossd' },
    { path: 'plugins/bossd-plugin-alpha', target: 'test-alpha' },
    { path: 'plugins/bossd-plugin-zeta', target: 'test-zeta' },
  ])
})

test('manifest checker prints a diff and remedy when stale', () => {
  const stderr = renderManifestDrift({
    actual: '# Test Command Manifest\n\nold\n',
    expected: '# Test Command Manifest\n\nnew\n',
  })

  assert.match(stderr, /ERROR: docs\/testing\/test-command-manifest\.md/)
  assert.match(stderr, /make test-manifest-update/)
  assert.match(stderr, /^--- docs\/testing\/test-command-manifest\.md/m)
  assert.match(stderr, /^\+new$/m)
})
