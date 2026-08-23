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
import { fileURLToPath } from 'node:url'
import { REPAIR_RESULTS, DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'
import { hasOpenCronPR } from './cron-open-pr.mjs'
import { rewriteClaudeSkillMarkdown } from './sync-codex-skills.mjs'
import { assertExactSize, assertMirrorRegenerated, measureFile } from './size-ratchet-lib.mjs'

const here = (rel) => new URL(rel, import.meta.url)
const abs = (rel) => fileURLToPath(here(rel))
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
  assert.match(SKILL, /bossd\s+injects/i, 'must state bossd injects [#N]')
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
  assert.match(
    SKILL,
    /run\s+file\s+only|run-file\s+sentinel\s+only|from\s+the\s+run\s+file\s+only/i,
  )
})

test('a missing/stale sentinel routes to the safe branch, never a false success', () => {
  assert.match(SKILL, /missing|stale/i)
  // The watch dead-subagent branch must never record green.
  assert.match(SKILL, /never\s+green|safe\s+non-green/i)
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
    assert.match(skill, /not\s+decrease/i, `${label} must require coverage does not decrease`)
    assert.match(
      skill,
      /module.{0,20}test\s+gate.{0,20}still\s+passes?/i,
      `${label} must require the module gate still passes`,
    )
  }
})

test('the web coverage command is measurable and names the baseline metric', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    assert.ok(
      skill.includes('make -C services/web test-coverage'),
      `${label} must use the dedicated web coverage target`,
    )
    assert.doesNotMatch(
      skill,
      /make\s+-C\s+services\/web\s+test\s+--\s+--coverage/,
      `${label} must not use the broken make goal-forwarding form`,
    )
    assert.match(
      skill,
      /total\s+statements[\s>]+percentage\s+from[\s>]+`services\/web\/coverage\/coverage-summary\.json`/i,
      `${label} must name the json-summary total statements baseline metric`,
    )
  }
})

// The kill-set gate is the PRIMARY proof for a Go removal. Coverage proves a line ran;
// only mutation proves the test asserted anything about it, so a coverage-only gate both
// admits tests that assert nothing and rejects tests that quietly kill mutants. These
// assertions stop a future edit from silently reverting to the weaker instrument.
test('the mutation kill-set guardrail is stated in both mirrors', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    assert.match(skill, /kill-set/i, `${label} must name the kill-set guardrail`)
    assert.match(skill, /mutants_killed/, `${label} must compare the concrete mutants_killed field`)
    assert.match(
      skill,
      /make\s+mutate-pkg/,
      `${label} must use the same per-package mutation target as bs-sweep-mutation`,
    )
  }
})

test('a Go removal may not fall back to coverage alone when gremlins is absent', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    assert.match(skill, /HAVE_GREMLINS/, `${label} must probe for gremlins in preflight`)
    assert.match(
      skill,
      /inadmissible|never (?:substitute|fall\s+back)/i,
      `${label} must make a Go candidate inadmissible when the kill-set proof is unavailable`,
    )
  }
})

test('the removal sentinel reports the kill-set, and shrinking it is a rejection', () => {
  assert.match(
    SKILL,
    /kill-set (?:shrank|≥|>=)/i,
    'the removed/rejected contract must key on the kill-set',
  )
  assert.match(
    SKILL,
    /would\s+shrink\s+the\s+kill-set: reject/i,
    'failure handling must reject a candidate whose removal shrinks the kill-set',
  )
})

test('judgment guardrails protect invariant / regression / golden / security tests', () => {
  assert.match(
    SKILL,
    /documented\s+invariant/i,
    'must protect the last test of a documented invariant',
  )
  assert.match(SKILL, /regression/i, 'must protect a bug-linked regression test')
  assert.match(SKILL, /golden|snapshot/i, 'must protect a golden/snapshot contract')
  assert.match(SKILL, /security/i, 'must protect a security-path test')
})

test('the skill never adds coverage and never edits production code beyond a mechanical follow-on', () => {
  assert.match(SKILL, /never\s+adds? coverage|does\s+not\s+add\s+coverage/i)
  assert.match(SKILL, /mechanical\s+follow-on/i)
  assert.match(
    SKILL,
    /bs-sweep-debt|bs-sweep-mutation/,
    'must call out the non-overlap with the sibling sweeps',
  )
})

test('the obsolete test-file-count manifest instruction is absent', () => {
  assert.doesNotMatch(SKILL, /make\s+test-manifest-update/)
})

// ---------------------------------------------------------------------------
// Completion / Stop-hook contract.
// ---------------------------------------------------------------------------

test('the completion contract owns PR readiness and stop-hook removal', () => {
  // BOS-640: `gh pr create` / `gh pr ready` moved into skills-toolbox/sweep-pr-gate.sh. The
  // skill still OWNS both — by invoking the gate — so the pins repoint at the invocation
  // rather than being dropped. scripts/sweep-pr-gate.test.mjs pins the moved bytes themselves.
  assert.ok(
    SKILL.includes('bash "$(git rev-parse --show-toplevel)/skills-toolbox/sweep-pr-gate.sh")"'),
    'skill owns PR creation + readiness by executing the extracted PR gate',
  )
  assert.ok(SKILL.includes('export PR_NUMBER'), 'the gate exports the PR number for later phases')
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
    assert.match(gate, /Bossanova[ ]sweep[ ]tests/, `${label} gate must match the live cron name`)
    assert.match(gate, /'bs-sweep-tests'/, `${label} gate must carry the legacy cron name`)
    assert.match(
      gate,
      /bs-sweep-tests[ ]gate: prior[ ]sweep[ ]PR[ ]still[ ]open/,
      `${label} skip reason must be loud`,
    )
    assert.match(gate, /gateExit\(false/, `${label} gh errors must fail closed`)
    assert.match(skill, /GateCommand/, `${label}/SKILL.md must document the gate command`)
  }
})

test('the gate CRON_NAMES pair drives hasOpenCronPR suppression', () => {
  assert.match(GATE, /CRON_NAMES = \['Bossanova[ ]sweep[ ]tests', 'bs-sweep-tests'\]/)
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
    '../skills-toolbox/sweep-pr-gate.sh',
    '../skills-toolbox/linear-gate-lib.mjs',
    '../scripts/cron-open-pr.mjs',
  ]
  for (const rel of referenced) {
    assert.ok(existsSync(here(rel)), `referenced file must exist: ${rel}`)
  }
})

// ---------------------------------------------------------------------------
// BOS-640 — the extracted PR gate, and the ratchet that keeps its saving spent.
// ---------------------------------------------------------------------------

test('the extracted PR gate is referenced in both mirrors and exists on disk', () => {
  // No COMMON_REWRITES rule matches a bare repo-root `skills-toolbox/` path, so the mirror
  // carries the invocation byte-identically; asserting BOTH trees is what catches a mirror
  // that silently lost it.
  for (const [label, skill] of [
    ['.claude/skills/bs-sweep-tests', SKILL],
    ['.codex/skills/bs-sweep-tests', CODEX],
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

test('the resident body is pinned at its exact post-extraction size', () => {
  // Measured post-extraction bodies: 26520 B (.claude) / 26601 B (.codex), down from the
  // 27050 B (.claude) / 27130 B (.codex) pre-extraction baseline. The ceiling is the larger
  // mirror + 64 B, so the 22-line PR-gate saving is actually banked and cannot be silently
  // re-spent on body regrowth — move situational content into a reference instead.
  //
  // Bumped 26665 -> 26818 for the model-tier work: the Phase 3 Model tier paragraph gained
  // the escalate contract it was missing (drop the `model:` line and revert to Opus if the
  // cheap leg ever loses findings). scripts/skill-model-tier.test.mjs now requires one — a
  // routed leg with no documented exit is the failure this ticket exists to prevent, so the
  // ceiling absorbs exactly that sentence. (The four legs that gate names all carry one;
  // bs-sweep-mutation §2 and bs-sweep-debt Phase 3 route without one and are gated only by
  // their own content tests — a real gap, out of scope here.) Bodies are re-measured below;
  // the ceiling stays the larger mirror + 64 B and still sits below the pre-extraction
  // baseline.
  //
  // BOS-653 added `START_SHA="$START_SHA" ` to the gate invocation (+23 B, no bump): bodies are
  // 26717 B (.claude) / 26798 B (.codex), leaving 20 B. Nothing further fits without a bump.
  //
  // BOS-768 replaces the ceiling with an exact pin on the AUTHORED source, and drops the
  // mirror from the byte loop entirely. Two reasons. First, a ceiling set to "larger mirror
  // + 64 B" is slack by construction: the body sat 83 B below it, so 83 B of growth — or any
  // trim at all — moved nothing. Second, the `.codex` copy is GENERATED: it is not a second
  // artifact whose size is worth an opinion, it is a function of this one, and it is checked
  // below by regenerating it and comparing exactly. Pinning bytes on a generated file only
  // invites someone to hand-edit it up to the pin.
  // Rebased onto main at 5978bd850: #2090 rewrote the awk whole-record positionals out of the
  // published bodies, growing this one by 55 B. That is a correctness rewrite of code the body
  // must carry, not new prose, so the pin absorbs exactly it.
  // BOS-919 adds the measurable web coverage target plus its load-bearing baseline metric
  // (`services/web/coverage/coverage-summary.json` total statements percentage) to the
  // resident Phase 6 worker prompt. The repo-root-relative artifact path is required because
  // the worker's declared working directory is the session worktree.
  const SOURCE_BYTES = 26600 // exact measured .claude body, re-measured after BOS-919 web coverage artifact path
  assertExactSize({
    below: { name: 'PRE_EXTRACTION_BASELINE', value: 27050 },
    constFile: 'scripts/bs-sweep-tests-skill.test.mjs',
    constName: 'SOURCE_BYTES',
    expected: SOURCE_BYTES,
    label: 'bs-sweep-tests resident body',
    measured: measureFile(abs('../.claude/skills/bs-sweep-tests/SKILL.md')),
    path: '.claude/skills/bs-sweep-tests/SKILL.md',
    residual:
      'the references/ files this body routes to, and the gate/ scripts it invokes — bytes ' +
      'moved out of the body leave this pin entirely',
  })
})

test('the codex mirror is exactly what regenerating it from the .claude source produces', () => {
  // Replaces the old byte ceiling on the mirror. The mirror is generated by
  // `make codex-skills`, which unconditionally prepends a generated-by header — so the mirror
  // is ALWAYS larger than its source and "larger than source" can never be the tell. Exact
  // regeneration equality is, and it subsumes size: a mirror that regenerates exactly cannot
  // have drifted in any way, byte count included.
  assertMirrorRegenerated({
    mirrorPath: abs('../.codex/skills/bs-sweep-tests/SKILL.md'),
    regenerate: rewriteClaudeSkillMarkdown,
    sourcePath: abs('../.claude/skills/bs-sweep-tests/SKILL.md'),
  })
})
