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
import { spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, readdirSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'
import {
  DYNAMIC_VIOLATION_CODE_PREFIXES,
  VIOLATION_CODES,
} from '../skills-toolbox/plan-contract-guard.mjs'
import { discoverExtensions } from '../skills-toolbox/skill-extensions.mjs'
import { precedes, regionUntilNext } from './gate-region-lib.mjs'
import { assertExactSize, measureFile } from './size-ratchet-lib.mjs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const abs = (rel) => fileURLToPath(new URL(rel, import.meta.url))
const readIfExists = (rel) => {
  const url = new URL(rel, import.meta.url)
  return existsSync(url) ? readFileSync(url, 'utf8') : ''
}

const CORE = '../services/boss/internal/skillinstall/skills/boss-plan'
const PLUGIN_COPY = '../plugins/bossd-plugin-claude/skilldata/skills/boss-plan'
const SKILL = read(`${CORE}/SKILL.md`)
const INTERACTIVE = read(`${CORE}/references/interactive-mode.md`)
const BRIEF = read(`${CORE}/references/headless-drafting-brief.md`)
const EXTENSION_REVIEWERS = read(`${CORE}/references/extension-reviewers.md`)
const PLAN_STORAGE = read(`${CORE}/references/plan-storage.md`)
const PAYLOAD_REFERENCES = [
  ['SKILL.md', SKILL],
  ['references/headless-drafting-brief.md', BRIEF],
  ['references/extension-reviewers.md', EXTENSION_REVIEWERS],
  ['references/interactive-mode.md', INTERACTIVE],
  ['references/plan-storage.md', PLAN_STORAGE],
]
const PAYLOAD_TEXT = PAYLOAD_REFERENCES.map(([, body]) => body).join('\n')
const PAYLOAD_COPIES = [
  {
    name: 'skillinstall',
    skill: SKILL,
    brief: BRIEF,
  },
  {
    name: 'plugin mirror',
    skill: read(`${PLUGIN_COPY}/SKILL.md`),
    brief: read(`${PLUGIN_COPY}/references/headless-drafting-brief.md`),
  },
]
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

function draftBehaviorRegions(body) {
  assert.ok(
    precedes(body, '### Interactive', '### Headless', `${DRAFT_NAME} behavior sections`),
    `${DRAFT_NAME} must keep Interactive before Headless`,
  )
  assert.ok(
    precedes(body, '### Headless', '## Normalize and promote', `${DRAFT_NAME} behavior sections`),
    `${DRAFT_NAME} must keep Headless before Normalize and promote`,
  )

  return {
    interactive: regionUntilNext(
      body,
      '### Interactive',
      '\n### ',
      `${DRAFT_NAME} interactive section`,
    ),
    headless: regionUntilNext(body, '### Headless', '\n## ', `${DRAFT_NAME} headless section`),
  }
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
const PHASE_0_SECTION = sectionBetween(SKILL, '## Phase 0 — Preflight', '\n## Phase 1')
const PHASE_4_SECTION = sectionBetween(
  SKILL,
  '## Phase 4 — Finalize the plan attachment and write back to the tracker',
  '\n## Phase 5',
)
// BOS-1102: the eight-line inline probe collapsed to ONE sourced line. The resolution it used
// to spell out now lives in the shipped helper this line sources, so the payload docs must
// contain the line verbatim and no BOSS_PLAN_TOOLBOX assignment of their own. The locate tests
// `[ -f ]` rather than letting `.` fail: `.` is a POSIX special built-in, so under sh/dash a
// missing file exits the shell outright and a `. a || . b || { echo …; exit 1; }` chain would
// silently skip every remaining candidate along with its own BLOCKED message. ~/.claude is named
// twice on purpose: `${BOSS_SKILLS_HOME:-…}` defaults only when the variable is UNSET, so without
// the explicit second candidate a pre-set value drops ~/.claude out of the search entirely.
const TOOLBOX_PREAMBLE_LINE =
  'BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; ' +
  '[ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; ' +
  '[ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; ' +
  '[ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run \'boss skills install\'"; exit 1; }; ' +
  '. "$BOSS_PLAN_ENV"'
const TOOLBOX_ENV_HELPER = read(`${CORE}/toolbox/boss-plan-env.sh`)

function fencedBashBlocks(body) {
  return [...body.matchAll(/```bash\n([\s\S]*?)\n```/g)].map((match) => match[1])
}

function normalizeShell(body) {
  return body
    .split('\n')
    .map((line) => line.replace(/^>\s?/, '').trim())
    .join('\n')
}

// ---------------------------------------------------------------------------
// Toolbox path contract — every shell block resolves the same installed helper.
// ---------------------------------------------------------------------------

// AC2's second clause — "the helper fails loudly when the tree is absent" — is a runtime
// property, so it is pinned by RUNNING the shipped payload rather than by reading it. A shape
// assertion cannot tell a loud failure from a silent one: the whole reason the locate uses
// `[ -f ]` is that the cheaper `. a || . b` spelling exited sh/dash before its own error line
// ever ran, which every text pin in this file would still have called green.
const BLOCKED_MESSAGE =
  "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"

// Both loud paths, because they fail in different places: the locate line gives up before it
// finds anything to source, and the helper gives up after being sourced from a tree that turned
// out to carry nothing. Under `sh` as well as `bash` — `.` is a POSIX special built-in, so a
// bash-only run cannot see a message that an early shell exit has already skipped.
for (const shell of ['bash', 'sh']) {
  test(`boss-plan toolbox resolution fails loudly under ${shell} when no tree is installed`, () => {
    const emptyHome = mkdtempSync(join(tmpdir(), 'boss-plan-no-skills-'))
    const env = { PATH: process.env.PATH ?? '', HOME: emptyHome }

    for (const [label, script] of [
      ['locate line', TOOLBOX_PREAMBLE_LINE],
      ['sourced helper', `. ${JSON.stringify(abs(`${CORE}/toolbox/boss-plan-env.sh`))}`],
    ]) {
      const run = spawnSync(shell, ['-c', `${script}\nprintf '%s' "$BOSS_PLAN_TOOLBOX"`], {
        encoding: 'utf8',
        env,
      })
      assert.equal(run.error, undefined, `${label} failed to spawn under ${shell}`)
      assert.notEqual(
        run.status,
        0,
        `${label} exited 0 under ${shell} with no install tree; a silent success here ships an unset $BOSS_PLAN_TOOLBOX into every later command`,
      )
      assert.ok(
        `${run.stdout}${run.stderr}`.includes(BLOCKED_MESSAGE),
        `${label} under ${shell} exited non-zero without printing the BLOCKED remedy`,
      )
      assert.equal(
        run.stdout.includes(emptyHome),
        false,
        `${label} under ${shell} printed a resolved toolbox path despite having no install tree`,
      )
    }
  })
}

test('boss-plan resolves BOSS_PLAN_TOOLBOX through one canonical preamble', () => {
  // The only assignment left anywhere in the payload is the helper's own. An assignment in a
  // doc means some block re-derived the path by hand, which is the drift this pin exists for.
  assert.deepEqual(
    [...PAYLOAD_TEXT.matchAll(/BOSS_PLAN_TOOLBOX="([^"]*)"/g)].map((match) => match[1]),
    [],
    'payload docs must source the toolbox helper, never assign BOSS_PLAN_TOOLBOX inline',
  )
  assert.deepEqual(
    [...TOOLBOX_ENV_HELPER.matchAll(/BOSS_PLAN_TOOLBOX="([^"]*)"/g)].map((match) => match[1]),
    ['$BOSS_SKILLS_HOME/boss-plan/toolbox'],
    'the sourced helper must carry exactly one canonical BOSS_PLAN_TOOLBOX assignment',
  )
  assert.ok(
    TOOLBOX_ENV_HELPER.includes('export BOSS_SKILLS_HOME BOSS_PLAN_TOOLBOX'),
    'the helper must export what it resolves; a sourced value that is not exported dies with the block',
  )

  for (const staleRoot of ['.claude/skills/bossanova', '.codex/skills/bossanova']) {
    assert.equal(PAYLOAD_TEXT.includes(staleRoot), false, `payload must not mention ${staleRoot}`)
  }

  for (const [name, body] of PAYLOAD_REFERENCES) {
    for (const [index, block] of fencedBashBlocks(body).entries()) {
      if (!block.includes('$BOSS_PLAN_TOOLBOX')) continue
      if (!block.includes('"${BOSS_PLAN_TOOLBOX:?}"') && !block.includes('boss-plan-env.sh')) {
        continue
      }
      assert.ok(
        normalizeShell(block).includes(TOOLBOX_PREAMBLE_LINE),
        `${name} fenced bash block ${index + 1} uses $BOSS_PLAN_TOOLBOX without the canonical sourced preamble line`,
      )
    }
  }

  assert.match(SKILL, /That\s+first\s+`\.`\s+line\s+is\s+the\s+\*\*toolbox\s+preamble\*\*/)
  assert.match(SKILL, /\(drift\s+helper\s+not\s+installed\)/)
  assert.match(SKILL, /boss\s+skills\s+check\s+--gate/)
  assert.match(SKILL, /self-edited/)
  assert.match(SKILL, /drift\s+helper\s+not\s+installed/)
  assert.match(SKILL, /loadSkillConfig\(\{ cwd \}\)/)
  assert.match(
    SKILL,
    /conventional\s+tracker-adapter\s+operations\s+declared\s+in\s+the\s+adapter\s+`operationMap`/,
  )

  for (const [needle, body] of [
    ['"${BOSS_PLAN_TOOLBOX:?}"` after running the toolbox preamble first', SKILL],
    ['"${BOSS_PLAN_TOOLBOX:?}"`\n  after running the toolbox preamble first', BRIEF],
    [
      'discover --core boss-plan --role draft --json`\nafter running the toolbox preamble first',
      BRIEF,
    ],
    [
      'discover --core boss-plan --role draft --json`\nafter running the toolbox preamble first',
      INTERACTIVE,
    ],
    ['<headers-json-file>` after running the toolbox preamble first', PLAN_STORAGE],
    ['validate --role notes --file\n"<outPath>"` after running the toolbox preamble first', SKILL],
  ]) {
    assert.ok(body.includes(needle), `missing inline toolbox-preamble citation: ${needle}`)
  }
})

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
    /Do \*\*not\*\* draft\s+inline|Do\s+not\s+draft\s+inline/i,
    'headless mode must explicitly forbid inline drafting',
  )
  assert.doesNotMatch(
    HEADLESS_SECTION,
    /inline\s+fallback/i,
    'headless mode must not have an inline fallback',
  )
  assert.doesNotMatch(
    HEADLESS_SECTION,
    /draft\s+inline \*\*once\*\*/i,
    'headless mode must not route dispatch-tool errors into inline drafting',
  )
})

// ---------------------------------------------------------------------------
// Return contract — the subagent returns path + bounded metadata, never content.
// ---------------------------------------------------------------------------

test('the return contract is path + bounded metadata, never the plan content', () => {
  assert.match(
    SKILL,
    /only\s+the\s+plan-file\s+path\s+plus\s+a\s+bounded\s+metadata\s+object/i,
    'SKILL.md must state the subagent returns only the plan-file path + bounded metadata',
  )
  assert.match(
    SKILL,
    /never\s+the\s+plan\s+file's\s+content/i,
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
    /run-file\s+sentinel\s+only|from\s+the (run-)?file\s+only/i,
    'SKILL.md must classify from the run-file sentinel only (never from returned prose)',
  )
})

test('a missing/stale sentinel routes to the safe branch: no Linear write, non-zero exit', () => {
  assert.match(SKILL, /missing/i, 'must handle a missing sentinel (dead/failed subagent)')
  assert.match(SKILL, /stale/i, 'must handle a stale (foreign leftover) sentinel')
  assert.match(
    SKILL,
    /no\s+Linear\s+write/i,
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
    /metadata `planPath`\s+resolves\s+to `PLAN_PATH`/,
    'the returned metadata planPath must resolve to the validated sentinel path',
  )
  assert.match(
    PHASE_4_SECTION,
    /PLAN_FILE="\$\{PLAN_FILE:-\.linear-plans\/run-<RUN-SCRATCH-ID>\/<ISSUE-ID>-<slug>\.md\}"/,
    'Phase 4 must carry forward the already validated headless PLAN_FILE instead of overwriting it',
  )
})

test('headless Phase 2 measures on-disk artifacts instead of trusting reported sizes in both payload copies', () => {
  for (const payload of PAYLOAD_COPIES) {
    const headless = sectionBetween(
      payload.skill,
      '### Headless (`BOSS_CRON=true`) — dispatch ONE awaited drafting subagent',
      '\n## Phase 2.5',
    )
    assert.match(
      headless,
      /orchestrator\s+measures\s+on-disk\s+artifacts/i,
      `${payload.name}: headless Phase 2 must state the orchestrator measures artifacts`,
    )
    assert.match(
      headless,
      /reported\s+size\s+is\s+never\s+the\s+(?:source|input)/i,
      `${payload.name}: reported size must not be a decision input`,
    )
    assert.match(
      headless,
      /\b(stat|wc -c)\b/,
      `${payload.name}: the rule must name a concrete byte measurement command`,
    )
  }
})

test('post-sentinel re-verification covers all dispatch artifacts while retaining dispatch-failure shape in both payload copies', () => {
  for (const payload of PAYLOAD_COPIES) {
    const headless = sectionBetween(
      payload.skill,
      '### Headless (`BOSS_CRON=true`) — dispatch ONE awaited drafting subagent',
      '\n## Phase 2.5',
    )
    assert.match(
      headless,
      /every\s+orchestrator-consumed\s+artifact/i,
      `${payload.name}: post-sentinel check must cover every consumed artifact`,
    )
    assert.match(
      headless,
      /guard,\s+child-plan\s+and\s+epic-spec\s+scratch/i,
      `${payload.name}: post-sentinel check must name guard, child-plan, and epic-spec scratch`,
    )
    assert.match(
      headless,
      /child-plan/i,
      `${payload.name}: post-sentinel check must name child plan files`,
    )
    assert.match(
      headless,
      /\$DISPATCH_FAILURE:\s+sentinel\s+ok\s+but\s+artifact\s+missing\/empty\s+or\s+wrong\s+path/,
      `${payload.name}: widened check must retain the dispatch-failure abort-message shape`,
    )
    assert.match(
      headless,
      // prose-pin: literal-space ok — this pins a minified JavaScript code construct.
      /O=p\.resolve\(D,`\$\{R\}\.epic-spec\.json`\)/,
      `${payload.name}: post-sentinel check must bind the serialized spec to the canonical parent artifact`,
    )
    assert.doesNotMatch(
      headless,
      /T\(c\.id\)/,
      `${payload.name}: serialized epic specs intentionally omit child ids, so the verifier must not read c.id`,
    )
    assert.match(
      headless,
      // prose-pin: literal-space ok — this pins a minified JavaScript code construct.
      /T\(q\.parentId\)!==R/,
      `${payload.name}: post-sentinel check must validate serialized spec parentId against the epic parent`,
    )
    assert.match(
      headless,
      // prose-pin: literal-space ok — this pins a minified JavaScript code construct.
      /U=new Set[\s\S]*A\.findIndex\(y=>p\.basename\(c\)===`\$\{R\}-child-\$\{y\[0\]\}-\$\{n\(id,y\[1\]\)\}\.md`\);if\(j<0\|\|U\.has\(j\)\)E\(c\);else U\.add\(j\)/,
      `${payload.name}: post-sentinel check must bind child paths bijectively to exact canonical parent/key/id/title artifacts without spec child ids`,
    )
    assert.match(
      headless,
      /else\s+if\(!v\("planPath"\)\.some\(T\)\)E\("planPath"\)/,
      `${payload.name}: post-sentinel check must still require single-ticket planPath`,
    )
    assert.match(
      headless,
      /z===P0\|\|p\.dirname\(z\)===D/,
      `${payload.name}: post-sentinel check must allow PLAN_PATH or direct .linear-plans children by resolved path`,
    )
  }
})

test('the epic sentinel carries required artifact paths in both payload copies', () => {
  for (const payload of PAYLOAD_COPIES) {
    assert.match(
      payload.brief,
      /childPlanPaths:\{\(\$childId\):\$childPlan\}/,
      `${payload.name}: epic sentinel must carry child plan paths keyed by child id`,
    )
    assert.match(
      payload.brief,
      /childIds:\$childIds/,
      `${payload.name}: epic sentinel must carry child ids for manifest cardinality`,
    )
    assert.match(
      payload.brief,
      /epicSpecPaths:\$epicSpecPaths/,
      `${payload.name}: epic sentinel must carry optional epic spec body scratch paths`,
    )
    assert.match(
      payload.brief,
      /<PARENT>-child-<key>-<slug>[.]md[.]rejected/,
      `${payload.name}: epic sentinel must retain rejected child plan structure artifacts in guardScratchPaths`,
    )
    assert.doesNotMatch(
      payload.brief,
      /childPlanKeysById:\{/,
      `${payload.name}: epic sentinel must not carry child keys from the drafting worker`,
    )
    assert.match(
      payload.brief,
      /maps\s+each\s+actual\s+child\s+id\s+to\s+that\s+child's\s+actual\s+plan\s+path/i,
      `${payload.name}: epic brief must bind child plan paths to child ids`,
    )
    assert.match(
      payload.brief,
      /Do\s+not\s+copy\s+child\s+keys\s+into\s+the\s+sentinel\s+payload/i,
      `${payload.name}: epic brief must require spec-derived child keys`,
    )
    assert.match(
      payload.brief,
      /prefix\s+does\s+not\s+satisfy\s+a\s+missing\s+child/i,
      `${payload.name}: epic brief must reject unrelated paths as child-plan substitutes`,
    )
    assert.match(
      payload.brief,
      /refuses\s+an\s+epic\s+sentinel\s+that\s+omits\s+the\s+required\s+epic\s+artifact\s+paths/i,
      `${payload.name}: epic brief must state that artifact paths are required`,
    )
    assert.match(
      payload.brief,
      /retained\s+`<child-plan>[.]md[.]rejected`\s+structure\s+artifact/i,
      `${payload.name}: epic brief must say retained structure artifacts belong in guardScratchPaths`,
    )
  }
})

test('the headless drafting brief requires measured reported sizes in both payload copies', () => {
  for (const payload of PAYLOAD_COPIES) {
    assert.match(
      payload.brief,
      /any\s+(?:byte\s+count|size).{0,80}reports?.{0,80}measured.{0,80}(?:stat|wc\s+-c)/is,
      `${payload.name}: the drafter must measure any size it reports`,
    )
    assert.match(
      payload.brief,
      /unmeasurable\s+size.{0,80}unmeasured|cannot\s+measure.{0,80}unmeasured/is,
      `${payload.name}: unmeasurable sizes must be reported as unmeasured`,
    )
  }
})

test('Phase 4 step 5 supplies the three inputs the library cannot derive for itself', () => {
  // Each of these is a SILENT failure in the shipped glue rather than an error.
  // A logical verdict with no direction lets the library orient a declared
  // prerequisite by priority and write the edge backwards; `extractKeyChangeAreas`
  // drops every slash-free area token when `moduleRoots` is absent, so a plan that
  // names bare module names contributes no areas and every overlap is missed; and
  // the expansion depth cap bounds ONE call, so a re-run loop that resets it can
  // walk a malformed parent/child graph forever.
  assert.match(
    PHASE_4_SECTION,
    /\*\*Direction\s+is\s+part\s+of\s+the\s+verdict\*\*/,
    'step 5(b) must tell the caller a logical verdict carries its own direction',
  )
  assert.match(
    PHASE_4_SECTION,
    /`blockedBy`\s+\(the\s+default[^)]*\)\s+or\s+`blocks`/,
    'step 5(b) must name both directions and say which one a bare verdict means',
  )
  assert.ok(
    PHASE_4_SECTION.includes('moduleRoots'),
    'step 5(c) must build moduleRoots into the classify payload',
  )
  assert.match(
    PHASE_4_SECTION,
    /extractKeyChangeAreas\(g,x\.description,\{moduleRoots:/,
    'the step 5(c) invocation must PASS moduleRoots through — naming it in prose while the runnable line drops it is the same silent miss',
  )
  assert.match(
    PHASE_4_SECTION,
    /add\s+every\s+parent\s+id\s+you\s+have\s+already\s+expanded\s+to\s+`excludeIds`/,
    'step 5(d) must bound the documented epic re-run loop, which resets the library depth cap',
  )
  // BOS-1187. The library can refuse to fabricate an edge, but only the caller can
  // refuse to READ an arealess scan as a clean one — these four pins are the half
  // of the fix that does not live in plan-deps-lib.mjs.
  assert.match(
    PHASE_4_SECTION,
    /verbatim\*\*[\s\S]{0,200}coarsening/,
    'step 5(b) must say a candidate’s `## Key changes` entries are fed in verbatim — paraphrasing a path into a coarser one manufactures an edge, because overlap is containment-based',
  )
  assert.match(
    PHASE_4_SECTION,
    /`repoWideTokens`,\s+`areaAliases`/,
    'step 5(c) must build the suppression and alias tuning into the classify payload, not only moduleRoots',
  )
  assert.match(
    PHASE_4_SECTION,
    /subjectAreas[\s\S]{0,120}\*\*before\s+any\s+edge\s+is\s+written\*\*/,
    'step 5(c) must require the printed subjectAreas line to be read BEFORE any edge write — a non-zero `compared` over an empty subjectAreas prints byte-identically to a clean scan',
  )
  assert.match(
    PHASE_4_SECTION,
    /`no-subject-areas`\s+or\s+`subject-unresolved-areas`[\s\S]{0,200}could\s+not\s+evaluate/,
    'step 5(d) must carry an imperative branch for both arealess warnings; a library warning that reaches no caller branch changes nothing',
  )
  assert.match(
    PHASE_4_SECTION,
    /i\.subjectUnresolvedAreas=s\.unresolved/,
    'the step 5(c) invocation must FEED the unresolved list back to planDependencyEdges — naming the branch in prose while the runnable line drops the input is the same silent miss',
  )
})

test('Phase 4 step 5 names its dependency library adjacent to the toolbox variable', () => {
  // The shipped-toolbox gate (services/boss/internal/skillinstall/skills_manifest_test.go)
  // keys on a literal `$BOSS_<CORE>_TOOLBOX/<file>` token. The step-5 invocation reaches
  // the helper by concatenation (`T+"/plan-deps-lib.mjs"`), which that gate structurally
  // cannot see — so without an adjacent mention the file this whole step depends on
  // could drop out of the payload with every gate green.
  assert.ok(
    PHASE_4_SECTION.includes('$BOSS_PLAN_TOOLBOX/plan-deps-lib.mjs'),
    'the dependency library must be named adjacent to $BOSS_PLAN_TOOLBOX so the shipped-toolbox gate covers it',
  )
})

test('Phase 4 step 5 warns when an auto-linked blocker is itself transitively blocked (BOS-287)', () => {
  assert.ok(
    PHASE_4_SECTION.includes('Transitive-block warning'),
    'Phase 4 step 5 must document the transitive-block warning',
  )
  // BOS-776 re-points the citation: the cleared-state rule moved out of the repo-specific
  // `scripts/linear-deps-lib.mjs` (never shipped inside a published core) into the vendored
  // `toolbox/plan-deps-lib.mjs`. A name-exact assertion alone is the failure mode the BOS-741 gate
  // below records — it stays green while the surrounding prose stops saying what the citation is
  // FOR — so pin the behaviour the name carries as well.
  assert.ok(
    PHASE_4_SECTION.includes('toolbox/plan-deps-lib.mjs'),
    'the warning must reuse the toolbox/plan-deps-lib.mjs cleared-state rule by name',
  )
  assert.match(
    PHASE_4_SECTION,
    /`DEFAULT_CLEARED_STATE_TYPES`\s+\/\s+`DEFAULT_CANCELED_STATE_TYPES`\s+rule\s+in\s+`toolbox\/plan-deps-lib\.mjs`,\s+the\s+single\s+source/,
    'the cleared definition must cite the library constants as its single source, not restate states in prose',
  )
  assert.match(
    PHASE_4_SECTION,
    /treat\s+a\s+blocker's\s+blocker\s+as\s+\*\*still\s+blocking\*\*\s+unless\s+its\s+state\s+type\s+is\s+cleared\s+or\s+canceled/,
    'the warning must keep the still-blocking-until-cleared rule it exists to apply',
  )
  assert.match(
    PHASE_4_SECTION,
    /Detection\s+only\s+—\s+never\s+auto-prune/,
    'the transitive-block warning must stay detection-only',
  )
})

test('Phase 4 step 5 is I/O glue over the dependency library, not a prose decision (BOS-776)', () => {
  // Every bullet here is a defect that shipped while step 5 decided edges in prose. Assert the
  // BEHAVIOUR, not just the module name: the six fixes are only real if the prose still says what
  // each one requires of the caller. Match against a whitespace-normalized view so a re-wrap of the
  // prose does not read as a removed requirement.
  const flat = PHASE_4_SECTION.replace(/\s+/g, ' ')
  assert.match(
    flat,
    /tracker\s+full-text\s+search\s+is\s+\*\*fuzzy\s+and\s+must\s+never\s+decide\s+overlap\*\*/,
    'step 5 must state that fuzzy tracker search never decides overlap (the ## Key changes section is the oracle)',
  )
  assert.match(
    flat,
    /\*\*explicit\s+field\s+list\*\*:\s+`description,\s+labels,\s+priority,\s+createdAt`\s+plus\s+the\s+adapter's\s+workflow-state\/status\s+fields/,
    'the candidate fetch must name the explicit data fields plus adapter-specific workflow state/status fields — defaults omit them and yield a silent zero-link run',
  )
  assert.match(
    flat,
    /\*\*by\s+id,\s+regardless\s+of\s+state\*\*\s+—\s+`selectPlanned`\s+never\s+returns\s+a\s+cleared\s+ticket/,
    'declared relations must be fetched by id regardless of state, or a cleared prerequisite is never reachable',
  )
  // Both appendRelatedTo branches, written as instructions rather than as an aside.
  assert.match(
    flat,
    /if\s+the\s+adapter\s+does\s+not\s+declare\s+`appendRelatedTo`\s+\(it\s+is\s+optional\),\s+record\s+the\s+relation\s+as\s+a\s+`##\s+Planning`\s+note\s+instead;\s+if\s+a\s+declared\s+one\s+fails,\s+log\s+the\s+reason\s+and\s+continue/,
    'step 5 must write BOTH appendRelatedTo branches (undeclared, and declared-but-failed) as instructions',
  )
  // Cycle safety after the downgrade, scoped to blocking edges. Ordering is the whole point: a
  // 2-cycle check ahead of the downgrade skips the pair and eats the relatedTo edge with it.
  const downgradeAt = PHASE_4_SECTION.indexOf("edge: 'relatedTo'")
  const cycleAt = PHASE_4_SECTION.indexOf('Cycle safety')
  assert.ok(
    downgradeAt > 0 && cycleAt > 0,
    'step 5 must carry both the downgrade branch and cycle safety',
  )
  assert.ok(
    cycleAt > downgradeAt,
    'cycle safety must run AFTER the started-side downgrade, not before it',
  )
  assert.match(
    PHASE_4_SECTION,
    /`relatedTo`\s+is\s+symmetric\s+and\s+non-blocking\s+and\s+cannot\s+form\s+a\s+cycle/,
    'cycle safety must be scoped to blocking writes only',
  )
  // The library is invoked, not paraphrased — and through the CommonJS pathToFileURL spelling the
  // byte budget already paid for at the Phase 0 preflight and the Phase 3 slug one-liner.
  assert.match(
    PHASE_4_SECTION,
    /import\(u\.pathToFileURL\(T\+p\)\.href\)/,
    'step 5 must resolve the toolbox module through require("node:url").pathToFileURL(...).href',
  )
  assert.match(
    PHASE_4_SECTION,
    /planDependencyEdges/,
    'step 5 must call the library rather than re-deriving the ladder in prose',
  )
  // The payload's own workflow state/status shape. The subject is blocked by inbound edges and
  // blocks outbound ones, so missing state resolves to the unknown role on BOTH sides of the
  // ladder's last rung and downgrades the whole set to `relatedTo`.
  assert.match(
    flat,
    /`subject`\s+needs\s+the\s+SAME\s+fields\s+as\s+a\s+candidate,\s+including\s+workflow\s+state\/status/,
    'step 5 must require the subject payload to carry its own state/status shape, or every edge downgrades',
  )
  // The prefilter is a context-scale measure. Read as an overlap filter it re-introduces the
  // missed-prerequisite defect one layer above the oracle.
  assert.match(
    flat,
    /prefilter\s+is\s+a\s+\*\*context-scale\s+measure\s+only,\s+never\s+an\s+overlap\s+decision\*\*/,
    'the title+label prefilter must be bounded, or it silently decides overlap ahead of the oracle',
  )
  // The run's only post-dependency save. Gated on relations alone it discards every zero-relation
  // outcome the library went to the trouble of raising.
  assert.match(
    flat,
    /Record\s+what\s+step\s+5\s+found\s+—\s+\*\*whenever\s+\(d\)\s+produced\s+≥1\s+relation,\s+note,\s+or\s+question\*\*/,
    'the recording save must fire on a note or a question too, not on relations alone',
  )
  assert.match(
    flat,
    /union\s+it\s+into\s+the\s+set\s+Step\s+4\s+saved,\s+because\s+`labels`\s+\*\*replaces\*\*\s+the\s+whole\s+set/,
    'the agent-question write must state the union rule — a bare labels write clobbers Step 4 labels',
  )
})

test('Phase 4 permits required secret redaction without weakening attachment parity (BOS-702)', () => {
  assert.match(
    PHASE_4_SECTION,
    /redact\s+it\s+in[\s\S]{0,20}every\s+persisted\s+artifact/i,
    'the secret gate must cover both persisted artifacts, not only the attachment',
  )
  assert.match(
    PHASE_4_SECTION,
    /attachment-guard-orig\.md[\s\S]{0,280}only.*mandatory\s+secret\/PII\s+redactions/i,
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

// BOS-1199 — the post-save read-back. Every other gate is pre-write prevention over local bytes,
// so nothing observed what actually landed until this one. The three assertions below are the
// three prose changes the ticket lands; each is pinned so removing it fails.
test('BOS-1199: Phase 2 states the snapshot is the STORED description with no added byte', () => {
  for (const payload of PAYLOAD_COPIES) {
    const headless = sectionBetween(
      payload.skill,
      '### Headless (`BOSS_CRON=true`) — dispatch ONE awaited drafting subagent',
      '\n## Phase 2.5',
    )
    assert.match(
      headless,
      /tracker's\s+\*\*stored\*\*\s+description,\s+not\s+a\s+rendering\s+of\s+it/,
      `${payload.name}: Phase 2 must state the snapshot's provenance`,
    )
    assert.match(
      headless,
      /no\s+byte\s+the\s+stored\s+description\s+does\s+not\*\*[\s\S]{0,120}no\s+trailing\s+newline/,
      `${payload.name}: Phase 2 must state the snapshot's byte shape`,
    )
  }
  assert.match(
    BRIEF,
    /tracker's\s+\*\*stored\*\*\s+description,\s+not\s+a\s+rendering\s+of\s+it/,
    'the headless drafting brief must restate the snapshot provenance rule',
  )
  assert.match(
    INTERACTIVE,
    /tracker's\s+\*\*stored\*\*\s+description\s+bytes,\s+not\s+a\s+rendering\s+of\s+them/,
    'the interactive reference must restate the snapshot provenance rule',
  )
})

test('BOS-1199: Phase 4 runs the write-back verification once, after the final save', () => {
  for (const payload of PAYLOAD_COPIES) {
    const phase4 = sectionBetween(payload.skill, '## Phase 4 —', '\n## Phase 5')
    assert.match(
      phase4,
      /STOP\s+—\s+write-back\s+verification\s+\(mandatory,\s+mechanical,\s+do\s+not\s+skip\)/,
      `${payload.name}: the read-back must be a mandatory mechanical STOP`,
    )
    assert.match(
      phase4,
      /plan-writeback-verify\.mjs" --intended "\$WB_FINAL" --stored "\$WB_STORED"/,
      `${payload.name}: the read-back invocation must name both inputs`,
    )
    assert.match(
      phase4,
      /\*\*exactly\s+once\*\*[\s\S]{0,200}final\*\*\s+description\s+save/,
      `${payload.name}: the read-back must run once, after the FINAL description save`,
    )
    // KTD5: loud but non-destructive. A corrective rewrite on a drift verdict is an unattended
    // agent overwriting a description it just proved it cannot reproduce.
    assert.match(
      phase4,
      /`drift`\s+verdict[\s\S]{0,700}\*\*retain\*\*\s+the\s+scratch[\s\S]{0,300}not\*\*\s+attempt\s+a\s+corrective\s+rewrite/,
      `${payload.name}: the drift branch must retain scratch and forbid a corrective rewrite`,
    )
    assert.equal(
      count(phase4, 'plan-writeback-verify.mjs'),
      1,
      `${payload.name}: exactly one read-back invocation`,
    )
  }
})

test('BOS-1199: the patch-anchor guidance names the stored normalized text', () => {
  for (const payload of PAYLOAD_COPIES) {
    const phase4 = sectionBetween(payload.skill, '## Phase 4 —', '\n## Phase 5')
    assert.match(
      phase4,
      /copy\s+every\s+anchor\s+from\s+the\s+tracker's\s+\*\*stored,\s+normalized\*\*\s+text/,
      `${payload.name}: an anchor must come from the stored text`,
    )
    assert.match(
      phase4,
      /never\s+from\s+the\s+gated\s+local\s+draft/,
      `${payload.name}: the anchor guidance must reject the local draft`,
    )
  }
})

test('Phase 4 carries a mandatory plan-contract STOP gate before the tracker write (BOS-741)', () => {
  // Assert on BEHAVIOURAL WORDING, not just the helper name: a name-exact ratchet stays green while
  // the surrounding prose has stopped saying the gate is mandatory or that failure means no write.
  assert.match(
    PHASE_4_SECTION,
    /STOP — plan-contract\s+gate \(mandatory, mechanical, do\s+not\s+skip\)/,
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
    /SAFE\s+branch[\s\S]{0,200}no\s+Linear\s+write/,
    'the contract gate must name the SAFE branch and that it performs no tracker write',
  )
  assert.match(gateBlock, /exit\s+non-zero/, 'the SAFE branch must exit non-zero')
  assert.match(
    gateBlock,
    /zero\*\* extra\s+tracker[\s>]+reads/,
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
    /config-first\*\* order;[\s\S]{0,140}named\s+argument-order\s+error/,
    'the resident body must say the swapped call throws a named argument-order error',
  )
})

test('epic reverify decodes spec attachments and rejects missing childIds distinctly (BOS-755)', () => {
  for (const copy of PAYLOAD_COPIES) {
    assert.ok(
      copy.skill.includes(
        'node "$BOSS_PLAN_TOOLBOX/plan-attachment.mjs" decode <in-file> <out-file>',
      ),
      `${copy.name} must name the plan-attachment decode verb before parseEpicSpec`,
    )
    assert.ok(
      copy.skill.includes('missing/empty childIds is a sentinel-shape failure'),
      `${copy.name} must reject a sentinel that omits childIds`,
    )
    assert.ok(
      copy.skill.includes('rejected separately from a child-reconciliation miss'),
      `${copy.name} must name sentinel and child-reconciliation failures separately`,
    )
  }
})

test('epic wiring prose names the reserved parent id entry (BOS-755)', () => {
  assert.ok(
    SKILL.includes('created-id map passed to') &&
      SKILL.includes('must include the reserved `parent` entry beside every child id'),
    'Phase 2.5 step 4 must name the reserved parent entry expected by epicWiringPlan',
  )
})

test('epic parent validation mode is documented (BOS-755)', () => {
  assert.ok(
    SKILL.includes("validatePlanDescription(config, description, {mode:'epic-parent'})"),
    'boss-plan must document the explicit epic-parent validatePlanDescription mode',
  )
  assert.ok(
    SKILL.includes('with `## Summary`, `## Child tickets`, `## Planning`, and `## Original notes`'),
    'boss-plan must list the epic-parent overview section set',
  )
  assert.ok(
    SKILL.includes('Unknown modes warn and fall back to child-plan'),
    'boss-plan must document unknown validation mode fallback',
  )
})

test('epic parent overview runs the contract guard in epic-parent mode before parent save', () => {
  for (const payload of PAYLOAD_COPIES) {
    assert.match(
      payload.skill,
      /plan-contract\s+gate[\s\S]{0,220}plan-contract-guard\.mjs\s+--mode\s+epic-parent[\s\S]{0,220}before\s+attachment\s+finalize\s+or\s+parent\s+save/i,
      `${payload.name}: resident epic path must gate the parent overview with --mode epic-parent before parent writes`,
    )
    assert.match(
      payload.skill,
      /plan-contract-guard\.mjs\s+--mode\s+epic-parent[\s\S]{0,180}on\s+a\s+violation\s+take\s+the\s+same\s+SAFE\s+branch/i,
      `${payload.name}: resident epic gate must share the parent SAFE branch`,
    )
  }
  assert.match(
    BRIEF,
    /plan-contract-guard\.mjs\s+--mode\s+epic-parent[\s\S]{0,180}on\s+a\s+violation,\s+no\s+parent\s+write,\s+abort/i,
    'headless drafting brief must require the epic-parent contract gate before parent writes',
  )
  assert.match(
    BRIEF,
    /plan-contract-guard\.mjs\s+--mode\s+epic-parent[\s\S]{0,160}before\s+attachment\s+finalize\s+or\s+parent\s+save/i,
    'headless drafting brief must name the exact epic-parent guard mode',
  )
})

test('epic sentinel childIds are required in the drafting brief (BOS-755)', () => {
  for (const copy of PAYLOAD_COPIES) {
    assert.ok(
      copy.brief.includes('childIds:     ["<ISSUE-ID>", ...]    // REQUIRED'),
      `${copy.name} drafting brief must declare childIds required`,
    )
  }
})

test('the drafting brief runs the contract guard before the ok sentinel (BOS-741)', () => {
  assert.match(
    BRIEF,
    /plan-contract-guard\.mjs" --description "\$NEW" --plan "\$PLAN_PATH"/,
    'the brief must run the contract guard over the description and the plan file',
  )
  assert.match(
    BRIEF,
    /non-zero\s+exit\s+means\s+write\s+no `ok` sentinel/,
    'a contract violation must block the ok sentinel, not merely be reported',
  )
  const guardAt = BRIEF.indexOf('plan-contract-guard.mjs')
  assert.ok(guardAt > 0 && guardAt < BRIEF.indexOf('## Step 9'), 'the guard runs inside Step 8')
})

test('BOS-769: Phase 1 carries the idempotence precheck and clean no-op', () => {
  for (const payload of PAYLOAD_COPIES) {
    assert.match(
      payload.skill,
      /planIdempotencePrecheck\(\.\.\.\)[\s\S]{0,120}plan-run-guards\.mjs/,
      `${payload.name}: Phase 1 must cite the idempotence guard export`,
    )
    assert.match(
      payload.skill,
      /plan-run-guards\.mjs" idempotence "\$PRECHECK"/,
      `${payload.name}: Phase 1 must run the idempotence CLI`,
    )
    assert.match(
      payload.skill,
      /exit\s+\*\*0\*\*[\s\S]{0,80}\*\*zero\s+tracker\s+writes\*\*/,
      `${payload.name}: idempotence noop must be a clean zero-write exit`,
    )
  }
})

test('BOS-769: headless Phase 2 snapshots the description before dispatch', () => {
  for (const payload of PAYLOAD_COPIES) {
    const headless = sectionBetween(
      payload.skill,
      '### Headless (`BOSS_CRON=true`) — dispatch ONE awaited drafting subagent',
      '\n## Phase 2.5',
    )
    assert.match(
      headless,
      /\.linear-plans\/run-<RUN-SCRATCH-ID>\/<ISSUE-ID>\.image-guard-orig\.md[\s\S]{0,240}single\s+raw-description\s+snapshot/,
      `${payload.name}: Phase 2 must write the raw description snapshot before dispatch`,
    )
    assert.match(
      headless,
      /description\s+snapshot\s+path[\s\S]{0,120}PLAN_PATH/,
      `${payload.name}: dispatch input must pass the snapshot path to the worker`,
    )
  }
  assert.match(
    BRIEF,
    /DESCRIPTION_SNAPSHOT_PATH[\s\S]{0,240}Build\s+`## Original\s+notes`\s+from\s+this\s+file\s+only/,
    'brief must name the snapshot file as the sole description source',
  )
})

test('BOS-769: headless Phase 2 validates bounded metadata before Phase 3.5', () => {
  for (const payload of PAYLOAD_COPIES) {
    const headless = sectionBetween(
      payload.skill,
      '### Headless (`BOSS_CRON=true`) — dispatch ONE awaited drafting subagent',
      '\n## Phase 2.5',
    )
    const metadataAt = headless.indexOf('plan-run-guards.mjs" metadata "$METADATA"')
    const phase35At = payload.skill.indexOf('## Phase 3.5')
    assert.ok(metadataAt > 0, `${payload.name}: metadata guard must be present`)
    assert.ok(
      payload.skill.indexOf('plan-run-guards.mjs" metadata "$METADATA"') < phase35At,
      `${payload.name}: metadata guard must precede Phase 3.5`,
    )
    assert.match(
      headless,
      new RegExp(`\\$DISPATCH_FAILURE: draft metadata failed plan-run-guards\\.mjs metadata`),
      `${payload.name}: metadata failure must route through DISPATCH_FAILURE`,
    )
  }
})

test('BOS-769: premises ride the sentinel and Phase 4 re-verifies them', () => {
  for (const payload of PAYLOAD_COPIES) {
    assert.match(
      payload.skill,
      /PREMISES="\$\(printf '%s' "\$READ" \| jq -c '\.payload\.premises \/\/ \[\]'\)"/,
      `${payload.name}: Phase 2 must capture premises from the sentinel payload`,
    )
    assert.match(
      payload.skill,
      /STOP\s+—\s+premise\s+re-verification/,
      `${payload.name}: Phase 4 must run the premises guard`,
    )
    assert.match(
      payload.skill,
      /plan-run-guards\.mjs" premises "\$PREMISES_FILE" "\$LIVE_STATES_FILE"/,
      `${payload.name}: Phase 4 must invoke the premises CLI`,
    )
    assert.match(
      payload.skill,
      /PREMISE_REPORT="\$\(node[\s\S]{0,220}PREMISE_RC=\$\?/,
      `${payload.name}: Phase 4 must capture premise drift output without conflating it with an abort`,
    )
    assert.match(
      payload.skill,
      /-\s+Premise\s+drift:\s+<ticket>\s+was\s+<state\s+at\s+recon>,\s+is\s+now\s+<current\s+state>/,
      `${payload.name}: Phase 4 must document the drift annotation line`,
    )
  }
  assert.ok(BRIEF.includes('Include `premises` in the sentinel payload'))
  assert.ok(BRIEF.includes('It rides the run-file sentinel'))
})

test('BOS-769: headless drafting defers resolution to the shared Fallback contract', () => {
  assert.match(
    SKILL,
    /Draft-resolution\s+\(shared\s+Fallback\s+contract\)[\s\S]{0,160}Resolve\s+drafting\s+by\s+the\s+Fallback\s+contract/,
    'resident skill must keep the single draft-resolution source of truth',
  )
  assert.match(
    HEADLESS_SECTION,
    /headless-specific\s+mechanics|sentinel\s+context|description\s+snapshot\s+path/i,
    'headless section should describe mechanics rather than restating a competing concrete resolver',
  )
})

test('the drafting brief pins the cased ## Open Questions heading and plan-document-only rule (BOS-741)', () => {
  assert.match(
    BRIEF,
    /\*\*`## Open\s+Questions`\*\*[\s\S]{0,160}exact\s+casing, capital `Q`/,
    'the brief must state the exact cased heading so plan file and description agree',
  )
  assert.match(
    BRIEF,
    /## Open\s+questions/,
    'the brief must name the drifted lower-case spelling it is correcting',
  )
  assert.match(
    BRIEF,
    /plan\s+file\s+must\s+contain\s+one\s+plan\s+document/,
    'the brief must require the plan file to carry one plan document',
  )
  assert.match(
    BRIEF,
    /No\s+tool-call\s+scaffolding[\s\S]{0,120}transcript\s+residue/,
    'the plan-document-only rule must name the residue it excludes',
  )
})

test('Phase 4 counts canonical upload identities for the image guard (BOS-702)', () => {
  assert.match(
    PHASE_4_SECTION,
    /distinct\s+canonical\s+upload\s+identities[\s\S]{0,140}origin\s+plus\s+pathname, ignoring\s+query\s+strings/i,
    'EXPECTED_IMAGES must use the same signed-URL-normalizing identity as the guard',
  )
  assert.ok(
    PHASE_4_SECTION.includes(
      'EXPECTED_IMAGES="<distinct canonical upload identities observed in Phase 1>"',
    ),
    'the command placeholder must not ask callers to count raw upload URLs',
  )
})

test('Phase 4 deletes reporter-source scratch before every failed-gate exit (BOS-702)', () => {
  // BOS-1189 narrowed the deleted set from four paths to three. The security property is
  // unchanged and is the whole point of this test: `$ORIG` is the raw Phase 1 reporter source
  // and may carry sensitive content, `$SAFE_ORIG`/`$NEW` are derived from it, and Phase 5 never
  // runs after `exit 1` — so EVERY failing gate in Phase 4 must destroy all three itself.
  // `$PLAN_FILE` is deliberately no longer deleted: it is the run's most expensive artifact and
  // has already passed the secret gate upstream of this block, so retaining it turns a one-line
  // formatting fault into an EDIT instead of a full redraft.
  assert.match(
    PHASE_4_SECTION,
    /cleanup_guard_scratch\(\)[\s\S]{0,240}rm -f "\$ORIG" "\$SAFE_ORIG" "\$NEW"/,
    'the failed-gate cleanup must remove the raw, safe, and rewritten source scratch files',
  )
  assert.equal(
    (PHASE_4_SECTION.match(/cleanup_guard_scratch\n>   exit\s+1/g) ?? []).length,
    4,
    'each of the four image-guard failures must clean scratch before exiting',
  )
  // Counting one helper name cannot see a failed-gate exit that cleans up some OTHER way — which is
  // how the BOS-741 contract gate shipped leaking `$ORIG`/`$SAFE_ORIG`. Assert the property instead:
  // EVERY `exit 1` in Phase 4 must be preceded by a cleanup naming all three source scratch paths.
  const exits = PHASE_4_SECTION.split(/^>\s+exit\s+1$/m)
  assert.ok(exits.length - 1 >= 5, 'Phase 4 must carry the image gates plus the contract gate')
  for (const [i, before] of exits.slice(0, -1).entries()) {
    const tail = before.slice(-400)
    assert.ok(
      /cleanup_guard_scratch$/m.test(tail) || /rm -f "\$ORIG" "\$SAFE_ORIG" "\$NEW"/.test(tail),
      `failed-gate exit #${i + 1} must delete all three source scratch paths — $ORIG may carry sensitive content and Phase 5 never runs after exit 1`,
    )
  }
  // The other half of the BOS-1189 contract, and the half a positive match cannot see: no
  // failing-gate cleanup may delete the drafted plan. Without this, re-adding `"$PLAN_FILE"` to
  // either `rm -f` leaves every assertion above green while restoring the full-redraft cost.
  const removals = PHASE_4_SECTION.split('\n')
    .map((line) => line.replace(/^>\s?/, ''))
    .filter((line) => /^rm -(?:f|rf) /.test(line.trim()))
  assert.ok(removals.length >= 2, 'Phase 4 must carry the helper and contract-gate removals')
  for (const removal of removals) {
    assert.ok(
      !removal.includes('"$PLAN_FILE"'),
      `no failing gate may delete the drafted plan — the secret gate has already cleared it and the next attempt edits it in place: ${removal.trim()}`,
    )
  }
})

test('plan storage supersedes stale duplicate attachments only after verified read-back (BOS-773)', () => {
  const flat = PLAN_STORAGE.replace(/\s+/g, ' ')
  assert.match(
    flat,
    /After\s+the\s+read-back\s+succeeds,\s+take\s+a\s+\*\*single\s+fresh\*\*\s+attachment\s+list/,
    'supersede must use one fresh post-read-back attachment list',
  )
  assert.match(
    flat,
    /selectSupersededPlanAttachments[\s\S]{0,160}freshly\s+finalized\s+id\s+as\s+`keepAttachmentId`/,
    'plan storage must call the exact-title supersede predicate with the retained id',
  )
  assert.match(
    flat,
    /A\s+failed\s+supersede\s+list\s+takes\s+the\s+SAFE\s+branch[\s\S]{0,160}does\s+not\s+roll\s+back\s+the\s+successful\s+publish/,
    'supersede failures must not destroy or roll back the freshly verified artifact',
  )
  assert.match(
    flat,
    /Retry\s+each\s+failed\s+`deletePlanAttachment`\s+once/,
    'failed duplicate deletes must retry once',
  )
  assert.match(
    flat,
    /surviving\s+duplicate\s+attachment\s+id/,
    'a delete that still fails must be reported as a surviving duplicate',
  )
  assert.match(
    flat,
    /deleted\s+attachment\s+id\s+and\s+exact\s+title/,
    'each supersede deletion must be logged with id and exact title',
  )
})

test('Phase 4 asserts exactly one canonical plan attachment remains after publish (BOS-773)', () => {
  const flat = PHASE_4_SECTION.replace(/\s+/g, ' ')
  assert.match(
    flat,
    /After\s+`references\/plan-storage\.md`\s+returns\s+success[\s\S]{0,220}exactly\s+one\s+`Implementation\s+plan\s+\(<ISSUE-ID>\)`\s+attachment\s+remains/,
    'Phase 4 must assert canonical plan attachment cardinality after storage success',
  )
  assert.match(
    flat,
    /more\s+than\s+one\s+exact-title\s+attachment\s+takes\s+the\s+SAFE\s+branch/,
    'duplicate canonical plan attachments must block metadata/state writeback',
  )
  assert.match(
    flat,
    /no\s+plan\s+metadata\/state\s+write/,
    'the duplicate branch must preserve the existing no-write safety contract',
  )
})

test('the headless drafting brief forbids tracker writes on the non-EPIC path (BOS-773)', () => {
  assert.match(
    BRIEF,
    /non-EPIC\s+path[\s\S]{0,140}zero\s+tracker\s+writes/i,
    'the drafting worker must be explicitly read-only for tracker writes on single-ticket plans',
  )
  assert.match(
    BRIEF,
    /Only\s+the\s+EPIC\s+path\s+may\s+perform\s+tracker\s+writes/i,
    'the exception must be limited to the existing epic decomposition path',
  )
})

// ---------------------------------------------------------------------------
// Bulk-output discipline — no raw bulk in the orchestrator.
// ---------------------------------------------------------------------------

test('the SKILL carries the bulk-output-discipline block', () => {
  assert.match(
    SKILL,
    /Bulk-output\s+discipline/i,
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

test('the description contract does not flatten the plan attachment (BOS-1176)', () => {
  for (const copy of PAYLOAD_COPIES) {
    for (const [name, body] of [
      ['resident skill', copy.skill],
      ['headless drafting brief', copy.brief],
    ]) {
      const label = `${copy.name} ${name}`
      assert.match(
        body,
        /description(?:-section)?\s+contract\s+governs\s+`descriptionSummary`\s+only/i,
        `${label} must scope the description contract to descriptionSummary`,
      )
      assert.match(
        body,
        /plan\s+(?:file|attachment)\s+may\s+retain\s+additional\s+drafting-layer\s+headings\s+and\s+(?:its\s+)?native\s+structure/i,
        `${label} must preserve plan-native drafting structure`,
      )
      assert.match(
        body,
        /`##\s+Original\s+notes`\s+(?:must\s+)?remain(?:s)?\s+the\s+terminal\s+heading[\s\S]{0,120}body\s+(?:continues|runs)\s+to\s+EOF/i,
        `${label} must make Original notes terminal through EOF`,
      )
      assert.match(
        body,
        /every\s+query-bearing\s+upload\s+URL\s+anywhere\s+in\s+the\s+plan\s+file\s+must\s+be\s+query-stripped/i,
        `${label} must strip upload queries across the whole plan file`,
      )
    }

    for (const forbidden of [
      /normalize\s+the\s+(?:plan|result)\s+to\s+(?:the\s+)?planContract\s+sections/i,
      /plan\s+file\s+must\s+contain\s+ONLY\s+the\s+plan\s+body/i,
      /Any\s+other\s+heading\s+is\s+off-contract/i,
    ]) {
      assert.doesNotMatch(
        `${copy.skill}\n${copy.brief}`,
        forbidden,
        `${copy.name} must not imply the plan file is limited to description-contract sections`,
      )
    }
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
    /helper\s+is\s+missing[\s\S]*"extensions":\[\][\s\S]*portable\s+fallback\s+tiers\s+still\s+run/,
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
    /load\s+each\s+discovered\s+extension\s+by\s+its\s+returned\s+descriptor\s+`name`\s+via\s+the\s+Skill\s+tool/i,
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
    /draft\s+success\s+predicate/i,
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
    /valid\s+envelope\s+that\s+wrote\s+no\s+plan/i,
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
    /including\s+when\s+a\s+sibling\s+succeeded/i,
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
    /fall\s+through\s+to\s+Tier\s+2, then\s+Tier\s+3/,
    'interactive-mode.md must keep the Tier-2/Tier-3 fall-through',
  )
  assert.doesNotMatch(
    INTERACTIVE,
    /If\s+at\s+least\s+one\s+extension\s+ran\s+successfully/i,
    'the suppression gate must not use the undefined "ran successfully" criterion',
  )
  assert.doesNotMatch(
    INTERACTIVE,
    /if\s+no\s+extension\s+ran\s+successfully/i,
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
      /Remove\s+it\s+with `rm -rf`[^.]*promoted\s+the\s+winning\s+plan[^.]*failure\s+paths\s+too/i,
      'the headless brief must remove the run scratch it created, on the failure paths too',
    )
  }

  // The headless brief's Step 5 is the *shared* drafting spec that interactive-mode.md points at
  // for Tier 3, and it is the whole draft resolution on the headless path. It carried both defects
  // verbatim, so fixing only interactive-mode.md would leave the shared spec contradicting it.
  assert.match(
    BRIEF,
    /draft\s+success\s+predicate/i,
    'headless-drafting-brief.md Step 5 must carry the same draft success predicate',
  )
  assert.match(
    BRIEF,
    /valid[\s\S]{0,160}\bAND\b[\s\S]{0,200}non-empty[\s\S]{0,80}`PLAN_PATH`/,
    'the shared drafting spec must require BOTH a valid result and a non-empty plan at `PLAN_PATH`',
  )
  assert.match(
    BRIEF,
    /including\s+when\s+a\s+sibling\s+succeeded/i,
    'the shared drafting spec must record a failed dispatch even when a sibling succeeds',
  )
  assert.ok(
    count(BRIEF, 'succeeded under the draft success predicate') >= 3,
    'the shared drafting spec must gate Tier-1 suppression and both Tier-2/Tier-3 entries on the same predicate',
  )
  assert.doesNotMatch(
    `${BRIEF}\n${SKILL}`,
    /(?:If\s+at\s+least\s+one|if\s+no) extension\s+ran\s+successfully/i,
    'no boss-plan draft site may keep the undefined "ran successfully" criterion',
  )

  // The always-resident Fallback contract is in context before any reference is loaded, so it must
  // not state a looser definition of "succeeded" than the references do.
  assert.match(
    SKILL,
    /succeeded\*\* only\s+when\s+its\s+result\s+is\s+valid \*\*AND\*\* the\s+requested\s+non-empty\s+plan\s+exists/i,
    'SKILL.md must define a Tier-1 draft success as a valid result AND the produced plan',
  )
  assert.match(
    SKILL,
    /for\s+every\s+failed\s+dispatch, including\s+when\s+a\s+sibling\s+succeeded/i,
    'SKILL.md must record every failed draft dispatch, not only the all-failed case',
  )
  assert.doesNotMatch(
    SKILL,
    /If\s+every\s+discovered\s+extension\s+failed,\s+record/i,
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
      /written\s+by (\*\*)?th(is|at)(\*\*)? dispatch/i,
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
      /is\s+a\s+test\s+of\s+shared\s+state, not\s+of\s+this\s+extension/i,
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
      /Per-dispatch\s+plan\s+target/i,
      `${name} must hand each draft dispatch its own plan target`,
    )
    assert.match(
      flat,
      /unique\s+to\s+the\s+dispatch\s+you\s+are\s+about\s+to\s+classify/i,
      `${name} must state that the per-dispatch target is unique to the dispatch being classified`,
    )
    assert.match(
      flat,
      /copy\s+the\s+file\s+produced\s+by\s+the \*\*first\*\* dispatch\s+that\s+succeeded\s+under\s+the\s+predicate\s+below/i,
      `${name} must promote the winning per-dispatch plan onto the real plan target`,
    )
    assert.doesNotMatch(
      flat,
      /modification\s+time\s+to\s+have\s+moved/i,
      `${name} must not use timestamp inequality as proof that this dispatch wrote the plan`,
    )
    assert.match(
      flat,
      /modification\s+time\s+need\s+not\s+advance/i,
      `${name} must say why an mtime comparison cannot carry the attribution`,
    )
    assert.match(
      flat,
      /timestamp\s*resolution\s+is\s+coarser\s+than\s+the\s+rewrite/i,
      `${name} must name the coarse-timestamp filesystem that defeats an mtime comparison`,
    )
    assert.match(
      flat,
      /Identical\s+bytes\s+are\s+the\s+ordinary\s+output\s+of\s+a\s+deterministic\s+redraft/i,
      `${name} must say why a byte comparison cannot carry the attribution either`,
    )
  }
  assert.match(
    SKILL.replace(/\s+/g, ' '),
    /per-dispatch\s+plan\s+path\s+that\s+dispatch\s+alone\s+was\s+given[\s\S]{0,120}never\s+at\s+a\s+path\s+a\s+peer\s+could\s+have\s+written/i,
    'the always-resident Fallback contract must not state a looser attribution rule than the references',
  )
})

test('the resident body states the draft Fallback contract', () => {
  assert.match(SKILL, /Fallback\s+contract/, 'SKILL.md must name the Fallback contract')
  assert.match(
    SKILL,
    /extension.*host\s+built-in.*inline\s+prompt/is,
    'SKILL.md must state the extension -> host built-in -> inline prompt order',
  )
  // BOS-663: suppression keys on a Tier-1 dispatch SUCCEEDING, not on an extension merely being
  // present. Gating on presence made a failed dispatch silently drop the whole drafting layer.
  assert.match(
    SKILL,
    /tiers\s+2\/3\s+suppressed\s+only\s+when\s+a\s+Tier-1\s+dispatch\s+\*\*succeeded\*\*/i,
    'SKILL.md must state that lower fallback tiers are suppressed only when a Tier-1 dispatch succeeded',
  )
  assert.match(
    SKILL,
    /fall\s+through\s+to\s+tier\s+2, then\s+tier\s+3/i,
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
  const { interactive, headless } = draftBehaviorRegions(DRAFT)
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
    /runs `ce-doc-review` in\s+headless\s+mode\s+itself/i,
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
    /do\s+not\s+shortcut\s+its\s+menus/i,
    'the interactive path must not hand CE’s post-generation menu blanket authority',
  )
  assert.match(
    interactive,
    /never\s+a\s+CE\s+menu\s+action/i,
    'the interactive path must name CE’s plan file — not a menu action — as the deliverable',
  )
  assert.match(
    interactive.replace(/\s+/g, ' '),
    /decline\s+every\s+menu\s+branch\s+that\s+leaves\s+the\s+plan\s+file/i,
    'the interactive path must decline the menu branches that leave the plan file',
  )
  assert.match(
    interactive.replace(/\s+/g, ' '),
    /the\s+core\s+owns\s+the\s+tracker\s+write/i,
    'the interactive path must say why: the core, not CE, owns the tracker write',
  )
  assert.match(
    interactive.replace(/\s+/g, ' '),
    /already\s+been\s+created[\s\S]{0,60}before\s+you\s+could\s+decline[\s\S]{0,400}failure\s+envelope/i,
    'an already-fired tracker-issue branch must route to the failure envelope, not be papered over',
  )
})

test('BOS-1176: CE behavior prose pins stop at the next heading and fail on missing landmarks', () => {
  const decoyHeading = '## Decoy before normalize'
  const withDecoy = DRAFT.replace(
    '\n## Normalize and promote',
    `\n${decoyHeading}\n\n## Normalize and promote`,
  )
  assert.notEqual(withDecoy, DRAFT, 'fixture must insert the decoy before Normalize and promote')

  const legacyHeadless = sectionBetween(withDecoy, '### Headless', '## Normalize and promote')
  assert.match(legacyHeadless, new RegExp(`^${decoyHeading}$`, 'm'))

  const { headless } = draftBehaviorRegions(withDecoy)
  assert.doesNotMatch(
    headless,
    new RegExp(`^${decoyHeading}$`, 'm'),
    'the headless prose pin must exclude an intervening H2 instead of silently widening',
  )

  const withoutNormalize = DRAFT.replace('\n## Normalize and promote', '')
  assert.notEqual(
    withoutNormalize,
    DRAFT,
    'fixture must delete the Normalize and promote delimiter',
  )
  assert.throws(
    () => draftBehaviorRegions(withoutNormalize),
    /later\s+marker\s+not\s+found:\s+"##\s+Normalize\s+and\s+promote"/,
    'a deleted named delimiter must fail with the missing marker',
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
  assert.match(BRIEF, /## Acceptance\s+criteria/, 'the brief carries the plan-body requirements')
  assert.match(
    BRIEF,
    /## Original\s+notes/,
    'the brief carries the fill-in description-summary template',
  )
  assert.match(
    INTERACTIVE,
    /references\/headless-drafting-brief\.md` \*\*Step\s+5\*\* and\s+\*\*Step\s+7\*\*/,
    'interactive drafting must point at the shared Step 5/Step 7 sections that actually contain the moved template',
  )
  assert.doesNotMatch(
    BRIEF,
    /^- Dependencies:/m,
    'the subagent-returned template must not include the Dependencies line the orchestrator owns',
  )
})

test('BOS-926: sibling-class enumeration and file-count cap rules are pinned in both drafting payloads', () => {
  for (const payload of PAYLOAD_COPIES) {
    for (const [label, body] of [
      ['SKILL.md', payload.skill],
      ['headless-drafting-brief.md', payload.brief],
    ]) {
      const where = `${payload.name} ${label}`
      assert.match(
        body,
        /specific\s+call\s+site,\s+construct,\s+literal\s+claim,\s+or\s+other\s+mechanism[\s\S]{0,220}repo-wide\s+sibling-class\s+enumeration/i,
        `${where}: must require enumeration when a named mechanism could recur`,
      )
      assert.match(
        body,
        /every\s+site\s+the\s+search\s+returns[\s\S]{0,180}verdict\s+\(`fix`\s+or\s+`not\s+a\s+defect`\)[\s\S]{0,120}reason/i,
        `${where}: must require a per-site verdict with a reason`,
      )
      assert.match(
        body,
        /adjudicate\s+the\s+class\s+per\s+site\s+rather\s+than\s+sweeping\s+every\s+match\s+wholesale/i,
        `${where}: must forbid sweeping the whole class`,
      )
      assert.match(
        body,
        /discriminator[\s\S]{0,120}where\s+the\s+branch\s+actually\s+lives/i,
        `${where}: must name the branch-location discriminator example`,
      )
      assert.match(
        body,
        /(?:acceptance\s+criterion[\s\S]{0,100}must\s+not[\s\S]{0,100}cap\s+the\s+number\s+of\s+changed\s+files|Do\s+not[\s\S]{0,100}acceptance\s+criterion[\s\S]{0,100}caps\s+the\s+number\s+of\s+changed\s+files)/i,
        `${where}: must forbid changed-file-count acceptance criteria`,
      )
    }
  }
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
    /future\s+proof\s+type/i,
    'the brief must not describe video as a "future" proof type — it is captured today',
  )
  assert.match(
    BRIEF,
    /stills\s+and\s+video/i,
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
    /proof\.mjs\s+plan/,
    'the readiness pass must use the same `proof.mjs plan` gate the implementer uses',
  )
  assert.match(
    BRIEF,
    /map\s+each\s+acceptance\s+criterion\s+to\s+a\s+concrete\s+proof\s+artifact/i,
    'the readiness pass must map each acceptance criterion to a concrete proof artifact',
  )
  assert.match(
    BRIEF,
    /missing\s+but\s+buildable[\s\S]{0,200}IN-PR\s+work/i,
    'the readiness pass must schedule buildable-but-missing affordances as in-PR work',
  )
  assert.match(
    BRIEF,
    /never\s+call `AskUserQuestion`/,
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

test('the brief Step 7 byte-copy recipe does not strip trailing newlines', () => {
  assert.match(
    BRIEF,
    /Return\s+the\s+contents\s+of `"\$BODY"` as `descriptionSummary`/,
    'the brief must tell the drafter to return the assembled file bytes',
  )
  assert.match(
    BRIEF,
    /command\s+substitution\s+strips\s+trailing\s+newline\s+bytes/,
    'the brief must explain why command substitution is unsafe for the byte contract',
  )
  assert.doesNotMatch(
    BRIEF,
    /^DESCRIPTION_SUMMARY="\$\(cat "\$BODY"\)"$/m,
    'the executable recipe must not assign descriptionSummary through command substitution',
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
  assert.equal(
    MIRROR_BRIEF,
    BRIEF,
    'the plugin mirror brief must be byte-identical to the canonical brief (run make copy-skills and stage it)',
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
    /multiple\s+independently-shippable\s+PRs/,
    'the phase must define EPIC as multi-PR work',
  )
  assert.match(
    EPIC_PHASE,
    /≥ 2\*\*\s+genuinely\s+separable/,
    'the phase must require >=2 separable children',
  )
  // The estimate-as-forcing-function trigger: honest >=5 auto-triages EPIC.
  assert.match(
    EPIC_PHASE,
    /honest\s+estimate\s+is \*\*≥ 5\*\*/,
    'the phase must make an honest >=5 estimate auto-trigger EPIC',
  )
  assert.match(
    EPIC_PHASE,
    /Estimate\s+is\s+the\s+forcing\s+function/,
    'the phase must name estimate as the forcing function',
  )
  assert.match(
    EPIC_PHASE,
    /`8` is\s+never\s+a\s+single-ticket\s+estimate/,
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
      /minus\s+any\s+stage\s+the\s+preconditions\s+below\s+skip/,
      `${name} must qualify the emitted order with the stage-level skip`,
    )
    assert.match(
      text,
      /minus\s+every\s+child `reconcileEpicChildren` does\s+NOT\s+report `missing`/,
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
    /never\*\*\s*a\s+single\s+oversized\s+ticket|needs-human/i,
    'over the child cap must fall to needs-human, never a single oversized ticket',
  )
  // recursion guard
  assert.match(EPIC_PHASE, /allowEpic: false/, 'must state the allowEpic:false recursion guard')
  assert.match(
    EPIC_PHASE,
    /no\s+child\s+recursion|never\s+itself\s+decomposed/i,
    'must forbid child recursion',
  )
  // cycle safety
  assert.match(EPIC_PHASE, /cycle\s+safety|blockedByKeys` cycle/i, 'must state cycle safety')
  // validate-before-write atomicity
  assert.match(
    EPIC_PHASE,
    /validate\s+everything\s+locally\s+BEFORE\s+the\s+first\s+Linear\s+write|validate-before-write/i,
    'must state the validate-before-write atomicity guard',
  )
  assert.match(EPIC_PHASE, /zero\s+Linear\s+writes/i, 'must state zero writes on failure')
  // idempotent resume
  assert.match(EPIC_PHASE, /idempotent\s+resume/i, 'must state idempotent resume')
  assert.match(EPIC_PHASE, /adopts/i, 'must state that re-runs adopt existing children')
  assert.match(EPIC_PHASE, /clean\s+no-op/i, 'must state a fully-built epic re-run is a no-op')
  // original-becomes-parent + parent-label exception
  assert.match(EPIC_PHASE, /original-becomes-parent/i, 'must state original-becomes-parent')
  assert.match(EPIC_PHASE, /Parent-label\s+exception/, 'must state the parent-label exception')
  assert.match(
    EPIC_PHASE,
    /neither\*\*\s*`agent-friendly`\s*\*\*nor\*\*\s*`needs-human`/,
    'the parent must carry neither agent-friendly nor needs-human',
  )
  // per-child full plan + separate contract description + intra-epic DAG + external links
  assert.match(
    EPIC_PHASE,
    /each\s+a\s+\*\*Phase\s+3\*\*\s+plan/,
    'children must receive full plans',
  )
  assert.match(
    EPIC_PHASE,
    /save\s+each\s+child's\s+contract\s+description/,
    'children need descriptions',
  )
  assert.match(EPIC_PHASE, /external\s+conflict\s+links/i, 'must wire external conflict links')
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
    /reconcileEpicChildren\(spec,\s*hydratedLiveChildren\)`\s*—\s*never\s+by\s+eye,\s*never\s*\n?by\s+title/,
    'the phase must mandate reconcileEpicChildren as the idempotent-resume join over hydrated children, not an eyeball or title match',
  )
  // The unambiguous-rename repair must be aimed at the CHILD's marker, never at the spec key:
  // `specKey` is the namespace `adopted`, the siblings' `blockedByKeys` and `epicWiringPlan` all
  // resolve through, so re-pointing the spec at `liveKey` strands those refs and throws mid-wire.
  assert.match(
    EPIC_PHASE,
    /rewrite\s+\*\*its\s+own\*\*\s+description\s+marker\s+to `epicChildMarker\(specKey\)`[\s\S]*?never\s+the\s+spec\s+key/,
    'the unambiguous rename must repair the child marker, never re-point the spec key',
  )
  // The repair is a description WRITE, and the tracker save replaces the description (the same
  // hazard this phase already warns about at its other two marker-write points). Without the
  // preserve-the-bytes clause, a literal `save_issue(id, description: epicChildMarker(specKey))`
  // wipes the child's gated plan body while the child still reads as adopted — a silent loss.
  assert.match(
    EPIC_PHASE,
    /replacing\s*\n?only\s+the\s+marker\s+substring\s+and \*\*preserving\s+the\s+rest\s+of\s+that\s+description's\s+bytes\s+verbatim\*\*/,
    'the rename repair must preserve the rest of the child description, since the save replaces it',
  )
})

test('both references carry the EPIC triage tier and flow', () => {
  // interactive
  assert.match(INTERACTIVE, /\*\*EPIC\*\*/, 'interactive-mode must add the EPIC triage tier')
  assert.match(
    INTERACTIVE,
    /Epic\s+decomposition \(interactive: propose → confirm → create\)/,
    'interactive-mode must carry the propose-confirm-create flow',
  )
  assert.match(
    INTERACTIVE,
    /create\s+this\s+epic/i,
    'interactive AskUserQuestion must offer create-this-epic',
  )
  assert.match(
    INTERACTIVE,
    /plan\s+as\s+one\s+ticket/i,
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
    /Epic\s+decompose-and-auto-create/,
    'the brief must carry the headless auto-create flow',
  )
  assert.match(
    BRIEF,
    /allowEpic: false/,
    'the brief must document the allowEpic:false recursion guard',
  )
  assert.match(
    BRIEF,
    /fall\s+back\s+to\s+a\s+single-ticket\s+plan\s+and\s+record\s+the\s+reason/i,
    'the brief must document the single-ticket fallback on guard failure',
  )
  assert.match(
    BRIEF,
    /reconcileEpicChildren\(spec,\s*hydratedLiveChildren\)`[\s\S]*?Never\s+adopt\s+by\s+eye,\s*never\s+by\s+title/,
    'the brief must mandate reconcileEpicChildren as the idempotent-resume join over hydrated children, not an eyeball match',
  )
  // Pin the three reconcileEpicChildren outcomes by their actual wording, not just the symbol's
  // presence — a resume step that stops calling the function while the identifier still appears
  // elsewhere in the file (e.g. only in the return-shape doc) must not stay green.
  assert.match(
    BRIEF,
    /\*\*\(1\)\s+aligned\*\*\s*\(no\s+orphans\)\s*—\s*create\s+exactly\s+the\s+spec\s+keys `missing` names/,
    'the brief must document outcome (1) aligned: create exactly the missing spec keys',
  )
  assert.match(
    BRIEF,
    /\*\*\(2\)\s+unambiguous\s+rename\*\*\s*\(`repairs` holds\s+exactly\s+one `\{specKey,\s*liveKey,\s*id\}`/,
    'the brief must document outcome (2) unambiguous rename: repairs holds exactly one entry',
  )
  assert.match(
    BRIEF,
    /\*\*\(3\)\s+ambiguous\s+drift\*\*\s*\(`ok:false`\s*—\s*multiple\s+orphans,\s*an\s+unmarked\s+child,\s*duplicate\s+live\s+marker\s+keys,\s*or\s+a\s+non-array `liveChildren`\)\s*—\s*take\s+the\s+SAFE\s+branch:\s*report `errors`,\s*write\s+nothing,\s*create\s+nothing,\s*never\s+guess/,
    'the brief must document outcome (3) ambiguous drift taking the SAFE branch refusal',
  )
  // Same repair-direction pin as the SKILL phase carries: the rename repair rewrites the CHILD's
  // marker. Re-pointing the spec key at `liveKey` instead would strand every sibling `blockedByKeys`
  // ref and the `adopted` entry (both keyed by `specKey`) and throw inside `epicWiringPlan`.
  assert.match(
    BRIEF,
    /rewrite\s+\*\*its\s+own\*\*\s+description\s+marker\s+to `epicChildMarker\(specKey\)`[\s\S]*?never\s+the\s+spec\s+key/,
    'the brief must repair the child marker on an unambiguous rename, never re-point the spec key',
  )
  // Same preserve-the-bytes clause as the SKILL phase carries: the repair is a description write and
  // the save replaces the description, so a marker-only save wipes the child's plan body.
  assert.match(
    BRIEF,
    /preserving\s+the\s+rest\s+of\s+that\s+description's\s+bytes\s+verbatim\*\*[\s\S]*?save\s*\n?\s*replaces\s+the\s+description/,
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
    /estimate.*rejected[\s\S]{0,180}retry\s+without.*estimate/i,
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
    /every\s+planned\s+ticket.{0,80}non-null\s+estimate/i,
    'all planned tickets must carry an estimate',
  )
  assert.match(
    BRIEF,
    /parent:\{title,goal,keyChanges\[\],priority\}/,
    'the epic draft shape must include parent priority',
  )
  assert.match(
    BRIEF,
    /sum\s+of\s+its\s+children.?s\s+estimates/i,
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

test('BOS-992: Phase 0 invokes the stale plan-scratch reaper before tracker reads', () => {
  assert.match(
    PHASE_0_SECTION,
    /node "\$BOSS_PLAN_TOOLBOX\/plan-scratch-reap\.mjs" \.linear-plans/,
    'Phase 0 must invoke the plan-scratch reaper from the installed toolbox',
  )
  assert.ok(
    SKILL.indexOf('plan-scratch-reap.mjs') < SKILL.indexOf('## Phase 1'),
    'plan-scratch-reap must run before Phase 1, not from the success-only cleanup tail',
  )
  assert.match(
    PHASE_0_SECTION,
    /plan-scratch-reap\.mjs"\s+\.linear-plans\s+\|\|\n\s+echo\s+"warning:\s+stale\s+plan-scratch\s+reap\s+failed\s+\(non-fatal\)"\s+>&2/,
    'the run-start reaper must warn on failure without aborting planning',
  )
  assert.doesNotMatch(
    PHASE_0_SECTION,
    /plan-scratch-reap\.mjs[^\n]*\|\| true/,
    'the run-start reaper must not hide failures with `|| true`',
  )
})

test('BOS-1193: Phase 5 cleanup is run-scoped, never issue-scoped', () => {
  // BOS-992 pinned an `<ISSUE-ID>`-scoped cleanup as the safety property. It is not one: two runs
  // can plan the SAME ticket concurrently, and a correctly `<ISSUE-ID>`-scoped delete was observed
  // removing a peer's live in-flight scratch. The run id is the only identifier that separates
  // them, so cleanup removes one run directory and can no longer name anything outside it.
  const phase5 = sectionBetween(SKILL, '## Phase 5 — Discard local artifacts', '\n## Phase 6')
  assert.equal(
    count(phase5, 'rm -rf .linear-plans/run-<RUN-SCRATCH-ID>'),
    1,
    "Phase 5 must remove this run's scratch directory exactly once",
  )
  assert.ok(
    phase5.includes('if [ -e .linear-plans/run-<RUN-SCRATCH-ID> ]; then CLEANUP_RC=1; fi'),
    'Phase 5 must assert the post-condition: `rm` can exit 0 on macOS having removed nothing',
  )
  for (const forbidden of ['find .linear-plans', "-name '<ISSUE-ID>", '.linear-plans/<ISSUE-ID>']) {
    assert.ok(
      !phase5.includes(forbidden),
      `Phase 5 must not reintroduce issue-scoped cleanup (${forbidden}) — it reaches a same-ticket peer's live scratch`,
    )
  }
})

test("every scratch-cleanup site removes exactly this run's directory, with no glob surface", () => {
  // Under zsh and fish an UNMATCHED glob aborts the WHOLE command line, so a cleanup line carrying
  // one deletes nothing while reading as a clean pass (BOS-1160). The old shape mitigated that with
  // one `find … -name 'PREFIX*' -delete` per line; removing a run directory retires the glob
  // surface altogether, which is the strictly stronger form of the same property. Pin it: no
  // scratch-cleanup line anywhere in the body may carry a glob.
  // 5 since BOS-1193 review: the draft-metadata abort was the one path the collapse missed — it
  // was re-addressed to the run directory but still enumerated filenames and carried no residual
  // assertion, so it now uses the same shape as its four siblings.
  const SITES = 5
  const lines = SKILL.split('\n')
  assert.equal(
    lines.filter((l) => l.includes('rm -rf .linear-plans/run-<RUN-SCRATCH-ID>')).length,
    SITES,
    "every cleanup path must remove this run's scratch directory",
  )
  assert.equal(
    lines.filter((l) => l.trim() === 'CLEANUP_RC=0').length,
    SITES,
    'each cleanup site must reset CLEANUP_RC before accumulating',
  )
  assert.equal(
    lines.filter((l) => l.includes('[ -e .linear-plans/run-<RUN-SCRATCH-ID> ]')).length,
    SITES,
    'each cleanup site must re-check existence — `rm` can exit 0 having removed nothing',
  )
  assert.equal(
    lines.filter((l) => /\[\s+"\$CLEANUP_RC"\s+(?:=|!=)\s+0\s+\]/.test(l)).length,
    SITES,
    'each cleanup site must act on the accumulated status',
  )
  for (const line of lines) {
    const isCleanup = /(^|\s)(rm -f|rm -rf|find) /.test(line) && line.includes('.linear-plans')
    assert.ok(
      !(isCleanup && line.includes('*')),
      `a scratch-cleanup line must never carry a glob — an unmatched one aborts the line under zsh and fish: ${line.trim()}`,
    )
    assert.ok(
      !(isCleanup && line.includes('|| true')),
      `a cleanup line must not force success with \`|| true\` — real deletion errors must propagate: ${line.trim()}`,
    )
  }
})

test('BOS-1193: the body records the scratch-concurrency hazards the notes paid for', () => {
  // These are the corrections a future run needs before it touches `.linear-plans/`. Without them
  // the shape above is a rule with no reason attached, and the next edit re-derives the defect.
  for (const [needle, why] of [
    ['shared mutable state', 'that `.linear-plans/` is shared across concurrent runs'],
    ['is _not_ sufficient', 'that an issue-scoped pattern is not sufficient'],
    ['belongs to a peer', "that a peer's untracked file is never deleted"],
    ["-name 'PREFIX*' -delete", 'the load-bearing find spelling'],
    ['aborts the **whole command line**', 'why a bare shell glob is unsafe under zsh and fish'],
    ['never by grepping a ticket id', 'that a sentinel run dir is selected from the handed RUN_ID'],
  ]) {
    assert.ok(
      SKILL.includes(needle),
      `the skill body must record ${why} (missing ${JSON.stringify(needle)})`,
    )
  }
})

test('BOS-1193: the payload never instructs an agent to invent a scratch filename', () => {
  for (const placeholder of [
    '<safe-orig.md>',
    '<new.md>',
    'BODY="$(mktemp)"',
    'scratch JSON file you just wrote',
  ]) {
    assert.ok(
      !SKILL.includes(placeholder),
      `SKILL.md still names scratch by placeholder: ${placeholder}`,
    )
    assert.ok(
      !BRIEF.includes(placeholder),
      `the drafting brief still names scratch by placeholder: ${placeholder}`,
    )
  }
})

// ---------------------------------------------------------------------------
// Size-ratchet — the resident body stays below the pre-split baseline.
// ---------------------------------------------------------------------------

test('the resident SKILL.md body is pinned exactly, below the pre-split baseline', () => {
  // PRE_SPLIT_BASELINE is a rolling upper bound kept a small margin above RATCHET, NOT the
  // literal pre-split body size: it began at 25548 (the hand-written body before the BOS-204
  // references split) and is re-baselined upward as Phase-4 prose legitimately grows. The
  // RATCHET < PRE_SPLIT_BASELINE invariant preserves that explicit margin so an accidental
  // bulk regrow in one edit trips the guard instead of sliding both constants up together.
  //
  // BOS-768 kept it (rather than deleting a decorative constant) precisely because it is NOT
  // decorative: it is the only thing that reds when a single edit slides both numbers up
  // together, which is the failure the exact pin below cannot see. It is passed as `below`, so
  // a violation now names both readings — pin raised toward the baseline, or baseline overdue
  // for re-derivation — instead of prescribing one cause.
  // On a rebase this constant conflicts too; see the REBASE HAZARD note at RATCHET below for
  // how to resolve BOTH — this one is re-baselined above the new measurement, never set to it.
  const PRE_SPLIT_BASELINE = 122329
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
  // Re-baselined a further +826 (88851 → 89677) for BOS-776, carrying PRE_SPLIT_BASELINE with it
  // to keep the 16-byte guard margin. Phase 4 step 5 stopped deciding dependency edges in prose and
  // became I/O glue over `toolbox/plan-deps-lib.mjs`. The narrative it replaced was larger than the
  // caller that replaced it, but step 5 simultaneously gained four requirements that did not exist
  // before and are resident by necessity: the explicit candidate field list (without it the fetch
  // returns empty descriptions and the run reports zero links with no error — indistinguishable
  // from a clean result), fetching declared relations BY ID regardless of state (the only path by
  // which a cleared prerequisite is ever considered, since selectPlanned never returns one), cycle
  // safety relocated AFTER the started-side downgrade and scoped to blocking writes, and both
  // `appendRelatedTo` branches written as instructions. The invocation itself is CommonJS
  // `pathToFileURL` for the same byte reason as the two lines above. Net growth is the new
  // requirements, not regrown narrative: the decision ladder itself now lives in the library.
  // Re-baselined a further +905 (89677 → 90582) for BOS-776 review round 1, carrying
  // PRE_SPLIT_BASELINE with it to keep the 16-byte guard margin. Step 5(c)'s block was
  // unrunnable and its payload underspecified, both silently: it read an empty `mktemp` file
  // (`JSON.parse("")` throws, and `check-skill-shell.mjs` only `bash -n`s, so the gate was
  // green on a block that could never execute), it named `stateRoles` with no accessor —
  // omitting it resolves every state to unknown, downgrades every blocking edge to
  // `relatedTo` under an `info` note, and renders a run that linked nothing
  // indistinguishable from one that found nothing to link — and it neither re-derived
  // `BOSS_PLAN_TOOLBOX` (shell env does not persist across tool invocations, so `T` can be
  // undefined) nor carried the `.catch` every sibling invocation in this file carries. All
  // four are resident by necessity: this is the single invocation the whole extraction hangs
  // on, and an agent that hits a raw throw here re-decides the edges in prose, which is the
  // defect BOS-776 exists to remove.
  // Re-baselined a further +1129 (90582 -> 91711) for BOS-776 review round 2, carrying
  // PRE_SPLIT_BASELINE with it to keep the 16-byte guard margin. Two step-5 instructions were
  // wrong in the direction of silence. (a)'s new title+label prefilter is a context-scale
  // measure, but read as an overlap filter it drops a candidate whose title looks unrelated and
  // whose `## Key changes` names your files -- the missed-prerequisite defect re-entering through
  // the filter rather than through fuzzy search, so the prose now bounds what the prefilter is
  // allowed to decide. And (f), the run's only post-dependency tracker save, was gated on
  // `>=1 relation was written`: an arealess subject, an unresolved declared relation, an ambiguous
  // orientation and a canceled prerequisite each write no edge and each raise a warning or a
  // question, so every one of those outcomes -- and the `agent-question` label with them -- was
  // computed, rendered into the description, and then never saved. Both are instruction bytes the
  // caller must execute, not narrative.
  // Re-baselined a further +335 (91711 -> 92046) for BOS-776 review round 3, carrying
  // PRE_SPLIT_BASELINE with it to keep the 16-byte guard margin. Two instruction bytes the caller
  // must execute. 5(c) named the `subject` payload without saying it needs a state of its own, and
  // the ladder's last rung reads BOTH sides' roles -- so a subject assembled from the fields (a)
  // lists for candidates downgraded every edge, not some. And 5(f)'s new `labels` write was the one
  // label write in this file that did not restate the union rule the file states three times
  // elsewhere; `labels` replaces the whole set, so executed literally it strips the labels Step 4
  // had just merged and saved.
  // Re-baselined a further +1081 (92046 -> 93127) for BOS-776 review round 4, carrying
  // PRE_SPLIT_BASELINE with it to keep the 16-byte guard margin. Three step-5 inputs the library
  // cannot derive and the caller was never told to supply, each silent in the shipped glue. A
  // logical verdict carried no direction, so the library oriented a declared prerequisite by
  // priority and could write the edge backwards -- the wrong-edge class this branch exists to
  // remove, produced by the fix. `extractKeyChangeAreas` drops every slash-free token without
  // `moduleRoots`, so a plan naming bare module names contributed no areas at all and every
  // overlap was missed. And the documented epic re-run loop reset the library's one-call depth
  // cap, so a malformed parent/child graph could be walked forever; the loop is now bounded and
  // carries expanded parents in `excludeIds`. The +11 remainder renames the step's own citation
  // to `$BOSS_PLAN_TOOLBOX/plan-deps-lib.mjs`: the shipped-toolbox gate keys on that adjacency,
  // and the concatenated invocation it had instead left the file this step depends on able to
  // drop out of the payload with every gate green.
  // Re-baselined a further +566 (93127 -> 93693) for BOS-907, carrying
  // PRE_SPLIT_BASELINE with it to keep the 16-byte guard margin. Headless Phase 2 now carries the
  // resident measurement-trust rule: the orchestrator measures on-disk artifacts with `stat` or
  // `wc -c`, never decides from reported sizes, and widens post-sentinel reverify to all consumed
  // dispatch artifacts. This belongs in the resident orchestrator section because the failure is the
  // orchestrator trusting a dispatch report before it opens any drafting reference.
  // Review repair settled at +783 B (92594 -> 93377) to require epic child-plan manifest
  // paths to be keyed by child id, so an unrelated .linear-plans file cannot cover a missing child.
  // Review repair settled at +1809 B (93377 -> 95186) to bind those child-id entries to
  // canonical parent/key child-plan artifacts and to run the same scratch cleanup when the widened
  // artifact verifier fails. These bytes are resident because they execute in the headless
  // orchestrator path before any reference can be consulted.
  // Review repair spent +89 B (94997 -> 95086) to derive child plan keys from the epic spec
  // artifact instead of the untrusted sentinel payload.
  // Review repair settled at +93 B (95086 -> 95179) to require resumed epics to rehydrate spec
  // validation scratch and to bind child plan paths to the exact parent/key/id/title filename
  // without reading child ids from the serialized spec, which deliberately omits them.
  // Review repair settled at +7 B (95179 -> 95186) to require the rehydrated epic spec path to be
  // `.linear-plans/<parent>.epic-spec.json` with a matching `parentId`, closing fabricated-spec
  // child metadata from the resident headless verifier.
  // Review repair settled at +13 B (95186 -> 95199) to require full spec-child coverage and the
  // complete parent/child guard manifest in the resident headless verifier.
  // Review repair settled at +2 B (95199 -> 95201) to require a spec-child bijection and direct
  // `.linear-plans` scratch artifacts while keeping the pre-split ceiling.
  // Rebased BOS-773 onto BOS-907 and banked the merged -37 B resident shrink.
  // BOS-775 banks 95164 -> 95082 (-82 B): content labels now use optionalLabelName with literal
  // fallback and Phase 4 strips agent-plan itself; the wording is tighter than the old taxonomy
  // paragraph it replaces, so preserve the shrink rather than leaving silent headroom.
  // Re-baselined +4607 B (95082 -> 99689) for BOS-769 after the BOS-775 shrink: the resident
  // orchestrator section gained
  // three run-boundary guards that execute before any reference can be consulted — Phase 1
  // idempotence, Phase 2 metadata validation/premise capture, and Phase 4 premise re-verification.
  // The review repair bytes are included here: premise drift stays non-aborting while still
  // deleting the new run-boundary JSON scratch on every abort/success cleanup path.
  // BOS-755 re-baselines 99689 -> 100577 after the BOS-769 rebase and carries
  // PRE_SPLIT_BASELINE to keep the 98-byte guard margin. Resident bytes name the
  // spec-attachment decode verb, required childIds rejection, the reserved parent wiring key, and
  // the epic-parent validation mode. Re-measured 2026-08-22.
  // BOS-759 re-baselines 100577 -> 103382 (+2805 B): cleanup now names the single-ticket,
  // run-boundary, attachment-header, and description scratch files explicitly in every resident
  // terminal path, with checked fixed-path deletion beside the one-pattern-per-line find sweep.
  // BOS-757 banks 103382 -> 103310 (-72 B): the new optional Premises pointer is folded into
  // tighter drafting-spec prose, so preserve the shrink rather than leaving silent headroom.
  // BOS-754 re-baselines 103310 -> 103902 and carries PRE_SPLIT_BASELINE with it to keep
  // the 129-byte guard margin. The resident Phase 0 and post-terminal notes blocks now carry the
  // same fail-loud toolbox preamble as the sibling cores, plus the missing drift-helper branch and
  // the operation-map/signature notes that must run before any drafting reference is opened.
  // BOS-926 re-baselines 103902 -> 104003 (+101 B): the sibling-class enumeration rule is resident
  // as well as in the headless brief, with Phase 3 prose kept under the pre-split baseline.
  // BOS-1024 banks 104003 -> 104002 (-1 B): the await-helper citation was folded into existing prose.
  // BOS-1002 re-baselines 104002 -> 104495 (+493 B) for the resident fail-closed installed-skill
  // drift gate. This must run before tracker writes, so it cannot live behind a later reference.
  // Review repair re-baselines 104495 -> 104511 (+16 B) for the old-CLI fallback: unsupported
  // `--gate` degrades to the warning helper, while real drift remains BLOCKED.
  // BOS-999 banks 104511 -> 103913 (-598 B): resident epic orchestration now rejects decomposing
  // epic children, hydrates children before resume reconciliation, verifies child guard scratch
  // under the parent prefix, states the run-prefix cleanup invariant, and names the stronger
  // epic reverify/header-prefix contracts found during review.
  // BOS-992 re-baselines 103913 -> 104149 (+236 B): Phase 0 now invokes the existing age-based
  // plan-scratch reaper after the config-ready gate and before any tracker read, with a visible
  // non-fatal warning path. This must stay resident because abort paths skip Phase 5.
  // BOS-996 re-baselines 104149 -> 104519 (+370 B): Phase 4 now names the adapter-specific
  // workflow-state/status contract, stateRolesFor(config), and same-epic skip path.
  // BOS-1030 banks 104519 -> 104483 (-36 B) while naming Codex's awaited dispatch pair.
  // BOS-1099 re-baselines 104483 -> 107066 (+2583 B), carrying PRE_SPLIT_BASELINE with it to keep
  // the 37-byte guard margin. The notes phase gained two gates it must clear before it does
  // anything: the caller-suppression check (a nested run must not duplicate the notes its
  // dispatcher already owns) and the once-per-run sampling roll shared with every other
  // reporting phase. Both are read-before-acting rails, not situational detail — routing them
  // to a reference would have them read after the dispatch they exist to prevent.
  // Review round 2 re-baselines 107066 -> 107462 (+396 B), carrying PRE_SPLIT_BASELINE with it to
  // keep the 37-byte guard margin. A dispatched worker inherits none of its caller's environment,
  // so the suppression gate needed the caller to bind `BOSS_NOTES_SUPPRESSED` into the invocation
  // as well as name it: without that the gate reads an unassigned name, takes the not-suppressed
  // branch, and ships the duplicate it exists to remove with both pins still green. Carrying the
  // baseline up is what this constant is for — unlike boss-build's PRE_EXTRACTION_BASELINE (a
  // fixed literal that must never rise), this one is documented above as a rolling bound whose job
  // is to force exactly this justification, not to forbid the growth.
  // BOS-1102 re-baselines 107462 -> 108459 (+997 B), carrying PRE_SPLIT_BASELINE with it to keep
  // the 37-byte guard margin. Collapsing each resident toolbox preamble from an eight-line inline
  // probe to one line that sources the shipped `toolbox/boss-plan-env.sh` banked -371 B; review
  // then spent that saving twice over on two defects the collapse had shipped, and the pin records
  // the net rather than the flattering half.
  //   +1000 B, the third locate candidate at each of the ten resident sites. `${…:-default}`
  //   substitutes only when the variable is UNSET, so the two-candidate line searched
  //   {pre-set, ~/.codex} and dropped ~/.claude entirely the moment anything pre-set
  //   BOSS_SKILLS_HOME — a healthy Claude install BLOCKing with a remedy that cannot fix it.
  //   Naming ~/.claude explicitly is the only spelling that survives a pre-set value.
  //   +368 B, the paragraph describing the helper, which had said it tests for the
  //   `boss-plan/toolbox` DIRECTORY (it tests for the helper file — the distinction the helper's
  //   own comment exists to defend) and called the fallback "the second `.`" when there is only
  //   ever one `.` in the line.
  // Correctness over bytes, as with the earlier `. a || . b` locate that this same line replaced:
  // `.` is a POSIX special built-in, so a missing file exits sh/dash before the fallback or the
  // BLOCKED message can run. Bytes that buy a loud failure on a real install are not overhead.
  // REBASE HAZARD: RATCHET is a MEASUREMENT of the resident body at this branch's base, never a
  // value to merge. Any concurrent branch that touches the body re-measures it too, so this file
  // conflicts by construction — and resolving that conflict by picking a side banks a number
  // nothing measured, which reds the gate or quietly moves the pin UP. Re-run this test after the
  // rebase, bank the size it reports here, and keep every prior re-baseline entry above: the
  // history is why the pin is allowed to move only down. PRE_SPLIT_BASELINE is NOT the same kind
  // of number and must not be set to that measurement — it is the rolling upper bound described at
  // its own declaration, so re-baseline it ABOVE the new RATCHET with the existing margin intact.
  // Collapsing that margin disables the one check that catches both numbers sliding up together.
  // BOS-1105 re-baselines 108459 -> 109064 (+605 B), carrying PRE_SPLIT_BASELINE 108496 ->
  // 109101 to keep the existing 37-byte guard margin. Bookkeeping became advisory: the skills-
  // drift gate now warns instead of exiting BLOCKED, the step-2 heading says "report" rather
  // than "gate", and a failed ledger write warns and continues. All three are rails read at the
  // moment they fire, so they stay resident -- a reference would be consulted after the run had
  // already stopped on the failure the new prose exists to prevent.
  // BOS-1178 re-baselines 109064 -> 109448 (+384 B), carrying PRE_SPLIT_BASELINE 109101 ->
  // 109485 to keep the existing 37-byte guard margin. Resident epic orchestration now names the
  // parent-overview plan-contract gate with `--mode epic-parent` at the exact point before parent
  // attachment finalize/save; this cannot live solely in the drafting brief because the resident
  // orchestrator path skips that reference while making the parent write.
  // BOS-1176 re-baselines 109448 -> 109804 (+356 B), carrying PRE_SPLIT_BASELINE 109485 ->
  // 109841 to keep the existing 37-byte guard margin. The resident drafting contract now states
  // that its section list governs `descriptionSummary` only, while preserving the plan attachment's
  // native headings, terminal Original notes body, and plan-wide unsigned-upload rule. These bytes
  // stay resident because both drafting modes enter Phase 3 before the detailed reference path.
  // BOS-1177 re-baselines 109804 -> 111805 (+2001 B), carrying PRE_SPLIT_BASELINE 109841 ->
  // 111842 to keep the existing 37-byte guard margin. The resident body now names the config-backed
  // plan-file floor, explicit exemptions, consumer non-goal, and structure-only `.rejected`
  // retention, including the review-found abort cleanup enumerations, because Phase 4 owns the
  // pre-write gate and cleanup behavior.
  // BOS-1187 re-baselines 111805 -> 113125 (+1320 B), carrying PRE_SPLIT_BASELINE 111842 ->
  // 113162 to keep the existing 37-byte guard margin. Phase 4 step 5 now states the verbatim
  // `## Key changes` rule, names `repoWideTokens`/`areaAliases` in the classify payload, requires
  // the printed `subjectAreas`/unresolved line to be read before any edge write, and carries the
  // imperative branch for the `no-subject-areas` and `subject-unresolved-areas` warnings. These
  // bytes stay resident because step 5 IS the write path: a reference would be consulted after
  // the fabricated or missed edge the prose exists to prevent had already been written.
  // BOS-1186 re-baselines 113125 -> 113326 (+201 B), carrying PRE_SPLIT_BASELINE 113162 ->
  // 113363 to keep the existing 37-byte guard margin. Phase 4's violation-code list was itself a
  // copied-forward claim -- it named seven of the guard's codes and had gone out of date twice --
  // so it now enumerates every entry of the guard's exported `VIOLATION_CODES` plus the dynamic
  // `vacuous-*` family, and `scripts/bs-plan-skill.test.mjs` asserts the two against each other.
  // These bytes stay resident because Phase 4 IS the pre-write gate: an operator reading a tagged
  // stderr line it cannot find in the list has no way to tell a new code from a typo.
  // BOS-1193 banks 113326 -> 112243 (-1083 B), leaving PRE_SPLIT_BASELINE at 113363 as the
  // BOS-999 and BOS-1030 down-banks did: the baseline is carried only when the pin rises.
  // Phase 4's scratch cleanup collapsed from a per-artifact enumeration to one run-scoped
  // removal addressed through the registry, so the abort paths that each restated the list now
  // name a single removal. The saving is deleted enumeration, not routed prose.
  // BOS-1193 review re-pins 112243 -> 112608 (+365 B); PRE_SPLIT_BASELINE stays at 113363, so the
  // guard margin narrows from 1120 to 755 bytes of headroom rather than being carried up — the one
  // check that catches both numbers sliding together stays armed. The bytes are the draft-metadata
  // abort adopting the same five-line removal its four siblings already use: it was the single
  // cleanup path the collapse missed, re-addressed to the run directory but still enumerating
  // `{precheck,premises,premise-states}.json` and carrying no residual-existence assertion, so it
  // stranded the raw Phase 1 source every other failing gate deletes. These bytes are resident
  // because they ARE the cleanup contract this ticket exists to make uniform; a reference cannot
  // hold the shell an abort path has to run inline.
  // BOS-1199 re-baselines 112608 -> 117028 (+4420 B), carrying PRE_SPLIT_BASELINE 113363 ->
  // 117783 to keep the 755-byte guard margin BOS-1193's review narrowed it to -- the +4420 is
  // this branch's own growth, re-derived against the rebased base rather than merged from a
  // side. Phase 2 step 2 now states the snapshot's PROVENANCE (the tracker's stored
  // description, not a rendering) and its byte shape (no added terminal byte), and Phase 4
  // gains the post-save write-back verification with its
  // loud-but-non-destructive drift branch plus the corrected patch-anchor guidance. All of it
  // stays resident because it IS the write path: the snapshot rule is read before the dispatch
  // that consumes it, and the read-back and anchor rules are consulted at the save itself -- a
  // reference would be opened only after the damage the prose exists to prevent had been stored,
  // and the tracker keeps no description history to recover it from.
  // BOS-1199 review round re-baselines 117028 -> 118415 (+1387 B), carrying PRE_SPLIT_BASELINE
  // 117783 -> 119170 to keep the 755-byte guard margin. Phase 6's report list now names the
  // step-6 write-back verdict, Phase 5's cleanup rule carves out the drift branch that deliberately
  // retains the scratch, and step 5(f)'s incremental `patch` path now says how it materializes the
  // `--intended` bytes step 6 requires (the stored read-back with the same patch ops applied), with
  // step 6 naming that input on its side. These bytes stay resident because they close a hole
  // BETWEEN two resident steps: 5(f) writes and 6 verifies, and a reference would be consulted only
  // after a patch save had already landed with nothing to verify it against.
  // BOS-1199 rebase re-pins 118415 -> 118499 (+84 B); PRE_SPLIT_BASELINE stays at 119170, so the
  // guard margin narrows from 755 to 671 bytes rather than being carried up. The bytes are four
  // occurrences of `run-<RUN-SCRATCH-ID>/`: this branch wrote the write-back check's two scratch
  // artifacts flat under `.linear-plans/`, which was the correct shape at the branch point and
  // stopped being one when BOS-1193 landed run-scoped scratch on main. Nothing conflicted -- the
  // two changes touch different lines -- so the defect arrived textually clean and was caught by
  // BOS-1193's payload ratchet, which reds on any `.linear-plans/` token that is not run-scoped.
  // Left flat, Phase 5's single `rm -rf .linear-plans/run-<RUN-SCRATCH-ID>` could never reach
  // them and both files -- one of which holds the tracker's stored description -- would outlive
  // the run. The matching `image-guard-final`/`image-guard-stored` families are declared in
  // `skills-toolbox/plan-scratch-paths.mjs`, since run-scoping alone still leaves a name the
  // registry does not know.
  // BOS-1189 re-baselines 118499 -> 121658 (+3159 B), carrying PRE_SPLIT_BASELINE 119170 ->
  // 122329 to keep the 671-byte guard margin the BOS-1199 rebase left. Four resident additions,
  // each read at the point it fires on a write path, which is why none of them route to
  // `references/headless-drafting-brief.md`:
  //   +~1.4 kB, the retained-plan justification beside BOTH Phase 4 failing-gate cleanups. These
  //   bytes ARE the cleanup contract — the same argument BOS-1193's review entry makes — and a
  //   reference cannot hold the shell an abort path runs inline. Without the stated reason, the
  //   next editor tidying the `rm -f` line re-adds `"$PLAN_FILE"` and silently restores the
  //   full-redraft cost this ticket exists to remove.
  //   +~380 B, the epic child-marker placement rule. `--require-verbatim` treats everything from
  //   the terminal `## Original notes` heading onward as the compared block, so a marker written
  //   after it is rejected by the guard. The resident epic orchestrator makes that child write
  //   before it opens any reference, exactly as BOS-1178's parent-overview gate does.
  //   +~500 B, the config-first argument-order rule generalized past `validatePlanDescription` to
  //   its three siblings and `extractKeyChangeAreas`, plus which of the guard's two faults each
  //   message names. The two faults need OPPOSITE fixes, and `extractKeyChangeAreas` is invoked
  //   from resident Phase 4 step 5 — a reader who cannot tell "swapped" from "no contract loaded"
  //   reorders a correct call and gets the same red back.
  //   +~330 B, the interactive `agent-question` strip in Phase 4 step 3's labels bullet. That
  //   bullet IS the single tracker save; interactive resolves every fork with the human and by
  //   contract emits no `## Open Questions`, so a pre-existing label there is a claim the run has
  //   disproved and no other path can clear it.
  const RATCHET = 121658 // exact measured resident body, re-measured 2026-09-07 (BOS-1189)
  assertExactSize({
    below: { name: 'PRE_SPLIT_BASELINE', value: PRE_SPLIT_BASELINE },
    constFile: 'scripts/bs-plan-skill.test.mjs',
    constName: 'RATCHET',
    expected: RATCHET,
    label: 'boss-plan resident SKILL.md',
    measured: measureFile(abs(`${CORE}/SKILL.md`)),
    path: 'services/boss/internal/skillinstall/skills/boss-plan/SKILL.md',
    previous: {
      value: 118499,
      delta: 3159,
      label: 'BOS-1189 retaining the drafted plan across failing Phase 4 gates',
    },
    residual:
      'the references/ files the body routes to, and whether the resident prose is worth its ' +
      'bytes — this pin only knows how many there are',
  })
})

test('BOS-775: content labels use the optional resolver and Phase 4 strips agent-plan', () => {
  assert.match(
    SKILL,
    /Content-taxonomy\s+labels[\s\S]{0,700}optionalLabelName\(config, '<role>'\)[\s\S]{0,180}if\s+it\s+returns\s+`null`,\s+apply\s+the\s+literal\s+display\s+name[\s\S]{0,80}Never\s+create\s+labels/,
    'content taxonomy labels must use optionalLabelName with literal fallback',
  )
  assert.doesNotMatch(
    SKILL,
    /taxonomy\s+name\s+through\s+`labelName`/,
    'taxonomy prose must not send optional labels through labelName',
  )
  assert.match(
    SKILL,
    /existing ∪ additions ∖ strips/,
    'Phase 4 must compute labels as merge minus strip set',
  )
  assert.match(
    SKILL,
    /Set\s+`stripLabels`\s+to\s+`agent-plan`\s+on\s+a\s+successful\s+plan[\s\S]{0,160}do\s+not\s+strip\s+it/,
    'successful planning must strip agent-plan while preserving agent-question',
  )
  assert.match(
    SKILL,
    /`labels`:\s+the\s+merged-minus-stripped\s+set\s+\(names\)/,
    'the saved labels field must send the merged-minus-stripped set',
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

test('BOS-1002: installed-skill gate degrades for an old boss CLI', () => {
  for (const copy of PAYLOAD_COPIES) {
    assert.match(copy.skill, /skills\s+check\s+--gate/, copy.name)
    assert.match(
      copy.skill,
      /case "\$O" in[\s\S]{0,120}\*--gate\*\) node "\$BOSS_PLAN_TOOLBOX\/toolbox-drift\.mjs"/,
      copy.name,
    )
    // BOS-1105 flipped skills drift from BLOCKING to advisory: drift is bookkeeping, so the gate
    // reports it and the run continues. A totally missing install still blocks (asserted
    // separately); only the drift arm warns.
    assert.match(
      copy.skill,
      /warning:\s+installed\s+boss\s+skills\s+drift\s+from\s+checkout\s+source/,
      copy.name,
    )
    assert.match(copy.skill, /bookkeeping\s+only,\s+work\s+state\s+unaffected/, copy.name)
    assert.doesNotMatch(
      copy.skill,
      /BLOCKED:\s+installed\s+boss\s+skills\s+differ\s+from\s+checkout\s+source/,
      copy.name,
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

// ---------------------------------------------------------------------------
// BOS-1186 — the guard's violation vocabulary, machine-checked against the prose.
// ---------------------------------------------------------------------------

// The two prose lists that enumerate the contract guard's violation codes are themselves
// copied-forward claims of exactly the kind this ticket is about: they were written once, went out
// of date as codes were added, and nothing read them against the guard. This test is the durable
// anti-rot mechanism -- it reads the guard's exported set rather than a literal list, so a code
// added to the guard reds here until both bodies name it.
//
// Deliberately NOT asserted: that the lists carry NOTHING else. `unreadable-input` is tagged by the
// CLI rather than by `violation()` and is legitimately in both lists, so this is a subset relation.
// Also not asserted: that the codes appear in the guard's sorted order -- the prose is for a human
// reading a stderr tag, and ordering churn is not a defect worth a red.
test('BOS-1186: both prose lists enumerate every guard violation code', () => {
  assert.ok(VIOLATION_CODES.length > 0, 'the exported code set must be non-empty')
  assert.deepEqual(DYNAMIC_VIOLATION_CODE_PREFIXES, ['vacuous-*'])

  for (const copy of PAYLOAD_COPIES) {
    const phase4 = regionUntilNext(
      copy.skill,
      'One stderr line per violation',
      '; a missing',
      `${copy.name} boss-plan Phase 4 violation-code list`,
    )
    const step8 = regionUntilNext(
      copy.brief,
      'It prints one stderr line per violation',
      '**A non-zero exit',
      `${copy.name} drafting-brief Step 8 violation-code list`,
    )

    for (const code of VIOLATION_CODES) {
      assert.ok(
        phase4.includes(`\`${code}\``),
        `${copy.name}: boss-plan Phase 4 list must name violation code ${code}`,
      )
      assert.ok(
        step8.includes(`\`${code}\``),
        `${copy.name}: drafting brief Step 8 list must name violation code ${code}`,
      )
    }
    for (const prefix of DYNAMIC_VIOLATION_CODE_PREFIXES) {
      assert.ok(
        phase4.includes(`\`${prefix}\``),
        `${copy.name}: boss-plan Phase 4 list must name the dynamic ${prefix} family`,
      )
      assert.ok(
        step8.includes(`\`${prefix}\``),
        `${copy.name}: drafting brief Step 8 list must name the dynamic ${prefix} family`,
      )
    }
  }
})

// The producer side of the same defect: a premise coordinate the drafter never re-read, and a
// constant copied out of a ticket's notes instead of re-measured.
test('BOS-1186: the drafting brief requires anchored premises and re-measured constants', () => {
  for (const copy of PAYLOAD_COPIES) {
    assert.match(
      copy.brief,
      /A\s+premise\s+that\s+cites\s+`file:line`\s+must\s+carry\s+an\s+anchor[\s\S]{0,200}backticked\s+token\s+copied\s+from\s+that\s+location/,
      `${copy.name}: Step 5 must require an anchor token on a premise citing file:line`,
    )
    assert.match(
      copy.brief,
      /unanchored-premise-citation[\s\S]{0,240}stale-premise-citation/,
      `${copy.name}: Step 5 must name both anchor violations the guard can raise`,
    )
    assert.match(
      copy.brief,
      /cite\s+the\s+file\s+\*\*without\*\*\s+a\s+line\s+number/,
      `${copy.name}: Step 5 must state the escape hatch`,
    )
    assert.match(
      copy.brief,
      /pinned\s+numeric\s+constant\s+is\s+a\s+premise[\s\S]{0,320}`—\s+check:\s+`\s+clause\s+names\s+the\s+command\s+that\s+re-measures\s+it/,
      `${copy.name}: Step 5 must require a pinned constant to name its re-measuring command`,
    )
    assert.match(
      copy.brief,
      /The\s+same\s+rule\s+governs\s+\*\*every\s+pinned\s+constant\*\*[\s\S]{0,260}re-measured\s+from\s+the\s+current\s+tree/,
      `${copy.name}: Step 8's measured-size rule must cover every pinned constant`,
    )
    assert.match(
      copy.brief,
      /Re-read\s+every\s+coordinate\s+and\s+constant\s+the\s+ticket\s+supplies[\s\S]{0,700}no\s+longer\s+resolves\s+is\s+re-cited\s+from\s+the\s+current\s+tree\s+or\s+dropped/,
      `${copy.name}: Step 2 recon must re-read ticket-supplied coordinates and constants`,
    )
  }
})
