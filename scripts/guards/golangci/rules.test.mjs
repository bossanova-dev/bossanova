// Proofs that the depguard boundaries and the session-bootstrap exec.Command ban fire.
// Each runs the pinned
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
  'internal/adapter/git/run.go': BARE_EXEC('git'),
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
  const findings = lintFixture().filter(
    ({ linter, file }) => linter === 'forbidigo' && file === 'internal/session/run.go',
  )
  assert.equal(findings.length, 1, JSON.stringify(findings))
  assert.equal(findings[0].file, 'internal/session/run.go')
  assert.match(findings[0].text, /use of `exec\.Command` forbidden/)
})

test('forbidigo rejects a bare exec.Command in the git adapter', () => {
  const findings = lintFixture().filter(
    ({ linter, file }) => linter === 'forbidigo' && file === 'internal/adapter/git/run.go',
  )
  assert.equal(findings.length, 1, JSON.stringify(findings))
  assert.match(findings[0].text, /use of `exec\.Command` forbidden/)
})

test('neither rule fires on the same code outside its scoped paths', () => {
  const controls = lintFixture().filter(({ file }) =>
    ['cmd/fixture/main.go', 'internal/control/run.go'].includes(file),
  )
  assert.deepEqual(controls, [])
})

// Each nested fixture owns its real module path so Go permits its internal imports.
// The local bossalib stub keeps typechecking hermetic before depguard runs.
let mcpLintResult = null
function lintMcpFixtures() {
  if (mcpLintResult) return mcpLintResult
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'golangci-mcp-rules-'))
  tempRoots.push(root)
  fs.copyFileSync(path.join(repoRoot, '.golangci.yml'), path.join(root, '.golangci.yml'))
  const write = (file, text) => {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }
  write('stub/go.mod', 'module github.com/recurser/bossalib\n\ngo 1.22\n')
  write('stub/gen/bossanova/v1/types.go', 'package bossanovav1\n\nconst Version = 1\n')
  write(
    'stub/gen/bossanova/v1/bossanovav1connect/types.go',
    'package bossanovav1connect\n\nconst Version = 1\n',
  )
  const issues = []
  for (const service of ['mcp', 'mcp-gateway']) {
    const module = `github.com/recurser/bossanova-${service}`
    const prefix = `services/${service}`
    const adapterImport = `import "${module}/internal/adapter/x"\n\nvar Value = x.Version\n`
    const protoImport = [
      'import (',
      '  wire "github.com/recurser/bossalib/gen/bossanova/v1"',
      '  "github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"',
      ')',
      '',
      'var Value = wire.Version + bossanovav1connect.Version',
      '',
    ].join('\n')
    write(
      `${prefix}/go.mod`,
      `module ${module}\n\ngo 1.22\n\nrequire github.com/recurser/bossalib v0.0.0\n\nreplace github.com/recurser/bossalib => ../../stub\n`,
    )
    write(`${prefix}/internal/adapter/x/adapter.go`, 'package x\n\nconst Version = 1\n')
    write(`${prefix}/api/door/door.go`, `package door\n\n${adapterImport}`)
    write(`${prefix}/api/control/control.go`, 'package control\n')
    write(`${prefix}/api/control/control_test.go`, `package control\n\n${adapterImport}`)
    write(`${prefix}/cmd/wiring.go`, `package main\n\n${adapterImport}\nfunc main() {}\n`)
    write(`${prefix}/internal/feature/feature.go`, `package feature\n\n${protoImport}`)
    write(`${prefix}/internal/adapter/wire/wire.go`, `package wire\n\n${protoImport}`)
    write(`${prefix}/internal/control/control.go`, 'package control\n')
    write(`${prefix}/internal/control/control_test.go`, `package control\n\n${protoImport}`)
    const report = path.join(root, `${service}-report.json`)
    const run = spawnSync(
      'golangci-lint',
      [
        'run',
        '--enable-only',
        'depguard',
        `--output.json.path=${report}`,
        '--output.text.path=stderr',
        './...',
      ],
      {
        cwd: path.join(root, prefix),
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
    assert.ok(
      !run.error && (run.status === 0 || run.status === 1),
      `golangci-lint ${service} exited ${run.status}: ${run.error ?? ''}\n${run.stdout}\n${run.stderr}`,
    )
    const reported = JSON.parse(fs.readFileSync(report, 'utf8')).Issues ?? []
    assert.deepEqual(
      reported.filter((issue) => issue.FromLinter !== 'depguard'),
      [],
      `${service}: a typecheck issue means the nested fixture stopped resolving`,
    )
    issues.push(
      ...reported.map((issue) => ({
        service,
        file: issue.Pos.Filename,
        text: issue.Text,
      })),
    )
  }
  mcpLintResult = issues
  return issues
}

const mcpFindings = (rule) =>
  lintMcpFixtures().filter(({ text }) => text.includes(`list '${rule}'`))

test('depguard mcp-doors-no-adapters rejects adapter imports from both MCP doors', () => {
  const findings = mcpFindings('mcp-doors-no-adapters')
  assert.deepEqual(
    findings.map(({ file }) => file).sort(),
    ['services/mcp/api/door/door.go', 'services/mcp-gateway/api/door/door.go'].sort(),
    JSON.stringify(findings),
  )
})

test('depguard mcp-doors-no-adapters permits cmd wiring and test adapter imports', () => {
  const controls = lintMcpFixtures().filter(({ file }) =>
    /\/(cmd\/wiring\.go|api\/control\/control_test\.go)$/.test(file),
  )
  assert.deepEqual(controls, [])
})

test('depguard mcp-proto-at-the-edge rejects wire imports from both MCP features', () => {
  const findings = mcpFindings('mcp-proto-at-the-edge')
  for (const service of ['mcp', 'mcp-gateway']) {
    const moduleFindings = findings.filter((issue) => issue.service === service)
    assert.equal(moduleFindings.length, 2, JSON.stringify(moduleFindings))
    assert.ok(
      moduleFindings.every(
        ({ file }) => file === `services/${service}/internal/feature/feature.go`,
      ),
    )
    assert.ok(moduleFindings.some(({ text }) => text.includes("v1'")))
    assert.ok(moduleFindings.some(({ text }) => text.includes("v1/bossanovav1connect'")))
  }
})

test('depguard mcp-proto-at-the-edge permits adapter and test wire imports', () => {
  const controls = lintMcpFixtures().filter(({ file }) =>
    /\/internal\/(adapter\/wire\/wire\.go|control\/control_test\.go)$/.test(file),
  )
  assert.deepEqual(controls, [])
})
