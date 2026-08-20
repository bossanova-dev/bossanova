// Content/contract test for the bs-sweep-debt skill (BOS-146).
//
// Follows the BOS-144 content-test pattern (scripts/bs-<skill>-skill.test.mjs, wired into
// `make test-smoke` via the scripts/bs-*-skill.test.mjs glob). bs-sweep-debt is a HAND-WRITTEN
// skill (no constructor) — the test reads the committed .claude and .codex SKILL.md directly.
// It pins:
//   * reachability — every reference is pointed at from the body AND exists on disk, in both mirrors;
//   * residency — the load-bearing rotation/PR-reliability/cron contract strings + all 10 slugs
//     stay resident in both mirrors (moving one into a reference silently breaks rotation);
//   * dispatch + sentinel language — the three subagent isolations landed (SURVEY/FIX/CHECK
//     sentinels, dispatch-failure, tier annotations, bounded-poll watch, bulk-output discipline);
//   * loss parity — the cheap-tier survey extraction drops no detector finding (the D8 loss gate);
//   * ratchet — the post-split resident body stays under a ceiling well below the 32575 B baseline.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'
import { hasOpenCronPR } from './cron-open-pr.mjs'
import { parseDetectorFindings, candidateKey } from './bs-sweep-debt-survey.mjs'
import { rewriteClaudeSkillMarkdown } from './sync-codex-skills.mjs'
import {
  assertArtifactSet,
  assertExactSize,
  assertMirrorRegenerated,
  measureFile,
} from './size-ratchet-lib.mjs'

const rootDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const skillDirs = ['.claude/skills/bs-sweep-debt', '.codex/skills/bs-sweep-debt']
const references = [
  'references/category-vocabulary.md',
  'references/category-playbooks.md',
  'references/catalog-and-templates.md',
  'references/subagent-dispatch.md',
  'references/orientation.md',
]
const read = (p) => fs.readFileSync(path.join(rootDir, p), 'utf8')
const CLAUDE_SKILL = read('.claude/skills/bs-sweep-debt/SKILL.md')
const NOTES_TEARDOWN =
  "Before exiting, follow `bs-record-notes` with this run's outcome. Recording is non-fatal: never change the terminal state, exit code, or `git status --porcelain`. Skip gated/no-op runs that observed nothing."

function assertExactBlock(skill, block, label) {
  assert.ok(skill.includes(block), `SKILL.md must keep exact ${label} block resident`)
}

test('documents the non-fatal notes teardown contract', () => {
  for (const [label, skill] of [
    ['source', CLAUDE_SKILL],
    ['codex mirror', read('.codex/skills/bs-sweep-debt/SKILL.md')],
  ]) {
    assert.ok(skill.includes(NOTES_TEARDOWN), `${label} must include the notes teardown contract`)
  }
})

// ---------------------------------------------------------------------------
// Reachability — a body pointer plus an existing file, in BOTH mirrors.
// ---------------------------------------------------------------------------

test('every reference is reachable: a body pointer plus an existing file, in both mirrors', () => {
  for (const dir of skillDirs) {
    const skill = read(path.join(dir, 'SKILL.md'))
    for (const ref of references) {
      assert.match(
        skill,
        new RegExp(ref.replace(/[.]/g, '\\.')),
        `${dir}/SKILL.md must point to ${ref}`,
      )
      assert.ok(fs.existsSync(path.join(rootDir, dir, ref)), `${dir}/${ref} must exist on disk`)
    }
  }
})

// BOS-640 — the extracted PR gate is reachable identically from both trees. No COMMON_REWRITES
// rule matches a bare repo-root `skills-toolbox/` path, so the mirror carries it byte-identically;
// asserting BOTH trees is what catches a mirror that silently lost the invocation.
test('the extracted PR gate is referenced in both mirrors and exists on disk', () => {
  for (const dir of skillDirs) {
    const skill = read(path.join(dir, 'SKILL.md'))
    // Pin the EXECUTED bytes, not the bare path — the body also NAMES the helper in its
    // resident "executed, not read" prose, so a bare-path substring survives deleting the
    // fenced invocation entirely.
    assert.ok(
      skill.includes('bash "$(git rev-parse --show-toplevel)/skills-toolbox/sweep-pr-gate.sh")"'),
      `${dir}/SKILL.md must execute skills-toolbox/sweep-pr-gate.sh`,
    )
    assert.ok(
      skill.includes('test -n "$PR_NUMBER"'),
      `${dir}/SKILL.md must fail the block when the gate produced no PR number`,
    )
  }
  assert.ok(
    fs.existsSync(path.join(rootDir, 'skills-toolbox/sweep-pr-gate.sh')),
    'skills-toolbox/sweep-pr-gate.sh must exist on disk',
  )
})

// ---------------------------------------------------------------------------
// Residency — load-bearing contract strings stay resident in BOTH mirrors.
// ---------------------------------------------------------------------------

test('load-bearing contract strings + all 10 slugs stay resident in both mirrors', () => {
  const resident = [
    'Debt-Area:',
    'Debt-Category:',
    'READY_GREEN_PR',
    'NO_CHANGE',
    // Byte-identical PR-reliability contract (kept case-exact via the leading-article-free form).
    'pushed commit alone does NOT create a PR',
    'Leave no local artifacts',
  ]
  const slugs = [
    'dead-code',
    'duplication',
    'error-handling',
    'security',
    'complexity-hotspot',
    'type-safety',
    'docs-drift',
    'portability',
    'guardrail',
    'test-coverage',
  ]
  for (const dir of skillDirs) {
    const skill = read(path.join(dir, 'SKILL.md'))
    for (const s of [...resident, ...slugs]) {
      assert.ok(skill.includes(s), `${dir}/SKILL.md must keep "${s}" resident`)
    }
  }
})

test('external contract blocks stay exact in both mirrors', () => {
  const exactBlocks = [
    {
      label: 'tagless commit + Debt trailers',
      text: `git commit \\
  -m "<type(scope): specific debt cleanup>" \\
  -m "Debt-Area: <top-level module or package path, e.g. services/bossd/internal/session>
Debt-Category: <exactly one canonical slug: dead-code | duplication | error-handling | security | complexity-hotspot | type-safety | docs-drift | portability | guardrail | test-coverage>"`,
    },
    {
      label: 'pushed commit does not create PR rule',
      text: `A cron run has no human watching. A pushed commit alone does NOT create a PR, and a draft
PR is not shippable.`,
    },
    {
      // BOS-640: the draft->ready spine moved into skills-toolbox/sweep-pr-gate.sh, where its
      // exact bytes are pinned by scripts/sweep-pr-gate.test.mjs. What stays pinned HERE is the
      // invocation — the caller's whole interface to the gate: the three env inputs, the
      // single captured scalar, and the `test -n` that turns an empty capture back into a
      // non-zero block. A helper that is never invoked readies no PR; a gate whose failure
      // the caller swallows readies no PR either, and says so nowhere.
      //
      // ORDER IS LOAD-BEARING. The fence has no `set -e`, so the block's exit status is its
      // LAST command's. `test -n` must therefore come last: with `export PR_NUMBER` after it
      // the guard is inert (`export` always exits 0) and the block reports success on a gate
      // that never created a PR.
      // BOS-653 added START_SHA: the gate's branch-safety guard refuses to retag, force-push
      // and ready a PR on a branch that already carried non-placeholder commits at START_SHA,
      // and START_SHA is the caller's to supply — Phase 0 already computes it.
      label: 'PR gate helper invocation',
      text: `PR_NUMBER="$(SESSION_BRANCH="$SESSION_BRANCH" START_SHA="$START_SHA" BASE_BRANCH="$BASE_BRANCH" PR_BODY="$PR_BODY" \\
  bash "$(git rev-parse --show-toplevel)/skills-toolbox/sweep-pr-gate.sh")"
rm -f "$PR_BODY"
export PR_NUMBER
test -n "$PR_NUMBER"`,
    },
    {
      label: 'NO_CHANGE output',
      text: `NO_CHANGE
Reason: no high-confidence bounded debt fix found this run.
Categories excluded (recent Debt-Category): <list>
Areas excluded (recent Debt-Area): <list>
Surveyed categories: <>=2 non-excluded, e.g. dead-code, error-handling>
Surveyed areas: <>=3 areas, e.g. services/bossd/internal/session, services/web/src/lib, plugins/bossd-plugin-repair>
Top rejected candidates:
- <file/path> (<category>): <specific reason rejected>
- <file/path> (<category>): <specific reason rejected>
- <file/path> (<category>): <specific reason rejected>
Next run: rotate to a different neglected category and area.`,
    },
    {
      label: 'READY_GREEN_PR output',
      text: `READY_GREEN_PR
PR: <url>
Candidate: <short title>
Area: <Debt-Area>
Category: <Debt-Category>
Files changed:
- <path>
Verification:
- <command>: pass
Branch: <branch>
Checks: pass`,
    },
  ]

  for (const dir of skillDirs) {
    const skill = read(path.join(dir, 'SKILL.md'))
    for (const block of exactBlocks) {
      assertExactBlock(skill, block.text, `${block.label} in ${dir}`)
    }
  }
})

// ---------------------------------------------------------------------------
// Dispatch + sentinel language — the subagent isolation landed (source skill).
// ---------------------------------------------------------------------------

test('the three subagent dispatches + sentinel language are present', () => {
  for (const token of ['SURVEY_RESULT', 'FIX_RESULT', 'CHECK_RESULT']) {
    assert.match(CLAUDE_SKILL, new RegExp(token), `SKILL.md must document the ${token} sentinel`)
  }
  assert.match(CLAUDE_SKILL, /bs-run-sentinel/, 'must resolve the shared sentinel helper')
  assert.match(CLAUDE_SKILL, /run-file\s+sentinel (only|only\b)|from\s+the (run-file )?sentinel/i)
  assert.match(CLAUDE_SKILL, /bounded/i, 'watch must be a bounded poll (watchdog hardening)')
  assert.match(
    CLAUDE_SKILL,
    /Bulk-output\s+discipline/,
    'the bulk-output rule block must be present',
  )
  assert.match(CLAUDE_SKILL, /cheap\s+tier/i, 'the survey tier annotation must be present')
  assert.match(CLAUDE_SKILL, /Opus/, 'the fix/watch tier annotation must be present')
  assert.match(
    CLAUDE_SKILL,
    /never.{0,40}run_in_background|run_in_background.{0,40}(subagent|await)/i,
  )
})

test('survey dispatch requires focus plus extra non-excluded categories and reports breadth', () => {
  for (const dir of skillDirs) {
    const skill = read(path.join(dir, 'SKILL.md'))
    const dispatch = read(path.join(dir, 'references/subagent-dispatch.md'))
    for (const body of [skill, dispatch]) {
      assert.match(
        body,
        /focus-category\s+detector\s+plus\s+additional\s+non-excluded\s+category\s+detectors/i,
        `${dir} must require detectors beyond the category focus`,
      )
      assert.match(body, /surveyedCategories/, `${dir} must include surveyedCategories payload`)
      assert.match(body, /surveyedAreas/, `${dir} must include surveyedAreas payload`)
    }
  }
})

test('a missing/stale sentinel is a distinct dispatch-failure on the safe non-green branch', () => {
  assert.equal(DISPATCH_FAILURE, 'dispatch-failure')
  assert.ok(CLAUDE_SKILL.includes(DISPATCH_FAILURE), 'SKILL.md must use the dispatch-failure token')
  // The shell constant must be pinned byte-identical to the shared module constant.
  assert.ok(
    CLAUDE_SKILL.includes(`DISPATCH_FAILURE="${DISPATCH_FAILURE}"`),
    'the shell DISPATCH_FAILURE must equal the module constant',
  )
  assert.match(CLAUDE_SKILL, /missing\/stale|missing\s+or\s+stale/i)
  assert.match(CLAUDE_SKILL, /never .{0,30}READY_GREEN_PR|never `READY_GREEN_PR`/i)
})

test('green is re-verified by a cheap gh call, never trusted from the sentinel alone', () => {
  assert.match(CLAUDE_SKILL, /re-verif/i, 'green must be re-verified')
  assert.ok(CLAUDE_SKILL.includes('gh pr checks'), 'green re-verify uses a cheap gh pr checks call')
})

test('cron gate exists and uses shared open-PR suppression', () => {
  for (const dir of skillDirs) {
    const gate = read(path.join(dir, 'gate/gate.mjs'))
    const skill = read(path.join(dir, 'SKILL.md'))
    assert.match(gate, /cron-open-pr\.mjs/, `${dir} gate must use shared cron-open-pr helper`)
    assert.match(gate, /Bossanova[ ]sweep[ ]debt/, `${dir} gate must match the live cron name`)
    assert.match(gate, /prior[ ]sweep[ ]PR[ ]still[ ]open/, `${dir} skip reason must be loud`)
    assert.match(gate, /gateExit\(false/, `${dir} gh errors must fail closed`)
    assert.match(skill, /GateCommand/, `${dir}/SKILL.md must document the gate command`)
    assert.doesNotMatch(skill, /Intentionally\s+ungated/, `${dir}/SKILL.md must not say ungated`)
    assert.doesNotMatch(
      skill,
      /Leave `GateCommand` empty/,
      `${dir}/SKILL.md must not tell operators to leave the gate empty`,
    )
  }
  assert.equal(
    hasOpenCronPR(
      [{ headRefName: 'cron-bossanova-sweep-debt-1780000000' }],
      ['Bossanova sweep debt', 'bs-sweep-debt'],
    ),
    true,
  )
  assert.equal(hasOpenCronPR([], ['Bossanova sweep debt', 'bs-sweep-debt']), false)
})

test('the codex mirror carries the same dispatch + sentinel tokens (no un-synced drift)', () => {
  const codex = read('.codex/skills/bs-sweep-debt/SKILL.md')
  for (const token of ['SURVEY_RESULT', 'FIX_RESULT', 'CHECK_RESULT', DISPATCH_FAILURE]) {
    assert.ok(codex.includes(token), `codex mirror must carry the ${token} token`)
  }
})

// ---------------------------------------------------------------------------
// Loss parity — the cheap-tier survey extraction drops no detector finding.
// ---------------------------------------------------------------------------

test('survey loss check: every detector finding surfaces as a candidate (no drop)', () => {
  const fixtureDir = 'scripts/fixtures/bs-sweep-debt'
  const output = read(path.join(fixtureDir, 'detector-output.txt'))
  const expected = JSON.parse(read(path.join(fixtureDir, 'expected-candidates.json')))

  const surfaced = parseDetectorFindings(output)
  const surfacedKeys = new Set(surfaced.map(candidateKey))

  for (const cand of expected) {
    assert.ok(
      surfacedKeys.has(candidateKey(cand)),
      `survey dropped a detector finding: ${candidateKey(cand)}`,
    )
  }
  // Sanity: the fixture spans >=3 findings across >=2 categories (the breadth the survey needs).
  assert.ok(expected.length >= 3, 'fixture must carry >=3 findings')
  assert.ok(new Set(expected.map((c) => c.category)).size >= 2, 'fixture must span >=2 categories')
})

// ---------------------------------------------------------------------------
// Detector wiring (BOS-525) — the file-size detector the complexity-hotspot
// playbook points at exists, and its threshold knob is not silently inert.
// ---------------------------------------------------------------------------

test('the debt-filesize detector exists and its threshold knob actually rewrites the config', () => {
  const makefile = read('Makefile')
  const toml = read('scripts/debt/revive-filesize.toml')

  assert.match(makefile, /^debt-filesize-\$\(2\):$/m, 'define-debt-targets must define the target')
  assert.match(
    makefile,
    /^DEBT_FILESIZE_THRESHOLD \?= (\d+)$/m,
    'the threshold must be overridable',
  )
  assert.match(makefile, /REVIVE_PKG\s*:=/, 'the revive tool must be pinned in a variable')
  assert.match(toml, /\[rule\.file-length-limit\]/, 'the detector config must carry the rule')

  // The recipe seds DEBT_FILESIZE_THRESHOLD over the committed default. Replay that exact
  // expression here: a pattern that no longer matches the config would leave the knob inert
  // (the detector would silently keep reporting against the committed default forever).
  const recipe = /^debt-filesize-\$\(2\):$[\s\S]*?^endef$/m.exec(makefile)
  assert.ok(recipe, 'the debt-filesize recipe must live inside define-debt-targets')
  const sed = /sed 's\/(.+)\/(.+)\/'/.exec(recipe[0])
  assert.ok(sed, 'the debt-filesize recipe must carry a sed rewrite of the threshold')
  const pattern = new RegExp(sed[1].replace(/\\\(/g, '(').replace(/\\\)/g, ')'))
  const replacement = sed[2].replace('\\1', '$1').replace('$$(DEBT_FILESIZE_THRESHOLD)', '4242')
  const rewritten = toml.replace(pattern, replacement)
  assert.notEqual(rewritten, toml, 'the sed pattern no longer matches revive-filesize.toml')
  assert.match(rewritten, /max = 4242/, 'the rewrite must land on the file-length-limit max')

  // The committed default must equal the Makefile default, or `make debt-filesize-*` and a
  // bare `revive -config scripts/debt/revive-filesize.toml` would disagree.
  const makeDefault = /^DEBT_FILESIZE_THRESHOLD \?= (\d+)$/m.exec(makefile)[1]
  const tomlDefault = /max = (\d+)/.exec(toml)[1]
  assert.equal(tomlDefault, makeDefault, 'toml max must equal the Makefile threshold default')

  // The recipe's `|| true` makes the detector non-blocking, which also means revive exiting
  // non-zero (unparseable config) presents as "0 findings" — and revive DISABLES
  // file-length-limit when max <= 0, so a bare `0` silences it while looking green. Three
  // guards keep that from becoming a silent all-clear. Pin each guard TOGETHER WITH the
  // `exit 1` it must reach: matching the condition alone would still pass a guard whose body
  // was emptied, or one whose failing half was deleted along with its consequent.
  assert.match(
    recipe[0],
    /case "\$\$\(DEBT_FILESIZE_THRESHOLD\)" in ''\|\*\[!0-9\]\*\|0\*\)[\s\S]{0,220}?exit\s+1;; esac/,
    'a non-numeric/leading-zero DEBT_FILESIZE_THRESHOLD must exit 1 before the config is built',
  )
  assert.match(
    recipe[0],
    /grep -q '\^\\\[rule\\\.file-length-limit\\\]'[\s\S]{0,80}?grep -qE "max = \$\$\(DEBT_FILESIZE_THRESHOLD\)[\s\S]{0,240}?exit\s+1; \}/,
    'a generated config missing the rule or the REQUESTED max must exit 1',
  )
  assert.match(
    recipe[0],
    /trap 'rm -f "\$\$\$\$cfg"' EXIT\s+INT\s+TERM/,
    'the recipe must trap-clean its temp config',
  )
  // The survey parser only understands revive's `default` one-line shape. Fed `friendly`
  // output it returns [] — a 100% silent loss the D8 gate cannot see, because that gate
  // asserts over a static fixture rather than a live run. Pin the formatter to the parser.
  assert.match(
    recipe[0],
    /-formatter\s+default\b/,
    "the recipe must use revive's default formatter; the survey parser silently drops every friendly-shaped finding",
  )
})

// ---------------------------------------------------------------------------
// Tier→model wiring (BOS-323) — the cheap survey leg runs on Sonnet, guarded on
// $BOSS_AGENT, in BOTH mirrors; the Opus fix/check-watch legs are unchanged.
// ---------------------------------------------------------------------------

// Split subagent-dispatch.md into its `## ` phase sections and return the one whose
// heading text starts with `prefix` (e.g. "Phase 3", "Phase 6", "Phase 8").
function dispatchSection(body, prefix) {
  return body.split(/\n## /).find((section) => section.startsWith(prefix))
}

test('the cheap survey dispatch carries a provider-guarded sonnet model directive (both mirrors)', () => {
  for (const dir of skillDirs) {
    const dispatch = read(path.join(dir, 'references/subagent-dispatch.md'))
    const survey = dispatchSection(dispatch, 'Phase 3')
    assert.ok(survey, `${dir} must have a Phase 3 survey section`)
    // A real provider-guarded model directive: model:, the sonnet alias, and a
    // $BOSS_AGENT guard naming both the lowercase `claude` and `codex-omit` cases.
    assert.match(survey, /model:/, `${dir} Phase 3 must carry a model: directive`)
    assert.match(survey, /sonnet/, `${dir} Phase 3 survey must dispatch on the sonnet alias`)
    assert.match(
      survey,
      /\$BOSS_AGENT/,
      `${dir} Phase 3 model directive must be $BOSS_AGENT-guarded`,
    )
    assert.match(survey, /\bclaude\b/, `${dir} Phase 3 guard must reference lowercase claude`)
    assert.match(survey, /\bcodex\b/, `${dir} Phase 3 guard must cover the codex-omit case`)

    // The Opus legs (Phase 6 fix, Phase 8 check-watch) must NOT gain a sonnet directive.
    const fix = dispatchSection(dispatch, 'Phase 6')
    const checkWatch = dispatchSection(dispatch, 'Phase 8')
    assert.ok(fix, `${dir} must have a Phase 6 fix section`)
    assert.ok(checkWatch, `${dir} must have a Phase 8 check-watch section`)
    assert.ok(
      !fix.includes('sonnet'),
      `${dir} Phase 6 fix (Opus) must not carry a sonnet directive`,
    )
    assert.ok(
      !checkWatch.includes('sonnet'),
      `${dir} Phase 8 check-watch (Opus) must not carry a sonnet directive`,
    )
  }
})

// ---------------------------------------------------------------------------
// Ratchet — the always-resident body stays under the post-split ceiling.
// ---------------------------------------------------------------------------

test('the always-resident body is pinned at its exact post-split size', () => {
  // Measured post-split resident bodies: 29635 B (.claude) / 29718 B (.codex), down from the
  // 32575 B (.claude) / 32658 B (.codex) pre-split baseline. CEILING = 30 KiB gives ~0.9 KiB of
  // headroom above the larger mirror and stays ~1.8 KiB below the 32575 B baseline — regrowth
  // past it must move situational content into a reference, not back into the body.
  //
  // Bumped 30720 → 30784 for BOS-633: the mandatory `disable-model-invocation: true`
  // frontmatter key costs every sweep 29 B, and the .codex mirror had 1 B of headroom. A
  // frontmatter key is not situational content — there is no reference to move it into — so
  // the ceiling absorbs exactly that key rather than the gate forcing a body edit. Body
  // regrowth still has to pay for itself.
  //
  // Lowered 30784 → 30302 for BOS-640: the 22-line PR-gate spine moved into
  // skills-toolbox/sweep-pr-gate.sh, taking 30669 → 30158 B (.claude) / 30748 → 30238 B
  // (.codex). The ceiling is the larger mirror + 64 B, so the extraction's saving is actually
  // banked rather than left re-spendable — a ceiling merely "below the old one" would have
  // left ~500 B, most of the saving, silently available.
  //
  // BOS-653 added `START_SHA="$START_SHA" ` to the gate invocation (+23 B, no bump): bodies are
  // 30181 B (.claude) / 30261 B (.codex), leaving 41 B.
  //
  // BOS-768 replaces the ceiling with an exact pin on the AUTHORED source. The ceiling was
  // "larger mirror + 64 B", i.e. slack by construction — 91 B of it by the time this landed,
  // and a trim would only have widened it. The `.codex` copy leaves the byte loop entirely:
  // it is GENERATED, so its size is a function of this file plus a fixed header, and it is
  // verified below by regenerating it and comparing exactly.
  //
  // Only the tighter of the two old upper bounds is kept. `CEILING < 32575` (pre-split) was
  // implied by `CEILING < 30669` (pre-extraction body) and so could never fire on its own.
  // Rebased onto main at 5978bd850: #2090 rewrote the awk whole-record positionals out of the
  // published bodies, growing this one by 110 B. That is a correctness rewrite of code the body
  // must carry, not new prose, so the pin absorbs exactly it.
  const SOURCE_BYTES = 30321 // exact measured .claude body, re-measured 2026-08-19

  // Seven separate gates in this file index or iterate skillDirs, and this pin reads
  // skillDirs[0]. A list that silently shortened would leave every one of them asserting
  // against a single directory with nothing going red to say the other stopped being
  // checked — the vacuity assertArtifactSet exists for.
  assertArtifactSet(skillDirs, 2, 'skillDirs')

  assertExactSize({
    below: { name: 'PRE_EXTRACTION_BODY', value: 30669 },
    constFile: 'scripts/bs-sweep-debt-skill.test.mjs',
    constName: 'SOURCE_BYTES',
    expected: SOURCE_BYTES,
    label: 'bs-sweep-debt always-resident body',
    measured: measureFile(path.join(rootDir, skillDirs[0], 'SKILL.md')),
    path: `${skillDirs[0]}/SKILL.md`,
    residual:
      'the five references/ files this body routes to and the toolbox scripts it invokes — ' +
      'content moved out of the body is invisible to this pin',
  })
})

test('the codex mirror is exactly what regenerating it from the .claude source produces', () => {
  // Replaces the old byte ceiling on the mirror. `make codex-skills` unconditionally prepends
  // a generated-by header, so a healthy mirror is ALWAYS larger than its source — "larger than
  // source" can never be the tell. Exact regeneration equality is, and it subsumes size.
  assertMirrorRegenerated({
    mirrorPath: path.join(rootDir, skillDirs[1], 'SKILL.md'),
    regenerate: rewriteClaudeSkillMarkdown,
    sourcePath: path.join(rootDir, skillDirs[0], 'SKILL.md'),
  })
})
