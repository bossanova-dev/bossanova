#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'
import { renderManifest } from './generate-test-command-manifest.mjs'

test('manifest includes the Web Targets section heading', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bosso', target: 'test-bosso' }],
  })

  assert.match(manifest, /## Web Targets \(`services\/web`\)/)
})

test('manifest includes pnpm run test:e2e:real row and BOS-33 reference', () => {
  const manifest = renderManifest({
    rootTargets: ['test-smoke'],
    modules: [{ path: 'services/bosso', target: 'test-bosso' }],
  })

  assert.match(manifest, /`pnpm run test:e2e:real`/)
  assert.match(manifest, /docs\/plans\/BOS-33/)
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
