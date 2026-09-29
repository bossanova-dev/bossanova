#!/usr/bin/env node

// Command-contract suite for the BOS-339 make-over-bazel facade.
//
// Pins the exact bazel command lines `make` constructs, so a future Makefile edit
// that breaks the interface (drops --config=race, reorders the guards, skips the
// native ledger step, or breaks the bazel-absent fallback) fails loudly here.
//
// Strategy: `make -n` (dry-run) with BAZEL=<fake-bazel> for command-construction
// assertions (nothing executes — we parse stdout), plus ONE real exec to prove
// failure propagation. The fake bazel records its args and exits configurably.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '../..')
const fakeBazel = path.join(here, 'testdata', 'fake-bazel')

// `make -n` dry run: returns stdout; nothing executes.
function makeDryRun(args, extraEnv = {}) {
  // `make test-race` exports RACE=1 to this suite. Default-contract assertions
  // must stay default; recursive make also forwards it through MAKEFLAGS. The
  // race-specific test passes RACE=1 explicitly below.
  //
  // BOSS_GATE_FORCE_UNCACHED is scrubbed for the same reason and one more (BOS-1276): Makefile:91
  // appends --nocache_test_results to BAZEL_TEST_FLAGS when it is 1, so an AMBIENT value silently
  // rewrites the very command line the default-contract assertions pin. Until it was scrubbed, no
  // criterion in this repo could demand a proven-uncached gate run, because exporting the variable
  // to force one turned this suite red for a reason that had nothing to do with the facade. The
  // forced-uncached test below passes it as a make ARGUMENT instead, which is unaffected.
  const cleanEnv = { ...process.env }
  delete cleanEnv.RACE
  delete cleanEnv.MAKEFLAGS
  delete cleanEnv.BOSS_GATE_FORCE_UNCACHED
  return execFileSync('make', ['-n', `BAZEL=${fakeBazel}`, ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    env: { ...cleanEnv, PATH: process.env.PATH, ...extraEnv },
  })
}

test('test-bossd delegates to bazel //services/bossd/... + the bossd ledger step', () => {
  const out = makeDryRun(['test-bossd'])
  assert.match(out, /gate-cache\.mjs run --site test-bossd/)
  assert.match(
    out,
    /test --test_output=errors\s+(?:\$\{BOSS_GATE_FORCE_UNCACHED:\+--nocache_test_results\}\s+)?\/\/services\/bossd\/\.\.\./,
  )
  assert.match(out, /run-ledger\.mjs --module services\/bossd --disposition default-run/)
})

test('RACE=1 maps to --config=race on bazel AND --race on the ledger step', () => {
  const out = makeDryRun(['RACE=1', 'test-bossd'])
  assert.match(out, /--config=race/)
  assert.match(out, /run-ledger\.mjs .*--race/)
})

test('forced uncached module run preserves race config and adds uncached Bazel flag', () => {
  const out = makeDryRun(['RACE=1', 'BOSS_GATE_FORCE_UNCACHED=1', 'test-bossd'])
  assert.match(out, /--config=race/)
  assert.match(out, /\$\{BOSS_GATE_FORCE_UNCACHED:\+--nocache_test_results\}/)
  assert.match(out, /--nocache_test_results/)
})

test('test-smoke runs the short bazel loop over //...', () => {
  const out = makeDryRun(['test-smoke'])
  assert.match(out, /--test_arg=-test\.short \/\/\.\.\./)
})

// BOS-371 inverted `make test` to the fast AFFECTED path; the exhaustive
// whole-graph suite (guards → bazel //... → ledger) is now `make test-all`.
test('test-all: guards run before the bazel loop, native ledger runs after', () => {
  const out = makeDryRun(['test-all'])
  const idxScripts = out.indexOf('test-scripts')
  const idxMirror = out.indexOf('test-public-mirror')
  const idxBazel = out.search(/test --test_output=errors\s+\/\/\.\.\./)
  const idxLedger = out.search(/test-native-ledger|run-ledger/)
  assert.ok(idxScripts >= 0 && idxMirror >= 0 && idxBazel >= 0 && idxLedger >= 0, out)
  assert.ok(idxScripts < idxBazel, 'test-scripts guard must precede the bazel loop')
  assert.ok(idxMirror < idxBazel, 'test-public-mirror guard must precede the bazel loop')
  assert.ok(idxBazel < idxLedger, 'the native ledger step must run after the bazel loop')
})

// The fast default `make test` delegates to the affected selector, not the
// whole-graph bazel loop (BOS-371). Asserted from the Makefile text rather than
// `make -n test`: the `test-affected` recipe references `$(MAKE)`, so make would
// actually EXECUTE `select-affected-tests.mjs` (which runs `git diff origin/main`)
// even under -n — non-hermetic and CI-fragile.
test('test: default rule delegates to the affected selector (not the whole-graph loop)', () => {
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  assert.match(makefile, /^test:\s*test-affected\s*$/m)
})

test('failure propagation: a non-zero bazel exit aborts before the ledger step (real exec)', () => {
  const logFile = path.join(os.tmpdir(), `fake-bazel-${process.pid}-${Date.now()}.log`)
  let threw = false
  try {
    execFileSync('make', [`BAZEL=${fakeBazel}`, 'test-bossd'], {
      cwd: repoRoot,
      encoding: 'utf8',
      env: { ...process.env, FAKE_BAZEL_LOG: logFile, FAKE_BAZEL_EXIT: '1' },
      // The failing bazel makes `make` print "*** [test-bossd] Error 1" to
      // stderr; capture it instead of echoing to the parent. The test asserts
      // on err.status + the fake-bazel log, not on this output.
      stdio: ['ignore', 'pipe', 'pipe'],
    })
  } catch (err) {
    threw = true
    assert.ok(err.status && err.status !== 0, `expected non-zero exit, got ${err.status}`)
  } finally {
    // fake-bazel ran (recorded its args) and then failed; make aborted.
    if (fs.existsSync(logFile)) {
      const log = fs.readFileSync(logFile, 'utf8')
      assert.match(log, /\/\/services\/bossd\/\.\.\./)
      fs.rmSync(logFile, { force: true })
    }
  }
  assert.ok(threw, 'make test-bossd must exit non-zero when bazel fails')
})

test('bazel-absent fallback: BOSS_NO_BAZEL uses the native module loop, no bazel', () => {
  const out = makeDryRun(['BOSS_NO_BAZEL=1', 'test-bossd'])
  assert.doesNotMatch(out, /test --test_output=errors \/\/services\/bossd/)
  // Native fallback: `$(MAKE) -C services/bossd test`.
  assert.match(out, /-C services\/bossd test/)
})

test('optional-module guard: mcp-gateway present here yields its bazel line', () => {
  // When the module dir is ABSENT (public/pruned mirror) the `ifneq` guard omits
  // the target entirely, so no bazel line is emitted for it. services/mcp-gateway
  // IS present in this repo, so we assert the present-case bazel delegation.
  const out = makeDryRun(['test-mcp-gateway'])
  assert.match(out, /gate-cache\.mjs run --site test-mcp-gateway/)
  assert.match(
    out,
    /test --test_output=errors\s+(?:\$\{BOSS_GATE_FORCE_UNCACHED:\+--nocache_test_results\}\s+)?\/\/services\/mcp-gateway\/\.\.\./,
  )
})

// The prerequisite itself, asserted rather than assumed: with the variable ambient, the
// default-contract dry run must still be the DEFAULT contract. Without the scrub above this reads
// `--nocache_test_results` in a run that asked for no such thing.
test('an ambient forced-uncached variable does not leak into the default contract', () => {
  const original = process.env.BOSS_GATE_FORCE_UNCACHED
  process.env.BOSS_GATE_FORCE_UNCACHED = '1'
  try {
    const out = makeDryRun(['test-bossd'])
    // The literal flag must appear only inside the shell-conditional expansion, never as a bare
    // flag make itself appended.
    const bare = out.replace(/\$\{BOSS_GATE_FORCE_UNCACHED:\+--nocache_test_results\}/g, '')
    assert.doesNotMatch(bare, /--nocache_test_results/)
  } finally {
    if (original === undefined) delete process.env.BOSS_GATE_FORCE_UNCACHED
    else process.env.BOSS_GATE_FORCE_UNCACHED = original
  }
})

// BOS-1339: the readiness receipt runs `BOSS_GATE_FORCE_UNCACHED=1 make test-full`
// (commands.testReadiness). The //... line must then carry --nocache_test_results as a
// flag make itself appended, and the default iterative `make test-full` must not.
test('forced-uncached test-full puts --nocache_test_results on the //... bazel line', () => {
  const bazelAllLine = (out) =>
    out.split('\n').find((line) => /\btest\b.*--test_output=errors.*\s\/\/\.\.\.\s*$/.test(line))

  const forced = bazelAllLine(makeDryRun(['BOSS_GATE_FORCE_UNCACHED=1', 'test-full']))
  assert.ok(forced, 'forced dry run must emit the //... bazel line')
  assert.match(forced, /--nocache_test_results/)

  const cached = bazelAllLine(makeDryRun(['test-full']))
  assert.ok(cached, 'default dry run must emit the //... bazel line')
  assert.doesNotMatch(cached, /--nocache_test_results/)
})

// BOS-1339: `make test-affected` re-runs its SELECTED commands uncached when the branch adds or
// renames a file, using the gate cache's own predicate. Asserted over the recipe text for the
// reason given above for `test:` - a real `make -n test-affected` executes the selector.
test('test-affected asks gate-cache adds-or-renames in the selected branch only, before its loop', () => {
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  const start = makefile.search(/^test-affected:/m)
  assert.ok(start >= 0, 'test-affected target must exist')
  const rest = makefile.slice(start)
  // The recipe runs to the first following line that is not tab-indented recipe text.
  const bodyStart = rest.indexOf('\n') + 1
  const end = rest.slice(bodyStart).search(/^(?!\t)/m)
  const recipe = end >= 0 ? rest.slice(0, bodyStart + end) : rest

  const probe = recipe.indexOf('gate-cache.mjs adds-or-renames')
  assert.ok(probe >= 0, 'recipe must consult gate-cache.mjs adds-or-renames')
  assert.equal(
    recipe.indexOf('gate-cache.mjs adds-or-renames', probe + 1),
    -1,
    'the probe must appear exactly once (never in the smoke fallback)',
  )
  const smoke = recipe.search(/\$\(MAKE\) test-smoke/)
  const loop = recipe.search(/while\s+IFS=/)
  assert.ok(smoke >= 0 && loop >= 0, recipe)
  assert.ok(smoke < probe, 'the probe must sit after the smoke-fallback branch, not inside it')
  assert.ok(probe < loop, 'the probe must run before the selected-commands loop')
  assert.match(
    recipe.slice(probe, loop),
    /BOSS_GATE_FORCE_UNCACHED=1;\s*export BOSS_GATE_FORCE_UNCACHED/,
  )

  // Only the printed `no` verdict with exit 1 keeps the cache. Execute the recipe's own probe lines with the
  // probe stubbed, so a crash (exit 1 on a throw, 127 with node missing) or an empty answer
  // cannot pass for `no` the way exit-code polarity would let it.
  const snippet = recipe
    .slice(recipe.lastIndexOf('\n', probe) + 1, recipe.lastIndexOf('\n', loop) + 1)
    .split('\n')
    .map((line) => line.replace(/\\\s*$/, '').trim())
    .join('\n')
    .replaceAll('$$', '$')
    .replace('node scripts/gate-cache.mjs adds-or-renames', 'sh -c "$PROBE"')
  assert.ok(snippet.includes('sh -c "$PROBE"'), `probe stub did not land:\n${snippet}`)
  const forced = (probeScript) =>
    execFileSync('sh', ['-c', `${snippet}\nprintf 'FORCE=%s' "\${BOSS_GATE_FORCE_UNCACHED:-}"`], {
      encoding: 'utf8',
      env: { PATH: process.env.PATH, PROBE: probeScript },
    }).match(/FORCE=(.*)$/)[1]
  assert.equal(forced("echo 'adds-or-renames: no (modify-only)'; exit 1"), '', 'no keeps the cache')
  // `no` is exit 1 (gate-cache AC10); a printed `no` under any other status is not a clean verdict.
  assert.equal(
    forced("echo 'adds-or-renames: no (modify-only)'; exit 0"),
    '1',
    'no + exit 0 forces',
  )
  assert.equal(
    forced("echo 'adds-or-renames: no (modify-only)'; exit 2"),
    '1',
    'no + exit 2 forces',
  )
  assert.equal(forced("echo 'adds-or-renames: no (m)'; exit 134"), '1', 'no + abort forces')
  assert.equal(forced("echo 'adds-or-renames: yes (adds)'; exit 0"), '1', 'yes forces')
  assert.equal(forced("echo 'adds-or-renames: unknown (x)'; exit 0"), '1', 'unknown forces')
  assert.equal(forced('echo boom >&2; exit 1'), '1', 'a crash (exit 1, no verdict) forces')
  assert.equal(forced('exit 127'), '1', 'a missing node (exit 127) forces')
})

// BOS-1339: --bes_timeout defaults to 0s (wait forever for the BES upload after tests finish),
// which let a green full gate idle for minutes. A committed, non-zero bound must exist.
test('.bazelrc bounds the BES upload tail with a non-zero build --bes_timeout', () => {
  const rc = fs.readFileSync(path.join(repoRoot, '.bazelrc'), 'utf8')
  const match = rc.match(/^build\s+--bes_timeout=(\d+)(ms|s|m|h)?\s*$/m)
  assert.ok(match, 'expected a `build --bes_timeout=<N><unit>` line in .bazelrc')
  assert.ok(Number(match[1]) > 0, `bes_timeout must be non-zero, got ${match[0]}`)
})
