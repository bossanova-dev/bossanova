import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { runGolangciSync } from './golangci-admission.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')

test('a warm slot cache preserves same-package and dependency-change lint findings', (t) => {
  const pinned = fs
    .readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
    .match(/^GOLANGCI_LINT_VERSION\s*:=\s*v(\S+)/m)?.[1]
  assert.ok(pinned, 'the repository must declare its golangci-lint version')
  const probe = spawnSync('golangci-lint', ['--version'], { encoding: 'utf8' })
  assert.ok(
    !probe.error && probe.status === 0,
    'The warm-cache proof requires golangci-lint on PATH. Install the pinned version with `make deps-golangci`.',
  )
  assert.ok(
    probe.stdout.includes(`version ${pinned} `),
    `The warm-cache proof requires pinned golangci-lint ${pinned}: ${probe.stdout}`,
  )

  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'golangci-warm-cache-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  const write = (file, content) => {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), content)
  }
  fs.copyFileSync(path.join(repoRoot, '.golangci.yml'), path.join(root, '.golangci.yml'))
  write('go.mod', 'module example.com/cacheproof\n\ngo 1.22\n')
  write('dep/dep.go', 'package dep\n\nfunc Execute() {}\n')
  const unchangedCaller = [
    'package use',
    '',
    'import "example.com/cacheproof/dep"',
    '',
    'func Execute() {',
    '\tdep.Execute()',
    '}',
    '',
  ].join('\n')
  write('use/use.go', unchangedCaller)
  write(
    'use/chdir.go',
    'package use\n\nimport "os"\n\nfunc ChangeDir() error { return os.Chdir(".") }\n',
  )

  const env = {
    ...process.env,
    GOWORK: 'off',
    GOFLAGS: '-mod=mod',
    GOPROXY: 'off',
    GOTOOLCHAIN: 'local',
    BOSS_GATE_LINT_CACHE_POOL: '1',
    BOSS_GOLANGCI_SLOT_DIR: path.join(root, 'slots'),
    BOSS_GOLANGCI_SLOTS: '1',
  }
  delete env.GOLANGCI_LINT_CACHE
  delete env.BOSS_GATE_PRIVATE_LINT_CACHE

  const lint = (name, privateCache = false) => {
    const report = path.join(root, `${name}.json`)
    const run = runGolangciSync(
      ['run', `--output.json.path=${report}`, '--output.text.path=stderr', './...'],
      {
        cwd: root,
        encoding: 'utf8',
        privateCache,
        env: privateCache ? { ...env, GOLANGCI_LINT_CACHE: path.join(root, 'cold-cache') } : env,
      },
    )
    assert.ok(
      !run.error && (run.status === 0 || run.status === 1),
      `${name} failed: ${run.error ?? ''}\n${run.stdout}\n${run.stderr}`,
    )
    const issues = JSON.parse(fs.readFileSync(report, 'utf8')).Issues ?? []
    assert.ok(
      issues.every((issue) => issue.FromLinter !== 'typecheck'),
      `${name} fixture must typecheck: ${JSON.stringify(issues)}\n${run.stderr}`,
    )
    const findings = issues.map((issue) => ({
      file: path.isAbsolute(issue.Pos.Filename)
        ? path.relative(root, issue.Pos.Filename)
        : issue.Pos.Filename,
      line: issue.Pos.Line,
      linter: issue.FromLinter,
    }))
    return [
      ...new Map(findings.map((finding) => [JSON.stringify(finding), finding])).values(),
    ].sort(
      (a, b) => a.file.localeCompare(b.file) || a.line - b.line || a.linter.localeCompare(b.linter),
    )
  }

  assert.deepEqual(lint('clean'), [], 'the initial fixture must lint cleanly')
  const pool = path.join(root, 'slots', 'caches', 'slot-0')
  assert.ok(fs.existsSync(pool), 'the clean lint must warm the acquired slot cache')
  assert.ok(fs.readdirSync(pool).length > 0, 'the pool cache must contain lint data')

  write('use/chdir.go', 'package use\n\nimport "os"\n\nfunc ChangeDir() {\n\tos.Chdir(".")\n}\n')
  // Only the dependency changes for this finding: the original caller now discards an error.
  write('dep/dep.go', 'package dep\n\nfunc Execute() error { return nil }\n')
  assert.equal(fs.readFileSync(path.join(root, 'use/use.go'), 'utf8'), unchangedCaller)
  const warm = lint('warm')
  const cold = lint('cold', true)
  assert.deepEqual(warm, cold, 'warm lint findings must equal the fresh private-cache findings')
  for (const file of ['use/chdir.go', 'use/use.go']) {
    assert.ok(
      warm.some(
        (finding) => finding.file === file && finding.line === 6 && finding.linter === 'errcheck',
      ),
      `missing seeded errcheck finding in ${file}: ${JSON.stringify(warm)}`,
    )
  }
  t.diagnostic(`warm and cold findings: ${JSON.stringify(warm)}`)
})
