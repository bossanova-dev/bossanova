// Scenario coverage for base-drift.mjs.
//
// Every scenario runs against a real on-disk git repository rather than a mocked porcelain, because
// the contract this helper owes its caller is about git's actual exit statuses and output shapes —
// notably that `git merge-tree --write-tree` exits 1 for BOTH a content conflict and an unmergeable
// ref argument, which is why a conflict verdict is gated on an object id and not on the status.

import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  MERGE_TREE_CLEAN,
  MERGE_TREE_CONFLICTS,
  MERGE_TREE_SKIPPED,
  UNEVALUATED,
  baseDrift,
  changedPaths,
  countBehind,
  formatDriftNote,
  intersectPaths,
  mergeTreeStatus,
  deCrossReference,
  pullRequestFromSubject,
  redactMergeGateToken,
  runGit as realGit,
} from './base-drift.mjs'

const HELPER = fileURLToPath(new URL('./base-drift.mjs', import.meta.url))
const BASE_REF = 'refs/remotes/origin/main'

const GIT_ENV = {
  ...process.env,
  GIT_AUTHOR_NAME: 'Drift Test',
  GIT_AUTHOR_EMAIL: 'drift@example.invalid',
  GIT_COMMITTER_NAME: 'Drift Test',
  GIT_COMMITTER_EMAIL: 'drift@example.invalid',
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_SYSTEM: '/dev/null',
}

function git(cwd, args) {
  const res = spawnSync('git', args, { cwd, encoding: 'utf8', env: GIT_ENV })
  assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`)
  return res.stdout
}

function write(dir, file, body) {
  fs.writeFileSync(path.join(dir, file), body)
}

function commit(dir, message) {
  git(dir, ['add', '-A'])
  git(dir, ['commit', '-q', '-m', message])
}

/**
 * A repo whose branch `work` forked from `main`, with `refs/remotes/origin/main` standing in for the
 * already-fetched base ref. `baseCommits` lands on the base side after the fork; `branchCommits`
 * lands on the branch side. Neither side ever touches a remote — the tracking ref is set locally.
 */
function makeRepo({ baseFiles = {}, branchFiles = {}, baseCommits = [] } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'base-drift-'))
  git(dir, ['init', '-q', '-b', 'main'])
  // Fixture repos must never invoke the developer's signing key.
  git(dir, ['config', 'commit.gpgsign', 'false'])
  write(dir, 'shared.txt', 'original\n')
  write(dir, 'branch-only.txt', 'original\n')
  write(dir, 'base-only.txt', 'original\n')
  commit(dir, 'fork point')
  const fork = git(dir, ['rev-parse', 'HEAD']).trim()

  git(dir, ['checkout', '-q', '-b', 'work'])
  for (const [file, body] of Object.entries(branchFiles)) write(dir, file, body)
  if (Object.keys(branchFiles).length > 0) commit(dir, 'branch work')

  git(dir, ['checkout', '-q', 'main'])
  for (const [file, body] of Object.entries(baseFiles)) write(dir, file, body)
  if (Object.keys(baseFiles).length > 0) commit(dir, 'base work')
  // `baseCommits` is how a scenario controls the *subjects* on the base side — PR attribution is
  // parsed out of them, so a fixture that cannot set a subject cannot test the attribution at all.
  for (const { files = {}, message } of baseCommits) {
    for (const [file, body] of Object.entries(files)) write(dir, file, body)
    commit(dir, message)
  }
  const baseTip = git(dir, ['rev-parse', 'HEAD']).trim()
  git(dir, ['update-ref', BASE_REF, baseTip])
  // Delete the local base branch so nothing can accidentally read it instead of the tracking ref.
  git(dir, ['checkout', '-q', 'work'])
  git(dir, ['branch', '-q', '-D', 'main'])
  return { dir, fork, baseTip }
}

function repoSnapshot(dir) {
  return {
    status: git(dir, ['status', '--porcelain']),
    head: git(dir, ['rev-parse', 'HEAD']),
    refs: createHash('sha256')
      .update(git(dir, ['for-each-ref', '--format=%(refname) %(objectname)']))
      .digest('hex'),
  }
}

test('base has not moved: behind is 0 and stage 2 does no work', () => {
  const { dir } = makeRepo({ branchFiles: { 'branch-only.txt': 'changed\n' } })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.equal(report.behind, 0)
  assert.equal(report.stage2, false)
  assert.deepEqual(report.intersection, [])
  assert.equal(
    report.mergeTree,
    MERGE_TREE_SKIPPED,
    'a merge-tree that was never NEEDED reports skipped, not clean and not unevaluated',
  )
  assert.notEqual(report.mergeTree, MERGE_TREE_CLEAN, 'a probe that never ran proves nothing')
  assert.notEqual(
    report.mergeTree,
    UNEVALUATED,
    'an unmoved base leaves no unanswered question, so the healthy round must not read as one',
  )
  assert.match(formatDriftNote(report), /Base drift: none/)
})

test('base moved with disjoint change sets: intersection is empty', () => {
  const { dir } = makeRepo({
    baseFiles: { 'base-only.txt': 'base change\n' },
    branchFiles: { 'branch-only.txt': 'branch change\n' },
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.equal(report.behind, 1)
  assert.equal(report.stage2, true)
  assert.deepEqual(report.branchPaths, ['branch-only.txt'])
  assert.deepEqual(report.basePaths, ['base-only.txt'])
  assert.deepEqual(report.intersection, [])
  assert.equal(
    report.mergeTree,
    MERGE_TREE_SKIPPED,
    'disjoint change sets leave the probe nothing to answer, so it is skipped, not unevaluated',
  )
  assert.notEqual(
    report.mergeTree,
    UNEVALUATED,
    'the ordinary "base moved somewhere else" round must not read as an unresolved overlap',
  )
  assert.match(formatDriftNote(report), /disjoint/)
})

test('no merge base: the probe is UNEVALUATED, never the skipped a healthy round reports', () => {
  // The two "the probe did not run" reasons must stay distinguishable on the field itself. Here the
  // question — do the two sides overlap? — was asked and could not be answered, so it stays
  // `unevaluated` and a caller that acts on `unevaluated` still acts. Contrast the unmoved-base and
  // disjoint rounds above, where the question does not arise and the field reads `skipped`.
  const { dir } = makeRepo({ branchFiles: { 'branch-only.txt': 'changed\n' } })
  const orphan = git(dir, [
    'commit-tree',
    '-m',
    'unrelated root',
    `${git(dir, ['rev-parse', 'HEAD:']).trim()}`,
  ])
  git(dir, ['update-ref', BASE_REF, orphan.trim()])
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.ok(Number.isInteger(report.behind) && report.behind > 0, 'precondition: the base "moved"')
  assert.equal(report.stage2, false, 'stage 2 cannot run without a merge base')
  assert.equal(
    report.mergeTree,
    UNEVALUATED,
    'an overlap nobody could compute is unevaluated, never skipped',
  )
  assert.notEqual(report.mergeTree, MERGE_TREE_SKIPPED)
  assert.match(formatDriftNote(report), /overlap UNEVALUATED/)
})

test('a failed changed-path diff never publishes a disjoint verdict', () => {
  // The note is the only surface a human and the round reviewer read. `intersection` is empty here
  // because the comparison FAILED, not because the two sides are disjoint — and emptiness alone
  // once produced "the two change sets are disjoint (0 branch path(s), 0 base path(s)). No overlap
  // to review.", a proven-clean claim nobody obtained.
  const { dir } = makeRepo({
    baseFiles: { 'shared.txt': 'original\nbase appended\n' },
    branchFiles: { 'shared.txt': 'prepended by branch\noriginal\n' },
  })
  const run = (repo, args) =>
    args[0] === 'diff'
      ? { status: 128, stdout: '', stderr: 'simulated diff failure' }
      : realGit(repo, args)
  const report = baseDrift({ repo: dir, base: BASE_REF, run })
  assert.equal(report.stage2, true, 'precondition: stage 2 was entered')
  assert.deepEqual(report.intersection, [], 'precondition: nothing could be intersected')
  assert.equal(
    report.mergeTree,
    UNEVALUATED,
    'a comparison that failed leaves the probe unanswered, never skipped',
  )
  assert.notEqual(report.mergeTree, MERGE_TREE_SKIPPED)
  const note = formatDriftNote(report)
  assert.doesNotMatch(
    note,
    /disjoint/i,
    'a failed comparison must not claim the sides are disjoint',
  )
  assert.doesNotMatch(note, /No overlap to review/i)
  assert.match(note, /UNEVALUATED/)
  assert.match(note, /changed-path diff failed/)
})

test('a non-ASCII overlapping path is still attributed to the PR that landed it', () => {
  // `git diff --name-only` C-quotes such a path under the default `core.quotePath`, and the quoted
  // spelling matches no pathspec — so the overlap was detected while attribution reported "none
  // found", a false negative stated as a fact on the field the PR body publishes.
  const { dir } = makeRepo({
    branchFiles: { 'café.txt': 'branch side\n' },
    baseCommits: [
      { files: { 'café.txt': 'base side\n' }, message: 'feat(i18n): accented path (#4242)' },
    ],
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.deepEqual(report.intersection, ['café.txt'], 'the overlap must survive the round trip')
  assert.equal(report.attribution.status, 'ok')
  assert.deepEqual(
    report.attribution.prs,
    [4242],
    'the landing PR must be found, not reported absent',
  )
  const note4242 = formatDriftNote(report)
  assert.match(note4242, /PR 4242/, 'the landing PR is named')
  assert.doesNotMatch(note4242, /#4242/, 'but never with the sigil a forge cross-references on')
})

test('an overlapping path that looks like a glob matches only itself', () => {
  // Passed bare, `star*.txt` is a pathspec pattern: it also matches `starfish.txt`, and that file's
  // PR would be attributed to an overlap it never had.
  const { dir } = makeRepo({
    branchFiles: { 'star*.txt': 'branch side\n' },
    baseCommits: [
      { files: { 'starfish.txt': 'unrelated\n' }, message: 'chore: unrelated neighbour (#11)' },
      { files: { 'star*.txt': 'base side\n' }, message: 'chore: the literal path (#22)' },
    ],
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.deepEqual(report.intersection, ['star*.txt'])
  assert.deepEqual(
    report.attribution.prs,
    [22],
    'only the commit that touched the literal path may be attributed',
  )
})

test('base moved with an overlapping but textually clean change: mergeTree is clean', () => {
  const { dir } = makeRepo({
    baseFiles: { 'shared.txt': 'original\nbase appended\n' },
    branchFiles: { 'shared.txt': 'prepended by branch\noriginal\n' },
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.equal(report.behind, 1)
  assert.deepEqual(report.intersection, ['shared.txt'])
  assert.equal(report.mergeTree, MERGE_TREE_CLEAN)
  assert.deepEqual(report.conflictPaths, [])
  assert.match(formatDriftNote(report), /SEMANTIC/)
})

test('base moved with a textually conflicting change: mergeTree names the conflicting paths', () => {
  const { dir } = makeRepo({
    baseFiles: { 'shared.txt': 'base rewrote this line\n' },
    branchFiles: { 'shared.txt': 'branch rewrote this line\n' },
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.equal(report.behind, 1)
  assert.deepEqual(report.intersection, ['shared.txt'])
  assert.equal(report.mergeTree, MERGE_TREE_CONFLICTS)
  assert.deepEqual(report.conflictPaths, ['shared.txt'])
  assert.match(formatDriftNote(report), /CONFLICTING/)
})

test('merge-tree unsupported by this git: mergeTree is unevaluated, never clean', () => {
  const { dir } = makeRepo({
    baseFiles: { 'shared.txt': 'base line\n' },
    branchFiles: { 'shared.txt': 'branch line\n' },
  })
  const run = (repo, args) => {
    if (args[0] === 'merge-tree') {
      return { status: 129, stdout: '', stderr: "error: unknown option `write-tree'" }
    }
    const res = spawnSync('git', args, { cwd: repo, encoding: 'utf8', env: GIT_ENV })
    return { status: res.status, stdout: res.stdout || '', stderr: res.stderr || '' }
  }
  const report = baseDrift({ repo: dir, base: BASE_REF, run })
  assert.deepEqual(report.intersection, ['shared.txt'])
  assert.equal(report.mergeTree, UNEVALUATED)
  assert.notEqual(report.mergeTree, MERGE_TREE_CLEAN)
  assert.match(report.notes.join(' '), /merge-tree did not run/)
  assert.match(formatDriftNote(report), /UNEVALUATED/)
})

test('merge-tree exits 1 without a tree id: unevaluated, not a conflict verdict', () => {
  // git exits 1 both for a real content conflict and for "not something we can merge", so the
  // status alone is ambiguous. Only the object id on the first stdout line disambiguates them.
  const { dir } = makeRepo({ branchFiles: { 'shared.txt': 'branch line\n' } })
  const probe = mergeTreeStatus({ repo: dir, base: 'refs/remotes/origin/absent' })
  assert.equal(probe.status, UNEVALUATED)
  assert.notEqual(probe.status, MERGE_TREE_CONFLICTS)
  assert.deepEqual(probe.paths, [])
})

test('unresolvable base ref: behind is unevaluated, never 0', () => {
  const { dir } = makeRepo({ branchFiles: { 'branch-only.txt': 'changed\n' } })
  const raw = spawnSync('git', ['rev-list', '--count', 'HEAD..refs/remotes/origin/absent', '--'], {
    cwd: dir,
    encoding: 'utf8',
    env: GIT_ENV,
  })
  assert.notEqual(raw.status, 0, 'precondition: git must fail on the unresolvable ref')
  assert.equal(raw.stdout.trim(), '', 'precondition: the failing count prints nothing')

  assert.equal(countBehind({ repo: dir, base: 'refs/remotes/origin/absent' }), UNEVALUATED)
  const report = baseDrift({ repo: dir, base: 'refs/remotes/origin/absent' })
  assert.equal(report.behind, UNEVALUATED)
  assert.notEqual(report.behind, 0)
  assert.match(report.notes.join(' '), /does not resolve/)
  assert.match(formatDriftNote(report), /possibly moved/)
})

test('intersection is symmetric across an asymmetric pair of change sets', () => {
  const { dir, fork } = makeRepo({
    baseFiles: { 'shared.txt': 'base line\n', 'base-only.txt': 'base only\n' },
    branchFiles: { 'shared.txt': 'branch line\n' },
  })
  const branchPaths = changedPaths({ repo: dir, from: fork, to: 'HEAD' })
  const basePaths = changedPaths({ repo: dir, from: fork, to: BASE_REF })
  assert.deepEqual(branchPaths, ['shared.txt'])
  assert.deepEqual(basePaths, ['base-only.txt', 'shared.txt'])
  assert.notDeepEqual(branchPaths, basePaths, 'precondition: the two sides must differ in size')
  assert.deepEqual(intersectPaths(branchPaths, basePaths), ['shared.txt'])
  assert.deepEqual(intersectPaths(basePaths, branchPaths), ['shared.txt'])

  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.deepEqual(report.intersection, ['shared.txt'])
})

test('the check mutates no worktree state, and the snapshot proves it would notice one', () => {
  const { dir } = makeRepo({
    baseFiles: { 'shared.txt': 'base line\n' },
    branchFiles: { 'shared.txt': 'branch line\n' },
  })
  const before = repoSnapshot(dir)
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.equal(report.mergeTree, MERGE_TREE_CONFLICTS, 'precondition: the deepest path ran')
  const after = repoSnapshot(dir)
  assert.equal(after.status, before.status)
  assert.equal(after.head, before.head)
  assert.equal(after.refs, before.refs)

  // Non-vacuity control: a real mutation must change the same snapshot the assertions above read,
  // otherwise a snapshot that is blind to everything would pass them too.
  git(dir, ['update-ref', 'refs/heads/mutation-probe', before.head.trim()])
  fs.writeFileSync(path.join(dir, 'shared.txt'), 'dirtied\n')
  const mutated = repoSnapshot(dir)
  assert.notEqual(mutated.refs, before.refs, 'a new ref must move the ref digest')
  assert.notEqual(mutated.status, before.status, 'a dirty file must move the porcelain status')
})

test('the check verb prints one line of JSON on stdout and the note on stderr', () => {
  const { dir } = makeRepo({
    baseFiles: { 'shared.txt': 'base line\n' },
    branchFiles: { 'shared.txt': 'branch line\n' },
  })
  const res = spawnSync('node', [HELPER, 'check', '--repo', dir, '--base', BASE_REF], {
    encoding: 'utf8',
    env: GIT_ENV,
  })
  assert.equal(res.status, 0, res.stderr)
  assert.equal(res.stdout.trimEnd().split('\n').length, 1, 'stdout must be a single JSON line')
  const parsed = JSON.parse(res.stdout)
  assert.equal(parsed.behind, 1)
  assert.equal(parsed.mergeTree, MERGE_TREE_CONFLICTS)
  assert.deepEqual(parsed.intersection, ['shared.txt'])
  assert.match(res.stderr, /Base drift:/)
})

test('the CLI rejects a missing base and an unknown verb with exit 2', () => {
  const { dir } = makeRepo()
  const noBase = spawnSync('node', [HELPER, 'check', '--repo', dir], {
    encoding: 'utf8',
    env: GIT_ENV,
  })
  assert.equal(noBase.status, 2)
  assert.match(noBase.stderr, /--base is required/)

  const badVerb = spawnSync('node', [HELPER, 'sniff', '--base', BASE_REF], {
    encoding: 'utf8',
    env: GIT_ENV,
  })
  assert.equal(badVerb.status, 2)
  assert.match(badVerb.stderr, /usage: node base-drift\.mjs check/)
})

// A failed `git fetch` leaves the local base ref pointing at whatever it already held. Reading that
// stale ref plausibly reports `behind: 0`, and a note of `Base drift: none.` is then a claim nobody
// obtained — the exact substitution the reviewer template forbids. The failure has to reach the
// report, not just the caller's stderr.
test('a failed base fetch is UNEVALUATED, never "Base drift: none."', () => {
  const { dir } = makeRepo({ branchFiles: { 'branch-only.txt': 'changed\n' } })
  // Control: on this very repo, with the fetch believed good, the honest answer IS "no drift".
  const healthy = baseDrift({ repo: dir, base: BASE_REF })
  assert.equal(healthy.behind, 0)
  assert.equal(healthy.fetchFailed, false)
  assert.match(formatDriftNote(healthy), /^Base drift: none\./)

  const report = baseDrift({ repo: dir, base: BASE_REF, fetchFailed: true })
  assert.equal(report.fetchFailed, true)
  assert.equal(report.behind, UNEVALUATED)
  assert.notEqual(report.behind, 0, 'a stale ref must never be read as a proven-unmoved base')
  assert.equal(report.stage2, false)
  assert.match(report.notes.join(' '), /could not be refreshed/)
  const note = formatDriftNote(report)
  assert.match(note, /UNEVALUATED/)
  assert.doesNotMatch(note, /Base drift: none\./)
})

test('the CLI --fetch-failed flag carries the fetch failure into the report', () => {
  const { dir } = makeRepo({ branchFiles: { 'branch-only.txt': 'changed\n' } })
  const res = spawnSync(
    'node',
    [HELPER, 'check', '--repo', dir, '--base', BASE_REF, '--fetch-failed'],
    { encoding: 'utf8', env: GIT_ENV },
  )
  assert.equal(res.status, 0, res.stderr)
  const parsed = JSON.parse(res.stdout)
  assert.equal(parsed.fetchFailed, true)
  assert.equal(parsed.behind, UNEVALUATED)
  assert.match(parsed.note, /UNEVALUATED/)
  assert.doesNotMatch(parsed.note, /Base drift: none\./)
})

test('subject parsing recognises both landing conventions and nothing else', () => {
  assert.equal(pullRequestFromSubject('feat(core): rework the shared path (#2225)'), 2225)
  assert.equal(pullRequestFromSubject('Merge pull request #2229 from org/some-branch'), 2229)
  // A bare `#N` mid-subject is a reference to an issue, not evidence of the PR that landed it.
  assert.equal(pullRequestFromSubject('fix: issue #2225 is unrelated'), null)
  assert.equal(pullRequestFromSubject('chore: plain subject'), null)
  assert.equal(pullRequestFromSubject(''), null)
})

test('an overlapping base change names the PR that landed it', () => {
  const { dir } = makeRepo({
    branchFiles: { 'shared.txt': 'prepended by branch\noriginal\n' },
    baseCommits: [
      {
        files: { 'shared.txt': 'original\nbase appended\n' },
        message: 'feat(core): rework the shared path (#2225)',
      },
      { files: { 'base-only.txt': 'base only\n' }, message: 'chore: unrelated (#2226)' },
    ],
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.equal(report.behind, 2)
  assert.deepEqual(report.intersection, ['shared.txt'])
  assert.equal(report.mergeTree, MERGE_TREE_CLEAN)
  assert.equal(report.attribution.status, 'ok')
  // Attribution is scoped to the OVERLAP, so the base PR that touched nothing shared is absent.
  assert.deepEqual(report.attribution.prs, [2225])
  assert.deepEqual(report.attribution.unattributed, [])
  const note = formatDriftNote(report)
  assert.match(note, /PR 2225/, 'the landing PR is named')
  assert.doesNotMatch(note, /#2225/, 'but never with the sigil a forge cross-references on')
  assert.doesNotMatch(note, /2226/, 'and a base PR outside the overlap is absent entirely')
})

test('a merge-commit landing is attributed through the merge that carried it', () => {
  // A path-limited `git log` walks past the merge (it is TREESAME to the side branch) and reports
  // the developer's own commit, whose subject names no PR. Without the landing lookup the whole
  // `Merge pull request #N` convention attributes to nothing.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'base-drift-merge-'))
  git(dir, ['init', '-q', '-b', 'main'])
  git(dir, ['config', 'commit.gpgsign', 'false'])
  write(dir, 'shared.txt', 'original\n')
  commit(dir, 'fork point')
  git(dir, ['checkout', '-q', '-b', 'work'])
  write(dir, 'shared.txt', 'prepended by branch\noriginal\n')
  commit(dir, 'branch work')
  git(dir, ['checkout', '-q', 'main'])
  git(dir, ['checkout', '-q', '-b', 'feat'])
  write(dir, 'shared.txt', 'original\nbase appended\n')
  commit(dir, 'rework the shared path')
  git(dir, ['checkout', '-q', 'main'])
  git(dir, ['merge', '-q', '--no-ff', '-m', 'Merge pull request #4242 from x/feat', 'feat'])
  git(dir, ['update-ref', BASE_REF, git(dir, ['rev-parse', 'HEAD']).trim()])
  git(dir, ['checkout', '-q', 'work'])
  git(dir, ['branch', '-q', '-D', 'main'])

  // Precondition, measured rather than assumed: the path-limited log really does hide the merge.
  const visible = git(dir, [
    'log',
    '--format=%s',
    'HEAD..' + BASE_REF,
    '--',
    ':(literal)shared.txt',
  ])
  assert.doesNotMatch(visible, /Merge pull request/, 'precondition: the merge is elided by the log')

  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.deepEqual(report.intersection, ['shared.txt'])
  assert.deepEqual(report.attribution.prs, [4242], 'the landing merge must supply the PR number')
  assert.deepEqual(report.attribution.unattributed, [], 'nothing may be left unattributed here')
  assert.equal(report.attribution.commits[0].landedBy.length > 0, true, 'the merge is recorded')
  const note4242 = formatDriftNote(report)
  assert.match(note4242, /PR 4242/, 'the landing PR is named')
  assert.doesNotMatch(note4242, /#4242/, 'but never with the sigil a forge cross-references on')
  fs.rmSync(dir, { recursive: true, force: true })
})

test('an attribution log that returned nothing reports an absent record, not an absent commit', () => {
  const { dir } = makeRepo({
    baseFiles: { 'shared.txt': 'original\nbase appended\n' },
    branchFiles: { 'shared.txt': 'prepended by branch\noriginal\n' },
  })
  const run = (repo, args) =>
    args[0] === 'log' ? { status: 0, stdout: '', stderr: '' } : realGit(repo, args)
  const report = baseDrift({ repo: dir, base: BASE_REF, run })
  assert.deepEqual(report.intersection, ['shared.txt'], 'precondition: there IS an overlap')
  assert.equal(report.attribution.status, 'ok')
  assert.deepEqual(report.attribution.commits, [])
  const note = formatDriftNote(report)
  assert.match(note, /Landing PRs: none recorded/)
  // The old spelling asserted a fact about the base branch that an empty log does not establish.
  assert.doesNotMatch(note, /none found/)
  assert.doesNotMatch(note, /no base commit in this range touches/)
})

test('a base commit subject cannot smuggle the merge-gate token into the note', () => {
  // The note is published into a PR body. `do not merge` is the substring a merge gate matches on a
  // PR title/body, so an unredacted base subject would hold THIS branch's PR back — and the note is
  // quoted from other people's commits, so no "rephrase the finding" rule reaches it.
  const R = '[merge-gate token redacted]'
  assert.equal(
    redactMergeGateToken('chore: revert the do not merge marker'),
    `chore: revert the ${R} marker`,
  )
  assert.equal(redactMergeGateToken('DO NOT MERGE yet'), `${R} yet`)
  assert.equal(redactMergeGateToken('do   not\tmerge'), R)
  // Not the hyphenated spelling: that is the marker's own documented name in this protocol's prose.
  assert.doesNotMatch(redactMergeGateToken('do not merge'), /do-not-merge/)
  assert.equal(
    redactMergeGateToken('merge does not do'),
    'merge does not do',
    'unrelated words survive',
  )

  const { dir } = makeRepo({
    branchFiles: { 'shared.txt': 'prepended by branch\noriginal\n' },
    baseCommits: [
      {
        files: { 'shared.txt': 'original\nbase\n' },
        message: 'chore: revert the do not merge marker',
      },
    ],
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.deepEqual(
    report.intersection,
    ['shared.txt'],
    'precondition: there is an overlap to report',
  )
  const note = formatDriftNote(report)
  assert.match(note, /merge-gate token redacted/, 'the redaction is visible to the reader')
  assert.doesNotMatch(note, /do not merge/i, 'the gate token must not survive into the note')
  assert.doesNotMatch(note, /do-not-merge/i, 'nor the marker name the token is known by')
})

test('a quoted unattributed subject cannot cross-reference another PR', () => {
  // The repo convention `[#1234]` matches neither anchored landing pattern, so EVERY overlapping
  // commit here lands in `unattributed` and its subject is published verbatim into a PR body.
  assert.equal(deCrossReference('feat(x): [#2231] do a thing'), 'feat(x): [PR 2231] do a thing')
  assert.equal(deCrossReference('no numbers here'), 'no numbers here')

  const { dir } = makeRepo({
    branchFiles: { 'shared.txt': 'prepended by branch\noriginal\n' },
    baseCommits: [
      { files: { 'shared.txt': 'original\nbase\n' }, message: 'feat(boss-build): [#2231] land it' },
    ],
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.deepEqual(report.intersection, ['shared.txt'], 'precondition: there is an overlap')
  assert.equal(report.attribution.prs.length, 0, 'precondition: the subject attributes to nothing')
  assert.equal(report.attribution.unattributed.length, 1, 'precondition: it is quoted verbatim')
  const note = formatDriftNote(report)
  assert.match(note, /PR 2231/, 'the number is still readable')
  assert.doesNotMatch(note, /#2231/, 'but never as a bare #N a forge cross-references on')
})

test('a capped landing lookup is disclosed in the note, not only in the JSON', () => {
  // `formatDriftNote`'s clean and conflicting branches never interpolate `notes`, so a truncation
  // recorded only there would publish "N commit(s) carry no PR reference" with no hint that some of
  // them were never looked up.
  const attribution = {
    status: 'ok',
    commits: [{ sha: 'abc1234', subject: 'raw commit', pr: null }],
    prs: [],
    unattributed: [{ sha: 'abc1234', subject: 'raw commit' }],
    reason:
      'landing-merge lookup capped at 25 commit(s); some of the unattributed commits may carry a PR this report does not name',
  }
  const report = {
    base: BASE_REF,
    head: 'HEAD',
    fetchFailed: false,
    behind: 3,
    mergeBase: 'a'.repeat(40),
    stage2: true,
    branchPaths: ['shared.txt'],
    basePaths: ['shared.txt'],
    intersection: ['shared.txt'],
    mergeTree: MERGE_TREE_CLEAN,
    conflictPaths: [],
    attribution,
    notes: [attribution.reason],
  }
  const note = formatDriftNote(report)
  assert.match(note, /textually CLEAN/, 'precondition: this is the branch that omits notes')
  assert.match(note, /capped at 25 commit\(s\)/, 'the truncation must reach the published note')
})

test('a base commit with no PR reference is named as unattributed, never dropped', () => {
  const { dir } = makeRepo({
    branchFiles: { 'shared.txt': 'prepended by branch\noriginal\n' },
    baseCommits: [
      {
        files: { 'shared.txt': 'original\nbase appended\n' },
        message: 'hotfix: pushed straight to the base',
      },
    ],
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.deepEqual(report.intersection, ['shared.txt'])
  assert.equal(report.attribution.status, 'ok')
  assert.deepEqual(report.attribution.prs, [])
  assert.equal(report.attribution.unattributed.length, 1)
  assert.equal(report.attribution.unattributed[0].subject, 'hotfix: pushed straight to the base')
  const note = formatDriftNote(report)
  assert.match(note, /no PR reference/)
  assert.match(note, /hotfix: pushed straight to the base/)
})

test('attribution that cannot run is UNEVALUATED in the note, never silently omitted', () => {
  const { dir } = makeRepo({
    baseFiles: { 'shared.txt': 'original\nbase appended\n' },
    branchFiles: { 'shared.txt': 'prepended by branch\noriginal\n' },
  })
  const run = (repo, args) => {
    if (args[0] === 'log')
      return { status: 128, stdout: '', stderr: 'fatal: simulated log failure' }
    const res = spawnSync('git', args, { cwd: repo, encoding: 'utf8', env: GIT_ENV })
    return { status: res.status, stdout: res.stdout || '', stderr: res.stderr || '' }
  }
  const report = baseDrift({ repo: dir, base: BASE_REF, run })
  assert.deepEqual(report.intersection, ['shared.txt'])
  assert.equal(report.attribution.status, UNEVALUATED)
  assert.match(formatDriftNote(report), /Landing PRs: UNEVALUATED/)
})

test('a disjoint base move attempts no attribution and says so', () => {
  const { dir } = makeRepo({
    baseFiles: { 'base-only.txt': 'base change\n' },
    branchFiles: { 'branch-only.txt': 'branch change\n' },
  })
  const report = baseDrift({ repo: dir, base: BASE_REF })
  assert.deepEqual(report.intersection, [])
  assert.equal(report.attribution.status, UNEVALUATED)
  assert.deepEqual(report.attribution.commits, [])
})
