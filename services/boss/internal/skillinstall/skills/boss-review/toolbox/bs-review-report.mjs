// Deterministic renderer for the bs-review skill's PR summary comment.
//
// Mirrors the wondercanvas `wc-auto-review` layout so the report is easy to
// skim: a one-line header + horizontal rule, a ✅/❌ verdict block where the
// badge signals pass/fail at a glance, and collapsible <details> sections for
// the long material (test-coverage prose, the reviewer roster, round-by-round
// evidence, must-fix detail, leave-as-is rationales, the "Create N follow-up
// issues" prompt). The renderer OWNS the layout and the ✅/❌ classification so a
// hand-written report can't drift per run.
//
// Node built-ins only — cron worktrees are dependency-free.

import { readFileSync } from 'node:fs'
import { loadSkillConfig, trackerConfigFor } from './skill-config.mjs'

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

function renderHeader({ rounds = 1, status = 'clean', mustfix = {}, invalid = [] } = {}) {
  const r = `${rounds} round(s)`
  if (status === 'capped') {
    // Two different things cap a run: open must-fix findings, and unrepaired
    // invalid entries. Naming only the first states the wrong reason whenever
    // malformed or missing reviewer evidence is the actual blocker -- an
    // invalid-only cap has `unresolved: 0`, so the header would have read
    // "0 must-fix findings remain" while the real cause sat in its own section.
    const open = mustfix.unresolved ?? 0
    const bad = Array.isArray(invalid) ? invalid.length : 0
    const parts = []
    if (open) parts.push(`${open} must-fix ${open === 1 ? 'finding' : 'findings'}`)
    if (bad) parts.push(`${bad} invalid ${bad === 1 ? 'entry' : 'entries'}`)
    // Neither count is reportable (a cap for some other reason): stay truthful
    // and generic rather than asserting a count of zero.
    if (!parts.length) {
      return `bs-review completed after ${r}. Unresolved review evidence remains (surfaced below); see gates.`
    }
    const verb = open + bad === 1 ? 'remains' : 'remain'
    return `bs-review completed after ${r}. ${parts.join(' and ')} ${verb} (surfaced below); see gates.`
  }
  return `bs-review completed after ${r}. All must-fix findings fixed; required gates green.`
}

// The always-visible verdict block, split into `lead` (the combined issue-count
// + security line, plus any security bullets — each its own paragraph) and
// `badges` (Assessment / Evidence / Confidence / Test Coverage / Recommendation).
// The badge lines are returned as a list so the caller can join them with single
// newlines and render them as one tight, consecutive block. Evidence / Test
// Coverage / Recommendation are optional.
function renderVerdictBlock({ verdict = {}, issuesHeadline = '', security = [] } = {}) {
  const lead = []
  const n = Array.isArray(security) ? security.length : 0
  const securityStatus =
    n > 0
      ? `**🚨 ${n} security ${n === 1 ? 'issue' : 'issues'} identified 🚨**`
      : 'No security issues identified.'
  const headline = issuesHeadline
    ? `**${esc(String(issuesHeadline).replace(/\.\s*$/, ''))}.** `
    : ''
  lead.push(`${headline}${securityStatus}`)
  if (n > 0) {
    lead.push(
      security
        .map(
          (s) =>
            `- **${esc(s.severity)}** ${esc(s.title)}${loc(s)}${s.fix ? ` — Fix: ${esc(s.fix)}` : ''}`,
        )
        .join('\n'),
    )
  }
  const badges = []
  badges.push(verdictLine('assessment', 'Assessment', verdict.assessment ?? 'N/A'))
  if (verdict.evidence) badges.push(verdictLine('evidence', 'Evidence', verdict.evidence))
  badges.push(verdictLine('confidence', 'Confidence', verdict.confidence ?? 'N/A'))
  if (verdict.testing_assessment) {
    badges.push(verdictLine('testing_assessment', 'Test Coverage', verdict.testing_assessment))
  }
  if (verdict.recommendation)
    badges.push(verdictLine('recommendation', 'Recommendation', verdict.recommendation))
  return { lead, badges }
}

// "N Reviewers": the per-lens / per-round reviewer roster as bullets
// (`- **golang-pro** — clean`), rendered inside a collapsible section by the
// caller. '' when there are no reviewers, so detailsSection omits the toggle
// entirely.
function renderReviewers(reviewers = []) {
  const items = Array.isArray(reviewers) ? reviewers : []
  if (!items.length) return ''
  return items
    .map((r) => `- **${esc(r.name)}** — ${esc(r.status)}${r.note ? ` (${esc(r.note)})` : ''}`)
    .join('\n')
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
  return leaveAsIs
    .map(
      (l) =>
        `- **${esc(l.title)}**${loc(l)} — ${esc(l.rationale)}${
          l.evidence ? ` — Evidence: ${esc(l.evidence)}` : ''
        }`,
    )
    .join('\n')
}

function renderInvalid(invalid = []) {
  if (!Array.isArray(invalid) || !invalid.length) return ''
  return invalid
    .map((entry) => {
      const source = entry?.source
      const sourceDetails =
        source && (source.filename || source.reviewer)
          ? ` — Source: ${esc(source.filename || 'unknown output')} (reviewer: ${esc(source.reviewer || 'unknown')})`
          : ''
      const reason = `- ${esc(entry.reason || 'Malformed reviewer finding')}${sourceDetails}`
      if (!Object.hasOwn(entry, 'item')) return reason
      const payload = JSON.stringify(entry.item, null, 2) ?? String(entry.item)
      const fence = codeFence(payload)
      return `${reason}\n\n${fence}json\n${payload}\n${fence}`
    })
    .join('\n\n')
}

// "Create N follow-up issues": a single copyable, fence-guarded prompt an agent
// can paste to file each suggestion as a tracker issue. '' when there are no
// suggestions. The fenced code block is what gives GitHub the copy-to-clipboard
// button. The label set is sourced verbatim from the configured tracker's
// `followUpLabels` list, so the published core carries no project-specific
// literal and the choice of which labels a follow-up issue gets lives in config,
// not here; an unconfigured repo (tracker null / no list) drops the label line
// and stays generic. `prUrl` / `issueUrl` are optional related links — each line
// (and the related-issue instruction + trailing report-back line) is omitted
// when its URL is absent, so a standalone run with no PR degrades cleanly.
function renderSuggestions(suggestions = [], tracker = null, { prUrl = '', issueUrl = '' } = {}) {
  if (!suggestions.length) return ''
  const lines = [
    'Please create the following follow-up issues from the automated code review of this PR.',
  ]
  if (issueUrl) lines.push('Create each in the same project/team as the related issue below.')
  const labels = Array.isArray(tracker?.followUpLabels) ? tracker.followUpLabels : []
  if (labels.length) lines.push(`Label all issues with: ${labels.join(', ')}.`)
  if (prUrl || issueUrl) lines.push('')
  if (prUrl) lines.push(`Related PR: ${prUrl}`)
  if (issueUrl) lines.push(`Related issue: ${issueUrl}`)
  for (const s of suggestions) {
    const originating = s.file ? ` (originating: ${s.file}${s.line ? `:${s.line}` : ''})` : ''
    const body = `${s.detail || s.title}${originating}`
    lines.push('')
    lines.push('<ticket>')
    lines.push(`<title>${s.title}</title>`)
    lines.push(`<body>${body}</body>`)
    lines.push(`<priority>${s.priority ?? 'Low'}</priority>`)
    lines.push('</ticket>')
  }
  if (prUrl) {
    lines.push('')
    lines.push(
      `Once you have created the issues, add a comment to the related PR (${prUrl}) listing every follow-up issue you created as a bullet list, with each bullet linking to the created issue.`,
    )
  }
  const prompt = lines.join('\n')
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
    reviewers = [],
    evidenceRows = [],
    gates = [],
    mustfix = {},
    invalid = [],
    leaveAsIs = [],
    suggestions = [],
    prUrl = '',
    issueUrl = '',
  } = data

  const tracker = data.tracker !== undefined ? data.tracker : trackerConfigFor(loadSkillConfig())

  const blocks = []
  blocks.push(`${MARKER}\n${renderHeader(data)}`)
  blocks.push('---')
  blocks.push('#### Code Review')
  if (summary) blocks.push(esc(summary))
  const { lead, badges } = renderVerdictBlock({ verdict, issuesHeadline, security })
  blocks.push(...lead)
  blocks.push(badges.join('\n'))

  blocks.push(detailsSection('Test Coverage', renderTestCoverage(verdict)))

  const reviewerList = Array.isArray(reviewers) ? reviewers : []
  const nr = reviewerList.length
  blocks.push(detailsSection(`${nr} Reviewer${nr === 1 ? '' : 's'}`, renderReviewers(reviewerList)))

  const evidence = renderEvidence({ evidenceRows, gates })
  if (evidence) blocks.push(detailsSection('Evidence — rounds & gates', evidence))

  const mf = renderMustfix(mustfix)
  if (mf) {
    const tally =
      `found ${mustfix.found ?? 0} / fixed ${mustfix.fixed ?? 0} / ` +
      `verified ${mustfix.verified ?? 0} / unresolved ${mustfix.unresolved ?? 0}`
    blocks.push(detailsSection(`Must-fix detail — ${tally}`, mf))
  }

  blocks.push(
    detailsSection('Invalid reviewer findings — repair or re-run required', renderInvalid(invalid)),
  )

  blocks.push(detailsSection('Leave as-is', renderLeaveAsIs(leaveAsIs)))
  const n = suggestions.length
  blocks.push(
    detailsSection(
      `<strong>Create ${n} follow-up ${n === 1 ? 'issue' : 'issues'}</strong>`,
      renderSuggestions(suggestions, tracker, { prUrl, issueUrl }),
    ),
  )

  return blocks.filter(Boolean).join('\n\n') + '\n'
}

// Thin CLI: `node bs-review-report.mjs --in <report.json>` (or JSON on
// stdin) prints the rendered markdown to stdout.
import { isMainModule } from './main-module.mjs'

if (isMainModule(import.meta.url)) {
  const argv = process.argv.slice(2)
  const usage =
    'usage: bs-review-report [--in <path>]\n' +
    '  Renders report JSON to markdown on stdout.\n' +
    '  With no arguments, reads the JSON from stdin.\n' +
    '  --in <path>  read the JSON from a file instead of stdin\n' +
    '  -h, --help   show this message\n'
  // Anything other than a bare `--in <path>` pair (or no args at all) is
  // rejected up front — unconditionally, regardless of process.stdin.isTTY.
  // This runs BEFORE the isTTY branch below so an unrecognised flag (a typo,
  // --help, a future caller's flag) can never fall through to the
  // empty-stdin default-report path and print a fabricated pass.
  const isValidIn = argv.length === 2 && argv[0] === '--in' && Boolean(argv[1])
  if (argv.length > 0 && !isValidIn) {
    process.stderr.write(usage)
    process.exit(2)
  } else if (isValidIn) {
    process.stdout.write(renderReport(JSON.parse(readFileSync(argv[1], 'utf8'))))
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
