#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

// Always exercise the REPO SOURCE of the skill payload, never an installed copy
// under ~/.claude or ~/.codex — those go stale the moment this file changes.
const scriptPath = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  '..',
  'services',
  'boss',
  'internal',
  'skillinstall',
  'skills',
  'boss-finalize',
  'add-pr-numbers.sh',
)

const PR_NUM = 4242
const TAG = `[#${PR_NUM}]`

// A developer's global/system git config must not be able to change the result.
const gitEnv = { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null' }

const PERMISSIVE_HOOK = '#!/bin/sh\nexit 0\n'

// Rejects any BODY line longer than 20 chars (the subject, line 1, is exempt).
const LONG_BODY_HOOK = `#!/bin/sh
n=0
while IFS= read -r line; do
  n=$((n + 1))
  [ "$n" -eq 1 ] && continue
  if [ "\${#line}" -gt 20 ]; then
    echo "commit-msg: body line too long" >&2
    exit 1
  fi
done < "$1"
exit 0
`

// Rejects any SUBJECT longer than 40 chars.
const LONG_SUBJECT_HOOK = `#!/bin/sh
subject=$(head -n 1 "$1")
if [ "\${#subject}" -gt 40 ]; then
  echo "commit-msg: subject too long" >&2
  exit 1
fi
exit 0
`

const tempRoots = []

function tempDir(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix))
  tempRoots.push(dir)
  // macOS /tmp is a symlink; git reports realpaths, so normalise up front.
  return fs.realpathSync(dir)
}

function git(cwd, ...args) {
  return execFileSync('git', args, { cwd, encoding: 'utf8', env: gitEnv }).replace(/\n$/, '')
}

// Build a fixture repo with a real `origin` (the script always runs
// `git fetch origin "$BASE_BRANCH"`), a `feature` branch carrying `commits`,
// and `hook` installed as the native .git/hooks/commit-msg (not husky).
// Branch commits are made with --no-verify so only the amends are hook-gated.
function makeRepo({ hook, commits }) {
  const bare = tempDir('pr-tag-origin-')
  execFileSync('git', ['-c', 'init.defaultBranch=main', 'init', '-q', '--bare', bare], {
    env: gitEnv,
  })

  const repo = tempDir('pr-tag-work-')
  git(repo, '-c', 'init.defaultBranch=main', 'init', '-q')
  git(repo, 'config', 'user.email', 'test@example.com')
  git(repo, 'config', 'user.name', 'Test User')
  git(repo, 'config', 'commit.gpgsign', 'false')

  fs.writeFileSync(path.join(repo, 'README.md'), 'base\n')
  git(repo, 'add', 'README.md')
  git(repo, 'commit', '--no-verify', '-q', '-m', 'chore: base')
  git(repo, 'remote', 'add', 'origin', bare)
  git(repo, 'push', '-q', '-u', 'origin', 'main')
  git(repo, 'checkout', '-q', '-b', 'feature')

  commits.forEach((commit, index) => {
    fs.writeFileSync(path.join(repo, `file-${index}.txt`), `${index}\n`)
    git(repo, 'add', `file-${index}.txt`)
    const message = commit.body ? `${commit.subject}\n\n${commit.body}` : commit.subject
    git(repo, 'commit', '--no-verify', '-q', '-m', message)
  })

  fs.writeFileSync(path.join(repo, '.git', 'hooks', 'commit-msg'), hook, { mode: 0o755 })
  return repo
}

// Run the script under test. PR_NUM is argv[0] and BASE_BRANCH comes from the
// env so the script never shells out to `gh` (absent in the test environment).
// stderr goes to a file outside the worktree so it is captured on BOTH the
// success and the failure path (execFileSync only surfaces stderr on throw).
function runScript(repo) {
  const errFile = path.join(tempDir('pr-tag-err-'), 'stderr.log')
  let code = 0
  let stdout = ''
  try {
    stdout = execFileSync('bash', ['-c', '"$0" "$1" 2>"$2"', scriptPath, String(PR_NUM), errFile], {
      cwd: repo,
      encoding: 'utf8',
      env: { ...gitEnv, BASE_BRANCH: 'main' },
    })
  } catch (err) {
    // `err.status` is null when the process was SIGNALLED. Coercing that to 1 would let a
    // crashed/killed run satisfy every `notEqual(code, 0)` assertion below, so fail loudly.
    assert.equal(err.signal, null, `script was signalled (${err.signal}), not exited`)
    code = err.status ?? 1
    stdout = err.stdout?.toString() ?? ''
  }
  const stderr = fs.existsSync(errFile) ? fs.readFileSync(errFile, 'utf8') : ''
  return { code, stdout, stderr }
}

// Re-derive the truth from git itself — never from the script's own prose.
function subjects(repo) {
  const out = git(repo, 'log', '--reverse', '--format=%s', 'main..HEAD')
  return out === '' ? [] : out.split('\n')
}

function shortShaFor(repo, subject) {
  const out = git(repo, 'log', '--reverse', '--format=%h%x09%s', 'main..HEAD')
  for (const line of out.split('\n')) {
    const [sha, ...rest] = line.split('\t')
    if (rest.join('\t') === subject) return sha
  }
  throw new Error(`no commit with subject ${JSON.stringify(subject)} in:\n${out}`)
}

after(() => {
  for (const dir of tempRoots) fs.rmSync(dir, { recursive: true, force: true })
})

test('add-pr-numbers case 1: permissive hook tags every commit and exits 0', () => {
  assert.ok(fs.existsSync(scriptPath), 'add-pr-numbers.sh must exist')
  const repo = makeRepo({
    hook: PERMISSIVE_HOOK,
    commits: [
      { subject: 'feat(core): add thing' },
      { subject: 'fix: repair thing' },
      { subject: 'chore(deps): bump thing' },
    ],
  })

  const r = runScript(repo)
  assert.equal(r.code, 0, `expected success:\n${r.stdout}\n${r.stderr}`)

  const after1 = subjects(repo)
  assert.equal(after1.length, 3)
  for (const subject of after1) {
    assert.ok(subject.includes(TAG), `subject must carry ${TAG}: ${subject}`)
  }
  assert.doesNotMatch(r.stderr, /WARNING:/, `no warning expected: ${r.stderr}`)
  assert.doesNotMatch(r.stderr, /untagged/, `no skip list expected: ${r.stderr}`)
})

test('add-pr-numbers case 2: a hook-rejected amend fails loudly and lists the skipped commit', () => {
  const rejected = 'fix: repair thing'
  const repo = makeRepo({
    hook: LONG_BODY_HOOK,
    commits: [
      { subject: 'feat(core): add thing', body: 'short body' },
      { subject: rejected, body: 'this body line is definitely longer than twenty characters' },
      { subject: 'chore(deps): bump thing', body: 'ok' },
    ],
  })

  const r = runScript(repo)
  assert.notEqual(r.code, 0, `expected a non-zero exit:\n${r.stdout}\n${r.stderr}`)

  const after2 = subjects(repo)
  assert.equal(after2.length, 3)
  assert.ok(after2[0].includes(TAG), `first commit must be tagged: ${after2[0]}`)
  assert.ok(after2[2].includes(TAG), `third commit must be tagged: ${after2[2]}`)
  assert.equal(after2[1], rejected, 'the rejected commit must keep its original untagged subject')

  const short = shortShaFor(repo, rejected)
  assert.match(r.stderr, /WARNING:/, `stderr must carry a WARNING: ${r.stderr}`)
  assert.ok(
    r.stderr.includes(`WARNING: could not amend ${short} ${rejected}:`),
    `stderr must name the short sha and subject: ${r.stderr}`,
  )
  // The reported cause must be git's own error, not a cause the script assumed.
  assert.ok(
    r.stderr.includes('(amend rejected: ') && r.stderr.includes('commit-msg: body line too long'),
    `the skip list must annotate the REAL reason from the hook: ${r.stderr}`,
  )
  assert.ok(
    r.stderr.split('\n').some((line) => line.includes(short) && line.includes(rejected)),
    `the skip list must name the untagged commit: ${r.stderr}`,
  )
  // Pin the operator-facing header and summary — these are the lines a human greps for.
  assert.ok(
    r.stderr.includes('ERROR: these commits were left untagged:'),
    `stderr must carry the skip-list header: ${r.stderr}`,
  )
  assert.ok(
    r.stderr.includes(`1 of 3 commits do not carry ${TAG}.`),
    `stderr must summarise the skip count: ${r.stderr}`,
  )
})

test('add-pr-numbers case 3: a hook-rejected amend leaves no mid-rebase worktree', () => {
  const repo = makeRepo({
    hook: LONG_BODY_HOOK,
    commits: [
      { subject: 'feat(core): add thing', body: 'short body' },
      {
        subject: 'fix: repair thing',
        body: 'this body line is definitely longer than twenty chars',
      },
      { subject: 'chore(deps): bump thing', body: 'ok' },
    ],
  })

  const r = runScript(repo)
  // Assert the run actually failed, so this case cannot be satisfied by a script that no-ops.
  assert.notEqual(r.code, 0, `expected a non-zero exit:\n${r.stdout}\n${r.stderr}`)

  assert.equal(
    fs.existsSync(path.join(repo, '.git', 'rebase-merge')),
    false,
    'worktree must not be left mid-rebase (.git/rebase-merge)',
  )
  assert.equal(
    fs.existsSync(path.join(repo, '.git', 'rebase-apply')),
    false,
    'worktree must not be left mid-rebase (.git/rebase-apply)',
  )
  assert.equal(
    git(repo, 'symbolic-ref', '-q', 'HEAD'),
    'refs/heads/feature',
    'HEAD must be attached',
  )
  assert.equal(git(repo, 'status', '--porcelain'), '', 'worktree must be clean')
})

test('add-pr-numbers case 4: an over-long tagged subject fails as loudly as an over-long body', () => {
  const rejected = 'feat(core): add the long feature name'
  const repo = makeRepo({
    hook: LONG_SUBJECT_HOOK,
    commits: [{ subject: 'fix: a' }, { subject: rejected }],
  })

  const r = runScript(repo)
  assert.notEqual(r.code, 0, `expected a non-zero exit:\n${r.stdout}\n${r.stderr}`)

  const after4 = subjects(repo)
  assert.equal(after4.length, 2)
  assert.ok(after4[0].includes(TAG), `short commit must be tagged: ${after4[0]}`)
  assert.equal(after4[1], rejected, 'the over-long tagged subject must be left untagged')

  const short = shortShaFor(repo, rejected)
  assert.ok(
    r.stderr.includes(`WARNING: could not amend ${short} ${rejected}:`),
    `stderr must name the short sha and subject: ${r.stderr}`,
  )
  assert.ok(
    r.stderr.split('\n').some((line) => line.includes(short) && line.includes(rejected)),
    `the skip list must name the untagged commit: ${r.stderr}`,
  )
  // The WARNING line alone carries the sha + subject, so assert the annotated skip-list
  // entry too — otherwise this case would pass with the post-condition scan removed.
  assert.ok(
    r.stderr.includes('(amend rejected: ') && r.stderr.includes('commit-msg: subject too long'),
    `the skip list must annotate the REAL reason from the hook: ${r.stderr}`,
  )
})

// The script must not name a cause it never checked. An amend can also be refused by git
// itself — here, amending a commit that would become empty — while the commit-msg hook is
// perfectly happy. Claiming "the commit-msg hook rejected the amend" there sends an operator
// to edit a message that can never be the problem.
test('add-pr-numbers case 8: a non-hook amend failure reports git’s own reason', () => {
  const repo = makeRepo({ hook: PERMISSIVE_HOOK, commits: [{ subject: 'feat(core): add thing' }] })
  git(repo, 'commit', '--no-verify', '-q', '--allow-empty', '-m', 'chore: an empty commit')

  const r = runScript(repo)
  assert.notEqual(r.code, 0, `expected a non-zero exit:\n${r.stdout}\n${r.stderr}`)

  const short = shortShaFor(repo, 'chore: an empty commit')
  assert.ok(
    r.stderr.includes(`WARNING: could not amend ${short} chore: an empty commit:`),
    `stderr must name the commit it could not amend: ${r.stderr}`,
  )
  assert.match(
    r.stderr,
    /\(amend rejected: [^)]*empty/,
    `the skip list must carry git's own reason, not an assumed one: ${r.stderr}`,
  )
  assert.doesNotMatch(
    r.stderr,
    /commit-msg hook rejected/,
    `must not blame the commit-msg hook, which passed: ${r.stderr}`,
  )
})

// A hook can reject with no output at all, and a subject can contain a tab. Neither may
// leave the operator with a bare or garbled skip-list entry — an entry with nothing after
// the subject reads exactly like a commit the helper never reached.
test('add-pr-numbers case 10: a silent rejection and a tabbed subject still read cleanly', () => {
  const rejected = 'fix:\ttabbed REJECTME'
  const repo = makeRepo({
    hook: '#!/bin/sh\ngrep -q REJECTME "$1" && exit 1\nexit 0\n',
    commits: [{ subject: 'feat(core): add thing' }, { subject: rejected }],
  })

  const r = runScript(repo)
  assert.notEqual(r.code, 0, `expected a non-zero exit:\n${r.stdout}\n${r.stderr}`)

  const short = shortShaFor(repo, rejected)
  const flat = rejected.replace(/\t/g, ' ')
  assert.ok(
    r.stderr.includes(`WARNING: could not amend ${short} ${flat}: `),
    `the tab must be flattened, not left to split the record: ${r.stderr}`,
  )
  // The reason placeholder, not an empty parenthetical or a truncated subject tail.
  assert.ok(
    r.stderr.includes('(amend rejected: the amend failed without printing a reason)'),
    `a silent rejection must still carry a stated reason: ${r.stderr}`,
  )
})

// A branch carrying a merge FROM the base loses its resolution when the flattening rebase
// replays the side commits, so the rebase stops mid-way. That path must keep the helper
// (`git rebase --continue` re-executes it; a deleted one degrades to a bare `warning:` and
// leaves every later commit untagged) and must tell the operator to re-run for a verdict —
// this process's post-condition cannot cover a rebase it did not finish.
test('add-pr-numbers case 11: a conflicted rebase keeps the helper and demands a re-run', (t) => {
  const bare = tempDir('pr-tag-origin-')
  execFileSync('git', ['-c', 'init.defaultBranch=main', 'init', '-q', '--bare', bare], {
    env: gitEnv,
  })
  const repo = tempDir('pr-tag-work-')
  git(repo, '-c', 'init.defaultBranch=main', 'init', '-q')
  git(repo, 'config', 'user.email', 'test@example.com')
  git(repo, 'config', 'user.name', 'Test User')
  git(repo, 'config', 'commit.gpgsign', 'false')
  fs.writeFileSync(path.join(repo, 'f.txt'), 'base\n')
  git(repo, 'add', 'f.txt')
  git(repo, 'commit', '--no-verify', '-q', '-m', 'chore: base')
  git(repo, 'remote', 'add', 'origin', bare)
  git(repo, 'push', '-q', '-u', 'origin', 'main')

  git(repo, 'checkout', '-q', '-b', 'feature')
  fs.writeFileSync(path.join(repo, 'f.txt'), 'feature\n')
  git(repo, 'commit', '--no-verify', '-q', '-am', 'feat(core): side edit')
  git(repo, 'checkout', '-q', 'main')
  fs.writeFileSync(path.join(repo, 'f.txt'), 'mainside\n')
  git(repo, 'commit', '--no-verify', '-q', '-am', 'fix: base edit')
  git(repo, 'push', '-q', 'origin', 'main')
  git(repo, 'checkout', '-q', 'feature')
  try {
    git(repo, 'merge', 'main', '-m', 'chore: merge main')
  } catch {
    fs.writeFileSync(path.join(repo, 'f.txt'), 'resolved\n')
    git(repo, 'add', 'f.txt')
    git(repo, 'commit', '--no-verify', '-q', '--no-edit')
  }
  fs.writeFileSync(path.join(repo, '.git', 'hooks', 'commit-msg'), PERMISSIVE_HOOK, { mode: 0o755 })

  const r = runScript(repo)
  // Assert the strand FIRST: if the fixture ever stops conflicting the run tags everything
  // and exits 0, and this message names the real cause instead of the downstream symptom.
  assert.ok(
    fs.existsSync(path.join(repo, '.git', 'rebase-merge')),
    'fixture must actually strand the rebase, or this case proves nothing',
  )
  assert.notEqual(r.code, 0, `a stopped rebase must not report success:\n${r.stdout}\n${r.stderr}`)

  // Not anchored to /tmp or to the `$$` basename: the retained-helper property is what
  // matters, and this still fails closed if the NOTE stops naming a path at all.
  const helper = r.stderr.match(/\S*add-pr-to-commit-\S*\.sh/)?.[0]
  assert.ok(helper, `the NOTE must name the retained helper: ${r.stderr}`)
  // Registered before the remaining asserts so a failure cannot leak it.
  t.after(() => fs.rmSync(helper, { force: true }))

  assert.ok(fs.existsSync(helper), `the helper must survive a stranded rebase: ${helper}`)
  assert.match(r.stderr, /RE-RUN this script/, `must demand a re-run for a verdict: ${r.stderr}`)
})

// A caller that passes an option where the PR number belongs must be refused, not obeyed:
// the tag written and the tag verified come from the same value, so a bad one is
// self-consistently "successful" and ships `[#--pr]` to every consumer.
test('add-pr-numbers case 9: a non-numeric PR number is refused instead of tagged', () => {
  const subject = 'feat(core): add thing'
  const repo = makeRepo({ hook: PERMISSIVE_HOOK, commits: [{ subject }] })

  const errFile = path.join(tempDir('pr-tag-err-'), 'stderr.log')
  let code = 0
  try {
    execFileSync('bash', ['-c', '"$0" "$1" 2>"$2"', scriptPath, '--pr', errFile], {
      cwd: repo,
      encoding: 'utf8',
      env: { ...gitEnv, BASE_BRANCH: 'main' },
    })
  } catch (err) {
    code = err.status ?? 1
  }
  const stderr = fs.readFileSync(errFile, 'utf8')

  assert.notEqual(code, 0, `a non-numeric PR number must not succeed: ${stderr}`)
  assert.match(stderr, /PR number must be numeric/, `stderr must say why: ${stderr}`)
  assert.deepEqual(subjects(repo), [subject], 'no commit may be rewritten')
})

test('add-pr-numbers case 5: already-tagged commits are idempotent and exit 0', () => {
  const repo = makeRepo({
    hook: PERMISSIVE_HOOK,
    commits: [{ subject: `feat(core): ${TAG} add thing` }, { subject: `fix: ${TAG} repair thing` }],
  })

  const before = subjects(repo)
  const r = runScript(repo)
  assert.equal(r.code, 0, `expected success:\n${r.stdout}\n${r.stderr}`)
  assert.deepEqual(subjects(repo), before, 'subjects must be byte-identical after a re-run')
})

// The helper decides "already tagged" and the post-condition decides "untagged"; if they
// disagree the script fails on a commit it refused to amend, and re-running never clears it.
// A body that merely MENTIONS the PR ("Follow-up to [#N].") is the input that separates
// them -- and it is exactly what the pre-fix fallback produced, so it occurs in the wild.
test('add-pr-numbers case 7: a tag in the BODY does not count as tagged; the subject gets one', () => {
  const subject = 'feat(core): add thing'
  const repo = makeRepo({
    hook: PERMISSIVE_HOOK,
    commits: [{ subject, body: `Follow-up to ${TAG}.` }],
  })

  const r = runScript(repo)
  assert.equal(r.code, 0, `a body-only mention must not fail the run:\n${r.stdout}\n${r.stderr}`)
  assert.doesNotMatch(r.stderr, /untagged/, `no skip list expected: ${r.stderr}`)

  const tagged = git(repo, 'log', '-1', '--format=%s')
  assert.ok(tagged.includes(TAG), `the subject must have been amended: ${tagged}`)
  assert.notEqual(tagged, subject, 'the commit must not have been skipped as already-tagged')

  // And the fixed point holds: a second pass is a no-op, not a double tag.
  const second = runScript(repo)
  assert.equal(second.code, 0, `re-run must exit 0:\n${second.stdout}\n${second.stderr}`)
  assert.equal(git(repo, 'log', '-1', '--format=%s'), tagged, 're-run must be byte-identical')
})

test('add-pr-numbers case 6: a non-conventional subject gets the tag on line 1, not in the body', () => {
  const repo = makeRepo({
    hook: PERMISSIVE_HOOK,
    commits: [{ subject: 'no colon here', body: 'some body text\nlast body line' }],
  })

  const r = runScript(repo)
  assert.equal(r.code, 0, `expected success:\n${r.stdout}\n${r.stderr}`)

  const subject = git(repo, 'log', '-1', '--format=%s')
  assert.ok(subject.endsWith(TAG), `the tag must land on the subject line: ${subject}`)

  const bodyLines = git(repo, 'log', '-1', '--format=%b').split('\n').filter(Boolean)
  const lastBodyLine = bodyLines[bodyLines.length - 1]
  assert.equal(lastBodyLine, 'last body line')
  assert.ok(!lastBodyLine.includes(TAG), `the tag must not land in the body: ${lastBodyLine}`)
})
