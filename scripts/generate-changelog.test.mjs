import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { chmodSync, existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')

test('generate-changelog skips cleanly when ANTHROPIC_API_KEY is missing', () => {
  const version = '999.999.999'
  const outPath = resolve(ROOT, 'services/marketing/src/content/changelog', `${version}.md`)
  const tmp = mkdtempSync(join(tmpdir(), 'boss-changelog-test-'))
  const binDir = join(tmp, 'bin')
  const ghPath = join(binDir, 'gh')

  rmSync(outPath, { force: true })
  rmSync(binDir, { recursive: true, force: true })
  mkdirSync(binDir, { recursive: true })
  writeFileSync(
    ghPath,
    `#!/usr/bin/env node
const endpoint = process.argv[3] || ''
if (endpoint.includes('/releases/tags/')) {
  console.log(JSON.stringify({ published_at: '2026-01-01T00:00:00Z' }))
} else if (endpoint.includes('/tags?')) {
  console.log(JSON.stringify([{ name: 'v${version}' }]))
} else if (endpoint.includes('/compare/')) {
  console.log(JSON.stringify({ commits: [] }))
} else {
  console.error('unexpected gh endpoint: ' + endpoint)
  process.exit(1)
}
`,
    { mode: 0o755 },
  )
  chmodSync(ghPath, 0o755)

  try {
    const result = spawnSync(
      process.execPath,
      ['scripts/generate-changelog.mjs', '--version', version],
      {
        cwd: ROOT,
        encoding: 'utf8',
        env: {
          ...process.env,
          ANTHROPIC_API_KEY: '',
          GITHUB_REPOSITORY: 'recurser/bossanova',
          GH_TOKEN: 'fake-token',
          LINEAR_API_KEY: '',
          PATH: `${binDir}:${process.env.PATH || ''}`,
        },
      },
    )
    const output = `${result.stdout}\n${result.stderr}`
    assert.equal(result.status, 0)
    assert.match(output, /skipped: no ANTHROPIC_API_KEY/)
    assert.doesNotMatch(output, /generation failed/)
    assert.equal(existsSync(outPath), false)
  } finally {
    rmSync(outPath, { force: true })
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('generate-changelog skips prerelease versions before any GitHub or model lookup', () => {
  for (const version of ['999.999.999-staging.1', '999.999.999-beta.1']) {
    testPrereleaseSkip(version)
  }
})

function testPrereleaseSkip(version) {
  const outPath = resolve(ROOT, 'services/marketing/src/content/changelog', `${version}.md`)
  const tmp = mkdtempSync(join(tmpdir(), 'boss-changelog-prerelease-test-'))
  const binDir = join(tmp, 'bin')
  const ghPath = join(binDir, 'gh')

  rmSync(outPath, { force: true })
  mkdirSync(binDir, { recursive: true })
  writeFileSync(
    ghPath,
    `#!/usr/bin/env node
console.error('gh must not be called for prerelease changelog versions')
process.exit(1)
`,
    { mode: 0o755 },
  )
  chmodSync(ghPath, 0o755)

  try {
    const result = spawnSync(
      process.execPath,
      ['scripts/generate-changelog.mjs', '--version', version],
      {
        cwd: ROOT,
        encoding: 'utf8',
        env: {
          ...process.env,
          ANTHROPIC_API_KEY: 'sk-fake-key',
          GITHUB_REPOSITORY: 'recurser/bossanova',
          GH_TOKEN: 'fake-token',
          LINEAR_API_KEY: '',
          PATH: `${binDir}:${process.env.PATH || ''}`,
        },
      },
    )
    const output = `${result.stdout}\n${result.stderr}`
    assert.equal(result.status, 0)
    assert.match(output, new RegExp(`skipped: prerelease ${version.replaceAll('.', '\\.')}`))
    assert.doesNotMatch(output, /gh must not be called/)
    assert.doesNotMatch(output, /generation failed/)
    assert.equal(existsSync(outPath), false)
  } finally {
    rmSync(outPath, { force: true })
    rmSync(tmp, { recursive: true, force: true })
  }
}
