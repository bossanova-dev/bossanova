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
import { DISPATCH_FAILURE } from './bs-run-sentinel.mjs'
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

function assertExactBlock(skill, block, label) {
  assert.ok(skill.includes(block), `SKILL.md must keep exact ${label} block resident`)
}

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
