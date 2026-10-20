// Proofs that two .golangci.yml rules fire: depguard `plugins-no-host-imports` and the
// forbidigo `^exec\.Command$` ban on the session-bootstrap packages. Each runs the pinned
// golangci-lint against a hermetic fixture module that uses the repo's real config.
//
// This file sits in scripts/guards/golangci/ on purpose: `make test-scripts` globs only
// scripts/guards/*.test.mjs, and the CI job behind it has no golangci-lint. It runs through
// `make test-golangci-rules` instead, which bazel.yml's `go-lint` job calls after installing
// the pinned binary.

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { after, test } from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..')

const PLUGIN_IMPORT = [
  'package main',
  '',
  'import "github.com/recurser/bossd/migrations"',
  '',
  'func main() { println(migrations.Version) }',
  '',
].join('\n')

const BARE_EXEC = (pkg) =>
  [
    `package ${pkg}`,
    '',
    'import "os/exec"',
    '',
    '// Run starts a subprocess with no deadline.',
    'func Run() error { return exec.Command("true").Run() }',
    '',
  ].join('\n')

const FIXTURE = {
  'go.mod': [
    'module example.com/fixture',
    '',
    'go 1.22',
    '',
    'require github.com/recurser/bossd v0.0.0',
    '',
    'replace github.com/recurser/bossd => ./stub',
    '',
  ].join('\n'),
  // The stub module is load-bearing. Without a resolvable github.com/recurser/bossd the
  // importer fails to type-check, golangci-lint reports a `typecheck` error instead, and
  // depguard never runs, so the depguard proof would have nothing to observe. A `replace`
  // onto a local directory resolves under GOPROXY=off with no go.sum entry.
  'stub/go.mod': 'module github.com/recurser/bossd\n\ngo 1.22\n',
  'stub/migrations/migrations.go':
    'package migrations\n\n// Version is all an importer needs.\nconst Version = 1\n',
  // Violations: a plugin importing a host package, and a bare exec.Command on the bootstrap path.
  'plugins/bossd-plugin-fixture/main.go': PLUGIN_IMPORT,
  'internal/session/run.go': BARE_EXEC('session'),
  // Negative controls: the same code outside each rule's scoped paths.
  'cmd/fixture/main.go': PLUGIN_IMPORT,
  'internal/control/run.go': BARE_EXEC('control'),
}

const tempRoots = []
after(() => {
  for (const root of tempRoots) {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

let lintResult = null

// Lints the fixture once and returns golangci-lint's issues. Throws, failing every test,
// when golangci-lint is missing: a skipped proof would read as a pass.
function lintFixture() {
  if (lintResult) return lintResult
  const probe = spawnSync('golangci-lint', ['--version'], { encoding: 'utf8' })
  if (probe.error || probe.status !== 0) {
    assert.fail(
      'golangci-lint is not on PATH, so the golangci rule proofs cannot run. ' +
        'Install the pinned version with `make deps` (or `make deps-golangci`).',
    )
  }

  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'golangci-rules-'))
  tempRoots.push(root)
  fs.copyFileSync(path.join(repoRoot, '.golangci.yml'), path.join(root, '.golangci.yml'))
  for (const [file, text] of Object.entries(FIXTURE)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }

  const report = path.join(root, 'report.json')
  const run = spawnSync(
    'golangci-lint',
    [
      'run',
      '--enable-only',
      'depguard,forbidigo',
      `--output.json.path=${report}`,
      '--output.text.path=stderr',
      './...',
    ],
    {
      cwd: root,
      encoding: 'utf8',
      env: {
        ...process.env,
        GOWORK: 'off',
        GOFLAGS: '-mod=mod',
        GOPROXY: 'off',
        GOTOOLCHAIN: 'local',
        GOLANGCI_LINT_CACHE: path.join(root, '.golangci-cache'),
      },
    },
  )
  // 0 = no issues, 1 = issues found; anything else is a run failure, not a verdict.
  assert.ok(
    run.status === 0 || run.status === 1,
    `golangci-lint run exited ${run.status}:\n${run.stdout}\n${run.stderr}`,
  )
  const issues = JSON.parse(fs.readFileSync(report, 'utf8')).Issues ?? []
  lintResult = issues.map((issue) => ({
    linter: issue.FromLinter,
    file: issue.Pos.Filename,
    text: issue.Text,
  }))
  return lintResult
}

test('the fixture lints cleanly apart from the two rules under test', () => {
  const others = lintFixture().filter(({ linter }) => !['depguard', 'forbidigo'].includes(linter))
  assert.deepEqual(others, [], 'a typecheck issue here means the stub module stopped resolving')
})

test('depguard plugins-no-host-imports rejects a plugin importing a host bossd package', () => {
  const findings = lintFixture().filter(({ linter }) => linter === 'depguard')
  assert.equal(findings.length, 1, JSON.stringify(findings))
  assert.equal(findings[0].file, 'plugins/bossd-plugin-fixture/main.go')
  assert.match(
    findings[0].text,
    /import 'github\.com\/recurser\/bossd\/migrations' is not allowed from list 'plugins-no-host-imports'/,
  )
})

test('forbidigo rejects a bare exec.Command on the session bootstrap path', () => {
  const findings = lintFixture().filter(({ linter }) => linter === 'forbidigo')
  assert.equal(findings.length, 1, JSON.stringify(findings))
  assert.equal(findings[0].file, 'internal/session/run.go')
  assert.match(findings[0].text, /use of `exec\.Command` forbidden/)
})

test('neither rule fires on the same code outside its scoped paths', () => {
  const controls = lintFixture().filter(({ file }) =>
    ['cmd/fixture/main.go', 'internal/control/run.go'].includes(file),
  )
  assert.deepEqual(controls, [])
})
