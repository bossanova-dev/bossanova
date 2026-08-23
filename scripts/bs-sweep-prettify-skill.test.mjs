// Content/contract + gate-behavior test for the bs-sweep-prettify skill (BOS-372).
//
// Follows the BOS-144 content-test pattern (scripts/bs-<skill>-skill.test.mjs,
// wired into `make test-smoke` via the scripts/bs-*-skill.test.mjs glob). It pins
// the byte-stable external contracts the SKILL.md documents — the two terminal
// states, the tagless-commit + PR-number-injection finalize, the formatting-only
// guard, the Stop-hook removal, and the cron-gate path — plus a behavior-parity
// check that the gate's pure drift-detection core (scripts/sweep-prettify-gate.mjs)
// runs the sweep exactly when a formatter reports drift and fails closed when a
// required formatter cannot be spawned.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import {
  detectDrift,
  parseGoFiles,
  chunk,
  GO_BATCH,
  BIOME_FORMAT_DRIFT_MARKER,
  prettierReportedDrift,
  syncpackReportedDrift,
  SYNCPACK_DRIFT_MARKER,
} from './sweep-prettify-gate.mjs'
import { hasOpenCronPR } from './cron-open-pr.mjs'
import { rewriteClaudeSkillMarkdown } from './sync-codex-skills.mjs'
import {
  NO_REFERENCE_REMEDY,
  assertExactSize,
  assertMirrorRegenerated,
  measureFile,
} from './size-ratchet-lib.mjs'

const here = (rel) => new URL(rel, import.meta.url)
const abs = (rel) => fileURLToPath(here(rel))
const readSkill = (rel) => readFileSync(here(rel), 'utf8')
const SKILL = readSkill('../.claude/skills/bs-sweep-prettify/SKILL.md')
const CODEX = readSkill('../.codex/skills/bs-sweep-prettify/SKILL.md')
const GATE = readSkill('../.claude/skills/bs-sweep-prettify/gate/gate.mjs')
const CODEX_GATE = readSkill('../.codex/skills/bs-sweep-prettify/gate/gate.mjs')
const NOTES_TEARDOWN =
  "Before exiting, follow `bs-record-notes` with this run's outcome. Recording is non-fatal: never change the terminal state, exit code, or `git status --porcelain`. Skip gated/no-op runs that observed nothing."

// Live cron name + legacy slug the gate matches to suppress duplicate sweep PRs.
const GATE_CRON_NAMES = ['Bossanova sweep prettify', 'bs-sweep-prettify']

function allowedTools(skill) {
  const match = skill.match(/^allowed-tools:\s*(.+)$/m)
  assert.ok(match, 'skill frontmatter must declare allowed-tools')
  return match[1].split(',').map((tool) => tool.trim())
}

test('documents the non-fatal notes teardown contract', () => {
  for (const [label, skill] of [
    ['source', SKILL],
    ['codex mirror', CODEX],
  ]) {
    assert.ok(skill.includes(NOTES_TEARDOWN), `${label} must include the notes teardown contract`)
  }
})

// ---------------------------------------------------------------------------
// Skill frontmatter + terminal-state contract.
// ---------------------------------------------------------------------------

test('frontmatter names the skill and declares the mechanical tool set', () => {
  assert.match(SKILL, /^name:\s*bs-sweep-prettify$/m, 'name must be bs-sweep-prettify')
  const tools = allowedTools(SKILL)
  for (const t of ['Bash', 'Read', 'Write']) {
    assert.ok(tools.includes(t), `source skill must allow ${t}`)
  }
  // Mechanical sweep: no judgment subagents, so no Task dispatch is required.
  assert.ok(!SKILL.includes('run_in_background'), 'a mechanical sweep dispatches no subagents')
})

test('exactly two terminal states are documented', () => {
  assert.ok(SKILL.includes('READY_GREEN_PR'), 'READY_GREEN_PR terminal state documented')
  assert.ok(SKILL.includes('NO_CHANGE'), 'NO_CHANGE terminal state documented')
})

test('headless / cron-safe: no questions', () => {
  assert.match(SKILL, /Do\s+not\s+ask\s+the\s+user\s+questions/i, 'must forbid asking questions')
  assert.ok(SKILL.includes('BOSS_CRON'), 'must reference the headless cron env')
})

// ---------------------------------------------------------------------------
// Formatting-only + self-owned finalize contract.
// ---------------------------------------------------------------------------

test('the sweep is formatting-only and never hand-edits', () => {
  assert.match(SKILL, /formatting-only/i, 'must state the diff is formatting-only')
  assert.match(SKILL, /never\s+hand-edit/i, 'must forbid hand-editing files')
})

test('commits are tagless with a style scope; PR number injected at finalize', () => {
  assert.ok(SKILL.includes('style(repo):'), 'documents the tagless style(scope) commit')
  assert.match(SKILL, /tagless/i, 'must state commits are tagless')
  // BOS-640: the `add-pr-numbers.sh` call and the `--force-with-lease` push moved into
  // skills-toolbox/sweep-pr-gate.sh, whose bytes scripts/sweep-pr-gate.test.mjs pins.
  // Injection is now proved by a CHAIN: this body EXECUTES the gate, and the gate's own bytes
  // carry both steps. The bare `add-pr-numbers.sh` mention still in this body is narrative
  // (the rewritten-head CI-rewatch sentence), so it is asserted for what it actually proves
  // rather than being labelled as proof of injection.
  assert.ok(
    SKILL.includes('bash "$(git rev-parse --show-toplevel)/skills-toolbox/sweep-pr-gate.sh")"'),
    'must execute the PR gate — executing it is what injects [#N] and force-pushes with lease',
  )
  assert.ok(
    SKILL.includes('add-pr-numbers.sh'),
    'must keep the rewritten-head CI-rewatch rationale, which names add-pr-numbers.sh',
  )
  const GATE_SRC = readSkill('../skills-toolbox/sweep-pr-gate.sh')
  assert.ok(
    GATE_SRC.includes('--force-with-lease') && GATE_SRC.includes('add-pr-numbers.sh'),
    'the extracted PR gate still injects [#N] and force-pushes with lease',
  )
})

test('self-owned finalize removes bossd Stop-hooks before stopping', () => {
  assert.ok(
    SKILL.includes('skills-toolbox/remove-bossd-stop-hooks.mjs'),
    'must remove bossd Stop-hooks so bossd cannot double-finalize',
  )
  // BOS-640: `gh pr ready` moved into the extracted gate; the skill still owns readying the
  // PR by invoking it, and scripts/sweep-pr-gate.test.mjs pins the moved draft -> ready block.
  assert.ok(
    SKILL.includes('bash "$(git rev-parse --show-toplevel)/skills-toolbox/sweep-pr-gate.sh")"'),
    'this skill owns readying the PR, by executing the extracted PR gate',
  )
  assert.ok(
    readSkill('../skills-toolbox/sweep-pr-gate.sh').includes('gh pr ready "$PR_NUMBER" >&2'),
    'the extracted PR gate still flips the PR out of draft',
  )
})

test('mirror gate is documented for any edit to this skill', () => {
  assert.ok(SKILL.includes('make codex-skills'), 'must document the codex-skills mirror gate')
})

// bossd writes these into Claude cron worktrees (plugins/bossd-plugin-claude/
// dirty_files.go). They are untracked, so `git diff` never sees them, but the
// NO_CHANGE porcelain gate and Phase 4 `git add -A` would — so both must exclude
// them, or a managed lock alone opens a lock-only PR / gets committed.
test('NO_CHANGE gate and staging exclude bossd-managed dirty files', () => {
  for (const p of ['.claude/settings.local.json', '.claude/scheduled_tasks.lock']) {
    assert.ok(SKILL.includes(p), `SKILL must account for managed path ${p}`)
  }
  assert.match(SKILL, /grep -Ev/, 'NO_CHANGE gate must filter managed paths from porcelain')
  assert.match(SKILL, /git[ ]reset -q --/, 'staging must unstage managed files after git add -A')
})

test('Phase 0 switches off the default branch before deriving BASE_BRANCH', () => {
  const startIndex = SKILL.indexOf('START_SHA="$(git rev-parse HEAD)"')
  const defaultBranchIndex = SKILL.indexOf(
    'DEFAULT_BRANCH="$(gh repo view --json defaultBranchRef -q .defaultBranchRef.name)" || exit 1',
  )
  const switchIndex = SKILL.indexOf('git switch -c "$SESSION_BRANCH" || exit 1')
  const baseBranchIndex = SKILL.indexOf('BASE_BRANCH="$(\n  git rev-parse --abbrev-ref')
  const pushIndex = SKILL.indexOf('git push -u origin "$SESSION_BRANCH"')

  assert.ok(startIndex !== -1, 'Phase 0 captures START_SHA')
  assert.ok(defaultBranchIndex !== -1, 'Phase 0 resolves the repo default branch')
  assert.ok(switchIndex !== -1, 'Phase 0 creates the sweep branch')
  assert.ok(baseBranchIndex !== -1, 'Phase 0 derives BASE_BRANCH')
  assert.ok(pushIndex !== -1, 'Phase 4 pushes SESSION_BRANCH')

  assert.ok(startIndex < defaultBranchIndex, 'START_SHA is captured before the branch step')
  assert.ok(defaultBranchIndex < switchIndex, 'default branch is resolved before branch creation')
  assert.ok(switchIndex < baseBranchIndex, 'branch creation precedes BASE_BRANCH derivation')
  assert.ok(baseBranchIndex < pushIndex, 'BASE_BRANCH derivation precedes the Phase 4 push')
})

test('Phase 0 branch step is default-branch-only and collision-safe', () => {
  assert.ok(
    SKILL.includes('if [ "$SESSION_BRANCH" = "$DEFAULT_BRANCH" ]; then'),
    'branch creation must be conditional on the current branch being the default',
  )
  assert.ok(
    SKILL.includes('SESSION_BRANCH="bs-sweep-prettify/$(date -u +%Y%m%dT%H%M%SZ)"'),
    'created branch must carry the bs-sweep-prettify/ prefix',
  )
  assert.ok(
    SKILL.includes('git switch -c "$SESSION_BRANCH" || exit 1'),
    'branch creation failure, including a collision, must stop the run',
  )
  assert.ok(
    SKILL.includes(
      'DEFAULT_BRANCH="$(gh repo view --json defaultBranchRef -q .defaultBranchRef.name)" || exit 1',
    ),
    'default branch resolution failure must stop the run',
  )
  assert.ok(
    !SKILL.includes('git switch "$SESSION_BRANCH"'),
    'the branch step must not adopt an existing branch',
  )
})

// ---------------------------------------------------------------------------
// Cron gate contract.
// ---------------------------------------------------------------------------

test('SKILL documents the sibling-convention gate path', () => {
  assert.ok(
    SKILL.includes('node .claude/skills/bs-sweep-prettify/gate/gate.mjs'),
    'must document the gate GateCommand path',
  )
})

test('the thin gate reuses the pure core and the shared gateExit', () => {
  assert.ok(
    GATE.includes("from '../../../../scripts/sweep-prettify-gate.mjs'"),
    'imports detectDrift',
  )
  assert.ok(
    GATE.includes("from '../../../../skills-toolbox/linear-gate-lib.mjs'"),
    'imports gateExit',
  )
  assert.match(GATE, /fail-closed/i, 'gate documents its fail-closed contract')
})

test('gate delegates Go-file enumeration to the pure parseGoFiles core', () => {
  assert.ok(GATE.includes('parseGoFiles'), 'gate uses the shared, behavior-tested parseGoFiles')
})

// The gate must also suppress a duplicate in-flight sweep PR (like the sibling
// bs-sweep-* gates): while a prettify sweep PR is open, its drift is still unmerged
// on base, so probing drift would only re-open an identical PR. Assert both mirrors
// wire the shared cron-open-pr helper and carry the CRON_NAMES pair.
test('both gate mirrors suppress duplicate in-flight sweep PRs via cron-open-pr', () => {
  for (const [label, gate] of [
    ['.claude', GATE],
    ['.codex', CODEX_GATE],
  ]) {
    assert.match(
      gate,
      /cron-open-pr\.mjs/,
      `${label} gate must import the shared cron-open-pr helper`,
    )
    assert.match(gate, /hasOpenCronPR/, `${label} gate must call hasOpenCronPR`)
    assert.match(
      gate,
      /Bossanova[ ]sweep[ ]prettify/,
      `${label} gate must match the live cron name`,
    )
    assert.match(gate, /'bs-sweep-prettify'/, `${label} gate must carry the legacy cron name`)
    assert.match(
      gate,
      /bs-sweep-prettify[ ]gate: prior[ ]sweep[ ]PR[ ]still[ ]open/,
      `${label} skip reason must be loud`,
    )
  }
})

test('the gate CRON_NAMES pair drives hasOpenCronPR suppression', () => {
  assert.match(GATE, /CRON_NAMES = \['Bossanova[ ]sweep[ ]prettify', 'bs-sweep-prettify'\]/)
  assert.equal(
    hasOpenCronPR([{ headRefName: 'cron-bossanova-sweep-prettify-1780000000' }], GATE_CRON_NAMES),
    true,
  )
  assert.equal(
    hasOpenCronPR([{ headRefName: 'cron-bs-sweep-prettify-1780000000' }], GATE_CRON_NAMES),
    true,
  )
  assert.equal(hasOpenCronPR([], GATE_CRON_NAMES), false)
  assert.equal(
    hasOpenCronPR([{ headRefName: 'cron-bossanova-sweep-tests-1780000000' }], GATE_CRON_NAMES),
    false,
    'must not match a different sweep cron',
  )
})

test('the gate is node-builtins only (no bare third-party imports)', () => {
  const imports = [...GATE.matchAll(/^import\s+.*?from\s+'([^']+)'/gm)].map((m) => m[1])
  for (const spec of imports) {
    const ok = spec.startsWith('node:') || spec.startsWith('.') || spec.startsWith('/')
    assert.ok(ok, `gate import "${spec}" must be a node: builtin or a relative path`)
  }
})

// parseGoFiles is the fail-closed Go-file enumeration, tested by BEHAVIOR (not a
// source grep) so a rename or a dropped guard is caught. Both failure shapes of a
// broken `git ls-files` must throw; a clean result parses to a trimmed list.
test('parseGoFiles: ENOENT spawn error throws (fail closed)', () => {
  assert.throws(() => parseGoFiles({ error: new Error('no git') }), /git[ ]ls-files[ ]failed/)
})

test('parseGoFiles: non-zero exit throws (fail closed), not an empty list', () => {
  assert.throws(
    () => parseGoFiles({ status: 128, stdout: '' }),
    /git[ ]ls-files[ ]failed: exit[ ]128/,
  )
})

test('parseGoFiles: clean result parses to a trimmed, blank-filtered list', () => {
  assert.deepEqual(parseGoFiles({ status: 0, stdout: 'a.go\n b.go \n\n' }), ['a.go', 'b.go'])
})

// ---------------------------------------------------------------------------
// Codex mirror — must carry the same terminal + gate tokens (no un-synced drift).
// ---------------------------------------------------------------------------

test('the .codex mirror carries the same terminal + gate tokens', () => {
  // sync-codex-skills rewrites `.claude/skills/` paths to `.codex/skills/` in the mirror.
  for (const token of [
    'READY_GREEN_PR',
    'NO_CHANGE',
    'node .codex/skills/bs-sweep-prettify/gate/gate.mjs',
    'add-pr-numbers.sh',
    // BOS-640: no COMMON_REWRITES rule matches a bare repo-root `skills-toolbox/` path, so the
    // mirror must carry the PR-gate invocation byte-identically.
    'skills-toolbox/sweep-pr-gate.sh',
  ]) {
    assert.ok(CODEX.includes(token), `codex mirror must carry the token "${token}"`)
  }
})

// ---------------------------------------------------------------------------
// Gate behavior-parity — detectDrift runs the sweep exactly when a formatter
// reports drift, and fails closed when a required formatter cannot be spawned.
// A fake runner keyed by command stands in for the real execFileSync probes.
// ---------------------------------------------------------------------------

/** Build a fake `run(cmd, args)` from a { cmd: result } map (result may be a fn). */
function fakeRunner(map) {
  return (cmd, args) => {
    const key = `${cmd} ${(args ?? []).join(' ')}`
    const r = map[key] ?? map[cmd]
    return typeof r === 'function' ? r(args) : r
  }
}

const SYNCPACK_LINT_KEY = 'pnpm syncpack lint'
const SYNCPACK_FORMAT_KEY = 'pnpm syncpack format --check'
const SCRIPTS_PRETTIER_KEY =
  'pnpm exec prettier --check scripts/*.{cjs,mjs} scripts/bazel/*.mjs scripts/changelog/*.{cjs,mjs} scripts/skill-parity/*.{cjs,mjs} skills-toolbox/*.mjs skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.{cjs,mjs} .claude/skills/*/gate/*.mjs'
const DOCS_PRETTIER_KEY = 'pnpm --dir services/docs run lint'
const WEB_BIOME_KEY = 'pnpm --dir services/web run lint'
const ROOT_PRETTIER_KEY = 'pnpm run lint:docs'

const CLEAN = {
  make: { status: 0, stdout: '' }, // `make lint-check-version` prereq passes
  pnpm: { status: 0, stdout: '' },
  gofmt: { status: 0, stdout: '' },
}
const goFiles = ['a.go', 'b.go']

test('clean tree: no probe reports drift -> do not run', () => {
  const res = detectDrift(fakeRunner(CLEAN), { goFiles })
  assert.equal(res.drift, false)
  assert.deepEqual(res.reasons, [])
})

// `make lint-check-version` is a CONSERVATIVE environment probe, not a declared
// prerequisite of any format target (BOS-653 corrected that claim wherever it appeared,
// including this throw's own wording; the PROBE is what stays deliberately unchanged). A
// host that cannot satisfy the pinned toolchain is one whose formatter output we will not
// trust, so fail CLOSED — the failure direction is a skipped sweep, never a bad one.
test('lint-check-version environment probe failure fails closed (throws), not drift', () => {
  assert.throws(
    () =>
      detectDrift(
        fakeRunner({ ...CLEAN, make: { status: 1, stdout: 'golangci-lint not installed' } }),
        {
          goFiles,
        },
      ),
    /make[ ]lint-check-version[ ]environment[ ]probe[ ]failed \(exit[ ]1\)/,
  )
})

test('lint-check-version probe spawn error fails closed (throws)', () => {
  assert.throws(
    () => detectDrift(fakeRunner({ ...CLEAN, make: { error: new Error('no make') } }), { goFiles }),
    /make[ ]lint-check-version[ ]environment[ ]probe[ ]failed[ ]to[ ]run/,
  )
})

// Real prettier `--check` drift: exit 1 WITH its marker on stderr (pnpm forwards
// prettier's stderr). The marker is what proves this is drift, not a tooling failure.
const PRETTIER_DRIFT = {
  status: 1,
  stdout: 'Checking formatting...\n',
  stderr:
    '[warn] docs/x.md\n[warn] Code style issues found in the above file. Run Prettier with --write to fix.\n',
}
const SYNCPACK_DRIFT = {
  status: 1,
  stdout: '',
  stderr: `${SYNCPACK_DRIFT_MARKER} dependencies differ from syncpack policy\n`,
}
const BIOME_FORMAT_DRIFT = {
  status: 1,
  stdout: 'Checked 248 files in 208ms. No fixes applied.\nFound 1 error.\n',
  stderr: `example.ts format ━━━━━━━━━\n\n  × ${BIOME_FORMAT_DRIFT_MARKER} the following content:\n`,
}

test('syncpackReportedDrift matches syncpack findings marker only', () => {
  assert.equal(syncpackReportedDrift(SYNCPACK_DRIFT), true)
  assert.equal(syncpackReportedDrift({ status: 1, stderr: '✓ No issues found' }), false)
  assert.equal(syncpackReportedDrift({ status: 1, stderr: '✗ error: unexpected argument' }), false)
  assert.equal(syncpackReportedDrift({}), false)
})

for (const { name, key, label, reason, drift } of [
  {
    name: 'syncpack lint',
    key: SYNCPACK_LINT_KEY,
    label: 'syncpack lint',
    reason: 'syncpack',
    drift: SYNCPACK_DRIFT,
  },
  {
    name: 'syncpack format',
    key: SYNCPACK_FORMAT_KEY,
    label: 'syncpack format',
    reason: 'syncpack',
    drift: SYNCPACK_DRIFT,
  },
  {
    name: 'scripts prettier',
    key: SCRIPTS_PRETTIER_KEY,
    label: 'scripts prettier',
    reason: 'scripts-prettier',
    drift: PRETTIER_DRIFT,
  },
  {
    name: 'docs prettier',
    key: DOCS_PRETTIER_KEY,
    label: 'docs prettier',
    reason: 'docs-prettier',
    drift: PRETTIER_DRIFT,
  },
  {
    name: 'web biome',
    key: WEB_BIOME_KEY,
    label: 'web biome',
    reason: 'web-biome',
    drift: BIOME_FORMAT_DRIFT,
  },
]) {
  test(`${name} clean result contributes no drift reason`, () => {
    const res = detectDrift(fakeRunner({ ...CLEAN, [key]: { status: 0, stdout: '' } }), {
      goFiles: [],
    })
    assert.equal(res.drift, false)
    assert.ok(!res.reasons.includes(reason))
  })

  test(`${name} findings result reports drift with reason ${reason}`, () => {
    const res = detectDrift(fakeRunner({ ...CLEAN, [key]: drift }), { goFiles })
    assert.equal(res.drift, true)
    assert.deepEqual(res.reasons, [reason])
  })

  test(`${name} spawn error fails closed`, () => {
    assert.throws(
      () =>
        detectDrift(fakeRunner({ ...CLEAN, [key]: { error: new Error(`no ${name}`) } }), {
          goFiles,
        }),
      new RegExp(`${label.replaceAll(' ', '\\s+')}\\s+probe\\s+failed\\s+to\\s+run`),
    )
  })

  test(`${name} exit 1 without findings marker fails closed`, () => {
    assert.throws(
      () =>
        detectDrift(fakeRunner({ ...CLEAN, [key]: { status: 1, stderr: 'tool failed' } }), {
          goFiles,
        }),
      new RegExp(`${label.replaceAll(' ', '\\s+')}\\s+probe\\s+errored \\(exit\\s+1\\)`),
    )
  })

  for (const status of [2, 127]) {
    test(`${name} exit ${status} fails closed`, () => {
      assert.throws(
        () => detectDrift(fakeRunner({ ...CLEAN, [key]: { status, stderr: '' } }), { goFiles }),
        new RegExp(`${label.replaceAll(' ', '\\s+')}\\s+probe\\s+errored \\(exit\\s+${status}\\)`),
      )
    })
  }
}

test('prettier drift (exit 1 WITH its --check marker) -> run, reason prettier-docs', () => {
  const res = detectDrift(fakeRunner({ ...CLEAN, [ROOT_PRETTIER_KEY]: PRETTIER_DRIFT }), {
    goFiles,
  })
  assert.equal(res.drift, true)
  assert.ok(res.reasons.includes('prettier-docs'))
})

// A dependency-free worktree's `pnpm run lint:docs` can exit 1 because Corepack
// failed to fetch the pinned pnpm BEFORE prettier ran. That is a tooling failure,
// not drift: with no prettier `--check` marker the gate must fail CLOSED (throw ->
// skip) so the cron never wakes an agent on a problem `make format` can't fix.
test('pnpm/Corepack exit 1 without the drift marker fails closed (throws)', () => {
  const toolingFail = {
    status: 1,
    stdout: '',
    stderr:
      'Internal Error: Server answered with HTTP 404 while fetching pnpm\n ELIFECYCLE  Command failed.\n',
  }
  assert.throws(
    () => detectDrift(fakeRunner({ ...CLEAN, [ROOT_PRETTIER_KEY]: toolingFail }), { goFiles }),
    /prettier\s+probe\s+errored \(exit\s+1\)/,
  )
})

// prettierReportedDrift is the pure predicate that disambiguates exit 1; test it
// directly on both stream carriers and both non-drift shapes.
test('prettierReportedDrift matches the --check marker on either stream, else false', () => {
  assert.equal(prettierReportedDrift({ stderr: 'Code style issues found in 2 files.' }), true)
  assert.equal(
    prettierReportedDrift({ stdout: 'Code style issues found in the above file.' }),
    true,
  )
  assert.equal(prettierReportedDrift({ stdout: 'Checking formatting...', stderr: '' }), false)
  assert.equal(prettierReportedDrift({ stderr: 'ELIFECYCLE  Command failed.' }), false)
  assert.equal(prettierReportedDrift({}), false)
})

// prettier `--check` exits 1 for drift but 2 for a config/parse error and 127 for
// a missing binary (via pnpm). Only a marker-carrying exit 1 is drift; any other
// non-zero must fail CLOSED (throw -> skip), never be mistaken for drift and waste
// an agent run in a dependency-free worktree where prettier is absent.
test('prettier error exit (2) fails closed (throws), not counted as drift', () => {
  assert.throws(
    () =>
      detectDrift(fakeRunner({ ...CLEAN, [ROOT_PRETTIER_KEY]: { status: 2, stdout: '' } }), {
        goFiles,
      }),
    /prettier\s+probe\s+errored \(exit\s+2\)/,
  )
})

test('prettier missing-binary exit (127) fails closed (throws)', () => {
  assert.throws(
    () =>
      detectDrift(fakeRunner({ ...CLEAN, [ROOT_PRETTIER_KEY]: { status: 127, stdout: '' } }), {
        goFiles,
      }),
    /prettier\s+probe\s+errored \(exit\s+127\)/,
  )
})

test('gofmt drift (listed file) -> run, reason gofmt', () => {
  const res = detectDrift(fakeRunner({ ...CLEAN, gofmt: { status: 0, stdout: 'a.go\n' } }), {
    goFiles,
  })
  assert.equal(res.drift, true)
  assert.ok(res.reasons.includes('gofmt'))
})

test('detectDrift short-circuits after the first drift probe', () => {
  const calls = []
  const res = detectDrift(
    (cmd, args) => {
      const key = `${cmd} ${(args ?? []).join(' ')}`
      calls.push(key)
      if (cmd === 'make') return { status: 0, stdout: '' }
      if (key === SYNCPACK_LINT_KEY) return SYNCPACK_DRIFT
      throw new Error(`unexpected later probe ${key}`)
    },
    { goFiles },
  )
  assert.equal(res.drift, true)
  assert.deepEqual(res.reasons, ['syncpack'])
  assert.deepEqual(calls, ['make lint-check-version', SYNCPACK_LINT_KEY])
})

test('detectDrift uses check-mode commands only', () => {
  const calls = []
  detectDrift(
    (cmd, args = []) => {
      calls.push([cmd, args])
      return { status: 0, stdout: '' }
    },
    { goFiles },
  )

  for (const [cmd, args] of calls) {
    assert.ok(!args.includes('--write'), `${cmd} ${args.join(' ')} must not write`)
    assert.ok(!args.includes('-w'), `${cmd} ${args.join(' ')} must not write`)
  }

  const syncpackCalls = calls.filter(([cmd, args]) => cmd === 'pnpm' && args[0] === 'syncpack')
  assert.deepEqual(syncpackCalls, [
    ['pnpm', ['syncpack', 'lint']],
    ['pnpm', ['syncpack', 'format', '--check']],
  ])
})

// goimports is intentionally NOT probed: each module's `format` target runs plain
// `gofmt -w` and never `goimports -w`, so a goimports-only drift the sweep can't
// reconcile must not trigger a (wasted) run.
test('goimports-only drift does NOT run, and `go` is never probed', () => {
  const run = (cmd, args) => {
    if (cmd === 'make') return { status: 0, stdout: '' }
    if (cmd === 'pnpm') return { status: 0, stdout: '' }
    if (cmd === 'gofmt') return { status: 0, stdout: '' }
    throw new Error(`gate must not probe ${cmd} ${args?.join(' ')} — format-all has no goimports`)
  }
  const res = detectDrift(run, { goFiles })
  assert.equal(res.drift, false)
  assert.deepEqual(res.reasons, [])
})

test('required prettier probe spawn error fails closed (throws)', () => {
  assert.throws(
    () =>
      detectDrift(fakeRunner({ ...CLEAN, [ROOT_PRETTIER_KEY]: { error: new Error('no pnpm') } }), {
        goFiles,
      }),
    /prettier\s+probe\s+failed\s+to\s+run/,
  )
})

test('required gofmt probe spawn error fails closed (throws)', () => {
  assert.throws(
    () =>
      detectDrift(fakeRunner({ ...CLEAN, gofmt: { error: new Error('no gofmt') } }), { goFiles }),
    /gofmt\s+probe\s+failed\s+to\s+run/,
  )
})

// `gofmt -l` exits 0 and lists drift on stdout, so a non-zero exit is a genuine
// error (unreadable/unparseable file), not "no drift" — it must fail closed.
test('gofmt error exit (2) fails closed (throws), not read as clean', () => {
  assert.throws(
    () => detectDrift(fakeRunner({ ...CLEAN, gofmt: { status: 2, stdout: '' } }), { goFiles }),
    /gofmt\s+probe\s+errored \(exit\s+2\)/,
  )
})

test('no Go files: gofmt is not probed', () => {
  let calls = 0
  const run = (cmd) => {
    calls += 1
    if (cmd === 'make') return { status: 0, stdout: '' }
    if (cmd === 'pnpm') return { status: 0, stdout: '' }
    throw new Error(`unexpected probe of ${cmd} with no Go files`)
  }
  const res = detectDrift(run, { goFiles: [] })
  assert.equal(res.drift, false)
  assert.equal(
    calls,
    7,
    'only the make prereq + non-Go formatter probes run when there are no Go files',
  )
})

test('chunk batches Go files under GO_BATCH to stay within ARG_MAX', () => {
  const files = Array.from({ length: GO_BATCH * 2 + 5 }, (_, i) => `f${i}.go`)
  const batches = chunk(files)
  assert.equal(batches.length, 3)
  assert.ok(batches.every((b) => b.length <= GO_BATCH))
  assert.equal(batches.flat().length, files.length)
})

test('BOS-653: Phase 1 runs the WHOLE-REPO formatter target, not the changed-files one', () => {
  // BOS-371 made `make format` the AFFECTED/changed-files formatter and moved the whole-repo
  // run to `make format-all` (Makefile:1001 vs :1007). This sweep's entire purpose is the
  // whole-tree pass, so on a fresh cron branch — which has no changed files — `make format`
  // formatted nothing and Phase 2 emitted a false NO_CHANGE. The sweep was inert on that path.
  //
  // Pin the FENCED bytes, not a bare `make format-all` substring: the body names formatter
  // targets in resident prose too, so a bare-path pin stays vacuously satisfied even if the
  // fence itself regressed to `make format` (BOS-640's lesson, applied to Phase 1).
  const FENCE = 'make format-all >"$FORMAT_LOG" 2>&1 || {'
  for (const [label, skill] of [
    ['.claude/skills/bs-sweep-prettify', SKILL],
    ['.codex/skills/bs-sweep-prettify', CODEX],
  ]) {
    assert.ok(skill.includes(FENCE), `${label}/SKILL.md Phase 1 must run: ${FENCE}`)
    // `make format` not followed by `-` — i.e. the changed-files target, never `format-all`
    // and never `format-affected` (which the body may legitimately contrast against).
    const stale = skill.match(/make\s+format(?![-\w])/g) ?? []
    assert.deepEqual(
      stale,
      [],
      `${label}/SKILL.md must name no bare \`make format\` — BOS-371 made it the changed-files target`,
    )
    assert.ok(
      !skill.includes('If BOS-371 later introduces a dedicated whole-repo `format-all`'),
      `${label}/SKILL.md must drop the obsolete forward reference — the alias exists`,
    )
    assert.ok(
      skill.includes('`goimports -w`'),
      `${label}/SKILL.md must keep the goimports caveat — format-all does not apply it either`,
    )
  }
})

// ---------------------------------------------------------------------------
// BOS-640 — the extracted PR gate, and the ratchet that keeps its saving spent.
// ---------------------------------------------------------------------------

test('the extracted PR gate is referenced in both mirrors and exists on disk', () => {
  // No COMMON_REWRITES rule matches a bare repo-root `skills-toolbox/` path, so the mirror
  // carries the invocation byte-identically; asserting BOTH trees is what catches a mirror
  // that silently lost it.
  for (const [label, skill] of [
    ['.claude/skills/bs-sweep-prettify', SKILL],
    ['.codex/skills/bs-sweep-prettify', CODEX],
  ]) {
    // Pin the EXECUTED bytes, not the bare path — the body also NAMES the helper in its
    // resident "executed, not read" prose, so a bare-path substring survives deleting the
    // fenced invocation entirely. prettify had NO other fence-local pin, so before this the
    // whole invocation could be deleted with every test still green.
    assert.ok(
      skill.includes('bash "$(git rev-parse --show-toplevel)/skills-toolbox/sweep-pr-gate.sh")"'),
      `${label}/SKILL.md must execute skills-toolbox/sweep-pr-gate.sh`,
    )
    assert.ok(
      skill.includes('test -n "$PR_NUMBER"'),
      `${label}/SKILL.md must fail the block when the gate produced no PR number`,
    )
  }
  assert.ok(
    existsSync(here('../skills-toolbox/sweep-pr-gate.sh')),
    'skills-toolbox/sweep-pr-gate.sh must exist on disk',
  )
})

test('the resident body is pinned at its exact post-extraction size', () => {
  // Measured post-extraction bodies: 16761 B (.claude) / 16843 B (.codex), down from the
  // 17365 B (.claude) / 17446 B (.codex) pre-extraction baseline. The ceiling is the larger
  // mirror + 64 B, so the 22-line PR-gate saving is actually banked and cannot be silently
  // re-spent on body regrowth — move situational content into a reference instead.
  //
  // Bumped 16907 -> 17312 for the model-tier work: the Hard Rules now record the negative
  // result that this skill dispatches NO subagent, so it has no leg to route to a cheaper
  // tier and per-dispatch `effort:` is not settable from a skill here. That is a decision
  // record, not situational content — there is no reference to move it into, and without it
  // the question gets re-litigated every time someone audits tier coverage. Bodies are
  // 17174 B (.claude) / 17256 B (.codex), so the ceiling leaves 56 B of slack — nearly none.
  // It still sits below the pre-extraction baseline, so the extraction saving is not handed
  // back, but the NEXT addition here does not fit: move situational content into a reference
  // rather than bumping again.
  //
  // BOS-653 spent 21 B of that 56 without a bump: `START_SHA="$START_SHA" ` in the gate
  // invocation (+23) and `make format` -> `make format-all` at 8 sites (+32), funded by
  // deleting the obsolete "if a format-all alias later appears" paragraph and by correcting
  // two now-false prose claims (the golangci parenthetical, and the cron-gate sentence that
  // still said the sweep "could never reconcile" drift `format-all` in fact reconciles).
  // Bodies are 17195 B (.claude) / 17277 B (.codex) — 35 B of slack. Re-measure before editing.
  //
  // BOS-768 replaces the ceiling with an exact pin on the AUTHORED source. The "re-measure
  // before editing" instruction above is what a slack ceiling forces on every reader; an exact
  // pin does the measuring itself and reds with the number. The `.codex` copy leaves the byte
  // loop: it is GENERATED, and is verified below by regenerating it and comparing exactly.
  //
  // The remedy is NOT "move situational content into a reference": bs-sweep-prettify has no
  // references/ directory, only gate/, so that advice named a destination that does not exist.
  //
  // BOS-917 repins 17001 -> 17324 for the Phase 0 default-branch safety step. This is
  // resident because the executable preflight fence must create the sweep branch before
  // BASE_BRANCH derivation; moving it out would leave the unsafe push path documented here.
  //
  // BOS-916 repins 17324 -> 17191 while updating the cron-gate prose from a deliberate
  // sound-subset claim to the expanded check-mode formatter mirror. The change banks the
  // shorter wording alongside the functional gate probes.
  const SOURCE_BYTES = 17191 // exact measured .claude body, re-measured 2026-08-23
  assertExactSize({
    below: { name: 'PRE_EXTRACTION_BASELINE', value: 17365 },
    constFile: 'scripts/bs-sweep-prettify-skill.test.mjs',
    constName: 'SOURCE_BYTES',
    expected: SOURCE_BYTES,
    label: 'bs-sweep-prettify resident body',
    measured: measureFile(abs('../.claude/skills/bs-sweep-prettify/SKILL.md')),
    path: '.claude/skills/bs-sweep-prettify/SKILL.md',
    remedy: NO_REFERENCE_REMEDY,
    residual:
      'gate/gate.mjs and the toolbox scripts this body invokes — a line moved out of the body ' +
      'into one of those is invisible to this pin',
  })
})

test('the codex mirror is exactly what regenerating it from the .claude source produces', () => {
  // Replaces the old byte ceiling on the mirror. `make codex-skills` unconditionally prepends
  // a generated-by header, so a healthy mirror is ALWAYS larger than its source — "larger than
  // source" can never be the tell. Exact regeneration equality is, and it subsumes size.
  assertMirrorRegenerated({
    mirrorPath: abs('../.codex/skills/bs-sweep-prettify/SKILL.md'),
    regenerate: rewriteClaudeSkillMarkdown,
    sourcePath: abs('../.claude/skills/bs-sweep-prettify/SKILL.md'),
  })
})
