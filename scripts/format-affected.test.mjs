#!/usr/bin/env node

/**
 * Tests for scripts/format-affected.mjs
 *
 * Two layers:
 *   - Pure unit tests of the exported `routeFiles` bucket-assignment logic.
 *   - Hermetic integration tests: a throwaway temp git repo + stub formatters
 *     (gofmt / goimports / prettier) that deterministically append a marker line
 *     so assertions are exact. The child is launched with an absolute node path
 *     and a controlled PATH so tool discovery is fully deterministic.
 *
 * The hermetic repo intentionally has NO `origin/main`, which also exercises the
 * graceful-degradation of the branch-diff source (it must not crash — staged +
 * untracked files are still formatted).
 */

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

import { routeFiles } from './format-affected.mjs'

const SCRIPT = path.join(path.dirname(fileURLToPath(import.meta.url)), 'format-affected.mjs')

// PATH with git + coreutils but NO gofmt/goimports/prettier/pnpm.
const MINIMAL_PATH = '/usr/bin:/bin:/usr/local/bin'

const tempDirs = []

function mkTemp(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix))
  tempDirs.push(dir)
  return dir
}

/** Create a fresh repo with an empty initial commit so HEAD exists (no origin/main). */
function initRepo() {
  const dir = mkTemp('fmtaffected-repo-')
  execFileSync('git', ['init', '-q', dir])
  execFileSync('git', ['-C', dir, 'config', 'user.email', 'test@example.com'])
  execFileSync('git', ['-C', dir, 'config', 'user.name', 'Test'])
  execFileSync('git', ['-C', dir, 'config', 'commit.gpgsign', 'false'])
  execFileSync('git', ['-C', dir, 'commit', '--allow-empty', '-q', '-m', 'init'])
  return dir
}

/** Run format-affected.mjs in cwd=repoDir with the given PATH. Returns {code, stdout, stderr}. */
function runScript(repoDir, args = [], pathOverride = MINIMAL_PATH) {
  try {
    const stdout = execFileSync(process.execPath, [SCRIPT, ...args], {
      cwd: repoDir,
      encoding: 'utf8',
      env: {
        HOME: process.env.HOME ?? '/tmp',
        PATH: pathOverride,
      },
      // Capture stderr onto the result instead of letting execFileSync echo the
      // script's diagnostics (e.g. git's "fatal: ambiguous argument
      // origin/main...HEAD" on the missing-base-ref path) to the parent.
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    return { code: 0, stdout, stderr: '' }
  } catch (err) {
    return {
      code: err.status ?? 1,
      stdout: err.stdout?.toString() ?? '',
      stderr: err.stderr?.toString() ?? '',
    }
  }
}

/**
 * Write a stub formatter executable.
 * When called with (-w | --write) <file...> it appends a marker line to each file.
 */
function writeStub(dir, name, marker) {
  const p = path.join(dir, name)
  fs.writeFileSync(
    p,
    `#!/bin/sh\n# stub ${name}: skip flags, append marker to each file arg\nwhile [ "$1" = "-w" ] || [ "$1" = "--write" ]; do shift; done\nfor f in "$@"; do\n  printf '\\n# ${marker}\\n' >> "$f"\ndone\n`,
  )
  fs.chmodSync(p, 0o755)
  return p
}

/**
 * Write a stub `biome` executable. It expects `biome check --write <file...>`,
 * strips the `check`/`--write` tokens, and appends a marker to each remaining arg
 * (resolved relative to the CWD it was launched in — i.e. the package root).
 */
function writeBiomeStub(dir, marker) {
  const p = path.join(dir, 'biome')
  fs.writeFileSync(
    p,
    `#!/bin/sh\n# stub biome: drop 'check' + '--write', append marker to each file arg\n[ "$1" = "check" ] && shift\nwhile [ "$1" = "--write" ]; do shift; done\nfor f in "$@"; do\n  printf '\\n/* ${marker} */\\n' >> "$f"\ndone\n`,
  )
  fs.chmodSync(p, 0o755)
  return p
}

/** Write a stub formatter that always exits non-zero (simulates a real formatter error). */
function writeFailingStub(dir, name, code = 3) {
  const p = path.join(dir, name)
  fs.writeFileSync(p, `#!/bin/sh\nexit ${code}\n`)
  fs.chmodSync(p, 0o755)
  return p
}

after(() => {
  for (const dir of tempDirs) fs.rmSync(dir, { recursive: true, force: true })
})

// ---------------------------------------------------------------------------
// Pure: routeFiles bucket assignment
// ---------------------------------------------------------------------------
test('format-affected: routeFiles routes a mixed list into the right buckets', () => {
  const plan = routeFiles([
    'services/bossd/main.go',
    'services/web/src/app.ts',
    'README.md',
    'services/web/data.json',
    'services/web/src/app.css',
    'services/web/package.json',
    'scripts/foo.mjs',
    'scripts/bar.cjs',
  ])

  assert.deepEqual(plan.go, ['services/bossd/main.go'])
  // Non-biome, non-package.json web-ish files only.
  assert.deepEqual([...plan.prettier].sort(), ['README.md', 'scripts/bar.cjs', 'scripts/foo.mjs'])
  // services/web/ files (incl. its package.json) route to biome, not prettier.
  assert.deepEqual(Object.keys(plan.biome), ['services/web/'])
  assert.deepEqual([...plan.biome['services/web/']].sort(), [
    'services/web/data.json',
    'services/web/package.json',
    'services/web/src/app.css',
    'services/web/src/app.ts',
  ])
  assert.equal(plan.packageJson, true, 'package.json presence must set packageJson flag')
})

test('format-affected: workspace dependency updates reformat every biome package', () => {
  // A Biome upgrade can change formatting even when no TypeScript source changed.
  // Formatting only the manifest leaves pre-existing source drift for CI to find.
  const plan = routeFiles(['pnpm-lock.yaml', 'services/web/package.json'])

  assert.deepEqual(plan.biomeAll, ['services/web/', 'services/marketing/'])
})

test('format-affected: routeFiles routes docs .mdx to prettier (matches services/docs format gate)', () => {
  // services/docs has no own .prettierrc, so it inherits root prettier config;
  // `make format` formats docs/**/*.{md,mdx} with that same prettier. Routing .mdx
  // to the root prettier bucket keeps `make format-affected` from silently dropping
  // a changed .mdx that the later services/docs prettier lint gate would flag.
  const plan = routeFiles(['services/docs/docs/intro.mdx', 'services/docs/docs/intro.md'])
  assert.deepEqual([...plan.prettier].sort(), [
    'services/docs/docs/intro.md',
    'services/docs/docs/intro.mdx',
  ])
})

test('format-affected: routeFiles routes biome-owned dirs to biome and drops their markdown', () => {
  const plan = routeFiles([
    'services/marketing/a.tsx',
    'services/web/readme.md',
    'services/web/x.ts',
  ])

  // .md under a biome-owned dir is dropped (make format does not format it either).
  assert.deepEqual(plan.prettier, [])
  assert.deepEqual(plan.biome['services/web/'], ['services/web/x.ts'])
  assert.deepEqual(plan.biome['services/marketing/'], ['services/marketing/a.tsx'])
})

test('format-affected: routeFiles drops web generated code ignored by biome', () => {
  const plan = routeFiles([
    'services/web/src/gen/bossanova/v1/daemon_pb.ts',
    'services/web/src/app.ts',
  ])

  assert.deepEqual(plan.biome['services/web/'], ['services/web/src/app.ts'])
})

test('format-affected: routeFiles sends a non-web package.json to syncpack only (not prettier)', () => {
  const plan = routeFiles(['package.json', 'services/bossd/go.mod'])
  assert.deepEqual(plan.prettier, [], 'root package.json must not land in the prettier bucket')
  assert.deepEqual(plan.biome, {})
  assert.equal(plan.packageJson, true)
})

test('format-affected: routeFiles routes services/docs/package.json to prettier, not syncpack', () => {
  // services/docs is outside the pnpm workspace, so syncpack skips its manifest;
  // its own `make format` prettier globs `*.json`, so route it to prettier and do
  // NOT set packageJson (avoids a spurious repo-wide syncpack for a docs-only change).
  const plan = routeFiles(['services/docs/package.json'])
  assert.deepEqual(plan.prettier, ['services/docs/package.json'])
  assert.equal(plan.packageJson, false, 'a non-workspace docs manifest must not trigger syncpack')
})

test('format-affected: routeFiles still sends a workspace package.json to syncpack alongside a docs manifest', () => {
  // A workspace package.json (root) sets the syncpack flag; the docs manifest routes
  // to prettier. Both are handled in a single mixed change set.
  const plan = routeFiles(['package.json', 'services/docs/package.json'])
  assert.deepEqual(plan.prettier, ['services/docs/package.json'])
  assert.equal(plan.packageJson, true)
})

test('format-affected: routeFiles on empty input yields empty buckets and no packageJson', () => {
  const plan = routeFiles([])
  assert.deepEqual(plan.go, [])
  assert.deepEqual(plan.prettier, [])
  assert.deepEqual(plan.biome, {})
  assert.equal(plan.packageJson, false)
})

test('format-affected: routeFiles normalizes and dedupes, ignoring empty entries', () => {
  const plan = routeFiles(['./a.go', 'a.go', 'sub\\b.ts', '', '   '])
  assert.deepEqual(plan.go, ['a.go'])
  assert.deepEqual(plan.prettier, ['sub/b.ts'])
})

// ---------------------------------------------------------------------------
// Integration: changed .go formatted by stub gofmt
// ---------------------------------------------------------------------------
test('format-affected: changed .go file is formatted by stub gofmt (exit 0)', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeStub(binDir, 'gofmt', 'stub-gofmt')

  fs.writeFileSync(path.join(repo, 'main.go'), 'package main\nfunc main() {}\n')

  const result = runScript(repo, ['main.go'], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)

  const content = fs.readFileSync(path.join(repo, 'main.go'), 'utf8')
  assert.ok(content.includes('stub-gofmt'), 'gofmt stub marker must be appended')
  assert.match(result.stdout, /format-affected: gofmt\/goimports \(1 file\(s\)\)/)
  assert.doesNotMatch(result.stdout, /prettier/, 'prettier line must not appear when no web files')
})

// ---------------------------------------------------------------------------
// Integration: changed web-ish files formatted by stub prettier
// ---------------------------------------------------------------------------
test('format-affected: changed .ts/.md/.json/.css/.mjs/.cjs files are formatted by stub prettier', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeStub(binDir, 'prettier', 'stub-prettier')

  const files = ['app.ts', 'notes.md', 'data.json', 'style.css', 'helper.mjs', 'legacy.cjs']
  for (const f of files) fs.writeFileSync(path.join(repo, f), 'x\n')

  const result = runScript(repo, files, `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)

  for (const f of files) {
    const content = fs.readFileSync(path.join(repo, f), 'utf8')
    assert.ok(content.includes('stub-prettier'), `${f} must be prettier-formatted`)
  }
  assert.match(result.stdout, /format-affected: prettier --write \(6 file\(s\)\)/)
  assert.doesNotMatch(result.stdout, /gofmt/, 'gofmt line must not appear when no .go files')
})

// ---------------------------------------------------------------------------
// Integration: buckets are skipped entirely when they have no files
// ---------------------------------------------------------------------------
test('format-affected: Go bucket produces no output when no .go changed', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeStub(binDir, 'gofmt', 'stub-gofmt')
  writeStub(binDir, 'prettier', 'stub-prettier')

  fs.writeFileSync(path.join(repo, 'notes.md'), 'hello\n')

  const result = runScript(repo, ['notes.md'], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)

  assert.doesNotMatch(result.stdout, /gofmt/, 'no gofmt output when no .go changed')
  assert.doesNotMatch(result.stdout, /syncpack/, 'no syncpack output when no package.json changed')
  assert.match(result.stdout, /format-affected: prettier --write \(1 file\(s\)\)/)
})

test('format-affected: prettier bucket produces no output when nothing web-ish changed', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeStub(binDir, 'gofmt', 'stub-gofmt')
  writeStub(binDir, 'prettier', 'stub-prettier')

  fs.writeFileSync(path.join(repo, 'main.go'), 'package main\n')

  const result = runScript(repo, ['main.go'], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)
  assert.doesNotMatch(result.stdout, /prettier/, 'no prettier output when nothing web-ish changed')
})

// ---------------------------------------------------------------------------
// Integration: staged + untracked files are picked up when run with NO args
// (this also exercises graceful degradation — the repo has no origin/main).
// ---------------------------------------------------------------------------
test('format-affected: staged + untracked files are picked up with no args', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeStub(binDir, 'prettier', 'stub-prettier')

  // Staged file
  fs.writeFileSync(path.join(repo, 'staged.ts'), 'const a = 1\n')
  execFileSync('git', ['-C', repo, 'add', 'staged.ts'])
  // Untracked file
  fs.writeFileSync(path.join(repo, 'untracked.md'), 'hi\n')

  const result = runScript(repo, [], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)

  assert.ok(
    fs.readFileSync(path.join(repo, 'staged.ts'), 'utf8').includes('stub-prettier'),
    'staged file must be formatted',
  )
  assert.ok(
    fs.readFileSync(path.join(repo, 'untracked.md'), 'utf8').includes('stub-prettier'),
    'untracked file must be formatted',
  )
  assert.match(result.stdout, /format-affected: prettier --write \(2 file\(s\)\)/)
})

// ---------------------------------------------------------------------------
// Integration: a biome-owned (services/web) file is formatted by stub biome,
// run from the package root — NOT routed to prettier.
// ---------------------------------------------------------------------------
test('format-affected: a services/web file is formatted by stub biome, not prettier', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeBiomeStub(binDir, 'stub-biome')
  writeStub(binDir, 'prettier', 'stub-prettier')

  fs.mkdirSync(path.join(repo, 'services/web/src'), { recursive: true })
  const rel = 'services/web/src/app.ts'
  fs.writeFileSync(path.join(repo, rel), 'const a = 1\n')

  const result = runScript(repo, [rel], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)

  const content = fs.readFileSync(path.join(repo, rel), 'utf8')
  assert.ok(content.includes('stub-biome'), 'services/web file must be biome-formatted')
  assert.ok(!content.includes('stub-prettier'), 'services/web file must NOT be prettier-formatted')
  assert.match(
    result.stdout,
    /format-affected: biome check --write \(1 file\(s\) in services\/web\/\)/,
  )
  assert.doesNotMatch(
    result.stdout,
    /prettier/,
    'no prettier line when only a biome-owned file changed',
  )
})

// ---------------------------------------------------------------------------
// Integration: a real formatter error surfaces as a non-zero exit code (the
// differentiating behavior vs the always-exit-0 pre-commit hook).
// ---------------------------------------------------------------------------
test('format-affected: a formatter that errors makes the command exit non-zero', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeFailingStub(binDir, 'gofmt')

  fs.writeFileSync(path.join(repo, 'main.go'), 'package main\n')

  const result = runScript(repo, ['main.go'], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 1, 'a formatter error must produce a non-zero exit')
  assert.match(result.stdout, /format-affected: gofmt\/goimports \(1 file\(s\)\)/)
})

// ---------------------------------------------------------------------------
// Integration: a changed package.json triggers the syncpack route (repo-wide).
// ---------------------------------------------------------------------------
test('format-affected: a changed package.json runs syncpack via stub pnpm', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  // Stub pnpm records every invocation's args to a log file in the cwd.
  const pnpmStub = path.join(binDir, 'pnpm')
  fs.writeFileSync(pnpmStub, `#!/bin/sh\nprintf '%s\\n' "$*" >> pnpm-calls.log\n`)
  fs.chmodSync(pnpmStub, 0o755)

  fs.writeFileSync(path.join(repo, 'package.json'), '{"name":"x"}\n')

  const result = runScript(repo, ['package.json'], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)
  assert.match(result.stdout, /format-affected: syncpack \(package\.json changed\)/)

  const calls = fs.readFileSync(path.join(repo, 'pnpm-calls.log'), 'utf8')
  assert.match(calls, /syncpack format/, 'syncpack format must run')
  assert.match(calls, /syncpack fix/, 'syncpack fix must run')
})

// ---------------------------------------------------------------------------
// Integration: a services/web/package.json fires BOTH buckets (repo-wide
// syncpack AND a full per-package biome format), and syncpack MUST run before biome so the
// final writer matches `make format` (biome last). This ordering is load-bearing
// for CI parity — a future reorder must not silently regress it.
// ---------------------------------------------------------------------------
test('format-affected: a services/web/package.json runs syncpack BEFORE biome', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  const orderLog = path.join(repo, 'order.log') // absolute — biome runs with cwd=services/web

  const pnpmStub = path.join(binDir, 'pnpm')
  fs.writeFileSync(pnpmStub, `#!/bin/sh\nprintf 'syncpack\\n' >> "${orderLog}"\n`)
  fs.chmodSync(pnpmStub, 0o755)

  const biomeStub = path.join(binDir, 'biome')
  fs.writeFileSync(
    biomeStub,
    `#!/bin/sh\n[ "$1" = "check" ] && shift\nwhile [ "$1" = "--write" ]; do shift; done\nprintf 'biome %s\\n' "$*" >> "${orderLog}"\n`,
  )
  fs.chmodSync(biomeStub, 0o755)

  fs.mkdirSync(path.join(repo, 'services/web'), { recursive: true })
  fs.writeFileSync(path.join(repo, 'services/web/package.json'), '{"name":"w"}\n')

  const result = runScript(repo, ['services/web/package.json'], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)

  const order = fs.readFileSync(orderLog, 'utf8').split(/\r?\n/).filter(Boolean)
  assert.ok(order.includes('syncpack'), 'syncpack must run for a changed package.json')
  assert.ok(
    order.includes('biome migrate --write'),
    'Biome config must migrate with the upgraded CLI',
  )
  assert.ok(order.includes('biome .'), 'the full web package must be biome-formatted')
  assert.ok(
    order.indexOf('syncpack') < order.indexOf('biome migrate --write') &&
      order.indexOf('biome migrate --write') < order.lastIndexOf('biome .'),
    `syncpack must run before migration and full formatting (order: ${order.join(',')})`,
  )
})

// ---------------------------------------------------------------------------
// Static/regression: the script never invokes a linter.
// ---------------------------------------------------------------------------
test('format-affected: script source contains no golangci-lint reference', () => {
  const src = fs.readFileSync(SCRIPT, 'utf8')
  assert.ok(!src.includes('golangci-lint'), 'format-affected must never run a linter')
})

// ---------------------------------------------------------------------------
// Integration: a symlink in the changed set is skipped (target unmodified).
// ---------------------------------------------------------------------------
test('format-affected: a changed symlink is skipped; its target is not modified', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeStub(binDir, 'gofmt', 'stub-gofmt')

  const target = 'package main\n// target outside the changed set\n'
  fs.writeFileSync(path.join(repo, 'target.go'), target)
  fs.symlinkSync('target.go', path.join(repo, 'link.go'))

  const result = runScript(repo, ['link.go'], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)

  assert.equal(
    fs.readFileSync(path.join(repo, 'target.go'), 'utf8'),
    target,
    'symlink target must not be written through',
  )
  assert.doesNotMatch(
    result.stdout,
    /gofmt/,
    'skipped symlink must not trigger the gofmt bucket line',
  )
})

// ---------------------------------------------------------------------------
// Integration: a deleted / non-existent path in the changed set never reaches a
// formatter and does not crash.
// ---------------------------------------------------------------------------
test('format-affected: a deleted file in the changed set never reaches a formatter', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  writeStub(binDir, 'gofmt', 'stub-gofmt')

  const result = runScript(repo, ['gone.go'], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)
  assert.doesNotMatch(result.stdout, /gofmt/, 'non-existent path must not trigger the gofmt bucket')
})

// ---------------------------------------------------------------------------
// Integration: empty changed set → exit 0 with the "nothing to format" line.
// ---------------------------------------------------------------------------
test('format-affected: empty changed set prints "nothing to format" and exits 0', () => {
  const repo = initRepo()

  const result = runScript(repo, [], MINIMAL_PATH)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)
  assert.match(result.stdout, /nothing to format/)
})

test('format-affected: locks canonical skill payload writes', () => {
  const repo = initRepo()
  const binDir = mkTemp('fmtaffected-bin-')
  const skill = 'services/boss/internal/skillinstall/skills/boss/SKILL.md'
  fs.mkdirSync(path.join(repo, path.dirname(skill)), { recursive: true })
  fs.writeFileSync(path.join(repo, skill), '# skill\n')
  fs.writeFileSync(
    path.join(binDir, 'prettier'),
    `#!/bin/sh\ntest -f "$PWD/.git/boss-skill-snapshot.lock" || exit 1\nprintf '\\n# locked\\n' >> "$2"\n`,
  )
  fs.chmodSync(path.join(binDir, 'prettier'), 0o755)

  const result = runScript(repo, [skill], `${binDir}:${MINIMAL_PATH}`)
  assert.equal(result.code, 0, `exit 0; stderr: ${result.stderr}`)
  assert.match(fs.readFileSync(path.join(repo, skill), 'utf8'), /# locked/)
})
