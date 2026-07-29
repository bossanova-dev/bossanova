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
      label: 'PR ready check',
      text: `if [ "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "true" ]; then
  gh pr ready "$PR_NUMBER"
fi
test "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "false"`,
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
  assert.match(CLAUDE_SKILL, /run-file sentinel (only|only\b)|from the (run-file )?sentinel/i)
  assert.match(CLAUDE_SKILL, /bounded/i, 'watch must be a bounded poll (watchdog hardening)')
  assert.match(CLAUDE_SKILL, /Bulk-output discipline/, 'the bulk-output rule block must be present')
  assert.match(CLAUDE_SKILL, /cheap tier/i, 'the survey tier annotation must be present')
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
        /focus-category detector plus additional non-excluded category detectors/i,
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
  assert.match(CLAUDE_SKILL, /missing\/stale|missing or stale/i)
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
    assert.match(gate, /Bossanova sweep debt/, `${dir} gate must match the live cron name`)
    assert.match(gate, /prior sweep PR still open/, `${dir} skip reason must be loud`)
    assert.match(gate, /gateExit\(false/, `${dir} gh errors must fail closed`)
    assert.match(skill, /GateCommand/, `${dir}/SKILL.md must document the gate command`)
    assert.doesNotMatch(skill, /Intentionally ungated/, `${dir}/SKILL.md must not say ungated`)
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
    /case "\$\$\(DEBT_FILESIZE_THRESHOLD\)" in ''\|\*\[!0-9\]\*\|0\*\)[\s\S]{0,220}?exit 1;; esac/,
    'a non-numeric/leading-zero DEBT_FILESIZE_THRESHOLD must exit 1 before the config is built',
  )
  assert.match(
    recipe[0],
    /grep -q '\^\\\[rule\\\.file-length-limit\\\]'[\s\S]{0,80}?grep -qE "max = \$\$\(DEBT_FILESIZE_THRESHOLD\)[\s\S]{0,240}?exit 1; \}/,
    'a generated config missing the rule or the REQUESTED max must exit 1',
  )
  assert.match(
    recipe[0],
    /trap 'rm -f "\$\$\$\$cfg"' EXIT INT TERM/,
    'the recipe must trap-clean its temp config',
  )
  // The survey parser only understands revive's `default` one-line shape. Fed `friendly`
  // output it returns [] — a 100% silent loss the D8 gate cannot see, because that gate
  // asserts over a static fixture rather than a live run. Pin the formatter to the parser.
  assert.match(
    recipe[0],
    /-formatter default\b/,
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

test('the always-resident body stays under the post-split ratchet', () => {
  // Measured post-split resident bodies: 29635 B (.claude) / 29718 B (.codex), down from the
  // 32575 B (.claude) / 32658 B (.codex) pre-split baseline. CEILING = 30 KiB gives ~0.9 KiB of
  // headroom above the larger mirror and stays ~1.8 KiB below the 32575 B baseline — regrowth
  // past it must move situational content into a reference, not back into the body.
  const CEILING = 30720
  assert.ok(CEILING < 32575, 'ceiling must stay below the pre-split baseline')
  for (const dir of skillDirs) {
    const bytes = Buffer.byteLength(read(path.join(dir, 'SKILL.md')), 'utf8')
    assert.ok(
      bytes < CEILING,
      `${dir}/SKILL.md must stay under ${CEILING} bytes (post-split ratchet), got ${bytes} — move situational content into a reference`,
    )
  }
})
