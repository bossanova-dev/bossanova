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
//   * a size-ratchet keeping the resident body below the pre-split baseline,
//   * codex-mirror parity + reference mirror-safety.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'
import { rewriteClaudeSkillMarkdown } from './sync-codex-skills.mjs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const SKILL = read('../.claude/skills/boss-plan/SKILL.md')
const CODEX = read('../.codex/skills/boss-plan/SKILL.md')
const INTERACTIVE = read('../.claude/skills/boss-plan/references/interactive-mode.md')
const BRIEF = read('../.claude/skills/boss-plan/references/headless-drafting-brief.md')
const CODEX_INTERACTIVE = read('../.codex/skills/boss-plan/references/interactive-mode.md')
const CODEX_BRIEF = read('../.codex/skills/boss-plan/references/headless-drafting-brief.md')

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
  '\n## Phase 3',
)
const PHASE_4_SECTION = sectionBetween(
  SKILL,
  '## Phase 4 — Publish the plan and write back to the tracker',
  '\n## Phase 5',
)

function rewriteCopiedMarkdownForTest(body) {
  return body
    .replace(/~\/\.claude\/skills\//g, '~/.codex/skills/')
    .replace(/\bnode \.claude\/skills\//g, 'node .codex/skills/')
    .replace(/~\/\.claude\/skills\/bossanova\//g, '~/.codex/skills/bossanova/')
    .replace(/\bCLAUDE\.md\b/g, 'AGENTS.md')
    .replace(/\bClaude Code\b/g, 'Codex')
    .replace(/\bClaude agents\b/g, 'Codex agents')
    .replace(/\bClaude agent\b/g, 'Codex agent')
    .replace(/\bClaude\b/g, 'Codex')
    .replace(/\bTodoWrite\b/g, 'update_plan')
    .replace(/\bRead tool\b/g, 'file-reading tool')
    .replace(/`Read`/g, '`file-reading tool`')
    .replace(/\bEdit tool\b/g, 'apply_patch')
    .replace(/`Edit`/g, '`apply_patch`')
    .replace(/\bWrite tool\b/g, 'apply_patch')
    .replace(/`Write`/g, '`apply_patch`')
    .replace(/\bBash tool\b/g, 'shell command tool')
    .replace(/`Bash`/g, '`shell`')
    .replace(/`Grep`/g, '`search`')
    .replace(/`Glob`/g, '`file search`')
    .replace(/\bWebFetch\b/g, 'web fetch')
    .replace(/\bPlaywright MCP server\b/g, 'Codex browser automation')
    .replace(/\bPlaywright MCP\b/g, 'Codex browser automation')
    .replace(/\bAGENTS\.md`, `AGENTS\.md\b/g, 'AGENTS.md`, `CLAUDE.md')
    .replace(
      /`apply_patch`\/`apply_patch` "modified since read"/g,
      '`write`/`apply_patch` "modified since read"',
    )
}

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
    /recon.*writing-plans|writing-plans.*recon/is,
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
// Byte-identical Linear description section contract (boss-implement/bs-sweep-plan consume it).
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

test('the interactive plan-eng-review invocation survives verbatim in the reference', () => {
  assert.ok(
    INTERACTIVE.includes('Invoke `plan-eng-review` via the **Skill** tool'),
    'interactive-mode.md must preserve the plan-eng-review invocation sentence verbatim',
  )
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
    'Invoke `superpowers:writing-plans` via the Skill tool to produce the implementation plan',
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
// Size-ratchet — the resident body stays below the pre-split baseline.
// ---------------------------------------------------------------------------

test('the resident SKILL.md body stays under the ratchet, below the pre-split baseline', () => {
  const PRE_SPLIT_BASELINE = 25548 // bytes, the hand-written body before this split
  const RATCHET = 25531 // pinned ceiling: post-split size + BOS-205's Phase 4 publish/tracker adapter seam (re-baselined after aggressive reclaim; still 17B below the pre-split baseline)
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

// ---------------------------------------------------------------------------
// Codex mirror — same dispatch/sentinel tokens; references stay mirror-safe.
// ---------------------------------------------------------------------------

test('the .codex mirror is regenerated from the full .claude skill body', () => {
  assert.equal(
    CODEX,
    rewriteClaudeSkillMarkdown(SKILL, '.claude/skills/boss-plan/SKILL.md'),
    'codex SKILL.md mirror must equal the generated rewrite of the full claude skill',
  )
})

test('the reference prose is mirror-safe (references get only COMMON_REWRITES)', () => {
  // References are copied with rewriteCopiedMarkdown (no `/command` -> `$command` rewrite),
  // so a backticked `/command` token (backtick, slash, letter) would survive un-rewritten in
  // .codex and confuse a Codex run. Code-span/slash boundaries like `Todo`/`In Progress` are
  // fine — the hazard is specifically a slash immediately opening a lowercase command name.
  const commandToken = /`\/[a-z]/g
  for (const [name, body] of [
    ['interactive-mode.md', INTERACTIVE],
    ['headless-drafting-brief.md', BRIEF],
  ]) {
    assert.equal(
      (body.match(commandToken) || []).length,
      0,
      `${name} must not contain backticked /command tokens (mirror-unsafe)`,
    )
  }
  // And the interactive directives the negative assertion pins survive into the mirror too.
  assert.equal(
    CODEX_INTERACTIVE,
    rewriteCopiedMarkdownForTest(INTERACTIVE),
    'codex interactive-mode mirror must equal the generated copied-markdown rewrite',
  )
  assert.equal(
    CODEX_BRIEF,
    rewriteCopiedMarkdownForTest(BRIEF),
    'codex headless-drafting-brief mirror must equal the generated copied-markdown rewrite',
  )
})
