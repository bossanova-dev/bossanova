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
import { discoverExtensions } from './skill-extensions.mjs'

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
  '## Phase 4 — Publish the plan and write back to the tracker',
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
  for (const sym of [
    'scripts/plan-epic-lib.mjs',
    'validateDecomposition',
    'assertAcyclic',
    'topoOrderChildren',
    'epicWiringPlan',
    'EPIC_MIN_CHILDREN',
    'EPIC_MAX_CHILDREN',
  ]) {
    assert.ok(EPIC_PHASE.includes(sym), `the phase must name the plan-epic-lib symbol ${sym}`)
  }
})

test('the SKILL documents every load-bearing epic guard', () => {
  // >=2 & <=8 children
  assert.match(EPIC_PHASE, /EPIC_MAX_CHILDREN = 8/, 'must state the 8-child cap')
  assert.match(EPIC_PHASE, /EPIC_MIN_CHILDREN = 2/, 'must state the 2-child minimum')
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
// Size-ratchet — the resident body stays below the pre-split baseline.
// ---------------------------------------------------------------------------

test('the resident SKILL.md body stays under the ratchet, below the pre-split baseline', () => {
  // PRE_SPLIT_BASELINE is a rolling upper bound kept a small margin above RATCHET, NOT the
  // literal pre-split body size: it began at 25548 (the hand-written body before the BOS-204
  // references split) and is re-baselined upward as Phase-4 prose legitimately grows. The
  // RATCHET < PRE_SPLIT_BASELINE invariant preserves that explicit margin so an accidental
  // bulk regrow in one edit trips the guard instead of sliding both constants up together.
  const PRE_SPLIT_BASELINE = 58435
  const RATCHET = 58379 // pinned exact byte ceiling: re-baselined +55 for BOS-458 generalizing the Phase-4 write-back `TRACKER` default to the configured tracker adapter (`adapters.tracker`, default `linear`) so a non-linear repo that omits the `TRACKER` env no longer routes write-back through the linear op map — removing the last `${TRACKER:-linear}` literal in any published core per the ticket audit; +889 for the codex-review P2 round making the Phase 0 r2-publish preflight abort resolve and NAME the concrete `adapters.publish` key (and state the publish config is keyed on `adapters.publish`, default `proof` — the destination block — NOT `PLAN_PUBLISH`, default `r2` — the upload transport) instead of the opaque `publishConfig.<adapter>` placeholder, so a consuming repo that stored its bucket/baseUrl under a `publishConfig` key other than the one `adapters.publish` selects gets a diagnosable failure rather than a bare "config missing"; +604 for the codex-review P2 round failing the Phase 0 preflight EARLY (before issue selection + the drafting subagent) when the r2 publish path lacks publishConfig.<adapter>.{bucket,baseUrl}, instead of skipping the export and throwing late inside Phase 4 publish; +275 for the codex-review P2 round gating the Phase 0 preflight on `isConfiguredForPlanning` (requires the full `states.{unplanned,planned,inProgress,inReview}` role map, not just tracker identity) so a repo configured only for a stateless core self-disables cleanly instead of running boss-plan with undefined state names; +632 for the codex-review P2 round making the Phase 0 preflight distinguish a `.boss-skills.json` loader failure (loadSkillConfig throws `skill-config:` → 'error'/empty → abort exit 1) from a valid unconfigured repo ('no' → clean self-disable exit 0), so a malformed config no longer masquerades as "no tracker" and silently skips planning; +874 for the codex-review P1 round replacing the three `eval "$(node -e …)"` publish-config emitters (Phase 0 §3, Phase 4, headless brief) with injection-safe command-substitution assignments so a `.boss-skills.json` bucket/baseUrl containing shell metacharacters is captured literally, never executed; was 55050 for BOS-455 de-identification + Phase 0 preflight self-disable (config-driven tracker/publish refs, generic state-role words, dropped BOS-NN citations); was 52064 for BOS-442's Phase 2.5 epic-decomposition phase (parent + N children + blockedBy DAG guards; +154 for the resume-marker embed + prefer-marker-over-title clause, +516 for the earlier codex-review fixes — parent-repurpose-last write-atomicity guard + `list_issues parentId` child enumeration for idempotent resume, +2192 for the second round of codex-review P1 fixes: a distinct epic sentinel outcome branch in Phase 2 §3 that skips Phase 3.5/Phase 4, plus stableChildKey-based durable resume that persists a `boss-plan-epic-spec` child-key marker in the parent description before any child write, +3013 for the third round of codex-review fixes: an epic-specific reverify in Phase 2 §3 that re-reads Linear (parent now `Todo` + child count matches) before accepting the epic sentinel, the two Phase 4 secret + image-parity gates run against the repurposed PARENT overview, and excluding the epic's own child+parent ids from the external conflict-link backlog set, +433 for the FULL-spec resume fixes, +1553 for the earlier deferred-exposure + preserve-original-notes fixes, +1086 for the latest codex-review round: step 6 now stamps each child with ITS OWN plan's agent-friendliness call (a `needs-human`/`agentFriendly:false` child gets `needs-human`, never `agent-friendly`, matching boss-epic's Todo+agent-friendly+plan-link+not-needs-human eligibility) instead of unconditionally stamping `agent-friendly`, and step 7 re-appends the `boss-plan-epic-spec` marker to the repurposed parent so idempotent resume can recover the FULL spec from a fully-built parent instead of re-decomposing into duplicate children), +523 for persisting each child's `agentFriendly` call in the `boss-plan-epic-spec` marker (spec-shape + marker field enumerations and a step-6/resume clause so an already-created-but-unexposed child adopted on a fresh-worktree resume re-derives its deferred-exposure label — `needs-human` vs `agent-friendly` — from the recovered spec rather than the vanished `.linear-plans/` plan), +739 for the codex-review review-thread fixes (an R2 publish bootstrap reminder in Phase 2.5 step 4 — `scripts/plan-upload.mjs` throws when `BOSS_PROOF_R2_BUCKET` is unset, so each fresh-shell child/parent publish must re-run the Phase 4 `.env`/bucket setup — and gating the parent-overview secret + image-parity gates BEFORE step 6 child exposure so a deterministic parent-gate failure never leaves a child `agent-friendly`/buildable while the parent aborts `Unplanned`), +1632 for the codex-review-thread round: Phase 5 now globs the epic child-plan + image-guard scratch (`.linear-plans/<ISSUE-ID>-child-*.md`) so a successful epic or a mid-child abort leaves no scratch, Phase 2.5 step 6 now PUBLISHES + SAVES the parent overview onto the still-`Unplanned` parent BEFORE any child is exposed (the failure-prone R2 publish + Linear save move ahead of exposure so an exposed child is always backed by an already-published parent; step 7 is the final state-only `Unplanned → Todo` flip), and idempotent resume now detects an already-saved parent overview and reuses it verbatim rather than recomposing `## Original notes` from the transformed description, on top of BOS-111's Phase 3 `## Proof harness analysis` readiness-pass baseline, +205 for the codex-review round adding each child spec's validated `estimate` + `priority` to the Phase 2.5 step-4 child `save_issue` contract (so `boss-epic` schedules children by their intended priority, not default/None), +782 for the codex-review round making step 7's parent repurpose STRIP any pre-existing `agent-friendly`/`needs-human` label + stale single-ticket `Implementation plan (…)` link when a previously-planned (explicitly-named `Todo`/`In Progress`) ticket is turned into an epic, so the repurposed epic parent is not itself `boss-build`-selectable despite the parent-label exception, +767 for the codex-review round wiring the Phase 5 epic child-plan + image-guard cleanup globs into BOTH Phase 2 dispatch-failure exits (missing/stale sentinel AND epic reverify-fail), so a headless EPIC subagent that writes `.linear-plans/<ISSUE-ID>-child-*.md` (carrying `## Original notes`) then dies without an ok sentinel no longer leaves that scratch in the cron worktree, +1059 for the codex-review round: step 2 now RE-runs `validateDecomposition` after copying each child plan's `agentFriendly` verdict (so the non-boolean guard bites on values that did not exist at the step-1 validate), and step 4's first `save_issue` now STRIPS any pre-existing `agent-friendly`/`needs-human` label + stale single-ticket `Implementation plan (…)` link from the parent (an explicitly-named already-planned `Todo` source would otherwise stay `boss-build`-selectable through the whole build window and after a crash, not just until the step-7 flip), +1290 for the codex-review round adding the **Unplanned-source precondition** (an explicitly-named `Todo`/`In Progress` source that triages EPIC falls back to a single-ticket plan — a non-`Unplanned` parent is invisible to the `Unplanned` resume sweep and stranded on a crash; the step-4 strip is reframed as defense-in-depth) and pinning the child plan-link title to `Implementation plan (<child id>)` (boss-epic's `normalizeTicket` only recognizes a link whose title starts with `Implementation plan`, else the child is exposed agent-friendly but skipped as missing a plan), +656 for the codex-review P1 fix initializing `EPIC_REVERIFIED=false` and spelling out that the two Linear MCP reverify reads (parent now `Todo` + child count matches) must SET it true before the guard tests it — an unset flag would abort even a fully-successful epic and, with the parent already flipped off `Unplanned`, strand it from the resume sweep, +404 for the codex-review round carrying each child plan's `openQuestions` decision through creation + the epic spec marker (persisted as a derived `agentQuestion` boolean) so an epic child with controversial open questions gets the `agent-question` label at creation and re-gets it on a fresh-worktree resume, matching the single-ticket Phase 4 contract, +571 for the codex-review round making the Unplanned-source precondition `parseEpicSpecMarker` FIRST: a non-`Unplanned` source that already carries a `boss-plan-epic-spec` marker (a completed epic parent, flipped to `Todo` but keeping the marker) routes to the idempotent resume/no-op path, never the single-ticket fallback that would re-plan a finished epic as a normal buildable ticket, +1045 for the codex-review round deferring the Phase 4 step-5 external conflict links until AFTER the step-6 parent-overview commit — previously the external pass ran in the step-5 wiring, so a deterministic parent-gate abort (e.g. a persistently failing image-parity gate) could leave non-epic backlog tickets blocked behind an epic child that never becomes buildable; now external edges are only written once the parent overview has committed, so a parent-gate abort writes zero external edges and strands no existing backlog work, +452 for the codex-review follow-up making the idempotent-resume "already-saved parent overview" path re-run the deferred external conflict links BEFORE stamping children buildable (a crash after the parent save but before the deferred external pass would otherwise resume straight to child exposure and skip the links, exposing an agent-friendly root child without blocking overlapping active backlog work; the links are append-only so re-running is a safe no-op)
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
