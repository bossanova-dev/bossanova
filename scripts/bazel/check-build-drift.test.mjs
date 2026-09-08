#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { after, test } from 'node:test'

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

test('gazelle drift failure names the remedy command', () => {
  const script = fs.readFileSync(scriptPath, 'utf8')

  assert.match(script, /ERROR: BUILD files are out of sync with 'bazel run \/\/:gazelle':/)
  assert.equal([...script.matchAll(/bazel run \/\/:gazelle/g)].length, 3)
})

// BOS-1205: the shared-tempfile guard above is a name-exact ratchet over two pre-existing
// filenames, so it says nothing about the three diagnostic captures this change adds. Left
// as the only assertion it certifies "every capture uses mktemp" vacuously — replacing all
// three `$(mktemp)` assignments with fixed /tmp names keeps the suite green while restoring
// exactly the concurrent-clobbering race BOS-582 removed. These two assertions generalise the
// ratchet from those two names to the property itself, so a future fixed-name capture fails
// here instead of in production.
test('every diagnostic capture in the script is allocated with mktemp', () => {
  const script = fs.readFileSync(scriptPath, 'utf8')

  // Comment lines are stripped so prose may still discuss /tmp without tripping the gate.
  const code = script
    .split('\n')
    .filter((line) => !/^\s*#/.test(line))
    .join('\n')

  assert.doesNotMatch(
    code,
    /\/tmp\//,
    'no diagnostic capture may hard-code a /tmp path; allocate it with "$(mktemp)"',
  )

  const captures = [...script.matchAll(/^\s*(\w*_log)=(.*)$/gm)]
  assert.ok(captures.length > 0, 'expected at least one *_log diagnostic capture assignment')
  for (const [, name, rhs] of captures) {
    assert.match(rhs, /\$\(mktemp\b/, `${name} must be allocated with mktemp, got: ${rhs}`)
  }
})

// BOS-582: this guard shipped as dead code — referenced by no Make target and no CI
// workflow — so BUILD drift went unchallenged for months. These assertions ratchet the
// wiring itself so it cannot silently become dead code a second time. Presence of the
// string is not enough: a target nothing depends on, or a commented-out CI step, is
// dead code that a substring match would still call wired. So each assertion checks
// *reachability* — that the recipe runs the script AND that a live caller invokes it.
// BOS-1205: the recipe now runs through scripts/run-gate.mjs, so a host-environment token in
// the gate's output is classified as ENVIRONMENT FAILURE (not a code defect) / exit 75 for both
// CI callers from one place. The previous pattern required the script path to be the only
// content on its recipe line and could not survive the wrap. This replacement still proves
// *reachability* rather than mention: the recipe line must invoke the wrapper AND end in the
// script path, and it is scoped to this target's own block so a sibling target's recipe can
// never satisfy it on this target's behalf.
const INVOKES_DRIFT_SCRIPT =
  /^[ \t]*@?node scripts\/run-gate\.mjs\b[^\n]*--[ \t]+\.\/scripts\/bazel\/check-build-drift\.sh[ \t]*$/m

function makeTargetBlock(makefile, target) {
  const lines = makefile.split('\n')
  const start = lines.findIndex((line) => line.startsWith(`${target}:`))
  if (start === -1) return null
  let end = lines.length
  for (let i = start + 1; i < lines.length; i += 1) {
    // A new rule or variable assignment at column 0 ends this target's block. Make
    // conditionals (ifeq/else/endif) also sit at column 0 but carry no ':', so the
    // BAZEL_USABLE-guarded recipe stays inside the block.
    if (/^[^\s#][^\n]*:/.test(lines[i])) {
      end = i
      break
    }
  }
  return lines.slice(start, end).join('\n')
}

test('the root Makefile has a build-drift-check target that runs the script', () => {
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  const block = makeTargetBlock(makefile, 'build-drift-check')

  assert.ok(block, 'the root Makefile must declare a build-drift-check target')
  assert.match(
    block,
    INVOKES_DRIFT_SCRIPT,
    'the build-drift-check recipe must invoke ./scripts/bazel/check-build-drift.sh through scripts/run-gate.mjs',
  )
})

test('the build-drift-check ratchet rejects mention without invocation', () => {
  const invoker = '\tnode scripts/run-gate.mjs --label "x" -- ./scripts/bazel/check-build-drift.sh'

  const mentionOnly = [
    'build-drift-check: copy-skills',
    '\t@echo ./scripts/bazel/check-build-drift.sh',
    '',
    'other-target:',
    invoker,
  ].join('\n')

  const noRecipe = ['build-drift-check: copy-skills', '', 'other-target:', invoker].join('\n')

  assert.doesNotMatch(
    makeTargetBlock(mentionOnly, 'build-drift-check'),
    INVOKES_DRIFT_SCRIPT,
    'a recipe that only names the script must not count as invoking it',
  )
  assert.doesNotMatch(
    makeTargetBlock(noRecipe, 'build-drift-check'),
    INVOKES_DRIFT_SCRIPT,
    'a target with no recipe must not borrow the recipe of a sibling target',
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

test('the bazel CI affected target list is published as a notice from the selected targets', () => {
  const workflow = fs.readFileSync(path.join(repoRoot, '.github', 'workflows', 'bazel.yml'), 'utf8')

  assert.match(workflow, /echo "Affected bazel targets: \$TARGETS"/)
  assert.match(workflow, /echo "::notice title=Affected bazel targets::\$TARGETS"/)
})

// BOS-1205: the assertions above read the script as *text*. Everything below runs it.
// A gate that derives "the committed artifact drifted" from a status that is also produced
// when the measuring tool never ran cannot be caught by a source-text ratchet, so these
// tests execute the real script inside a temp git repository with a stub `bazel` on PATH
// and assert on the attribution it actually emits. Fixture shape mirrors
// scripts/tidy-drift-all.test.mjs (mkdtemp + git init + stub binary + spawnSync).
const tempRoots = []

const STUB_BAZEL = [
  '#!/usr/bin/env bash',
  'set -uo pipefail',
  'mode="$DRIFT_STUB_MODE"',
  'sub="$1"',
  'shift',
  '',
  'emit_host_fault() {',
  '  echo "ERROR: /private/var/tmp/_bazel_ci/output: writing file failed: No space left on device" >&2',
  '  echo "ERROR: Build failed. Not running target" >&2',
  '  exit 1',
  '}',
  '',
  'emit_bins() {',
  '  echo "//services/alpha/cmd:cmd"',
  '  echo "//services/beta/cmd:cmd"',
  '  echo "//plugins/gamma:gamma"',
  '}',
  '',
  'emit_manual() {',
  '  echo "//services/alpha:alpha_test"',
  '  echo "//services/beta:beta_test"',
  '}',
  '',
  'case "$sub" in',
  '  build)',
  '    if [ "$mode" = "gazelle-build-fail" ]; then emit_host_fault; fi',
  '    echo "INFO: Build completed successfully, 1 total action" >&2',
  '    exit 0',
  '    ;;',
  '  run)',
  '    if [ "$mode" = "gazelle-build-fail" ]; then emit_host_fault; fi',
  '    if [ "$mode" = "gazelle-drift" ]; then',
  '      echo "--- services/alpha/BUILD.bazel"',
  '      echo "+++ services/alpha/BUILD.bazel.gazelle"',
  '      echo \'+    srcs = ["added.go"],\'',
  '      exit 1',
  '    fi',
  '    exit 0',
  '    ;;',
  '  query)',
  '    expr="$1"',
  '    case "$expr" in',
  '      *go_binary*) which="bins" ;;',
  '      *manual*) which="manual" ;;',
  '      *) echo "unexpected query: $expr" >&2; exit 90 ;;',
  '    esac',
  '    case "$mode" in',
  '      query-fail-bins)',
  '        if [ "$which" = "bins" ]; then echo "ERROR: bazel server terminated abruptly during query" >&2; exit 37; fi ;;',
  '      query-fail-manual)',
  '        if [ "$which" = "manual" ]; then echo "ERROR: bazel server terminated abruptly during query" >&2; exit 37; fi ;;',
  '      query-empty-bins)',
  '        if [ "$which" = "bins" ]; then exit 0; fi ;;',
  '      query-empty-manual)',
  '        if [ "$which" = "manual" ]; then exit 0; fi ;;',
  '      query-noise)',
  '        echo "INFO: Loading: 12 packages loaded" >&2',
  '        echo "INFO: Found 3 targets..." >&2 ;;',
  '      query-drift-bins)',
  '        if [ "$which" = "bins" ]; then emit_bins; echo "//services/delta/cmd:cmd"; exit 0; fi ;;',
  '      query-drift-manual)',
  '        if [ "$which" = "manual" ]; then echo "//services/alpha:alpha_test"; exit 0; fi ;;',
  '    esac',
  '    if [ "$which" = "bins" ]; then emit_bins; else emit_manual; fi',
  '    exit 0',
  '    ;;',
  'esac',
  'echo "unexpected bazel invocation: $sub $*" >&2',
  'exit 91',
  '',
].join('\n')

const FIXTURE_INVENTORY = {
  services: { alpha: '//services/alpha/cmd:cmd', beta: '//services/beta/cmd:cmd' },
  plugins: { gamma: '//plugins/gamma:gamma' },
}

const FIXTURE_LEDGER = [
  { label: '//services/alpha:alpha_test' },
  { label: '//services/beta:beta_test' },
]

function initFixture() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'build-drift-'))
  tempRoots.push(dir)
  execFileSync('git', ['init', '-q', '-b', 'main'], { cwd: dir })
  execFileSync('git', ['config', 'user.email', 'test@example.com'], { cwd: dir })
  execFileSync('git', ['config', 'user.name', 'Test'], { cwd: dir })
  // scripts/env-failure-lib.mjs classifies a GPG signing failure as a host fault, so an
  // inherited global signing config would make this fixture look like the very thing under
  // test. Disable it explicitly.
  execFileSync('git', ['config', 'commit.gpgsign', 'false'], { cwd: dir })

  fs.mkdirSync(path.join(dir, 'scripts', 'bazel'), { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'scripts', 'bazel', 'binary-inventory.json'),
    `${JSON.stringify(FIXTURE_INVENTORY, null, 2)}\n`,
  )
  fs.writeFileSync(
    path.join(dir, 'scripts', 'bazel', 'ledger.json'),
    `${JSON.stringify(FIXTURE_LEDGER, null, 2)}\n`,
  )

  fs.mkdirSync(path.join(dir, 'bin'), { recursive: true })
  fs.writeFileSync(path.join(dir, 'bin', 'bazel'), STUB_BAZEL, { mode: 0o755 })
  return dir
}

function runDrift(mode) {
  const dir = initFixture()
  const result = spawnSync('bash', [scriptPath], {
    cwd: dir,
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${path.join(dir, 'bin')}${path.delimiter}${process.env.PATH}`,
      DRIFT_STUB_MODE: mode,
    },
  })
  return {
    dir,
    code: result.status ?? 1,
    stdout: result.stdout ?? '',
    stderr: result.stderr ?? '',
    output: `${result.stdout ?? ''}${result.stderr ?? ''}`,
  }
}

after(() => {
  for (const dir of tempRoots) fs.rmSync(dir, { recursive: true, force: true })
})

test('a clean tree passes all three checks', () => {
  const result = runDrift('clean')

  assert.equal(result.code, 0, result.output)
  assert.match(result.stdout, /BUILD files clean \(repo-wide gazelle diff\)/)
  assert.match(result.stdout, /binary-inventory\.json matches the live go_binary graph/)
  assert.match(result.stdout, /ledger\.json matches the live manual-tagged test set/)
})

test('a gazelle build that dies on a full disk is not reported as BUILD drift', () => {
  const result = runDrift('gazelle-build-fail')

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(
    result.output,
    /out of sync/,
    'a build that never ran gazelle cannot have measured drift, so it must not accuse BUILD files',
  )
  assert.match(result.output, /NOT BUILD drift/)
  // R4: the underlying tool output must survive verbatim so scripts/env-failure-lib.mjs
  // can still classify it as a host failure through scripts/run-gate.mjs.
  assert.match(result.output, /No space left on device/i)
  assert.match(result.output, /Build failed\. Not running target/)
})

test('genuine gazelle drift still emits the existing headline and diff body', () => {
  const result = runDrift('gazelle-drift')

  assert.notEqual(result.code, 0)
  assert.match(result.output, /ERROR: BUILD files are out of sync with 'bazel run \/\/:gazelle':/)
  assert.match(result.output, /Run: bazel run \/\/:gazelle/)
  assert.match(result.output, /\+\+\+ services\/alpha\/BUILD\.bazel\.gazelle/)
})

test('a failed go_binary query is reported as a host failure, not inventory drift', () => {
  const result = runDrift('query-fail-bins')

  assert.notEqual(result.code, 0)
  assert.notEqual(result.output.trim(), '', 'a failure exit must never be silent')
  assert.doesNotMatch(result.output, /is out of sync/)
  assert.match(result.output, /NOT inventory drift/)
  assert.match(result.output, /bazel server terminated abruptly during query/)
})

test('a failed manual-tests query is reported as a host failure, not ledger drift', () => {
  const result = runDrift('query-fail-manual')

  assert.notEqual(result.code, 0)
  assert.notEqual(result.output.trim(), '', 'a failure exit must never be silent')
  assert.doesNotMatch(result.output, /is out of sync/)
  assert.match(result.output, /NOT ledger drift/)
  assert.match(result.output, /bazel server terminated abruptly during query/)
})

test('an empty but successful go_binary query is not reported as inventory drift', () => {
  const result = runDrift('query-empty-bins')

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(result.output, /is out of sync/)
  assert.match(result.output, /zero labels/)
  // KTD3's trade-off: name the committed file so a legitimate delete-everything change is
  // one grep away from the operator reading this message.
  assert.match(result.output, /scripts\/bazel\/binary-inventory\.json/)
})

test('an empty but successful manual-tests query is not reported as ledger drift', () => {
  const result = runDrift('query-empty-manual')

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(result.output, /is out of sync/)
  assert.match(result.output, /zero labels/)
  assert.match(result.output, /scripts\/bazel\/ledger\.json/)
})

test('query stderr noise never enters the captured label set', () => {
  const result = runDrift('query-noise')

  assert.equal(result.code, 0, result.output)
  assert.match(result.stdout, /binary-inventory\.json matches the live go_binary graph/)
  assert.match(result.stdout, /ledger\.json matches the live manual-tagged test set/)
})

test('genuine inventory drift still emits the existing message and diff body', () => {
  const result = runDrift('query-drift-bins')

  assert.notEqual(result.code, 0)
  assert.match(
    result.output,
    /ERROR: scripts\/bazel\/binary-inventory\.json is out of sync with 'bazel query kind\(go_binary, \/\/\.\.\.\)':/,
  )
  assert.match(result.output, /in query but missing from inventory/)
  assert.match(result.output, /< \/\/services\/delta\/cmd:cmd/)
})

test('genuine ledger drift still emits the existing message and diff body', () => {
  const result = runDrift('query-drift-manual')

  assert.notEqual(result.code, 0)
  assert.match(
    result.output,
    /ERROR: scripts\/bazel\/ledger\.json is out of sync with 'bazel query attr\(tags,"manual",tests\(\/\/\.\.\.\)\)':/,
  )
  assert.match(result.output, /ledger label no longer a manual target/)
  assert.match(result.output, /> \/\/services\/beta:beta_test/)
})
