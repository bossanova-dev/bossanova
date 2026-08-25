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
  const cleanEnv = { ...process.env }
  delete cleanEnv.RACE
  delete cleanEnv.MAKEFLAGS
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
