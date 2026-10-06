// Post-rebase audit: name what a rebase that exited 0 silently changed.
//
// A clean `git rebase` exit proves only that every patch applied textually. It says nothing about
// whether the rebased branch still carries what the branch meant: git can drop a commit whose patch
// the base already has, a conflict taken wholesale from one side can lose the other side's lines,
// the base can add a test over a set the branch extended, move a convention the branch still
// writes to, or reference a name the branch deleted. Each of those merges cleanly into a broken
// branch. This helper turns each shape into one mechanical detector, so the caller acts on a named
// finding instead of a prose reminder.
//
// Read-only, offline and non-mutating like base-drift.mjs, whose git runner and changed-path
// helper it reuses: no fetch, no checkout, no ref or worktree write. Every probe reads commit and
// tree objects that are already local.
//
// Definitions (all revs resolved to object ids up front):
//   M0           merge-base(preRebaseHead, base) — the branch's OLD fork point
//   branchDiff   M0..preRebaseHead — what the branch changed before the rebase
//   baseAdvance  M0..base          — what the base changed underneath it
//   postDiff     base..HEAD        — what the branch changes now, after the rebase
//
// Fail-closed contract: a detector that could not run is 'unevaluated', and any unevaluated
// detector makes the top-level verdict 'unevaluated' — never 'clean'. 'clean' means every detector
// ran and every list is empty. 'findings' is advisory: every list names something to LOOK at; none
// of them is a reason to stop on its own.

import path from 'node:path'

import { changedPaths, resolveCommit, runGit } from './base-drift.mjs'
import { isMainModule } from './main-module.mjs'

export const VERDICT_CLEAN = 'clean'
export const VERDICT_FINDINGS = 'findings'
export const UNEVALUATED = 'unevaluated'
export const DETECTOR_OK = 'ok'

export const REASON_PATCH_UPSTREAM = 'patch-already-upstream'
export const REASON_DROPPED_EMPTY = 'dropped-empty'

// Every reported list is bounded. Hitting a bound names the detector in `truncated` and adds a
// note; it never changes the verdict, because a truncated list is still a non-empty one.
export const MAX_FINDINGS = 50
export const MAX_LOST_LINES = 10
export const MAX_LOST_PATHS = 50
// Paths whose leftover lines get checked against the base blob. Each costs one `git show`.
export const MAX_BLOB_READS = 400
export const MAX_COMMIT_PATHS = 50
export const MAX_HEAD_HITS = 20
// Tree probes per token detector, AFTER narrowing (see detectRetiredByBase). Each is one
// `git grep -q`, which scans the whole tree when the token is absent.
export const MAX_TOKEN_PROBES = 200
const LS_TREE_BATCH = 300

export const RESIDUAL =
  'Not detected: purely semantic breakage that loses no line (a changed default, a reordered call), a symbol the base renamed rather than deleted, a branch line rewritten during conflict resolution (it reads as kept), and a convention change whose old and new paths do not share a directory. Refactored call shapes are not checked; grep for them.'

export const FINDING_LISTS = [
  'skippedCommits',
  'lostAdditions',
  'baseAddedTests',
  'baseRelocations',
  'retiredByBase',
  'conventionSuspects',
  'deletedByBranch',
]
// One status per detector. Detector 4b reports two lists, so it owns two entries.
const DETECTORS = FINDING_LISTS

// The OID shape base-drift.mjs accepts, for the few raw `git` answers resolveCommit does not cover.
const OID = /^[0-9a-f]{40}(?:[0-9a-f]{24})?$/
const PATH_TOKEN = /[\w.<>/-]+/g
const IDENT_TOKEN = /[A-Za-z_][A-Za-z0-9_]{3,}/g
const TEST_FILE = [
  /_test\.go$/,
  /\.test\.[^/]+$/,
  /\.spec\.[^/]+$/,
  /_test\.[^/.]+$/,
  /^test_[^/]*\.py$/,
]

function fail(detail, res) {
  const why = (res && (res.stderr || '').trim().split('\n')[0]) || ''
  return `${detail}${why ? `: ${why}` : res ? ` (git exited ${res.status})` : ''}`
}

function sortedUnique(items) {
  return [...new Set(items)].sort()
}

function dirOf(p) {
  const d = path.posix.dirname(p)
  return d === '' ? '.' : d
}

/** C-style unquote of a path git printed inside double quotes (core.quotePath). */
export function unquoteGitPath(text) {
  if (!text.startsWith('"') || !text.endsWith('"')) return text
  const body = text.slice(1, -1)
  const bytes = []
  const simple = { n: 10, t: 9, r: 13, b: 8, f: 12, a: 7, v: 11, '"': 34, '\\': 92 }
  for (let i = 0; i < body.length; i += 1) {
    const ch = body[i]
    if (ch !== '\\') {
      bytes.push(...Buffer.from(ch, 'utf8'))
      continue
    }
    const next = body[i + 1]
    if (/[0-7]/.test(next || '')) {
      bytes.push(parseInt(body.slice(i + 1, i + 4), 8))
      i += 3
    } else {
      bytes.push(simple[next] ?? next.charCodeAt(0))
      i += 1
    }
  }
  return Buffer.from(bytes).toString('utf8')
}

function stripPrefix(raw, prefix) {
  const p = unquoteGitPath(raw.trim())
  if (p === '/dev/null') return null
  return p.startsWith(prefix) ? p.slice(prefix.length) : p
}

/**
 * Added and removed lines of `git diff from to`, each tagged with its path. Rename detection is
 * forced on so a renamed file contributes only the lines that actually changed, whatever the
 * caller's diff.renames config says. Returns null when the diff cannot be taken.
 * @returns {{added: Array<{path: string, line: string}>, removed: Array<{path: string, line: string}>}|null}
 */
export function diffLines({ repo, from, to, run = runGit }) {
  const res = run(repo, [
    '-c',
    'core.quotePath=false',
    'diff',
    '--no-color',
    '--no-ext-diff',
    '--no-textconv',
    '--src-prefix=a/',
    '--dst-prefix=b/',
    '-M',
    '-U0',
    from,
    to,
    '--',
  ])
  if (res.status !== 0) return null
  const added = []
  const removed = []
  let inHeader = false
  let oldPath = null
  let newPath = null
  for (const line of res.stdout.split('\n')) {
    if (line.startsWith('diff --git ')) {
      inHeader = true
      oldPath = null
      newPath = null
      continue
    }
    if (inHeader) {
      if (line.startsWith('--- ')) oldPath = stripPrefix(line.slice(4), 'a/')
      else if (line.startsWith('+++ ')) newPath = stripPrefix(line.slice(4), 'b/')
      else if (line.startsWith('@@')) inHeader = false
      continue
    }
    if (line.startsWith('@@')) continue
    if (line.startsWith('+') && newPath !== null) added.push({ path: newPath, line: line.slice(1) })
    else if (line.startsWith('-') && oldPath !== null)
      removed.push({ path: oldPath, line: line.slice(1) })
  }
  return { added, removed }
}

/**
 * `git diff --name-status -z -M from to` as entries. Returns null when the diff cannot be taken.
 * @returns {Array<{status: string, from: string, to: string|null}>|null}
 */
export function nameStatus({ repo, from, to, run = runGit }) {
  const res = run(repo, ['diff', '--name-status', '-z', '-M', '--no-color', from, to, '--'])
  if (res.status !== 0) return null
  const parts = res.stdout.split('\0')
  const entries = []
  for (let i = 0; i < parts.length;) {
    const status = parts[i]
    if (!status) {
      i += 1
      continue
    }
    const letter = status[0]
    if (letter === 'R' || letter === 'C') {
      entries.push({ status: letter, from: parts[i + 1], to: parts[i + 2] })
      i += 3
    } else {
      entries.push({ status: letter, from: parts[i + 1], to: letter === 'D' ? null : parts[i + 1] })
      i += 2
    }
  }
  return entries
}

/**
 * Whether `token` occurs in the tree of `rev` (`git grep -q`, which stops at the first hit).
 * Returns null when the grep fails. One probe per token on purpose: `git grep -o` with many fixed
 * patterns is pathologically slow on a real tree, so the token detectors narrow their candidates
 * to a handful before probing instead of batching the whole set.
 * @returns {boolean|null}
 */
export function tokenInTree({ repo, rev, token, word = false, run = runGit }) {
  const res = run(repo, ['grep', '-q', '-I', '-F', ...(word ? ['-w'] : []), '-e', token, rev, '--'])
  if (res.status === 0) return true
  if (res.status === 1) return false
  return null
}

/** Files in the tree of `rev` that contain `token`, or null when the grep fails. */
export function tokenHits({ repo, rev, token, word = false, run = runGit }) {
  const res = run(repo, ['grep', '-l', '-I', '-F', ...(word ? ['-w'] : []), '-e', token, rev, '--'])
  if (res.status === 1) return []
  if (res.status !== 0) return null
  const prefix = `${rev}:`
  return sortedUnique(
    res.stdout
      .split('\n')
      .filter(Boolean)
      .map((l) => (l.startsWith(prefix) ? l.slice(prefix.length) : l)),
  )
}

/** Which of `paths` exist as files in the tree of `rev`, or null when the listing fails. */
export function pathsInTree({ repo, rev, paths, run = runGit }) {
  const found = new Set()
  for (let i = 0; i < paths.length; i += LS_TREE_BATCH) {
    const batch = paths.slice(i, i + LS_TREE_BATCH)
    const res = run(repo, ['ls-tree', '-r', '-z', '--name-only', rev, '--', ...batch])
    if (res.status !== 0) return null
    const wanted = new Set(batch)
    for (const p of res.stdout.split('\0')) if (wanted.has(p)) found.add(p)
  }
  return found
}

function unevaluated(reason) {
  return { status: UNEVALUATED, items: [], reason, truncated: false }
}

function ok(items, truncated = false, reason = null) {
  return { status: DETECTOR_OK, items, reason, truncated }
}

/**
 * Detector 1 — commits the rebase dropped. `git cherry` marks a commit `-` when its patch-id is
 * already upstream; a `+` commit whose subject no longer appears in base..HEAD was dropped as
 * empty. Neither is presumed inert: a skipped regeneration commit leaves a generated file stale.
 */
export function detectSkippedCommits({ repo, preRebaseHead, base, head, run = runGit }) {
  const cherry = run(repo, ['cherry', base, preRebaseHead])
  if (cherry.status !== 0) return unevaluated(fail('git cherry failed', cherry))
  const log = run(repo, ['log', '--no-merges', '--format=%s', `${base}..${head}`, '--'])
  if (log.status !== 0) return unevaluated(fail('git log base..HEAD failed', log))
  const kept = new Set(log.stdout.split('\n').map((s) => s.trim()))
  const items = []
  for (const line of cherry.stdout.split('\n')) {
    const m = /^([+-]) ([0-9a-f]+)$/.exec(line.trim())
    if (!m) continue
    const sha = m[2]
    const subj = run(repo, ['log', '-1', '--format=%s', sha, '--'])
    if (subj.status !== 0) return unevaluated(fail(`reading commit ${sha} failed`, subj))
    const subject = subj.stdout.trim()
    let reason = null
    if (m[1] === '-') reason = REASON_PATCH_UPSTREAM
    else if (!kept.has(subject)) reason = REASON_DROPPED_EMPTY
    if (!reason) continue
    const show = run(repo, ['show', '--no-renames', '--name-only', '-z', '--format=', sha, '--'])
    if (show.status !== 0) return unevaluated(fail(`listing paths of ${sha} failed`, show))
    const paths = sortedUnique(show.stdout.split('\0').filter(Boolean))
    items.push({ sha, subject, reason, paths: paths.slice(0, MAX_COMMIT_PATHS) })
  }
  items.sort((a, b) => a.sha.localeCompare(b.sha))
  return ok(items.slice(0, MAX_FINDINGS), items.length > MAX_FINDINGS)
}

// A line made only of punctuation (`}`, `});`, `*/`) carries no meaning a reader could act on, and
// matching it across the whole diff is a coin toss — the lines around it carry the signal.
function meaningfulLine(raw) {
  const line = raw.trim()
  return line && /[A-Za-z0-9]/.test(line) ? line : null
}

/**
 * Detector 2 — lines the branch wrote that no longer exist anywhere in its effective change. The
 * net branch diff's added lines, as a multiset matched across the WHOLE diff (so a base-side file
 * move does not read as loss), minus the post-rebase diff's added lines, minus any line the new
 * base already carries at that path (the base adopted it).
 */
export function detectLostAdditions({
  repo,
  oldMergeBase,
  preRebaseHead,
  base,
  head,
  run = runGit,
}) {
  const branch = diffLines({ repo, from: oldMergeBase, to: preRebaseHead, run })
  if (!branch) return unevaluated('the pre-rebase branch diff could not be taken')
  const post = diffLines({ repo, from: base, to: head, run })
  if (!post) return unevaluated('the post-rebase diff could not be taken')
  const postCount = new Map()
  for (const { line } of post.added) {
    const key = meaningfulLine(line)
    if (key) postCount.set(key, (postCount.get(key) || 0) + 1)
  }
  const leftover = new Map()
  for (const { path: p, line } of branch.added) {
    const key = meaningfulLine(line)
    if (!key) continue
    const n = postCount.get(key) || 0
    if (n > 0) {
      postCount.set(key, n - 1)
      continue
    }
    if (!leftover.has(p)) leftover.set(p, [])
    leftover.get(p).push(key)
  }
  const candidates = [...leftover.keys()].sort()
  const scanned = candidates.slice(0, MAX_BLOB_READS)
  const onBase = pathsInTree({ repo, rev: base, paths: scanned, run })
  if (!onBase) return unevaluated('listing the base tree failed')
  const items = []
  for (const p of scanned) {
    let lines = leftover.get(p)
    if (onBase.has(p)) {
      const blob = run(repo, ['show', `${base}:${p}`])
      if (blob.status !== 0) return unevaluated(fail(`reading ${p} on the base failed`, blob))
      const adopted = new Set(blob.stdout.split('\n').map((l) => l.trim()))
      lines = lines.filter((l) => !adopted.has(l))
    }
    if (lines.length === 0) continue
    items.push({ path: p, lines: lines.slice(0, MAX_LOST_LINES), count: lines.length })
  }
  const truncated = candidates.length > scanned.length || items.length > MAX_LOST_PATHS
  return ok(items.slice(0, MAX_LOST_PATHS), truncated)
}

function looksLikeTest(p) {
  const name = path.posix.basename(p)
  return TEST_FILE.some((re) => re.test(name))
}

/**
 * Detector 3 — tests the base added or changed in a directory the branch touched. These must be
 * run by name: an affected-test selection keyed to the branch's own diff is not proof they ran.
 */
export function detectBaseAddedTests({ repo, oldMergeBase, preRebaseHead, base, run = runGit }) {
  const touched = changedPaths({ repo, from: oldMergeBase, to: preRebaseHead, run })
  if (!touched) return unevaluated('the branch changed-path list could not be taken')
  const baseSide = nameStatus({ repo, from: oldMergeBase, to: base, run })
  if (!baseSide) return unevaluated('the base-advance name-status could not be taken')
  const branchDirs = new Set(touched.map(dirOf))
  const items = []
  for (const e of baseSide) {
    if (!['A', 'M', 'R', 'C'].includes(e.status) || !e.to) continue
    if (!looksLikeTest(e.to) || !branchDirs.has(dirOf(e.to))) continue
    items.push({ path: e.to, branchDir: dirOf(e.to) })
  }
  items.sort((a, b) => a.path.localeCompare(b.path))
  return ok(items.slice(0, MAX_FINDINGS), items.length > MAX_FINDINGS)
}

/**
 * Detector 4a — base renames and deletions under something the branch wrote: the old path itself,
 * or its directory. The branch may still be writing to where the base no longer looks.
 */
export function detectBaseRelocations({ repo, oldMergeBase, preRebaseHead, base, run = runGit }) {
  const branchSide = nameStatus({ repo, from: oldMergeBase, to: preRebaseHead, run })
  if (!branchSide) return unevaluated('the branch name-status could not be taken')
  const baseSide = nameStatus({ repo, from: oldMergeBase, to: base, run })
  if (!baseSide) return unevaluated('the base-advance name-status could not be taken')
  const written = sortedUnique(
    branchSide.filter((e) => ['A', 'M', 'R', 'C'].includes(e.status) && e.to).map((e) => e.to),
  )
  const items = []
  for (const e of baseSide) {
    if (e.status !== 'R' && e.status !== 'D') continue
    const dir = dirOf(e.from)
    const branchPaths = written.filter((p) => p === e.from || dirOf(p) === dir)
    if (branchPaths.length === 0) continue
    items.push({
      from: e.from,
      to: e.status === 'R' ? e.to : null,
      status: e.status === 'R' ? 'renamed' : 'deleted',
      branchPaths: branchPaths.slice(0, MAX_FINDINGS),
    })
  }
  items.sort((a, b) => a.from.localeCompare(b.from))
  return ok(items.slice(0, MAX_FINDINGS), items.length > MAX_FINDINGS)
}

/** Path-like tokens (containing `/`) in a set of diff lines, normalized and de-duplicated. */
export function pathTokens(lines) {
  const out = new Set()
  for (const { line } of lines) {
    for (const raw of line.match(PATH_TOKEN) || []) {
      const token = raw.replace(/^[/-]+/, '').replace(/[.-]+$/, '')
      if (token.length >= 3 && token.includes('/') && /[A-Za-z0-9]/.test(token)) out.add(token)
    }
  }
  return [...out].sort()
}

/** Identifier-shaped tokens (4+ characters) in a set of diff lines. */
export function identTokens(lines) {
  const out = new Set()
  for (const { line } of lines) for (const t of line.match(IDENT_TOKEN) || []) out.add(t)
  return [...out].sort()
}

function tokenDir(token) {
  const i = token.lastIndexOf('/')
  return i <= 0 ? '' : token.slice(0, i)
}

/**
 * Detector 4b — path conventions the base retired. A path-like token on the base's removed lines
 * that the old fork point's tree carries and the new base's tree does not is retired; any HEAD
 * still contains is reported (`retiredByBase`). A path-like token the branch added that the base
 * tree lacks, sitting in a retired token's directory, is a `conventionSuspects` entry: advisory —
 * judge it, do not auto-fix it.
 *
 * Candidates are narrowed before any tree probe, without changing the answer: HEAD is the base
 * plus postDiff, so a token the base tree lacks can occur in HEAD only on a postDiff added line.
 * A retired token is therefore reportable only if postDiff's added text contains it, and it can
 * anchor a suspect only if its directory is one a postDiff path token sits in.
 * @returns {{retired: object, suspects: object}}
 */
export function detectRetiredByBase({ repo, oldMergeBase, base, head, run = runGit }) {
  const both = (why) => ({ retired: unevaluated(why), suspects: unevaluated(why) })
  const baseDiff = diffLines({ repo, from: oldMergeBase, to: base, run })
  if (!baseDiff) return both('the base-advance diff could not be taken')
  const post = diffLines({ repo, from: base, to: head, run })
  if (!post) return both('the post-rebase diff could not be taken')
  const postText = post.added.map((a) => a.line).join('\n')
  const postTokens = pathTokens(post.added)
  const postDirs = new Set(postTokens.map(tokenDir).filter(Boolean))
  let candidates = pathTokens(baseDiff.removed).filter(
    (t) => postText.includes(t) || postDirs.has(tokenDir(t)),
  )
  let reason = null
  const truncatedProbe = candidates.length > MAX_TOKEN_PROBES
  if (truncatedProbe) {
    candidates = candidates.slice(0, MAX_TOKEN_PROBES)
    reason = `retired-path probe capped at ${MAX_TOKEN_PROBES} candidate tokens`
  }
  const retiredTokens = []
  for (const token of candidates) {
    const onBase = tokenInTree({ repo, rev: base, token, run })
    if (onBase === null) return both(`grepping the base tree for ${token} failed`)
    if (onBase) continue
    const onFork = tokenInTree({ repo, rev: oldMergeBase, token, run })
    if (onFork === null) return both(`grepping the old fork point for ${token} failed`)
    if (onFork) retiredTokens.push(token)
  }
  const retiredItems = []
  for (const token of retiredTokens.filter((t) => postText.includes(t))) {
    const hits = tokenHits({ repo, rev: head, token, run })
    if (!hits) return both(`grepping HEAD for ${token} failed`)
    if (hits.length > 0) retiredItems.push({ token, headHits: hits.slice(0, MAX_HEAD_HITS) })
  }
  const retired = ok(
    retiredItems.slice(0, MAX_FINDINGS),
    truncatedProbe || retiredItems.length > MAX_FINDINGS,
    reason,
  )

  const byDir = new Map()
  for (const t of retiredTokens) {
    const d = tokenDir(t)
    if (d && !byDir.has(d)) byDir.set(d, t)
  }
  const retiredSet = new Set(retiredTokens)
  const added = postTokens.filter((t) => !retiredSet.has(t) && byDir.has(tokenDir(t)))
  const existing = pathsInTree({ repo, rev: base, paths: added, run })
  if (!existing) return { retired, suspects: unevaluated('listing the base tree failed') }
  const suspectItems = []
  for (const token of added.slice(0, MAX_TOKEN_PROBES)) {
    if (existing.has(token)) continue
    const onBase = tokenInTree({ repo, rev: base, token, run })
    if (onBase === null) {
      return { retired, suspects: unevaluated(`grepping the base tree for ${token} failed`) }
    }
    if (!onBase) suspectItems.push({ token, retiredSibling: byDir.get(tokenDir(token)) })
  }
  const truncatedSuspects = added.length > MAX_TOKEN_PROBES || suspectItems.length > MAX_FINDINGS
  return { retired, suspects: ok(suspectItems.slice(0, MAX_FINDINGS), truncatedSuspects) }
}

/**
 * Detector 5 — names the branch deleted everywhere that HEAD references again. Identifier tokens
 * from the branch's removed lines with no whole-word hit left in the pre-rebase tree are names the
 * branch retired; a hit in HEAD means the base brought a reference back (often in a comment, which
 * nothing compiles). Narrowed the same way as detector 4b: a word the pre-rebase tree lacks can
 * occur in HEAD only on an added line of `git diff preRebaseHead HEAD`.
 */
export function detectDeletedByBranch({ repo, oldMergeBase, preRebaseHead, head, run = runGit }) {
  const branch = diffLines({ repo, from: oldMergeBase, to: preRebaseHead, run })
  if (!branch) return unevaluated('the pre-rebase branch diff could not be taken')
  const since = diffLines({ repo, from: preRebaseHead, to: head, run })
  if (!since) return unevaluated('the pre-rebase-to-HEAD diff could not be taken')
  const reintroduced = new Set(identTokens(since.added))
  let candidates = identTokens(branch.removed).filter((t) => reintroduced.has(t))
  let reason = null
  const truncated = candidates.length > MAX_TOKEN_PROBES
  if (truncated) {
    candidates = candidates.slice(0, MAX_TOKEN_PROBES)
    reason = `deleted-identifier probe capped at ${MAX_TOKEN_PROBES} candidate tokens`
  }
  const items = []
  for (const token of candidates) {
    const onPre = tokenInTree({ repo, rev: preRebaseHead, token, word: true, run })
    if (onPre === null) return unevaluated(`grepping the pre-rebase tree for ${token} failed`)
    if (onPre) continue
    const hits = tokenHits({ repo, rev: head, token, word: true, run })
    if (!hits) return unevaluated(`grepping HEAD for ${token} failed`)
    if (hits.length > 0) items.push({ token, headHits: hits.slice(0, MAX_HEAD_HITS) })
  }
  return ok(items.slice(0, MAX_FINDINGS), truncated || items.length > MAX_FINDINGS, reason)
}

function emptyReport({ preRebaseHead, base, head }) {
  return {
    verdict: UNEVALUATED,
    preRebaseHead,
    base,
    head,
    oldMergeBase: null,
    skippedCommits: [],
    lostAdditions: [],
    baseAddedTests: [],
    baseRelocations: [],
    retiredByBase: [],
    conventionSuspects: [],
    deletedByBranch: [],
    detectors: Object.fromEntries(DETECTORS.map((d) => [d, UNEVALUATED])),
    truncated: [],
    notes: [],
    residual: RESIDUAL,
  }
}

/**
 * The full audit. Call it after a rebase onto `base` finished, with the HEAD the branch had
 * before the rebase started.
 * @param {{repo: string, preRebaseHead: string, base: string, head?: string, run?: Function}} opts
 */
export function postRebaseAudit({ repo, preRebaseHead, base, head = 'HEAD', run = runGit }) {
  const report = emptyReport({ preRebaseHead, base, head })
  const { notes } = report
  const pre = preRebaseHead ? resolveCommit({ repo, rev: preRebaseHead, run }) : null
  const baseOid = base ? resolveCommit({ repo, rev: base, run }) : null
  const headOid = resolveCommit({ repo, rev: head, run })
  if (!pre)
    notes.push(`--pre-rebase-head ${preRebaseHead || '(empty)'} does not resolve to a commit`)
  if (!baseOid) notes.push(`--base ${base || '(empty)'} does not resolve to a commit`)
  if (!headOid) notes.push(`--head ${head} does not resolve to a commit`)
  if (!pre || !baseOid || !headOid) return report
  Object.assign(report, { preRebaseHead: pre, base: baseOid, head: headOid })

  if (pre === headOid) {
    for (const d of Object.keys(report.detectors)) report.detectors[d] = DETECTOR_OK
    report.verdict = VERDICT_CLEAN
    notes.push('the pre-rebase head equals HEAD: no rebase happened, so nothing changed under it')
    return report
  }
  const mb = run(repo, ['merge-base', pre, baseOid])
  const m0 = mb.stdout.trim()
  if (mb.status !== 0 || !OID.test(m0)) {
    notes.push(fail('the pre-rebase head and the base share no merge base', mb))
    return report
  }
  report.oldMergeBase = m0
  const contains = run(repo, ['merge-base', '--is-ancestor', baseOid, headOid])
  if (contains.status !== 0) {
    notes.push(
      contains.status === 1
        ? 'HEAD does not contain the base: run the audit after the rebase onto that base'
        : fail('checking that HEAD contains the base failed', contains),
    )
    return report
  }

  const ctx = { repo, oldMergeBase: m0, preRebaseHead: pre, base: baseOid, head: headOid, run }
  const record = (name, result) => {
    report.detectors[name] = result.status
    if (result.truncated) report.truncated.push(name)
    if (result.reason) notes.push(`${name}: ${result.reason}`)
  }
  const skipped = detectSkippedCommits(ctx)
  record('skippedCommits', skipped)
  report.skippedCommits = skipped.items
  const lost = detectLostAdditions(ctx)
  record('lostAdditions', lost)
  report.lostAdditions = lost.items
  const tests = detectBaseAddedTests(ctx)
  record('baseAddedTests', tests)
  report.baseAddedTests = tests.items
  const moved = detectBaseRelocations(ctx)
  record('baseRelocations', moved)
  report.baseRelocations = moved.items
  const { retired, suspects } = detectRetiredByBase(ctx)
  record('retiredByBase', retired)
  report.retiredByBase = retired.items
  record('conventionSuspects', suspects)
  report.conventionSuspects = suspects.items
  const deleted = detectDeletedByBranch(ctx)
  record('deletedByBranch', deleted)
  report.deletedByBranch = deleted.items

  report.truncated.sort()
  if (Object.values(report.detectors).some((s) => s !== DETECTOR_OK)) report.verdict = UNEVALUATED
  else if (FINDING_LISTS.some((k) => report[k].length > 0)) report.verdict = VERDICT_FINDINGS
  else report.verdict = VERDICT_CLEAN
  return report
}

/** One-line human summary, for a decisions log or a reviewer brief. */
export function formatAuditNote(report) {
  if (report.verdict === UNEVALUATED) {
    const failed = Object.entries(report.detectors)
      .filter(([, s]) => s !== DETECTOR_OK)
      .map(([d]) => d)
    return `Post-rebase audit: UNEVALUATED (${failed.join(', ') || 'no detector ran'}) — ${report.notes.join('; ') || 'reason unrecorded'}. This is NOT clean.`
  }
  if (report.verdict === VERDICT_CLEAN) {
    return `Post-rebase audit: clean.${report.notes.length ? ` ${report.notes.join('; ')}.` : ''}`
  }
  const parts = FINDING_LISTS.filter((k) => report[k].length > 0).map(
    (k) => `${k} ${report[k].length}${report.truncated.includes(k) ? '+' : ''}`,
  )
  return `Post-rebase audit: findings — ${parts.join(', ')}.`
}

export const POST_REBASE_AUDIT_USAGE = `usage: node post-rebase-audit.mjs check --pre-rebase-head <sha> --base <ref> [--head <ref>] [--repo <dir>]

subcommands:
  check --pre-rebase-head <sha> --base <ref>
      After a rebase onto <ref>, report what it silently changed: commits it
      skipped, branch lines it lost, base-added tests in directories the branch
      touched, base relocations and retired paths the branch still writes, and
      names the branch deleted that HEAD references again. Writes a one-line
      note to stderr and the JSON report to stdout. Verdict is clean, findings,
      or unevaluated (never clean when a detector could not run).
      --head <ref>      audit this ref instead of HEAD
      --repo <dir>      run against this checkout instead of the cwd

  --help, -h
      Print this message and exit 0.
`

if (isMainModule(import.meta.url)) {
  const argv = process.argv.slice(2)
  if (argv.includes('--help') || argv.includes('-h') || argv[0] === 'help') {
    process.stdout.write(POST_REBASE_AUDIT_USAGE)
    process.exit(0)
  }
  const [cmd, ...rest] = argv
  const opts = { repo: process.cwd(), preRebaseHead: '', base: '', head: 'HEAD' }
  const FLAGS = {
    '--repo': 'repo',
    '--pre-rebase-head': 'preRebaseHead',
    '--base': 'base',
    '--head': 'head',
  }
  for (let i = 0; i < rest.length; i += 1) {
    const key = FLAGS[rest[i]]
    if (!key) {
      process.stderr.write(
        `post-rebase-audit: unknown argument ${rest[i]}\n${POST_REBASE_AUDIT_USAGE}`,
      )
      process.exit(2)
    }
    if (rest[i + 1] === undefined) {
      process.stderr.write(`post-rebase-audit: ${rest[i]} needs a value\n`)
      process.exit(2)
    }
    opts[key] = rest[i + 1]
    i += 1
  }
  if (cmd !== 'check') {
    process.stderr.write(POST_REBASE_AUDIT_USAGE)
    process.exit(2)
  }
  // A missing flag is still graded rather than rejected: the report says which rev did not
  // resolve and stays 'unevaluated', so a caller that lost its captured pre-rebase head gets a
  // not-clean answer on stdout instead of a usage error it might read as "nothing to report".
  const report = postRebaseAudit(opts)
  process.stderr.write(`${formatAuditNote(report)}\n`)
  process.stdout.write(`${JSON.stringify(report)}\n`)
}
