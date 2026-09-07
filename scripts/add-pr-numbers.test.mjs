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
// and native .git/hooks entries (not husky). Branch commits are made with
// --no-verify so only the amends are hook-gated.
function makeRepo({ hook, hooks, commits }) {
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
    const message = commit.body ? `${commit.subject}\n\n${commit.body}` : commit.subject
    if (commit.empty) {
      git(repo, 'commit', '--no-verify', '--allow-empty', '-q', '-m', message)
    } else {
      fs.writeFileSync(path.join(repo, `file-${index}.txt`), `${index}\n`)
      git(repo, 'add', `file-${index}.txt`)
      git(repo, 'commit', '--no-verify', '-q', '-m', message)
    }
  })

  const installedHooks = hooks ?? { 'commit-msg': hook }
  for (const [name, body] of Object.entries(installedHooks)) {
    fs.writeFileSync(path.join(repo, '.git', 'hooks', name), body, { mode: 0o755 })
  }
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

function occurrences(haystack, needle) {
  return haystack.split(needle).length - 1
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

// The script must not name a cause it never checked. An amend can be refused by a hook
// other than commit-msg, while the commit-msg hook is perfectly happy. Claiming "the
// commit-msg hook rejected the amend" there sends an operator to edit the wrong policy.
test('add-pr-numbers case 8: a non-commit-msg amend failure reports the hook’s own reason', () => {
  const rejected = 'feat(core): add thing'
  const repo = makeRepo({
    hooks: {
      'commit-msg': PERMISSIVE_HOOK,
      'pre-commit': '#!/bin/sh\necho "pre-commit: amend not allowed" >&2\nexit 1\n',
    },
    commits: [{ subject: rejected }],
  })

  const r = runScript(repo)
  assert.notEqual(r.code, 0, `expected a non-zero exit:\n${r.stdout}\n${r.stderr}`)

  const short = shortShaFor(repo, rejected)
  assert.ok(
    r.stderr.includes(`WARNING: could not amend ${short} ${rejected}:`),
    `stderr must name the commit it could not amend: ${r.stderr}`,
  )
  assert.match(
    r.stderr,
    /\(amend rejected: [^)]*pre-commit: amend not allowed/,
    `the skip list must carry the hook's own reason, not an assumed one: ${r.stderr}`,
  )
  assert.doesNotMatch(
    r.stderr,
    /commit-msg hook rejected/,
    `must not blame the commit-msg hook, which passed: ${r.stderr}`,
  )
})

test('add-pr-numbers case 12: empty commits are skipped while real commits are tagged', () => {
  const empty1 = 'chore: [skip ci] create pull request'
  const empty2 = 'chore: another empty marker'
  const repo = makeRepo({
    hook: PERMISSIVE_HOOK,
    commits: [
      { subject: empty1, empty: true },
      { subject: 'feat(core): add thing' },
      { subject: empty2, empty: true },
      { subject: 'fix: repair thing' },
    ],
  })
  const beforeShort1 = shortShaFor(repo, empty1)
  const beforeShort2 = shortShaFor(repo, empty2)

  const r = runScript(repo)
  assert.equal(r.code, 0, `expected success:\n${r.stdout}\n${r.stderr}`)
  assert.doesNotMatch(r.stderr, /WARNING:/, `empty commits must not reach amend: ${r.stderr}`)
  assert.doesNotMatch(
    r.stderr,
    /amend rejected/,
    `empty commits must not be skip-report rows: ${r.stderr}`,
  )
  assert.doesNotMatch(r.stderr, /untagged/, `empty commits must not be error-listed: ${r.stderr}`)

  const after12 = subjects(repo)
  assert.equal(after12.length, 4)
  assert.equal(after12[0], empty1, 'first empty commit subject must be byte-identical')
  assert.ok(after12[1].includes(TAG), `first real commit must be tagged: ${after12[1]}`)
  assert.equal(after12[2], empty2, 'second empty commit subject must be byte-identical')
  assert.ok(after12[3].includes(TAG), `second real commit must be tagged: ${after12[3]}`)

  assert.equal(occurrences(r.stdout, `Skipped empty commit ${beforeShort1} ${empty1}`), 1, r.stdout)
  assert.equal(occurrences(r.stdout, `Skipped empty commit ${beforeShort2} ${empty2}`), 1, r.stdout)
})

test('add-pr-numbers case 13: an all-empty range exits 0 without an untagged error', () => {
  const empty1 = 'chore: empty one'
  const empty2 = 'chore: empty two'
  const repo = makeRepo({
    hook: PERMISSIVE_HOOK,
    commits: [
      { subject: empty1, empty: true },
      { subject: empty2, empty: true },
    ],
  })
  const beforeShort1 = shortShaFor(repo, empty1)
  const beforeShort2 = shortShaFor(repo, empty2)

  const r = runScript(repo)
  assert.equal(r.code, 0, `expected success:\n${r.stdout}\n${r.stderr}`)
  assert.deepEqual(subjects(repo), [empty1, empty2], 'empty subjects must stay untagged')
  assert.doesNotMatch(r.stderr, /ERROR: these commits were left untagged:/, r.stderr)
  assert.equal(occurrences(r.stdout, `Skipped empty commit ${beforeShort1} ${empty1}`), 1, r.stdout)
  assert.equal(occurrences(r.stdout, `Skipped empty commit ${beforeShort2} ${empty2}`), 1, r.stdout)
})

test('add-pr-numbers case 14: empty predicates and fail-closed scans stay pinned', () => {
  const script = fs.readFileSync(scriptPath, 'utf8')
  // BOS-1195: emptiness has ONE definition, in the shared predicate module. Neither the
  // rebase helper nor the post-condition may re-derive it from trees by hand again --
  // two hand-written copies across the heredoc boundary is the drift this replaced.
  assert.doesNotMatch(
    script,
    /git show -s --format=%T "\$commit"/,
    'no site may hand-derive emptiness from trees; the shared module owns it',
  )
  assert.equal(
    (script.match(/^empty_commit_shas\(\) \{$/gm) ?? []).length,
    1,
    'the range classifier must be defined exactly once',
  )
  assert.match(
    script,
    /node "\$COMMIT_WORK_PREDICATE" empty-commits --empty-tree "\$empty_tree"/,
    'classification must go through the shared predicate module',
  )
  // Resolve the dependency BEFORE anything rewrites history: a missing module must fail
  // with the branch untouched, never half-rebased.
  assert.ok(
    script.indexOf('COMMIT_WORK_PREDICATE="$SCRIPT_DIR/toolbox/commit-work-predicate.mjs"') <
      script.indexOf('git fetch origin "$BASE_BRANCH"'),
    'the predicate must be resolved before the first history-touching command',
  )
  // The heredoc is unexpanded, so the environment is the ONLY channel into the helper.
  assert.match(script, /^export PR_TAG_EMPTY_SHAS$/m, 'the helper is fed through the environment')
  assert.match(
    script,
    /^is_known_empty_commit\(\) \{\n(?:  #[^\n]*\n)*  \[ -n "\$1" \] \|\| return 1\n  printf '%s\\n' "\$PR_TAG_EMPTY_SHAS" \| grep -qxF "\$1"\n\}$/m,
    'the inner helper must consult the passed-in set, not re-derive it',
  )
  // `grep -qxF ""` matches the blank line an empty set prints, so an unguarded empty
  // needle would classify an unresolved commit as empty and skip its amend. Both
  // membership tests must refuse it.
  assert.equal(
    (script.match(/^  \[ -n "\$1" \] \|\| return 1$/gm) ?? []).length,
    2,
    'both membership tests must refuse an empty commit id',
  )
  // The rebase rewrites every SHA, so the post-condition must re-classify rather than
  // reuse the pre-rebase set — and must still fail closed when it cannot.
  assert.match(
    script,
    /POST_EMPTY_SHAS=\$\(empty_commit_shas "\$BASE_COMMIT"\) \|\| \{/,
    'the post-condition must re-classify the rebased range',
  )
  assert.equal(
    (script.match(/ERROR: could not classify the empty commits in \$BASE_COMMIT\.\.HEAD\./g) ?? [])
      .length,
    2,
    'both classifications must fail closed',
  )
  assert.ok(
    script.lastIndexOf('"$PR_TAG_EMPTY_SHAS"') < script.indexOf('POST_EMPTY_SHAS='),
    'the pre-rebase set must not be reused after the rebase rewrote its SHAs',
  )
  // A non-zero --exec strands the worktree mid-rebase, so the helper still swallows a
  // rejected amend and lets the outer post-condition be the thing that fails.
  assert.match(
    script,
    /# Deliberately exit 0: a non-zero --exec strands the worktree mid-rebase/,
    'the exec helper must keep exiting zero on a rejected amend',
  )
  assert.match(script, /git hash-object -t tree \/dev\/null/, 'empty tree must be computed by git')
  assert.match(
    script,
    /^current_commit\(\) \{$/m,
    'inner helper must inspect rebase state for the commit currently under --exec',
  )
  assert.match(
    script,
    /git rev-parse --git-path rebase-merge\/done/,
    'inner helper must read the rebase done file before falling back to HEAD',
  )
  assert.match(
    script,
    /CURRENT_COMMIT=\$\(current_commit\)\nif is_known_empty_commit "\$CURRENT_COMMIT"; then/,
    'empty-commit skip must classify the current rebase pick, not only HEAD',
  )
  assert.match(
    script,
    /if printf '%s' "\$AMEND_ERR" \| grep -q "would make it empty"; then/,
    'git amend empty-commit rejection must be converted to an empty skip, not a skip-report row',
  )
  assert.doesNotMatch(script, /4b825dc642cb6eb9a060e54bf8d69288fbee4904/, 'no SHA-1 literal')
  assert.match(
    script,
    /ERROR: could not read \$BASE_COMMIT\.\.HEAD/,
    'unreadable range fails closed',
  )
  assert.match(
    script,
    /ERROR: no commits found in \$BASE_COMMIT\.\.HEAD, but \$COMMIT_COUNT were expected\./,
    'empty scan with a positive commit count fails closed',
  )
  assert.doesNotMatch(
    script,
    /amending a commit into an empty one/,
    'empty commits are not amend failures',
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

// BOS-1195: emptiness is now answered by the shared predicate module vendored beside
// this script rather than by two hand-written shell copies. The dependency is therefore
// load-bearing, and the one thing that must never happen is discovering it is missing
// half way through a rebase — a stranded worktree is worse than a refused run.
test('add-pr-numbers case 15: a missing shared predicate fails closed, history untouched', () => {
  const repo = makeRepo({
    hook: PERMISSIVE_HOOK,
    commits: [
      { subject: 'chore: [skip ci] create pull request', empty: true },
      { subject: 'feat(core): add thing' },
    ],
  })
  const before = git(repo, 'rev-parse', 'HEAD')

  // A copy of the real script with no toolbox/ beside it.
  const isolated = path.join(tempDir('pr-tag-noopt-'), 'add-pr-numbers.sh')
  fs.copyFileSync(scriptPath, isolated)
  fs.chmodSync(isolated, 0o755)

  const errFile = path.join(tempDir('pr-tag-err-'), 'stderr.log')
  let code = 0
  try {
    execFileSync('bash', ['-c', '"$0" "$1" 2>"$2"', isolated, String(PR_NUM), errFile], {
      cwd: repo,
      encoding: 'utf8',
      env: { ...gitEnv, BASE_BRANCH: 'main' },
    })
  } catch (err) {
    assert.equal(err.signal, null, `script was signalled (${err.signal}), not exited`)
    code = err.status ?? 1
  }
  const stderr = fs.readFileSync(errFile, 'utf8')

  assert.notEqual(code, 0, 'a missing predicate must not be a silent success')
  assert.match(stderr, /shared commit predicate not found/, stderr)
  assert.match(stderr, /commit-work-predicate\.mjs/, stderr)
  assert.equal(git(repo, 'rev-parse', 'HEAD'), before, 'history must be untouched')
  for (const dir of ['rebase-merge', 'rebase-apply']) {
    assert.equal(fs.existsSync(path.join(repo, '.git', dir)), false, `no ${dir} may be left behind`)
  }
  assert.deepEqual(subjects(repo), [
    'chore: [skip ci] create pull request',
    'feat(core): add thing',
  ])
})

// The routed classification must still be the one the whole range is judged by: the
// module reads real trees, so a commit that only LOOKS like the bootstrap placeholder
// while carrying a diff is real work and is tagged like any other.
test('add-pr-numbers case 16: a non-empty commit wearing the bootstrap subject is tagged', () => {
  const bootstrap = 'chore: [skip ci] create pull request'
  const repo = makeRepo({
    hook: PERMISSIVE_HOOK,
    commits: [
      { subject: bootstrap, empty: true },
      { subject: bootstrap },
      { subject: 'feat(core): add thing' },
    ],
  })

  const r = runScript(repo)
  assert.equal(r.code, 0, `expected success:\n${r.stdout}\n${r.stderr}`)

  const after = subjects(repo)
  assert.equal(after[0], bootstrap, 'the EMPTY placeholder stays byte-identical')
  assert.ok(after[1].includes(TAG), `the non-empty lookalike must be tagged: ${after[1]}`)
  assert.ok(after[2].includes(TAG), `real work must be tagged: ${after[2]}`)
  assert.doesNotMatch(r.stderr, /untagged/, r.stderr)
})
