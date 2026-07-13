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
import { readFileSync } from 'node:fs'

import {
  detectDrift,
  parseGoFiles,
  chunk,
  GO_BATCH,
  prettierReportedDrift,
} from './sweep-prettify-gate.mjs'
import { hasOpenCronPR } from './cron-open-pr.mjs'

const readSkill = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const SKILL = readSkill('../.claude/skills/bs-sweep-prettify/SKILL.md')
const CODEX = readSkill('../.codex/skills/bs-sweep-prettify/SKILL.md')
const GATE = readSkill('../.claude/skills/bs-sweep-prettify/gate/gate.mjs')
const CODEX_GATE = readSkill('../.codex/skills/bs-sweep-prettify/gate/gate.mjs')

// Live cron name + legacy slug the gate matches to suppress duplicate sweep PRs.
const GATE_CRON_NAMES = ['Bossanova sweep prettify', 'bs-sweep-prettify']

function allowedTools(skill) {
  const match = skill.match(/^allowed-tools:\s*(.+)$/m)
  assert.ok(match, 'skill frontmatter must declare allowed-tools')
  return match[1].split(',').map((tool) => tool.trim())
}

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
  assert.match(SKILL, /Do not ask the user questions/i, 'must forbid asking questions')
  assert.ok(SKILL.includes('BOSS_CRON'), 'must reference the headless cron env')
})

// ---------------------------------------------------------------------------
// Formatting-only + self-owned finalize contract.
// ---------------------------------------------------------------------------

test('the sweep is formatting-only and never hand-edits', () => {
  assert.match(SKILL, /formatting-only/i, 'must state the diff is formatting-only')
  assert.match(SKILL, /never hand-edit/i, 'must forbid hand-editing files')
})

test('commits are tagless with a style scope; PR number injected at finalize', () => {
  assert.ok(SKILL.includes('style(repo):'), 'documents the tagless style(scope) commit')
  assert.match(SKILL, /tagless/i, 'must state commits are tagless')
  assert.ok(SKILL.includes('add-pr-numbers.sh'), 'must inject the PR number via boss-finalize')
  assert.ok(
    SKILL.includes('--force-with-lease'),
    'must force-push with lease after PR-number injection',
  )
})

test('self-owned finalize removes bossd Stop-hooks before stopping', () => {
  assert.ok(
    SKILL.includes('scripts/remove-bossd-stop-hooks.mjs'),
    'must remove bossd Stop-hooks so bossd cannot double-finalize',
  )
  assert.ok(SKILL.includes('gh pr ready'), 'this skill owns readying the PR')
})

test('mirror gate is documented for any edit to this skill', () => {
  assert.ok(SKILL.includes('make codex-skills'), 'must document the codex-skills mirror gate')
})

// bossd writes these into Claude cron worktrees (plugins/bossd-plugin-claude/
// dirty_files.go). They are untracked, so `git diff` never sees them, but the
// NO_CHANGE porcelain gate and Phase 4 `git add -A` would — so both must exclude
// them, or a managed lock alone opens a lock-only PR / gets committed.
test('NO_CHANGE gate and staging exclude bossd-managed dirty files', () => {
  for (const p of [
    '.claude/settings.local.json',
    '.claude/scheduled_tasks.lock',
    '.superpowers/',
  ]) {
    assert.ok(SKILL.includes(p), `SKILL must account for managed path ${p}`)
  }
  assert.match(SKILL, /grep -Ev/, 'NO_CHANGE gate must filter managed paths from porcelain')
  assert.match(SKILL, /git reset -q --/, 'staging must unstage managed files after git add -A')
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
  assert.ok(GATE.includes("from '../../../../scripts/linear-gate-lib.mjs'"), 'imports gateExit')
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
    assert.match(gate, /Bossanova sweep prettify/, `${label} gate must match the live cron name`)
    assert.match(gate, /'bs-sweep-prettify'/, `${label} gate must carry the legacy cron name`)
    assert.match(
      gate,
      /bs-sweep-prettify gate: prior sweep PR still open/,
      `${label} skip reason must be loud`,
    )
  }
})

test('the gate CRON_NAMES pair drives hasOpenCronPR suppression', () => {
  assert.match(GATE, /CRON_NAMES = \['Bossanova sweep prettify', 'bs-sweep-prettify'\]/)
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
  assert.throws(() => parseGoFiles({ error: new Error('no git') }), /git ls-files failed/)
})

test('parseGoFiles: non-zero exit throws (fail closed), not an empty list', () => {
  assert.throws(() => parseGoFiles({ status: 128, stdout: '' }), /git ls-files failed: exit 128/)
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
    const r = map[cmd]
    return typeof r === 'function' ? r(args) : r
  }
}

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

// `make format` runs `lint-check-version` first and exits before any formatter
// when the pinned golangci-lint is missing/wrong-version. If the gate reported
// drift there, Phase 1 `make format` would fail → NO_CHANGE → the drift re-triggers
// the gate forever, burning an agent session each time. Probe the same prereq and
// fail CLOSED so every reported drift is one the sweep can actually reconcile.
test('make format prerequisite failure fails closed (throws), not drift', () => {
  assert.throws(
    () =>
      detectDrift(
        fakeRunner({ ...CLEAN, make: { status: 1, stdout: 'golangci-lint not installed' } }),
        {
          goFiles,
        },
      ),
    /make format prerequisite \(lint-check-version\) failed \(exit 1\)/,
  )
})

test('make prerequisite spawn error fails closed (throws)', () => {
  assert.throws(
    () => detectDrift(fakeRunner({ ...CLEAN, make: { error: new Error('no make') } }), { goFiles }),
    /make prerequisite probe failed to run/,
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

test('prettier drift (exit 1 WITH its --check marker) -> run, reason prettier-docs', () => {
  const res = detectDrift(fakeRunner({ ...CLEAN, pnpm: PRETTIER_DRIFT }), { goFiles })
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
    () => detectDrift(fakeRunner({ ...CLEAN, pnpm: toolingFail }), { goFiles }),
    /prettier probe errored \(exit 1\)/,
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
    () => detectDrift(fakeRunner({ ...CLEAN, pnpm: { status: 2, stdout: '' } }), { goFiles }),
    /prettier probe errored \(exit 2\)/,
  )
})

test('prettier missing-binary exit (127) fails closed (throws)', () => {
  assert.throws(
    () => detectDrift(fakeRunner({ ...CLEAN, pnpm: { status: 127, stdout: '' } }), { goFiles }),
    /prettier probe errored \(exit 127\)/,
  )
})

test('gofmt drift (listed file) -> run, reason gofmt', () => {
  const res = detectDrift(fakeRunner({ ...CLEAN, gofmt: { status: 0, stdout: 'a.go\n' } }), {
    goFiles,
  })
  assert.equal(res.drift, true)
  assert.ok(res.reasons.includes('gofmt'))
})

// goimports is intentionally NOT probed: `make format` runs `gofmt -w` +
// read-only golangci per module and never applies `goimports -w`, so a
// goimports-only drift the sweep can't reconcile must not trigger a (wasted) run.
test('goimports-only drift does NOT run, and `go` is never probed', () => {
  const run = (cmd, args) => {
    if (cmd === 'make') return { status: 0, stdout: '' }
    if (cmd === 'pnpm') return { status: 0, stdout: '' }
    if (cmd === 'gofmt') return { status: 0, stdout: '' }
    throw new Error(`gate must not probe ${cmd} ${args?.join(' ')} — make format has no goimports`)
  }
  const res = detectDrift(run, { goFiles })
  assert.equal(res.drift, false)
  assert.deepEqual(res.reasons, [])
})

test('required prettier probe spawn error fails closed (throws)', () => {
  assert.throws(
    () => detectDrift(fakeRunner({ ...CLEAN, pnpm: { error: new Error('no pnpm') } }), { goFiles }),
    /prettier probe failed to run/,
  )
})

test('required gofmt probe spawn error fails closed (throws)', () => {
  assert.throws(
    () =>
      detectDrift(fakeRunner({ ...CLEAN, gofmt: { error: new Error('no gofmt') } }), { goFiles }),
    /gofmt probe failed to run/,
  )
})

// `gofmt -l` exits 0 and lists drift on stdout, so a non-zero exit is a genuine
// error (unreadable/unparseable file), not "no drift" — it must fail closed.
test('gofmt error exit (2) fails closed (throws), not read as clean', () => {
  assert.throws(
    () => detectDrift(fakeRunner({ ...CLEAN, gofmt: { status: 2, stdout: '' } }), { goFiles }),
    /gofmt probe errored \(exit 2\)/,
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
  assert.equal(calls, 2, 'only the make prereq + prettier probes run when there are no Go files')
})

test('chunk batches Go files under GO_BATCH to stay within ARG_MAX', () => {
  const files = Array.from({ length: GO_BATCH * 2 + 5 }, (_, i) => `f${i}.go`)
  const batches = chunk(files)
  assert.equal(batches.length, 3)
  assert.ok(batches.every((b) => b.length <= GO_BATCH))
  assert.equal(batches.flat().length, files.length)
})
