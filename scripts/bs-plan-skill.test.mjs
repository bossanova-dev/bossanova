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
// codex-mirror parity for the core. The repo-local draft extension
// (boss-plan-compound-engineering) stays under .claude/skills.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
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
const DRAFT_NAME = 'boss-plan-compound-engineering'
const DRAFT = readIfExists(`../.claude/skills/${DRAFT_NAME}/SKILL.md`)
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

test('an ok sentinel accepts only a path that resolves to the expected non-empty plan file before upload', () => {
  assert.match(
    HEADLESS_SECTION,
    /PLAN_FILE_RAW="\$\(printf '%s' "\$READ" \| jq -r '\.payload\.planPath \/\/ empty'\)"/,
    'the raw sentinel path must be retained for path-equivalence validation',
  )
  assert.match(
    HEADLESS_SECTION,
    /resolve\(reportedPath\)!==resolve\(expectedPath\)/,
    'an ok sentinel path must resolve to exactly the expected plan path',
  )
  assert.match(
    HEADLESS_SECTION,
    /process\.stdout\.write\(expectedPath\)/,
    'a validated equivalent path must normalize to the canonical relative PLAN_PATH',
  )
  assert.match(
    HEADLESS_SECTION,
    /non-empty|!\s+-s "\$PLAN_FILE"/i,
    'an ok sentinel must be re-verified (plan file exists + non-empty) before trusting it',
  )
  assert.match(
    HEADLESS_SECTION,
    /metadata `planPath`\s+resolves to `PLAN_PATH`/,
    'the returned metadata planPath must resolve to the validated sentinel path',
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

test('Phase 4 permits required secret redaction without weakening attachment parity (BOS-702)', () => {
  assert.match(
    PHASE_4_SECTION,
    /redact it in[\s\S]{0,20}every persisted artifact/i,
    'the secret gate must cover both persisted artifacts, not only the attachment',
  )
  assert.match(
    PHASE_4_SECTION,
    /attachment-guard-orig\.md[\s\S]{0,280}only.*mandatory secret\/PII redactions/i,
    'the safe source must be derived from Phase 1 notes, not a generated artifact',
  )
  assert.match(
    PHASE_4_SECTION,
    /--original "\$ORIG" --rewritten "\$NEW"[\s\S]{0,180}--expect-images "\$EXPECTED_IMAGES" --require-unsigned-uploads/,
    'the raw source must retain image parity checks without demanding unredacted verbatim notes',
  )
  assert.match(
    PHASE_4_SECTION,
    /--original "\$ORIG" --rewritten "\$SAFE_ORIG"[\s\S]{0,120}--require-safe-source/,
    'the safe source must be mechanically checked against the raw Phase 1 notes',
  )
  assert.match(
    PHASE_4_SECTION,
    /--original "\$SAFE_ORIG" --rewritten "\$NEW"[\s\S]{0,120}--require-verbatim --require-unsigned-uploads/,
    'the tracker description must preserve the independently prepared safe source',
  )
  assert.match(
    PHASE_4_SECTION,
    /--original "\$SAFE_ORIG" --rewritten "\$PLAN_FILE"[\s\S]{0,120}--require-verbatim --require-unsigned-uploads/,
    'the attachment must preserve the same safe source',
  )
})

test('Phase 4 carries a mandatory plan-contract STOP gate before the tracker write (BOS-741)', () => {
  // Assert on BEHAVIOURAL WORDING, not just the helper name: a name-exact ratchet stays green while
  // the surrounding prose has stopped saying the gate is mandatory or that failure means no write.
  assert.match(
    PHASE_4_SECTION,
    /STOP — plan-contract gate \(mandatory, mechanical, do not skip\)/,
    'Phase 4 must carry the contract gate as a mandatory mechanical STOP',
  )
  assert.match(
    PHASE_4_SECTION,
    /plan-contract-guard\.mjs" --description "\$NEW" --plan "\$PLAN_FILE"/,
    'the contract gate must run the guard over the composed description AND the plan file',
  )
  // The gate must be the LAST word before the numbered steps: a gate that runs after the write is
  // not a gate. `1.` is the first numbered Phase 4 step (finalize the attachment).
  const gateAt = PHASE_4_SECTION.indexOf('STOP — plan-contract gate')
  const firstStepAt = PHASE_4_SECTION.search(/^1\. /m)
  assert.ok(gateAt > 0 && firstStepAt > gateAt, 'the contract gate must precede Phase 4 step 1')
  // …and it must sit AFTER the image-parity gate, not before it.
  assert.ok(
    gateAt > PHASE_4_SECTION.indexOf('STOP — image-parity gate'),
    'the contract gate must follow the image-parity gate',
  )
  const gateBlock = PHASE_4_SECTION.slice(gateAt)
  assert.match(
    gateBlock,
    /SAFE branch[\s\S]{0,200}no Linear write/,
    'the contract gate must name the SAFE branch and that it performs no tracker write',
  )
  assert.match(gateBlock, /exit non-zero/, 'the SAFE branch must exit non-zero')
  assert.match(
    gateBlock,
    /zero\*\* extra tracker[\s>]+reads/,
    'the gate must state it adds no additional tracker read',
  )
  for (const code of [
    'missing-sections',
    'unknown-section',
    'section-order',
    'placeholder-residue',
    'not-a-description',
    'plan-file-residue',
    'unreadable-input',
  ]) {
    assert.ok(gateBlock.includes(code), `the contract gate must name violation code ${code}`)
  }
})

test('the resident body documents the config-first validatePlanDescription signature (BOS-741)', () => {
  assert.match(
    SKILL,
    /validatePlanDescription\(config, description\)/,
    'the plan-contract reference must state the config-first argument order',
  )
  assert.match(
    SKILL,
    /config-first\*\* order;[\s\S]{0,140}named\s+argument-order error/,
    'the resident body must say the swapped call throws a named argument-order error',
  )
})

test('the drafting brief runs the contract guard before the ok sentinel (BOS-741)', () => {
  assert.match(
    BRIEF,
    /plan-contract-guard\.mjs" --description <new\.md> --plan "\$PLAN_PATH"/,
    'the brief must run the contract guard over the description and the plan file',
  )
  assert.match(
    BRIEF,
    /non-zero exit means write no `ok` sentinel/,
    'a contract violation must block the ok sentinel, not merely be reported',
  )
  const guardAt = BRIEF.indexOf('plan-contract-guard.mjs')
  assert.ok(guardAt > 0 && guardAt < BRIEF.indexOf('## Step 9'), 'the guard runs inside Step 8')
})

test('the drafting brief pins the cased ## Open Questions heading and plan-body-only rule (BOS-741)', () => {
  assert.match(
    BRIEF,
    /\*\*`## Open Questions`\*\*[\s\S]{0,160}exact casing, capital `Q`/,
    'the brief must state the exact cased heading so plan file and description agree',
  )
  assert.match(
    BRIEF,
    /## Open questions/,
    'the brief must name the drifted lower-case spelling it is correcting',
  )
  assert.match(
    BRIEF,
    /plan file must contain ONLY the plan body/,
    'the brief must require the plan file to carry only the plan body',
  )
  assert.match(
    BRIEF,
    /No tool-call scaffolding[\s\S]{0,120}transcript residue/,
    'the plan-body-only rule must name the residue it excludes',
  )
})

test('Phase 4 counts canonical upload identities for the image guard (BOS-702)', () => {
  assert.match(
    PHASE_4_SECTION,
    /distinct canonical upload identities[\s\S]{0,140}origin plus pathname, ignoring query strings/i,
    'EXPECTED_IMAGES must use the same signed-URL-normalizing identity as the guard',
  )
  assert.ok(
    PHASE_4_SECTION.includes(
      'EXPECTED_IMAGES="<distinct canonical upload identities observed in Phase 1>"',
    ),
    'the command placeholder must not ask callers to count raw upload URLs',
  )
})

test('Phase 4 deletes guard scratch before every failed-gate exit (BOS-702)', () => {
  assert.match(
    PHASE_4_SECTION,
    /cleanup_guard_scratch\(\)[\s\S]{0,240}rm -f "\$ORIG" "\$SAFE_ORIG" "\$NEW" "\$PLAN_FILE"/,
    'the failed-gate cleanup must remove raw, safe, rewritten, and plan scratch files',
  )
  assert.equal(
    (PHASE_4_SECTION.match(/cleanup_guard_scratch\n>   exit 1/g) ?? []).length,
    4,
    'each of the four image-guard failures must clean scratch before exiting',
  )
  // Counting one helper name cannot see a failed-gate exit that cleans up some OTHER way — which is
  // how the BOS-741 contract gate shipped leaking `$ORIG`/`$SAFE_ORIG`. Assert the property instead:
  // EVERY `exit 1` in Phase 4 must be preceded by a cleanup naming all four scratch paths.
  const exits = PHASE_4_SECTION.split(/^>\s+exit 1$/m)
  assert.ok(exits.length - 1 >= 5, 'Phase 4 must carry the image gates plus the contract gate')
  for (const [i, before] of exits.slice(0, -1).entries()) {
    const tail = before.slice(-400)
    assert.ok(
      /cleanup_guard_scratch$/m.test(tail) ||
        /rm -f "\$ORIG" "\$SAFE_ORIG" "\$NEW" "\$PLAN_FILE"/.test(tail),
      `failed-gate exit #${i + 1} must delete all four scratch paths — $ORIG may carry sensitive content and Phase 5 never runs after exit 1`,
    )
  }
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
  // BOS-663: the dispatch must read the descriptor's `skillPath` from disk. Loading by the bare
  // descriptor `name` through the Skill tool is refused for any skill declaring
  // `disable-model-invocation: true`, which every extension now does — so the pre-663 literal this
  // once pinned would silently inert the whole draft-extension layer.
  assert.match(
    `${INTERACTIVE}\n${BRIEF}`,
    /descriptor's\s+`skillPath`/i,
    "draft dispatch must load the extension by its descriptor's skillPath",
  )
  assert.doesNotMatch(
    `${INTERACTIVE}\n${BRIEF}`,
    /load each discovered extension by its returned\s+descriptor\s+`name`\s+via the Skill tool/i,
    'draft dispatch must not load a discovered extension by name via the Skill tool',
  )
  for (const marker of ['Tier 1', 'Tier 2', 'Tier 3']) {
    assert.ok(INTERACTIVE.includes(marker), `interactive-mode.md must document ${marker}`)
  }
  // The legacy-dependency gate itself lives in its own test below (BOS-813) — it covers the whole
  // core reference set plus the active repo-local planning skills, not just these three bodies.
})

// ---------------------------------------------------------------------------
// BOS-813 — no ACTIVE planning surface may name a legacy planning dependency.
// ---------------------------------------------------------------------------
//
// The pre-813 gate was name-exact AND verb-exact (it required the word "Invoke" before the skill
// name) and therefore vacuous: it passed green the whole time references/interactive-mode.md said
// "Compute the slug + branch exactly the way `plan-eng-review` does" and shelled out to a retired
// third-party helper binary — a hard requirement the assertion could not see, because the sentence
// did not begin with "Invoke".
//
// The replacement matches ANY mention of a legacy planning skill anywhere in an active planning
// surface. Historical records (docs/plans/**, TODOS.md) are deliberately NOT surfaces: the ticket
// orders them preserved verbatim.
//
// `writing-plans` is matched bare rather than plugin-qualified: the skill is reachable under its
// bare name too, so a surface naming it that way would slip a `<plugin>:`-anchored pattern while
// carrying exactly the dependency BOS-813 removed. Matching bare also covers every qualified form,
// since the qualified name contains the bare one as a substring.
//
// BOS-815: the retired tooling's own names are no longer listed here. They are gated repo-wide —
// not merely across planning surfaces — by scripts/legacy-support-refs.test.mjs, which scans every
// tracked file. Naming them in a second place would reintroduce the very strings this ticket
// removed, and would be strictly weaker than the tree-wide scan that now subsumes it.
const LEGACY_PLANNING_DEP = /plan-eng-review|writing-plans/i

/** Every active planning surface, as `[label, body]`.
 *
 *  Both halves are enumerated from disk rather than listed literally. The core's references come
 *  from `readdirSync`; the repo-local half comes from `discoverExtensions` over every role plus the
 *  planning sweep entry point, which is not an extension of the core but drives the same flow. A
 *  hand-maintained path list is the part of a ratchet that rots: the literal list this replaced
 *  named two files and so never scanned `.claude/skills/boss-plan-notes/`, an active planning
 *  surface that was on disk the whole time.
 */
function activePlanningSurfaces() {
  const surfaces = [[`${CORE}/SKILL.md`, SKILL]]
  const refDir = new URL(`${CORE}/references/`, import.meta.url)
  for (const entry of readdirSync(refDir).sort()) {
    if (!entry.endsWith('.md')) continue
    surfaces.push([`${CORE}/references/${entry}`, readFileSync(new URL(entry, refDir), 'utf8')])
  }
  const { extensions } = discoverExtensions({ core: 'boss-plan', root: REPO_ROOT })
  const localDirs = extensions.map((ext) => [ext.name, ext.dir])
  localDirs.push(['bs-sweep-plan', join(REPO_ROOT, '.claude', 'skills', 'bs-sweep-plan')])
  for (const [name, dir] of localDirs) {
    for (const [rel, body] of markdownUnder(dir)) {
      surfaces.push([`.claude/skills/${name}/${rel}`, body])
    }
  }
  return surfaces
}

/** Every prose-or-script file under `dir`, recursively, as `[pathRelativeToDir, body]`, sorted for
 *  determinism. Executable planning surfaces count: `.claude/skills/bs-sweep-plan/gate/gate.mjs`
 *  drives the sweep and could name a legacy dependency just as a SKILL.md could. */
function markdownUnder(dir, prefix = '') {
  const out = []
  for (const entry of readdirSync(dir, { withFileTypes: true }).sort((a, b) =>
    a.name.localeCompare(b.name),
  )) {
    const rel = prefix ? `${prefix}/${entry.name}` : entry.name
    if (entry.isDirectory()) out.push(...markdownUnder(join(dir, entry.name), rel))
    else if (/\.(md|mjs|js|sh)$/.test(entry.name))
      out.push([rel, readFileSync(join(dir, entry.name), 'utf8')])
  }
  return out
}

test('BOS-813: no active planning surface mentions a legacy planning dependency', () => {
  const surfaces = activePlanningSurfaces()
  // Guard the guard: an empty or truncated surface list would make every assertion below vacuous.
  assert.ok(
    surfaces.length >= 5,
    `expected the core + its references + the repo-local planning skills, got ${surfaces.length}`,
  )
  // Name the two extensions the enumeration must reach: the CE draft extension this ticket added,
  // and the notes extension the superseded literal list silently omitted.
  for (const name of ['boss-plan-compound-engineering', 'boss-plan-notes']) {
    assert.ok(
      surfaces.some(([label]) => label.startsWith(`.claude/skills/${name}/`)),
      `${name} must be among the scanned surfaces — disk enumeration, not a hand-kept list`,
    )
  }
  for (const [label, body] of surfaces) {
    const hit = body.match(LEGACY_PLANNING_DEP)
    assert.equal(
      hit,
      null,
      `${label} must not name a legacy planning dependency (found ${JSON.stringify(hit?.[0])}); ` +
        'planning drafting goes through the discovered role:draft extension, and the published ' +
        'core stays project-agnostic',
    )
  }
})

test('BOS-693: one draft-success predicate governs skip recording and tier fall-through', () => {
  // The pre-693 Phase 4 used two different, undefined criteria for the same decision: Tier 1 fell
  // through only when "every discovered extension failed to load or returned no valid envelope",
  // while Tiers 2/3 entered when "no extension ran successfully". An extension that loaded, returned
  // a valid envelope, and then never wrote the plan satisfies the second and not the first — so the
  // drafting layer could be silently dropped with no plan on disk and no skip recorded.
  assert.match(
    INTERACTIVE,
    /draft success predicate/i,
    'interactive-mode.md must name one draft success predicate for the Tier-1 gate',
  )
  // (a) valid result AND (b) the requested non-empty plan actually produced at `planPath`.
  assert.match(
    INTERACTIVE,
    /valid[\s\S]{0,160}\bAND\b[\s\S]{0,200}non-empty[\s\S]{0,80}`planPath`/,
    'the draft success predicate must require BOTH a valid result and a non-empty plan at `planPath`',
  )
  assert.match(
    INTERACTIVE,
    /valid envelope\s+that wrote no plan/i,
    'interactive-mode.md must state that a valid envelope without the plan is not a success',
  )
  // Every failed dispatch is recorded as it is classified — a succeeding sibling does not excuse
  // omitting its failed peers from the ledger.
  assert.match(
    INTERACTIVE,
    /extension <name>: skipped \(<reason>\)/,
    'interactive-mode.md must keep the standard skip-ledger entry',
  )
  assert.match(
    INTERACTIVE,
    /including when a sibling succeeded/i,
    'a failed draft dispatch must still be recorded when a sibling extension succeeds',
  )
  // The SAME predicate gates both "suppress tiers 2/3" and "enter Tier 2/Tier 3", so the two can
  // never drift apart again.
  assert.ok(
    count(INTERACTIVE, 'succeeded under the draft success predicate') >= 3,
    'the Tier-1 suppression gate and both Tier-2/Tier-3 entry gates must cite the same draft success predicate',
  )
  // Match on the collapsed body: prettier rewraps this reference at 100 columns, so the line break
  // between "Tier 2, then" and "Tier 3" moves whenever a word ahead of it changes. A regex pinned
  // to today's break reds on a reflow that changed no meaning.
  assert.match(
    INTERACTIVE.replace(/\s+/g, ' '),
    /fall through to Tier 2, then Tier 3/,
    'interactive-mode.md must keep the Tier-2/Tier-3 fall-through',
  )
  assert.doesNotMatch(
    INTERACTIVE,
    /If at least one extension ran successfully/i,
    'the suppression gate must not use the undefined "ran successfully" criterion',
  )
  assert.doesNotMatch(
    INTERACTIVE,
    /if no extension ran successfully/i,
    'the Tier-2/Tier-3 entry gates must not use the undefined "ran successfully" criterion',
  )

  // BOS-813: the headless brief hands every Tier-1 dispatch a `runTmp` and anchors the per-dispatch
  // plan target to it, but nothing on that path ever created one — `RUN_TMP` appeared in the brief
  // only as a value being passed. Undefined it expands to empty, which collapses every sibling's
  // target onto one shared path and hollows out the attribution the success predicate below is
  // built on; a draft extension anchoring its staging there inherits the same emptiness. Anchored
  // to the seed block itself so an `RUN_TMP` mention anywhere else in the brief cannot satisfy it.
  {
    // Indent-aware, like the interactive seed gate below: a fence nested in a list item is
    // indented, and a column-0 anchor would pass vacuously on an empty candidate set.
    const seedBlocks = [...BRIEF.matchAll(/^([ \t]*)```bash\n([\s\S]*?)^\1```/gm)]
      .map(([, indent, block]) =>
        indent ? block.replace(new RegExp('^' + indent, 'gm'), '') : block,
      )
      .filter((block) => /mktemp\s+-d/.test(block))
    assert.equal(seedBlocks.length, 1, 'the headless brief must seed exactly one run scratch')
    assert.match(
      seedBlocks[0],
      /^RUN_TMP=\$\(mktemp -d/m,
      'the headless brief must create the runTmp it passes to every Tier-1 dispatch',
    )
    assert.match(
      seedBlocks[0],
      /^echo "\$RUN_TMP"$/m,
      'the seed block must print the scratch the brief tells the drafter to record',
    )
    assert.match(
      BRIEF.replace(/\s+/g, ' '),
      /Remove it with `rm -rf`[^.]*promoted the winning plan[^.]*failure paths too/i,
      'the headless brief must remove the run scratch it created, on the failure paths too',
    )
  }

  // The headless brief's Step 5 is the *shared* drafting spec that interactive-mode.md points at
  // for Tier 3, and it is the whole draft resolution on the headless path. It carried both defects
  // verbatim, so fixing only interactive-mode.md would leave the shared spec contradicting it.
  assert.match(
    BRIEF,
    /draft success predicate/i,
    'headless-drafting-brief.md Step 5 must carry the same draft success predicate',
  )
  assert.match(
    BRIEF,
    /valid[\s\S]{0,160}\bAND\b[\s\S]{0,200}non-empty[\s\S]{0,80}`PLAN_PATH`/,
    'the shared drafting spec must require BOTH a valid result and a non-empty plan at `PLAN_PATH`',
  )
  assert.match(
    BRIEF,
    /including when a sibling succeeded/i,
    'the shared drafting spec must record a failed dispatch even when a sibling succeeds',
  )
  assert.ok(
    count(BRIEF, 'succeeded under the draft success predicate') >= 3,
    'the shared drafting spec must gate Tier-1 suppression and both Tier-2/Tier-3 entries on the same predicate',
  )
  assert.doesNotMatch(
    `${BRIEF}\n${SKILL}`,
    /(?:If at least one|if no) extension ran successfully/i,
    'no boss-plan draft site may keep the undefined "ran successfully" criterion',
  )

  // The always-resident Fallback contract is in context before any reference is loaded, so it must
  // not state a looser definition of "succeeded" than the references do.
  assert.match(
    SKILL,
    /succeeded\*\* only when its\s+result is valid \*\*AND\*\* the requested non-empty plan exists/i,
    'SKILL.md must define a Tier-1 draft success as a valid result AND the produced plan',
  )
  assert.match(
    SKILL,
    /for every failed dispatch, including when a sibling\s+succeeded/i,
    'SKILL.md must record every failed draft dispatch, not only the all-failed case',
  )
  assert.doesNotMatch(
    SKILL,
    /If every discovered extension failed,\s+record/i,
    'SKILL.md must not scope draft skip recording to the all-extensions-failed branch',
  )

  // The predicate's second conjunct is a test of SHARED state: every sibling is dispatched with the
  // same plan path. "A plan is there now" therefore says nothing about the extension being
  // classified — once one sibling writes the plan, every later sibling that returns a valid
  // envelope and writes nothing reads as a success, keeps its skip out of the ledger, and holds
  // tiers 2/3 suppressed. That is the same false-success class the predicate exists to close, one
  // level down, so each site must attribute the output to the dispatch it is classifying.
  for (const [name, text] of [
    ['interactive-mode.md', INTERACTIVE],
    ['headless-drafting-brief.md', BRIEF],
    ['SKILL.md', SKILL],
  ]) {
    assert.match(
      text.replace(/\s+/g, ' '),
      /written by (\*\*)?th(is|at)(\*\*)? dispatch/i,
      `${name} must attribute the produced plan to the dispatch being classified, not merely to the shared plan path`,
    )
  }
  for (const [name, text] of [
    ['interactive-mode.md', INTERACTIVE],
    ['headless-drafting-brief.md', BRIEF],
  ]) {
    const flat = text.replace(/\s+/g, ' ')
    assert.match(
      flat,
      /is a test of shared state, not of this extension/i,
      `${name} must say why the bare existence check is insufficient`,
    )
    // …and the remedy has to be the dispatch TARGET, not a before/after comparison of one shared
    // path. Both halves of that comparison fail on some host these published skills legitimately
    // run on: identical bytes are the ordinary output of a deterministic redraft (a retry after a
    // failed upload), which a byte check records as a skip and drops to a lower tier that overwrites
    // a valid plan; and on a filesystem whose timestamp resolution is coarser than the rewrite, the
    // mtime does not advance either, so the same valid dispatch reads as skipped. Give each dispatch
    // its own path and the existence check attributes by construction, whatever the bytes say.
    assert.match(
      flat,
      /Per-dispatch plan target/i,
      `${name} must hand each draft dispatch its own plan target`,
    )
    assert.match(
      flat,
      /unique to the dispatch you are about to classify/i,
      `${name} must state that the per-dispatch target is unique to the dispatch being classified`,
    )
    assert.match(
      flat,
      /copy the file produced by the \*\*first\*\* dispatch that succeeded under the predicate below/i,
      `${name} must promote the winning per-dispatch plan onto the real plan target`,
    )
    assert.doesNotMatch(
      flat,
      /modification time to have moved/i,
      `${name} must not use timestamp inequality as proof that this dispatch wrote the plan`,
    )
    assert.match(
      flat,
      /modification time need not advance/i,
      `${name} must say why an mtime comparison cannot carry the attribution`,
    )
    assert.match(
      flat,
      /timestamp\s*resolution is coarser than the rewrite/i,
      `${name} must name the coarse-timestamp filesystem that defeats an mtime comparison`,
    )
    assert.match(
      flat,
      /Identical bytes are the ordinary output of a deterministic redraft/i,
      `${name} must say why a byte comparison cannot carry the attribution either`,
    )
  }
  assert.match(
    SKILL.replace(/\s+/g, ' '),
    /per-dispatch plan path that dispatch alone was given[\s\S]{0,120}never at a path a peer could have written/i,
    'the always-resident Fallback contract must not state a looser attribution rule than the references',
  )
})

test('the resident body states the draft Fallback contract', () => {
  assert.match(SKILL, /Fallback contract/, 'SKILL.md must name the Fallback contract')
  assert.match(
    SKILL,
    /extension.*host built-in.*inline prompt/is,
    'SKILL.md must state the extension -> host built-in -> inline prompt order',
  )
  // BOS-663: suppression keys on a Tier-1 dispatch SUCCEEDING, not on an extension merely being
  // present. Gating on presence made a failed dispatch silently drop the whole drafting layer.
  assert.match(
    SKILL,
    /tiers 2\/3 suppressed only when a Tier-1 dispatch\s+\*\*succeeded\*\*/i,
    'SKILL.md must state that lower fallback tiers are suppressed only when a Tier-1 dispatch succeeded',
  )
  assert.match(
    SKILL,
    /fall through to tier 2, then tier 3/i,
    'SKILL.md must state the fall-through when every discovered extension failed',
  )
})

test('BOS-813: the CE draft extension is authored and discoverable', () => {
  assert.match(
    DRAFT,
    /x-boss-extension:\s*\n\s+extends: boss-plan\s*\n\s+role: draft\s*\n\s+order: 40/,
    `${DRAFT_NAME} must declare the draft extension marker`,
  )
  const { extensions, skipped } = discoverExtensions({
    core: 'boss-plan',
    role: 'draft',
    root: REPO_ROOT,
  })
  // Discovery is by `role`, not by name — but the rename must still leave exactly one draft
  // extension standing, so a stale `boss-plan-draft` directory left behind by an incomplete
  // rename fails here rather than silently double-dispatching the draft step.
  assert.deepEqual(
    extensions.map((e) => e.name),
    [DRAFT_NAME],
    `boss-plan draft discovery must return exactly ${DRAFT_NAME}`,
  )
  assert.deepEqual(skipped, [], 'boss-plan draft discovery must have zero skips')
})

test('BOS-813: the CE draft extension points at the core drafting brief', () => {
  // Point at the canonical core source, not at `plugins/bossd-plugin-claude/skilldata/`. That tree
  // is a generated mirror `make copy-skills` overwrites, so a reader sent there reads a copy that
  // can lag the source it was copied from — and an editor sent there edits a file the next
  // regeneration discards.
  const rel = '../../../services/boss/internal/skillinstall/skills/boss-plan/references'
  assert.ok(
    DRAFT.includes(`${rel}/headless-drafting-brief.md`),
    `${DRAFT_NAME} must reference the canonical core drafting brief`,
  )
  assert.doesNotMatch(
    DRAFT,
    /plugins\/bossd-plugin-claude\/skilldata\/skills\/boss-plan\/references\/headless-drafting-brief\.md`? Step/,
    `${DRAFT_NAME} must not send readers to the generated skilldata mirror of the brief`,
  )
  // The pointer is a relative path from the extension's own directory: resolve it rather than
  // trusting the string, so a moved or renamed brief reds here instead of rotting silently.
  const draftDir = join(REPO_ROOT, '.claude', 'skills', DRAFT_NAME)
  assert.ok(
    existsSync(join(draftDir, rel, 'headless-drafting-brief.md')),
    'the drafting-brief pointer must resolve to a file that exists',
  )
})

test('BOS-813: the CE draft extension drives CE natively in both modes', () => {
  // AC#2: interactive planning uses CE's own interview/review; cron planning stays
  // non-interactive. Pin the CE skill names this repo verified as installed, and pin that the
  // headless path names CE's non-interactive entry rather than re-using the interactive one.
  for (const skill of ['ce-plan', 'ce-doc-review']) {
    assert.ok(DRAFT.includes(`\`${skill}\``), `${DRAFT_NAME} must drive CE through \`${skill}\``)
  }
  const interactive = sectionBetween(DRAFT, '### Interactive', '### Headless')
  const headless = sectionBetween(DRAFT, '### Headless', '## Normalize and promote')
  assert.match(
    interactive,
    /AskUserQuestion/,
    'the interactive path must keep the blocking question tool available for CE’s interview',
  )
  assert.match(
    headless,
    /Never\s+call\s+`AskUserQuestion`/i,
    'the headless path must forbid the blocking question tool',
  )
  // CE's pipeline-mode exception already runs `ce-doc-review` in headless mode before returning
  // control, so the headless path must NOT invoke it a second time — a second dispatch re-runs the
  // whole persona fan-out and applies its `safe_auto` mutations to the plan twice.
  assert.match(
    headless,
    /Do \*\*not\*\* invoke `ce-doc-review` yourself/i,
    'the headless path must not re-invoke CE document review on top of CE’s own headless pass',
  )
  assert.match(
    headless,
    /runs `ce-doc-review` in headless mode itself/i,
    'the headless path must say why it does not re-invoke: CE runs the review pass itself',
  )
  assert.match(
    headless,
    /\*\*pipeline\*\*\s+\(non-interactive\)/i,
    'the headless path must invoke the CE planner on its non-interactive pipeline path',
  )

  // CE ends a markdown run with a post-generation menu whose branches create a tracker issue from
  // the plan and start implementing it — and CE fires the routed action itself rather than just
  // rendering the menu. Deferring to that menu would mint a second, unmanaged copy of the ticket
  // outside the core's attachment-before-writeback and image-parity contracts, or begin
  // implementing mid-plan. The interactive path must stop at CE's plan file instead.
  assert.doesNotMatch(
    interactive,
    /do not shortcut its menus/i,
    'the interactive path must not hand CE’s post-generation menu blanket authority',
  )
  assert.match(
    interactive,
    /never a CE menu action/i,
    'the interactive path must name CE’s plan file — not a menu action — as the deliverable',
  )
  assert.match(
    interactive.replace(/\s+/g, ' '),
    /decline every menu branch that leaves the plan file/i,
    'the interactive path must decline the menu branches that leave the plan file',
  )
  assert.match(
    interactive.replace(/\s+/g, ' '),
    /the core owns the tracker write/i,
    'the interactive path must say why: the core, not CE, owns the tracker write',
  )
  assert.match(
    interactive.replace(/\s+/g, ' '),
    /already been created[\s\S]{0,60}before you could decline[\s\S]{0,400}failure envelope/i,
    'an already-fired tracker-issue branch must route to the failure envelope, not be papered over',
  )
})

test('BOS-813: the CE draft extension fails its envelope when CE cannot load', () => {
  // AC#3: a missing CE must skip the extension and let the portable core fallback run. The core's
  // draft-success predicate needs BOTH a valid envelope AND a plan at the passed planPath, so an
  // `ok: false` envelope with no plan is what routes the run to Tier 2 / Tier 3.
  assert.match(
    DRAFT,
    /"ok":\s*false/,
    `${DRAFT_NAME} must document an ok:false envelope for the unavailable-CE path`,
  )
  assert.match(
    DRAFT,
    /Tier\s+2[\s\S]{0,80}Tier\s+3/,
    `${DRAFT_NAME} must say the failure envelope routes the core to its own fallback tiers`,
  )
  assert.match(
    DRAFT,
    // `\*{0,2}` tolerates the bold emphasis the prose carries ("do **not** draft"); `\s+` between
    // words tolerates prettier's 100-col wrapping.
    /do\s+\*{0,2}not\*{0,2}\s+draft\s+the\s+plan\s+inline/i,
    `${DRAFT_NAME} must not degrade to drafting the plan itself when CE is missing`,
  )
})

test('BOS-813: the CE draft extension stages CE output and cleans it up', () => {
  // AC#4 + constraint D: CE writes its plan to a repo path of its own choosing, so the extension
  // must promote to `context.planPath`, normalize to the plan contract, and leave nothing behind.
  assert.match(
    DRAFT,
    /docs\/plans\/YYYY-MM-DD/,
    `${DRAFT_NAME} must name the CE staging path it is responsible for removing`,
  )
  assert.match(
    DRAFT,
    /Nothing\s+CE\s+wrote\s+\*\*outside\*\*\s+`runTmp`\s+may\s+survive/i,
    `${DRAFT_NAME} must forbid CE artifacts surviving outside the core-supplied scratch`,
  )
  assert.match(
    DRAFT,
    /Cleanup\s+is\s+unconditional/i,
    `${DRAFT_NAME} must run staging cleanup on the failure paths too`,
  )
  assert.match(
    DRAFT,
    /planContract\.version:\s*1/,
    `${DRAFT_NAME} must normalize the CE plan to the versioned plan contract`,
  )
  assert.match(
    DRAFT,
    /`-\s+Contract:\s+v<N>`/,
    `${DRAFT_NAME} must stamp the in-band contract version under \`## Planning\``,
  )
  // AC#4 is "every CONFIGURED plan-contract section", so the list is read from the config rather
  // than snapshotted here: a hardcoded copy stays green when someone renames or adds a section in
  // .boss-skills.json, which is exactly the drift this gate exists to catch.
  const sections = JSON.parse(readFileSync(join(REPO_ROOT, '.boss-skills.json'), 'utf8'))
    .planContract.sections
  assert.ok(sections.length >= 9, 'the plan contract must still declare its sections')
  for (const { heading } of sections) {
    assert.ok(
      DRAFT.includes(`\`${heading}\``),
      `${DRAFT_NAME} must name the configured plan-contract section ${heading}`,
    )
  }
  assert.match(
    DRAFT,
    /verbatim/i,
    `${DRAFT_NAME} must preserve the ticket's original notes verbatim`,
  )
})

test('BOS-813: the published core seeds its design doc in its own private scratch', () => {
  // Constraint A: boss-plan is published into every user's global skill dir, so the design-doc
  // seed may not reach into a third-party tool's directory or any user-global path. `mktemp -d`
  // under the core's own scratch is the portable replacement.
  assert.match(
    INTERACTIVE,
    /mktemp\s+-d\s+"\$\{TMPDIR:-\/tmp\}\/boss-plan-run/,
    'the design-doc seed must live under a run-private mktemp scratch',
  )
  assert.doesNotMatch(
    INTERACTIVE,
    /\$HOME\/\.[a-z]/,
    'the published core must not seed a design doc under a user-global dotdir',
  )
  // The optional envelope field survives the rewrite so existing extensions keep working.
  assert.match(INTERACTIVE, /designDoc/, 'the draft envelope must still carry context.designDoc')
  // Cleanup forbids reconstructing the scratch path (the mktemp suffix is non-deterministic), so
  // the seed block has to PRINT it. A block that prints only the design-doc path leaves the agent
  // with nothing to `rm -rf`, and nothing to hand Phase 4 as `runTmp`.
  //
  // Anchored to the seed block itself, not to the document: an `echo "$RUN_TMP"` in some unrelated
  // fence elsewhere in the reference would satisfy a whole-file match while the block the agent
  // actually copies still printed nothing.
  // Indent-aware: the seed fence is nested inside a numbered list item, so a column-0 anchor
  // matches nothing and the gate below would pass vacuously on an empty candidate set.
  const seedBlocks = [...INTERACTIVE.matchAll(/^([ \t]*)```bash\n([\s\S]*?)^\1```/gm)]
    .map(([, indent, block]) =>
      indent ? block.replace(new RegExp('^' + indent, 'gm'), '') : block,
    )
    .filter((block) => /mktemp\s+-d/.test(block))
  assert.equal(seedBlocks.length, 1, 'exactly one bash block may seed the run scratch')
  assert.match(
    seedBlocks[0],
    /^echo "\$RUN_TMP"$/m,
    'the seed block must print the run scratch it tells you to record',
  )
  assert.match(
    seedBlocks[0],
    /^ISSUE_ID="\$\{ISSUE_ID:\?/m,
    'the seed block must guard the id it interpolates rather than expanding it to empty',
  )
})

test('plan-reviewer discovery ignores the draft sibling', () => {
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
  const SAFE_SOURCE = '<ISSUE-ID>*.attachment-guard-orig.md'
  assert.equal(
    lines.filter((l) => l.includes(`-name '${SAFE_SOURCE}' -delete`)).length,
    3,
    'every cleanup path must delete the redacted safe-source scratch file',
  )
  assert.equal(
    lines.filter((l) => l.includes(`-name '${SAFE_SOURCE}'`) && l.includes('-print)')).length,
    3,
    'every residual sweep must detect a surviving redacted safe-source scratch file',
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
    lines.filter((l) => /\[\s+"\$CLEANUP_RC"\s+(?:=|!=)\s+0\s+\]/.test(l)).length,
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
  const PRE_SPLIT_BASELINE = 88867
  // BOS-782 re-baselines 87975 → 88035 (+60 B), carrying PRE_SPLIT_BASELINE with it to keep the
  // 16-byte guard margin. The Phase 0 preflight and the Phase 3 issueSlug one-liner both built
  // their ESM specifier as `'file://' + <path>`, which resolves a RELATIVE toolbox path as a bare
  // package and truncates an absolute one at a `#`. Both now resolve through
  // `require("node:url").pathToFileURL(…).href` — kept in CommonJS and written without optional
  // whitespace precisely to hold the growth to +30 B per line, against +65 B for the
  // `--input-type=module` spelling. Resident by necessity: both lines are executed/copied verbatim.
  // Re-baselined a further +816 (88035 → 88851) for BOS-744, carrying PRE_SPLIT_BASELINE with it
  // to keep the 16-byte guard margin: Phase 3.5's resident sentence now names the literal
  // `--role plan-reviewer` token, and the Phase-8 notes discover site gained the
  // record-the-broken-skips clause. Both are resident by necessity — a headless orchestrator that
  // never opens `references/extension-reviewers.md` infers `--role review` from the phase name and
  // rejects every correctly-installed extension into `skipped`, and a discover site whose skips go
  // unrecorded reports its fallback tier as though it had always been the intended tier.
  const RATCHET = 88851 // exact measured resident body
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

test('the resident body cross-references how to dispatch a zero-change planning session', () => {
  // boss-plan is itself the canonical zero-change workload: it reads, drafts and
  // writes to the tracker, and commits nothing. Dispatched as an ordinary
  // session it finalizes blocked behind an empty draft PR.
  //
  // The pointer must live in the RESIDENT body, not references/*.md — the
  // default headless orchestrator path reads no reference (see the on-demand
  // references table above), so a cross-reference hidden in one is invisible to
  // exactly the path that needs it. Reading SKILL.md, not a whole-payload glob,
  // is what enforces that.
  const bullet = SKILL.split('\n').find(
    (line) => line.includes('Sessions that change nothing') && line.trimStart().startsWith('-'),
  )
  assert.ok(
    bullet,
    'the resident SKILL.md body must carry a bullet cross-referencing the boss skill section "Sessions that change nothing"',
  )
  for (const option of ['quick_chat', 'defer_pr', 'create_session']) {
    assert.ok(
      bullet.includes(option),
      `the zero-change cross-reference must name ${option}: ${bullet.trim()}`,
    )
  }
  for (const notAFlag of ['--quick-chat', '--defer-pr']) {
    assert.ok(
      !bullet.includes(notAFlag),
      `the zero-change cross-reference must not name ${notAFlag} — the pointer names the create_session field spellings; the CLI flags live in the boss skill's generated command reference`,
    )
  }
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
