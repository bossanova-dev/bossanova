// scripts/sweep-review-gate.mjs
// Pure-core decision logic for the bs-sweep-review skill (BOS-617). NO side
// effects in the decision core; the only I/O is the thin, injected Linear
// marker-fetch wrapper at the bottom, mirroring the established
// sweep-sentry-gate.mjs / sweep-maingate-gate.mjs shape. No imports beyond node
// builtins and the shared `fetchMarkedLinearIssues` reuse — the cron worktree is
// dependency-free.
//
// The sweep reads the inline review comments the Codex connector leaves on the
// staging/production release PRs, clusters them by their head-side ANCHOR, and
// hands the clusters to selectStub, which picks ONE deduped Unplanned Linear stub
// to file. Detect + surface only — the sweep never plans, implements, or merges;
// it feeds the bs-sweep-plan → boss-plan → boss-build pipeline.
//
// Marker scheme (line-anchored `^Review: <path>@<line>$`, the exact last line of a
// stub body so the next run dedupes off it):
//   services/bossd/internal/accountwiring/adapters.go:252
//     → `Review: services/bossd/internal/accountwiring/adapters.go@252`
//
// WHY the anchor and not the GitHub comment id: verified against the 2026-07-29
// release PR pair, the SAME finding carries a DIFFERENT `id` and a re-paraphrased
// headline on the staging vs the production PR, while `path`/`line`/`side` are
// identical. Keying on the comment id (or the headline) provably never dedupes the
// pair — which is the whole point of the ticket. `commit_id` is deliberately NOT in
// the key either: the two release PRs are commonly cut from different `main` SHAs.
// The accepted cost is that two distinct findings at one anchor collapse into one
// ticket — handled by rendering EVERY comment in the cluster, so no finding is lost.

// The automated reviewer whose inline comments this sweep files. Documented at
// services/bossd/internal/vcs/github/provider.go:531 (the BOS-254 fix), which also
// records that this bot posts a byte-identical empty boilerplate `COMMENTED` review
// on every push — we read the INLINE comments, so that boilerplate never reaches us.
export const REVIEW_BOT_LOGIN = 'chatgpt-codex-connector[bot]'

// Release PRs are identified by BASE BRANCH, never by title: the
// create-staging-release / create-production-release workflows open them with
// `gh pr create --base staging|production --head main`, and the title carries a
// timestamp that is not a stable key.
export const RELEASE_BASE_BRANCHES = ['staging', 'production']

// The marker namespace. Shared by the write side (bodyFor) and the read side
// (fetchTrackedReviewMarkers) so the two can never drift.
export const MARKER_PREFIX = 'Review: '

// Severity → weight. The Codex connector badges findings `P1` / `P2`; anything
// unbadged (or a future `P3`) falls back to 5 so it still scores above zero and is
// filed rather than silently lost when the bot's output format drifts.
const SEVERITY_WEIGHT = { P1: 30, P2: 15 }
const UNBADGED_WEIGHT = 5

// Bounded rendering so one pathological anchor cannot produce a giant stub.
const MAX_RENDERED_FINDINGS = 20
const MAX_RENDERED_BODY_CHARS = 4000
const MAX_TITLE_CHARS = 250

/**
 * Collapse ALL FOUR ECMAScript line terminators (LF, CR, LS U+2028, PS U+2029) —
 * not just \r\n — to a single space. Review-comment bodies are LLM-authored
 * UNTRUSTED text. The tracked-marker scan's `^…$` runs with the JS `m` flag, whose
 * line boundaries include a bare CR and U+2028/U+2029, so an untreated body
 * carrying one of those before `Review: other/path@1` would forge a phantom marker
 * line and poison the dedupe (a real finding then skipped as already-tracked).
 * Every interpolated comment/headline string goes through this first.
 */
export function sanitize(text) {
  return String(text ?? '').replace(/[\r\n\u2028\u2029]+/g, ' ')
}

// Paths can carry regex metacharacters (`+`, `(`, `)`, `.`). Escape before
// building a per-marker regex, or the anchored check would match the wrong
// description (or throw).
function escapeRegExp(s) {
  return String(s).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

/**
 * True iff `description` carries `marker` as an exact, line-anchored line
 * (case-sensitive, trailing whitespace tolerated). This is the same rule the
 * Phase 3 pre-create re-check applies to a `list_issues` hit, so the gate's
 * snapshot dedupe and the skill's race-narrowing re-check agree byte for byte.
 */
export function hasMarker(description, marker) {
  if (typeof marker !== 'string' || marker === '') return false
  const re = new RegExp('^' + escapeRegExp(marker) + '[ \\t]*$', 'm')
  return re.test(String(description ?? ''))
}

/**
 * Parse one inline review comment into a finding.
 *
 * The live first line is:
 *   **<sub><sub>![P1 Badge](https://img.shields.io/badge/P1-orange?style=flat)</sub></sub>  <headline>**
 * so severity and a human headline are both mechanically parseable. An UNBADGED
 * body degrades to `severity: null` + the first non-empty line as the headline
 * rather than throwing — the badge shape is an undocumented artifact of the Codex
 * connector and must be allowed to drift without losing findings. A null /
 * non-object / body-less comment never throws.
 */
export function parseFinding(comment) {
  const c = comment && typeof comment === 'object' ? comment : {}
  const body = typeof c.body === 'string' ? c.body : ''

  const first =
    body
      .split(/[\r\n\u2028\u2029]/)
      .map((l) => l.trim())
      .find((l) => l.length > 0) ?? ''

  const badge = first.match(/!\[\s*(P\d+)\s+Badge\s*\]/)
  let headline = first
  if (badge) {
    // Drop the badge markup and its `(url)` target, then the <sub> wrappers and
    // any surrounding markdown emphasis, leaving the bare headline.
    headline = headline
      .slice(headline.indexOf(badge[0]) + badge[0].length)
      .replace(/^\([^)]*\)/, '')
  }
  headline = headline
    .replace(/<\/?sub>/g, '')
    .replace(/^[\s*_`]+/, '')
    .replace(/[\s*_`]+$/, '')
    .trim()

  // GitHub nulls `line` once a comment's diff hunk goes stale (OUTDATED) and keeps
  // the real head-side line in `original_line`. Without the fallback every outdated
  // comment in a file would degrade to the file-level `<path>@0` key, so unrelated
  // stale findings would merge into one ticket AND the first `Review: <path>@0` filed
  // would permanently suppress every later outdated finding anywhere in that file.
  const lineNum = Number(c.line ?? c.original_line)

  return {
    id: c.id ?? null,
    severity: badge ? badge[1] : null,
    headline,
    path: typeof c.path === 'string' && c.path !== '' ? c.path : null,
    line: Number.isFinite(lineNum) ? lineNum : null,
    side: typeof c.side === 'string' ? c.side : null,
    body,
    url: typeof c.html_url === 'string' ? c.html_url : null,
  }
}

/** True iff this comment was authored by the automated reviewer we file for. */
export function isReviewBotComment(comment) {
  return comment?.user?.login === REVIEW_BOT_LOGIN
}

/**
 * The cluster key: `<path>@<line>` — the head-side anchor, the ONLY field set
 * verified stable across the staging/production release PR pair. A comment with no
 * string `path` is unkeyable and returns null (payload sanitization: it never
 * appears in any output). A file-level (line-less) comment keys at line `0`.
 *
 * `side` is DELIBERATELY excluded, not overlooked. In principle a `LEFT`
 * (deleted-side) comment could collide with a `RIGHT` one at the same `path@line`
 * and its marker would then suppress the RIGHT finding. In practice the reviewer
 * this sweep reads posts head-side comments only — sampled across four PRs
 * (including both 2026-07-29 release PRs), 37 of 37 bot comments were `side:
 * "RIGHT"` and none was `LEFT`. Keying on side would either change the marker
 * format that `Review: <path>@<line>` pins, or require dropping LEFT comments
 * outright (a silent loss). Neither cost is worth paying for an unobserved case;
 * revisit if a `LEFT` bot comment is ever seen.
 */
export function clusterKey(comment) {
  const f = parseFinding(comment)
  if (f.path === null) return null
  return `${f.path}@${f.line ?? 0}`
}

/** `Review: <key>`, from a cluster object or a raw key string. */
export function markerFor(cluster) {
  const key = typeof cluster === 'string' ? cluster : String(cluster?.key ?? '')
  return `${MARKER_PREFIX}${sanitize(key)}`
}

/**
 * Group raw inline comments into anchor clusters, preserving first-seen order.
 * Non-object comments and comments lacking a string `path` are sanitized out.
 *
 * A cluster is `{ key, marker, findings, prUrls }` where `findings` are
 * parseFinding results in input order and `prUrls` are the distinct release PRs
 * the anchor was seen on (derived from each comment's `html_url`).
 */
export function clusterComments(comments) {
  const list = Array.isArray(comments) ? comments : []
  const byKey = new Map()
  for (const comment of list) {
    if (!comment || typeof comment !== 'object') continue
    const key = clusterKey(comment)
    if (key === null) continue
    if (!byKey.has(key)) byKey.set(key, { key, marker: markerFor(key), findings: [], prUrls: [] })
    const cluster = byKey.get(key)
    const finding = parseFinding(comment)
    cluster.findings.push(finding)
    // `html_url` is `…/pull/<n>#discussion_r<id>`; the part before `#` is the PR.
    const prUrl = finding.url ? finding.url.split('#')[0] : null
    if (prUrl && !cluster.prUrls.includes(prUrl)) cluster.prUrls.push(prUrl)
  }
  return [...byKey.values()]
}

/**
 * Deterministic cluster score — no time input, so the core stays pure and pinnable.
 * Takes the MAX severity in the cluster (P1 30, P2 15, unbadged 5) and adds 5 per
 * ADDITIONAL comment, so a repeatedly-flagged anchor outranks a lone one.
 */
export function scoreCluster(cluster) {
  const findings = Array.isArray(cluster?.findings)
    ? cluster.findings
    : Array.isArray(cluster?.comments)
      ? cluster.comments.map(parseFinding)
      : []
  if (findings.length === 0) return 0
  const max = Math.max(...findings.map((f) => SEVERITY_WEIGHT[f?.severity] ?? UNBADGED_WEIGHT))
  return max + 5 * (findings.length - 1)
}

// The highest-severity finding in a cluster (first wins on a tie) — the one the
// title reflects. A same-anchor collision means the title names only the top
// finding; the BODY still carries every one of them.
function topFinding(cluster) {
  const findings = Array.isArray(cluster?.findings) ? cluster.findings : []
  let best = null
  let bestWeight = -1
  for (const f of findings) {
    const w = SEVERITY_WEIGHT[f?.severity] ?? UNBADGED_WEIGHT
    if (w > bestWeight) {
      best = f
      bestWeight = w
    }
  }
  return best
}

function truncate(text, max) {
  return text.length <= max ? text : `${text.slice(0, max - 1)}…`
}

/**
 * The stub title: the highest-severity headline plus the anchor (headlines are
 * re-paraphrased per PR, so the anchor is what makes the title unambiguous).
 * Always a single sanitized line.
 */
export function titleFor(cluster) {
  const top = topFinding(cluster)
  const severity = top?.severity ? `[${sanitize(top.severity)}] ` : ''
  const headline = sanitize(top?.headline ?? '').trim() || 'Automated review finding'
  const anchor = sanitize(cluster?.key ?? '').replace('@', ':')
  return truncate(`${severity}${headline} — ${anchor}`, MAX_TITLE_CHARS)
}

/**
 * Render the stub body. The marker is the EXACT last line — no trailing newline —
 * so the all-states dedupe scan keys off `^Review: <path>@<line>$` reliably.
 *
 * EVERY interpolated comment/headline string runs through sanitize() first, and
 * every interpolated line is rendered behind a structural prefix (`### `, `> `,
 * `Source: `, `- `). Both together are what make a forged marker impossible: even a
 * body that literally STARTS with `Review: forged/path@1` lands mid-line behind
 * `> `, so `^Review: ` can only ever match the one real marker.
 */
export function bodyFor(cluster, marker) {
  const findings = Array.isArray(cluster?.findings) ? cluster.findings : []
  const anchor = topFinding(cluster) ?? findings[0] ?? null
  const path = sanitize(anchor?.path ?? '(unknown path)')
  const line = anchor?.line ?? 0
  const side = sanitize(anchor?.side ?? 'RIGHT')

  const lines = []
  lines.push(
    `Automated review finding on the release PR for \`${path}\` line \`${line}\`.`,
    '',
    '## Findings',
  )

  for (const f of findings.slice(0, MAX_RENDERED_FINDINGS)) {
    const severity = f?.severity ? `[${sanitize(f.severity)}] ` : ''
    const headline = sanitize(f?.headline ?? '').trim() || '(no headline)'
    lines.push('', `### ${severity}${headline}`, '')
    lines.push(`> ${truncate(sanitize(f?.body ?? '').trim(), MAX_RENDERED_BODY_CHARS)}`)
    if (f?.url) lines.push('', `Source: ${sanitize(f.url)}`)
  }
  if (findings.length > MAX_RENDERED_FINDINGS) {
    lines.push(
      '',
      `_…and ${findings.length - MAX_RENDERED_FINDINGS} more comment(s) at this anchor._`,
    )
  }

  lines.push('', '## Context', '')
  lines.push(`- anchor: \`${path}\` line \`${line}\` (\`${side}\` side)`)
  const prUrls = Array.isArray(cluster?.prUrls) ? cluster.prUrls : []
  if (prUrls.length > 0) lines.push(`- seen on: ${prUrls.map((u) => sanitize(u)).join(' · ')}`)
  lines.push(
    '- filed by the `bs-sweep-review` cron sweep (BOS-617); dedupe keys off the anchor marker below',
  )

  lines.push('', marker)
  return lines.join('\n')
}

/**
 * The single selection entry point — the skill's Phase 2 triage, the cron gate and
 * the pure-core tests all call this; none re-implements clustering or dedupe.
 *
 * Input:  { comments: [<raw GitHub inline review comment>, ...],
 *           trackedMarkers: Set<string> | string[],
 *           threshold?: number, cap?: number }
 * Output: { toFile: { key, marker, title, body, score, count } | null,
 *           deferred: [{ key, marker, score, reason }],
 *           dropped:  [{ key, marker, score, reason }] }
 *
 * Rules (every non-filed cluster carries a reason — no silent caps):
 *   1. Sanitize: non-object comments and comments lacking a string `path` are
 *      excluded outright (payload sanitization, not a reasoned drop — they never
 *      appear in any output bucket).
 *   2. Cluster the rest by `<path>@<line>`, so the staging/production duplicate
 *      pair collapses to ONE cluster and one stub.
 *   3. A cluster whose marker is already in `trackedMarkers` → dropped,
 *      `already-tracked` (a Linear stub exists in some state, incl. archived).
 *   4. score < threshold → dropped, `below-threshold`.
 *   5. Remaining eligible clusters sort score desc, tie-break key asc. The first is
 *      filed (`toFile`); the next up-to-`cap` are `deferred` (`over-cap`, still
 *      eligible on a later run); anything beyond is dropped `over-cap`.
 */
export function selectStub({ comments, trackedMarkers, threshold = 1, cap = 25 } = {}) {
  const tracked = trackedMarkers instanceof Set ? trackedMarkers : new Set(trackedMarkers ?? [])
  const clusters = clusterComments(comments)

  const dropped = []
  const eligible = []
  for (const cluster of clusters) {
    const score = scoreCluster(cluster)
    const entry = { key: cluster.key, marker: cluster.marker, score }
    if (tracked.has(cluster.marker)) {
      dropped.push({ ...entry, reason: 'already-tracked' })
      continue
    }
    if (score < threshold) {
      dropped.push({ ...entry, reason: 'below-threshold' })
      continue
    }
    eligible.push({ cluster, score })
  }

  eligible.sort((a, b) => {
    if (b.score !== a.score) return b.score - a.score
    return a.cluster.key < b.cluster.key ? -1 : a.cluster.key > b.cluster.key ? 1 : 0
  })

  const rest = eligible.slice(1)
  const deferred = rest.slice(0, cap).map(({ cluster, score }) => ({
    key: cluster.key,
    marker: cluster.marker,
    score,
    reason: 'over-cap',
  }))
  for (const { cluster, score } of rest.slice(cap)) {
    dropped.push({ key: cluster.key, marker: cluster.marker, score, reason: 'over-cap' })
  }

  if (eligible.length === 0) return { toFile: null, deferred, dropped }

  const { cluster, score } = eligible[0]
  return {
    toFile: {
      key: cluster.key,
      marker: cluster.marker,
      title: titleFor(cluster),
      body: bodyFor(cluster, cluster.marker),
      score,
      count: cluster.findings.length,
    },
    deferred,
    dropped,
  }
}

// ---------------------------------------------------------------------------
// Thin async I/O wrapper, kept out of the pure core. Reuses the shared paginated,
// fail-closed all-states marker scan (fetchMarkedLinearIssues) rather than
// re-implementing it — the same helper bs-sweep-sentry and the main-gate probe use
// — parameterised to the `Review: ` marker namespace. `linearRequest`
// (skills-toolbox/linear-gate-lib.mjs) is injected so this module keeps a testable
// seam and no hidden network calls.
import { fetchMarkedLinearIssues } from './sweep-sentry-gate.mjs'

// The line-anchored marker regex used to lift `Review: <path>@<line>` lines out of
// a Linear issue description (case-sensitive, trailing whitespace tolerated). The
// content class excludes ALL four ECMAScript line terminators — the same set
// sanitize() collapses on the write side — so the read side stays symmetric with
// the write side and cannot straddle an exotic boundary the `m`-flag `$` also
// breaks on.
const REVIEW_MARKER_RE = /^Review: [^\r\n\u2028\u2029]+?[ \t]*$/gm

/**
 * Fetch every `Review: `-marked Linear issue (ALL states, incl. archived) and
 * return the exact Set of tracked marker lines for selectStub's dedupe. Bounded,
 * fail-closed pagination is inherited from fetchMarkedLinearIssues: a TRUNCATED
 * scan throws — a partial dedupe snapshot must never be triaged, because it would
 * re-file an already-tracked anchor.
 */
export async function fetchTrackedReviewMarkers({ apiKey, linearRequest, maxPages = 20 } = {}) {
  const nodes = await fetchMarkedLinearIssues({
    apiKey,
    linearRequest,
    maxPages,
    markerPrefix: MARKER_PREFIX,
  })
  const markers = new Set()
  for (const node of nodes) {
    const desc = String(node?.description ?? '')
    for (const line of desc.match(REVIEW_MARKER_RE) ?? []) {
      markers.add(line.replace(/[ \t]+$/, ''))
    }
  }
  return markers
}

// CLI: `select <comments.json> <tracked.json> [threshold] [cap]` prints the
// selectStub result. Mirrors sweep-maingate-gate.mjs's runCli so the skill and the
// gate shell out to a single source of truth instead of re-implementing selection.
import { readFileSync } from 'node:fs'

export function runCli(argv, { readFile = (f) => readFileSync(f, 'utf8') } = {}) {
  const [cmd, ...rest] = argv
  const json = (f) => JSON.parse(readFile(f))
  switch (cmd) {
    case 'select': {
      const comments = json(rest[0])
      const trackedMarkers = rest[1] !== undefined ? json(rest[1]) : []
      return JSON.stringify(
        selectStub({
          comments,
          trackedMarkers,
          threshold: rest[2] !== undefined ? Number(rest[2]) : undefined,
          cap: rest[3] !== undefined ? Number(rest[3]) : undefined,
        }),
      )
    }
    default:
      throw new Error(`unknown subcommand: ${cmd}`)
  }
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(runCli(process.argv.slice(2)))
    process.stdout.write('\n')
  } catch (err) {
    process.stderr.write(`sweep-review-gate: ${err.message}\n`)
    process.exit(1)
  }
}
