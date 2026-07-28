#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

const here = path.dirname(new URL(import.meta.url).pathname)
const scriptPath = path.join(here, 'check-build-drift.sh')
const repoRoot = path.resolve(here, '..', '..')

test('build-drift diagnostics do not use globally shared temporary files', () => {
  const script = fs.readFileSync(scriptPath, 'utf8')

  assert.doesNotMatch(
    script,
    /\/tmp\/(?:inventory|ledger)-drift\.diff/,
    'concurrent guard runs must not overwrite shared /tmp diff files',
  )
})

// BOS-582: this guard shipped as dead code — referenced by no Make target and no CI
// workflow — so BUILD drift went unchallenged for months. These assertions ratchet the
// wiring itself so it cannot silently become dead code a second time. Presence of the
// string is not enough: a target nothing depends on, or a commented-out CI step, is
// dead code that a substring match would still call wired. So each assertion checks
// *reachability* — that the recipe runs the script AND that a live caller invokes it.
test('the root Makefile has a build-drift-check target that runs the script', () => {
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')

  assert.match(
    makefile,
    /^build-drift-check:[^\n]*\n(?:[^\n]*\n)*?[ \t]*\.\/scripts\/bazel\/check-build-drift\.sh$/m,
    'the build-drift-check target must run ./scripts/bazel/check-build-drift.sh in its recipe',
  )
})

test('the build-drift-check target is reachable from make test-all', () => {
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  const testAll = /^test-all:(?<prereqs>[^\n]*)$/m.exec(makefile)

  assert.ok(testAll, 'the root Makefile must declare a test-all target')
  assert.ok(
    testAll.groups.prereqs.split(/\s+/).includes('build-drift-check'),
    'test-all must list build-drift-check as a prerequisite, or the gate is unreachable from make',
  )
})

test('the bazel CI go-test job runs make build-drift-check in a live step', () => {
  const workflow = fs.readFileSync(path.join(repoRoot, '.github', 'workflows', 'bazel.yml'), 'utf8')
  const lines = workflow.split('\n')

  // Slice out the `go-test:` job block: from its header to the next job header at
  // the same indent. Scoping matters — the gate is deliberately in this job because
  // its bazel server and remote cache are already warm, so a move elsewhere is a
  // behaviour change the ratchet should catch rather than wave through.
  const start = lines.findIndex((line) => /^ {2}go-test:\s*$/.test(line))
  assert.notEqual(start, -1, 'bazel.yml must declare a go-test job')

  let end = lines.length
  for (let i = start + 1; i < lines.length; i += 1) {
    if (/^ {2}\S.*:\s*$/.test(lines[i])) {
      end = i
      break
    }
  }

  const liveStep = lines
    .slice(start, end)
    .some((line) => /^\s*run:\s*make build-drift-check\s*$/.test(line))

  assert.ok(
    liveStep,
    "bazel.yml's go-test job must have an uncommented `run: make build-drift-check` step",
  )
})
