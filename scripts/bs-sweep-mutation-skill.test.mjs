// Content/contract test for the bs-sweep-mutation skill (BOS-145, child 2 of the
// BOS-143 token-efficiency epic).
//
// Follows the "content-test file pattern" BOS-144 established
// (scripts/bs-<skill>-skill.test.mjs, wired into `make test-smoke` via the
// `scripts/bs-*-skill.test.mjs` glob). It pins the byte-stable external contracts the
// SKILL.md documents — the subagent dispatch directives for the mutation run,
// per-survivor cycles, Phase-B batch, and completion watch; the run-file sentinel token
// vocabularies (sourced from the helper modules, the single source of truth); the
// dead-subagent / dispatch-failure branch; the compact-survivor contract; and the
// completion contract — plus a loss-check that the deterministic compact-survivor
// extraction reproduces EVERY survivor present in a captured mutation-suite output.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { REPAIR_RESULTS, DISPATCH_FAILURE } from './bs-run-sentinel.mjs'
import {
  MUTATION_RESULTS,
  PER_MUTANT_RESULTS,
  PHASE_B_RESULTS,
  extractSurvivors,
  extractUncoveredRows,
  extractCoverageRows,
} from './bs-sweep-mutation-survivors.mjs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const SKILL = read('../.claude/skills/bs-sweep-mutation/SKILL.md')
const CODEX = read('../.codex/skills/bs-sweep-mutation/SKILL.md')

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
// Dispatch directives — the three heavy steps run in awaited subagents.
// ---------------------------------------------------------------------------

test('every heavy step dispatches a general-purpose subagent', () => {
  // Mutation run + per-survivor cycles + Phase-B batch + completion watch each name
  // the subagent type.
  assert.ok(
    countOf(SKILL, 'subagent_type: general-purpose') >= 4,
    'expected >= 4 `subagent_type: general-purpose` dispatch directives',
  )
})

test('frontmatter allows Task for the required subagent dispatches', () => {
  assert.ok(allowedTools(SKILL).includes('Task'), 'source skill must allow the Task tool')
  assert.ok(allowedTools(CODEX).includes('Task'), 'codex mirror must allow the Task tool')
})

test('every dispatch is tier-annotated as mechanical or judgment with a reason', () => {
  // BOS-133 format: `tier: mechanical|judgment — <reason>`.
  assert.ok(countOf(SKILL, '<!-- tier: ') >= 4, 'expected a `<!-- tier: ...` on each dispatch')
  assert.match(SKILL, /tier: mechanical — .+/, 'the mutation run is tier: mechanical with a reason')
  assert.match(
    SKILL,
    /tier: judgment — .+/,
    'the test-writing steps are tier: judgment with a reason',
  )
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

test('per-survivor dispatches are awaited AND sequential (no test-file races)', () => {
  assert.match(SKILL, /sequential/i, 'per-survivor dispatches must be sequential')
  assert.match(
    SKILL,
    /one awaited `general-purpose` subagent per killable survivor/i,
    'one subagent per survivor',
  )
  assert.match(
    SKILL,
    /no (parallel )?.{0,30}write race/i,
    'must state no write races by construction',
  )
})

// ---------------------------------------------------------------------------
// Run-file sentinel — the orchestrator classifies FROM THE FILE ONLY.
// ---------------------------------------------------------------------------

test('the SKILL resolves both shared helpers', () => {
  assert.ok(SKILL.includes('bs-run-sentinel.mjs'), 'must resolve the sentinel-mechanics helper')
  assert.ok(
    SKILL.includes('bs-sweep-mutation-survivors.mjs'),
    'must resolve the compact-survivor extraction helper',
  )
})

test('the SKILL documents every sentinel token from the shared helpers', () => {
  // Completion watch reuses bs-run-sentinel.mjs's REPAIR_RESULT set verbatim.
  for (const token of REPAIR_RESULTS) {
    assert.ok(SKILL.includes(token), `SKILL.md must document the REPAIR_RESULT token "${token}"`)
  }
  // Mutation-run + per-survivor sentinels use the BOS-145 token sets.
  for (const token of MUTATION_RESULTS) {
    assert.ok(SKILL.includes(token), `SKILL.md must document the MUTATION_RESULT token "${token}"`)
  }
  for (const token of PER_MUTANT_RESULTS) {
    assert.ok(SKILL.includes(token), `SKILL.md must document the PER_MUTANT token "${token}"`)
  }
  for (const token of PHASE_B_RESULTS) {
    assert.ok(SKILL.includes(token), `SKILL.md must document the PHASE_B token "${token}"`)
  }
})

test('the SKILL uses the byte-identical DISPATCH_FAILURE token', () => {
  assert.equal(DISPATCH_FAILURE, 'dispatch-failure')
  assert.ok(SKILL.includes(DISPATCH_FAILURE), 'SKILL.md must use the dispatch-failure token')
  assert.ok(
    SKILL.includes(`DISPATCH_FAILURE="${DISPATCH_FAILURE}"`),
    'the shell DISPATCH_FAILURE must equal the module constant',
  )
})

test('the orchestrator classifies from the run-file sentinel only', () => {
  assert.match(SKILL, /run file only|run-file sentinel only|from the run file only/i)
})

test('a missing/stale sentinel routes to the safe branch, never a false success', () => {
  assert.match(SKILL, /missing|stale/i)
  // The mutation-run dead-subagent branch must not fabricate a "no survivors".
  assert.match(SKILL, /never a false .{0,20}no.?survivors/i)
  // The watch dead-subagent branch must never record green.
  assert.match(SKILL, /never green|safe non-green/i)
})

test('green is re-verified by one cheap gh call, never trusted from the sentinel alone', () => {
  assert.match(SKILL, /re-verif|re-check/i)
  assert.ok(SKILL.includes('isDraft'), 'green re-verify checks the draft state')
  assert.ok(SKILL.includes('statusCheckRollup'), 'green re-verify checks PR status checks')
})

// ---------------------------------------------------------------------------
// Compact-survivor contract — the four raw views never enter the orchestrator.
// ---------------------------------------------------------------------------

test('the four raw views are never pasted inline; the payload is the compact list', () => {
  assert.match(SKILL, /never.{0,40}four raw views|four raw views never/i)
  assert.match(SKILL, /compact survivor list/i)
  assert.match(SKILL, /compact uncovered list|uncoveredRows/i)
})

test('the PR body still lists ALL survivors', () => {
  assert.match(SKILL, /lists ALL survivors|ALL survivors/i, 'PR body must list every survivor')
})

test('Phase B batch planning happens inside the subagent from compact uncovered rows', () => {
  assert.match(
    SKILL,
    /Phase-B subagent owns batch planning/i,
    'the orchestrator must not plan batches from raw uncovered output',
  )
  assert.match(
    SKILL,
    /\.mutate\/uncovered\.txt.{0,120}compact uncovered list from Step 2.{0,80}not the raw view/i,
    'Phase B must consume the compact uncovered list, not the raw view',
  )
})

test('Phase B classifies from a concrete run-file sentinel', () => {
  assert.ok(SKILL.includes('sentinel `phase-b`'), 'must name the Phase-B sentinel')
  assert.ok(SKILL.includes('read "$RUN_DIR" "$RUN_ID" phase-b'), 'must read the Phase-B sentinel')
  assert.match(SKILL, /missing\/stale `phase-b` sentinel is a `dispatch-failure`/i)
  for (const token of PHASE_B_RESULTS) {
    assert.ok(SKILL.includes(token), `SKILL.md must document Phase-B token "${token}"`)
  }
})

// ---------------------------------------------------------------------------
// Completion contract — byte-identical to baseline.
// ---------------------------------------------------------------------------

test('the completion + terminal contracts stay byte-identical', () => {
  assert.ok(SKILL.includes('READY_GREEN_PR'), 'READY_GREEN_PR terminal output preserved')
  assert.ok(SKILL.includes('NO_CHANGE'), 'NO_CHANGE terminal output preserved')
  assert.ok(
    SKILL.includes('test(mutate): kill surviving mutants'),
    'Phase A commit subject preserved',
  )
  assert.ok(SKILL.includes('test(mutate): raise coverage for'), 'Phase B commit subject preserved')
  assert.match(SKILL, /never stage `\.mutate\/`|never.{0,20}`\.mutate\/`/i, '.mutate/ never staged')
  assert.ok(SKILL.includes('gh pr checks'), 'gh pr checks watch preserved')
  assert.ok(
    SKILL.includes('node scripts/remove-bossd-stop-hooks.mjs'),
    'Stop-hook removal preserved',
  )
})

test('the manifest runtime note is present', () => {
  assert.ok(SKILL.includes('test-command-manifest.md'), 'must note the manifest bump')
  assert.ok(SKILL.includes('make test-manifest-update'), 'must run test-manifest-update')
})

// ---------------------------------------------------------------------------
// Codex mirror — must carry the same dispatch + sentinel tokens (no un-synced drift).
// ---------------------------------------------------------------------------

test('the .codex mirror carries the same dispatch + sentinel tokens', () => {
  assert.ok(
    countOf(CODEX, 'subagent_type: general-purpose') >= 4,
    'codex mirror must carry every dispatch directive',
  )
  assert.ok(CODEX.includes(DISPATCH_FAILURE), 'codex mirror must carry the dispatch-failure token')
  for (const token of [...REPAIR_RESULTS, ...MUTATION_RESULTS, ...PER_MUTANT_RESULTS]) {
    assert.ok(CODEX.includes(token), `codex mirror must document sentinel token "${token}"`)
  }
  for (const token of PHASE_B_RESULTS) {
    assert.ok(CODEX.includes(token), `codex mirror must document Phase-B token "${token}"`)
  }
})

// ---------------------------------------------------------------------------
// Loss-check — the deterministic extraction reproduces EVERY survivor in a
// captured mutation-suite output. This is the gate that must pass before the
// cheap-tier mutation run ships; any dropped survivor fails here.
// ---------------------------------------------------------------------------

const survivorsFixture = read('./fixtures/bs-sweep-mutation/survivors.txt')
const uncoveredFixture = read('./fixtures/bs-sweep-mutation/uncovered.txt')
const coverageFixture = read('./fixtures/bs-sweep-mutation/coverage.txt')

test('loss-check: extractSurvivors reproduces every survivor in the fixture', () => {
  const survivors = extractSurvivors(survivorsFixture)

  // The canonical set of unique, well-formed survivor lines present in the fixture.
  const expected = [
    '[bosso--internal-auth] internal/auth/token.go:42 CONDITIONALS_BOUNDARY',
    '[bosso--internal-auth] internal/auth/token.go:58 INVERT_NEGATIVES',
    '[bosso--internal-auth] internal/auth/jwt.go:113 ARITHMETIC_BASE',
    '[boss--internal-client] internal/client/socket.go:77 CONDITIONALS_NEGATION',
    '[bossd--internal-server] internal/server/session.go:204 INCREMENT_DECREMENT',
    '[bossd--internal-server] internal/server/session.go:204 INCREMENT_DECREMENT',
    '[bossd--internal-server] internal/server/lifecycle.go:319 REMOVE_SELF_ASSIGNMENTS',
  ]
  assert.deepEqual(survivors, expected, 'every survivor must be reproduced, in order')

  // Belt-and-suspenders: no well-formed survivor line in the fixture is dropped.
  const fixtureSurvivors = new Set(
    survivorsFixture
      .split(/\r?\n/)
      .map((l) => l.trim())
      .filter((l) => /^\[[^\]]+\]\s+\S+:\d+\s+\S+/.test(l)),
  )
  for (const line of fixtureSurvivors) {
    assert.ok(survivors.includes(line), `dropped survivor: ${line}`)
  }
})

test('extractUncoveredRows reproduces compact uncovered rows for Phase B', () => {
  const rows = extractUncoveredRows(uncoveredFixture)
  assert.deepEqual(rows, [
    '[bossd--internal-server] internal/server/session.go:204 CONDITIONALS_BOUNDARY',
    '[bossd--internal-server] internal/server/session.go:211 INVERT_NEGATIVES',
    '[bosso--internal-auth] internal/auth/token.go:88 CONDITIONALS_NEGATION',
  ])
})

test('extractSurvivors drops blanks and non-survivor noise', () => {
  const out = extractSurvivors('\n==> Surviving mutants: make mutate-survivors\n   \n')
  assert.deepEqual(out, [], 'noise-only input yields no survivors')
})

test('extractCoverageRows parses the tab-separated coverage view, lowest first', () => {
  const rows = extractCoverageRows(coverageFixture)
  assert.deepEqual(
    rows.map((r) => r.name),
    ['bossd--internal-server', 'bosso--internal-auth', 'boss--internal-client', 'bossd--cmd'],
  )
  assert.equal(rows[0].coverage, '31.2')
  assert.equal(rows[0].notCovered, 22)
})
