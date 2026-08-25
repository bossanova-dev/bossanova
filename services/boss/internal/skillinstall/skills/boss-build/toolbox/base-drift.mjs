// Base-drift detection for a long-running review loop.
//
// A review stack that resolves its base once at Preflight is blind to anything that lands on the
// base branch while it runs. This helper answers "has the base moved, and does the move overlap
// what this branch changed?" from refs that are already local — it performs no network I/O, makes
// no worktree/HEAD/ref mutation, and imports nothing outside node built-ins plus ./main-module.mjs.
// The caller owns fetching the base ref before calling; the caller also owns any rebase decision.
//
// Two stages, so the common case stays cheap:
//   stage 1  `behind` — how many commits the base carries that HEAD does not. Zero means the base
//            has not moved and stage 2 is skipped entirely.
//   stage 2  `intersection` of the paths each side changed since the merge base, plus a
//            `git merge-tree --write-tree` probe that separates a *textual* conflict from a silent
//            *semantic* overlap (an overlapping change that still merges cleanly is exactly the
//            case a review loop cannot see and a human reviewer most needs told about).
//
// A failed fetch is the caller's to report, not to swallow: pass `fetchFailed: true` (CLI:
// `--fetch-failed`) and the whole reading collapses to 'unevaluated', because a base ref nobody
// could refresh is not evidence that the base has not moved.
//
// Attribution is read-only and offline like everything else here: the pull requests that landed the
// overlapping base commits are parsed out of the local commit subjects, never asked of a forge.
//
// Fail-closed contract: 'unevaluated' is its own outcome and is never collapsed onto 'clean'.
// `behind` is a non-negative integer or the string 'unevaluated'; `mergeTree` is one of
// 'clean' | 'conflicts' | 'skipped' | 'unevaluated'. A caller may treat the non-'clean' values as it
// likes, but it must never be able to mistake one of them for a proven-clean result.
//
// 'skipped' and 'unevaluated' are deliberately DIFFERENT values, and collapsing them is the whole
// reason 'skipped' exists. A probe that was never needed — the base has not moved, or the two change
// sets are disjoint, so there is no overlap to probe — is a complete, healthy answer. A probe that
// was needed and could not run — no merge base, a failed changed-path diff, a git that does not
// support `merge-tree --write-tree` — is the detector saying it could not tell. Reported on one
// value, a caller has to infer which it holds from a second field, and the ordinary healthy case
// then reads as an unresolved one: 'skipped' makes that a single-field lookup instead.

import { spawnSync } from 'node:child_process'

import { isMainModule } from './main-module.mjs'

export const UNEVALUATED = 'unevaluated'
export const MERGE_TREE_CLEAN = 'clean'
export const MERGE_TREE_CONFLICTS = 'conflicts'
// The probe was not needed: nothing overlapped for it to say anything about. Never 'clean' (nothing
// was proven mergeable) and never 'unevaluated' (nothing was left unanswered).
export const MERGE_TREE_SKIPPED = 'skipped'

// `git merge-tree --write-tree` prints the merged tree OID on its first line. It exits 1 for a
// content conflict — but it *also* exits 1 when a ref argument is not mergeable ("not something we
// can merge"), and older gits exit 129 for the unknown `--write-tree` option. Exit status alone
// therefore cannot separate "conflicts" from "could not run", so a conflict verdict additionally
// requires that first line to be an object id.
const OID = /^[0-9a-f]{40}(?:[0-9a-f]{24})?$/
// Conflicted-file records are `ls-files -u` shaped: `<mode> <object> <stage>\t<path>`.
const CONFLICT_LINE = /^[0-7]{6} [0-9a-f]{40,64} [123]\t(.+)$/

// The two conventions a forge writes onto the base branch when a pull request lands: a squash-merge
// puts `(#N)` at the very end of the subject, a merge commit opens with `Merge pull request #N`.
// Anchored on purpose — a bare `#N` in the middle of a subject is a reference to something, not
// evidence of the change that landed this commit, and guessing from one would attribute an
// overlapping base change to a PR that never touched it.
export const SQUASH_MERGE_PR = /\(#(\d+)\)\s*$/
export const MERGE_COMMIT_PR = /^Merge pull request #(\d+)\b/
// `git log --format` field separator: ASCII unit separator cannot occur in a `%s` subject.
const FIELD_SEP = '\u001f'
export const ATTRIBUTED = 'ok'

/**
 * Default command runner. Injectable so tests can simulate a git that lacks a subcommand.
 * @param {string} repo working directory
 * @param {string[]} args git arguments
 * @returns {{status: number, stdout: string, stderr: string}}
 */
export function runGit(repo, args) {
  const res = spawnSync('git', args, {
    cwd: repo,
    encoding: 'utf8',
    env: { ...process.env, GIT_OPTIONAL_LOCKS: '0' },
  })
  if (res.error) return { status: -1, stdout: '', stderr: String(res.error.message || res.error) }
  return {
    status: res.status === null ? -1 : res.status,
    stdout: res.stdout || '',
    stderr: res.stderr || '',
  }
}

/**
 * Resolve a rev to a commit id, or null when it does not name a commit.
 * @returns {string|null}
 */
export function resolveCommit({ repo, rev, run = runGit }) {
  const res = run(repo, ['rev-parse', '--verify', '--quiet', `${rev}^{commit}`])
  if (res.status !== 0) return null
  const oid = res.stdout.trim()
  return OID.test(oid) ? oid : null
}

/**
 * Stage 1: commits reachable from `base` but not from `head`.
 * Returns a non-negative integer, or 'unevaluated' when either rev does not resolve or the count
 * cannot be parsed. An unresolvable ref makes `git rev-list --count` exit 128 with empty stdout —
 * reading that empty string as 0 would report "base has not moved" for a base nobody could read,
 * which is the precise failure this helper exists to prevent.
 */
export function countBehind({ repo, base, head = 'HEAD', run = runGit }) {
  const res = run(repo, ['rev-list', '--count', `${head}..${base}`, '--'])
  if (res.status !== 0) return UNEVALUATED
  const text = res.stdout.trim()
  if (!/^\d+$/.test(text)) return UNEVALUATED
  return Number(text)
}

/**
 * Paths changed between two revs, as a sorted array. Returns null when the diff cannot be taken.
 * @returns {string[]|null}
 */
export function changedPaths({ repo, from, to, run = runGit }) {
  // `-z` is not a formatting preference. Without it `core.quotePath` (on by default) C-quotes any
  // path outside ASCII — `café.txt` comes back as the eight-character literal `"caf\303\251.txt"`
  // — and that spelling matches no pathspec when the caller hands it back to `git log`. Both sides
  // of the intersection are quoted identically, so the overlap is still DETECTED; only the
  // attribution silently finds nothing and reports "no base commit touches the shared paths",
  // which is a false negative stated as a fact. NUL separation also lets a path containing a space
  // or a newline survive the round trip, where line-splitting plus `.trim()` corrupted it.
  const res = run(repo, ['diff', '--name-only', '-z', from, to, '--'])
  if (res.status !== 0) return null
  const paths = res.stdout.split('\0').filter(Boolean)
  return [...new Set(paths)].sort()
}

/**
 * Sorted set intersection of two path lists. Membership is symmetric, so the caller may pass the
 * two sides in either order and get the same answer.
 * @returns {string[]}
 */
export function intersectPaths(left, right) {
  const rightSet = new Set(right)
  return [...new Set(left.filter((p) => rightSet.has(p)))].sort()
}

/**
 * The pull request number a base commit subject attributes itself to, or null when it names none.
 * @param {string} subject
 * @returns {number|null}
 */
export function pullRequestFromSubject(subject) {
  const text = String(subject ?? '')
  const m = MERGE_COMMIT_PR.exec(text) || SQUASH_MERGE_PR.exec(text)
  return m ? Number(m[1]) : null
}

// A path-limited `git log` never shows the merge commit of an ordinary merge-commit landing: the
// merge is TREESAME to the side branch it merged, so history simplification walks past it and
// reports the side-branch commits instead. Their subjects carry the developer's own wording, not
// the forge's `Merge pull request #N`, so `MERGE_COMMIT_PR` is effectively unreachable through that
// traversal and a whole landing convention silently attributes to nothing. `--first-parent` does
// not rescue it — with a pathspec it drops the merge as well, and returns nothing at all. So look
// the landing up directly, per commit, and only for the commits the subject scan could not place.
// Capped: this is one extra pair of git reads per unresolved commit, and an overlap can be wide.
export const MAX_LANDING_LOOKUPS = 25

/**
 * The merge commit that first carried `sha` onto `base`, or null when none did (a rebase or
 * fast-forward landing) or the lookup could not run.
 * @returns {{sha: string, subject: string}|null}
 */
export function mergeCommitLanding({ repo, base, sha, run = runGit }) {
  const res = run(repo, ['rev-list', '--merges', '--ancestry-path', `${sha}..${base}`, '--'])
  if (res.status !== 0) return null
  const ids = res.stdout
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => OID.test(line))
  // `rev-list` prints newest first, so the OLDEST entry is the merge that landed this commit;
  // every later one merely contains it.
  const landing = ids[ids.length - 1]
  if (!landing) return null
  const subject = run(repo, ['log', '-1', `--format=%s`, landing, '--'])
  if (subject.status !== 0) return null
  return { sha: landing, subject: subject.stdout.trim() }
}

/**
 * Attribute the base commits that touched `paths` back to the pull requests that landed them.
 * Scoped to the overlap on purpose: a base PR that changed nothing this branch also changed is not
 * what the reviewer is being asked about. A commit whose subject names no PR is looked up against
 * the merge that landed it, and is reported in `unattributed` when that finds nothing too — rather
 * than dropped, since an omitted commit reads as an overlap nobody landed.
 * @returns {{status: 'ok'|'unevaluated',
 *   commits: Array<{sha: string, subject: string, pr: number|null, landedBy?: string}>,
 *   prs: number[], unattributed: Array<{sha: string, subject: string}>, reason: string|null}}
 */
export function attributeBaseCommits({ repo, base, head = 'HEAD', paths = [], run = runGit }) {
  const result = { status: UNEVALUATED, commits: [], prs: [], unattributed: [], reason: null }
  if (!Array.isArray(paths) || paths.length === 0) {
    result.reason = 'no overlapping paths to attribute'
    return result
  }
  // `:(literal)` — these are paths git handed us, not patterns a caller typed. Passed bare, a file
  // legitimately named `star*.txt` is a glob that also matches `starfish.txt`, and the base PRs
  // that touched *those* files get attributed to an overlap they never had.
  const pathspecs = paths.map((p) => `:(literal)${p}`)
  const res = run(repo, [
    'log',
    `--format=%h${FIELD_SEP}%s`,
    `${head}..${base}`,
    '--',
    ...pathspecs,
  ])
  if (res.status !== 0) {
    const detail = res.stderr.trim().split('\n')[0] || `git log exited ${res.status}`
    result.reason = `commit log for the overlapping paths failed: ${detail}`
    return result
  }
  result.status = ATTRIBUTED
  const scanned = []
  for (const line of res.stdout.split('\n')) {
    const sep = line.indexOf(FIELD_SEP)
    if (sep < 0) continue
    const sha = line.slice(0, sep).trim()
    const subject = line.slice(sep + 1).trim()
    if (!sha) continue
    scanned.push({ sha, subject, pr: pullRequestFromSubject(subject) })
  }
  let lookups = 0
  let capped = false
  for (const commit of scanned) {
    if (commit.pr === null) {
      if (lookups < MAX_LANDING_LOOKUPS) {
        lookups += 1
        const landing = mergeCommitLanding({ repo, base, sha: commit.sha, run })
        const pr = landing ? pullRequestFromSubject(landing.subject) : null
        if (pr !== null) {
          commit.pr = pr
          commit.landedBy = landing.sha
        }
      } else {
        capped = true
      }
    }
    result.commits.push(commit)
    if (commit.pr === null) result.unattributed.push({ sha: commit.sha, subject: commit.subject })
    else if (!result.prs.includes(commit.pr)) result.prs.push(commit.pr)
  }
  if (capped) {
    result.reason = `landing-merge lookup capped at ${MAX_LANDING_LOOKUPS} commit(s); some of the unattributed commits may carry a PR this report does not name`
  }
  return result
}

/**
 * Stage 2 probe: would merging `base` into `head` conflict textually?
 * @returns {{status: 'clean'|'conflicts'|'unevaluated', paths: string[], reason: string|null}}
 */
export function mergeTreeStatus({ repo, base, head = 'HEAD', run = runGit }) {
  const res = run(repo, ['merge-tree', '--write-tree', base, head])
  const lines = res.stdout.split('\n')
  const first = (lines[0] || '').trim()
  if (!OID.test(first)) {
    const detail = (res.stderr.trim() || first || 'no tree object id on stdout').split('\n')[0]
    return { status: UNEVALUATED, paths: [], reason: `merge-tree did not run: ${detail}` }
  }
  if (res.status === 0) return { status: MERGE_TREE_CLEAN, paths: [], reason: null }
  if (res.status !== 1) {
    return { status: UNEVALUATED, paths: [], reason: `merge-tree exited ${res.status}` }
  }
  const paths = []
  for (const line of lines.slice(1)) {
    const m = CONFLICT_LINE.exec(line)
    if (m) paths.push(m[1])
  }
  return { status: MERGE_TREE_CONFLICTS, paths: [...new Set(paths)].sort(), reason: null }
}

/**
 * Full two-stage drift report.
 * @param {{repo: string, base: string, head?: string, run?: Function}} opts
 * @returns {{base: string, head: string, behind: number|'unevaluated', mergeBase: string|'unevaluated',
 *   stage2: boolean, branchPaths: string[], basePaths: string[], intersection: string[],
 *   mergeTree: 'clean'|'conflicts'|'skipped'|'unevaluated', conflictPaths: string[],
 *   notes: string[]}}
 */
export function baseDrift({ repo, base, head = 'HEAD', fetchFailed = false, run = runGit }) {
  const notes = []
  const report = {
    base,
    head,
    fetchFailed: Boolean(fetchFailed),
    behind: UNEVALUATED,
    mergeBase: UNEVALUATED,
    stage2: false,
    branchPaths: [],
    basePaths: [],
    intersection: [],
    mergeTree: UNEVALUATED,
    conflictPaths: [],
    attribution: { status: UNEVALUATED, commits: [], prs: [], unattributed: [], reason: null },
    notes,
  }

  // A ref the caller could not refresh describes the base branch as it was at some unknown earlier
  // moment. Every field below would be computed against that, and `behind: 0` would then be a
  // proven-unmoved verdict nobody actually obtained — so stop here instead.
  if (report.fetchFailed) {
    notes.push(
      "the base ref could not be refreshed (git fetch failed); it may predate the base branch's real tip",
    )
    return report
  }

  const baseOid = resolveCommit({ repo, rev: base, run })
  const headOid = resolveCommit({ repo, rev: head, run })
  if (!baseOid) notes.push(`base ref ${base} does not resolve to a commit`)
  if (!headOid) notes.push(`head ref ${head} does not resolve to a commit`)
  if (!baseOid || !headOid) return report

  report.behind = countBehind({ repo, base, head, run })
  if (report.behind === UNEVALUATED) {
    notes.push('commit count could not be read; treat the base as possibly moved')
    return report
  }
  if (report.behind === 0) {
    // Not 'unevaluated': there is nothing left to evaluate. An unmoved base cannot overlap.
    report.mergeTree = MERGE_TREE_SKIPPED
    notes.push('base has not moved; stage 2 skipped')
    return report
  }

  const mb = run(repo, ['merge-base', base, head])
  if (mb.status !== 0 || !OID.test(mb.stdout.trim())) {
    // Deliberately NOT a `mergeTree` of 'skipped': with no merge base there is no way to tell
    // whether the two sides overlap, so this stays the initialised 'unevaluated' — an unanswered
    // question, not an unneeded one.
    notes.push('no merge base between the branch and the base ref; overlap could not be computed')
    return report
  }
  report.mergeBase = mb.stdout.trim()
  report.stage2 = true

  const branchPaths = changedPaths({ repo, from: report.mergeBase, to: head, run })
  const basePaths = changedPaths({ repo, from: report.mergeBase, to: base, run })
  if (branchPaths === null || basePaths === null) {
    notes.push('changed-path diff failed; intersection could not be computed')
    return report
  }
  report.branchPaths = branchPaths
  report.basePaths = basePaths
  report.intersection = intersectPaths(branchPaths, basePaths)

  if (report.intersection.length === 0) {
    // Not 'unevaluated' either: the two change sets are disjoint, so the probe had nothing to
    // answer. This is the ordinary "the base moved, but somewhere else" round.
    report.mergeTree = MERGE_TREE_SKIPPED
    notes.push('no path intersection; merge-tree skipped')
    return report
  }

  report.attribution = attributeBaseCommits({
    repo,
    base,
    head,
    paths: report.intersection,
    run,
  })
  if (report.attribution.reason) notes.push(report.attribution.reason)

  const probe = mergeTreeStatus({ repo, base, head, run })
  report.mergeTree = probe.status
  report.conflictPaths = probe.paths
  if (probe.reason) notes.push(probe.reason)
  return report
}

// `do not merge` is the merge-gate token a PR title or body is matched against, so a base commit
// subject that happens to contain it would wedge THIS branch's PR the moment the drift note is
// published under `## Autonomous decisions`. The note is not review text — it is quoted from other
// people's commits — so the "rephrase the finding" rule cannot reach it; the emitter has to.
// The replacement is deliberately NOT the hyphenated spelling: prose in these skills uses
// `do-not-merge` as the marker's own name, so substituting it would swap the literal for its
// documented alias inside the very section a merge gate reads. A neutral placeholder says what
// happened and resembles nothing.
const MERGE_GATE_TOKEN = /do(\s+)not(\s+)merge/gi
export const MERGE_GATE_REDACTION = '[merge-gate token redacted]'
export function redactMergeGateToken(text) {
  return String(text ?? '').replace(MERGE_GATE_TOKEN, MERGE_GATE_REDACTION)
}

// Same reason the attributed list spells `PR 123`: a forge turns every `#N` in a pull-request body
// into a cross-reference event on N. Quoted commit subjects are the wider path, not the narrower
// one — a repo whose convention is `[#1234]` matches neither anchored landing pattern, so EVERY
// overlapping commit lands in `unattributed` and its subject is published verbatim.
const CROSS_REFERENCE = /#(\d+)/g
export function deCrossReference(text) {
  return String(text ?? '').replace(CROSS_REFERENCE, 'PR $1')
}

// Everything quoted out of a base commit goes through both: the merge-gate token would wedge this
// PR, and a bare `#N` would write to someone else's.
export function sanitizeQuotedSubject(subject) {
  return deCrossReference(redactMergeGateToken(subject))
}

function attributionClause(report) {
  const a = report.attribution
  if (!a || a.status !== ATTRIBUTED) {
    return ` Landing PRs: UNEVALUATED (${(a && a.reason) || 'attribution was not attempted'}).`
  }
  if (a.commits.length === 0) {
    // Reports what the log RETURNED, not what the base branch contains: an empty result is the
    // absence of a record, and stating it as the absence of a commit asserts more than the field
    // establishes — the same overclaim the disjoint note was gated against above.
    return ' Landing PRs: none recorded — no base commit in this range is recorded as touching the shared paths.'
  }
  const parts = []
  // `PR 123`, never `#123`. This clause is published into a pull-request body, where a forge turns
  // every `#N` into a cross-reference event on PR N — so the bare form would post a "referenced this
  // pull request" notification onto every overlapping PR, writing to other people's work as a side
  // effect of reading our own. Backticks would suppress it on one forge and not in a plain-text
  // comment, so drop the sigil instead of hiding it.
  if (a.prs.length > 0) parts.push(`landed by ${a.prs.map((n) => `PR ${n}`).join(', ')}`)
  if (a.unattributed.length > 0) {
    const listed = a.unattributed
      .map((c) => `${c.sha} "${sanitizeQuotedSubject(c.subject)}"`)
      .join('; ')
    parts.push(`${a.unattributed.length} commit(s) carry no PR reference (${listed})`)
  }
  // The landing-lookup cap belongs HERE, not only in `notes`: the two note branches that carry this
  // clause — the clean overlap and the conflicting one — never interpolate `notes`, so a truncated
  // lookup would publish "N commit(s) carry no PR reference" with no hint that some of them were
  // never looked up. That is a stated absence standing in for an unasked question.
  if (a.reason) parts.push(a.reason)
  return ` Landing PRs: ${parts.join('; ')}.`
}

/**
 * One-line human summary of a drift report, for a reviewer brief.
 * @returns {string}
 */
export function formatDriftNote(report) {
  if (report.fetchFailed) {
    return `Base drift: UNEVALUATED for ${report.base} — ${report.notes.join('; ') || 'the base ref could not be refreshed (git fetch failed)'}. This is NOT "no drift"; nothing was compared against the base branch's real tip.`
  }
  if (report.behind === UNEVALUATED) {
    return `Base drift: UNEVALUATED for ${report.base} — ${report.notes.join('; ') || 'reason unrecorded'}. Treat the base as possibly moved.`
  }
  if (report.behind === 0)
    return `Base drift: none. ${report.base} carries no commit this branch lacks.`
  const moved = `${report.base} moved ahead by ${report.behind} commit(s)`
  if (!report.stage2)
    return `Base drift: ${moved}; overlap UNEVALUATED — ${report.notes.join('; ')}.`
  // Gate the disjoint verdict on the value that MEANS disjoint, never on an empty `intersection`.
  // A failed changed-path diff also leaves `intersection` empty — with `stage2` already true and
  // both path lists empty — so reading emptiness as disjointness published "the two change sets
  // are disjoint (0 branch path(s), 0 base path(s)). No overlap to review." for a comparison that
  // never ran. That is the same false-clean the fetch-failure path already refuses, on the one
  // surface a human and the round reviewer actually read.
  if (report.mergeTree === MERGE_TREE_SKIPPED) {
    return `Base drift: ${moved}, but the two change sets are disjoint (${report.branchPaths.length} branch path(s), ${report.basePaths.length} base path(s)). No overlap to review.`
  }
  if (report.intersection.length === 0) {
    return `Base drift: ${moved}; overlap UNEVALUATED — ${report.notes.join('; ') || 'reason unrecorded'}. This is NOT "no overlap"; the two change sets were never compared.`
  }
  const shared = report.intersection.join(', ')
  if (report.mergeTree === MERGE_TREE_CONFLICTS) {
    return `Base drift: ${moved} and overlaps this branch on: ${shared}.${attributionClause(report)} The merge is textually CONFLICTING (${report.conflictPaths.join(', ') || 'paths unlisted'}) — review the rebased result, not the pre-rebase diff.`
  }
  if (report.mergeTree === MERGE_TREE_CLEAN) {
    return `Base drift: ${moved} and overlaps this branch on: ${shared}.${attributionClause(report)} The merge is textually CLEAN, so the overlap is SEMANTIC and invisible to git — check these paths for duplicated or contradicted work.`
  }
  return `Base drift: ${moved} and overlaps this branch on: ${shared}.${attributionClause(report)} Textual mergeability is UNEVALUATED (${report.notes.join('; ') || 'reason unrecorded'}) — assume the overlap is unreviewed.`
}

if (isMainModule(import.meta.url)) {
  const argv = process.argv.slice(2)
  const [cmd, ...rest] = argv
  const opts = { repo: process.cwd(), base: '', head: 'HEAD', fetchFailed: false }
  for (let i = 0; i < rest.length; i += 1) {
    const flag = rest[i]
    const value = rest[i + 1]
    if (flag === '--fetch-failed') {
      opts.fetchFailed = true
    } else if (flag === '--repo' || flag === '--base' || flag === '--head') {
      if (value === undefined) {
        process.stderr.write(`base-drift: ${flag} needs a value\n`)
        process.exit(2)
      }
      opts[flag.slice(2)] = value
      i += 1
    } else {
      process.stderr.write(`base-drift: unknown argument ${flag}\n`)
      process.exit(2)
    }
  }
  if (cmd !== 'check') {
    process.stderr.write(
      'usage: node base-drift.mjs check --base <ref> [--head <ref>] [--repo <dir>] [--fetch-failed]\n',
    )
    process.exit(2)
  }
  if (!opts.base) {
    process.stderr.write('base-drift: --base is required\n')
    process.exit(2)
  }
  const report = baseDrift(opts)
  const note = formatDriftNote(report)
  process.stderr.write(`${note}\n`)
  process.stdout.write(`${JSON.stringify({ ...report, note })}\n`)
}
