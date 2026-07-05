import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
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
  assert.match(md, /### bs-review report/)
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
  assert.match(md, /<details><summary>Evidence — rounds & gates<\/summary>/)
  assert.match(
    md,
    /<details><summary>Must-fix detail — found 1 \/ fixed 1 \/ verified 0 \/ unresolved 0<\/summary>/,
  )
  assert.match(md, /<details><summary>Leave as-is<\/summary>/)
  assert.match(md, /<details><summary><strong>Create 1 Linear issue<\/strong><\/summary>/)
})

test('the follow-up block summary counts suggestions with singular/plural', () => {
  const one = renderReport({
    suggestions: [{ title: 'Only one', file: 'a.ts', line: 1 }],
  })
  assert.match(one, /<details><summary><strong>Create 1 Linear issue<\/strong><\/summary>/)
  const two = renderReport({
    suggestions: [
      { title: 'First', file: 'a.ts', line: 1 },
      { title: 'Second', file: 'b.ts', line: 2 },
    ],
  })
  assert.match(two, /<details><summary><strong>Create 2 Linear issues<\/strong><\/summary>/)
})

test('the copyable prompt refers to issues, not tickets', () => {
  const md = renderReport(cleanFixture())
  assert.match(md, /create one Bossanova issue per item/)
  assert.match(md, /existing Todo\/In Progress issues\./)
  assert.doesNotMatch(md, /ticket/i)
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
  const md = renderReport(cleanFixture())
  assert.match(md, /Using the linear-bossanova MCP/)
  assert.match(md, /- Cover the isTTY CLI branch \(`scripts\/bs-review-detect\.mjs:44`\)/)
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
