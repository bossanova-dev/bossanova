import { after, test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  discoverModules,
  deriveDeps,
  configHashOf,
  isModuleCached,
  gcStamps,
  runGolangci,
  LOCK_CONTENTION_EXHAUSTED_MESSAGE,
} from './lint-affected.mjs'

function scaffold() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-'))
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
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-nodep-'))
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
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-transitive-'))
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
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-versioned-'))
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
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-confighash-'))
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
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-explicit-gowork-root-'))
  const workspaceDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-explicit-gowork-'))
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
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-cached-'))
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
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-gc-'))
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

// --- runGolangci: bounded retry on lock contention (BOS-736 Task 4) --------
//
// A stub `golangci-lint` executable, prepended to PATH, so these tests never
// invoke the real linter. Each invocation appends a line to counterFile,
// giving the test an exact, independent invocation count (never trust the
// returned `attempts` alone — that's the thing under test).

const stubDirs = []

after(() => {
  for (const dir of stubDirs) fs.rmSync(dir, { recursive: true, force: true })
})

function makeStubDir(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix))
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
// The two streams are kept apart deliberately: a single combined buffer would
// let an assertion about the exhausted-contention message pass even if that
// message were written to stdout, and CLAUDE.md tells agents to grep for it on
// stderr. Same for the "re-emitted verbatim" claim — stdout must come back on
// stdout and stderr on stderr, not merged.
function withCapturedOutput(dir, fn) {
  const originalPath = process.env.PATH
  const originalStdoutWrite = process.stdout.write.bind(process.stdout)
  const originalStderrWrite = process.stderr.write.bind(process.stderr)
  let stdout = ''
  let stderr = ''
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
    process.env.PATH = originalPath
    process.stdout.write = originalStdoutWrite
    process.stderr.write = originalStderrWrite
  }
}

test('runGolangci retries lock contention and succeeds once it clears (invocation count N+1)', () => {
  const stubDir = makeStubDir('lint-affected-stub-retry-')
  const counterFile = path.join(stubDir, 'counter')
  fs.writeFileSync(counterFile, '')
  // Contention on the first 2 invocations, succeeds on the 3rd.
  writeGolangciStub(
    stubDir,
    `#!/usr/bin/env bash
echo run >> "${counterFile}"
count=$(wc -l < "${counterFile}")
if [ "$count" -le 2 ]; then
  echo "Error: parallel golangci-lint is running" 1>&2
  exit 1
fi
exit 0
`,
  )
  const { result: outcome, stderr } = withCapturedOutput(stubDir, () =>
    runGolangci({ cwd: stubDir, attempts: 3, backoffMs: 1 }),
  )
  assert.equal(outcome.status, 0)
  assert.equal(invocationCount(counterFile), 3) // N=2 contended attempts + 1 success
  // The contention output golangci-lint printed on stderr must still reach the
  // user on stderr — capture must not swallow or re-route it.
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
    runGolangci({ cwd: stubDir, attempts: 3, backoffMs: 1 }),
  )
  assert.equal(outcome.status, 1)
  assert.equal(invocationCount(counterFile), 1)
  // The finding golangci-lint printed on stdout must come back on stdout.
  assert.match(stdout, /main\.go:1:1: undefined: foo/)
})

test('runGolangci exhausts retries on persistent contention and exits non-zero with a distinct message', () => {
  const stubDir = makeStubDir('lint-affected-stub-exhausted-')
  const counterFile = path.join(stubDir, 'counter')
  fs.writeFileSync(counterFile, '')
  writeGolangciStub(
    stubDir,
    `#!/usr/bin/env bash
echo run >> "${counterFile}"
echo "Error: parallel golangci-lint is running" 1>&2
exit 1
`,
  )
  const {
    result: outcome,
    stdout,
    stderr,
  } = withCapturedOutput(stubDir, () => runGolangci({ cwd: stubDir, attempts: 3, backoffMs: 1 }))
  assert.notEqual(outcome.status, 0)
  assert.equal(invocationCount(counterFile), 3)
  // CLAUDE.md documents this literal as a stderr grep token, so assert the
  // stream as well as the text.
  assert.match(
    stderr,
    new RegExp(LOCK_CONTENTION_EXHAUSTED_MESSAGE.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
  )
  assert.doesNotMatch(stdout, /lock contention exhausted/)
})

test('runGolangci exhausted message reports the attempts actually made, not a hard-coded 3', () => {
  const stubDir = makeStubDir('lint-affected-stub-attempts-')
  const counterFile = path.join(stubDir, 'counter')
  fs.writeFileSync(counterFile, '')
  writeGolangciStub(
    stubDir,
    `#!/usr/bin/env bash
echo run >> "${counterFile}"
echo "Error: parallel golangci-lint is running" 1>&2
exit 1
`,
  )
  const { stderr } = withCapturedOutput(stubDir, () =>
    runGolangci({ cwd: stubDir, attempts: 2, backoffMs: 1 }),
  )
  assert.equal(invocationCount(counterFile), 2)
  // A message pinned to the literal "3 attempts" would itself be a gate that
  // reports something other than what happened — the class BOS-736 removes.
  assert.match(stderr, /exhausted after 2 attempts/)
  assert.doesNotMatch(stderr, /exhausted after 3 attempts/)
  // The default rendering that CLAUDE.md greps for is still exactly this.
  assert.equal(
    LOCK_CONTENTION_EXHAUSTED_MESSAGE,
    'golangci-lint: lock contention exhausted after 3 attempts (not a lint finding)',
  )
})

test('runGolangci rejects a non-positive attempts instead of claiming false contention', () => {
  const stubDir = makeStubDir('lint-affected-stub-badattempts-')
  const counterFile = path.join(stubDir, 'counter')
  fs.writeFileSync(counterFile, '')
  writeGolangciStub(stubDir, `#!/usr/bin/env bash\necho run >> "${counterFile}"\nexit 0\n`)
  withCapturedOutput(stubDir, () => {
    assert.throws(() => runGolangci({ cwd: stubDir, attempts: 0 }), RangeError)
  })
  // It must not have claimed contention it never observed, nor run the linter.
  assert.equal(invocationCount(counterFile), 0)
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
    runGolangci({ cwd: stubDir, attempts: 3, backoffMs: 1 }),
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
    },
  })
  assert.equal(run.status, 1, 'a lint finding must still exit non-zero')
  assert.match(
    run.stdout,
    /main\.go:1:1: undefined: sentinelPastThePipeBuffer/,
    'output past the pipe buffer was truncated — the gate reported less than it produced',
  )
})

// Same flush hazard, on the stream that matters most: CLAUDE.md tells agents to
// grep stderr for the exhausted-contention literal. If a truncating exit drops
// it, every agent is grepping for a string the run really did print — a gate
// signal that lies, which is the class BOS-736 exists to remove.
test('the CLI still prints the exhausted-contention literal after bulk output on a pipe', () => {
  const stubDir = makeStubDir('lint-affected-stub-pipecontention-')
  const stampDir = makeStubDir('lint-affected-stamps-pipecontention-')
  // As above: answer the `--version` probe cheaply so the run reaches the code
  // under test instead of dying with ENOBUFS for an unrelated reason.
  writeGolangciStub(
    stubDir,
    `#!/usr/bin/env bash
if [ "$1" = "--version" ]; then echo "golangci-lint has version 0.0.0-stub"; exit 0; fi
head -c 3000000 /dev/zero | tr '\\0' 'x' >&2
echo >&2
echo "Error: parallel golangci-lint is running" >&2
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
      BOSS_LINT_STAMP_DIR: stampDir,
    },
  })
  assert.notEqual(run.status, 0)
  assert.ok(
    run.stderr.includes(LOCK_CONTENTION_EXHAUSTED_MESSAGE),
    `the literal agents grep for was lost to pipe truncation:\n  ${LOCK_CONTENTION_EXHAUSTED_MESSAGE}`,
  )
})

// CLAUDE.md tells agents to grep for this exact literal to tell exhausted lock
// contention from a real lint finding. Nothing else pins the two together, so a
// reword on either side would silently leave every agent grepping for a string
// this script no longer prints — a documented gate signal that lies, which is
// the class BOS-736 exists to remove.
test('CLAUDE.md documents the exhausted-contention literal verbatim', () => {
  const claudeMd = fs.readFileSync(
    path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'CLAUDE.md'),
    'utf8',
  )
  assert.ok(
    claudeMd.includes(LOCK_CONTENTION_EXHAUSTED_MESSAGE),
    `CLAUDE.md no longer contains the literal agents are told to grep for:\n  ${LOCK_CONTENTION_EXHAUSTED_MESSAGE}`,
  )
})
