// Content/contract test for the bs-sweep-security skill (BOS-144).
//
// This is the "content-test file pattern" the BOS-143 epic's children 2–5 copy:
// scripts/bs-<skill>-skill.test.mjs, wired into `make test-smoke` via the
// `scripts/bs-*-skill.test.mjs` glob. It pins the byte-stable external contracts
// the SKILL.md documents — the two subagent dispatch directives, the run-file
// sentinel tokens (sourced from skills-toolbox/bs-run-sentinel.mjs, the single source of
// truth), the dead-subagent/dispatch-failure branch, and the dry-run contract —
// plus a behavior-parity check that the triage gate selects a known batch on a
// non-empty fixture set.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { REPAIR_RESULTS, DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'

const readSkill = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const SKILL = readSkill('../.claude/skills/bs-sweep-security/SKILL.md')
const CODEX = readSkill('../.codex/skills/bs-sweep-security/SKILL.md')

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
// Dispatch directives — both heavy steps run in awaited subagents.
// ---------------------------------------------------------------------------

test('both heavy steps dispatch a general-purpose subagent', () => {
  // Phase 2 (triage) + Phase 4 (repair-watch) each name the subagent type.
  assert.ok(
    countOf(SKILL, 'subagent_type: general-purpose') >= 2,
    'expected >= 2 `subagent_type: general-purpose` dispatch directives',
  )
})

test('frontmatter allows Task for the required subagent dispatches', () => {
  assert.ok(allowedTools(SKILL).includes('Task'), 'source skill must allow the Task tool')
  assert.ok(allowedTools(CODEX).includes('Task'), 'codex mirror must allow the Task tool')
})

test('every dispatch is annotated tier: opus (judgment steps)', () => {
  assert.ok(
    countOf(SKILL, '<!-- tier: opus -->') >= 2,
    'expected a `<!-- tier: opus -->` annotation on each dispatch',
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

// ---------------------------------------------------------------------------
// Run-file sentinel — the orchestrator classifies FROM THE FILE ONLY.
// ---------------------------------------------------------------------------

test('the SKILL documents every REPAIR_RESULT token from the shared helper', () => {
  for (const token of REPAIR_RESULTS) {
    assert.ok(SKILL.includes(token), `SKILL.md must document the REPAIR_RESULT token "${token}"`)
  }
})

test('the SKILL uses the byte-identical DISPATCH_FAILURE token', () => {
  // Sourced from skills-toolbox/bs-run-sentinel.mjs so the matcher fn stays the single source.
  assert.equal(DISPATCH_FAILURE, 'dispatch-failure')
  assert.ok(SKILL.includes(DISPATCH_FAILURE), 'SKILL.md must use the dispatch-failure token')
  // The shell constant must be pinned byte-identical to the module constant.
  assert.ok(
    SKILL.includes(`DISPATCH_FAILURE="${DISPATCH_FAILURE}"`),
    'the shell DISPATCH_FAILURE must equal the module constant',
  )
})

test('repair is classified from the run-file sentinel, never from returned prose', () => {
  assert.match(SKILL, /run-file sentinel (only|only\b)|FROM THE (RUN )?FILE ONLY/i)
  assert.ok(SKILL.includes('bs-run-sentinel.mjs'), 'must resolve the sentinel helper')
})

test('a missing/stale sentinel routes to the safe non-green branch, never green', () => {
  // The dead-subagent branch: a missing sentinel is a distinct dispatch-failure that is
  // never recorded as green.
  assert.match(SKILL, /missing|stale/i)
  assert.match(SKILL, /safe non-green/i, 'must route dispatch-failure to the safe non-green branch')
  assert.match(SKILL, /never green|never .{0,20}green/i, 'must state it is never recorded green')
})

test('green is re-verified by a cheap gh call, never trusted from the sentinel alone', () => {
  assert.match(SKILL, /re-verif|re-check|re-verify/i)
  assert.ok(SKILL.includes('MERGEABLE'), 'green re-verify checks a settled MERGEABLE state')
  assert.ok(SKILL.includes('statusCheckRollup'), 'green re-verify checks PR status checks')
  assert.ok(SKILL.includes('CHECKS_OK'), 'green re-verify requires successful checks')
})

test('triage subagent output is validated before jq field parsing', () => {
  assert.ok(
    SKILL.includes('invalid gate select-batch output'),
    'invalid triage output must block loudly',
  )
  assert.match(SKILL, /has\("manifest"\).*has\("ecosystem"\).*has\("batch"\)/s)
  assert.match(SKILL, /\.batch \| type == "array"/)
})

test('classify CLI outcome is decoded before shell branching', () => {
  assert.ok(
    SKILL.includes(
      'OUTCOME="$(node "$GATE" classify "$REPAIR_RESULT" "$MERGEABLE" "$REVIEW_DECISION" | jq -r \'.\')"',
    ),
    'classify emits a JSON string, so the shell must decode it before case handling',
  )
})

// ---------------------------------------------------------------------------
// Byte-identical external contracts (unchanged from baseline).
// ---------------------------------------------------------------------------

test('the --dry-run contract is byte-identical: zero GitHub writes, clean tree', () => {
  assert.ok(SKILL.includes('--dry-run'), 'dry-run flag documented')
  assert.match(SKILL, /zero.{0,40}GitHub writes/i, 'dry-run makes zero GitHub writes')
  assert.ok(SKILL.includes('git status --porcelain'), 'clean-tree contract documented')
})

test('the PR sentinels and sticky markers stay byte-identical', () => {
  assert.ok(SKILL.includes('bs-sweep-security:ghsa:<id>'), 'ghsa PR sentinel documented')
  assert.ok(SKILL.includes('<!-- bs-sweep-security:state -->'), 'state sticky marker documented')
})

test('the 3-attempt poison-pill budget is preserved', () => {
  assert.match(SKILL, /\b3\b.{0,40}no-progress|3 no-progress|at most \*\*3\*\*/i)
})

// ---------------------------------------------------------------------------
// Codex mirror — must carry the same dispatch tokens (no un-synced drift).
// ---------------------------------------------------------------------------

test('the .codex mirror carries the same dispatch + sentinel tokens', () => {
  assert.ok(
    countOf(CODEX, 'subagent_type: general-purpose') >= 2,
    'codex mirror must carry both dispatch directives',
  )
  assert.ok(CODEX.includes(DISPATCH_FAILURE), 'codex mirror must carry the dispatch-failure token')
  for (const token of REPAIR_RESULTS) {
    assert.ok(CODEX.includes(token), `codex mirror must document REPAIR_RESULT token "${token}"`)
  }
})

// ---------------------------------------------------------------------------
// Triage behavior-parity — the gate selects a known batch on a NON-EMPTY set.
// The subagent-in-the-loop path runs exactly this gate CLI, so pinning the CLI
// selection proves the triage behavior did not change.
// ---------------------------------------------------------------------------

const gatePath = fileURLToPath(new URL('./sweep-security-gate.mjs', import.meta.url))
const alertsFixture = fileURLToPath(
  new URL('./fixtures/bs-sweep-security/alerts.sample.json', import.meta.url),
)
const prsFixture = fileURLToPath(
  new URL('./fixtures/bs-sweep-security/prs.sample.json', import.meta.url),
)

test('triage parity: gate select-batch picks the expected non-empty batch on fixtures', () => {
  const res = spawnSync(
    process.execPath,
    [gatePath, 'select-batch', alertsFixture, prsFixture, '10'],
    {
      encoding: 'utf8',
    },
  )
  assert.equal(res.status, 0, res.stderr)
  const sel = JSON.parse(res.stdout)

  assert.equal(sel.manifest, 'pnpm-lock.yaml')
  assert.equal(sel.ecosystem, 'npm')

  // Batch is non-empty and ranked by score desc (critical axios before high lodash).
  assert.deepEqual(
    sel.batch.map((a) => a.number),
    [102, 101],
  )
  assert.deepEqual(
    sel.batch.map((a) => a.security_advisory.ghsa_id),
    ['GHSA-axios-0002', 'GHSA-lodash-0001'],
  )

  // Deferred covers the major-version bump; dropped covers the no-patch alert.
  assert.deepEqual(sel.deferred, [
    { number: 103, ghsa: 'GHSA-next-0003', reason: 'major version bump' },
  ])
  assert.deepEqual(sel.dropped, [
    { number: 104, ghsa: 'GHSA-request-0004', reason: 'no patched version' },
  ])
})
