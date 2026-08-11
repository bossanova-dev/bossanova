#!/usr/bin/env node

// Contract test for skills-toolbox/sweep-pr-gate.sh (BOS-640).
//
// The helper is the PR-gate spine that used to be duplicated, byte-identical, inside
// bs-sweep-{debt,tests,mutation,prettify}/SKILL.md. Moving it out of markdown took it out of
// scripts/check-skill-shell.mjs's reach (that gate reads FENCED blocks in markdown) and
// scripts/lint-scripts.mjs only `node --check`s .cjs/.mjs — so a standalone .sh is covered by
// nothing else in the repo. This file is that coverage.
//
// It pins four things:
//   * bash -n — the file parses (the failure mode that produced improvement note [82]);
//   * the draft -> ready exact block, moved here out of scripts/bs-sweep-debt-skill.test.mjs;
//   * the parameter contract — an unset SESSION_BRANCH exits non-zero, enforced not documented;
//   * stdout hygiene — the caller captures stdout in a command substitution, so EVERY
//     diagnostic-emitting line must carry `>&2` and `printf '%s\n' "$PR_NUMBER"` must be the
//     only write to stdout. A regression here silently corrupts PR_NUMBER in all four sweeps.
//
// The stdout-hygiene checker is a line-oriented approximation of bash, not a parser: it has no
// heredoc or function-body state, so adding either to the helper will flag their inner lines as
// offenders. That direction is fail-CLOSED (a false red, never a false green) — if you hit it,
// teach the checker the construct rather than loosening the rule.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const rootDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const scriptPath = path.join(rootDir, 'skills-toolbox', 'sweep-pr-gate.sh')
const SOURCE = fs.readFileSync(scriptPath, 'utf8')

/** Run the helper with a controlled env; never throws. */
function run(env) {
  try {
    const stdout = execFileSync('bash', [scriptPath], {
      cwd: rootDir,
      encoding: 'utf8',
      env,
    })
    return { code: 0, stdout, stderr: '' }
  } catch (err) {
    return {
      code: err.status ?? 1,
      stdout: err.stdout?.toString() ?? '',
      stderr: err.stderr?.toString() ?? '',
    }
  }
}

test('the helper exists, is executable, and parses under bash -n', () => {
  assert.ok(fs.existsSync(scriptPath), 'skills-toolbox/sweep-pr-gate.sh must exist')
  // Git records only the exec bit (100755); checkout materialises `0777 & ~umask`, so an exact
  // 0o755 assertion reds on a clone under a different umask. The exec bit is the real contract.
  const mode = fs.statSync(scriptPath).mode & 0o777
  assert.ok(mode & 0o111, `sweep-pr-gate.sh must be executable, got mode ${mode.toString(8)}`)
  // Throws on a syntax error; the assertion is that this does not throw.
  execFileSync('bash', ['-n', scriptPath], { encoding: 'utf8' })
})

test('the helper carries no skill identity', () => {
  // The header comment names the FAMILY (`bs-sweep-*`); naming a MEMBER would make the helper
  // un-shareable, which is the whole point of extracting it.
  assert.ok(
    !/bs-sweep-(debt|tests|mutation|prettify|sentry|review|security)\b/.test(SOURCE),
    'helper must name no individual bs-sweep skill',
  )
  assert.ok(!/\.claude\/skills\/bs-sweep/.test(SOURCE), 'helper must carry no sweep skill path')
  assert.ok(!/\.mutate\//.test(SOURCE), 'helper must carry no skill-specific scratch path')
})

test('the draft -> ready exact block is present (moved out of bs-sweep-debt-skill.test.mjs)', () => {
  // This block was pinned by scripts/bs-sweep-debt-skill.test.mjs as an "external contract
  // block"; it now lives here, asserted against the helper's own bytes.
  const block = `if [ "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "true" ]; then
  gh pr ready "$PR_NUMBER" >&2
fi
test "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "false"`
  assert.ok(SOURCE.includes(block), 'sweep-pr-gate.sh must keep the exact draft -> ready block')
})

test('the find-or-create and [#N]-injection spine is present', () => {
  for (const token of [
    `gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty'`,
    'gh pr create \\',
    '--body-file "$PR_BODY" >&2',
    '"$ADD_PR_NUMBERS" "$PR_NUMBER" >&2',
    'git push --force-with-lease origin "$SESSION_BRANCH" >&2',
    'test "$(git rev-parse HEAD)" = "$(git rev-parse @{u})"',
  ]) {
    assert.ok(SOURCE.includes(token), `sweep-pr-gate.sh must contain: ${token}`)
  }
})

test('the [#N] injector is resolved from whichever agent skills home exists', () => {
  // Before extraction this call lived in mirrored SKILL.md text, where sync-codex-skills.mjs'
  // `~/.claude/skills/ -> ~/.codex/skills/` COMMON_REWRITES rule repointed it for the Codex
  // mirror. A .sh is never rewritten, and `boss` installs only the homes whose agent binary is
  // present (services/boss/cmd/main.go skips an agent that is not on PATH) — so hard-coding
  // either home makes the gate exit 127 under `set -e` on a host that has only the other.
  // bs-sweep-mutation runs under codex, so this is not hypothetical.
  assert.ok(
    SOURCE.includes(
      'ADD_PR_NUMBERS="$HOME/.claude/skills/bossanova/boss-finalize/add-pr-numbers.sh"',
    ),
    'the gate must probe the Claude skills home for add-pr-numbers.sh',
  )
  assert.ok(
    SOURCE.includes(
      'ADD_PR_NUMBERS="$HOME/.codex/skills/bossanova/boss-finalize/add-pr-numbers.sh"',
    ),
    'the gate must fall back to the Codex skills home for add-pr-numbers.sh',
  )
  assert.ok(
    SOURCE.includes('if [ ! -x "$ADD_PR_NUMBERS" ]; then'),
    'the fallback must be guarded by an executable-existence probe, not applied unconditionally',
  )
  const code = SOURCE.split('\n').filter((line) => !line.trim().startsWith('#'))
  assert.ok(
    !code.some((line) => /~\/\.(claude|codex)\/skills\//.test(line)),
    'no un-probed tilde skills-home path may remain — a .sh is never path-rewritten by the mirror',
  )
})

test('the [#N] check fails closed when git log itself fails', () => {
  // Bash suppresses errexit inside an `if` condition, so `if git log … | grep -v …; then` made a
  // FAILING git log (missing origin/$BASE_BRANCH, a corrupt object) produce exactly the same
  // false condition as "grep found no untagged commits" — the gate would then force-push and
  // ready a PR whose commits carry no [#N]. Running git log as its own assignment restores
  // errexit; `|| true` must sit on the GREP, whose no-match exit 1 is the only status that may
  // be absorbed here.
  assert.ok(
    SOURCE.includes('COMMITS="$(git log "origin/$BASE_BRANCH"..HEAD --oneline)"'),
    'git log must run as its own command, where errexit applies, not as an `if` condition head',
  )
  assert.ok(
    SOURCE.includes(`UNTAGGED="$(printf '%s' "$COMMITS" | grep -v "\\[#$PR_NUMBER\\]" || true)"`),
    'only grep’s no-match exit may be absorbed, and an empty COMMITS must stay empty',
  )
  assert.ok(
    SOURCE.includes('if [ -n "$UNTAGGED" ]; then'),
    'the untagged-commit branch must key on the captured list, not on a pipeline exit status',
  )
  assert.ok(
    !/^if git log/m.test(SOURCE),
    'no git log may head an `if` condition — errexit does not apply there',
  )
  // Without this the block still fails closed, but the operator is told only THAT commits are
  // untagged, never WHICH — a diagnostic the pre-fix `grep … >&2` did provide.
  assert.ok(
    SOURCE.includes('echo "$UNTAGGED" >&2'),
    'the untagged commits themselves must be listed on stderr, not just the summary line',
  )
})

// ---------------------------------------------------------------------------
// Behavioural harness — the gate driven end to end against stubbed git/gh.
//
// Every assertion above reads the helper's SOURCE, and a source pin cannot see a semantic
// escape: inserting `set +e` before the COMMITS= assignment leaves all of them green while
// restoring the exact fail-open the errexit fix exists to close. These cases RUN the script.
// ---------------------------------------------------------------------------

// The stubs are STATEFUL on purpose. A `pr list` that always answers 7 would make the whole
// find-or-create branch dead code, and a `rev-parse` that answers every argument identically
// would make the push-landed check trivially true — both would pass for the wrong reason.
const GH_STUB = `#!/usr/bin/env bash
echo "gh $*" >> "$STUB_TRACE"
case "$*" in
  "pr list"*)
    if [ "$STUB_PR_EXISTS" = true ] || [ -f "$STUB_CREATED_MARKER" ]; then
      echo "$STUB_PR_NUMBER"
    fi ;;
  "pr create"*)          : > "$STUB_CREATED_MARKER"; echo created ;;
  *"--json baseRefName"*) echo main ;;
  *"--json isDraft"*)
    if [ -f "$STUB_READY_MARKER" ]; then echo false; else echo "$STUB_IS_DRAFT"; fi ;;
  "pr ready"*)           : > "$STUB_READY_MARKER"; echo readied ;;
esac
`

// The `log` case keys on the RANGE, not just on $STUB_GIT_LOG: the gate now runs two different
// `git log`s — the branch-safety guard's `origin/$BASE..$START_SHA` and the [#N] check's
// `origin/$BASE..HEAD`. A stub that answered both identically would let one check pass for the
// other's reason (e.g. the guard "passing" only because the [#N] fixture happens to be empty),
// which is exactly the wrong-reason failure this header warns about.
//
// The guard arm prefixes each subject with a fake abbreviated hash because the guard logs
// `--format='%h %s'`, not `%s`. Fixtures stay readable (callers pass bare subjects) while the
// stub still reproduces the real line shape the guard's ERE is anchored against — a stub that
// emitted bare subjects would make the anchor untested and a blank subject invisible.
const GIT_STUB = `#!/usr/bin/env bash
echo "git $*" >> "$STUB_TRACE"
case "$1" in
  log)
    case "$*" in
      *"..HEAD"*)
        case "$STUB_GIT_LOG" in
          tagged)   printf 'aaa [#7] one\\n' ;;
          untagged) printf 'aaa [#7] one\\nbbb untagged subject\\n' ;;
          empty)    : ;;
          fail)     echo "fatal: bad revision 'origin/main..HEAD'" >&2; exit 128 ;;
        esac ;;
      *"fail-guard"*) echo "fatal: bad revision 'origin/main..fail-guard'" >&2; exit 128 ;;
      *) if [ -n "$STUB_PRE_EXISTING" ]; then printf '%s\\n' "$STUB_PRE_EXISTING" | sed 's/^/deadbee /'; fi ;;
    esac ;;
  rev-parse)
    case "$2" in
      HEAD) echo "$STUB_LOCAL_HEAD" ;;
      *)    echo "$STUB_REMOTE_HEAD" ;;
    esac ;;
  push)
    if [ "$STUB_PUSH_FAILS" = true ]; then
      echo "! [rejected] feature -> feature (stale info)" >&2
      exit 1
    fi ;;
esac
`

/** Build a sandbox (stub PATH + a fake HOME) and run the real helper inside it. */
function runStubbed({
  gitLog = 'tagged',
  homes = ['.claude'],
  isDraft = 'false',
  prExists = true,
  prNumber = '7',
  preExisting = '',
  pushLands = true,
  pushFails = false,
  startSha = 'startsha',
} = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sweep-pr-gate-'))
  const bin = path.join(dir, 'bin')
  const home = path.join(dir, 'home')
  const trace = path.join(dir, 'trace.log')
  fs.mkdirSync(bin, { recursive: true })
  fs.writeFileSync(trace, '')
  for (const agentHome of homes) {
    const finalize = path.join(home, agentHome, 'skills', 'bossanova', 'boss-finalize')
    fs.mkdirSync(finalize, { recursive: true })
    const injector = path.join(finalize, 'add-pr-numbers.sh')
    fs.writeFileSync(injector, '#!/usr/bin/env bash\necho "add-pr-numbers ran" >&2\n')
    fs.chmodSync(injector, 0o755)
  }
  for (const [name, body] of [
    ['gh', GH_STUB],
    ['git', GIT_STUB],
  ]) {
    fs.writeFileSync(path.join(bin, name), body)
    fs.chmodSync(path.join(bin, name), 0o755)
  }
  const result = run({
    PATH: `${bin}:/usr/bin:/bin`,
    HOME: home,
    STUB_TRACE: trace,
    STUB_READY_MARKER: path.join(dir, 'readied'),
    STUB_CREATED_MARKER: path.join(dir, 'created'),
    STUB_GIT_LOG: gitLog,
    STUB_PRE_EXISTING: preExisting,
    STUB_IS_DRAFT: isDraft,
    STUB_PR_EXISTS: String(prExists),
    STUB_PR_NUMBER: prNumber,
    STUB_LOCAL_HEAD: 'deadbeef',
    STUB_REMOTE_HEAD: pushLands ? 'deadbeef' : 'cafebabe',
    STUB_PUSH_FAILS: String(pushFails),
    SESSION_BRANCH: 'feature',
    BASE_BRANCH: 'main',
    PR_BODY: '/dev/null',
    START_SHA: startSha,
  })
  const calls = fs.readFileSync(trace, 'utf8')
  fs.rmSync(dir, { force: true, recursive: true })
  return { ...result, calls }
}

test('behaviour: the happy path prints the PR number and nothing else on stdout', () => {
  const { code, stdout, calls } = runStubbed()
  assert.equal(code, 0)
  assert.equal(stdout, '7\n', 'stdout is the PR number alone — the caller captures it verbatim')
  assert.match(calls, /git push/, 'a successful gate pushes')
  // Real `gh pr create` exits non-zero when a PR already exists for the branch, so under
  // `set -e` an inverted find-or-create would abort every rerun of every sweep.
  assert.doesNotMatch(calls, /pr create/, 'an existing PR is found, never re-created')
  // The base ref must be fetched BEFORE the commit range is computed: against a stale
  // origin/$BASE_BRANCH the range widens to already-merged commits, which carry someone
  // else's tag or none, and the [#N] check then fails on commits this run never made.
  // Pin it per RANGE, not by counting fetches. There are two fetch/log pairs now: the guard's,
  // against the CALLER-supplied BASE_BRANCH, and the [#N] check's, which re-fetches AFTER
  // BASE_BRANCH is reassigned to the PR's baseRefName — a ref that may differ. A `findIndex`
  // over fetches matches the guard's and leaves the [#N] check's own fetch unpinned (deleting it
  // kept the whole suite green); asserting `fetches.length === 2` would pin it but freeze the
  // fetch COUNT, so an unrelated third fetch would red. Asserting a fetch sits between the
  // baseRefName lookup and the range that depends on it says the actual invariant instead — and
  // still reds if EITHER fetch is deleted, each with its own message.
  const order = calls.split('\n')
  const at = (pred) => order.findIndex(pred)
  const guardLogged = at((c) => c.startsWith('git log') && c.includes('..startsha'))
  const rebased = at((c) => c.includes('--json baseRefName'))
  const logged = at((c) => c.startsWith('git log') && c.includes('..HEAD'))
  assert.ok(guardLogged !== -1 && rebased !== -1 && logged !== -1, 'both ranges must be computed')
  assert.ok(
    order.slice(0, guardLogged).some((c) => c.startsWith('git fetch origin main')),
    "the guard's range must be computed against a freshly fetched caller base",
  )
  assert.ok(
    order.slice(rebased + 1, logged).some((c) => c.startsWith('git fetch origin main')),
    "the [#N] range must be computed against a base fetched AFTER the PR's baseRefName is read",
  )
})

test('behaviour: with no open PR the gate creates one and re-resolves its number', () => {
  const { code, stdout, calls } = runStubbed({ prExists: false })
  assert.equal(code, 0)
  assert.equal(stdout, '7\n', 'the number comes from the re-list, not from the create output')
  assert.match(calls, /pr create/, 'no open PR means the create branch runs')
  assert.equal(calls.match(/gh pr list/g)?.length, 2, 'and the number is re-resolved after it')
})

test('behaviour: a create that resolves no number aborts before any push', () => {
  // `test -n "$PR_NUMBER"` right after the find-or-create split: without it an empty number
  // flows into `gh pr view ""` and every later step operates on nothing.
  const { code, stdout, calls } = runStubbed({ prExists: false, prNumber: '' })
  assert.notEqual(code, 0)
  assert.equal(stdout, '')
  assert.doesNotMatch(
    calls,
    /pr view/,
    'it must abort at the guard, before any command is handed the empty number',
  )
  assert.doesNotMatch(calls, /git push/)
  assert.doesNotMatch(calls, /pr ready/)
})

test('behaviour: a rejected force-push aborts the gate', () => {
  // Deliberately isolates "push errored" from "push did not land": real git couples them, and
  // that coupling is exactly what hides a `|| true` on the push — the HEAD == @{u} check would
  // still catch a stale remote, so only an errored push whose remote happens to match proves
  // the push's own exit status is honoured.
  const { code, stdout, calls } = runStubbed({ pushFails: true })
  assert.notEqual(code, 0, 'the push exit status must be honoured, not swallowed')
  assert.equal(stdout, '')
  assert.doesNotMatch(calls, /pr ready/)
})

test('behaviour: a push that does not land aborts before readying the PR', () => {
  // A rejected --force-with-lease (someone else pushed the branch) must not end with a ready
  // PR whose remote tip is not the commit set this gate validated.
  const { code, stdout, calls } = runStubbed({ pushLands: false })
  assert.notEqual(code, 0, 'HEAD != @{u} must abort the gate')
  assert.equal(stdout, '')
  assert.doesNotMatch(calls, /pr ready/, 'a PR is never readied on an unlanded push')
})

test('behaviour: a failing git log aborts instead of readying an untagged PR', () => {
  // The pre-fix form swallowed this: git log failed, the `if` condition went false, and the
  // gate pushed and readied a PR whose commits carry no [#N].
  const { code, stdout, calls } = runStubbed({ gitLog: 'fail' })
  assert.notEqual(code, 0, 'a failing git log must abort the gate')
  assert.equal(stdout, '', 'an aborted gate writes no PR number, so the caller guard fires')
  assert.doesNotMatch(calls, /git push/, 'it must abort BEFORE force-pushing')
  assert.doesNotMatch(calls, /pr ready/, 'and before readying the PR')
})

test('behaviour: an untagged commit aborts and names the offending commit', () => {
  const { code, stdout, stderr, calls } = runStubbed({ gitLog: 'untagged' })
  assert.notEqual(code, 0)
  assert.equal(stdout, '')
  assert.match(stderr, /bbb untagged subject/, 'the offending commit is listed on stderr')
  assert.match(stderr, /Commits still missing \[#7\]/)
  assert.doesNotMatch(calls, /git push/)
})

test('behaviour: no commits ahead is not an untagged-commit failure', () => {
  // An empty commit range must stay a PASS: grep sees no input, so UNTAGGED is empty and the
  // untagged branch is skipped. What this case uniquely kills is an ADDED empty-range guard
  // (`if [ -z "$COMMITS" ] || [ -n "$UNTAGGED" ]`), which reads like a safety improvement and
  // would abort every clean rerun. Swapping the branch to key on `$COMMITS` is caught by the
  // happy path, not here — that mutation makes THIS case pass more readily, not less.
  const { code, stdout } = runStubbed({ gitLog: 'empty' })
  assert.equal(code, 0, 'an empty commit range is a pass, exactly as before the extraction')
  assert.equal(stdout, '7\n')
})

// ---------------------------------------------------------------------------
// Branch-safety guard (BOS-653) — the gate refuses a branch it does not own.
//
// THESE TWO FIXES ARE ORDERED, AND THAT IS WHY THEY SHIPPED IN ONE PR. bs-sweep-prettify's
// Phase 1 ran `make format` — BOS-371's CHANGED-FILES formatter — so on a fresh cron branch it
// found nothing, emitted a false NO_CHANGE at Phase 2, and never reached this gate at all. That
// staleness was accidentally masking the hole below: the gate retags every commit in
// origin/$BASE..HEAD, force-pushes with lease, and flips the branch's open draft PR to ready,
// without ever asking whether that PR is the sweep's. Landing the Phase 1 `make format-all` fix
// ALONE removes the mask and converts a dormant PR-hijack into a live one. Do not split them,
// and do not delete this guard while Phase 1 still runs a whole-repo formatter.
//
// The highest-consequence way to get the guard wrong is to make it too broad: bossd bootstraps
// EVERY session with an empty placeholder commit, so a naive "any commit ahead of base" refusal
// would silently disable all four sweeps. The two placeholder cases below exist for that.
// ---------------------------------------------------------------------------

test('behaviour: a branch carrying a foreign commit is refused before any GitHub call', () => {
  const { code, stdout, stderr, calls } = runStubbed({
    preExisting: 'feat(web): someone else’s work',
    prExists: true,
  })
  assert.notEqual(code, 0, 'a branch the sweep does not own must not be retagged or readied')
  assert.equal(stdout, '', 'a refusal writes no PR number, so the caller guard fires')
  assert.match(stderr, /someone else’s work/, 'the offending subject must be named on stderr')
  assert.match(stderr, /already carried commits before this sweep started/)
  assert.doesNotMatch(calls, /^gh /m, 'the refusal precedes GitHub entirely — no gh call at all')
  assert.doesNotMatch(calls, /pr create/)
  assert.doesNotMatch(calls, /pr ready/)
  assert.doesNotMatch(calls, /git push/)
})

test('behaviour: bossd’s draft-PR placeholder commit is not foreign', () => {
  // Without this case the guard would look correct while refusing every real cron sweep —
  // bossd opens the session draft PR by pushing exactly this empty commit
  // (DraftPRPlaceholderCommitSubject, services/bossd/internal/git/worktree.go).
  const { code, stdout } = runStubbed({ preExisting: 'chore: [skip ci] create pull request' })
  assert.equal(code, 0, 'the bootstrap placeholder must not trip the guard')
  assert.equal(stdout, '7\n')
})

test('behaviour: a [#N]-tagged placeholder commit is still not foreign', () => {
  // add-pr-numbers.sh tags the placeholder unconditionally and its idempotence check only
  // recognises the CURRENT number, so tags stack — hence draftPRPlaceholderTagRE's `+`.
  for (const subject of [
    'chore: [#7] [skip ci] create pull request',
    'chore: [#7] [#42] [skip ci] create pull request',
  ]) {
    const { code, stdout } = runStubbed({ preExisting: subject })
    assert.equal(code, 0, `a tagged placeholder must not trip the guard: ${subject}`)
    assert.equal(stdout, '7\n')
  }
})

test('behaviour: a failing guard git log aborts instead of proceeding unowned', () => {
  // The guard's exact counterpart to the [#N] check's own fail-closed case above, and for the
  // same reason commit 11327fe30 added that one. Collapse the guard's two own-line assignments
  // into the one-liner a future editor would naturally write —
  //   FOREIGN="$(git log … | grep -Ev '…' || true)"
  // — and under pipefail a FAILING git log (a bad START_SHA, a missing origin/$BASE_BRANCH, a
  // corrupt object) yields the same empty FOREIGN as "no foreign commits": the gate would then
  // retag, force-push and ready a branch whose ownership it could not prove. As separate
  // assignments it aborts. A source pin cannot see that escape, so this case RUNS the script.
  const { code, stdout, calls } = runStubbed({ startSha: 'fail-guard', prExists: true })
  assert.notEqual(code, 0, 'an unprovable ownership check must abort the gate')
  assert.equal(stdout, '', 'and write no PR number, so the caller guard fires')
  assert.doesNotMatch(calls, /^gh /m, 'and never touch GitHub')
  assert.doesNotMatch(calls, /git push/)
})

test('behaviour: a blank-subject pre-existing commit is foreign, not invisible', () => {
  // --allow-empty-message commits log a blank subject. Under a bare `--format=%s` both command
  // substitutions strip the resulting blank lines, so FOREIGN came back empty and the guard
  // failed OPEN on a foreign branch. `%h ` makes every logged line unconditionally non-empty.
  const blank = runStubbed({ preExisting: '\n', prExists: true })
  assert.notEqual(blank.code, 0, 'a blank-subject commit is a real commit and must be refused')
  assert.equal(blank.stdout, '', 'a refusal writes no PR number')
  assert.doesNotMatch(blank.calls, /^gh /m, 'and the refusal still precedes GitHub')

  // The control: a genuinely EMPTY range must stay allowed, or the fix above would refuse every
  // sweep — the same over-broad failure the placeholder cases guard against.
  const none = runStubbed({ preExisting: '', prExists: true })
  assert.equal(none.code, 0, 'no pre-existing commits at all is not foreign')
  assert.equal(none.stdout, '7\n')
})

test('behaviour: on a clean defer_pr branch the guard passes and the gate creates its own PR', () => {
  // The satisfiable stand-in for the ticket's live-sweep criterion: no bootstrap PR, no
  // pre-existing commits, so the gate opens the PR it then readies.
  const { code, stdout, calls } = runStubbed({ preExisting: '', prExists: false })
  assert.equal(code, 0)
  assert.equal(stdout, '7\n')
  assert.match(calls, /pr create/, 'a branch with no PR gets one created by the gate itself')
  // Not `pr ready|git push`: the default fixture is already out of draft, so the `pr ready` arm
  // can never fire and the disjunction would claim coverage it does not have.
  assert.match(calls, /git push/, 'and the run proceeds past the guard to a real push')
})

test('behaviour: the guard’s commit-range check precedes every GitHub call', () => {
  const order = runStubbed().calls.split('\n')
  // Key on START_SHA, not merely on "not ..HEAD": `gh pr create --title` also shells out to a
  // `git log -1 --pretty=%s` that carries no range, and matching THAT would satisfy this
  // assertion while saying nothing about the guard.
  const guardLogged = order.findIndex((c) => c.startsWith('git log') && c.includes('..startsha'))
  const firstGh = order.findIndex((c) => c.startsWith('gh '))
  assert.notEqual(guardLogged, -1, 'the guard must compute origin/$BASE_BRANCH..$START_SHA')
  assert.notEqual(firstGh, -1)
  assert.ok(
    guardLogged < firstGh,
    'the ownership check must run while the gate has changed nothing on GitHub',
  )
})

test('the placeholder regex accepts the subject read from worktree.go, not a retyped literal', () => {
  // Cross-language duplication: the shell guard re-expresses DraftPRPlaceholderCommitSubject +
  // draftPRPlaceholderTagRE in an ERE. Derive BOTH from the Go source — a retyped literal here
  // would stay green after the constant moves, which is precisely the drift being pinned.
  const goSource = fs.readFileSync(
    path.join(rootDir, 'services', 'bossd', 'internal', 'git', 'worktree.go'),
    'utf8',
  )
  const subjectMatch = goSource.match(/const DraftPRPlaceholderCommitSubject = "([^"]+)"/)
  assert.ok(subjectMatch, 'DraftPRPlaceholderCommitSubject must be readable from worktree.go')
  const subject = subjectMatch[1]
  const tagMatch = goSource.match(
    /draftPRPlaceholderTagRE = regexp\.MustCompile\(`\^\((.+?)\)\+`\)/,
  )
  assert.ok(tagMatch, 'draftPRPlaceholderTagRE must be readable from worktree.go')
  const tagShape = tagMatch[1]

  // Flag-tolerant (the guard also passes `-n`), but it still requires a single-quoted ERE fed to
  // `grep -Ev` — the `[#N]` check's `grep -v "…"` is double-quoted and can never match here.
  const patternMatch = SOURCE.match(/grep (?:-\S+ )*-Ev '([^']+)'/)
  assert.ok(patternMatch, 'the guard must filter the placeholder with a `grep -Ev` pattern')
  const pattern = patternMatch[1]
  assert.ok(
    pattern.includes(tagShape),
    `the guard's pattern must tolerate Go's tag shape ${tagShape}, got: ${pattern}`,
  )

  /**
   * The subjects the guard would call foreign, per the real grep it runs.
   *
   * Each subject is fed in the `%h %s` shape the guard actually logs — feeding bare subjects
   * would leave the pattern's `^<hash> ` anchor untested and silently accept a subject that
   * merely CONTAINS the placeholder.
   */
  const foreign = (subjects) =>
    spawnSync('grep', ['-Ev', pattern], {
      encoding: 'utf8',
      input: `${subjects.map((s) => `deadbee ${s}`).join('\n')}\n`,
    }).stdout

  const prefix = subject.slice(0, subject.indexOf(': ') + 2)
  const rest = subject.slice(prefix.length)
  assert.equal(foreign([subject]), '', `the guard must accept ${subject}`)
  assert.equal(foreign([`${prefix}[#7] ${rest}`]), '', 'and its single-tagged form')
  assert.equal(foreign([`${prefix}[#7] [#42] ${rest}`]), '', 'and its stacked-tag form')
  assert.equal(
    foreign(['feat(web): real work']),
    'deadbee feat(web): real work\n',
    'a real commit subject must still be foreign — the pattern must not match everything',
  )
  assert.equal(
    foreign(['']),
    'deadbee \n',
    'a blank subject must be foreign — %h is what keeps the line visible to grep',
  )
  assert.equal(
    foreign([`x ${subject}`]),
    `deadbee x ${subject}\n`,
    'and a subject that merely CONTAINS the placeholder must not be laundered by the anchor',
  )
})

test('behaviour: the gate runs under a Codex-only home', () => {
  // The regression this guards: the pre-fix helper hard-coded ~/.claude, so a host with only
  // ~/.codex (boss installs only the homes whose agent binary is present) died at exit 127.
  const { code, stdout } = runStubbed({ homes: ['.codex'] })
  assert.equal(code, 0, 'a Codex-only host must reach a ready PR')
  assert.equal(stdout, '7\n')
})

test('behaviour: with no injector in either home the gate refuses before touching GitHub', () => {
  const { code, stdout, stderr, calls } = runStubbed({ homes: [] })
  assert.notEqual(code, 0)
  assert.equal(stdout, '')
  assert.match(stderr, /no executable add-pr-numbers\.sh/)
  assert.equal(
    calls,
    '',
    'the precondition must fail while the gate has changed nothing — discovering it later ' +
      'leaves a created, still-draft PR whose commits carry no [#N]',
  )
})

test('behaviour: a draft PR is flipped ready; a ready one is left alone', () => {
  const draft = runStubbed({ isDraft: 'true' })
  assert.match(draft.calls, /pr ready/, 'a draft PR is readied')
  assert.equal(draft.code, 0, 'and the gate then confirms it is out of draft')
  assert.equal(draft.stdout, '7\n')
  assert.doesNotMatch(runStubbed().calls, /pr ready/, 'an already-ready PR is not re-readied')
})

test('a failure anywhere in the gate is announced on stderr', () => {
  // `test -n "$PR_NUMBER"`, the push-landed check and the final isDraft check all abort under
  // `set -e` printing nothing of their own; the ERR trap is what distinguishes them from each
  // other and from success. Its message must itself go to stderr — stdout is PR_NUMBER.
  assert.match(SOURCE, /^trap '.*>&2' ERR$/m, 'the gate must trap ERR and report it on stderr')
})

test('every required input is enforced, not merely documented', () => {
  const base = { PATH: process.env.PATH, HOME: process.env.HOME }
  // Unset SESSION_BRANCH -> the `:?` guard aborts before any gh call.
  const noBranch = run({ ...base, BASE_BRANCH: 'main', PR_BODY: '/dev/null' })
  assert.notEqual(noBranch.code, 0, 'unset SESSION_BRANCH must exit non-zero')
  assert.match(noBranch.stderr, /SESSION_BRANCH required/)
  assert.equal(noBranch.stdout, '', 'a failed precondition must write nothing to stdout')

  const noBase = run({ ...base, SESSION_BRANCH: 'b', PR_BODY: '/dev/null' })
  assert.notEqual(noBase.code, 0, 'unset BASE_BRANCH must exit non-zero')
  assert.match(noBase.stderr, /BASE_BRANCH required/)

  const noBody = run({ ...base, SESSION_BRANCH: 'b', BASE_BRANCH: 'main' })
  assert.notEqual(noBody.code, 0, 'unset PR_BODY must exit non-zero')
  assert.match(noBody.stderr, /PR_BODY required/)

  // START_SHA is the branch-safety guard's whole input. Defaulting it (to HEAD, say) would make
  // the guard's range empty and silently disarm it, so it must be REQUIRED, not optional.
  const noStart = run({ ...base, SESSION_BRANCH: 'b', BASE_BRANCH: 'main', PR_BODY: '/dev/null' })
  assert.notEqual(noStart.code, 0, 'unset START_SHA must exit non-zero')
  assert.match(noStart.stderr, /START_SHA required/)
  assert.equal(noStart.stdout, '', 'a failed precondition must write nothing to stdout')
})

/**
 * Remove every `$( … )` command substitution — its output is captured, never emitted.
 *
 * Parens are counted without quote state, so a `(` inside a quoted string inside a
 * substitution (`X="$(basename "a(b")"`) leaves the scan open and swallows the rest of the
 * line — which would HIDE a stdout writer after it. `balanced` reports that: the caller treats
 * an unbalanced line as an offender rather than trusting the truncated remainder, so the one
 * shape this approximation cannot read is a red, not a silent pass.
 */
function stripSubstitutions(text) {
  let out = ''
  let depth = 0
  for (let i = 0; i < text.length; i += 1) {
    if (text[i] === '$' && text[i + 1] === '(') {
      depth += 1
      i += 1
      continue
    }
    if (depth > 0) {
      if (text[i] === '(') depth += 1
      else if (text[i] === ')') depth -= 1
      continue
    }
    out += text[i]
  }
  return { code: out, balanced: depth === 0 }
}

/** Join `\`-continuations so a multi-line invocation is judged as one command. */
function logicalLines(text) {
  const joined = []
  let buffer = null
  for (const [i, raw] of text.split('\n').entries()) {
    const line = raw.trim()
    if (buffer === null) buffer = { line: '', lineNo: i + 1 }
    buffer.line += (buffer.line ? ' ' : '') + line.replace(/\\$/, '').trim()
    if (!line.endsWith('\\')) {
      joined.push(buffer)
      buffer = null
    }
  }
  if (buffer !== null) joined.push(buffer)
  return joined
}

// Commands that provably cannot write to this script's stdout. This is an ALLOWLIST, not a
// denylist of today's emitters: anything absent must redirect, so a newly introduced writer
// (`cat`, `jq`, `tee`, an unfamiliar `gh` subcommand) is caught by default instead of having
// to have been predicted here. Adding to this set is a deliberate, reviewable act.
const SILENT_COMMANDS = new Set([
  ':',
  '[',
  'test',
  'set',
  'export',
  'unset',
  'shift',
  'exit',
  'return',
  'true',
  'false',
  // `trap` registers a handler rather than writing anything itself; its body's own redirect is
  // pinned separately by the "a failure anywhere in the gate is announced on stderr" test.
  'trap',
])

// Three allowlist members are silent ONLY when given operands: bare `set` dumps every shell
// variable, bare `export` dumps the exported environment, and `trap -p` prints the trap list —
// all to stdout. Require operands for these, and reject the one `trap` form that prints.
const OPERANDS_REQUIRED = new Set(['set', 'export', 'trap'])
const SHELL_KEYWORDS = new Set([
  'if',
  'then',
  'else',
  'elif',
  'fi',
  'while',
  'until',
  'do',
  'done',
  'case',
  'esac',
  'in',
  '{',
  '}',
  '!',
])

/** The command word a segment actually runs, past any keyword and `VAR=…` prefixes ('' = none). */
function commandWord(segment) {
  const words = segment.trim().split(/\s+/).filter(Boolean)
  while (words.length > 0) {
    const head = words[0]
    // A bare assignment runs nothing; an env prefix defers to the word after it.
    if (SHELL_KEYWORDS.has(head) || /^[A-Za-z_][A-Za-z0-9_]*=/.test(head)) {
      words.shift()
      continue
    }
    break
  }
  return words[0] ?? ''
}

/**
 * Blank out the INSIDE of quoted spans so a `;` or `|` in a message is not read as a command
 * separator (the "Commits still missing …; rerun …" diagnostic contains one). Spaces are kept
 * so word-splitting still lines up; the quotes themselves are kept so the span stays visible.
 */
function maskQuoted(text) {
  let out = ''
  let quote = null
  for (const ch of text) {
    if (quote === null) {
      if (ch === "'" || ch === '"') quote = ch
      out += ch
      continue
    }
    if (ch === quote) {
      quote = null
      out += ch
      continue
    }
    out += ch === ' ' ? ' ' : 'x'
  }
  return out
}

/** True when an otherwise-silent builtin is used in the form that DOES print to stdout. */
function isPrintingForm(command, segment) {
  if (!OPERANDS_REQUIRED.has(command)) return false
  const words = segment.trim().split(/\s+/).filter(Boolean)
  const operands = words.slice(words.indexOf(command) + 1)
  // Bare `set` / `export` dump the variable set; `trap -p` prints the registered traps.
  if (operands.length === 0) return true
  return command === 'trap' && operands.includes('-p')
}

/** Split a logical line into the segments whose stdout reaches the terminal. */
function stdoutBearingSegments(line) {
  // Drop a trailing `#` comment first: a `>&2` inside one would otherwise satisfy the redirect
  // check for a live writer on the same line. `#` only starts a comment at a word boundary.
  const code = maskQuoted(line).replace(/(^|\s)#.*$/, '')
  // Split on every separator that ends a command, INCLUDING a bare `&` (backgrounding) — the
  // lookarounds keep `&&` to its own alternative and leave the `&` of a `>&2` fd-dup alone.
  // Only the LAST stage of a pipeline writes to stdout — earlier stages feed the pipe.
  return code
    .split(/&&|\|\||;|(?<![>&])&(?!&)/)
    .map((command) => command.split('|').pop())
    .filter((segment) => segment.trim() !== '')
}

test('stdout hygiene: only the final printf writes to stdout', () => {
  const STDOUT_WRITE = `printf '%s\\n' "$PR_NUMBER"`
  // Any command capable of writing to stdout must redirect to stderr — the caller reads this
  // script's stdout as PR_NUMBER. Command substitutions are stripped first (their output is
  // captured by the shell, never emitted), so what remains is genuinely live output.
  const offenders = []
  for (const { line, lineNo } of logicalLines(SOURCE)) {
    if (line === '' || line.startsWith('#')) continue
    if (line === STDOUT_WRITE) continue
    const { code, balanced } = stripSubstitutions(line)
    if (!balanced) {
      offenders.push(
        `${lineNo} (unreadable substitution — rewrite it or teach the checker): ${line}`,
      )
      continue
    }
    for (const segment of stdoutBearingSegments(code)) {
      const command = commandWord(segment)
      if (command === '') continue
      if (SILENT_COMMANDS.has(command) && !isPrintingForm(command, segment)) continue
      if (!segment.includes('>&2')) offenders.push(`${lineNo}: ${line}`)
    }
  }
  assert.deepEqual(
    offenders,
    [],
    `every diagnostic-emitting line must carry >&2 — stdout is the caller's PR_NUMBER:\n${offenders.join('\n')}`,
  )

  const lines = SOURCE.split('\n')

  // The sanctioned stdout write exists, exactly once, and is the last statement.
  const printfs = lines.filter((l) => l.trim().startsWith('printf '))
  assert.deepEqual(
    printfs.map((l) => l.trim()),
    [`printf '%s\\n' "$PR_NUMBER"`],
    'exactly one printf, writing the PR number, is the whole stdout contract',
  )
  const body = lines.filter((l) => l.trim() !== '')
  assert.equal(
    body[body.length - 1].trim(),
    `printf '%s\\n' "$PR_NUMBER"`,
    'the PR number must be the last thing the helper does',
  )
})

test('the helper fails closed: set -euo pipefail', () => {
  assert.ok(SOURCE.includes('set -euo pipefail'), 'must fail closed on any step')
})

test('all four PR-gate sweeps invoke the helper and check its result, in both mirrors', () => {
  // Pin the EXECUTED bytes, not the bare path: every skill also NAMES the helper in its
  // resident "executed, not read" prose, so a bare-path substring stays satisfied after the
  // fenced invocation is deleted — the silent-write-path failure this extraction exists to
  // prevent. `bash "$(git rev-parse …` only ever appears inside the fence.
  const INVOCATION = 'bash "$(git rev-parse --show-toplevel)/skills-toolbox/sweep-pr-gate.sh")"'
  // The helper writes NOTHING to stdout unless it succeeded all the way to a ready PR, so an
  // empty PR_NUMBER is the caller's complete failure detector. It must be the LAST command in
  // the fence: the fence has no `set -e`, so the block's exit status is its last command's,
  // and anything after the guard (`export PR_NUMBER` always exits 0) makes the guard inert —
  // the block then reports success and an empty PR_NUMBER flows into the check-watch phase as
  // a PR that was never created.
  const RESULT_CHECK = 'test -n "$PR_NUMBER" || exit 1\n```'
  // START_SHA is the branch-safety guard's only input, and the gate makes it REQUIRED — a caller
  // that omits it aborts at `: "${START_SHA:?}"`, yields an empty PR_NUMBER, and the sweep stops
  // producing PRs with no other signal: the same silent-inertness class as the stale Phase 1
  // formatter this branch fixes. Pin it HERE, across all four sweeps and both trees. Only
  // bs-sweep-debt quotes the whole invocation byte-identically in its own suite, so without this
  // the parameter could be dropped from the other three (in BOTH mirrors) with nothing failing.
  // Anchored from `PR_NUMBER="$(` so it pins the captured invocation, not a stray prose mention.
  const START_SHA_PIN =
    'PR_NUMBER="$(SESSION_BRANCH="$SESSION_BRANCH" START_SHA="$START_SHA" BASE_BRANCH="$BASE_BRANCH" '
  const sweeps = ['debt', 'tests', 'mutation', 'prettify']
  for (const tree of ['.claude', '.codex']) {
    for (const sweep of sweeps) {
      const rel = path.join(tree, 'skills', `bs-sweep-${sweep}`, 'SKILL.md')
      const skill = fs.readFileSync(path.join(rootDir, rel), 'utf8')
      assert.ok(skill.includes(INVOCATION), `${rel} must execute the PR gate: ${INVOCATION}`)
      assert.ok(
        skill.includes(START_SHA_PIN),
        `${rel} must pass START_SHA into the gate — the branch-safety guard requires it and the gate refuses without it: ${START_SHA_PIN}`,
      )
      assert.ok(
        skill.includes(RESULT_CHECK),
        `${rel} must close the gate fence with an exiting PR-number guard`,
      )
    }
  }
})

test('the PR body temp file is removed before the result is checked, where one is used', () => {
  // `rm -f "$PR_BODY"` must sit BETWEEN the invocation and the guard, or a failing gate leaves
  // the mktemp file behind — against each skill's "leave no local artifacts" hard rule.
  // bs-sweep-mutation passes a tracked scratch path (.mutate/pr-body.md) and deliberately has
  // no `rm`, so it is excluded here rather than silently passing a weaker check.
  for (const tree of ['.claude', '.codex']) {
    for (const sweep of ['debt', 'tests', 'prettify']) {
      const rel = path.join(tree, 'skills', `bs-sweep-${sweep}`, 'SKILL.md')
      const skill = fs.readFileSync(path.join(rootDir, rel), 'utf8')
      assert.ok(
        skill.includes(
          'sweep-pr-gate.sh")"\nrm -f "$PR_BODY"\nexport PR_NUMBER\ntest -n "$PR_NUMBER" || exit 1\n```',
        ),
        `${rel} must rm the PR body on both the success and failure paths, before the guard`,
      )
    }
  }
})
