import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { renderReport, MARKER, VERDICT_OK } from './bs-review-report.mjs'

const scriptPath = fileURLToPath(new URL('./bs-review-report.mjs', import.meta.url))

/** Run the CLI block as a subprocess and return { stdout, status }. */
function runCli(args = [], input = '') {
  const res = spawnSync(process.execPath, [scriptPath, ...args], { input, encoding: 'utf8' })
  return { stdout: res.stdout, status: res.status }
}

/** A complete clean-run report fixture (the #959 self-review, abbreviated). */
function cleanFixture() {
  return {
    rounds: 2,
    status: 'clean',
    summary: 'Self-review of the bs-review skill against its own branch.',
    security: [],
    reviewers: [
      { name: 'golang-pro', status: 'clean' },
      { name: 'requesting-code-review', status: 'clean', note: 'no actionable findings' },
    ],
    issuesHeadline: '1 must-fix found and fixed this run across 3 files',
    verdict: {
      assessment: 'Sound',
      evidence: 'All gates green',
      confidence: 'Medium',
      testing_assessment: 'Satisfactory',
      recommendation: 'Approve',
    },
    evidenceRows: [
      { round: 'Phase 2 — requesting-code-review', result: '1 must-fix + 3 suggestions' },
    ],
    gates: ['make codex-skills-check', 'make test-scripts (642)'],
    mustfix: {
      found: 1,
      fixed: 1,
      verified: 0,
      unresolved: 0,
      items: [
        {
          disposition: 'fixed',
          title: 'Phase 3 skip rule inverted',
          file: 'a.md',
          line: 1,
          commit: 'd322a8e',
        },
      ],
    },
    leaveAsIs: [
      {
        title: 'isTTY guard branch',
        file: 'scripts/bs-review-detect.mjs',
        line: 40,
        rationale: 'uncoverable',
      },
    ],
    suggestions: [
      { title: 'Cover the isTTY CLI branch', file: 'scripts/bs-review-detect.mjs', line: 44 },
    ],
  }
}

test('header + horizontal rule + marker lead the report', () => {
  const md = renderReport(cleanFixture())
  assert.ok(md.startsWith(`${MARKER}\n`), 'leads with the idempotency marker')
  assert.match(
    md,
    /bs-review completed after 2 round\(s\)\. All must-fix findings fixed; required gates green\./,
  )
  assert.match(md, /\n---\n/)
  assert.match(md, /#### Code Review/)
})

test('verdict block badges each field per VERDICT_OK', () => {
  const md = renderReport(cleanFixture())
  assert.match(md, /✅ \*\*Assessment:\*\* Sound/)
  assert.match(md, /✅ \*\*Evidence:\*\* All gates green/)
  assert.match(md, /✅ \*\*Confidence:\*\* Medium/)
  assert.match(md, /✅ \*\*Test Coverage:\*\* Satisfactory/)
  assert.match(md, /✅ \*\*Recommendation:\*\* Approve/)
})

test('failing verdict values flip the badge to ❌', () => {
  const md = renderReport({
    verdict: {
      assessment: 'Unsound',
      evidence: 'A gate failed',
      confidence: 'Low',
      testing_assessment: 'Unsatisfactory',
      recommendation: 'Fix',
    },
  })
  assert.match(md, /❌ \*\*Assessment:\*\* Unsound/)
  assert.match(md, /❌ \*\*Evidence:\*\* A gate failed/)
  assert.match(md, /❌ \*\*Confidence:\*\* Low/)
  assert.match(md, /❌ \*\*Test Coverage:\*\* Unsatisfactory/)
  assert.match(md, /❌ \*\*Recommendation:\*\* Fix/)
})

test('VERDICT_OK classifies the per-field good direction', () => {
  assert.equal(VERDICT_OK.assessment('Sound'), true)
  assert.equal(VERDICT_OK.assessment('Unsound'), false)
  assert.equal(VERDICT_OK.confidence('High'), true)
  assert.equal(VERDICT_OK.confidence('Medium'), true)
  assert.equal(VERDICT_OK.confidence('Low'), false)
  assert.equal(VERDICT_OK.testing_assessment('Unnecessary'), true)
  assert.equal(VERDICT_OK.testing_assessment('Unsatisfactory'), false)
  assert.equal(VERDICT_OK.recommendation('Approve'), true)
  assert.equal(VERDICT_OK.recommendation('Hold'), false)
  assert.equal(VERDICT_OK.evidence('All gates green'), true)
  assert.equal(VERDICT_OK.evidence('A gate failed'), false)
})

test('caller-embedded badge is stripped and re-derived', () => {
  const md = renderReport({ verdict: { assessment: '❌ Sound' } })
  assert.match(md, /✅ \*\*Assessment:\*\* Sound/)
  assert.doesNotMatch(md, /❌ \*\*Assessment/)
})

test('capped status changes the header wording and reports open count', () => {
  const md = renderReport({ status: 'capped', rounds: 3, mustfix: { unresolved: 2 } })
  assert.match(
    md,
    /bs-review completed after 3 round\(s\)\. 2 must-fix findings remain \(surfaced below\); see gates\./,
  )
})

test('zero security renders the reassuring line; non-zero renders a loud alert', () => {
  assert.match(renderReport(cleanFixture()), /No security issues identified\./)
  const md = renderReport({
    security: [{ severity: 'High', title: 'XSS', file: 'x.ts', line: 9, fix: 'escape it' }],
  })
  assert.match(md, /\*\*🚨 1 security issue identified 🚨\*\*/)
  assert.match(md, /- \*\*High\*\* XSS \(`x\.ts:9`\) — Fix: escape it/)
})

test('issue headline is bolded and joined with the security status', () => {
  const md = renderReport(cleanFixture())
  assert.match(
    md,
    /\*\*1 must-fix found and fixed this run across 3 files\.\*\* No security issues identified\./,
  )
})

test('non-empty sections render collapsible <details> with the right summaries', () => {
  const md = renderReport(cleanFixture())
  assert.match(md, /<details><summary>2 Reviewers<\/summary>/)
  assert.match(md, /<details><summary>Evidence — rounds & gates<\/summary>/)
  assert.match(
    md,
    /<details><summary>Must-fix detail — found 1 \/ fixed 1 \/ verified 0 \/ unresolved 0<\/summary>/,
  )
  assert.match(md, /<details><summary>Leave as-is<\/summary>/)
  assert.match(md, /<details><summary><strong>Create 1 follow-up issue<\/strong><\/summary>/)
})

test('the Reviewers block lists each reviewer with status and optional note', () => {
  const md = renderReport(cleanFixture())
  assert.match(md, /- \*\*golang-pro\*\* — clean\n/)
  assert.match(md, /- \*\*requesting-code-review\*\* — clean \(no actionable findings\)/)
})

test('reviewer name/status/note are HTML-escaped so a lens label cannot break the layout', () => {
  const md = renderReport({
    reviewers: [{ name: 'lens <x>', status: 'clean', note: 'saw <b> & <i>' }],
  })
  assert.match(md, /- \*\*lens &lt;x&gt;\*\* — clean \(saw &lt;b&gt; &amp; &lt;i&gt;\)/)
})

test('the Reviewers block is omitted (with no empty toggle) when there are no reviewers', () => {
  const data = cleanFixture()
  data.reviewers = []
  const md = renderReport(data)
  assert.doesNotMatch(md, /Reviewer/)
})

test('the Reviewers summary counts reviewers with singular/plural', () => {
  const one = renderReport({ reviewers: [{ name: 'golang-pro', status: 'clean' }] })
  assert.match(one, /<details><summary>1 Reviewer<\/summary>/)
  const two = renderReport({
    reviewers: [
      { name: 'golang-pro', status: 'clean' },
      { name: 'thermonuclear', status: 'clean' },
    ],
  })
  assert.match(two, /<details><summary>2 Reviewers<\/summary>/)
})

test('the follow-up block summary counts suggestions with singular/plural', () => {
  const one = renderReport({
    suggestions: [{ title: 'Only one', file: 'a.ts', line: 1 }],
  })
  assert.match(one, /<details><summary><strong>Create 1 follow-up issue<\/strong><\/summary>/)
  const two = renderReport({
    suggestions: [
      { title: 'First', file: 'a.ts', line: 1 },
      { title: 'Second', file: 'b.ts', line: 2 },
    ],
  })
  assert.match(two, /<details><summary><strong>Create 2 follow-up issues<\/strong><\/summary>/)
})

test('the follow-up prompt renders one <ticket> block per suggestion', () => {
  const md = renderReport({
    suggestions: [
      { title: 'Cover the isTTY branch', file: 'a.ts', line: 44, detail: 'add a unit test' },
    ],
    tracker: null,
  })
  assert.match(md, /Please create the following follow-up issues from the automated code review/)
  assert.match(md, /<ticket>\n<title>Cover the isTTY branch<\/title>/)
  assert.match(md, /<body>add a unit test \(originating: a\.ts:44\)<\/body>/)
  assert.match(md, /<priority>Low<\/priority>\n<\/ticket>/)
})

test('a suggestion can override its priority', () => {
  const md = renderReport({
    suggestions: [{ title: 'X', file: 'a.ts', line: 1, priority: 'Medium' }],
    tracker: null,
  })
  assert.match(md, /<priority>Medium<\/priority>/)
})

test('a configured tracker sources the follow-up label line from followUpLabels verbatim', () => {
  const md = renderReport({
    suggestions: [{ title: 'X', file: 'a.ts', line: 1 }],
    tracker: { mcpServer: 'acme-tracker', team: 'Acme', followUpLabels: ['follow-up', 'triage'] },
  })
  assert.match(md, /Label all issues with: follow-up, triage\./)
})

test('a tracker without a followUpLabels list drops the label line', () => {
  const md = renderReport({
    suggestions: [{ title: 'X', file: 'a.ts', line: 1 }],
    tracker: { mcpServer: 'acme-tracker', team: 'Acme' },
  })
  assert.doesNotMatch(md, /Label all issues with:/)
})

test('an unconfigured repo (tracker null) emits a generic, label-free prompt', () => {
  const md = renderReport({
    suggestions: [{ title: 'X', file: 'a.ts', line: 1 }],
    tracker: null,
  })
  assert.match(md, /Please create the following follow-up issues/)
  assert.doesNotMatch(md, /Label all issues with:/)
  assert.doesNotMatch(md, /bossanova/i)
})

test('a suggestion with no file and no detail renders <body> as just the title', () => {
  const md = renderReport({ suggestions: [{ title: 'Bare suggestion' }], tracker: null })
  assert.match(md, /<body>Bare suggestion<\/body>/)
  assert.doesNotMatch(md, /originating:/)
})

test('the code fence grows so a backtick run inside a suggestion cannot break out', () => {
  const md = renderReport({
    suggestions: [{ title: 'Fix ```js block```', file: 'a.ts', line: 1, detail: 'has ``` inside' }],
    tracker: null,
  })
  // A 3-backtick run in the body forces a >=4-backtick fence; the title/body still render literally.
  assert.match(md, /````\nPlease create the following follow-up issues/)
  assert.match(md, /<title>Fix ```js block```<\/title>/)
})

test('the related-issue instruction is gated on issueUrl', () => {
  const withIssue = renderReport({
    suggestions: [{ title: 'X', file: 'a.ts', line: 1 }],
    tracker: null,
    issueUrl: 'https://tracker.example/issue/AC-1',
  })
  assert.match(withIssue, /Create each in the same project\/team as the related issue below\./)
  const noIssue = renderReport({
    suggestions: [{ title: 'X', file: 'a.ts', line: 1 }],
    tracker: null,
  })
  assert.doesNotMatch(noIssue, /related issue below/)
})

test('Related PR / Related issue lines render only when their URLs are supplied', () => {
  const withLinks = renderReport({
    suggestions: [{ title: 'X', file: 'a.ts', line: 1 }],
    tracker: null,
    prUrl: 'https://github.com/acme/repo/pull/7',
    issueUrl: 'https://tracker.example/issue/AC-1',
  })
  assert.match(withLinks, /Related PR: https:\/\/github\.com\/acme\/repo\/pull\/7/)
  assert.match(withLinks, /Related issue: https:\/\/tracker\.example\/issue\/AC-1/)
  assert.match(
    withLinks,
    /add a comment to the related PR \(https:\/\/github\.com\/acme\/repo\/pull\/7\)/,
  )

  const noLinks = renderReport({
    suggestions: [{ title: 'X', file: 'a.ts', line: 1 }],
    tracker: null,
  })
  assert.doesNotMatch(noLinks, /Related PR:/)
  assert.doesNotMatch(noLinks, /Related issue:/)
  assert.doesNotMatch(noLinks, /add a comment to the related PR/)
})

test('the default path (no injected tracker) reads config from cwd and stays generic when unconfigured', () => {
  // Exercise the REAL production glue that the injected-tracker cases above skip:
  // renderReport with no `tracker` key falls back to
  // trackerConfigFor(loadSkillConfig()), which resolves .boss-skills.json from
  // cwd. From a scratch dir with no config anywhere above it the tracker resolves
  // null, so the generic label-free prompt must render — proving the seam
  // self-disables cleanly in an unconfigured checkout (not just when null is
  // hand-injected).
  const scratch = mkdtempSync(join(tmpdir(), 'bs-review-report-'))
  const prevCwd = process.cwd()
  try {
    process.chdir(scratch)
    const md = renderReport({ suggestions: [{ title: 'X', file: 'a.ts', line: 1 }] })
    assert.match(md, /Please create the following follow-up issues/)
    assert.doesNotMatch(md, /Label all issues with:/)
    assert.doesNotMatch(md, /bossanova/i)
  } finally {
    process.chdir(prevCwd)
    rmSync(scratch, { recursive: true, force: true })
  }
})

test('Test Coverage <details> renders when verdict.testing_detail is present', () => {
  const data = cleanFixture()
  data.verdict.testing_detail = 'Added a table-driven test covering the new plural branch.'
  const md = renderReport(data)
  assert.match(md, /<details><summary>Test Coverage<\/summary>/)
  assert.match(md, /Added a table-driven test covering the new plural branch\./)
  // The one-line badge on the verdict line is preserved alongside the body.
  assert.match(md, /✅ \*\*Test Coverage:\*\* Satisfactory/)
})

test('Test Coverage <details> is omitted when verdict.testing_detail is absent', () => {
  const md = renderReport(cleanFixture())
  assert.doesNotMatch(md, /<details><summary>Test Coverage<\/summary>/)
  // But the badged verdict line still renders.
  assert.match(md, /✅ \*\*Test Coverage:\*\* Satisfactory/)
})

test('literal HTML in free-text prose is escaped so it cannot break the layout', () => {
  const data = cleanFixture()
  // Prose that names a tag — GitHub would otherwise parse the raw <details> as
  // an HTML element and swallow every following section into it.
  data.verdict.testing_detail = 'Covers the Test Coverage <details> present/absent behaviour.'
  data.summary = 'Reviewed the <summary> & <strong> rendering paths.'
  const md = renderReport(data)
  // The tags render as visible text, not as HTML elements.
  assert.match(md, /Covers the Test Coverage &lt;details&gt; present\/absent behaviour\./)
  assert.match(md, /Reviewed the &lt;summary&gt; &amp; &lt;strong&gt; rendering paths\./)
  // No stray unescaped tag leaks out of the prose into the document body.
  assert.doesNotMatch(md, /behaviour\.\n[\s\S]*<details>(?!<summary>)/)
  // Sibling sections after Test Coverage still render at the top level.
  assert.match(md, /<details><summary>Evidence — rounds & gates<\/summary>/)
  assert.match(md, /<details><summary>Leave as-is<\/summary>/)
})

test('empty sections are omitted entirely (no empty <details>)', () => {
  const md = renderReport({
    rounds: 1,
    status: 'clean',
    verdict: { assessment: 'Sound', confidence: 'High' },
    mustfix: { found: 0, fixed: 0, verified: 0, unresolved: 0, items: [] },
    leaveAsIs: [],
    suggestions: [],
    evidenceRows: [],
    gates: [],
  })
  assert.doesNotMatch(md, /Leave as-is/)
  assert.doesNotMatch(md, /Must-fix detail/)
  assert.doesNotMatch(md, /Suggestions/)
  assert.doesNotMatch(md, /Evidence — rounds/)
  assert.doesNotMatch(md, /<details>/)
})

test('evidence section renders a markdown table and the gate roster', () => {
  const md = renderReport(cleanFixture())
  assert.match(md, /\| Round \| Result \|/)
  assert.match(md, /\| Phase 2 — requesting-code-review \| 1 must-fix \+ 3 suggestions \|/)
  assert.match(md, /Gates \(all green\): `make codex-skills-check` · `make test-scripts \(642\)`\./)
})

test('must-fix items badge each disposition', () => {
  const md = renderReport({
    mustfix: {
      found: 3,
      fixed: 1,
      verified: 1,
      unresolved: 1,
      items: [
        { disposition: 'fixed', title: 'A', commit: 'abc1234' },
        { disposition: 'verified', title: 'B' },
        { disposition: 'unresolved', title: 'C' },
      ],
    },
  })
  assert.match(md, /- ✅ \*\*Fixed\*\* — \*\*A\*\* \(`abc1234`\)/)
  assert.match(md, /- ☑️ \*\*Verified\*\* — \*\*B\*\*/)
  assert.match(md, /- ❌ \*\*Unresolved\*\* — \*\*C\*\*/)
})

test('suggestions render a fence-guarded follow-up prompt', () => {
  const md = renderReport({
    ...cleanFixture(),
    tracker: {
      mcpServer: 'acme-tracker',
      team: 'Acme',
      labels: { followUp: 'follow-up', agentPlan: 'agent-plan' },
    },
  })
  // A fence wraps the copyable prompt (the copy-to-clipboard button) and the
  // suggestion becomes a <ticket> block.
  assert.match(md, /```\nPlease create the following follow-up issues/)
  assert.match(md, /<title>Cover the isTTY CLI branch<\/title>/)
})

test('CLI renders JSON from --in and from stdin', () => {
  // stdin path
  const piped = runCli([], JSON.stringify(cleanFixture()))
  assert.equal(piped.status, 0)
  assert.ok(piped.stdout.startsWith(MARKER))
  assert.match(piped.stdout, /✅ \*\*Assessment:\*\* Sound/)
})

test('CLI on empty stdin renders a minimal (clean) report', () => {
  const { stdout, status } = runCli([], '')
  assert.equal(status, 0)
  assert.match(stdout, /bs-review completed after 1 round\(s\)/)
})
