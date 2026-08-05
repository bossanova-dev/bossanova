#!/usr/bin/env node

// Drift gate for `externalInputRules` in select-affected-tests.mjs.
//
// The affected selector may return an EMPTY target set when every changed file is
// Go-graph-irrelevant. That is only sound while the selector knows every path a Go test
// reads from outside its own module — otherwise a change to such a path selects nothing
// and the test that consumes it never runs.
//
// A sandboxed bazel test can only read what its target declares in `data`, so the bazel
// half of that knowledge is mechanically derivable: this test re-derives every cross-tree
// `data` dep from the BUILD files and fails if one is not covered by an external-input
// rule. A new `data = ["//somewhere/else:thing"]` therefore breaks this test rather than
// silently punching a hole in CI coverage.
//
// The NATIVE ledger half cannot be derived this way — those targets are bazel-`manual`,
// run unsandboxed, and read straight from the worktree with no declaration to inspect. The
// pinned cases below stand in for that, and each cites the reader.
//
// Node built-ins only — cron worktrees are dependency-free.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { selectBazelAffected, selectLedgerModules } from './select-affected-tests.mjs'

const repoRoot = path.dirname(fileURLToPath(new URL('../Makefile', import.meta.url)))

/** Every BUILD.bazel in the real source tree (agent worktrees excluded). */
function buildFiles() {
  const out = execFileSync(
    'git',
    ['ls-files', '--', '*/BUILD.bazel', 'BUILD.bazel', '**/BUILD.bazel'],
    { cwd: repoRoot, encoding: 'utf8' },
  )
  return out.split('\n').filter(Boolean)
}

/**
 * Cross-tree `data` deps: a label in some package's `data = [...]` that points outside the
 * directory the BUILD file lives in. Returns [{ owner, label }].
 */
function crossTreeDataDeps() {
  const found = []
  for (const rel of buildFiles()) {
    const dir = path.posix.dirname(rel)
    const text = fs.readFileSync(path.join(repoRoot, rel), 'utf8')
    // Each `data = [ ... ]` block; labels are `//pkg[:target]`.
    for (const block of text.matchAll(/data\s*=\s*\[([\s\S]*?)\]/g)) {
      for (const m of block[1].matchAll(/"(\/\/[^"]+)"/g)) {
        const pkg = m[1].replace(/^\/\//, '').replace(/:.*$/, '')
        if (pkg === dir || pkg.startsWith(`${dir}/`)) continue
        found.push({ owner: dir, label: m[1], pkg })
      }
    }
  }
  return found
}

// A dep is "covered" when a change to a representative file under it still selects the
// owning module's pattern — i.e. the selector routes the external input back to its reader.
function selectsOwner(pkg, owner) {
  const probe = `${pkg}/__drift_probe__`
  const result = selectBazelAffected([probe])
  if (result.full) return true // FULL always covers the owner
  // Module roots in this repo are two segments deep (lib/<x>, services/<x>, plugins/<x>).
  const ownerModule = owner.split('/').slice(0, 2).join('/')
  return result.patterns.some((p) => {
    const root = p.replace(/^\/\//, '').replace(/\/\.\.\.$/, '')
    return owner === root || owner.startsWith(`${root}/`) || ownerModule === root
  })
}

test('every cross-tree bazel data dep routes back to the target that reads it', () => {
  const deps = crossTreeDataDeps()
  assert.ok(deps.length > 0, 'expected to find at least one cross-tree data dep to check')
  const uncovered = deps.filter(({ pkg, owner }) => !selectsOwner(pkg, owner))
  assert.deepEqual(
    uncovered.map(({ owner, label }) => `${label} read by //${owner}`),
    [],
    'a bazel target declares a `data` dep whose package does NOT select that target when ' +
      'changed — add it to externalInputRules in select-affected-tests.mjs, or the affected ' +
      'selector will skip the test that reads it',
  )
})

// --- Native-ledger external inputs (not derivable from BUILD `data`) -------------------
// Each case pins a file that a bazel-`manual` row reads from outside its own module.

test('services/web/src/api.ts still runs the apiversion ledger row', () => {
  // lib/bossalib/apiversion/webversion_test.go reads it for Go/web API-version parity.
  const { all, modules } = selectLedgerModules(['services/web/src/api.ts'])
  assert.ok(all || modules.includes('lib/bossalib'), 'api.ts must select the bossalib row')
})

test('published skill trees and their prose still run the skillinstall ledger row', () => {
  for (const f of [
    'plugins/bossd-plugin-claude/skilldata/skills/boss-plan/SKILL.md',
    'docs/skills/extensions.md',
    'services/docs/docs/skills/extensions.md',
  ]) {
    const { all, modules } = selectLedgerModules([f])
    assert.ok(all || modules.includes('services/boss'), `${f} must select the boss ledger rows`)
  }
})

test('codex plugin testdata still runs the bossd testharness ledger row', () => {
  const f = 'plugins/bossd-plugin-codex/testdata/fake_codex_tui.sh'
  const { all, modules } = selectLedgerModules([f])
  assert.ok(all || modules.includes('services/bossd'), `${f} must select the bossd ledger row`)
})

test('a shared-module change runs every ledger row, not just its own', () => {
  // The boss/bossd native rows are integration tests that link bossalib, so scoping to
  // `lib/bossalib` alone would skip the rows a bossalib change is most likely to break.
  assert.deepEqual(selectLedgerModules(['lib/bossalib/apiversion/x.go']), {
    all: true,
    modules: [],
  })
})

test('analytics docs and web events still select the telemetry graph target', () => {
  for (const f of ['docs/analytics/events.md', 'services/web/src/analytics/events.ts']) {
    const { full, patterns } = selectBazelAffected([f])
    assert.ok(
      full || patterns.includes('//lib/bossalib/...'),
      `${f} is a declared data dep of //lib/bossalib/telemetry:telemetry_test`,
    )
  }
})

test('a genuinely inert change still selects nothing', () => {
  // The saving this whole change exists for must survive the fixes above.
  assert.deepEqual(selectBazelAffected(['README.md', '.github/workflows/ci.yml']), {
    full: false,
    patterns: [],
    reason: 'no go-relevant changes',
  })
  assert.deepEqual(selectLedgerModules(['README.md']), { all: false, modules: [] })
})
