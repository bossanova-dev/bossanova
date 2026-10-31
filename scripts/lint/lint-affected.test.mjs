import { tempDir } from '../test-infra/test-tmpdir.mjs'
import { after, test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  lintVersions,
  discoverModules,
  deriveDeps,
  configHashOf,
  isModuleCached,
  gcStamps,
  runGolangci,
  cachedLintBanner,
  readLintStamp,
  renderLintStamp,
  summarizeLintFindings,
} from './lint-affected.mjs'

function scaffold() {
  const root = tempDir('lint-affected-')
  const write = (rel, contents) => {
    fs.mkdirSync(path.join(root, path.dirname(rel)), { recursive: true })
    fs.writeFileSync(path.join(root, rel), contents)
  }
  write('lib/bossalib/go.mod', 'module github.com/recurser/bossalib\n')
  write(
    'services/boss/go.mod',
    'module x\n\nrequire github.com/recurser/bossalib v0.0.0\n\nreplace github.com/recurser/bossalib => ../../lib/bossalib\n',
  )
  write(
    'plugins/bossd-plugin-claude/go.mod',
    'module y\n\nreplace github.com/recurser/bossalib => ../../lib/bossalib\n',
  )
  // A directory without a go.mod must be ignored.
  fs.mkdirSync(path.join(root, 'services/notamodule'), { recursive: true })
  return root
}

test('discoverModules finds lib/services/plugins go.mod dirs, sorted', () => {
  const root = scaffold()
  assert.deepEqual(discoverModules(root), [
    'lib/bossalib',
    'plugins/bossd-plugin-claude',
    'services/boss',
  ])
})

test('deriveDeps returns lib/bossalib for a module that replaces it', () => {
  const root = scaffold()
  assert.deepEqual(deriveDeps(root, 'services/boss'), ['lib/bossalib'])
  assert.deepEqual(deriveDeps(root, 'plugins/bossd-plugin-claude'), ['lib/bossalib'])
})

test('deriveDeps returns no deps for bossalib itself', () => {
  const root = scaffold()
  assert.deepEqual(deriveDeps(root, 'lib/bossalib'), [])
})

test('deriveDeps returns no deps when go.mod does not replace bossalib', () => {
  const root = tempDir('lint-affected-nodep-')
  fs.mkdirSync(path.join(root, 'services/lonely'), { recursive: true })
  fs.writeFileSync(path.join(root, 'services/lonely/go.mod'), 'module lonely\n')
  assert.deepEqual(deriveDeps(root, 'services/lonely'), [])
})

// Under-invalidation guard: a module that locally `replace`s a SECOND module
// (beyond bossalib) — as services/bosso replaces ../bossd — is type-checked
// against that module's local source in workspace mode, so its findings depend
// on it. deriveDeps must return the transitive closure of local replaces, not
// just lib/bossalib, or an edit to the depended-on module leaves this one
// reporting a stale `(cached)`.
test('deriveDeps returns the transitive closure of local replaces', () => {
  const root = tempDir('lint-affected-transitive-')
  const write = (rel, contents) => {
    fs.mkdirSync(path.join(root, path.dirname(rel)), { recursive: true })
    fs.writeFileSync(path.join(root, rel), contents)
  }
  write('lib/bossalib/go.mod', 'module github.com/recurser/bossalib\n')
  // bossd → bossalib
  write(
    'services/bossd/go.mod',
    'module github.com/recurser/bossd\n\nreplace github.com/recurser/bossalib => ../../lib/bossalib\n',
  )
  // bosso → bossd (and → bossalib directly)
  write(
    'services/bosso/go.mod',
    'module github.com/recurser/bosso\n\n' +
      'replace github.com/recurser/bossalib => ../../lib/bossalib\n' +
      'replace github.com/recurser/bossd => ../bossd\n',
  )
  // bosso transitively depends on both bossd and bossalib.
  assert.deepEqual(deriveDeps(root, 'services/bosso'), ['lib/bossalib', 'services/bossd'])
  // bossd depends only on bossalib.
  assert.deepEqual(deriveDeps(root, 'services/bossd'), ['lib/bossalib'])
})

// A versioned single-line replace and a trailing // comment must still parse.
test('deriveDeps parses versioned and commented local replaces', () => {
  const root = tempDir('lint-affected-versioned-')
  fs.mkdirSync(path.join(root, 'lib/bossalib'), { recursive: true })
  fs.writeFileSync(path.join(root, 'lib/bossalib/go.mod'), 'module github.com/recurser/bossalib\n')
  fs.mkdirSync(path.join(root, 'services/x'), { recursive: true })
  fs.writeFileSync(
    path.join(root, 'services/x/go.mod'),
    'module x\n\nreplace github.com/recurser/bossalib v0.0.0 => ../../lib/bossalib v1.2.3 // pin\n',
  )
  assert.deepEqual(deriveDeps(root, 'services/x'), ['lib/bossalib'])
})

// Under-invalidation guard: lint runs in Go workspace mode, so a go.work /
// go.work.sum edit (e.g. bumping a workspace-only `replace`) changes the versions
// golangci-lint type-checks against and therefore its findings. The global config
// hash must fold in both workspace files so such an edit invalidates every
// module's stamp, not just .golangci.yml.
test('configHashOf folds in .golangci.yml, go.work, and go.work.sum', () => {
  const root = tempDir('lint-affected-confighash-')
  fs.writeFileSync(path.join(root, '.golangci.yml'), 'linters:\n  enable: [staticcheck]\n')
  fs.writeFileSync(path.join(root, 'go.work'), 'go 1.25\n\nuse ./services/mcp\n')
  fs.writeFileSync(path.join(root, 'go.work.sum'), 'example.com/x v1.0.0 h1:aaa\n')
  const baseline = configHashOf(root)

  // Deterministic for identical inputs.
  assert.equal(baseline, configHashOf(root))

  // A go.work edit (workspace replace bump) must change the hash.
  fs.writeFileSync(
    root + '/go.work',
    'go 1.25\n\nreplace foo => bar v1.2.3\n\nuse ./services/mcp\n',
  )
  const afterWork = configHashOf(root)
  assert.notEqual(baseline, afterWork)

  // A go.work.sum change must change the hash.
  fs.writeFileSync(path.join(root, 'go.work.sum'), 'example.com/x v1.0.1 h1:bbb\n')
  assert.notEqual(afterWork, configHashOf(root))

  // A .golangci.yml change must still change the hash.
  const beforeConfig = configHashOf(root)
  fs.writeFileSync(path.join(root, '.golangci.yml'), 'linters:\n  enable: [staticcheck, gosec]\n')
  assert.notEqual(beforeConfig, configHashOf(root))
})

// An explicit GOWORK file selects workspace directives outside the repository
// root. Its contents (and adjacent go.work.sum) affect type checking, so they
// must invalidate lint stamps even though the cache key deliberately omits the
// machine-specific workspace path.
test('configHashOf folds in an explicit GOWORK file and its sum', () => {
  const root = tempDir('lint-affected-explicit-gowork-root-')
  const workspaceDir = tempDir('lint-affected-explicit-gowork-')
  const workspaceFile = path.join(workspaceDir, 'workspace.work')
  fs.writeFileSync(path.join(root, '.golangci.yml'), 'linters:\n  enable: [staticcheck]\n')
  fs.writeFileSync(workspaceFile, 'go 1.25\n\nuse ./services/mcp\n')
  fs.writeFileSync(path.join(workspaceDir, 'go.work.sum'), 'example.com/x v1.0.0 h1:aaa\n')
  const baseline = configHashOf(root, workspaceFile)

  fs.appendFileSync(workspaceFile, '\nreplace example.com/x => example.com/x v1.0.1\n')
  const afterWork = configHashOf(root, workspaceFile)
  assert.notEqual(baseline, afterWork)

  fs.writeFileSync(path.join(workspaceDir, 'go.work.sum'), 'example.com/x v1.0.1 h1:bbb\n')
  assert.notEqual(afterWork, configHashOf(root, workspaceFile))
})

// The cache-skip gate must fail open: only an enabled + unforced + present stamp
// yields a skip; every other combination routes to a real lint.
test('isModuleCached gates on enabled + !force + stamp presence', () => {
  const dir = tempDir('lint-affected-cached-')
  const present = path.join(dir, 'present')
  fs.writeFileSync(present, 'x')
  const missing = path.join(dir, 'missing')
  assert.equal(isModuleCached({ stampsEnabled: true, force: false, stampPath: present }), true)
  assert.equal(isModuleCached({ stampsEnabled: true, force: true, stampPath: present }), false)
  assert.equal(isModuleCached({ stampsEnabled: false, force: false, stampPath: present }), false)
  assert.equal(isModuleCached({ stampsEnabled: true, force: false, stampPath: missing }), false)
})

// GC drops stamps past the 30-day TTL, keeps fresh ones, and no-ops on a missing
// dir (fail-open bounded growth).
test('gcStamps deletes only stale stamps and tolerates a missing dir', () => {
  const dir = tempDir('lint-affected-gc-')
  const stale = path.join(dir, 'stale')
  const fresh = path.join(dir, 'fresh')
  fs.writeFileSync(stale, 'x')
  fs.writeFileSync(fresh, 'x')
  const old = Date.now() / 1000 - 31 * 24 * 60 * 60 // 31 days ago, in seconds
  fs.utimesSync(stale, old, old)
  gcStamps(dir)
  assert.equal(fs.existsSync(stale), false)
  assert.equal(fs.existsSync(fresh), true)
  // Missing dir must not throw.
  gcStamps(path.join(dir, 'does-not-exist'))
})

// --- runGolangci: admitted single invocation --------
//
// A stub `golangci-lint` executable, prepended to PATH, so these tests never
// invoke the real linter. Each invocation appends a line to counterFile,
// giving the test an independent invocation count.

const stubDirs = []

after(() => {
  for (const dir of stubDirs) fs.rmSync(dir, { recursive: true, force: true })
})

function makeStubDir(prefix) {
  const dir = tempDir(prefix)
  stubDirs.push(dir)
  return dir
}

function writeGolangciStub(dir, script) {
  const stubPath = path.join(dir, 'golangci-lint')
  fs.writeFileSync(stubPath, script, { mode: 0o755 })
  return stubPath
}

function invocationCount(counterFile) {
  return fs
    .readFileSync(counterFile, 'utf8')
    .split('\n')
    .filter((line) => line.length > 0).length
}

// Runs `fn` with process.stdout/stderr writes captured into SEPARATE returned
// buffers instead of hitting the real terminal, and with `dir` prepended to
// PATH so the stub resolves before any real golangci-lint on the box. Restores
// both afterward even if `fn` throws.
//
// Keep findings on stdout and diagnostics on stderr to prove capture re-emits
// each stream without mixing or swallowing output.
function withCapturedOutput(dir, fn) {
  const originalPath = process.env.PATH
  const originalStdoutWrite = process.stdout.write.bind(process.stdout)
  const originalStderrWrite = process.stderr.write.bind(process.stderr)
  let stdout = ''
  let stderr = ''
  const originalSlots = process.env.BOSS_GOLANGCI_SLOT_DIR
  process.env.BOSS_GOLANGCI_SLOT_DIR = path.join(dir, 'slots')
  process.env.PATH = `${dir}${path.delimiter}${originalPath}`
  process.stdout.write = (chunk) => {
    stdout += chunk
    return true
  }
  process.stderr.write = (chunk) => {
    stderr += chunk
    return true
  }
  try {
    const result = fn()
    return { result, stdout, stderr }
  } finally {
    if (originalSlots === undefined) delete process.env.BOSS_GOLANGCI_SLOT_DIR
    else process.env.BOSS_GOLANGCI_SLOT_DIR = originalSlots
    process.env.PATH = originalPath
    process.stdout.write = originalStdoutWrite
    process.stderr.write = originalStderrWrite
  }
}

test('runGolangci propagates contention without retrying', () => {
  const dir = makeStubDir('lint-single-run-')
  const counter = path.join(dir, 'counter')
  writeGolangciStub(
    dir,
    `#!/bin/sh\necho run >> "${counter}"\necho 'parallel golangci-lint is running' >&2\nexit 7\n`,
  )
  const { result, stderr } = withCapturedOutput(dir, () => runGolangci({ cwd: dir }))
  assert.equal(result.status, 7)
  assert.equal(invocationCount(counter), 1)
  assert.match(stderr, /parallel golangci-lint is running/)
})

// The load-bearing case: without the invocation-count assertion this would
// pass against a script that blindly retries every non-zero exit, which is
// exactly the vacuous-gate shape BOS-736 exists to eliminate.
test('runGolangci does not retry a real lint finding (invocation count stays 1)', () => {
  const stubDir = makeStubDir('lint-affected-stub-finding-')
  const counterFile = path.join(stubDir, 'counter')
  fs.writeFileSync(counterFile, '')
  writeGolangciStub(
    stubDir,
    `#!/usr/bin/env bash
echo run >> "${counterFile}"
echo "main.go:1:1: undefined: foo"
exit 1
`,
  )
  const { result: outcome, stdout } = withCapturedOutput(stubDir, () =>
    runGolangci({ cwd: stubDir }),
  )
  assert.equal(outcome.status, 1)
  assert.equal(invocationCount(counterFile), 1)
  // The finding golangci-lint printed on stdout must come back on stdout.
  assert.match(stdout, /main\.go:1:1: undefined: foo/)
})

test('runGolangci survives golangci output larger than spawnSync default maxBuffer', () => {
  const stubDir = makeStubDir('lint-affected-stub-bigout-')
  const counterFile = path.join(stubDir, 'counter')
  fs.writeFileSync(counterFile, '')
  // 2 MB of findings — over spawnSync's 1 MiB default maxBuffer. Without an
  // explicit maxBuffer the child is SIGTERMed, output is truncated, and
  // spawnSync sets an ENOBUFS error that runGolangci rethrows, so the caller
  // sees "ENOBUFS" instead of the lint findings that actually failed the gate.
  writeGolangciStub(
    stubDir,
    `#!/usr/bin/env bash
echo run >> "${counterFile}"
head -c 2000000 /dev/zero | tr '\\0' 'x'
echo
echo "main.go:1:1: undefined: foo"
exit 1
`,
  )
  const { result: outcome, stdout } = withCapturedOutput(stubDir, () =>
    runGolangci({ cwd: stubDir }),
  )
  assert.equal(outcome.status, 1)
  assert.equal(invocationCount(counterFile), 1)
  // The real finding must survive the capture, not be truncated away.
  assert.match(stdout, /main\.go:1:1: undefined: foo/)
})

// The in-process tests above capture output by monkeypatching
// process.stdout.write, so their assertions are evaluated against a synchronous
// string concat and NO real file descriptor is ever crossed. That cannot see the
// one way this module can still lose output: on POSIX, Node's stdout/stderr are
// asynchronous when they are PIPES, and process.exit() does not flush pending
// writes — so exiting right after re-emitting a large captured buffer truncates
// it at the ~64 KiB pipe buffer. On a TTY the streams are synchronous and the
// loss is invisible, which is exactly why an eyeball check misses it.
//
// This case therefore drives the real CLI as a SUBPROCESS: spawnSync gives the
// child pipes for stdout/stderr, reproducing CI, `| tee`, and every agent
// harness. It is the only test here that would fail against `process.exit()`.
test('the CLI does not truncate large golangci output when stdout is a pipe', () => {
  const stubDir = makeStubDir('lint-affected-stub-pipeflush-')
  const stampDir = makeStubDir('lint-affected-stamps-pipeflush-')
  // 3 MB of findings, then a sentinel as the LAST line. Anything that truncates
  // at the pipe buffer drops the sentinel while keeping the head of the output,
  // so asserting on the tail is what makes this falsifiable.
  // main() also calls `golangci-lint --version` (via execFileSync, whose default
  // 1 MiB maxBuffer this stub's bulk output would blow), so the stub MUST answer
  // that probe cheaply. Without this branch the run dies with ENOBUFS and the
  // assertion below still fails — but for an unrelated reason, which would make
  // the test look falsifiable while proving nothing about truncation.
  writeGolangciStub(
    stubDir,
    `#!/usr/bin/env bash
if [ "$1" = "--version" ]; then echo "golangci-lint has version 0.0.0-stub"; exit 0; fi
head -c 3000000 /dev/zero | tr '\\0' 'x'
echo
echo "main.go:1:1: undefined: sentinelPastThePipeBuffer"
exit 1
`,
  )
  const scriptPath = fileURLToPath(new URL('./lint-affected.mjs', import.meta.url))
  const run = spawnSync(process.execPath, [scriptPath, '--module', 'lib/bossalib'], {
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
    env: {
      ...process.env,
      PATH: `${stubDir}${path.delimiter}${process.env.PATH}`,
      // A fresh stamp dir guarantees a cache miss, so golangci actually runs.
      BOSS_LINT_STAMP_DIR: stampDir,
      BOSS_GOLANGCI_SLOT_DIR: path.join(stubDir, 'slots'),
    },
  })
  assert.equal(run.status, 1, 'a lint finding must still exit non-zero')
  assert.match(
    run.stdout,
    /main\.go:1:1: undefined: sentinelPastThePipeBuffer/,
    'output past the pipe buffer was truncated — the gate reported less than it produced',
  )
})

// --- BOS-1276: a cached green must name what it is replaying -----------------------------------

test('summarizeLintFindings counts golangci finding lines and nothing else', () => {
  assert.equal(summarizeLintFindings(''), '0 issues')
  assert.equal(summarizeLintFindings(null), '0 issues')
  assert.equal(
    summarizeLintFindings('internal/foo/bar.go:12:3: ineffectual assignment to err (ineffassign)'),
    '1 issue',
  )
  assert.equal(
    summarizeLintFindings(
      [
        'internal/foo/bar.go:12:3: ineffectual assignment to err (ineffassign)',
        'internal/foo/baz.go:4:1: exported func Foo should have comment (revive)',
      ].join('\n'),
    ),
    '2 issues',
  )
  // Prose that merely mentions the shape mid-line is not a finding: the pattern is line-anchored.
  assert.equal(
    summarizeLintFindings('level=info msg="see internal/foo/bar.go:12:3: for details"'),
    '0 issues',
  )
})

test('a stamp round-trips its summary and timestamp', () => {
  const text = renderLintStamp({ summary: '0 issues', at: '2026-09-20T12:00:00.000Z' })
  assert.match(text, /^2026-09-20T12:00:00\.000Z\nsummary=0 issues\n$/)
  assert.deepEqual(readLintStamp(text), { at: '2026-09-20T12:00:00.000Z', summary: '0 issues' })
})

test('the cached branch replays the stored per-module summary', () => {
  const stampText = renderLintStamp({ summary: '0 issues', at: '2026-09-20T12:00:00.000Z' })
  const banner = cachedLintBanner({ module: 'services/boss', stampText })
  assert.equal(
    banner,
    '==> Linting services/boss (cached: 0 issues, linted 2026-09-20T12:00:00.000Z)',
  )
  // The distinguishing property: a cached green no longer reads the same as never-linted.
  assert.notEqual(banner, '==> Linting services/boss (cached)')
})

// The on-disk stamp format is machine-wide and shared across worktrees, so a stamp written before
// this change MUST degrade rather than crash every checkout that shares the stamp dir.
test('a stamp written without a summary degrades to the existing banner instead of throwing', () => {
  for (const stampText of [
    '2026-09-06T02:56:17.000Z\n',
    '',
    null,
    undefined,
    'not-a-timestamp-at-all\n',
    'summary=\n',
  ]) {
    assert.equal(
      cachedLintBanner({ module: 'services/boss', stampText }),
      '==> Linting services/boss (cached)',
      `stamp: ${JSON.stringify(stampText)}`,
    )
  }
  assert.deepEqual(readLintStamp('2026-09-06T02:56:17.000Z\n'), {
    at: '2026-09-06T02:56:17.000Z',
    summary: null,
  })
})

test('a summary with no parseable timestamp still replays the summary', () => {
  assert.equal(
    cachedLintBanner({ module: 'lib/bossalib', stampText: 'summary=3 issues\n' }),
    '==> Linting lib/bossalib (cached: 3 issues)',
  )
})

test('lint stamps change with dupl baseline and pin but ignore unrelated Makefile edits', () => {
  const root = scaffold()
  fs.mkdirSync(path.join(root, 'scripts/guards'), { recursive: true })
  fs.writeFileSync(path.join(root, 'Makefile'), 'DUPL_PKG := github.com/mibk/dupl@v1.1.0\n')
  fs.writeFileSync(path.join(root, 'scripts/guards/go-dupl-baseline.json'), 'first')
  const initial = configHashOf(root)
  fs.writeFileSync(path.join(root, 'scripts/guards/go-dupl-baseline.json'), 'second')
  const changedBaseline = configHashOf(root)
  assert.notEqual(initial, changedBaseline)
  fs.writeFileSync(path.join(root, 'Makefile'), 'DUPL_PKG := github.com/mibk/dupl@v1.2.0\n')
  const changedPin = configHashOf(root)
  assert.notEqual(changedBaseline, changedPin)
  fs.appendFileSync(path.join(root, 'Makefile'), '\n# unrelated build change\n')
  assert.equal(changedPin, configHashOf(root))
  fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
})

test('a dupl finding fails the real lint CLI without writing a content stamp', () => {
  const stubDir = makeStubDir('lint-dupl-stubs-')
  const stampDir = makeStubDir('lint-dupl-stamps-')
  writeGolangciStub(
    stubDir,
    `#!/usr/bin/env bash
if [ "$1" = "--version" ]; then echo stub; fi
exit 0
`,
  )
  fs.writeFileSync(
    path.join(stubDir, 'go'),
    `#!${process.execPath}
if(process.argv[2]==='version') console.log('go version stub')
else if(process.argv[2]==='env') console.log('darwin\\narm64\\n1\\n\\noff')
else console.log('found 2 clones:\\n  main.go:1,20\\n  main.go:40,60\\nFound total 1 clone groups.')
`,
    { mode: 0o755 },
  )
  const run = spawnSync(
    process.execPath,
    [
      fileURLToPath(new URL('./lint-affected.mjs', import.meta.url)),
      '--module',
      'plugins/bossd-plugin-dependabot',
      '--force',
    ],
    {
      encoding: 'utf8',
      env: {
        ...process.env,
        PATH: `${stubDir}${path.delimiter}${process.env.PATH}`,
        BOSS_LINT_STAMP_DIR: stampDir,
        BOSS_GOLANGCI_SLOT_DIR: path.join(stubDir, 'slots'),
      },
    },
  )
  assert.equal(run.status, 1, run.stderr)
  assert.match(run.stdout + run.stderr, /main\.go\|main\.go.*measured 1 > baselined 0/)
  assert.deepEqual(fs.readdirSync(stampDir), [])
})

test('runGolangci uses pool runners only without a caller cache', () => {
  const dir = makeStubDir('lint-private-cache-')
  writeGolangciStub(dir, '#!/bin/sh\nprintf "%s\\n" "$@"\n')
  const saved = {
    cache: process.env.GOLANGCI_LINT_CACHE,
    marker: process.env.BOSS_GATE_LINT_CACHE_POOL,
  }
  try {
    for (const [cache, marker, parallel] of [
      [undefined, '1', true],
      ['/caller', '1', false],
      ['/caller', undefined, false],
      [undefined, undefined, false],
    ]) {
      for (const [key, value] of [
        ['GOLANGCI_LINT_CACHE', cache],
        ['BOSS_GATE_LINT_CACHE_POOL', marker],
      ]) {
        if (value === undefined) delete process.env[key]
        else process.env[key] = value
      }
      const { stdout, result } = withCapturedOutput(dir, () => runGolangci({ cwd: dir }))
      assert.equal(result.status, 0)
      assert.equal(stdout.includes('--allow-parallel-runners'), parallel)
      assert.equal(stdout.includes('--allow-serial-runners'), !parallel)
    }
  } finally {
    for (const [key, value] of [
      ['GOLANGCI_LINT_CACHE', saved.cache],
      ['BOSS_GATE_LINT_CACHE_POOL', saved.marker],
    ]) {
      if (value === undefined) delete process.env[key]
      else process.env[key] = value
    }
  }
})

test('Go complexity stamp inputs ignore unrelated budgets and canonicalize guard keys', () => {
  const root = scaffold()
  fs.mkdirSync(path.join(root, 'scripts/guards'), { recursive: true })
  const file = path.join(root, 'scripts/guards/budgets.json')
  const entries = {
    'go-cognitive-complexity': { default: 30, subjects: { 'a:Read': 31, 'b:Write': 32 } },
    'go-interface-width': { default: 10, subjects: {} },
  }
  fs.writeFileSync(file, JSON.stringify(entries))
  const initial = configHashOf(root)
  fs.writeFileSync(file, JSON.stringify({ ...entries, unrelated: { default: 999 } }))
  assert.equal(configHashOf(root), initial)
  entries['go-cognitive-complexity'].subjects = { 'b:Write': 32, 'a:Read': 31 }
  fs.writeFileSync(file, JSON.stringify(entries))
  assert.equal(configHashOf(root), initial)
  entries['go-cognitive-complexity'].subjects['a:Read'] = 32
  fs.writeFileSync(file, JSON.stringify(entries))
  assert.notEqual(configHashOf(root), initial)
  const changedBudget = configHashOf(root)
  fs.writeFileSync(path.join(root, 'scripts/guards/go-returned-interfaces-baseline.json'), '[]')
  assert.notEqual(configHashOf(root), changedBudget)
  const changedBaseline = configHashOf(root)
  fs.writeFileSync(path.join(root, 'scripts/guards/go-complexity.mjs'), '// changed checker')
  assert.notEqual(configHashOf(root), changedBaseline)
})

test('lint stamp versions follow the resolved Go environment', (t) => {
  const dir = tempDir('lint-versions-', t)
  fs.writeFileSync(
    path.join(dir, 'go'),
    '#!/bin/sh\nif [ "$1" = version ]; then echo "go version ${GOTOOLCHAIN:-go1.28.1} fake/arch"; else printf "linux\\namd64\\n1\\n\\noff\\n"; fi\n',
    { mode: 0o755 },
  )
  fs.writeFileSync(path.join(dir, 'golangci-lint'), '#!/bin/sh\necho linter\n', { mode: 0o755 })
  const env = { ...process.env, PATH: dir + path.delimiter + process.env.PATH, GOTOOLCHAIN: '' }
  const local = lintVersions({ env })
  const pinned = lintVersions({ env: { ...env, GOTOOLCHAIN: 'go1.26.5' } })
  assert.match(local.go, /go1\.28\.1/)
  assert.match(pinned.go, /go1\.26\.5/)
  assert.notDeepEqual(local, pinned)
})
