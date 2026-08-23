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
import { readFileSync, existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { REPAIR_RESULTS, DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'
import { hasOpenCronPR } from './cron-open-pr.mjs'
import {
  MUTATION_RESULTS,
  PER_MUTANT_RESULTS,
  PHASE_B_RESULTS,
  extractSurvivors,
  extractUncoveredRows,
  extractCoverageRows,
} from './bs-sweep-mutation-survivors.mjs'
import { rewriteClaudeSkillMarkdown } from './sync-codex-skills.mjs'
import {
  NO_REFERENCE_REMEDY,
  assertExactSize,
  assertMirrorRegenerated,
  measureFile,
} from './size-ratchet-lib.mjs'

const here = (rel) => new URL(rel, import.meta.url)
const abs = (rel) => fileURLToPath(here(rel))
const read = (rel) => readFileSync(here(rel), 'utf8')
const SKILL = read('../.claude/skills/bs-sweep-mutation/SKILL.md')
const CODEX = read('../.codex/skills/bs-sweep-mutation/SKILL.md')
const NOTES_TEARDOWN =
  "Before exiting, follow `bs-record-notes` with this run's outcome. Recording is non-fatal: never change the terminal state, exit code, or `git status --porcelain`. Skip gated/no-op runs that observed nothing."

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

test('documents the non-fatal notes teardown contract', () => {
  for (const [label, skill] of [
    ['source', SKILL],
    ['codex mirror', CODEX],
  ]) {
    assert.ok(skill.includes(NOTES_TEARDOWN), `${label} must include the notes teardown contract`)
  }
})

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

// Return the `### ` step section whose heading starts with `prefix` (e.g. '2. Run Mutation Tests').
function stepSection(skill, prefix) {
  return skill.split(/\n### /).find((section) => section.startsWith(prefix))
}

test('the mechanical run leg carries a provider-guarded sonnet model directive (both mirrors) (BOS-323)', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    const run = stepSection(skill, '2. Run Mutation Tests')
    assert.ok(run, `${label} must have a Run Mutation Tests section`)
    // A real provider-guarded model directive: model:, the sonnet alias, and a $BOSS_AGENT
    // guard naming both the lowercase `claude` and the `codex`-omit cases.
    assert.match(run, /model:/, `${label} run leg must carry a model: directive`)
    assert.match(run, /sonnet/, `${label} run leg must dispatch on the sonnet alias`)
    assert.match(
      run,
      /\$BOSS_AGENT/,
      `${label} run leg model directive must be $BOSS_AGENT-guarded`,
    )
    assert.match(run, /\bclaude\b/, `${label} run leg guard must reference lowercase claude`)
    assert.match(run, /\bcodex\b/, `${label} run leg guard must cover the codex-omit case`)

    // The tier: judgment legs (Phase A, Phase B, completion watch) stay on Opus — no sonnet.
    for (const prefix of ['3. Phase A', '4. Phase B', '8. Create a Ready Green PR']) {
      const section = stepSection(skill, prefix)
      assert.ok(section, `${label} must have a ${prefix} section`)
      assert.ok(
        !section.includes('sonnet'),
        `${label} ${prefix} (judgment/Opus) must not carry a sonnet directive`,
      )
    }
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

test('per-survivor dispatches are awaited AND sequential (no test-file races)', () => {
  assert.match(SKILL, /sequential/i, 'per-survivor dispatches must be sequential')
  assert.match(
    SKILL,
    /one\s+awaited `general-purpose` subagent\s+per\s+killable\s+survivor/i,
    'one subagent per survivor',
  )
  assert.match(
    SKILL,
    /no (parallel )?.{0,30}write\s+race/i,
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
  assert.match(
    SKILL,
    /run\s+file\s+only|run-file\s+sentinel\s+only|from\s+the\s+run\s+file\s+only/i,
  )
})

test('a missing/stale sentinel routes to the safe branch, never a false success', () => {
  assert.match(SKILL, /missing|stale/i)
  // The mutation-run dead-subagent branch must not fabricate a "no survivors".
  assert.match(SKILL, /never\s+a\s+false .{0,20}no.?survivors/i)
  // The watch dead-subagent branch must never record green.
  assert.match(SKILL, /never\s+green|safe\s+non-green/i)
})

test('green is re-verified by one cheap gh call, never trusted from the sentinel alone', () => {
  assert.match(SKILL, /re-verif|re-check/i)
  assert.ok(SKILL.includes('isDraft'), 'green re-verify checks the draft state')
  assert.ok(SKILL.includes('statusCheckRollup'), 'green re-verify checks PR status checks')
})

test('cron gate exists and uses shared open-PR suppression', () => {
  for (const [label, skill, gate] of [
    [
      '.claude/skills/bs-sweep-mutation',
      SKILL,
      read('../.claude/skills/bs-sweep-mutation/gate/gate.mjs'),
    ],
    [
      '.codex/skills/bs-sweep-mutation',
      CODEX,
      read('../.codex/skills/bs-sweep-mutation/gate/gate.mjs'),
    ],
  ]) {
    assert.match(gate, /cron-open-pr\.mjs/, `${label} gate must use shared cron-open-pr helper`)
    assert.match(
      gate,
      /Bossanova[ ]sweep[ ]mutation/,
      `${label} gate must match the live cron name`,
    )
    assert.match(gate, /prior[ ]sweep[ ]PR[ ]still[ ]open/, `${label} skip reason must be loud`)
    assert.match(gate, /gateExit\(false/, `${label} gh errors must fail closed`)
    assert.match(skill, /GateCommand/, `${label}/SKILL.md must document the gate command`)
    assert.doesNotMatch(skill, /Intentionally\s+ungated/, `${label}/SKILL.md must not say ungated`)
    assert.doesNotMatch(
      skill,
      /Leave `GateCommand` empty/,
      `${label}/SKILL.md must not tell operators to leave the gate empty`,
    )
  }
  assert.equal(
    hasOpenCronPR(
      [{ headRefName: 'cron-bossanova-sweep-mutation-1780000000' }],
      ['Bossanova sweep mutation', 'bs-sweep-mutation'],
    ),
    true,
  )
  assert.equal(hasOpenCronPR([], ['Bossanova sweep mutation', 'bs-sweep-mutation']), false)
})

// ---------------------------------------------------------------------------
// Compact-survivor contract — the four raw views never enter the orchestrator.
// ---------------------------------------------------------------------------

test('the four raw views are never pasted inline; the payload is the compact list', () => {
  assert.match(SKILL, /never.{0,40}four\s+raw\s+views|four\s+raw\s+views\s+never/i)
  assert.match(SKILL, /compact\s+survivor\s+list/i)
  assert.match(SKILL, /compact\s+uncovered\s+list|uncoveredRows/i)
})

test('the PR body still lists ALL survivors', () => {
  assert.match(
    SKILL,
    /lists\s+ALL\s+survivors|ALL\s+survivors/i,
    'PR body must list every survivor',
  )
})

test('Phase B batch planning happens inside the subagent from compact uncovered rows', () => {
  assert.match(
    SKILL,
    /Phase-B\s+subagent\s+owns\s+batch\s+planning/i,
    'the orchestrator must not plan batches from raw uncovered output',
  )
  assert.match(
    SKILL,
    /\.mutate\/uncovered\.txt.{0,120}compact\s+uncovered\s+list\s+from\s+Step\s+2.{0,80}not\s+the\s+raw\s+view/i,
    'Phase B must consume the compact uncovered list, not the raw view',
  )
})

test('Phase B classifies from a concrete run-file sentinel', () => {
  assert.ok(SKILL.includes('sentinel `phase-b`'), 'must name the Phase-B sentinel')
  assert.ok(SKILL.includes('read "$RUN_DIR" "$RUN_ID" phase-b'), 'must read the Phase-B sentinel')
  assert.match(SKILL, /missing\/stale `phase-b` sentinel\s+is\s+a `dispatch-failure`/i)
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
  assert.match(
    SKILL,
    /never\s+stage `\.mutate\/`|never.{0,20}`\.mutate\/`/i,
    '.mutate/ never staged',
  )
  assert.ok(SKILL.includes('gh pr checks'), 'gh pr checks watch preserved')
  assert.ok(
    SKILL.includes('node skills-toolbox/remove-bossd-stop-hooks.mjs'),
    'Stop-hook removal preserved',
  )
})

test('the obsolete test-file-count manifest instruction is absent', () => {
  assert.doesNotMatch(SKILL, /make\s+test-manifest-update/)
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

// ---------------------------------------------------------------------------
// BOS-640 — the extracted PR gate, and the ratchet that keeps its saving spent.
// ---------------------------------------------------------------------------

test('the extracted PR gate is referenced in both mirrors and exists on disk', () => {
  // No COMMON_REWRITES rule matches a bare repo-root `skills-toolbox/` path, so the mirror
  // carries the invocation byte-identically; asserting BOTH trees is what catches a mirror
  // that silently lost it.
  for (const [label, skill] of [
    ['.claude/skills/bs-sweep-mutation', SKILL],
    ['.codex/skills/bs-sweep-mutation', CODEX],
  ]) {
    // Pin the EXECUTED bytes, not the bare path — the body also NAMES the helper in its
    // resident "executed, not read" prose, so a bare-path substring survives deleting the
    // fenced invocation entirely.
    assert.ok(
      skill.includes('bash "$(git rev-parse --show-toplevel)/skills-toolbox/sweep-pr-gate.sh")"'),
      `${label}/SKILL.md must execute skills-toolbox/sweep-pr-gate.sh`,
    )
    assert.ok(
      skill.includes('test -n "$PR_NUMBER"'),
      `${label}/SKILL.md must fail the block when the gate produced no PR number`,
    )
  }
  assert.ok(
    existsSync(here('../skills-toolbox/sweep-pr-gate.sh')),
    'skills-toolbox/sweep-pr-gate.sh must exist on disk',
  )
})

test('the PR-gate invocation passes this skill own body path and does not rm it', () => {
  // bs-sweep-mutation is the one caller whose PR body is a tracked scratch file
  // (.mutate/pr-body.md), not an mktemp; the helper never deletes $PR_BODY and this caller
  // deliberately keeps its no-`rm` behaviour.
  assert.ok(
    SKILL.includes('PR_BODY=.mutate/pr-body.md'),
    'the gate must be handed .mutate/pr-body.md as its PR body',
  )
  assert.ok(SKILL.includes('export PR_NUMBER'), 'the gate exports the PR number for later phases')
})

test('the resident body is pinned at its exact post-extraction size', () => {
  // Measured post-extraction bodies: 29221 B (.claude) / 29301 B (.codex), down from the
  // 29845 B (.claude) / 29924 B (.codex) pre-extraction baseline. The ceiling is the larger
  // mirror + 64 B, so the 22-line PR-gate saving is actually banked and cannot be silently
  // re-spent on body regrowth — move situational content into a reference instead.
  //
  // BOS-653 added `START_SHA="$START_SHA" ` to the gate invocation (+23 B, no bump): bodies are
  // 29244 B (.claude) / 29324 B (.codex), leaving 41 B.
  //
  // BOS-768 replaces the ceiling with an exact pin on the AUTHORED source. "Larger mirror +
  // 64 B" is slack by construction — 81 B of it by the time this landed — and a trim only
  // widened it. The `.codex` copy leaves the byte loop: it is GENERATED, and is verified
  // below by regenerating it and comparing exactly.
  //
  // The remedy is NOT "move situational content into a reference": bs-sweep-mutation has no
  // references/ directory, only agents/, gate/ and toolbox/, so that advice named a
  // destination that does not exist and left the reader with no next step.
  const SOURCE_BYTES = 28632 // exact measured .claude body, re-measured 2026-08-22 after BOS-763 removed obsolete manifest-count guidance
  assertExactSize({
    below: { name: 'PRE_EXTRACTION_BASELINE', value: 29845 },
    constFile: 'scripts/bs-sweep-mutation-skill.test.mjs',
    constName: 'SOURCE_BYTES',
    expected: SOURCE_BYTES,
    label: 'bs-sweep-mutation resident body',
    measured: measureFile(abs('../.claude/skills/bs-sweep-mutation/SKILL.md')),
    path: '.claude/skills/bs-sweep-mutation/SKILL.md',
    remedy: NO_REFERENCE_REMEDY,
    residual:
      'the gate/, agents/ and toolbox/ files this body invokes — a line moved out of the body ' +
      'into one of those is invisible to this pin',
  })
})

test('the codex mirror is exactly what regenerating it from the .claude source produces', () => {
  // Replaces the old byte ceiling on the mirror. `make codex-skills` unconditionally prepends
  // a generated-by header, so a healthy mirror is ALWAYS larger than its source — "larger than
  // source" can never be the tell. Exact regeneration equality is, and it subsumes size.
  assertMirrorRegenerated({
    mirrorPath: abs('../.codex/skills/bs-sweep-mutation/SKILL.md'),
    regenerate: rewriteClaudeSkillMarkdown,
    sourcePath: abs('../.claude/skills/bs-sweep-mutation/SKILL.md'),
  })
})
