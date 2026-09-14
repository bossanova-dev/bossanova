#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  BOOTSTRAP_COMMIT_SUBJECT,
  isBootstrapSubject,
  isEmptyCommit,
  isExemptBootstrapCommit,
  isTaggableWorkCommit,
  parseCommitRows,
  runCli,
  selectEmptyCommits,
  selectUntaggedWorkCommits,
  stripTagRun,
} from './commit-work-predicate.mjs'

const rootDir = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')

const EMPTY_TREE = '4b825dc642cb6eb9a060e54bf8d69288fbee4904'
const TREE_A = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
const TREE_B = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'

// ---------------------------------------------------------------------------
// Cross-language ownership pin. Go owns the definition; this module is a
// re-expression of it. Derive BOTH the subject and the tag shape from the Go
// source — a retyped literal here would stay green after the Go constant moved,
// which is exactly the drift being pinned. Same idiom as
// scripts/sweep-pr-gate.test.mjs' pin over the shell guard.
// ---------------------------------------------------------------------------

test('the bootstrap subject is the one read from worktree.go, not a retyped literal', () => {
  const goSource = fs.readFileSync(
    path.join(rootDir, 'services', 'bossd', 'internal', 'git', 'worktree.go'),
    'utf8',
  )
  const subjectMatch = goSource.match(/const DraftPRPlaceholderCommitSubject = "([^"]+)"/)
  assert.ok(subjectMatch, 'DraftPRPlaceholderCommitSubject must be readable from worktree.go')
  assert.equal(
    BOOTSTRAP_COMMIT_SUBJECT,
    subjectMatch[1],
    'BOOTSTRAP_COMMIT_SUBJECT must equal Go DraftPRPlaceholderCommitSubject byte for byte',
  )

  const tagMatch = goSource.match(
    /draftPRPlaceholderTagRE = regexp\.MustCompile\(`\^\((.+?)\)\+`\)/,
  )
  assert.ok(tagMatch, 'draftPRPlaceholderTagRE must be readable from worktree.go')
  // Build the tag run Go would strip and prove this module strips the same bytes. The
  // Go shape is `\[#[0-9]+\] `; instantiating it keeps the assertion honest without
  // re-implementing a regex parser here.
  assert.equal(tagMatch[1], '\\[#[0-9]+\\] ', 'the Go tag shape changed; re-derive this pin')
  const prefix = BOOTSTRAP_COMMIT_SUBJECT.slice(0, BOOTSTRAP_COMMIT_SUBJECT.indexOf(': ') + 2)
  const rest = BOOTSTRAP_COMMIT_SUBJECT.slice(prefix.length)
  assert.equal(stripTagRun(`${prefix}[#7] [#42] ${rest}`), BOOTSTRAP_COMMIT_SUBJECT)
})

// ---------------------------------------------------------------------------
// Subject recognition.
// ---------------------------------------------------------------------------

test('the untagged bootstrap subject is recognised', () => {
  assert.equal(isBootstrapSubject(BOOTSTRAP_COMMIT_SUBJECT), true)
})

test('the singly-tagged bootstrap subject is recognised', () => {
  assert.equal(isBootstrapSubject('chore: [#4242] [skip ci] create pull request'), true)
})

test('a run of several tags is recognised, and stripping is idempotent', () => {
  const stacked = 'chore: [#7] [#42] [#9] [skip ci] create pull request'
  assert.equal(isBootstrapSubject(stacked), true)
  const once = stripTagRun(stacked)
  assert.equal(once, BOOTSTRAP_COMMIT_SUBJECT)
  assert.equal(stripTagRun(once), once, 'stripping must be idempotent')
})

test('a subject that merely CONTAINS the bootstrap wording is not recognised', () => {
  // R4: whole-subject equality, never substring. Each of these embeds the constant.
  for (const subject of [
    'fix(hook): chore: [skip ci] create pull request',
    'chore: [skip ci] create pull request and tag it',
    'revert: "chore: [skip ci] create pull request"',
    'x chore: [skip ci] create pull request',
  ]) {
    assert.equal(isBootstrapSubject(subject), false, subject)
  }
})

test('a real work commit does not become exempt by having its tags stripped', () => {
  // The R4 failure this module exists to prevent: stripping tags off genuine work
  // must yield genuine work, not the placeholder.
  const tagged = 'feat(boss): [#1] [#2] add X'
  assert.equal(stripTagRun(tagged), 'feat(boss): add X')
  assert.equal(isBootstrapSubject(tagged), false)
  assert.equal(isBootstrapSubject(stripTagRun(tagged)), false)
})

test('a tag run that is not the placeholder leaves a non-placeholder subject', () => {
  assert.equal(isBootstrapSubject('chore: [#7] [skip ci] create pull requests'), false)
  assert.equal(isBootstrapSubject('chore(deps): [#7] [skip ci] create pull request'), false)
})

// ---------------------------------------------------------------------------
// Emptiness — established by tree comparison, never by subject text.
// ---------------------------------------------------------------------------

test('an empty commit is detected by tree equality with its parent', () => {
  assert.equal(isEmptyCommit(TREE_A, TREE_A), true)
  assert.equal(isEmptyCommit(TREE_A, TREE_B), false)
})

test('a root commit carrying the empty tree is detected', () => {
  assert.equal(isEmptyCommit(EMPTY_TREE, EMPTY_TREE), true)
  assert.equal(isEmptyCommit(TREE_A, EMPTY_TREE), false)
})

test('an unresolvable tree answers "not empty" rather than exempting the commit', () => {
  assert.equal(isEmptyCommit('', ''), false, 'two blanks must not compare equal')
  assert.equal(isEmptyCommit(undefined, undefined), false)
  assert.equal(isEmptyCommit(TREE_A, undefined), false)
})

test('two commits sharing a subject are not conflated: emptiness is per-commit trees', () => {
  const subject = 'chore: [skip ci] create pull request'
  // Same subject, different tree relationships — the empty one is exempt, the
  // non-empty one is real work that still owes a tag.
  assert.equal(isExemptBootstrapCommit({ subject, tree: TREE_A, parentTree: TREE_A }), true)
  assert.equal(isExemptBootstrapCommit({ subject, tree: TREE_B, parentTree: TREE_A }), false)
  assert.equal(isTaggableWorkCommit({ subject, tree: TREE_B, parentTree: TREE_A }), true)
})

// ---------------------------------------------------------------------------
// The composed question.
// ---------------------------------------------------------------------------

test('exemption requires BOTH the bootstrap subject and emptiness', () => {
  const empty = { tree: TREE_A, parentTree: TREE_A }
  const nonEmpty = { tree: TREE_B, parentTree: TREE_A }
  assert.equal(isExemptBootstrapCommit({ subject: BOOTSTRAP_COMMIT_SUBJECT, ...empty }), true)
  assert.equal(isExemptBootstrapCommit({ subject: BOOTSTRAP_COMMIT_SUBJECT, ...nonEmpty }), false)
  assert.equal(isExemptBootstrapCommit({ subject: 'feat(x): real work', ...empty }), false)
  assert.equal(isExemptBootstrapCommit({ subject: 'feat(x): real work', ...nonEmpty }), false)
  assert.equal(isExemptBootstrapCommit(), false)
})

test('isTaggableWorkCommit is the inverse of the exemption', () => {
  const commit = { subject: BOOTSTRAP_COMMIT_SUBJECT, tree: TREE_A, parentTree: TREE_A }
  assert.equal(isTaggableWorkCommit(commit), false)
  assert.equal(isTaggableWorkCommit({ ...commit, tree: TREE_B }), true)
})

// ---------------------------------------------------------------------------
// Batch classification + CLI.
// ---------------------------------------------------------------------------

test('parseCommitRows reads git log rows and drops blanks and short rows', () => {
  assert.deepEqual(parseCommitRows(`s1\t${TREE_A}\tp1 p2\n\ns2\t${TREE_B}\t\nbroken\n`), [
    { sha: 's1', tree: TREE_A, parents: ['p1', 'p2'] },
    { sha: 's2', tree: TREE_B, parents: [] },
  ])
})

test('selectEmptyCommits resolves parent trees from the batch', () => {
  const rows = [
    { sha: 'base', tree: TREE_A, parents: ['outside'] },
    { sha: 'empty', tree: TREE_A, parents: ['base'] },
    { sha: 'work', tree: TREE_B, parents: ['empty'] },
  ]
  // `base`'s own parent is outside the batch: unresolvable, so it is not reported.
  assert.deepEqual(selectEmptyCommits(rows, EMPTY_TREE), ['empty'])
})

test('selectEmptyCommits treats a parentless root commit against the supplied empty tree', () => {
  assert.deepEqual(
    selectEmptyCommits([{ sha: 'root', tree: EMPTY_TREE, parents: [] }], EMPTY_TREE),
    ['root'],
  )
  assert.deepEqual(selectEmptyCommits([{ sha: 'root', tree: TREE_A, parents: [] }], EMPTY_TREE), [])
})

test('runCli prints the empty commits and returns 0', () => {
  let out = ''
  const code = runCli(['empty-commits', '--empty-tree', EMPTY_TREE], {
    stdin: `base\t${TREE_A}\toutside\nempty\t${TREE_A}\tbase\nwork\t${TREE_B}\tempty\n`,
    stdout: (t) => {
      out += t
    },
  })
  assert.equal(code, 0)
  assert.equal(out, 'empty\n')
})

test('runCli returns 2 on a missing argument and never exits the process', () => {
  let err = ''
  const io = {
    stderr: (t) => {
      err += t
    },
  }
  assert.equal(runCli(['empty-commits'], io), 2, 'missing --empty-tree is a usage error')
  assert.match(err, /--empty-tree/)
  assert.equal(
    runCli(['empty-commits', '--empty-tree'], io),
    2,
    'a valueless flag is a usage error',
  )
  assert.equal(runCli([], io), 2, 'no command at all is a usage error')
  assert.equal(runCli(['nope'], io), 2, 'an unknown command is a usage error')
})

// --- untagged-work: the tag-state re-derivation's predicate --------------------

const TAG = '[#42]'

// One context row below the range (no subject, never graded), then: the daemon's
// empty bootstrap placeholder, a tagged work commit, and an untagged work commit.
const TAG_ROWS = [
  { sha: 'base', tree: TREE_A, parents: ['outside'] },
  { sha: 'bootstrap', tree: TREE_A, parents: ['base'], subject: BOOTSTRAP_COMMIT_SUBJECT },
  { sha: 'tagged', tree: TREE_B, parents: ['bootstrap'], subject: 'feat: [#42] add x' },
  { sha: 'untagged', tree: 'c'.repeat(40), parents: ['tagged'], subject: 'fix: y' },
]

test('selectUntaggedWorkCommits exempts the commits the injector is entitled to skip', () => {
  // The whole defect: the injector skips a known-EMPTY commit before any amend, so the
  // bootstrap placeholder survives untagged inside the graded range. A re-derivation
  // that graded every subject called that branch `partial` when it was fully tagged.
  const got = selectUntaggedWorkCommits(TAG_ROWS, { emptyTree: EMPTY_TREE, tag: TAG })
  assert.deepEqual(got, [{ sha: 'untagged', subject: 'fix: y' }])
})

test('selectUntaggedWorkCommits reports nothing when every work commit carries the tag', () => {
  const rows = TAG_ROWS.filter((r) => r.sha !== 'untagged')
  assert.deepEqual(selectUntaggedWorkCommits(rows, { emptyTree: EMPTY_TREE, tag: TAG }), [])
})

test('selectUntaggedWorkCommits never grades a context-only row', () => {
  // The base row carries no subject and its own emptiness is unresolvable, so grading
  // it would report the commit BELOW the range as untagged work.
  const got = selectUntaggedWorkCommits(TAG_ROWS, { emptyTree: EMPTY_TREE, tag: TAG })
  assert.ok(!got.some((c) => c.sha === 'base'))
})

test('selectUntaggedWorkCommits treats an unresolvable commit as work that owes a tag', () => {
  // Fail-safe direction: an unresolvable parent must NOT become a licence to publish
  // untagged work.
  const rows = [{ sha: 'orphan', tree: TREE_A, parents: ['missing'], subject: 'fix: y' }]
  assert.deepEqual(selectUntaggedWorkCommits(rows, { emptyTree: EMPTY_TREE, tag: TAG }), [
    { sha: 'orphan', subject: 'fix: y' },
  ])
})

test('parseCommitRows keeps a subject with a tab in it whole', () => {
  const rows = parseCommitRows(`s1\t${TREE_A}\tp1\tfeat: a\tb\n`)
  assert.equal(rows[0].subject, 'feat: a\tb')
  // A row with only three fields has NO subject key, which is how a context-only row
  // is distinguished from one whose subject is the empty string.
  assert.equal(parseCommitRows(`s2\t${TREE_B}\tp1\n`)[0].subject, undefined)
})

test('the untagged-work CLI prints sha and subject for each untagged work commit', () => {
  let out = ''
  let err = ''
  const stdin = [
    `base\t${TREE_A}\toutside`,
    `bootstrap\t${TREE_A}\tbase\t${BOOTSTRAP_COMMIT_SUBJECT}`,
    `untagged\t${TREE_B}\tbootstrap\tfix: y`,
  ].join('\n')
  const code = runCli(['untagged-work', '--empty-tree', EMPTY_TREE, '--tag', TAG], {
    stdin,
    stdout: (t) => (out += t),
    stderr: (t) => (err += t),
  })
  assert.equal(code, 0)
  assert.equal(err, '')
  assert.equal(out, 'untagged\tfix: y\n')
})

test('the untagged-work CLI refuses a missing --tag rather than reporting everything', () => {
  // An absent --tag must not read as "nothing carries the tag": that reports every work
  // commit as untagged, which looks like a catastrophic injector failure caused by a typo.
  let out = ''
  let err = ''
  const code = runCli(['untagged-work', '--empty-tree', EMPTY_TREE], {
    stdin: `c1\t${TREE_A}\tbase\tfix: y`,
    stdout: (t) => (out += t),
    stderr: (t) => (err += t),
  })
  assert.equal(code, 2)
  assert.equal(out, '')
  assert.match(err, /missing required --tag/)
})

test('the CLI answers --help with exit 0 and both commands', () => {
  for (const flag of ['--help', '-h', 'help']) {
    let out = ''
    let err = ''
    const code = runCli([flag], { stdout: (t) => (out += t), stderr: (t) => (err += t) })
    assert.equal(code, 0)
    assert.equal(err, '')
    assert.match(out, /empty-commits/)
    assert.match(out, /untagged-work/)
  }
})

test('an unknown command still prints the usage block', () => {
  let err = ''
  const code = runCli(['bogus'], { stdout: () => {}, stderr: (t) => (err += t) })
  assert.equal(code, 2)
  assert.match(err, /unknown command: bogus/)
  assert.match(err, /untagged-work/)
})
