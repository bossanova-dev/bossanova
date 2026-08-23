#!/usr/bin/env node

import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDirectory, '..')
const manifestPath = path.join(repoRoot, 'docs/testing/test-command-manifest.md')

export function renderManifestDrift({
  actual,
  expected,
  label = 'docs/testing/test-command-manifest.md',
}) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'test-command-manifest-diff-'))
  const actualPath = path.join(tmp, 'actual')
  const expectedPath = path.join(tmp, 'expected')
  fs.writeFileSync(actualPath, actual)
  fs.writeFileSync(expectedPath, expected)
  const diff = spawnSync(
    'diff',
    ['-u', '--label', label, actualPath, '--label', `${label} (expected)`, expectedPath],
    {
      encoding: 'utf8',
    },
  )
  fs.rmSync(tmp, { recursive: true, force: true })
  return [
    `ERROR: ${label} is out of sync with 'make test-manifest-update':\n`,
    diff.stdout,
    diff.stderr,
    'Run: make test-manifest-update\n',
  ]
    .filter(Boolean)
    .join('')
}

export function checkManifest({
  actual,
  expected,
  label = 'docs/testing/test-command-manifest.md',
}) {
  if (actual === expected) return true
  process.stderr.write(renderManifestDrift({ actual, expected, label }))
  return false
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const expected = execFileSync(process.execPath, ['scripts/generate-test-command-manifest.mjs'], {
    cwd: repoRoot,
    encoding: 'utf8',
  })
  const actual = fs.existsSync(manifestPath) ? fs.readFileSync(manifestPath, 'utf8') : ''
  if (!checkManifest({ actual, expected })) {
    process.exit(1)
  }
}
