// Deterministic renderer for the bs-review skill's PR summary comment.
//
// Mirrors the wondercanvas `wc-auto-review` layout so the report is easy to
// skim: a one-line header + horizontal rule, a ✅/❌ verdict block where the
// badge signals pass/fail at a glance, and collapsible <details> sections for
// the long material (test-coverage prose, round-by-round evidence, must-fix
// detail, leave-as-is rationales, the "Create N Linear issues" prompt). The
// renderer OWNS the layout and the ✅/❌ classification so a hand-written report
// can't drift per run.
//
// Node built-ins only — cron worktrees are dependency-free.

import { readFileSync } from 'node:fs'

// The idempotency marker: bs-implement upserts the single PR comment that
// carries this string, so a re-run edits the same comment instead of stacking.
export const MARKER = '<!-- bs-review -->'

// Whether a verdict field's value is "good / not a problem" (✅) vs a genuine
// problem (❌), per field. Modeled on wc-auto-review's VERDICT_OK; the good
// direction differs per field (High confidence is good; an unsatisfactory test
// assessment is bad). Evidence is adapted to our gate wording.
export const VERDICT_OK = {
  assessment: (v) => /^\s*sound/i.test(v), //                 Sound ✅ | Unsound ❌
  evidence: (v) => !/(unsatisf|fail|red)/i.test(v), //        gates green ✅ | a gate failed ❌
  confidence: (v) => !/^\s*low\b/i.test(v), //                High/Medium ✅ | Low ❌
  testing_assessment: (v) => !/^\s*unsatisf/i.test(v), //     Satisfactory/Unnecessary ✅ | Unsatisfactory ❌
  recommendation: (v) => /^\s*(merge|ship|approve)\b/i.test(v), // Merge/Ship/Approve ✅ | Fix/Hold ❌
}

// Escape the HTML-significant characters in caller-supplied free text so a
// literal tag like `<details>` in prose renders as visible text instead of
// being parsed as HTML by GitHub — which, for an unclosed tag, would swallow
// every following section into it. Applied ONLY to plain-markdown text; never
// to the renderer's own <details>/<summary>/<strong> markup, and never to
// content inside code spans/fences (already literal there). Markdown emphasis
// and inline `code` in the prose are left intact — only &<> are neutralised.
function esc(text) {
  return String(text).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

// Pick a code-fence long enough to wrap `text` without a backtick run inside it
// breaking out of the block (matches wc-auto-review's codeFence).
function codeFence(text) {
  const longest = (String(text).match(/`+/g) || []).reduce((m, r) => Math.max(m, r.length), 0)
  return '`'.repeat(Math.max(3, longest + 1))
}

// Render one `**Label:** value` line with a derived ✅/❌ badge at the start.
// Any emoji the caller embedded is stripped and re-derived so the badge is
// always self-consistent. Fields with no classifier (or N/A) render unbadged.
function verdictLine(field, label, value) {
  const v = String(value).replace(/^\s*[✅❌]\s*/, '')
  const classify = VERDICT_OK[field]
  // Classify on the raw value; escape only for display.
  if (!classify || v === 'N/A') return `**${label}:** ${esc(v)}`
  return `${classify(v) ? '✅' : '❌'} **${label}:** ${esc(v)}`
}

// `file:line` location suffix in code font, e.g. ` (`a/b.ts:42`)`. '' when no file.
function loc(item) {
  if (!item || !item.file) return ''
  return ` (\`${item.file}${item.line ? `:${item.line}` : ''}\`)`
}

// Wrap content in a collapsed <details> toggle. '' when there is no content, so
// empty sections are omitted entirely rather than rendering an empty toggle.
function detailsSection(summary, content) {
  if (!content) return ''
  return `<details><summary>${summary}</summary>\n\n${content}\n\n</details>`
}

function renderHeader({ rounds = 1, status = 'clean', mustfix = {} } = {}) {
  const r = `${rounds} round(s)`
  if (status === 'capped') {
    const open = mustfix.unresolved ?? 0
    const noun = open === 1 ? 'finding' : 'findings'
    return `bs-review completed after ${r}. ${open} must-fix ${noun} remain (surfaced below); see gates.`
  }
  return `bs-review completed after ${r}. All must-fix findings fixed; required gates green.`
}

// The always-visible verdict block: one combined issue-count + security line,
// then Assessment / Evidence / Confidence / Test Coverage / Recommendation, one
// per paragraph. Evidence / Test Coverage / Recommendation are optional.
function renderVerdictBlock({ verdict = {}, issuesHeadline = '', security = [] } = {}) {
  const lines = []
  const n = Array.isArray(security) ? security.length : 0
  const securityStatus =
    n > 0
      ? `**🚨 ${n} security ${n === 1 ? 'issue' : 'issues'} identified 🚨**`
      : 'No security issues identified.'
  const headline = issuesHeadline
    ? `**${esc(String(issuesHeadline).replace(/\.\s*$/, ''))}.** `
    : ''
  lines.push(`${headline}${securityStatus}`)
  if (n > 0) {
    lines.push(
      security
        .map(
          (s) =>
            `- **${esc(s.severity)}** ${esc(s.title)}${loc(s)}${s.fix ? ` — Fix: ${esc(s.fix)}` : ''}`,
        )
        .join('\n'),
    )
  }
  lines.push(verdictLine('assessment', 'Assessment', verdict.assessment ?? 'N/A'))
  if (verdict.evidence) lines.push(verdictLine('evidence', 'Evidence', verdict.evidence))
  lines.push(verdictLine('confidence', 'Confidence', verdict.confidence ?? 'N/A'))
  if (verdict.testing_assessment) {
    lines.push(verdictLine('testing_assessment', 'Test Coverage', verdict.testing_assessment))
  }
  if (verdict.recommendation)
    lines.push(verdictLine('recommendation', 'Recommendation', verdict.recommendation))
  return lines
}

// "Evidence — rounds & gates": the per-round result table plus the gate roster.
function renderEvidence({ evidenceRows = [], gates = [] } = {}) {
  const parts = []
  if (evidenceRows.length) {
    const rows = evidenceRows.map((r) => `| ${esc(r.round)} | ${esc(r.result)} |`).join('\n')
    parts.push(`| Round | Result |\n|-------|--------|\n${rows}`)
  }
  if (gates.length) {
    parts.push(`Gates (all green): ${gates.map((g) => `\`${g}\``).join(' · ')}.`)
  }
  return parts.join('\n\n')
}

// "Must-fix detail": one bullet per item, disposition badge first.
function renderMustfix(mustfix = {}) {
  const items = Array.isArray(mustfix.items) ? mustfix.items : []
  if (!items.length) return ''
  const badge = (d) =>
    d === 'verified' ? '☑️ **Verified**' : d === 'unresolved' ? '❌ **Unresolved**' : '✅ **Fixed**'
  return items
    .map((it) => {
      const commit = it.commit ? ` (\`${it.commit}\`)` : ''
      const detail = it.detail ? ` — ${esc(it.detail)}` : ''
      return `- ${badge(it.disposition)} — **${esc(it.title)}**${loc(it)}${detail}${commit}`
    })
    .join('\n')
}

function renderLeaveAsIs(leaveAsIs = []) {
  if (!leaveAsIs.length) return ''
  return leaveAsIs.map((l) => `- **${esc(l.title)}**${loc(l)} — ${esc(l.rationale)}`).join('\n')
}

// "Create N Linear issues": a single copyable, fence-guarded block an agent can
// paste to file each suggestion as a Linear issue. '' when there are no
// suggestions. The fenced code block is what gives GitHub the copy-to-clipboard
// button.
function renderSuggestions(suggestions = []) {
  if (!suggestions.length) return ''
  const list = suggestions
    .map((s) => `- ${s.title}${loc(s)}${s.detail ? ` — ${s.detail}` : ''}`)
    .join('\n')
  const prompt = [
    'Using the bossanova-linear MCP, create one Bossanova issue per item below (priority None,',
    'no project filter). Title = the item title; description = the detail + originating file:line.',
    'Do not create duplicates of existing Todo/In Progress issues.',
    '',
    list,
  ].join('\n')
  const fence = codeFence(prompt)
  return `${fence}\n${prompt}\n${fence}`
}

// "Test Coverage" details body: the optional coverage prose complementing the
// one-line verdict badge. '' when `verdict.testing_detail` is absent/empty, so
// detailsSection omits the toggle entirely (the badge still renders).
function renderTestCoverage(verdict = {}) {
  const detail = verdict.testing_detail
  return detail ? esc(String(detail).trim()) : ''
}

/**
 * Render the full bs-review PR comment markdown from a structured report.
 * @param {object} data See `services/boss/internal/skillinstall/skills/boss-review/SKILL.md` Phase 7 for the shape.
 * @returns {string} markdown, leading with the MARKER and trailing with a newline.
 */
export function renderReport(data = {}) {
  const {
    summary = '',
    verdict = {},
    issuesHeadline = '',
    security = [],
    evidenceRows = [],
    gates = [],
    mustfix = {},
    leaveAsIs = [],
    suggestions = [],
  } = data

  const blocks = []
  blocks.push(`${MARKER}\n${renderHeader(data)}`)
  blocks.push('---')
  blocks.push('### bs-review report')
  if (summary) blocks.push(esc(summary))
  blocks.push(...renderVerdictBlock({ verdict, issuesHeadline, security }))

  blocks.push(detailsSection('Test Coverage', renderTestCoverage(verdict)))

  const evidence = renderEvidence({ evidenceRows, gates })
  if (evidence) blocks.push(detailsSection('Evidence — rounds & gates', evidence))

  const mf = renderMustfix(mustfix)
  if (mf) {
    const tally =
      `found ${mustfix.found ?? 0} / fixed ${mustfix.fixed ?? 0} / ` +
      `verified ${mustfix.verified ?? 0} / unresolved ${mustfix.unresolved ?? 0}`
    blocks.push(detailsSection(`Must-fix detail — ${tally}`, mf))
  }

  blocks.push(detailsSection('Leave as-is', renderLeaveAsIs(leaveAsIs)))
  const n = suggestions.length
  blocks.push(
    detailsSection(
      `<strong>Create ${n} Linear ${n === 1 ? 'issue' : 'issues'}</strong>`,
      renderSuggestions(suggestions),
    ),
  )

  return blocks.filter(Boolean).join('\n\n') + '\n'
}

// Thin CLI: `node bs-review-report.mjs --in <report.json>` (or JSON on
// stdin) prints the rendered markdown to stdout.
if (import.meta.url === `file://${process.argv[1]}`) {
  const argv = process.argv.slice(2)
  const inIdx = argv.indexOf('--in')
  if (inIdx !== -1 && argv[inIdx + 1]) {
    process.stdout.write(renderReport(JSON.parse(readFileSync(argv[inIdx + 1], 'utf8'))))
  } else if (process.stdin.isTTY) {
    process.stderr.write('bs-review-report: provide JSON via --in <path> or on stdin\n')
    process.exit(2)
  } else {
    const chunks = []
    process.stdin.on('data', (c) => chunks.push(c))
    process.stdin.on('end', () =>
      process.stdout.write(renderReport(JSON.parse(chunks.join('') || '{}'))),
    )
  }
}
