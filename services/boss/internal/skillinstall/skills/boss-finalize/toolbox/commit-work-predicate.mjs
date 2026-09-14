#!/usr/bin/env node

import { readFileSync } from 'node:fs'

import { isMainModule } from './main-module.mjs'

// commit-work-predicate.mjs — the single definition of "is this a non-empty work
// commit", i.e. the unit a `[#PR]` tag policy is really about. Every consumer asks
// this module rather than re-deriving the answer: the commit-message policy hook,
// the PR-tag injector's pre-rebase scan and its post-condition, and the readiness
// guard's prose. Two implementations of this predicate that disagree with each other
// is the exact defect this module exists to remove.
//
// OWNERSHIP. The authoritative definition lives in Go, in
// services/bossd/internal/git/worktree.go: DraftPRPlaceholderCommitSubject and
// draftPRPlaceholderTagRE, interpreted by IsDraftPRPlaceholderSubject. Go cannot be
// imported from CommonJS or bash, so this module is a *pinned re-expression* of it,
// exactly as skills-toolbox/sweep-pr-gate.sh already re-expresses the same pair as an
// ERE. The literals below are NOT read from the Go source at runtime — this module
// ships into repos that have no worktree.go — they are held byte-identical to it by
// commit-work-predicate.test.mjs, which derives both from the Go source rather than
// retyping them. A retyped literal here would stay green after the Go constant moved,
// which is precisely the drift being pinned.
//
// PURITY. Nothing here shells out. The emptiness question takes the commit's tree and
// its parent's tree as inputs, so the caller resolves its own trees from wherever it
// legitimately can: the injector reads them from `git log`, while the commit-message
// hook runs BEFORE the commit object exists and must compare the PENDING tree against
// HEAD's. A predicate that ran `git show <sha>` could not answer the hook's question
// at all.

/**
 * The subject of the empty bootstrap commit a daemon creates to give a branch a diff
 * so the forge will open a pull request for it. Pinned to Go's
 * DraftPRPlaceholderCommitSubject.
 */
export const BOOTSTRAP_COMMIT_SUBJECT = 'chore: [skip ci] create pull request'

// The conventional-commit "type: " / "type(scope): " prefix, including its trailing
// whitespace. Deliberately loose (Go's draftPRPlaceholderConventionalPrefixRE is the
// same shape): its looseness is neutralised by the whole-subject equality check that
// follows it, which no amount of prefix tolerance can turn into a false positive.
const CONVENTIONAL_PREFIX_RE = /^[A-Za-z][0-9A-Za-z-]*(\([^)]*\))?!?:[\t\n\v\f\r ]+/

// A RUN of tags, each followed by a single literal space — not one tag. Tags STACK:
// an injector run for one PR number followed by a run for another can leave
// `chore: [#7] [#42] [skip ci] create pull request`, and a single-tag strip would
// classify that historical subject as real work. Pinned to Go's
// draftPRPlaceholderTagRE. `[skip ci]` can never be eaten by it: the tag shape
// requires `#` followed by digits.
const TAG_RUN_RE = /^(\[#[0-9]+\] )+/

/**
 * Remove a whole run of `[#N] ` tags sitting immediately after the conventional-commit
 * prefix. Returns the subject unchanged when there is no prefix or no tag run, and is
 * idempotent: the `+` consumes every tag in the run, so a second application finds
 * nothing left to strip.
 *
 * Normalisation is for MEMBERSHIP TESTS ONLY. Callers that display, log, or amend a
 * subject must keep using the original, unmodified text.
 */
export function stripTagRun(subject) {
  const text = String(subject ?? '')
  const prefix = text.match(CONVENTIONAL_PREFIX_RE)?.[0]
  if (!prefix) return text
  const rest = text.slice(prefix.length)
  const tags = rest.match(TAG_RUN_RE)?.[0]
  if (!tags) return text
  return prefix + rest.slice(tags.length)
}

/**
 * Whether `subject` is the bootstrap placeholder, tolerating any run of injected PR
 * tags after the conventional prefix.
 *
 * The decision is WHOLE-SUBJECT equality against the constant, never a substring or a
 * prefix match. That is the entire bound on this exemption: stripping
 * tags off genuine work yields genuine work — `feat(boss): [#1] [#2] add X` becomes
 * `feat(boss): add X`, which is not the constant — so no amount of tag stripping can
 * make a real commit exempt, and a subject that merely CONTAINS the placeholder
 * wording stays non-exempt.
 */
export function isBootstrapSubject(subject) {
  const trimmed = String(subject ?? '').trim()
  if (trimmed === BOOTSTRAP_COMMIT_SUBJECT) return true
  return stripTagRun(trimmed) === BOOTSTRAP_COMMIT_SUBJECT
}

/**
 * Whether a commit changes nothing, decided by TREE COMPARISON and never by subject
 * text. `parentTree` is the first parent's tree, or the repository's empty-tree object
 * id for a root commit — the caller supplies it because the empty tree depends on the
 * repository's hash algorithm and only git can compute it.
 *
 * Fails SAFE: a missing or unreadable tree on either side answers "not empty", so an
 * unresolvable commit is treated as real work that still needs a tag rather than
 * silently exempted.
 */
export function isEmptyCommit(tree, parentTree) {
  const a = String(tree ?? '').trim()
  const b = String(parentTree ?? '').trim()
  if (a === '' || b === '') return false
  return a === b
}

/**
 * The composed question: is this commit exempt from carrying a `[#PR]` tag?
 *
 * Exemption requires BOTH halves — the bootstrap subject AND emptiness. Either alone
 * is an over-broad exemption, which is the dangerous direction: subject alone would
 * let a hand-written commit claim the exemption by copying a string, and emptiness
 * alone is the injector's (correct, narrower) concern rather than a licence to publish
 * untagged work.
 */
export function isExemptBootstrapCommit({ subject, tree, parentTree } = {}) {
  return isBootstrapSubject(subject) && isEmptyCommit(tree, parentTree)
}

/**
 * The inverse, in the shape callers reason in: this commit is real work and must carry
 * a tag.
 */
export function isTaggableWorkCommit({ subject, tree, parentTree } = {}) {
  return !isExemptBootstrapCommit({ subject, tree, parentTree })
}

/**
 * Classify a batch of commits by emptiness alone.
 *
 * `rows` are `{ sha, tree, parents }` records straight out of
 * `git log --format='%H%x09%T%x09%P'`. A commit's parent tree is looked up from the
 * batch itself, so the caller supplies one extra row for the commit just below the
 * range (its tree is needed, its own emptiness is not asked). A row whose first parent
 * is absent from the batch is UNRESOLVABLE and reported as non-empty — the same
 * fail-safe direction as isEmptyCommit.
 */
export function selectEmptyCommits(rows, emptyTree) {
  const treeBySha = new Map()
  for (const row of rows) treeBySha.set(row.sha, row.tree)
  const empty = []
  for (const row of rows) {
    const firstParent = row.parents?.[0]
    const parentTree = firstParent === undefined ? emptyTree : treeBySha.get(firstParent)
    if (parentTree === undefined) continue
    if (isEmptyCommit(row.tree, parentTree)) empty.push(row.sha)
  }
  return empty
}

/**
 * Parse `sha<TAB>tree<TAB>parent parent[<TAB>subject]` lines. Blank lines and short
 * rows are dropped.
 *
 * The fourth field is OPTIONAL and its absence is meaningful: a row with no subject is
 * CONTEXT ONLY — the extra row below the range that supplies a parent tree — and is
 * never itself graded. Everything after the third tab is the subject, because a commit
 * subject may legitimately contain a tab and splitting on it would truncate one.
 */
export function parseCommitRows(text) {
  const rows = []
  for (const line of String(text ?? '').split('\n')) {
    if (line.trim() === '') continue
    const fields = line.split('\t')
    const [sha, tree, parents = ''] = fields
    if (!sha || !tree) continue
    const row = {
      sha: sha.trim(),
      tree: tree.trim(),
      parents: parents.trim().split(/\s+/).filter(Boolean),
    }
    if (fields.length > 3) row.subject = fields.slice(3).join('\t')
    rows.push(row)
  }
  return rows
}

/**
 * The commits in `rows` that are real work and do NOT carry `tag` in their subject —
 * the set a tag-state re-derivation may legitimately call `partial`.
 *
 * It exists because a re-derivation that graded EVERY subject in the range was asking
 * a STRONGER question than the injector answers. The injector skips a known-empty
 * commit before any amend, by design, so an empty commit (the daemon's bootstrap
 * placeholder, say) survives untagged inside the graded range and the grader reported
 * `partial` for a branch the injector had fully tagged. Both sides now ask this module.
 *
 * Rows with no `subject` are context-only and are never graded. Emptiness is decided
 * by tree comparison through selectEmptyCommits, so an UNRESOLVABLE commit counts as
 * real work that still owes a tag — the same fail-safe direction as everywhere else
 * here, and the one that keeps this from becoming a licence to publish untagged work.
 */
export function selectUntaggedWorkCommits(rows, { emptyTree, tag } = {}) {
  const needle = String(tag ?? '')
  const empty = new Set(selectEmptyCommits(rows, emptyTree))
  const out = []
  for (const row of rows) {
    if (row.subject === undefined) continue
    if (empty.has(row.sha)) continue
    if (needle !== '' && row.subject.includes(needle)) continue
    out.push({ sha: row.sha, subject: row.subject })
  }
  return out
}

const USAGE = `usage: commit-work-predicate.mjs <command> [flags]

commands:
  empty-commits --empty-tree <oid>
      reads "<sha>\\t<tree>\\t<parents>" rows on stdin, prints the sha of each empty commit

  untagged-work --empty-tree <oid> --tag <tag>
      reads "<sha>\\t<tree>\\t<parents>[\\t<subject>]" rows on stdin, prints
      "<sha>\\t<subject>" for each NON-EMPTY commit whose subject lacks <tag>.
      A row with no subject is context only (the extra row below the range) and is
      never graded. Empty stdout means every work commit carries the tag.

  --help, -h, help
      print this message`

/**
 * CLI entry point. RETURNS an exit code and never calls process.exit, so a caller can
 * test it in-process: 0 on success, 2 on a usage error. IO is injected.
 */
export function runCli(argv = [], io = {}) {
  const stdout = io.stdout ?? ((text) => process.stdout.write(text))
  const stderr = io.stderr ?? ((text) => process.stderr.write(text))
  const [command, ...rest] = argv

  if (command === '--help' || command === '-h' || command === 'help') {
    stdout(`${USAGE}\n`)
    return 0
  }
  if (command !== 'empty-commits' && command !== 'untagged-work') {
    stderr(
      `${command === undefined ? 'missing command' : `unknown command: ${command}`}\n${USAGE}\n`,
    )
    return 2
  }

  const flagValue = (name) => {
    const at = rest.indexOf(name)
    const value = at === -1 ? undefined : rest[at + 1]
    return value === undefined || value.startsWith('--') ? undefined : value
  }

  const emptyTree = flagValue('--empty-tree')
  if (!emptyTree) {
    stderr(`missing required --empty-tree <oid>\n${USAGE}\n`)
    return 2
  }

  const rows = parseCommitRows(io.stdin ?? '')
  if (command === 'empty-commits') {
    const empty = selectEmptyCommits(rows, emptyTree.trim())
    stdout(empty.length === 0 ? '' : `${empty.join('\n')}\n`)
    return 0
  }

  // An absent or empty --tag is a USAGE error, never "nothing carries the tag": the
  // latter would report every work commit as untagged, which reads as a catastrophic
  // injector failure caused by a typo'd flag.
  const tag = flagValue('--tag')
  if (!tag) {
    stderr(`missing required --tag <tag>\n${USAGE}\n`)
    return 2
  }
  const untagged = selectUntaggedWorkCommits(rows, { emptyTree: emptyTree.trim(), tag })
  stdout(
    untagged.length === 0 ? '' : `${untagged.map((c) => `${c.sha}\t${c.subject}`).join('\n')}\n`,
  )
  return 0
}

if (isMainModule(import.meta.url)) {
  // Read stdin LAZILY. Reading it up front blocks forever on a terminal, so
  // `commit-work-predicate.mjs --help` typed by hand hung instead of printing —
  // the listless-help failure in a different costume. runCli touches `io.stdin`
  // only on a command that consumes rows, so help and every usage error return
  // without ever touching the descriptor.
  let stdin
  const io = {
    get stdin() {
      if (stdin === undefined) {
        try {
          stdin = readFileSync(0, 'utf8')
        } catch {
          stdin = ''
        }
      }
      return stdin
    },
  }
  process.exitCode = runCli(process.argv.slice(2), io)
}
