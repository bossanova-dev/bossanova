// Content/contract test for the bs-sweep-tests skill (BOS-331).
//
// Follows the "content-test file pattern" (scripts/bs-<skill>-skill.test.mjs,
// auto-globbed by `node --test scripts/bs-*-skill.test.mjs`, wired into
// `make test-smoke`). It pins the byte-stable external contracts the SKILL.md
// documents — the two-terminal-state cron contract, the tagless-commit rule, the
// awaited-subagent + classify-from-sentinel-only bulk-output discipline, the
// dead-subagent / dispatch-failure branch, the 5-slug low-value-test taxonomy, the
// hard coverage-neutrality guardrail, and the gate's CRON_NAMES pair — across BOTH
// the .claude source and the generated .codex mirror.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import { REPAIR_RESULTS, DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'
import { hasOpenCronPR } from './cron-open-pr.mjs'

const here = (rel) => new URL(rel, import.meta.url)
const read = (rel) => readFileSync(here(rel), 'utf8')

const SKILL = read('../.claude/skills/bs-sweep-tests/SKILL.md')
const CODEX = read('../.codex/skills/bs-sweep-tests/SKILL.md')
const GATE = read('../.claude/skills/bs-sweep-tests/gate/gate.mjs')
const CODEX_GATE = read('../.codex/skills/bs-sweep-tests/gate/gate.mjs')
const OPENAI = read('../.claude/skills/bs-sweep-tests/agents/openai.yaml')
const NOTES_TEARDOWN =
  "Before exiting, follow `bs-record-notes` with this run's outcome. Recording is non-fatal: never change the terminal state, exit code, or `git status --porcelain`. Skip gated/no-op runs that observed nothing."

// The 5-slug low-value-test taxonomy (Phase 2), each a rotation-countable slug.
const TAXONOMY = [
  'change-detector',
  'tautological',
  'redundant-duplicate',
  'trivial-accessor',
  'over-mocked-mirror',
]

const CRON_NAMES = ['Bossanova sweep tests', 'bs-sweep-tests']

test('documents the non-fatal notes teardown contract', () => {
  for (const [label, skill] of [
    ['source', SKILL],
    ['codex mirror', CODEX],
  ]) {
    assert.ok(skill.includes(NOTES_TEARDOWN), `${label} must include the notes teardown contract`)
  }
})

/** Count non-overlapping occurrences of a literal substring. */
function countOf(haystack, needle) {
  let n = 0
  let i = 0
  for (;;) {
    const at = haystack.indexOf(needle, i)
    if (at === -1) return n
    n += 1
    i = at + needle.length
  }
}

function allowedTools(skill) {
  const match = skill.match(/^allowed-tools:\s*(.+)$/m)
  assert.ok(match, 'skill frontmatter must declare allowed-tools')
  return match[1].split(',').map((tool) => tool.trim())
}

// ---------------------------------------------------------------------------
// Frontmatter
// ---------------------------------------------------------------------------

test('frontmatter declares the skill name and a description', () => {
  assert.match(SKILL, /^name:\s*bs-sweep-tests\s*$/m, 'source name must be bs-sweep-tests')
  assert.match(CODEX, /^name:\s*bs-sweep-tests\s*$/m, 'codex mirror name must be bs-sweep-tests')
  assert.match(SKILL, /^description:\s*.+/m, 'source must declare a description')
  assert.match(CODEX, /^description:\s*.+/m, 'codex mirror must declare a description')
})

test('frontmatter allows Task for the required subagent dispatches', () => {
  assert.ok(allowedTools(SKILL).includes('Task'), 'source skill must allow the Task tool')
  assert.ok(allowedTools(CODEX).includes('Task'), 'codex mirror must allow the Task tool')
})

// ---------------------------------------------------------------------------
// Two-terminal-state cron contract.
// ---------------------------------------------------------------------------

test('both mirrors document exactly the two terminal states', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    assert.ok(skill.includes('READY_GREEN_PR'), `${label} must document READY_GREEN_PR`)
    assert.ok(skill.includes('NO_CHANGE'), `${label} must document NO_CHANGE`)
  }
})

test('the commit is tagless and bossd injects the issue tag', () => {
  assert.match(SKILL, /tagless/i, 'must state commits are tagless')
  assert.match(SKILL, /bossd injects/i, 'must state bossd injects [#N]')
  assert.match(CODEX, /tagless/i, 'codex mirror must state commits are tagless')
})

test('rotation trailers are the Test-Sweep-Category / Test-Sweep-Area pair', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    assert.ok(skill.includes('Test-Sweep-Category:'), `${label} must use Test-Sweep-Category:`)
    assert.ok(skill.includes('Test-Sweep-Area:'), `${label} must use Test-Sweep-Area:`)
  }
})

// ---------------------------------------------------------------------------
// Bulk-output discipline / awaited subagents.
// ---------------------------------------------------------------------------

test('every heavy step dispatches an awaited general-purpose subagent', () => {
  // Survey + removal/verify + completion-watch each name the subagent type.
  assert.ok(
    countOf(SKILL, 'subagent_type: general-purpose') >= 3,
    'expected >= 3 `subagent_type: general-purpose` dispatch directives',
  )
  assert.ok(
    countOf(CODEX, 'subagent_type: general-purpose') >= 3,
    'codex mirror must carry every dispatch directive',
  )
})

test('each read-back sentinel name has a matching write command in a dispatch prompt', () => {
  // The orchestrator reads the survey/watch sentinels by name; a dispatched subagent does
  // NOT inherit the orchestrator's shell env, so each prompt must hand it the concrete
  // `write "$RUN_DIR" "$RUN_ID" <name>` command under the SAME name the orchestrator reads.
  for (const name of ['survey', 'removal', 'watch']) {
    assert.ok(
      SKILL.includes(`write "$RUN_DIR" "$RUN_ID" ${name}`),
      `SKILL.md must give the ${name} subagent its explicit sentinel write command`,
    )
  }
  for (const name of ['survey', 'watch']) {
    assert.ok(
      SKILL.includes(`read "$RUN_DIR" "$RUN_ID" ${name}`),
      `the orchestrator must read the ${name} sentinel it asked the subagent to write`,
    )
  }
})

test('dispatches are awaited, never backgrounded', () => {
  assert.ok(SKILL.includes('run_in_background'), 'the never-background rule must be stated')
  assert.match(
    SKILL,
    /never.{0,40}run_in_background|run_in_background.{0,40}subagent/i,
    'must forbid run_in_background for the subagent dispatches',
  )
  assert.ok(SKILL.includes('await'), 'dispatches must say they are awaited')
})

// ---------------------------------------------------------------------------
// Run-file sentinel — classify FROM THE FILE ONLY; missing/stale => safe branch.
// ---------------------------------------------------------------------------

test('the SKILL resolves the shared sentinel helper', () => {
  assert.ok(SKILL.includes('bs-run-sentinel.mjs'), 'must resolve the sentinel-mechanics helper')
})

test('the SKILL uses the byte-identical DISPATCH_FAILURE token', () => {
  assert.equal(DISPATCH_FAILURE, 'dispatch-failure')
  assert.ok(SKILL.includes(DISPATCH_FAILURE), 'SKILL.md must use the dispatch-failure token')
  assert.ok(
    SKILL.includes(`DISPATCH_FAILURE="${DISPATCH_FAILURE}"`),
    'the shell DISPATCH_FAILURE must equal the module constant',
  )
  assert.ok(CODEX.includes(DISPATCH_FAILURE), 'codex mirror must carry the dispatch-failure token')
})

test('the orchestrator classifies from the run-file sentinel only', () => {
  assert.match(SKILL, /run file only|run-file sentinel only|from the run file only/i)
})

test('a missing/stale sentinel routes to the safe branch, never a false success', () => {
  assert.match(SKILL, /missing|stale/i)
  // The watch dead-subagent branch must never record green.
  assert.match(SKILL, /never green|safe non-green/i)
})

test('the SKILL documents the completion-watch sentinel tokens', () => {
  for (const token of REPAIR_RESULTS) {
    assert.ok(SKILL.includes(token), `SKILL.md must document the REPAIR_RESULT token "${token}"`)
  }
})

test('green is re-verified by one cheap gh call, never trusted from the sentinel alone', () => {
  assert.match(SKILL, /re-verif|re-check/i)
  assert.ok(SKILL.includes('isDraft'), 'green re-verify checks the draft state')
  assert.ok(SKILL.includes('statusCheckRollup'), 'green re-verify checks PR status checks')
})

// ---------------------------------------------------------------------------
// The low-value-test taxonomy (Phase 2).
// ---------------------------------------------------------------------------

test('both mirrors carry the full 5-slug low-value-test taxonomy', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    for (const slug of TAXONOMY) {
      assert.ok(skill.includes(slug), `${label} must document the "${slug}" taxonomy slug`)
    }
  }
})

// ---------------------------------------------------------------------------
// The hard coverage-neutrality guardrail + judgment guardrails.
// ---------------------------------------------------------------------------

test('the coverage-neutrality guardrail is stated in both mirrors', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    assert.match(
      skill,
      /coverage-neutrality/i,
      `${label} must name the coverage-neutrality guardrail`,
    )
    assert.match(skill, /not decrease/i, `${label} must require coverage does not decrease`)
    assert.match(
      skill,
      /module.{0,20}test gate.{0,20}still passes?/i,
      `${label} must require the module gate still passes`,
    )
  }
})

test('judgment guardrails protect invariant / regression / golden / security tests', () => {
  assert.match(
    SKILL,
    /documented invariant/i,
    'must protect the last test of a documented invariant',
  )
  assert.match(SKILL, /regression/i, 'must protect a bug-linked regression test')
  assert.match(SKILL, /golden|snapshot/i, 'must protect a golden/snapshot contract')
  assert.match(SKILL, /security/i, 'must protect a security-path test')
})

test('the skill never adds coverage and never edits production code beyond a mechanical follow-on', () => {
  assert.match(SKILL, /never adds? coverage|does not add coverage/i)
  assert.match(SKILL, /mechanical follow-on/i)
  assert.match(
    SKILL,
    /bs-sweep-debt|bs-sweep-mutation/,
    'must call out the non-overlap with the sibling sweeps',
  )
})

// ---------------------------------------------------------------------------
// Manifest coupling — deleting a Go _test.go file bumps the per-module count.
// ---------------------------------------------------------------------------

test('the manifest runtime note is present', () => {
  assert.ok(SKILL.includes('test-command-manifest.md'), 'must note the manifest coupling')
  assert.ok(SKILL.includes('make test-manifest-update'), 'must run test-manifest-update')
})

// ---------------------------------------------------------------------------
// Completion / Stop-hook contract.
// ---------------------------------------------------------------------------

test('the completion contract owns PR readiness and stop-hook removal', () => {
  assert.ok(SKILL.includes('gh pr create'), 'skill owns PR creation')
  assert.ok(SKILL.includes('gh pr ready'), 'skill owns PR readiness')
  assert.ok(SKILL.includes('gh pr checks'), 'skill watches checks')
  assert.ok(
    SKILL.includes('node skills-toolbox/remove-bossd-stop-hooks.mjs'),
    'Stop-hook removal preserved',
  )
})

// ---------------------------------------------------------------------------
// Cron gate — shared open-PR suppression keyed on CRON_NAMES.
// ---------------------------------------------------------------------------

test('cron gate exists and uses shared open-PR suppression', () => {
  for (const [label, skill, gate] of [
    ['.claude/skills/bs-sweep-tests', SKILL, GATE],
    ['.codex/skills/bs-sweep-tests', CODEX, CODEX_GATE],
  ]) {
    assert.match(
      gate,
      /linear-gate-lib\.mjs/,
      `${label} gate must import the shared gateExit helper`,
    )
    assert.match(gate, /cron-open-pr\.mjs/, `${label} gate must use shared cron-open-pr helper`)
    assert.match(gate, /Bossanova sweep tests/, `${label} gate must match the live cron name`)
    assert.match(gate, /'bs-sweep-tests'/, `${label} gate must carry the legacy cron name`)
    assert.match(
      gate,
      /bs-sweep-tests gate: prior sweep PR still open/,
      `${label} skip reason must be loud`,
    )
    assert.match(gate, /gateExit\(false/, `${label} gh errors must fail closed`)
    assert.match(skill, /GateCommand/, `${label}/SKILL.md must document the gate command`)
  }
})

test('the gate CRON_NAMES pair drives hasOpenCronPR suppression', () => {
  assert.match(GATE, /CRON_NAMES = \['Bossanova sweep tests', 'bs-sweep-tests'\]/)
  assert.equal(
    hasOpenCronPR([{ headRefName: 'cron-bossanova-sweep-tests-1780000000' }], CRON_NAMES),
    true,
  )
  assert.equal(hasOpenCronPR([{ headRefName: 'cron-bs-sweep-tests-1780000000' }], CRON_NAMES), true)
  assert.equal(hasOpenCronPR([], CRON_NAMES), false)
  assert.equal(
    hasOpenCronPR([{ headRefName: 'cron-bossanova-sweep-mutation-1780000000' }], CRON_NAMES),
    false,
    'must not match a different sweep cron',
  )
})

test('the gate is node-builtins only (no bare third-party imports)', () => {
  const imports = [...GATE.matchAll(/^import\s+.*?from\s+'([^']+)'/gm)].map((m) => m[1])
  for (const spec of imports) {
    const ok = spec.startsWith('node:') || spec.startsWith('.') || spec.startsWith('/')
    assert.ok(ok, `gate import "${spec}" must be a node: builtin or a relative path`)
  }
})

// ---------------------------------------------------------------------------
// Codex interface card.
// ---------------------------------------------------------------------------

test('the openai.yaml interface card is retargeted to bs-sweep-tests', () => {
  assert.match(OPENAI, /display_name:/, 'must declare a display_name')
  assert.match(OPENAI, /short_description:/, 'must declare a short_description')
  assert.match(OPENAI, /bs-sweep-tests/, 'default_prompt must name bs-sweep-tests')
})

// ---------------------------------------------------------------------------
// Reachability — every repo-relative file the SKILL references must exist.
// ---------------------------------------------------------------------------

test('files the SKILL references are reachable', () => {
  const referenced = [
    '../.claude/skills/bs-sweep-tests/gate/gate.mjs',
    '../.claude/skills/bs-sweep-tests/toolbox/bs-run-sentinel.mjs',
    '../.codex/skills/bs-sweep-tests/gate/gate.mjs',
    '../.codex/skills/bs-sweep-tests/toolbox/bs-run-sentinel.mjs',
    '../skills-toolbox/remove-bossd-stop-hooks.mjs',
    '../skills-toolbox/linear-gate-lib.mjs',
    '../scripts/cron-open-pr.mjs',
    '../docs/testing/test-command-manifest.md',
  ]
  for (const rel of referenced) {
    assert.ok(existsSync(here(rel)), `referenced file must exist: ${rel}`)
  }
})
