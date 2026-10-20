// guard-id: go-naming
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

function lintSeed(t, clean) {
  const probe = spawnSync('golangci-lint', ['version'], { encoding: 'utf8' })
  if (probe.error?.code === 'ENOENT') {
    assert.notEqual(process.env.GUARD_PROOF_REQUIRE_TOOLS, '1', 'golangci-lint is required on PATH')
    t.skip('golangci-lint is not on PATH')
    return null
  }
  assert.equal(probe.status, 0, probe.stderr || probe.error?.message)
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'go-naming-proof-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  fs.mkdirSync(path.join(dir, 'seedpkg'))
  fs.writeFileSync(path.join(dir, 'go.mod'), 'module example.com/namingproof\n\ngo 1.26\n')
  fs.writeFileSync(
    path.join(dir, 'seedpkg/seed.go'),
    `package seedpkg

var ${clean ? 'UserID' : 'UserId'} int

type ${clean ? 'Thing' : 'SeedpkgThing'} struct{}

type Receiver struct{}

func (s Receiver) First() {}
func (${clean ? 's' : 't'} Receiver) Second() {}
`,
  )
  const result = spawnSync(
    'golangci-lint',
    ['run', '--config', path.join(repoRoot, '.golangci.yml'), '--allow-parallel-runners', './...'],
    {
      cwd: dir,
      env: { ...process.env, GOWORK: 'off' },
      encoding: 'utf8',
      timeout: 120_000,
    },
  )
  assert.ifError(result.error)
  return { status: result.status, output: result.stdout + result.stderr }
}

test('golangci reports a seeded UserId, mixed receiver name and stutter type', (t) => {
  const result = lintSeed(t, false)
  if (!result) return
  assert.notEqual(result.status, 0, result.output)
  assert.match(result.output, /seedpkg\/seed\.go:3:\d+:.*ST1003/)
  assert.match(result.output, /seedpkg\/seed\.go:(?:9|10):\d+:.*ST1016/)
  assert.match(result.output, /seedpkg\/seed\.go:5:\d+:.*stutters.*\(revive\)/)
})

test('golangci passes the standard-named clean twin', (t) => {
  const result = lintSeed(t, true)
  if (!result) return
  assert.equal(result.status, 0, result.output)
})
