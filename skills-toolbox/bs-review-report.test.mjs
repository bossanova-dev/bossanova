import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  renderReport,
  MARKER,
  VERDICT_OK,
  parseRoundState,
  nextReviewRoundMode,
  carriedReviewClaims,
  carriedReviewObservations,
  validateReviewClaim,
  reviewerInputByteTotals,
} from './bs-review-report.mjs'

const scriptPath = fileURLToPath(new URL('./bs-review-report.mjs', import.meta.url))

const cleanLedger = {
  discovered: 2,
  completed: 2,
  skipped: 0,
  timedOut: 0,
  notReached: 0,
}

/** Run the CLI block as a subprocess and return { stdout, stderr, status }. */
function runCli(args = [], input = '') {
  const res = spawnSync(process.execPath, [scriptPath, ...args], { input, encoding: 'utf8' })
  return { stdout: res.stdout, stderr: res.stderr, status: res.status }
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
    panel: {
      initial: ['golang-pro', 'requesting-code-review', 'cross-model'],
      reviewers: ['golang-pro', 'requesting-code-review'],
    },
    agreement: {
      panelSize: 2,
      initialPanelSize: 3,
      terminalPanel: ['golang-pro', 'requesting-code-review'],
      initialPanel: ['golang-pro', 'requesting-code-review', 'cross-model'],
      panelShrank: true,
      uncorroboratedMustFixCount: 0,
      vanishedFindings: [],
    },
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
    invalid: [],
    ledger: cleanLedger,
    leaveAsIs: [
      {
        title: 'isTTY guard branch',
        file: 'scripts/bs-review-detect.mjs',
        line: 40,
        rationale: 'uncoverable',
        evidence: 'Read scripts/bs-review-detect.mjs:40; the branch is terminal-only.',
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

test('contradicted clean report with invalid evidence renders capped header and problem badges', () => {
  const md = renderReport({
    ...cleanFixture(),
    status: 'clean',
    rounds: 2,
    invalid: [{ reason: 'findings-lens-0-go.json: unexpected end of JSON input' }],
    verdict: {
      assessment: 'Sound',
      evidence: 'All gates green',
      confidence: 'High',
      testing_assessment: 'Satisfactory',
      recommendation: 'Approve',
    },
  })
  assert.match(
    md,
    /bs-review completed after 2 round\(s\)\. 1 invalid entry remains \(surfaced below\); see gates\./,
  )
  assert.doesNotMatch(md, /All must-fix findings fixed; required gates green\./)
  assert.match(md, /❌ \*\*Assessment:\*\* Unsound/)
  assert.match(md, /❌ \*\*Recommendation:\*\* Fix/)
  assert.equal((md.match(/Contradiction notice/g) || []).length, 1)
  assert.match(md, /caller supplied `clean` but report evidence derives `capped`/)
  assert.match(md, /Invalid reviewer findings — repair or re-run required/)
  assert.match(md, /findings-lens-0-go\.json: unexpected end of JSON input/)
})

test('review coverage counts render in the always-visible verdict block', () => {
  const md = renderReport({
    ...cleanFixture(),
    ledger: { discovered: 3, completed: 2, skipped: 0, timedOut: 0, notReached: 1 },
  })
  assert.match(
    md,
    /Review coverage: discovered 3; completed 2; skipped 0; timed-out 0; not-reached 1\./,
  )
  assert.match(md, /❌ \*\*Confidence:\*\* Low/)
})

test('missing ledger evidence fails closed instead of rendering as clean', () => {
  const fixture = cleanFixture()
  delete fixture.ledger
  const md = renderReport(fixture)
  assert.match(md, /❌ \*\*Assessment:\*\* Unsound/)
  assert.match(md, /Contradiction notice/)
  assert.doesNotMatch(md, /All must-fix findings fixed; required gates green\./)
})

test('single-reviewer panel derives Low confidence despite caller-supplied High', () => {
  const md = renderReport({
    ...cleanFixture(),
    panel: { initial: ['only-one'], reviewers: ['only-one'] },
    agreement: {
      panelSize: 1,
      initialPanelSize: 1,
      terminalPanel: ['only-one'],
      initialPanel: ['only-one'],
      panelShrank: false,
      uncorroboratedMustFixCount: 0,
      vanishedFindings: [],
    },
    verdict: { ...cleanFixture().verdict, confidence: 'High' },
  })
  assert.match(md, /❌ \*\*Confidence:\*\* Low/)
  assert.equal((md.match(/Confidence contradiction/g) || []).length, 1)
  assert.match(md, /caller supplied `High` but report evidence derives `Low`/)
  assert.match(md, /#### Code Review/)
  assert.match(md, /<details><summary>Agreement<\/summary>/)
})

test('Agreement section renders panel size, shrink, uncorroborated must-fix and vanished findings', () => {
  const md = renderReport({
    ...cleanFixture(),
    panel: { initial: ['a', 'b', 'c'], reviewers: ['a', 'b'] },
    agreement: {
      panelSize: 2,
      initialPanelSize: 3,
      terminalPanel: ['a', 'b'],
      initialPanel: ['a', 'b', 'c'],
      panelShrank: true,
      uncorroboratedMustFixCount: 1,
      vanishedFindings: [{ file: 'review.go', line: 42, title: 'vanished concern' }],
    },
  })
  assert.match(md, /<details><summary>Agreement<\/summary>/)
  assert.match(md, /Panel: 2 reviewer\(s\) terminal; 3 initial\./)
  assert.match(md, /Terminal panel: `a`, `b`\./)
  assert.match(md, /Initial panel: `a`, `b`, `c`\./)
  assert.match(md, /Panel shrank: yes\./)
  assert.match(md, /Uncorroborated must-fix findings: 1\./)
  assert.match(md, /vanished concern \(`review\.go:42`\)/)
})

test('Agreement section is omitted when panel evidence is absent', () => {
  const data = cleanFixture()
  delete data.panel
  delete data.agreement
  const md = renderReport(data)
  assert.doesNotMatch(md, /<details><summary>Agreement<\/summary>/)
})

test('Low confidence from disagreement renders an explicit escalation line', () => {
  const md = renderReport({
    ...cleanFixture(),
    panel: { initial: ['only-one'], reviewers: ['only-one'] },
    agreement: {
      panelSize: 1,
      initialPanelSize: 1,
      terminalPanel: ['only-one'],
      initialPanel: ['only-one'],
      panelShrank: false,
      uncorroboratedMustFixCount: 0,
      vanishedFindings: [],
    },
    verdict: { ...cleanFixture().verdict, confidence: 'High' },
  })
  assert.match(md, /Escalation: human adjudication needed for single-sample-panel\./)
})

test('vanished findings in agreement derive Low confidence and escalation', () => {
  const md = renderReport({
    ...cleanFixture(),
    panel: { initial: ['a', 'b'], reviewers: ['a', 'b'] },
    agreement: {
      panelSize: 2,
      initialPanelSize: 2,
      terminalPanel: ['a', 'b'],
      initialPanel: ['a', 'b'],
      panelShrank: false,
      uncorroboratedMustFixCount: 0,
      vanishedFindings: [{ file: 'review.go', line: 42, title: 'vanished concern' }],
    },
    verdict: { ...cleanFixture().verdict, confidence: 'High' },
  })
  assert.match(md, /❌ \*\*Confidence:\*\* Low/)
  assert.match(md, /caller supplied `High` but report evidence derives `Low`/)
  assert.match(md, /Escalation: human adjudication needed for vanished-finding\./)
})

test('contradicted clean report with unresolved must-fix renders capped header and problem badges', () => {
  const md = renderReport({
    ...cleanFixture(),
    status: 'clean',
    rounds: 3,
    mustfix: {
      found: 1,
      fixed: 0,
      verified: 0,
      unresolved: 1,
      items: [{ disposition: 'unresolved', title: 'Still open', file: 'service.go', line: 12 }],
    },
    invalid: [],
    verdict: {
      assessment: 'Sound',
      evidence: 'All gates green',
      confidence: 'High',
      testing_assessment: 'Satisfactory',
      recommendation: 'Approve',
    },
  })
  assert.match(
    md,
    /bs-review completed after 3 round\(s\)\. 1 must-fix finding remains \(surfaced below\); see gates\./,
  )
  assert.doesNotMatch(md, /All must-fix findings fixed; required gates green\./)
  assert.match(md, /❌ \*\*Assessment:\*\* Unsound/)
  assert.match(md, /❌ \*\*Recommendation:\*\* Fix/)
  assert.equal((md.match(/Contradiction notice/g) || []).length, 1)
  assert.match(md, /caller supplied `clean` but report evidence derives `capped`/)
  assert.match(md, /Must-fix detail — found 1 \/ fixed 0 \/ verified 0 \/ unresolved 1/)
})

test('uncontradicted clean report remains byte-identical when evidence is clean', () => {
  const data = { ...cleanFixture(), invalid: [] }
  assert.equal(renderReport(data), renderReport({ ...data, status: 'clean' }))
})

test('missing report evidence renders capped instead of contradicting the verdict CLI', () => {
  const scratch = mkdtempSync(join(tmpdir(), 'bs-review-report-'))
  try {
    const report = {
      status: 'clean',
      rounds: 1,
      verdict: { assessment: 'Sound', recommendation: 'Approve' },
    }
    const md = renderReport(report)
    assert.match(md, /Unresolved review evidence remains \(surfaced below\); see gates\./)
    assert.doesNotMatch(md, /All must-fix findings fixed; required gates green\./)
    assert.match(md, /❌ \*\*Assessment:\*\* Unsound/)
    assert.match(md, /❌ \*\*Recommendation:\*\* Fix/)
    assert.match(md, /caller supplied `clean` but report evidence derives `capped`/)

    const reportPath = join(scratch, 'missing-evidence.json')
    writeFileSync(reportPath, JSON.stringify(report))
    const verdict = spawnSync(
      process.execPath,
      [
        fileURLToPath(new URL('./bs-review-caps.mjs', import.meta.url)),
        'verdict',
        '--in',
        reportPath,
      ],
      { encoding: 'utf8' },
    )
    assert.equal(verdict.status, 0)
    assert.match(verdict.stdout, /bs-review capped:/)
  } finally {
    rmSync(scratch, { recursive: true, force: true })
  }
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

test('invalid reviewer findings are retained in the rendered report', () => {
  const md = renderReport({
    status: 'capped',
    invalid: [
      {
        reason: 'findings-go.json: line must be an integer or null',
        source: { filename: 'findings-go.json', reviewer: 'golang-pro' },
        item: {
          severity: 'Warning',
          file: 'service.go',
          line: 'not-an-integer',
          title: 'Preserve this production finding',
          detail: 'Use a numeric line before retrying.',
        },
      },
    ],
  })
  assert.match(md, /Invalid reviewer findings — repair or re-run required/)
  assert.match(md, /findings-go\.json: line must be an integer or null/)
  assert.match(md, /Source: findings-go\.json \(reviewer: golang-pro\)/)
  assert.match(md, /```json/)
  assert.match(md, /Preserve this production finding/)
  assert.match(md, /Use a numeric line before retrying\./)
})

test('leave-as-is entries retain their verification evidence', () => {
  const md = renderReport(cleanFixture())
  assert.match(md, /isTTY guard branch.*Evidence: Read scripts\/bs-review-detect\.mjs:40/)
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
  const md = renderReport({
    mustfix: { unresolved: 0 },
    invalid: [],
    ledger: cleanLedger,
    verdict: { assessment: '❌ Sound' },
  })
  assert.match(md, /✅ \*\*Assessment:\*\* Sound/)
  assert.doesNotMatch(md, /❌ \*\*Assessment/)
})

test('capped status changes the header wording and reports open count', () => {
  const md = renderReport({
    status: 'capped',
    rounds: 3,
    mustfix: { unresolved: 2 },
    invalid: [],
    ledger: cleanLedger,
  })
  assert.match(
    md,
    /bs-review completed after 3 round\(s\)\. 2 must-fix findings remain \(surfaced below\); see gates\./,
  )
})

// A run caps on unrepaired `invalid` entries just as it does on open must-fix
// findings, and an invalid-only cap has `unresolved: 0`. Naming only must-fix
// made the durable terminal report state the wrong reason for the cap.
test('an invalid-only cap names invalid evidence, never "0 must-fix findings"', () => {
  const md = renderReport({
    status: 'capped',
    rounds: 2,
    mustfix: { unresolved: 0 },
    invalid: [{ reason: 'findings-lens-0-x.json: unexpected end of JSON input' }],
    ledger: cleanLedger,
  })
  assert.match(
    md,
    /bs-review completed after 2 round\(s\)\. 1 invalid entry remains \(surfaced below\); see gates\./,
  )
  assert.doesNotMatch(md, /0 must-fix/)
})

test('a cap with both blockers reports both counts', () => {
  const md = renderReport({
    status: 'capped',
    rounds: 4,
    mustfix: { unresolved: 2 },
    invalid: [{ reason: 'a' }, { reason: 'b' }, { reason: 'c' }],
    ledger: cleanLedger,
  })
  assert.match(md, /2 must-fix findings and 3 invalid entries remain \(surfaced below\)/)
})

test('patch summary reports patchable, narrative, and null-with-reason counts', () => {
  const md = renderReport({
    patchSummary: { patchable: 2, narrative: 1, nullWithReason: 1 },
  })
  assert.match(md, /Patch handling: 2 patchable \/ 1 narrative \/ 1 patch-null-with-reason\./)
})

test('zero patch summary is omitted and never claims a patch was applied', () => {
  const md = renderReport({
    patchSummary: { patchable: 0, narrative: 0, nullWithReason: 0 },
  })
  assert.doesNotMatch(md, /Patch handling:/)
  assert.doesNotMatch(md, /patchable/)
})

// Neither count is reportable: stay truthful rather than asserting a zero.
test('a cap with neither count falls back to generic wording, not a zero claim', () => {
  const md = renderReport({
    status: 'capped',
    rounds: 1,
    mustfix: { unresolved: 'unknown' },
    invalid: [],
    ledger: cleanLedger,
  })
  assert.match(md, /Unresolved review evidence remains \(surfaced below\); see gates\./)
  assert.doesNotMatch(md, /0 must-fix/)
  assert.doesNotMatch(md, /0 invalid/)
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
    invalid: [],
    ledger: cleanLedger,
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

test('BOS-1019: evidence rows can render review mode, base, and carried count', () => {
  const md = renderReport({
    reviewerInputBytes: { baseline: 900, resolved: 420 },
    mustfix: { unresolved: 0 },
    invalid: [],
    ledger: cleanLedger,
    evidenceRows: [
      {
        round: 'Round 2',
        mode: 'delta',
        base: 'abc1234',
        carriedCount: 3,
        result: '1 must-fix fixed',
      },
    ],
  })
  assert.match(md, /\| Round \| Mode \| Base \| Carried \| Result \|/)
  assert.match(md, /\| Round 2 \| delta \| abc1234 \| 3 \| 1 must-fix fixed \|/)
  assert.match(md, /Reviewer input bytes: baseline 900; resolved 420\./)
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
    invalid: [],
    ledger: cleanLedger,
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

// Regression coverage for the fail-open bug: with stdin NOT a TTY (a pipe,
// closed immediately — spawnSync's `input: ''` gives exactly that), an
// unrecognised argv must be rejected outright rather than falling through to
// the empty-stdin default-report path. A TTY-only assertion would pass
// against the buggy code and prove nothing, since the bug only manifests
// when process.stdin.isTTY is falsy.
test('CLI --help rejects with usage on stderr, no stdout, exit 2 (stdin closed, not a TTY)', () => {
  const { stdout, stderr, status } = runCli(['--help'], '')
  assert.equal(status, 2)
  assert.equal(stdout, '')
  assert.match(stderr, /usage/i)
})

test('CLI rejects an unknown flag with usage on stderr, no stdout, exit 2 (stdin closed, not a TTY)', () => {
  const { stdout, stderr, status } = runCli(['--nope'], '')
  assert.equal(status, 2)
  assert.equal(stdout, '')
  assert.match(stderr, /usage/i)
})

test('CLI rejects --in with no following value (stdin closed, not a TTY)', () => {
  const { stdout, stderr, status } = runCli(['--in'], '')
  assert.equal(status, 2)
  assert.equal(stdout, '')
  assert.match(stderr, /usage/i)
})

// --- BOS-1019: review delta state helpers ---------------------------------

function roundState() {
  return {
    rounds: [
      {
        round: 1,
        mode: 'full',
        base: 'base-0',
        tip: 'tip-1',
        mergeBase: 'base-0',
        changedFiles: ['a.js', 'b.js'],
        claims: [
          {
            id: 'open-1',
            status: 'open',
            file: 'a.js',
            line: 3,
            title: 'Still open',
            anchor: 'function stillOpen',
          },
        ],
      },
      {
        round: 2,
        mode: 'delta',
        base: 'tip-1',
        tip: 'tip-2',
        mergeBase: 'base-0',
        changedFiles: ['c.js'],
        claims: [
          {
            id: 'fixed-1',
            status: 'fixed',
            file: 'b.js',
            line: 4,
            title: 'Fixed',
            anchor: 'fixedBranch()',
          },
          {
            id: 'verified-1',
            status: 'verified',
            file: 'c.js',
            line: 5,
            title: 'Verified',
            anchor: 'verifiedBranch()',
          },
        ],
      },
    ],
  }
}

test('BOS-1019: round 1 is always full and absent state forces full', () => {
  assert.deepEqual(nextReviewRoundMode({ state: { rounds: [] }, mergeBase: 'base' }), {
    mode: 'full',
    base: 'base',
    reason: 'first-round',
    carriedClaims: [],
    carriedObservations: [],
    carriedCount: 0,
    carriedObservationCount: 0,
    round: 1,
  })
  assert.equal(nextReviewRoundMode({ state: {}, mergeBase: 'base' }).mode, 'full')
  assert.match(nextReviewRoundMode({ state: {}, mergeBase: 'base' }).reason, /rounds array/)
})

test('BOS-1019: round 3 delta base equals round 2 tip', () => {
  const next = nextReviewRoundMode({
    state: roundState(),
    changedFiles: ['d.js'],
    mergeBase: 'base-0',
    deltaFileThreshold: 20,
  })
  assert.equal(next.round, 3)
  assert.equal(next.mode, 'delta')
  assert.equal(next.base, 'tip-2')
})

test('BOS-1025: carried observations accumulate in round order and reach the next round mode', () => {
  const state = roundState()
  state.rounds[0].derivedObservations = [
    {
      round: 1,
      category: 'false-universal',
      paragraph: 'Round 1 findings repeatedly cited false universal wording.',
    },
  ]
  state.rounds[1].carriedObservations = [
    {
      round: 2,
      category: 'line-reference',
      paragraph: 'Round 2 findings repeatedly cited line-number references.',
    },
  ]

  assert.deepEqual(carriedReviewObservations(state), [
    {
      round: 1,
      category: 'false-universal',
      paragraph: 'Round 1 findings repeatedly cited false universal wording.',
    },
    {
      round: 2,
      category: 'line-reference',
      paragraph: 'Round 2 findings repeatedly cited line-number references.',
    },
  ])
  const next = nextReviewRoundMode({ state, changedFiles: ['d.js'], mergeBase: 'base-0' })
  assert.equal(next.carriedObservationCount, 2)
  assert.equal(next.carriedObservations[1].category, 'line-reference')
})

test('BOS-1025: cumulative round snapshots do not duplicate carried observations', () => {
  const first = {
    round: 1,
    category: 'false-universal',
    paragraph: 'Round 1 findings repeatedly cited false universal wording.',
  }
  const second = {
    round: 2,
    category: 'line-reference',
    paragraph: 'Round 2 findings repeatedly cited line-number references.',
  }
  const state = {
    rounds: [
      { round: 1, mode: 'full', tip: 't1', carriedObservations: [first] },
      { round: 2, mode: 'delta', tip: 't2', carriedObservations: [first, second] },
    ],
  }

  assert.deepEqual(carriedReviewObservations(state), [first, second])
  const next = nextReviewRoundMode({ state, changedFiles: ['d.js'], mergeBase: 'base-0' })
  assert.equal(next.carriedObservationCount, 2)
})

test('BOS-1025: report renders observations attributed to their rounds', () => {
  const md = renderReport({
    carriedObservations: [
      {
        round: 2,
        category: 'false-universal',
        paragraph:
          'Within-run observation from round 2: findings cited false universal wording only.',
      },
      {
        round: 3,
        category: 'line-reference',
        paragraph: 'Within-run observation from round 3: findings cited line-number references.',
      },
    ],
  })
  assert.match(md, /<details><summary>Within-run observations<\/summary>/)
  assert.match(
    md,
    /Round 2 — false-universal.*Within-run observation from round 2: findings cited false universal wording only\./,
  )
  assert.match(md, /Round 3 — line-reference/)
})

test('BOS-1025: a run with no observations renders the same report as before', () => {
  assert.equal(renderReport({}), renderReport({ carriedObservations: [] }))
})

test('BOS-1019: cumulative changed-file threshold since last full escalates to full', () => {
  const next = nextReviewRoundMode({
    state: roundState(),
    changedFiles: ['d.js', 'e.js'],
    mergeBase: 'base-0',
    deltaFileThreshold: 2,
  })
  assert.equal(next.mode, 'full')
  assert.equal(next.reason, 'delta-file-threshold')
  assert.equal(next.base, 'base-0')
})

test('BOS-1019: unreviewed fix file escalates to full', () => {
  const next = nextReviewRoundMode({
    state: roundState(),
    changedFiles: ['d.js'],
    unreviewedFixFiles: ['fix.js'],
    mergeBase: 'base-0',
  })
  assert.equal(next.mode, 'full')
  assert.equal(next.reason, 'unreviewed-fix-file')
})

test('BOS-1019: merge-base changed and prior tip is not ancestor forces full', () => {
  const next = nextReviewRoundMode({
    state: roundState(),
    changedFiles: ['d.js'],
    mergeBase: 'base-1',
  })
  assert.equal(next.mode, 'full')
  assert.equal(next.reason, 'merge-base-changed')

  const rebased = nextReviewRoundMode({
    state: roundState(),
    changedFiles: ['d.js'],
    mergeBase: 'base-0',
    lastTipAncestorOfCurrentTip: false,
  })
  assert.equal(rebased.mode, 'full')
  assert.equal(rebased.reason, 'tip-not-ancestor')
})

test('BOS-1019: missing carried anchor forces full', () => {
  const state = roundState()
  state.rounds[1].claims.push({
    id: 'lost-anchor',
    status: 'open',
    file: 'gone.js',
    line: 9,
    title: 'Lost anchor',
    anchor: 'missingSymbol',
    anchorMissing: true,
  })
  const next = nextReviewRoundMode({ state, changedFiles: ['d.js'], mergeBase: 'base-0' })
  assert.equal(next.mode, 'full')
  assert.equal(next.reason, 'anchor-missing')
})

test('BOS-1019: malformed round state forces full', () => {
  assert.equal(parseRoundState({ rounds: [{ round: 1, mode: 'full' }] }).ok, false)
  const next = nextReviewRoundMode({ state: { rounds: [{ round: 1, mode: 'full' }] } })
  assert.equal(next.mode, 'full')
  assert.match(next.reason, /tip/)
})

test('BOS-1019: bare line number is rejected', () => {
  assert.deepEqual(validateReviewClaim({ id: 'x', status: 'open', line: 7 }), {
    ok: false,
    reason: 'claim with a line must include a file',
  })
  assert.deepEqual(validateReviewClaim({ id: 'x', status: 'open', file: 'a.js', anchor: '7' }), {
    ok: false,
    reason: 'claim anchor must not be a line number',
  })
  assert.equal(
    parseRoundState({
      rounds: [{ round: 1, mode: 'full', tip: 't1', claims: [{ id: 'x', file: 'a.js' }] }],
    }).ok,
    false,
  )
})

test('BOS-1019: carried claims include open, fixed, verified and persist until reclosed', () => {
  const state = {
    rounds: [
      {
        round: 1,
        mode: 'full',
        tip: 't1',
        claims: [
          {
            id: 'open',
            disposition: 'unresolved',
            file: 'a.js',
            line: 1,
            title: 'Open',
            anchor: 'openFinding',
          },
          {
            id: 'fixed',
            status: 'fixed',
            file: 'b.js',
            line: 2,
            title: 'Fixed',
            anchor: 'fixedFinding',
          },
        ],
      },
      {
        round: 2,
        mode: 'delta',
        tip: 't2',
        claims: [
          {
            id: 'verified',
            status: 'verified',
            file: 'c.js',
            line: 3,
            title: 'Verified',
            anchor: 'verifiedFinding',
          },
        ],
      },
    ],
  }
  assert.deepEqual(
    carriedReviewClaims(state)
      .map((c) => `${c.id}:${c.status}`)
      .sort(),
    ['fixed:fixed', 'open:open', 'verified:verified'],
  )
  state.rounds.push({
    round: 3,
    mode: 'delta',
    tip: 't3',
    claims: [
      {
        id: 'open',
        status: 'reclosed',
        file: 'a.js',
        line: 1,
        title: 'Open',
        anchor: 'openFinding',
      },
    ],
  })
  assert.deepEqual(
    carriedReviewClaims(state)
      .map((c) => c.id)
      .sort(),
    ['fixed', 'verified'],
  )
})

test('BOS-1019: reviewer input byte totals accumulate over synthetic 9-round and 3-round sequences', () => {
  const nine = {
    rounds: Array.from({ length: 9 }, (_, i) => ({
      round: i + 1,
      reviewerInputBytes: (i + 1) * 10,
    })),
  }
  assert.equal(reviewerInputByteTotals(nine).totalBytes, 450)
  assert.equal(reviewerInputByteTotals(nine).rounds.at(-1).totalBytes, 450)

  const three = {
    rounds: [
      { round: 1, reviewerInput: 'aa' },
      { round: 2, reviewerInput: '€' },
      { round: 3, reviewerInput: 'bbbb' },
    ],
  }
  assert.deepEqual(
    reviewerInputByteTotals(three).rounds.map((r) => r.totalBytes),
    [2, 5, 9],
  )
})

test('BOS-1019: CLI subcommands return JSON and preserve unknown argv rejection', () => {
  const state = JSON.stringify(roundState())
  assert.equal(JSON.parse(runCli(['round-state'], state).stdout).ok, true)
  assert.equal(JSON.parse(runCli(['next-round-mode'], state).stdout).mode, 'delta')
  assert.equal(JSON.parse(runCli(['carried-claims'], state).stdout).length, 3)
  assert.equal(
    JSON.parse(
      runCli(['reviewer-input-bytes'], JSON.stringify({ rounds: [{ reviewerInput: 'abc' }] }))
        .stdout,
    ).totalBytes,
    3,
  )

  const dir = mkdtempSync(join(tmpdir(), 'bs-review-report-cli-'))
  try {
    const file = join(dir, 'state.json')
    writeFileSync(file, state)
    assert.equal(JSON.parse(runCli(['next-round-mode', '--in', file]).stdout).mode, 'delta')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }

  const bad = runCli(['next-round-mode', '--unknown'], state)
  assert.equal(bad.status, 2)
  assert.equal(bad.stdout, '')
  assert.match(bad.stderr, /usage/i)
})

test('BOS-1019: coverage replay assertion for full and delta byte accounting', () => {
  const state = {
    rounds: [
      { round: 1, mode: 'full', tip: 't1', reviewerInputBytes: 100 },
      { round: 2, mode: 'delta', tip: 't2', reviewerInputBytes: 25 },
      { round: 3, mode: 'delta', tip: 't3', reviewerInputBytes: 30 },
    ],
  }
  const replay = reviewerInputByteTotals(state)
  assert.deepEqual(
    replay.rounds.map((r) => r.inputBytes),
    [100, 25, 30],
  )
  assert.deepEqual(
    replay.rounds.map((r) => r.totalBytes),
    [100, 125, 155],
  )
})
