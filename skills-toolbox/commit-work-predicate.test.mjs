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
