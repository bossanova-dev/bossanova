#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

// `fileURLToPath`, not `new URL(...).pathname`: the latter is the percent-ENCODED URL component, so
// a checkout under a path containing a space or a non-ASCII character resolves repoRoot to a
// directory that does not exist. The scan would then find nothing and this guard would redden on a
// host-path condition unrelated to the code under test — the exact failure class BOS-739 exists to
// remove, reintroduced in the artifact meant to prevent it.
const selfPath = fileURLToPath(import.meta.url)
const repoRoot = path.resolve(path.dirname(selfPath), '..')
const SELF_REL = path.relative(repoRoot, selfPath)

// BOS-739. These suites run under plain `node --test` (scripts/Makefile `test:`), NOT Bazel, so
// .bazelrc's `test --test_env=GIT_CONFIG_KEY_0=commit.gpgsign` (.bazelrc:21-23) never reaches
// them. A fixture that bootstraps a temp repo and commits therefore inherits the developer's
// global commit.gpgsign, and under host memory pressure fails with
// "gpg: signing failed: Cannot allocate memory" — reddening the whole gate on a branch that
// cannot reach the failing file. Disable signing per temp repo instead.
const SCAN_DIRS = ['scripts', 'skills-toolbox']

// A quoted bare `init` / `commit` git argument. `commit.gpgsign` is deliberately NOT matched by
// COMMITS: the closing quote must follow `commit` immediately, so a file that only configures
// signing is not mistaken for one that commits.
//
// KNOWN LIMITATION — this check is FILE-LEVEL, not per-fixture. One signing disable anywhere in a
// file immunises every temp repo in it. That is observed, not hypothetical: this ticket's own
// scripts/boss-build-skill.test.mjs:97 fixture was unprotected while the unrelated helper at
// :6104 already disabled signing, so this guard could never have flagged it. Per-fixture
// granularity is deliberately NOT attempted: the dominant idiom here puts the disable in a shared
// helper well away from the `init` sites it protects (scripts/bs-plan-ce-skill.test.mjs:204 covers
// seven repos, the furthest ~500 lines below it; boss-build-skill.test.mjs's own `author()` covers
// four), so any proximity window would false-positive on correct code. Three shapes evade this
// guard entirely: git args built wholly from variables, shell-string forms like
// `execSync('git init ' + dir)`, and a match that is only prose — these patterns read TEXT, not
// executed code, so a comment, doc fixture or assertion string mentioning the disable immunises
// its file exactly as a real call does. None of the first two exists under either scan root today.
// This is a ratchet against the realistic shapes in this repo, not a proof — read a green run as
// "every such FILE contains a disable", never as per-fixture coverage.
const INITS = /['"]init['"]/
const COMMITS = /['"]commit['"]/

// Either the minimal per-repo disable — `'commit.gpgsign', 'false'` or `-c commit.gpgsign=false` —
// or full host-config isolation by pointing GIT_CONFIG_GLOBAL at /dev/null
// (scripts/add-pr-numbers.test.mjs:29). Both neutralise inherited signing, so both satisfy this
// guard. The /dev/null value is required rather than the bare identifier: GIT_CONFIG_GLOBAL aimed
// at some other file proves nothing about signing, and the token appears in fixtures that set it
// for unrelated reasons (boss-build-skill.test.mjs:6322 pins it so `git status` ignores host
// excludes). Both real call sites already use /dev/null, so requiring it costs nothing today.
const SIGNING_DISABLED = [
  /commit\.gpgsign(['"]\s*,\s*['"]|=)false/,
  /GIT_CONFIG_GLOBAL['"]?\s*[:=,]\s*['"]?\/dev\/null/,
]

/**
 * True when `text` looks like a fixture that bootstraps a git repo and commits, without
 * neutralising inherited signing. Kept pure and separate from the file walk so the guard's own
 * detection can be table-tested — a disk scan over a currently-clean tree cannot distinguish a
 * working detector from a broken one.
 */
function isUnprotected(text) {
  if (!INITS.test(text) || !COMMITS.test(text)) return false
  return !SIGNING_DISABLED.some((re) => re.test(text))
}

/** Recursively collect repo-relative paths of every *.test.mjs under dir. */
function collectTestFiles(dir) {
  const abs = path.join(repoRoot, dir)
  if (!fs.existsSync(abs)) return []
  const found = []
  for (const entry of fs.readdirSync(abs, { withFileTypes: true })) {
    const rel = path.join(dir, entry.name)
    if (entry.isDirectory()) {
      if (entry.name === 'node_modules') continue
      found.push(...collectTestFiles(rel))
    } else if (entry.name.endsWith('.test.mjs')) {
      found.push(rel)
    }
  }
  return found
}

// One traversal, keyed by root: the per-root assertion below and the offender scan then read the
// same data instead of scanning the tree twice from two independent sources of truth.
const byRoot = new Map(SCAN_DIRS.map((dir) => [dir, collectTestFiles(dir)]))
const testFiles = [...byRoot.values()].flat()

test('the guard actually found files to scan', () => {
  // Without this the whole guard passes vacuously if the scan roots are ever renamed or moved.
  // Assert PER ROOT as well as on the total: `scripts` alone contributes well over the floor below
  // and holds the known fixture below it, so a total-only check stays green while an entire root
  // silently drops out — skills-toolbox is 42 files today and holds three real bootstrap-and-commit
  // fixtures (claude-review, codex-review, codex-review-embed .test.mjs).
  for (const [dir, files] of byRoot) {
    assert.ok(
      files.length > 0,
      `scan root ${dir} contributed no test files; it was renamed, moved, or removed`,
    )
  }
  assert.ok(
    testFiles.length > 20,
    `scanned only ${testFiles.length} test files; the scan is broken`,
  )
  assert.ok(
    testFiles.includes(path.join('scripts', 'add-pr-numbers.test.mjs')),
    'known bootstrap-and-commit fixture scripts/add-pr-numbers.test.mjs was not scanned',
  )
})

test('the detector discriminates protected from unprotected fixtures', () => {
  // The disk scan below only ever asserts that TODAY's tree is clean, so it stays green even if
  // INITS / COMMITS / SIGNING_DISABLED were mistyped or later broken — every file would simply fall
  // through as "not considered". These inline fixtures are the guard's negative test: they fail the
  // moment the predicate stops discriminating.
  const bootstrap = "git('init', '-q')\ngit('commit', '-m', 'base')\n"
  assert.equal(
    isUnprotected(bootstrap),
    true,
    'an unprotected bootstrap-and-commit must be flagged',
  )
  assert.equal(
    isUnprotected(bootstrap + "git('config', 'commit.gpgsign', 'false')\n"),
    false,
    'the per-repo config form must clear the guard',
  )
  assert.equal(
    isUnprotected(bootstrap + "execFileSync('git', ['-c', 'commit.gpgsign=false', 'status'])\n"),
    false,
    'the -c commit.gpgsign=false form must clear the guard',
  )
  assert.equal(
    isUnprotected(bootstrap + "const env = { GIT_CONFIG_GLOBAL: '/dev/null' }\n"),
    false,
    'full host-config isolation via GIT_CONFIG_GLOBAL=/dev/null must clear the guard',
  )
  assert.equal(
    isUnprotected(bootstrap + "const env = { GIT_CONFIG_GLOBAL: '/tmp/other-config' }\n"),
    true,
    'GIT_CONFIG_GLOBAL pointed somewhere other than /dev/null must NOT clear the guard',
  )
  assert.equal(
    isUnprotected("git('init', '-q')\n"),
    false,
    "a fixture that inits but never commits is not this guard's concern",
  )
  assert.equal(
    // `'true'`, deliberately: with `'false'` this row would pass even under a loosened COMMITS,
    // because SIGNING_DISABLED would exempt it anyway and the carve-out would go unpinned.
    isUnprotected("git('init', '-q')\ngit('config', 'commit.gpgsign', 'true')\n"),
    false,
    'a quoted `commit.gpgsign` must not satisfy COMMITS — only a bare quoted `commit` does',
  )
  assert.equal(
    isUnprotected("git('commit', '-m', 'base')\n"),
    false,
    "a fixture that commits but never inits is not this guard's concern",
  )
  assert.equal(isUnprotected('const x = 1\n'), false, 'a file with neither token is not considered')
})

test('every test file that bootstraps a git repo and commits contains a signing disable', () => {
  const offenders = []
  for (const file of testFiles) {
    // This guard is excluded from its own scan, PRE-EMPTIVELY rather than as a present-tense fix:
    // the table test above embeds `init`/`commit` literals, so the file matches its own INITS and
    // COMMITS patterns while creating no temp repo at all. It is not an offender today only because
    // those same fixtures embed all three accepted disable forms, any ONE of which self-exempts it
    // by the prose-match limitation above — so only a reword dropping every disable literal while
    // keeping the init/commit tokens would turn this guard into its own offender. Excluding one
    // file that never runs git is not the deferred granularity question: it drops a file from the
    // scan set rather than narrowing the match window.
    if (file === SELF_REL) continue
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    if (!isUnprotected(text)) continue
    offenders.push(file)
  }
  assert.deepEqual(
    offenders,
    [],
    `these test files create a git repo and commit without disabling signing, so a host with\n` +
      `commit.gpgsign=true can fail them for reasons unrelated to the code under test.\n` +
      `Add \`git config commit.gpgsign false\` to each temp repo before its first commit.\n` +
      `If a file below never creates a git repo, the quoted init/commit match is a false\n` +
      `positive — see the KNOWN LIMITATION comment at the top of this guard:\n` +
      offenders.map((f) => `  - ${f}`).join('\n'),
  )
})
