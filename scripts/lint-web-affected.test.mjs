#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { selectBiomePackages } from './lint-web-affected.mjs'

test('lint-web-affected: workspace dependency updates lint every biome package', () => {
  assert.deepEqual(selectBiomePackages(['pnpm-lock.yaml', 'services/web/package.json']), [
    'services/web',
    'services/marketing',
  ])
})

test('lint-web-affected: a web source change lint only its owning package', () => {
  assert.deepEqual(selectBiomePackages(['services/web/src/App.tsx']), ['services/web'])
})

test('lint-web-affected: make lint invokes the affected Biome gate', () => {
  const repoRoot = path.dirname(fileURLToPath(new URL('../Makefile', import.meta.url)))
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')

  assert.match(makefile, /node scripts\/lint-web-affected\.mjs/)
})
