// Content/contract test for the bs-sweep-security skill (BOS-144).
//
// This is the "content-test file pattern" the BOS-143 epic's children 2–5 copy:
// scripts/bs-<skill>-skill.test.mjs, wired into `make test-smoke` via the
// `scripts/bs-*-skill.test.mjs` glob. It pins the byte-stable external contracts
// the SKILL.md documents — the two subagent dispatch directives, the run-file
// sentinel tokens (sourced from skills-toolbox/bs-run-sentinel.mjs, the single source of
// truth), the dead-subagent/dispatch-failure branch, and the dry-run contract —
// plus a behavior-parity check that the triage gate selects a known batch on a
// non-empty fixture set.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { REPAIR_RESULTS, DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'

const readSkill = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const SKILL = readSkill('../.claude/skills/bs-sweep-security/SKILL.md')
const CODEX = readSkill('../.codex/skills/bs-sweep-security/SKILL.md')
const NOTES_TEARDOWN =
  "Before exiting, follow `bs-record-notes` with this run's outcome. Recording is non-fatal: never change the terminal state, exit code, or `git status --porcelain`. Skip gated/no-op runs that observed nothing."

/** Count non-overlapping occurrences of a literal substring. */
function countOf(haystack, needle) {
  let n = 0
  let i = 0
  for (;;) {
    const at = haystack.indexOf(needle, i)
    if (at === -1) return n
    n += 1
    i = at + needle.length
  }
}

test('documents the non-fatal notes teardown contract', () => {
  for (const [label, skill] of [
    ['source', SKILL],
    ['codex mirror', CODEX],
  ]) {
    assert.ok(skill.includes(NOTES_TEARDOWN), `${label} must include the notes teardown contract`)
  }
})

function allowedTools(skill) {
  const match = skill.match(/^allowed-tools:\s*(.+)$/m)
  assert.ok(match, 'skill frontmatter must declare allowed-tools')
  return match[1].split(',').map((tool) => tool.trim())
}

// ---------------------------------------------------------------------------
// Dispatch directives — both heavy steps run in awaited subagents.
// ---------------------------------------------------------------------------

test('both heavy steps dispatch a general-purpose subagent', () => {
  // Phase 2 (triage) + Phase 4 (repair-watch) each name the subagent type.
  assert.ok(
    countOf(SKILL, 'subagent_type: general-purpose') >= 2,
    'expected >= 2 `subagent_type: general-purpose` dispatch directives',
  )
})

test('frontmatter allows Task for the required subagent dispatches', () => {
  assert.ok(allowedTools(SKILL).includes('Task'), 'source skill must allow the Task tool')
  assert.ok(allowedTools(CODEX).includes('Task'), 'codex mirror must allow the Task tool')
})

// Split SKILL.md into its `## ` phase sections and return the one whose heading starts
// with `prefix` (e.g. "Phase 1.5:", "Phase 2:", "Phase 4:"). The colon matters: bare
// "Phase 1.5" would also match the "Phase 1.5b" stub-filing section.
function phaseSection(skill, prefix) {
  return skill.split(/\n## /).find((section) => section.startsWith(prefix))
}

// This used to be `countOf(SKILL, '<!-- tier: opus -->') >= 2` (plus a `>= 3` sibling below).
// Both were whole-file count thresholds over five unrelated occurrences, so routing the two
// mechanical legs while leaving the overview bullet and red-flags row asserting the opposite
// would have kept them GREEN on a self-contradictory skill. They are now per-section
// structural assertions in the bs-sweep-debt-skill.test.mjs shape.
test('the two mechanical dispatches carry a provider-guarded sonnet directive (both mirrors)', () => {
  for (const [label, skill] of [
    ['source', SKILL],
    ['codex mirror', CODEX],
  ]) {
    for (const [phase, what] of [
      ['Phase 1.5:', 'gosec probe'],
      ['Phase 2:', 'select-batch triage'],
    ]) {
      const section = phaseSection(skill, phase)
      assert.ok(section, `${label} must have a ${phase} section`)
      // Adjacent, not merely both present: the section also carries a `<!-- tier: sonnet -->`
      // annotation and prose about dropping "the `model:` line", so two independent matches
      // stay green on a section whose directive was deleted.
      assert.match(
        section,
        /model:\s*"?sonnet\b/,
        `${label} ${what} must carry a "model: sonnet" directive, not just the tier annotation`,
      )
      assert.match(
        section,
        /\$BOSS_AGENT/,
        `${label} ${what} directive must be $BOSS_AGENT-guarded`,
      )
      // Adjacent to `$BOSS_AGENT`, not merely present: Phase 1.5 is ~250 lines and quotes
      // `~/.claude/skills/…` paths and the `codex-skills` mirror in unrelated prose, so a
      // section-wide pair stays green even with the per-agent naming deleted from the guard.
      assert.match(
        section,
        /\$BOSS_AGENT[\s\S]{0,24}\bclaude\b/,
        `${label} ${what} guard must reference lowercase claude next to $BOSS_AGENT`,
      )
      assert.match(
        section,
        /\$BOSS_AGENT[\s\S]{0,24}\bcodex\b/,
        `${label} ${what} guard must cover the codex-omit case next to $BOSS_AGENT`,
      )
    }

    // Phase 4's boss-repair WATCH is a judgment step and stays on the orchestrator's Opus.
    const watch = phaseSection(skill, 'Phase 4:')
    assert.ok(watch, `${label} must have a Phase 4 repair-watch section`)
    assert.ok(
      watch.includes('<!-- tier: opus -->'),
      `${label} Phase 4 repair-watch must keep its tier: opus annotation`,
    )
    assert.ok(
      !watch.includes('sonnet'),
      `${label} Phase 4 repair-watch (Opus) must not carry a sonnet directive`,
    )
    // Exactly one annotation survives — the repair-watch one. Any other is a stale claim
    // about a leg that is now routed.
    assert.equal(
      countOf(skill, '<!-- tier: opus -->'),
      1,
      `${label} must keep exactly one tier: opus annotation (the repair-watch dispatch)`,
    )
  }
})

test('dispatches are awaited, never backgrounded', () => {
  assert.ok(SKILL.includes('run_in_background'), 'the never-background rule must be stated')
  assert.match(
    SKILL,
    /never.{0,40}run_in_background|run_in_background.{0,40}subagent/i,
    'must forbid run_in_background for the subagent dispatches',
  )
  assert.ok(SKILL.includes('await'), 'dispatches must say they are awaited')
})

// ---------------------------------------------------------------------------
// Run-file sentinel — the orchestrator classifies FROM THE FILE ONLY.
// ---------------------------------------------------------------------------

test('the SKILL documents every REPAIR_RESULT token from the shared helper', () => {
  for (const token of REPAIR_RESULTS) {
    assert.ok(SKILL.includes(token), `SKILL.md must document the REPAIR_RESULT token "${token}"`)
  }
})

test('the SKILL uses the byte-identical DISPATCH_FAILURE token', () => {
  // Sourced from skills-toolbox/bs-run-sentinel.mjs so the matcher fn stays the single source.
  assert.equal(DISPATCH_FAILURE, 'dispatch-failure')
  assert.ok(SKILL.includes(DISPATCH_FAILURE), 'SKILL.md must use the dispatch-failure token')
  // The shell constant must be pinned byte-identical to the module constant.
  assert.ok(
    SKILL.includes(`DISPATCH_FAILURE="${DISPATCH_FAILURE}"`),
    'the shell DISPATCH_FAILURE must equal the module constant',
  )
})

test('repair is classified from the run-file sentinel, never from returned prose', () => {
  assert.match(SKILL, /run-file sentinel (only|only\b)|FROM THE (RUN )?FILE ONLY/i)
  assert.ok(SKILL.includes('bs-run-sentinel.mjs'), 'must resolve the sentinel helper')
})

test('a missing/stale sentinel routes to the safe non-green branch, never green', () => {
  // The dead-subagent branch: a missing sentinel is a distinct dispatch-failure that is
  // never recorded as green.
  assert.match(SKILL, /missing|stale/i)
  assert.match(SKILL, /safe non-green/i, 'must route dispatch-failure to the safe non-green branch')
  assert.match(SKILL, /never green|never .{0,20}green/i, 'must state it is never recorded green')
})

test('green is re-verified by a cheap gh call, never trusted from the sentinel alone', () => {
  assert.match(SKILL, /re-verif|re-check|re-verify/i)
  assert.ok(SKILL.includes('MERGEABLE'), 'green re-verify checks a settled MERGEABLE state')
  assert.ok(SKILL.includes('statusCheckRollup'), 'green re-verify checks PR status checks')
  assert.ok(SKILL.includes('CHECKS_OK'), 'green re-verify requires successful checks')
})

test('triage subagent output is validated before jq field parsing', () => {
  assert.ok(
    SKILL.includes('invalid gate select-batch output'),
    'invalid triage output must block loudly',
  )
  assert.match(SKILL, /has\("manifest"\).*has\("ecosystem"\).*has\("batch"\)/s)
  assert.match(SKILL, /\.batch \| type == "array"/)
})

test('classify CLI outcome is decoded before shell branching', () => {
  assert.ok(
    SKILL.includes(
      'OUTCOME="$(node "$GATE" classify "$REPAIR_RESULT" "$MERGEABLE" "$REVIEW_DECISION" | jq -r \'.\')"',
    ),
    'classify emits a JSON string, so the shell must decode it before case handling',
  )
})

// ---------------------------------------------------------------------------
// Byte-identical external contracts (unchanged from baseline).
// ---------------------------------------------------------------------------

test('the --dry-run contract is byte-identical: zero GitHub writes, clean tree', () => {
  assert.ok(SKILL.includes('--dry-run'), 'dry-run flag documented')
  assert.match(SKILL, /zero.{0,40}GitHub writes/i, 'dry-run makes zero GitHub writes')
  assert.ok(SKILL.includes('git status --porcelain'), 'clean-tree contract documented')
})

test('the PR sentinels and sticky markers stay byte-identical', () => {
  assert.ok(SKILL.includes('bs-sweep-security:ghsa:<id>'), 'ghsa PR sentinel documented')
  assert.ok(SKILL.includes('<!-- bs-sweep-security:state -->'), 'state sticky marker documented')
})

test('untagged commits after the injector take the BLOCKED branch (both mirrors)', () => {
  // The `|| echo "note: …(continuing)"` on the add-pr-numbers.sh call absorbs benign setup
  // failures, so on its own it would also swallow a real "commits left untagged" failure.
  // The post-condition guard below it is what makes the sweep stop. It MUST be an
  // `if … then … fi` (a trailing `&& { …; }` compound returns non-zero and aborts the block
  // under `set -e`) and MUST use `grep -qv`, which succeeds when ANY line lacks the tag.
  for (const [label, skill] of [
    ['source', SKILL],
    ['codex mirror', CODEX],
  ]) {
    const commitsLine =
      'COMMITS="$(git log "origin/$BASE_BRANCH".."refs/heads/$SESSION_BRANCH" --oneline)"'
    const untaggedLine = String.raw`UNTAGGED="$(printf '%s' "$COMMITS" | grep -v "\[#$PR_NUMBER\]" || true)"`
    // Against the BRANCH REF, not HEAD — HEAD is detached mid-rebase, so a HEAD-scoped
    // range would be empty on a stranded rebase and the guard would pass on nothing.
    assert.ok(
      skill.includes(commitsLine) && skill.includes(untaggedLine),
      `${label} must fail closed when the branch range is unreadable`,
    )
    assert.doesNotMatch(
      skill,
      /git log "origin\/\$BASE_BRANCH"\.\.HEAD --oneline \| grep -qv/,
      `${label} must not scan HEAD, which is detached during a stranded rebase`,
    )
    assert.doesNotMatch(
      skill,
      /if git log "origin\/\$BASE_BRANCH"\.\."refs\/heads\/\$SESSION_BRANCH" --oneline \| grep -qv/,
      `${label} must not hide git log failures inside an if-condition pipeline`,
    )
    assert.match(
      skill,
      /if \[ -n "\$UNTAGGED" \]; then[\s\S]{0,80}?BLOCKED: commits remain untagged[\s\S]{0,60}?teardown[\s\S]{0,40}?exit 1/,
      `${label} must take the BLOCKED teardown branch when commits remain untagged`,
    )
    // The injector rewrites history, so the guard only proves a LOCAL property unless the
    // branch is re-pushed. Without this the PR still carries the untagged commits.
    assert.match(
      skill,
      /BLOCKED: commits remain untagged[\s\S]{0,400}?git push --force-with-lease origin "\$SESSION_BRANCH"[\s\S]{0,40}?BLOCKED: failed to push the tagged/,
      `${label} must force-push the rewritten branch after the untagged guard passes`,
    )
    assert.match(
      skill,
      /git push --force-with-lease origin "\$SESSION_BRANCH"[\s\S]{0,800}?CURRENT_SHA="\$\(git rev-parse "refs\/heads\/\$SESSION_BRANCH"\)"/,
      `${label} must persist the tagged branch SHA after the history-rewriting push`,
    )
    assert.match(
      skill,
      /CURRENT_SHA="\$\(git rev-parse "refs\/heads\/\$SESSION_BRANCH"\)"[\s\S]{0,160}?DECISION="\$\(node "\$GATE" decide-action "\$STATE_FILE" "\$CURRENT_SHA" 3\)"[\s\S]{0,160}?PRIOR_ATTEMPTS="\$\(printf '%s' "\$DECISION" \| jq -r '\.priorAttempts'\)"/,
      `${label} must reset the retry decision from the tagged branch SHA`,
    )
    // …and must BLOCK if that push fails. A softened `|| true` would restore the exact bug
    // the guard exists to prevent: a local verdict of "all tagged" over an untagged remote.
    assert.doesNotMatch(
      skill,
      /git push --force-with-lease origin "\$SESSION_BRANCH"\s*\|\|\s*true/,
      `${label} must not soften the force-push failure to a no-op`,
    )
  }
})

test('the 3-attempt poison-pill budget is preserved', () => {
  assert.match(SKILL, /\b3\b.{0,40}no-progress|3 no-progress|at most \*\*3\*\*/i)
})

// ---------------------------------------------------------------------------
// Codex mirror — must carry the same dispatch tokens (no un-synced drift).
// ---------------------------------------------------------------------------

test('the .codex mirror carries the same dispatch + sentinel tokens', () => {
  assert.ok(
    countOf(CODEX, 'subagent_type: general-purpose') >= 2,
    'codex mirror must carry both dispatch directives',
  )
  assert.ok(CODEX.includes(DISPATCH_FAILURE), 'codex mirror must carry the dispatch-failure token')
  for (const token of REPAIR_RESULTS) {
    assert.ok(CODEX.includes(token), `codex mirror must document REPAIR_RESULT token "${token}"`)
  }
})

// ---------------------------------------------------------------------------
// Triage behavior-parity — the gate selects a known batch on a NON-EMPTY set.
// The subagent-in-the-loop path runs exactly this gate CLI, so pinning the CLI
// selection proves the triage behavior did not change.
// ---------------------------------------------------------------------------

const gatePath = fileURLToPath(new URL('./sweep-security-gate.mjs', import.meta.url))
const alertsFixture = fileURLToPath(
  new URL('./fixtures/bs-sweep-security/alerts.sample.json', import.meta.url),
)
const prsFixture = fileURLToPath(
  new URL('./fixtures/bs-sweep-security/prs.sample.json', import.meta.url),
)

// ---------------------------------------------------------------------------
// BOS-491 — the read-only main-gate health probe (Phase 1.5) additive contract.
// ---------------------------------------------------------------------------

test('the main-gate health probe (Phase 1.5) is documented', () => {
  assert.match(SKILL, /Main-gate health probe/i, 'Phase 1.5 probe section must be present')
  assert.ok(
    SKILL.includes('scripts/sweep-maingate-gate.mjs'),
    'must resolve the pure-core gate helper',
  )
  assert.ok(SKILL.includes('selectStub'), 'must name the pure-core decision function')
  assert.ok(SKILL.includes('nosec-metadata-check.sh'), 'must run the nosec-metadata check inline')
})

test('the probe references the line-anchored MainGate: marker scheme', () => {
  assert.ok(SKILL.includes('MainGate:'), 'SKILL.md must reference the MainGate: marker')
  assert.match(SKILL, /\^MainGate: <id>\$/, 'must document the line-anchored marker rule')
})

test('the gosec probe adds a third awaited general-purpose dispatch', () => {
  // Phase 2 triage + Phase 4 repair-watch + Phase 1.5 gosec = 3 dispatches.
  assert.ok(
    countOf(SKILL, 'subagent_type: general-purpose') >= 3,
    'expected >= 3 `subagent_type: general-purpose` dispatch directives (adds the gosec probe)',
  )
  // The gosec probe's tier is asserted structurally, per section, above — it is a mechanical
  // leg (fixed CI flags in, one summary line out) and now routes to sonnet.
  const probe = phaseSection(SKILL, 'Phase 1.5:')
  assert.ok(
    probe.includes('subagent_type: general-purpose'),
    'the gosec probe dispatch must live in the Phase 1.5 section',
  )
})

test('the probe uses the exact CI gosec flags from security.yml', () => {
  for (const flag of [
    '-conf=.gosec.json',
    '-severity=medium',
    '-confidence=medium',
    '-verbose=json',
    "-exclude-dir='(^|/)gen(/|$)'",
    "-exclude-dir='(^|/)genproto(/|$)'",
    '-exclude-dir=testdata',
    '-exclude-dir=node_modules',
    'gosec@v2.22.5',
  ]) {
    assert.ok(SKILL.includes(flag), `probe must pass the exact CI flag/token ${flag}`)
  }
})

test('the probe files at most one gated, deduped Linear stub', () => {
  assert.match(SKILL, /would file stub/i, 'dry-run must print the would-file preview')
  assert.ok(SKILL.includes('main-gate:'), 'a main-gate: report line must be present')
  assert.match(SKILL, /origin\/staging/, 'gosec scope is changed modules vs origin/staging')
  assert.match(
    SKILL,
    /all.{0,6}go.work modules|ALL go.work modules/i,
    'base-ref-absent fallback documented',
  )
})

test('the .codex mirror carries the main-gate probe tokens (parity)', () => {
  assert.match(CODEX, /Main-gate health probe/i, 'codex mirror must carry the probe section')
  assert.ok(CODEX.includes('MainGate:'), 'codex mirror must carry the MainGate: marker token')
  assert.ok(
    CODEX.includes('scripts/sweep-maingate-gate.mjs') || CODEX.includes('sweep-maingate-gate.mjs'),
    'codex mirror must reference the pure-core gate helper',
  )
  assert.ok(
    countOf(CODEX, 'subagent_type: general-purpose') >= 3,
    'codex mirror must carry all three dispatch directives',
  )
})

test('triage parity: gate select-batch picks the expected non-empty batch on fixtures', () => {
  const res = spawnSync(
    process.execPath,
    [gatePath, 'select-batch', alertsFixture, prsFixture, '10'],
    {
      encoding: 'utf8',
    },
  )
  assert.equal(res.status, 0, res.stderr)
  const sel = JSON.parse(res.stdout)

  assert.equal(sel.manifest, 'pnpm-lock.yaml')
  assert.equal(sel.ecosystem, 'npm')

  // Batch is non-empty and ranked by score desc (critical axios before high lodash).
  assert.deepEqual(
    sel.batch.map((a) => a.number),
    [102, 101],
  )
  assert.deepEqual(
    sel.batch.map((a) => a.security_advisory.ghsa_id),
    ['GHSA-axios-0002', 'GHSA-lodash-0001'],
  )

  // Deferred covers the major-version bump; dropped covers the no-patch alert.
  assert.deepEqual(sel.deferred, [
    { number: 103, ghsa: 'GHSA-next-0003', reason: 'major version bump' },
  ])
  assert.deepEqual(sel.dropped, [
    { number: 104, ghsa: 'GHSA-request-0004', reason: 'no patched version' },
  ])
})
