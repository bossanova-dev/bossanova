// Content/contract test for the boss-plan skill (BOS-147).
//
// boss-plan's headless (BOSS_CRON=true) path isolates recon + plan drafting into ONE
// awaited general-purpose subagent that returns only a plan-file path + bounded
// metadata, and splits mode-exclusive prose into references/. This test follows the
// BOS-144 content-test file pattern (scripts/bs-<skill>-skill.test.mjs, wired into
// `make test-smoke` via the scripts/bs-*-skill.test.mjs glob). It pins:
//   * the headless dispatch directive (general-purpose, tier: opus, awaited),
//   * the path+metadata return contract (never the plan content),
//   * the run-file sentinel classification (DISPATCH_FAILURE sourced from the shared
//     helper, missing/stale -> safe no-Linear-write branch, ok -> re-verify plan file),
//   * the bulk-output-discipline block,
//   * a NEGATIVE assertion that the interactive-mode directives survive verbatim in
//     references/interactive-mode.md (interactive behaviour did not drift),
//   * a size-ratchet keeping the resident body below the pre-split baseline.
//
// BOS-271 collapsed the published cores onto the boss-repair single-source
// topology: the canonical committed home is the embedded skillinstall payload
// (services/boss/internal/skillinstall/skills/boss-plan/), with no .claude/.codex
// committed copy — so this test reads the skillinstall home and no longer asserts
// codex-mirror parity for the core. The repo-local boss-plan-draft extension stays
// under .claude/skills.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'
import { discoverExtensions } from '../skills-toolbox/skill-extensions.mjs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const readIfExists = (rel) => {
  const url = new URL(rel, import.meta.url)
  return existsSync(url) ? readFileSync(url, 'utf8') : ''
}

const CORE = '../services/boss/internal/skillinstall/skills/boss-plan'
const SKILL = read(`${CORE}/SKILL.md`)
const INTERACTIVE = read(`${CORE}/references/interactive-mode.md`)
const BRIEF = read(`${CORE}/references/headless-drafting-brief.md`)
const DRAFT = readIfExists('../.claude/skills/boss-plan-draft/SKILL.md')
const REPO_ROOT = fileURLToPath(new URL('..', import.meta.url))

const count = (body, needle) => body.split(needle).length - 1

function sectionBetween(body, startMarker, endMarker) {
  const start = body.indexOf(startMarker)
  assert.notEqual(start, -1, `missing section start: ${startMarker}`)
  const from = start + startMarker.length
  const end = body.indexOf(endMarker, from)
  assert.notEqual(end, -1, `missing section end after ${startMarker}: ${endMarker}`)
  return body.slice(from, end)
}

const REFERENCE_TABLE = sectionBetween(SKILL, '## On-demand references', '\n## Phase 0')
const INTERACTIVE_SECTION = sectionBetween(
  SKILL,
  '### Interactive (default `/boss-plan`)',
  '\n### Headless',
)
const HEADLESS_SECTION = sectionBetween(
  SKILL,
  '### Headless (`BOSS_CRON=true`) — dispatch ONE awaited drafting subagent',
  '\n## Phase 2.5',
)
const PHASE_4_SECTION = sectionBetween(
  SKILL,
  '## Phase 4 — Finalize the plan attachment and write back to the tracker',
  '\n## Phase 5',
)

// ---------------------------------------------------------------------------
// Headless dispatch directive — ONE awaited general-purpose subagent, tier: opus.
// ---------------------------------------------------------------------------

test('the headless path dispatches a general-purpose subagent', () => {
  assert.ok(
    HEADLESS_SECTION.includes('subagent_type: general-purpose'),
    'SKILL.md must name `subagent_type: general-purpose` for the drafting dispatch',
  )
  assert.equal(
    count(HEADLESS_SECTION, 'subagent_type: general-purpose'),
    1,
    'headless mode must contain exactly one general-purpose subagent dispatch',
  )
  assert.match(
    HEADLESS_SECTION,
    /recon.*draft|draft.*recon/is,
    'the same headless subagent must own both codebase recon and plan drafting',
  )
})

test('the drafting dispatch is annotated tier: opus (judgment step)', () => {
  assert.ok(
    HEADLESS_SECTION.includes('<!-- tier: opus -->'),
    'the dispatch must carry a `<!-- tier: opus -->` annotation',
  )
  assert.equal(
    count(HEADLESS_SECTION, '<!-- tier: opus -->'),
    1,
    'headless mode must have one tier annotation',
  )
})

test('the drafting dispatch is awaited, never backgrounded', () => {
  assert.ok(
    HEADLESS_SECTION.includes('run_in_background'),
    'the never-background rule must be stated',
  )
  assert.match(
    HEADLESS_SECTION,
    /never.{0,40}run_in_background|run_in_background.{0,40}(?:subagent|dispatch)/i,
    'must forbid run_in_background for the drafting dispatch',
  )
  assert.ok(HEADLESS_SECTION.includes('await'), 'the dispatch must say it is awaited')
})

test('headless mode has no inline drafting fallback', () => {
  assert.match(
    HEADLESS_SECTION,
    /Do \*\*not\*\* draft inline|Do not draft inline/i,
    'headless mode must explicitly forbid inline drafting',
  )
  assert.doesNotMatch(
    HEADLESS_SECTION,
    /inline fallback/i,
    'headless mode must not have an inline fallback',
  )
  assert.doesNotMatch(
    HEADLESS_SECTION,
    /draft inline \*\*once\*\*/i,
    'headless mode must not route dispatch-tool errors into inline drafting',
  )
})

// ---------------------------------------------------------------------------
// Return contract — the subagent returns path + bounded metadata, never content.
// ---------------------------------------------------------------------------

test('the return contract is path + bounded metadata, never the plan content', () => {
  assert.match(
    SKILL,
    /only the plan-file path plus a bounded metadata object/i,
    'SKILL.md must state the subagent returns only the plan-file path + bounded metadata',
  )
  assert.match(
    SKILL,
    /never the plan file's content/i,
    'SKILL.md must state the subagent never returns the plan file content',
  )
})

// ---------------------------------------------------------------------------
// Run-file sentinel — classify FROM THE FILE ONLY (BOS-144 convention).
// ---------------------------------------------------------------------------

test('the SKILL uses the byte-identical DISPATCH_FAILURE token from the shared helper', () => {
  assert.equal(DISPATCH_FAILURE, 'dispatch-failure')
  assert.ok(SKILL.includes('bs-run-sentinel.mjs'), 'SKILL.md must resolve the sentinel helper')
  assert.ok(
    SKILL.includes(`DISPATCH_FAILURE="${DISPATCH_FAILURE}"`),
    'the shell DISPATCH_FAILURE must equal the module constant',
  )
})

test('the drafting outcome is classified from the run-file sentinel only', () => {
  assert.match(
    SKILL,
    /run-file sentinel only|from the (run-)?file only/i,
    'SKILL.md must classify from the run-file sentinel only (never from returned prose)',
  )
})

test('a missing/stale sentinel routes to the safe branch: no Linear write, non-zero exit', () => {
  assert.match(SKILL, /missing/i, 'must handle a missing sentinel (dead/failed subagent)')
  assert.match(SKILL, /stale/i, 'must handle a stale (foreign leftover) sentinel')
  assert.match(
    SKILL,
    /no Linear write/i,
    'the dispatch-failure branch must make no Linear write (a half-planned issue is worse than none)',
  )
  assert.ok(SKILL.includes('exit 1'), 'the dispatch-failure branch must exit non-zero')
})

test('an ok sentinel is re-verified against a present, non-empty plan file before upload', () => {
  assert.match(
    HEADLESS_SECTION,
    /PLAN_FILE"\s+!=\s+"\$PLAN_PATH"/,
    'an ok sentinel must be rejected unless payload.planPath equals PLAN_PATH',
  )
  assert.match(
    HEADLESS_SECTION,
    /non-empty|!\s+-s "\$PLAN_FILE"/i,
    'an ok sentinel must be re-verified (plan file exists + non-empty) before trusting it',
  )
  assert.match(
    HEADLESS_SECTION,
    /metadata `planPath` must also equal `PLAN_PATH`/,
    'the returned metadata planPath must match the validated sentinel path',
  )
  assert.match(
    PHASE_4_SECTION,
    /PLAN_FILE="\$\{PLAN_FILE:-\.linear-plans\/<ISSUE-ID>-<slug>\.md\}"/,
    'Phase 4 must carry forward the already validated headless PLAN_FILE instead of overwriting it',
  )
})

test('Phase 4 step 5 warns when an auto-linked blocker is itself transitively blocked (BOS-287)', () => {
  assert.ok(
    PHASE_4_SECTION.includes('Transitive-block warning'),
    'Phase 4 step 5 must document the transitive-block warning',
  )
  assert.ok(
    PHASE_4_SECTION.includes('scripts/linear-deps-lib.mjs'),
    'the warning must reuse the scripts/linear-deps-lib.mjs cleared-state rule by name',
  )
})

// ---------------------------------------------------------------------------
// Bulk-output discipline — no raw bulk in the orchestrator.
// ---------------------------------------------------------------------------

test('the SKILL carries the bulk-output-discipline block', () => {
  assert.match(
    SKILL,
    /Bulk-output discipline/i,
    'SKILL.md must carry the bulk-output-discipline rule block',
  )
  assert.ok(
    SKILL.includes('never') && SKILL.includes('the plan body or a subagent transcript'),
    'the block must forbid pasting the plan body / subagent transcript into the orchestrator',
  )
})

// ---------------------------------------------------------------------------
// Byte-identical Linear description section contract (boss-build/bs-sweep-plan consume it).
// ---------------------------------------------------------------------------

test('the resident body documents the byte-identical description section contract', () => {
  for (const heading of [
    '## Summary',
    '## Approach',
    '## Key changes',
    '## Testing',
    '## Risks / unknowns',
    '## Acceptance criteria',
    '## Required proof',
    '## Planning',
    '## Original notes',
  ]) {
    assert.ok(
      SKILL.includes(heading),
      `SKILL.md must document the "${heading}" description section`,
    )
  }
})

// ---------------------------------------------------------------------------
// NEGATIVE assertion — interactive directives survive verbatim in the reference.
// This is the ticket's mandated proof that interactive behaviour did not drift.
// ---------------------------------------------------------------------------

test('the interactive confirm-loop options survive verbatim in references/interactive-mode.md', () => {
  for (const label of [
    'plan this one',
    'skip this one (use the next ticket)',
    'pick a different one',
    'cancel',
  ]) {
    assert.ok(
      INTERACTIVE.includes(label),
      `interactive-mode.md must preserve the confirm-loop option "${label}" verbatim`,
    )
  }
})

test('the interactive draft step resolves through discovery and the Fallback contract', () => {
  assert.ok(
    INTERACTIVE.includes('discover --core boss-plan --role draft'),
    'interactive-mode.md must discover draft extensions before drafting',
  )
  assert.match(
    `${INTERACTIVE}\n${BRIEF}`,
    /helper is missing[\s\S]*"extensions":\[\][\s\S]*portable fallback tiers still run/,
    'draft discovery must treat a missing public helper as no extensions',
  )
  assert.match(
    `${INTERACTIVE}\n${BRIEF}`,
    /discovered extension by its returned descriptor\s+`name`/i,
    'draft dispatch must use the discovered extension descriptor name',
  )
  for (const marker of ['Tier 1', 'Tier 2', 'Tier 3']) {
    assert.ok(INTERACTIVE.includes(marker), `interactive-mode.md must document ${marker}`)
  }
  assert.doesNotMatch(
    `${SKILL}\n${INTERACTIVE}\n${BRIEF}`,
    /Invoke `(?:plan-eng-review|superpowers:writing-plans)`/g,
    'boss-plan core and references must not directly invoke plan-eng-review or superpowers:writing-plans',
  )
})

test('the resident body states the draft Fallback contract', () => {
  assert.match(SKILL, /Fallback contract/, 'SKILL.md must name the Fallback contract')
  assert.match(
    SKILL,
    /extension.*host built-in.*inline prompt/is,
    'SKILL.md must state the extension -> host built-in -> inline prompt order',
  )
  assert.match(
    SKILL,
    /tiers 2\/3 suppressed when an extension exists/i,
    'SKILL.md must state that lower fallback tiers are suppressed when an extension exists',
  )
})

test('the boss-plan-draft extension is authored and discoverable', () => {
  assert.match(
    DRAFT,
    /x-boss-extension:\s*\n\s+extends: boss-plan\s*\n\s+role: draft\s*\n\s+order: 40/,
    'boss-plan-draft must declare the draft extension marker',
  )
  const { extensions, skipped } = discoverExtensions({
    core: 'boss-plan',
    role: 'draft',
    root: REPO_ROOT,
  })
  assert.deepEqual(
    extensions.map((e) => e.name),
    ['boss-plan-draft'],
    'boss-plan draft discovery must return exactly boss-plan-draft',
  )
  assert.deepEqual(skipped, [], 'boss-plan draft discovery must have zero skips')
})

test('the boss-plan-draft extension points at the core drafting brief', () => {
  assert.match(
    DRAFT,
    /\.\.\/\.\.\/\.\.\/plugins\/bossd-plugin-claude\/skilldata\/skills\/boss-plan\/references\/headless-drafting-brief\.md` Step 5/,
    'boss-plan-draft must reference the canonical core drafting brief',
  )
})

test('plan-reviewer discovery ignores the boss-plan-draft sibling', () => {
  const { extensions, skipped } = discoverExtensions({
    core: 'boss-plan',
    role: 'plan-reviewer',
    root: REPO_ROOT,
  })
  assert.deepEqual(extensions, [], 'no plan-reviewer extensions are installed by default')
  assert.deepEqual(skipped, [], 'draft siblings must not be reported as plan-reviewer skips')
})

// ---------------------------------------------------------------------------
// Mode-exclusive split — the shared drafting spec lives once, in the brief.
// ---------------------------------------------------------------------------

test('the references split is wired: SKILL points at both references', () => {
  assert.ok(
    INTERACTIVE_SECTION.includes('references/interactive-mode.md'),
    'SKILL.md must point interactive runs at references/interactive-mode.md',
  )
  assert.ok(
    HEADLESS_SECTION.includes('references/headless-drafting-brief.md'),
    'SKILL.md must hand the drafting subagent references/headless-drafting-brief.md',
  )
  assert.ok(
    REFERENCE_TABLE.includes('references/interactive-mode.md') &&
      REFERENCE_TABLE.includes('references/headless-drafting-brief.md'),
    'the reference table must document both mode-specific references',
  )
  assert.equal(
    count(HEADLESS_SECTION, 'references/headless-drafting-brief.md'),
    1,
    'headless mode must pass the brief path to the one subagent, not repeatedly read it in the orchestrator',
  )
  assert.equal(
    count(HEADLESS_SECTION, 'references/interactive-mode.md'),
    0,
    'headless mode must not route through the interactive reference',
  )
})

test('the shared drafting spec (plan-body requirements + template) lives in the brief', () => {
  assert.match(BRIEF, /## Acceptance criteria/, 'the brief carries the plan-body requirements')
  assert.match(
    BRIEF,
    /## Original notes/,
    'the brief carries the fill-in description-summary template',
  )
  assert.match(
    INTERACTIVE,
    /references\/headless-drafting-brief\.md` \*\*Step 5\*\* and\s+\*\*Step 7\*\*/,
    'interactive drafting must point at the shared Step 5/Step 7 sections that actually contain the moved template',
  )
  assert.doesNotMatch(
    BRIEF,
    /^- Dependencies:/m,
    'the subagent-returned template must not include the Dependencies line the orchestrator owns',
  )
})

test('the resident body does not duplicate the full drafting spec', () => {
  for (const marker of [
    'A first development step: **"Copy this plan to `docs/plans/<ISSUE-ID>-<slug>.md`',
    '## Step 7 — Compose the description summary',
    '<2-3 sentences: what & why>',
    '<verbatim prior description if the ticket had one',
  ]) {
    assert.ok(BRIEF.includes(marker), `brief must carry drafting marker: ${marker}`)
    assert.ok(
      !SKILL.includes(marker),
      `resident SKILL.md must not duplicate drafting marker: ${marker}`,
    )
  }
})

// ---------------------------------------------------------------------------
// Proof-harness readiness guidance (BOS-111) — boss-plan decides at plan time
// what proof each plan needs, and the stale screenshot-only claim is corrected.
// ---------------------------------------------------------------------------

const MIRROR_BRIEF = readIfExists(
  '../plugins/bossd-plugin-claude/skilldata/skills/boss-plan/references/headless-drafting-brief.md',
)

test('the brief no longer claims boss-proof is screenshot-only (stills AND video today)', () => {
  assert.equal(
    count(BRIEF, 'screenshot-only'),
    0,
    'the brief must not call boss-proof screenshot-only — it captures stills AND video today',
  )
  assert.doesNotMatch(
    BRIEF,
    /future proof type/i,
    'the brief must not describe video as a "future" proof type — it is captured today',
  )
  assert.match(
    BRIEF,
    /stills and video/i,
    'the brief must state boss-proof captures stills and video',
  )
})

test('the brief Step 5 instructs a proof-harness readiness analysis + criterion→artifact mapping', () => {
  assert.ok(
    BRIEF.includes('## Proof harness analysis'),
    'the brief must instruct a `## Proof harness analysis` readiness pass',
  )
  assert.match(
    BRIEF,
    /classify.{0,20}proof-applicability/i,
    'the readiness pass must classify proof-applicability via the shared surface gate',
  )
  assert.match(
    BRIEF,
    /proof\.mjs plan/,
    'the readiness pass must use the same `proof.mjs plan` gate the implementer uses',
  )
  assert.match(
    BRIEF,
    /map each acceptance criterion to a concrete proof artifact/i,
    'the readiness pass must map each acceptance criterion to a concrete proof artifact',
  )
  assert.match(
    BRIEF,
    /missing but buildable[\s\S]{0,200}IN-PR work/i,
    'the readiness pass must schedule buildable-but-missing affordances as in-PR work',
  )
  assert.match(
    BRIEF,
    /never call `AskUserQuestion`/,
    'the readiness pass must stay headless-safe (never AskUserQuestion)',
  )
})

test('the brief Step 7 template carries a `## Proof harness analysis` block (advisory, not a contract bump)', () => {
  // Two occurrences: the Step 5 plan-body requirement + the Step 7 fill-in template block.
  assert.equal(
    count(BRIEF, '## Proof harness analysis'),
    2,
    'the brief must carry `## Proof harness analysis` in both the Step 5 guidance and the Step 7 template',
  )
  const template = sectionBetween(BRIEF, '## Required proof', '## Why this needs a human')
  assert.ok(
    template.includes('## Proof harness analysis'),
    'the Step 7 template must place `## Proof harness analysis` between Required proof and Why this needs a human',
  )
})

test('the regenerated plugin mirror brief matches the canonical brief on the new contract strings', () => {
  assert.notEqual(
    MIRROR_BRIEF,
    '',
    'the plugin-mirror brief must exist (make copy-skills committed)',
  )
  assert.equal(
    count(MIRROR_BRIEF, 'screenshot-only'),
    0,
    'the mirror brief must also drop the stale screenshot-only claim (mirror committed in sync)',
  )
  assert.equal(
    count(MIRROR_BRIEF, '## Proof harness analysis'),
    2,
    'the mirror brief must carry the same `## Proof harness analysis` blocks as the canonical brief',
  )
})

// ---------------------------------------------------------------------------
// Epic decomposition (BOS-442) — the canonical SKILL documents the EPIC tier /
// phase / guards, and the two references carry the EPIC flow.
// ---------------------------------------------------------------------------

const EPIC_PHASE = sectionBetween(
  SKILL,
  '## Phase 2.5 — Epic decomposition (triage = EPIC only)',
  '\n## Phase 3',
)

test('the SKILL documents the EPIC decomposition phase and its plan-epic-lib core', () => {
  assert.match(
    EPIC_PHASE,
    /multiple independently-shippable\s+PRs/,
    'the phase must define EPIC as multi-PR work',
  )
  assert.match(
    EPIC_PHASE,
    /≥ 2\*\*\s+genuinely separable/,
    'the phase must require >=2 separable children',
  )
  // The estimate-as-forcing-function trigger: honest >=5 auto-triages EPIC.
  assert.match(
    EPIC_PHASE,
    /honest estimate is \*\*≥ 5\*\*/,
    'the phase must make an honest >=5 estimate auto-trigger EPIC',
  )
  assert.match(
    EPIC_PHASE,
    /Estimate is the forcing function/,
    'the phase must name estimate as the forcing function',
  )
  assert.match(
    EPIC_PHASE,
    /`8` is never a single-ticket estimate/,
    'the phase must state that 8 is never a single-ticket estimate',
  )
  for (const sym of [
    '$BOSS_PLAN_TOOLBOX/plan-epic-lib.mjs',
    'validateDecomposition',
    'assertAcyclic',
    'topoOrderChildren',
    'epicWiringPlan',
    'reconcileEpicChildren',
    'EPIC_MIN_CHILDREN',
    'EPIC_MAX_CHILDREN',
  ]) {
    assert.ok(EPIC_PHASE.includes(sym), `the phase must name the plan-epic-lib symbol ${sym}`)
  }
})

// ---------------------------------------------------------------------------
// BOS-652 — Phase 2.5's own deterministic core (skills-toolbox/plan-epic-phase25.mjs)
// must be cited by CALL, in both the SKILL phase and the headless brief.
// ---------------------------------------------------------------------------

test('BOS-652: Phase 2.5 cites plan-epic-phase25.mjs by CALL, not by bare name', () => {
  // The citation form is the whole gate. scripts/check-skill-symbols.mjs check 2 only sees an
  // inline code span that STARTS with a camelCase identifier followed by `(` (its CALL_CITATION
  // regex), so a bare `detectEpicParent` mention passes that check VACUOUSLY — it is never
  // extracted, never resolved against the module's export set, and therefore never breaks when
  // the export is renamed or deleted. Pin the literal `name(` prefix so a future edit that
  // degrades a citation back to a bare mention (silently un-gating it) fails here instead.
  assert.ok(
    EPIC_PHASE.includes('$BOSS_PLAN_TOOLBOX/plan-epic-phase25.mjs'),
    'the phase roll-call must name the plan-epic-phase25.mjs module by toolbox path',
  )
  for (const call of [
    '`detectEpicParent(',
    '`epicSpecRecoveryGate(',
    '`epicPhase25WritePlan(',
    '`stalePlanAttachmentSweep(',
  ]) {
    assert.ok(
      EPIC_PHASE.includes(call),
      `the phase must cite ${call}…)\` as a CALL — a bare backticked name is not a gated citation`,
    )
    assert.ok(
      BRIEF.includes(call),
      `the headless brief must cite ${call}…)\` as a CALL — the brief drives the headless path, where nothing else states the rule`,
    )
  }
  assert.ok(
    BRIEF.includes('$BOSS_PLAN_TOOLBOX/plan-epic-phase25.mjs'),
    'the brief must name the plan-epic-phase25.mjs module by toolbox path',
  )
})

test('BOS-652: the emitted write plan carries BOTH of its execution qualifiers', () => {
  // `epicPhase25WritePlan` is not a complete script, and each qualifier stops a
  // distinct duplication bug that no code gate can catch — the emitter is pure,
  // so only the prose tells a reader when NOT to execute an emitted op:
  //   * stage-level — it emits the three spec-upload ops unconditionally, while
  //     stage 2 skips them (and stage 3 with them) whenever either store already
  //     holds a spec; a finalize mints a NEW attachment row every call, so
  //     executing them anyway lands the parent in the permanently-aborting
  //     duplicate state.
  //   * child-level — it emits one `createChild` per SPEC child, never per
  //     MISSING child, while the resume path must create only the spec keys
  //     `reconcileEpicChildren` reports as `missing`; unfiltered, a resume
  //     duplicates every child that already exists.
  // Both live in prose alone, so pin them here or a future edit deletes them
  // silently.
  for (const [name, text] of [
    ['SKILL.md Phase 2.5', EPIC_PHASE],
    ['the headless brief', BRIEF],
  ]) {
    assert.match(
      text,
      /minus any stage the\s+preconditions below skip/,
      `${name} must qualify the emitted order with the stage-level skip`,
    )
    assert.match(
      text,
      /minus every child `reconcileEpicChildren` does NOT\s+report `missing`/,
      `${name} must qualify the emitted order with the resume's missing-set filter`,
    )
  }
})

test('the SKILL documents every load-bearing epic guard', () => {
  // >=2 & <=12 children
  assert.match(EPIC_PHASE, /EPIC_MAX_CHILDREN = 12/, 'must state the 12-child cap')
  assert.match(EPIC_PHASE, /EPIC_MIN_CHILDREN = 2/, 'must state the 2-child minimum')
  // per-child single-PR estimate ceiling (the forcing function) + never-a-monolith escape valve
  assert.match(
    EPIC_PHASE,
    /CHILD_MAX_ESTIMATE = 3/,
    'must state the per-child single-PR estimate ceiling',
  )
  assert.match(
    EPIC_PHASE,
    /never\*\*\s*a single oversized ticket|needs-human/i,
    'over the child cap must fall to needs-human, never a single oversized ticket',
  )
  // recursion guard
  assert.match(EPIC_PHASE, /allowEpic: false/, 'must state the allowEpic:false recursion guard')
  assert.match(
    EPIC_PHASE,
    /no child recursion|never itself decomposed/i,
    'must forbid child recursion',
  )
  // cycle safety
  assert.match(EPIC_PHASE, /cycle safety|blockedByKeys` cycle/i, 'must state cycle safety')
  // validate-before-write atomicity
  assert.match(
    EPIC_PHASE,
    /validate everything locally BEFORE the first Linear write|validate-before-write/i,
    'must state the validate-before-write atomicity guard',
  )
  assert.match(EPIC_PHASE, /zero\s+Linear writes/i, 'must state zero writes on failure')
  // idempotent resume
  assert.match(EPIC_PHASE, /idempotent resume/i, 'must state idempotent resume')
  assert.match(EPIC_PHASE, /adopts/i, 'must state that re-runs adopt existing children')
  assert.match(EPIC_PHASE, /clean no-op/i, 'must state a fully-built epic re-run is a no-op')
  // original-becomes-parent + parent-label exception
  assert.match(EPIC_PHASE, /original-becomes-parent/i, 'must state original-becomes-parent')
  assert.match(EPIC_PHASE, /Parent-label exception/, 'must state the parent-label exception')
  assert.match(
    EPIC_PHASE,
    /neither\*\*\s*`agent-friendly`\s*\*\*nor\*\*\s*`needs-human`/,
    'the parent must carry neither agent-friendly nor needs-human',
  )
  // per-child planContract-v1 + intra-epic DAG + external links
  assert.match(EPIC_PHASE, /planContract-v1/, 'children must be full planContract-v1 plans')
  assert.match(EPIC_PHASE, /external conflict links/i, 'must wire external conflict links')
  // reconcileEpicChildren's ambiguous-drift branch must be pinned SAFE (report, no writes, no
  // guessing) and a refusal must never degrade to "no children exist" — the whole-epic-duplication
  // hazard reconcileEpicChildren's own refuse() guards against.
  assert.match(
    EPIC_PHASE,
    /\*\*Ambiguous\s+drift\*\*[\s\S]*?ok:false[\s\S]*?SAFE\s+branch[\s\S]*?report\s+`errors`,\s+write\s+nothing,\s+create\s+nothing,\s+never\s+guess[\s\S]*?never\s+be\s+read\s+as\s+"no\s+children\s+exist"/,
    'ambiguous drift (multiple orphans, an unmarked child, duplicate live keys, or a non-array input) must take the SAFE branch and a refusal must never be read as "no children exist"',
  )
  // Pin the RESUME-STEP CALL itself, by its wording, exactly as the brief's mandate is pinned. The
  // symbol loop above is satisfied by the unrelated `reconcileEpicChildren` mention in the phase's
  // toolbox-symbol list, and the ambiguous-drift regex above matches only the outcome paragraph — so
  // without this assertion the resume step could stop calling the function and every check stays
  // green. That is the same failure mode the brief's outcome assertions already guard against.
  assert.match(
    EPIC_PHASE,
    /reconcileEpicChildren\(spec,\s*liveChildren\)`\s*—\s*never by eye,\s*never\s*\n?by title/,
    'the phase must mandate reconcileEpicChildren as the idempotent-resume join, not an eyeball or title match',
  )
  // The unambiguous-rename repair must be aimed at the CHILD's marker, never at the spec key:
  // `specKey` is the namespace `adopted`, the siblings' `blockedByKeys` and `epicWiringPlan` all
  // resolve through, so re-pointing the spec at `liveKey` strands those refs and throws mid-wire.
  assert.match(
    EPIC_PHASE,
    /rewrite\s+\*\*its own\*\*\s+description marker to `epicChildMarker\(specKey\)`[\s\S]*?never the spec key/,
    'the unambiguous rename must repair the child marker, never re-point the spec key',
  )
  // The repair is a description WRITE, and the tracker save replaces the description (the same
  // hazard this phase already warns about at its other two marker-write points). Without the
  // preserve-the-bytes clause, a literal `save_issue(id, description: epicChildMarker(specKey))`
  // wipes the child's gated plan body while the child still reads as adopted — a silent loss.
  assert.match(
    EPIC_PHASE,
    /replacing\s*\n?only the marker substring and \*\*preserving the rest of that description's bytes verbatim\*\*/,
    'the rename repair must preserve the rest of the child description, since the save replaces it',
  )
})

test('both references carry the EPIC triage tier and flow', () => {
  // interactive
  assert.match(INTERACTIVE, /\*\*EPIC\*\*/, 'interactive-mode must add the EPIC triage tier')
  assert.match(
    INTERACTIVE,
    /Epic decomposition \(interactive: propose → confirm → create\)/,
    'interactive-mode must carry the propose-confirm-create flow',
  )
  assert.match(
    INTERACTIVE,
    /create this epic/i,
    'interactive AskUserQuestion must offer create-this-epic',
  )
  assert.match(
    INTERACTIVE,
    /plan as one ticket/i,
    'interactive must offer the single-ticket option',
  )
  assert.match(
    INTERACTIVE,
    /allowEpic: false/,
    'interactive per-child drafting must pass allowEpic:false',
  )
  // headless
  assert.match(BRIEF, /\*\*EPIC\*\*/, 'the brief must add the EPIC triage tier')
  assert.match(
    BRIEF,
    /Epic decompose-and-auto-create/,
    'the brief must carry the headless auto-create flow',
  )
  assert.match(
    BRIEF,
    /allowEpic: false/,
    'the brief must document the allowEpic:false recursion guard',
  )
  assert.match(
    BRIEF,
    /fall back to a single-ticket plan and record the reason/i,
    'the brief must document the single-ticket fallback on guard failure',
  )
  assert.match(
    BRIEF,
    /reconcileEpicChildren\(spec,\s*liveChildren\)`\s*—\s*never adopt by eye,\s*never by title/,
    'the brief must mandate reconcileEpicChildren as the idempotent-resume join, not an eyeball match',
  )
  // Pin the three reconcileEpicChildren outcomes by their actual wording, not just the symbol's
  // presence — a resume step that stops calling the function while the identifier still appears
  // elsewhere in the file (e.g. only in the return-shape doc) must not stay green.
  assert.match(
    BRIEF,
    /\*\*\(1\)\s+aligned\*\*\s*\(no orphans\)\s*—\s*create exactly the spec keys `missing` names/,
    'the brief must document outcome (1) aligned: create exactly the missing spec keys',
  )
  assert.match(
    BRIEF,
    /\*\*\(2\)\s+unambiguous rename\*\*\s*\(`repairs` holds exactly one `\{specKey,\s*liveKey,\s*id\}`/,
    'the brief must document outcome (2) unambiguous rename: repairs holds exactly one entry',
  )
  assert.match(
    BRIEF,
    /\*\*\(3\)\s+ambiguous drift\*\*\s*\(`ok:false`\s*—\s*multiple orphans,\s*an unmarked child,\s*duplicate live marker\s+keys,\s*or a non-array `liveChildren`\)\s*—\s*take the SAFE branch:\s*report `errors`,\s*write nothing,\s*create\s+nothing,\s*never guess/,
    'the brief must document outcome (3) ambiguous drift taking the SAFE branch refusal',
  )
  // Same repair-direction pin as the SKILL phase carries: the rename repair rewrites the CHILD's
  // marker. Re-pointing the spec key at `liveKey` instead would strand every sibling `blockedByKeys`
  // ref and the `adopted` entry (both keyed by `specKey`) and throw inside `epicWiringPlan`.
  assert.match(
    BRIEF,
    /rewrite\s+\*\*its own\*\*\s+description marker to `epicChildMarker\(specKey\)`[\s\S]*?never the spec key/,
    'the brief must repair the child marker on an unambiguous rename, never re-point the spec key',
  )
  // Same preserve-the-bytes clause as the SKILL phase carries: the repair is a description write and
  // the save replaces the description, so a marker-only save wipes the child's plan body.
  assert.match(
    BRIEF,
    /preserving the rest of that description's bytes verbatim\*\*[\s\S]*?save\s*\n?\s*replaces the description/,
    'the brief must preserve the rest of the child description on the rename repair',
  )
})

test('BOS-475: epic parents carry configured label, summed estimate, and backlog-calibrated priority', () => {
  assert.match(
    SKILL,
    /labelName\(config, 'epic'\)/,
    'the epic label must resolve through skill-config',
  )
  assert.match(SKILL, /epicParentEstimate\(spec\)/, 'the parent estimate must sum child complexity')
  assert.match(
    SKILL,
    /estimate.*rejected[\s\S]{0,180}retry without.*estimate/i,
    'a rejected summed estimate must retain the existing fallback',
  )
  assert.match(
    SKILL,
    /priority\s*=\s*parent\.priority/,
    'the parent flip must persist the drafted priority',
  )
  assert.match(
    SKILL,
    /reporter.{0,80}priority[\s\S]{0,180}planned.{0,80}backlog/i,
    'priority guidance must honor reporter input or calibrate against Todo',
  )
  assert.match(
    SKILL,
    /every planned ticket.{0,80}non-null estimate/i,
    'all planned tickets must carry an estimate',
  )
  assert.match(
    BRIEF,
    /parent:\{title,goal,keyChanges\[\],priority\}/,
    'the epic draft shape must include parent priority',
  )
  assert.match(
    BRIEF,
    /sum of its children.?s estimates/i,
    'the brief must specify the summed parent estimate',
  )
  assert.match(
    BRIEF,
    /reporter.{0,80}priority[\s\S]{0,180}planned.{0,80}backlog/i,
    'the brief must carry backlog-relative priority guidance',
  )
})

// ---------------------------------------------------------------------------
// Mirror parity — the plugin SKILL.md mirror is byte-identical to the canonical
// (make copy-skills committed in sync). Follows the MIRROR_BRIEF pattern above.
// ---------------------------------------------------------------------------

const MIRROR_SKILL = readIfExists(
  '../plugins/bossd-plugin-claude/skilldata/skills/boss-plan/SKILL.md',
)

test('the plugin mirror SKILL.md is byte-identical to the canonical SKILL.md', () => {
  assert.notEqual(
    MIRROR_SKILL,
    '',
    'the plugin-mirror SKILL.md must exist (make copy-skills committed)',
  )
  assert.equal(
    MIRROR_SKILL,
    SKILL,
    'the plugin mirror must be byte-identical to the canonical SKILL.md (run make copy-skills and stage it)',
  )
})

// ---------------------------------------------------------------------------
// Scratch cleanup — one glob pattern per command line.
// ---------------------------------------------------------------------------

test('every glob-bearing scratch-cleanup line carries exactly one pattern', () => {
  // Under zsh and fish an UNMATCHED glob aborts the WHOLE command line. Three cleanup sites
  // used to share one `rm -f` line across the child-plan, image-guard and attachment-header
  // patterns, so a single-ticket run — which writes no child plan — aborted on the first
  // pattern and left the other scratch behind. The fix is one `find … -delete` per pattern.
  // Without this guard the property is prose only, and the next prose-shrinking edit
  // recollapses it silently: the failure is invisible on the happy path.
  const lines = SKILL.split('\n')
  // Deletion lines only. The residual `-print` sweep added below deliberately carries all three
  // patterns on one line (it is a post-condition check, not a deletion), so key on `-delete`.
  const globCleanupLines = lines.filter(
    (line) =>
      line.includes('.linear-plans') &&
      line.includes('*') &&
      /(^|\s)(rm -f|find) /.test(line) &&
      line.includes('-delete'),
  )
  assert.ok(
    globCleanupLines.length >= 9,
    `expected at least 9 glob cleanup lines (3 sites x child-plan/image-guard/attachment-headers), got ${globCleanupLines.length}`,
  )
  for (const line of globCleanupLines) {
    assert.match(
      line.trim(),
      /^if \[ -d \.linear-plans \]; then find \.linear-plans -maxdepth 1 -type f -name '[^']+' -delete \|\| CLEANUP_RC=1; fi$/,
      `a glob cleanup line must use the one-pattern-per-line find form: ${line.trim()}`,
    )
  }
  // A trailing `|| true` would swallow a REAL deletion error (permission denied, I/O) and let
  // cleanup report success with plan text or signed headers still on disk. The `if` wrapper
  // tolerates only the missing-directory case, so pin that: no cleanup line may force success.
  for (const line of globCleanupLines) {
    assert.ok(
      !line.includes('|| true'),
      `a cleanup line must not force success with \`|| true\` — real deletion errors must propagate: ${line.trim()}`,
    )
  }
  // Two masking bugs, two guards, one per site. (1) A block's exit status is its LAST command's,
  // so a failed delete followed by a no-match delete vanishes — hence CLEANUP_RC accumulation.
  // (2) BSD find (/usr/bin/find on macOS, where cron worktrees run) exits 0 even when `-delete`
  // hits EACCES, so the accumulator alone still misses it — hence the residual `-print` sweep,
  // which checks the post-condition instead of trusting any find's exit status. Both were
  // verified against /usr/bin/find; drop either and a real failure reports success again.
  const SITES = 3
  assert.equal(
    lines.filter((l) => l.trim() === 'CLEANUP_RC=0').length,
    SITES,
    'each cleanup site must reset CLEANUP_RC before accumulating',
  )
  assert.equal(
    lines.filter((l) => l.includes('-print)') && l.includes('CLEANUP_RC=1')).length,
    SITES,
    'each cleanup site must re-scan for surviving scratch — BSD find exits 0 on a failed -delete',
  )
  assert.equal(
    lines.filter((l) => l.includes('[ "$CLEANUP_RC" = 0 ]')).length,
    SITES,
    'each cleanup site must act on the accumulated status',
  )
  for (const line of lines) {
    assert.ok(
      !(/(^|\s)rm -f /.test(line) && line.includes('*')),
      `rm -f must never carry a glob — an unmatched one aborts the line under zsh and fish: ${line.trim()}`,
    )
  }
})

// ---------------------------------------------------------------------------
// Size-ratchet — the resident body stays below the pre-split baseline.
// ---------------------------------------------------------------------------

test('the resident SKILL.md body stays under the ratchet, below the pre-split baseline', () => {
  // PRE_SPLIT_BASELINE is a rolling upper bound kept a small margin above RATCHET, NOT the
  // literal pre-split body size: it began at 25548 (the hand-written body before the BOS-204
  // references split) and is re-baselined upward as Phase-4 prose legitimately grows. The
  // RATCHET < PRE_SPLIT_BASELINE invariant preserves that explicit margin so an accidental
  // bulk regrow in one edit trips the guard instead of sliding both constants up together.
  const PRE_SPLIT_BASELINE = 78196
  const RATCHET = 78180 // pinned exact byte ceiling: re-baselined +1041 for the FINAL #1761 commit (4c98f0597, "fail scratch cleanup on any deletion error, not just the last"), which grew the body past the ceiling its own earlier round had just set. The `scripts` job is path-filtered, so it did not run on that merge to main and the overshoot stayed latent until a PR touching skill payloads triggered it; was 62730. Re-baselined +771 for the Phase 2.5 reconcileEpicChildren idempotent-resume mandate (the reconcile-join call, its epicChildMarker(key) marker citation, and the three-outcome aligned/unambiguous-rename/ambiguous-drift wording); was 63771. Re-baselined +52 for BOS-649's doc-drift fix: SKILL.md's child-creation step now cites `epicChildMarker(key)` as the resume marker's canonical emitter (never a hand-written literal comment), matching the headless-drafting-brief's wording; was 64542. Re-baselined +219 for BOS-649's review fix: the unambiguous-rename repair now rewrites the CHILD's own `epicChildMarker(specKey)` marker instead of re-pointing the spec key at `liveKey` — the spec key is the namespace `adopted`, the siblings' `blockedByKeys` and `epicWiringPlan` all resolve through, so the old wording would have stranded those refs and thrown mid-wire after children already existed; was 64594. Re-baselined +196 for BOS-649's round-2 review fix: moving the repair target from the parent spec to the child dropped the "the save replaces the description" constraint this phase already carries at its other two marker-write points, so a literal marker-only save_issue would have wiped the child's gated plan body while it still read as adopted; was 64813. Re-baselined +3659 for BOS-651's move of the epic decomposition spec out of the base64 description marker and into a plain-JSON `epic-spec.json` attachment: Phase 2.5 gained the spec-attachment contract table (filename/MIME/title/body/read/duplicate-policy/identity plus the reuse-plan-storage-steps-1-4 mechanism and the two "why" clauses), the three-stage label-strip → spec-upload → deferred-destructive-strip write ordering, dual-store epic-parent detection with its presence-decides rule and present-but-unreadable recovery gate, and the `Implementation plan`-scoped stale-attachment sweep predicate — net of deleting the now-false marker append/re-append prose, the bounded-marker `planMarkdown` clauses and the base64 rationale; was 65009. Re-baselined +1123 for BOS-651's task-2 review fixes: step 4 stage 2 now names the `.linear-plans/<ISSUE-ID>.epic-spec.json` scratch file the file-taking PUT needs, and all three Phase 5 cleanup sites (terminal, dispatch-failure abort, epic reverify-fail abort) gained the matching `find … -delete` line plus its residual `-print` term, so the new artifact cannot violate the leave-no-local-artifacts invariant; stage 3 gained the explicit `deletePlanAttachment` / starts-with-`Implementation plan` predicate; the `deletePlanAttachment` precondition moved from "the first epic write" to stage 3; and the unreadable-spec recovery gate now admits the per-child attachment read it may need — net of replacing step 7's restatement of the stage-1 selectability argument with a back-reference; was 68668. Re-baselined +1266 for BOS-651's whole-branch review fixes: the epic-parent detection block now scopes `validateSpecIdentity` to an ATTACHMENT-sourced spec and states that a legacy inline spec — which predates `schemaVersion`/`parentId` and would therefore fail that check unconditionally — is bound by provenance and recovered as-is, resolving a contradiction that had routed 100% of legacy parents into the abort-or-no-op gate while the resume section claimed they were still recovered; and step 4 stage 2 gained the upload-exactly-once precondition (read `attachments[]` first; one existing `Epic spec (…)` ⇒ skip the upload and resume, two or more ⇒ abort), because a finalize mints a new attachment row every call where the description marker it replaces was overwritten in place, so a crash after stage 2 would otherwise accumulate duplicates into the permanently-aborting state the contract's duplicate policy defines — that policy also gained its manual remediation and the contract's read row now names `readPlanAttachment`; was 69791. Re-baselined +787 for BOS-651's round-2 review fixes, which made the identity mechanism actually functional: `serializeEpicSpec` reads `spec.parentId` and omits an absent id rather than inventing one, but the drafted-spec shape cited here (and in the headless brief) listed only `parent`/`children`, so every attachment this skill writes would have shipped without a `parentId` and failed `validateSpecIdentity` unconditionally — the shape now carries `parentId` and step 4 stage 2 sets it before serializing; and the MAINLINE idempotent-resume guard, which the unplanned sweep actually takes, recovered the spec by attachment TITLE and never validated identity at all, so it now runs `validateSpecIdentity` too and routes a failure to the unreadable-spec recovery gate. The stage-2 skip also states that stage 3 is skipped with it (the step-7 flip re-runs that strip), which was a dead end for a headless reader; was 71057. Re-baselined +638 for BOS-651's round-3 review fix: round 2 made `parentId` load-bearing but left it enforced by prose alone — `serializeEpicSpec` drops an unset id silently and `validateDecomposition` never inspects it, so an unbound spec uploaded clean and only turned fatal on a later resume, which is the silent-at-write/fatal-later shape the plan's failure-mode table reserves for a guard. Stage 2 now verifies its own bytes before the PUT with `validateSpecIdentity(parseEpicSpec(<file>), <ISSUE-ID>)` and aborts while zero children exist. `validateDecomposition` was deliberately NOT given the check: it validates the decomposition (children, DAG, estimates), not the attachment binding, and 30+ inline fixtures pass specs that legitimately carry no `parentId`. Also: the mainline resume guard now restates that identity is attachment-sourced-only, so the legacy sentence that follows it cannot be misread, and the Phase 2.5 toolbox roll-call names the three new exports plus `SPEC_ATTACHMENT_MIME`; was 71844. Re-baselined +1404 for the BOS-651 outside-voice (cross-model) review, which found the branch's legacy-support claim hollow in the two places that decide it. Step 6's parent-overview save is description-only and this branch had replaced the old re-append-the-marker mandate with an UNCONDITIONAL "this save cannot lose the spec" — true for an attachment-sourced spec, false for a legacy parent whose spec IS the description marker, so a legacy resume's own overview save destroyed the only store and left the parent with NEITHER, which the next sweep re-decomposes into duplicate children; that claim is now scoped, with the legacy arm required to carry the marker substring verbatim. And step 4 stage 2's re-pick check counted `Epic spec (…)` ATTACHMENTS only while the dual-store rule lived solely in the precondition paragraph scoped to a non-unplanned named source, so the unplanned sweep — the primary resume route — never saw a legacy parent and re-decomposed it into keys that no live child's marker matches, bricking `reconcileEpicChildren` on every later run; stage 2 now reads both stores. Also: the duplicate-attachment abort is restated at the two sites that actually perform detection (it had lived only in the contract table and stage 2), and the child copy-back now names `openQuestions` alongside `agentFriendly` — SKILL.md's "copy only the `agentFriendly` verdict" contradicted the brief and silently dropped every child's `agent-question` signal on resume; was 72482. Re-baselined +222 for the prose hygiene the bounded re-review flagged after four rounds of edits landed on the same paragraphs: stage 2's duplicate-attachment abort now precedes the either-store-present skip (the pre-fix predicate said "exactly one", which excluded duplicates by construction, so widening it to "either" had quietly made the skip match a two-attachment parent before the abort sentence was reached); the step-6 legacy carry no longer cites "Decision 3" of an unshipped repo-internal plan doc, which a globally-installed skill cannot resolve, and states the reason inline instead; and the stage-2 crash-safety rationale is scoped, since it asserted the parent holds a spec attachment which is false on the skip path; was 73886. Re-baselined +1958 for the boss-review multi-lens pass. Its cross-model round found that widening the re-pick check to the description store had made a bare QUOTED `<!-- boss-plan-epic-spec:` substring count as presence, so a brand-new ticket whose reporter notes merely mention the marker (this repo's own plan docs do) would be classified an unreadable epic parent and abort loudly on every sweep, permanently unplannable; presence is now store-specific — an attachment counts when present, a description only when `parseEpicSpec` actually returns a spec. Its requesting round found the `deletePlanAttachment` capability requirement had been moved to stage 3, which is skipped on every resume, deferring the check to step 7 — past child creation and `agent-friendly` exposure — so on an adapter lacking that optional op the run stranded buildable children under an unplanned parent; it is required before the first epic write again. Codex also refuted the "which no copy can forge" rationale for waiving identity on a legacy spec (duplicating an issue copies its description), so the waiver now rests on the legacy store being read-only, frozen and slated for removal rather than on a false forgery claim. Plus: the brief's stage-2 crash-safety sentence got the scoping SKILL.md received in the prior round, and the unreadable-spec gate documents its operator remediation, since an unplanned parent fails its first conjunct by definition; was 74108. Re-baselined +531 for BOS-652's four inline-code CALL citations to Phase 2.5's own deterministic core, `skills-toolbox/plan-epic-phase25.mjs`: `detectEpicParent(issue)` in the epic-parent precondition, `epicSpecRecoveryGate(...)` at the unreadable-spec gate, `epicPhase25WritePlan(...)` as step 4's ordered write-sequence emitter, and `stalePlanAttachmentSweep(...)` at step 7's stale sweep, plus the module's toolbox-path roll-call entry. The call form is what the bytes bought: `check-skill-symbols.mjs` only extracts a span that starts with a camelCase identifier followed by `(`, so a bare backticked name would leave the new core ungated. Each citation REPLACED the prose it now owns (the store-specific presence rule and duplicate ordering, the ALL-of conjunct enumeration, the three-stage ordering mechanics and stage-1/3 op restatements, the starts-with-`Implementation plan` predicate) rather than sitting beside it, so the net growth is the citations plus the kept rationale, not a second copy of the rules; was 76066. Re-baselined +780 for BOS-652's whole-branch review fixes, which retracted a claim this branch had just added: step 4 said the emitter owns "the per-child label subtraction", but `serializeEpicSpec` persists no `labels` array, so the subtraction ran against a field round-tripped spec data never carries — a no-op dressed as a rule, and a headless reader obeying "never re-derive either inline" would have created every child with an EMPTY label set, dropping the content labels and the `agent-question` signal step 4 mandates at creation (the same signal BOS-651's cross-model round already restored once). `labelsToStrip` is now stated PARENT-scoped (stage 1's `removeLabels` and nothing else) and the emitter is stated to emit no child `labels` field at all, with the reason inline so the boundary cannot be re-crossed. Also: "execute its ops in emitted order" gained "minus any stage the preconditions below skip", because the emitter emits the three spec-upload ops unconditionally while stage 2 skips them (and stage 3 with them) whenever either store already holds a spec — and a finalize mints a NEW attachment row every call, so a reader taking the emitted plan as complete would upload a second `Epic spec (…)` on the unplanned sweep, the primary resume route, landing the parent in the permanently-aborting duplicate state. Round 2 then applied the SAME reasoning to stage 4, which the round-1 clause had not covered: the emitter emits one `createChild` per SPEC child, never per MISSING child, while the resume path is required to "create only the spec keys `missing` names" — so a reader executing the emitted create-children ops unfiltered after a stage-2 skip duplicates every child that already exists, the one failure this phase's other guards are most obsessed with; the citation now also reads "minus every child `reconcileEpicChildren` does NOT report `missing`". Plus stage 3 now names the ONE-ARG sweep form (no parent-overview attachment exists yet, so there is nothing to keep), where copying step 7's two-arg call was benign but under-specified; Re-baselined a further +344 for the cross-model (Codex) round, which found the emitted `args` could not execute the ops they named: all three spec-upload entries carried one identical `{issueId, filename, mimeType, title}` blob and the delete was keyed `{issueId, attachmentId}`, while the adapter declares `{issue, filename, contentType, size}`, `{issue, assetUrl, title}` and `{id}` — and nothing caught it, because the fake tracker reads two arg fields, so a plan of plausible-looking WRONG keys replayed perfectly green. Entries are now `{stage, op, args, runtimeArgs}`: `args` is the statically-known subset under the adapter's own key names, and `runtimeArgs` names per op what only the executor can supply because it does not exist until the previous op ran (the prepare's `size`, the PUT's `file`/`uploadURL`/`headers`, the finalize's `assetUrl`), with a test cross-checking BOTH halves against the adapter's own operation summaries rather than a restated literal; was 77377. Re-baselined a further +459 for the bounded outside-voice re-review, which found that fix's ONE pinned exception was exactly the size of a real bug: `BEYOND_SUMMARY` waved through stage 1's `removeLabels`, and `save_issue` has no such argument — its `labels` REPLACES the set, which is why this same SKILL.md already tells the executor to read `readLabels` and merge. `removeLabels` occurred nowhere but this branch's own new code. So the one emitted key that was not a real argument was exempted from the one check that would have caught it, and an executor obeying the new "spread `args` into the call" contract would have errored or, on a tracker that drops unknown keys, silently sent `{id}` alone — the strip no-ops and the parent keeps `agent-friendly` plus its plan artifact through the whole create→wire→expose window, the exact exposure stage 1 exists to close, and this branch had already deleted the sentence that explained the mechanism. The labels to strip now ride OUTSIDE `args` as `stripLabels` (an instruction, not a call argument), both files restate the read-merge-replace mechanism, and `BEYOND_SUMMARY` is now EMPTY — so the arg cross-check has full teeth and re-adding the key turns the suite red; was 77721.
  assert.ok(
    RATCHET < PRE_SPLIT_BASELINE,
    'the ratchet ceiling must sit below the pre-split baseline',
  )
  const bytes = Buffer.byteLength(SKILL, 'utf8')
  assert.ok(
    bytes <= RATCHET,
    `resident SKILL.md is ${bytes} bytes; must stay <= ${RATCHET} (below the ${PRE_SPLIT_BASELINE} baseline)`,
  )
})

test('BOS-458: the published core carries no hard-coded ${TRACKER:-…} shell default', () => {
  // Direct regression guard for the BOS-458 fix, which lives in this SKILL.md body (not the
  // skill-config library the skill-config.test.mjs jira test covers). The bug was a shell
  // `${TRACKER:-linear}` literal that baked `linear` in when the TRACKER env was unset, so a
  // non-linear repo silently routed through the linear op map. The correct config-driven form
  // resolves the tracker in JS (`process.env.TRACKER || c.adapters?.tracker || "linear"`) or
  // names `adapters.tracker` in prose — never the shell `${TRACKER:-<default>}` form. Assert
  // that form is absent so a future edit cannot reintroduce the hardcode into a published core
  // (the Go identity-leak guard permits `linear` as the default adapter and would not catch it).
  assert.equal(
    count(SKILL, '${TRACKER:-'),
    0,
    'boss-plan SKILL.md must not hard-code a ${TRACKER:-<default>} shell fallback; resolve the tracker from adapters.tracker instead',
  )
})
