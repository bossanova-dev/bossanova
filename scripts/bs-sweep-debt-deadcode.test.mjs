import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

function dryRun(target, modules) {
  const fixture = fs.mkdtempSync(path.join(os.tmpdir(), 'debt-deadcode-'))
  try {
    fs.copyFileSync(path.join(repoRoot, 'Makefile'), path.join(fixture, 'Makefile'))
    for (const [directory, modulePath] of Object.entries(modules)) {
      fs.mkdirSync(path.join(fixture, directory), { recursive: true })
      fs.writeFileSync(path.join(fixture, directory, 'go.mod'), 'module ' + modulePath + '\n')
    }
    const result = spawnSync('make', ['-n', '--no-print-directory', target], {
      cwd: fixture,
      encoding: 'utf8',
    })
    assert.equal(result.status, 0, result.stderr)
    return result.stdout.trim()
  } finally {
    fs.rmSync(fixture, { recursive: true, force: true })
  }
}

const modules = {
  'lib/shared': 'example.org/project/shared',
  'lib/other': 'example.org/project/other',
  'services/app': 'example.org/project/app',
  'plugins/bossd-plugin-agent': 'example.org/project/agent',
}

test('library deadcode roots reach every workspace module and filter the library import path', () => {
  const command = dryRun('debt-deadcode-shared', modules)
  assert.equal(
    command,
    "go run golang.org/x/tools/cmd/deadcode@latest -test -filter '^example\\.org/project/shared(/|$)' ./lib/other/... ./lib/shared/... ./services/app/... ./plugins/bossd-plugin-agent/...",
  )
})

test('library deadcode filter is derived from go.mod instead of its directory name', () => {
  const command = dryRun('debt-deadcode-shared', {
    'lib/shared': 'acme.invalid/reusable/api.v2',
    'services/new-consumer': 'acme.invalid/application',
  })
  assert.equal(
    command,
    "go run golang.org/x/tools/cmd/deadcode@latest -test -filter '^acme\\.invalid/reusable/api\\.v2(/|$)' ./lib/shared/... ./services/new-consumer/...",
  )
})

test('service and plugin deadcode retain their module-local analysis', () => {
  assert.equal(
    dryRun('debt-deadcode-app', modules),
    'cd services/app && go run golang.org/x/tools/cmd/deadcode@latest -test ./...',
  )
  assert.equal(
    dryRun('debt-deadcode-agent', modules),
    'cd plugins/bossd-plugin-agent && go run golang.org/x/tools/cmd/deadcode@latest -test ./...',
  )
})
