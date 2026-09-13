// Contract suite for the bs-sweep-update WORKER gate (BOS-1225).
//
// This is the first cron gate in the repo that mutates the repository, and it
// inverts the sibling convention: the other `bs-sweep-*` gates exit 0 to wake an
// agent when there is work, while this one IS the work and always exits 1. There
// is no local precedent a reader could infer that from, so the inversion, the
// exit-code discipline, the preflight refusal, and the ladder's branch selection
// are asserted here rather than left to convention.
//
// Everything below runs against the gate's injected seams — no git is spawned and
// no checkout is touched.

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  COMMAND_TIMEOUT_MS,
  EXPECTED_PACKAGE_NAME,
  GATE_EXIT_CODE,
  GATE_TIMEOUT_CEILING_MS,
  LADDER_BUDGET_MS,
  STDERR_PREFIX,
  createBudget,
  refresh,
  resolveRoot,
  runUpdateGate,
  spawnGit,
  validateBossanovaCheckout,
} from '../.claude/skills/bs-sweep-update/gate/gate.mjs'
import { assertMirrorRegenerated } from './size-ratchet-lib.mjs'
import { rewriteClaudeSkillMarkdown } from './sync-codex-skills.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const abs = (rel) => path.join(repoRoot, rel)
const SKILL_PATH = abs('.claude/skills/bs-sweep-update/SKILL.md')
const MIRROR_PATH = abs('.codex/skills/bs-sweep-update/SKILL.md')
const GATE_PATH = abs('.claude/skills/bs-sweep-update/gate/gate.mjs')
const GATE_DIR = path.dirname(GATE_PATH)

const gateSource = () => readFileSync(GATE_PATH, 'utf8')
const skillBody = () => readFileSync(SKILL_PATH, 'utf8')

const ok = (stdout = '') => ({ status: 0, stdout, stderr: '' })
const fail = (stderr, status = 1) => ({ status, stdout: '', stderr })

// A command runner that answers by exact argv and records every invocation, so a
// test can assert both WHICH ladder rung ran and that a refused path ran none.
function scriptedCommand(script = {}) {
  const calls = []
  const command = (args, options) => {
    calls.push({ args, options })
    const key = args.join(' ')
    return Object.hasOwn(script, key) ? script[key] : ok()
  }
  return { calls, command, verbs: () => calls.map((call) => call.args.join(' ')) }
}

// The healthy answers for a clean checkout sitting on its default branch.
const CLEAN_ON_BASE = {
  'fetch --prune origin': ok(),
  'symbolic-ref --short refs/remotes/origin/HEAD': ok('origin/main\n'),
  'rev-parse --abbrev-ref HEAD': ok('main\n'),
  'status --porcelain': ok(''),
  'merge --ff-only refs/remotes/origin/main': ok(),
}

function runGate({ validate, run, command } = {}) {
  const stderrLines = []
  const exitCodes = []
  runUpdateGate({
    command,
    exit: (code) => exitCodes.push(code),
    run: run ?? (() => ({ outcome: 'advanced' })),
    stderr: (text) => stderrLines.push(text),
    validate: validate ?? (() => ({})),
  })
  return { exitCodes, stderrLines }
}

// ─── Root resolution and preflight (R6) ──────────────────────────────────────────────────

test('resolveRoot prefers REPO_DIR and otherwise resolves four levels up from the gate', () => {
  assert.equal(
    resolveRoot({ env: { REPO_DIR: '/tmp/main-checkout' }, gateDir: GATE_DIR }),
    '/tmp/main-checkout',
  )
  assert.equal(resolveRoot({ env: { REPO_DIR: '   ' }, gateDir: GATE_DIR }), repoRoot)
  assert.equal(resolveRoot({ env: {}, gateDir: GATE_DIR }), repoRoot)
})

test('preflight refuses a linked worktree before it even reads the package marker', () => {
  let packageReads = 0
  assert.throws(
    () =>
      validateBossanovaCheckout({
        exec: () => '/repo/.git/worktrees/cron-1\n/repo/.git\n',
        readFile: () => {
          packageReads += 1
          return '{"name":"bossanova"}'
        },
        root: '/repo/worktrees/cron-1',
      }),
    /linked\s+worktree/,
  )
  assert.equal(packageReads, 0, 'a linked worktree must be refused before any further probe')
})

test('preflight refuses a root whose package marker is not this repository', () => {
  assert.throws(
    () =>
      validateBossanovaCheckout({
        exec: () => '/elsewhere/.git\n/elsewhere/.git\n',
        readFile: () => '{"name":"some-other-repo"}',
        root: '/elsewhere',
      }),
    /some-other-repo/,
  )
})

test('preflight accepts a main checkout and reports the dirs it proved equal', () => {
  const accepted = validateBossanovaCheckout({
    exec: () => '/repo/.git\n/repo/.git\n',
    readFile: () => `{"name":"${EXPECTED_PACKAGE_NAME}"}`,
    root: '/repo',
  })
  assert.deepEqual(accepted, { commonDir: '/repo/.git', gitDir: '/repo/.git', root: '/repo' })
})

test('the real repository still carries the package marker the preflight asserts', () => {
  assert.equal(JSON.parse(readFileSync(abs('package.json'), 'utf8')).name, EXPECTED_PACKAGE_NAME)
})

test('a refused preflight issues ZERO ladder commands', () => {
  const { calls, command } = scriptedCommand(CLEAN_ON_BASE)
  const { exitCodes, stderrLines } = runGate({
    command,
    run: refresh,
    validate: () => {
      throw new Error('refusing a linked worktree: git dir A is not the common dir B')
    },
  })
  assert.equal(calls.length, 0, 'nothing may be spawned once preflight refuses')
  assert.deepEqual(exitCodes, [GATE_EXIT_CODE])
  assert.equal(stderrLines.length, 1)
})

// ─── Ladder branch selection ─────────────────────────────────────────────────────────────

test('the ladder picks exactly one advance rung per checkout shape', () => {
  const cases = [
    {
      expectAbsent: ['fetch origin main:main'],
      expectRan: 'merge --ff-only refs/remotes/origin/main',
      name: 'clean and on the base',
      outcome: { base: 'main', outcome: 'advanced', via: 'merge-ff-only' },
      script: CLEAN_ON_BASE,
    },
    {
      expectAbsent: ['merge --ff-only refs/remotes/origin/main', 'status --porcelain'],
      expectRan: 'fetch origin main:main',
      name: 'not on the base',
      outcome: { base: 'main', outcome: 'advanced', via: 'fetch-ref' },
      script: { ...CLEAN_ON_BASE, 'rev-parse --abbrev-ref HEAD': ok('feature-x\n') },
    },
    {
      expectAbsent: ['merge --ff-only refs/remotes/origin/main', 'status --porcelain'],
      expectRan: 'fetch origin main:main',
      name: 'detached HEAD is never the base',
      outcome: { base: 'main', outcome: 'advanced', via: 'fetch-ref' },
      script: { ...CLEAN_ON_BASE, 'rev-parse --abbrev-ref HEAD': ok('HEAD\n') },
    },
    {
      declined: /local\s+modifications/,
      expectAbsent: ['merge --ff-only refs/remotes/origin/main', 'fetch origin main:main'],
      name: 'dirty and on the base moves nothing',
      script: { ...CLEAN_ON_BASE, 'status --porcelain': ok(' M services/boss/main.go\n') },
    },
  ]

  for (const testCase of cases) {
    const { calls, command, verbs } = scriptedCommand(testCase.script)
    const result = refresh({ command, root: '/repo' })

    assert.equal(
      calls[0].args.join(' '),
      'fetch --prune origin',
      `${testCase.name}: fetch --prune must run first`,
    )

    if (testCase.declined) {
      assert.equal(result.outcome, 'declined', `${testCase.name}: expected a declined result`)
      assert.match(result.reason, testCase.declined)
    } else {
      assert.deepEqual(result, testCase.outcome, testCase.name)
      assert.ok(
        verbs().includes(testCase.expectRan),
        `${testCase.name}: expected ${testCase.expectRan}`,
      )
    }

    for (const absent of testCase.expectAbsent) {
      assert.ok(!verbs().includes(absent), `${testCase.name}: must not run ${absent}`)
    }
  }
})

test('the ladder resolves the default branch instead of assuming main', () => {
  const { command, verbs } = scriptedCommand({
    ...CLEAN_ON_BASE,
    'merge --ff-only refs/remotes/origin/trunk': ok(),
    'rev-parse --abbrev-ref HEAD': ok('trunk\n'),
    'symbolic-ref --short refs/remotes/origin/HEAD': ok('origin/trunk\n'),
  })
  const result = refresh({ command, root: '/repo' })
  assert.equal(result.base, 'trunk')
  assert.ok(verbs().includes('merge --ff-only refs/remotes/origin/trunk'))
})

test('a git refusal is declined, never thrown and never forced', () => {
  const refusals = [
    {
      name: 'the ref-only fetch refuses a non-fast-forward',
      script: {
        ...CLEAN_ON_BASE,
        'fetch origin main:main': fail('! [rejected] main -> main (non-fast-forward)'),
        'rev-parse --abbrev-ref HEAD': ok('feature-x\n'),
      },
    },
    {
      name: 'the ff-only merge refuses a diverged base',
      script: {
        ...CLEAN_ON_BASE,
        'merge --ff-only refs/remotes/origin/main': fail(
          'fatal: Not possible to fast-forward, aborting.',
        ),
      },
    },
    {
      name: 'the initial fetch fails',
      script: { ...CLEAN_ON_BASE, 'fetch --prune origin': fail('fatal: unable to access origin') },
    },
    {
      name: 'the default branch cannot be resolved',
      script: {
        ...CLEAN_ON_BASE,
        'symbolic-ref --short refs/remotes/origin/HEAD': fail(
          'fatal: ref refs/remotes/origin/HEAD is not a symbolic ref',
        ),
      },
    },
  ]

  for (const refusal of refusals) {
    const { command } = scriptedCommand(refusal.script)
    const result = refresh({ command, root: '/repo' })
    assert.equal(result.outcome, 'declined', refusal.name)
    assert.ok(result.reason.length > 0, `${refusal.name}: a decline must carry a reason`)
    assert.ok(!result.reason.includes('\n'), `${refusal.name}: the reason must be a single line`)
  }
})

// ─── Time budget (R5) ────────────────────────────────────────────────────────────────────

test('the configured budgets sit strictly below gatecmd DefaultTimeout', () => {
  assert.equal(GATE_TIMEOUT_CEILING_MS, 60_000)
  assert.ok(COMMAND_TIMEOUT_MS > 0 && COMMAND_TIMEOUT_MS < GATE_TIMEOUT_CEILING_MS)
  assert.ok(LADDER_BUDGET_MS > COMMAND_TIMEOUT_MS && LADDER_BUDGET_MS < GATE_TIMEOUT_CEILING_MS)
  // The preflight probe runs on its own seam and is NOT charged to the ladder
  // budget, so the two run back to back. Pin the SUM: asserting each constant
  // alone still passes at 59_000 + 59_000, which is the red `gate_failed` this
  // whole budget exists to prevent.
  assert.ok(
    COMMAND_TIMEOUT_MS + LADDER_BUDGET_MS < GATE_TIMEOUT_CEILING_MS,
    'preflight + ladder must fit inside the gatecmd ceiling with headroom',
  )
})

test('every command the ladder issues carries an explicit sub-ceiling timeout', () => {
  const { calls, command } = scriptedCommand(CLEAN_ON_BASE)
  refresh({ command, root: '/repo' })
  assert.ok(calls.length >= 4, 'the clean path must issue the whole ladder')
  for (const call of calls) {
    const budget = call.options?.timeoutMs
    assert.ok(Number.isFinite(budget) && budget > 0, `git ${call.args[0]} must carry a timeout`)
    assert.ok(
      budget <= COMMAND_TIMEOUT_MS,
      `git ${call.args[0]} must respect the per-command ceiling`,
    )
    assert.ok(
      budget < GATE_TIMEOUT_CEILING_MS,
      `git ${call.args[0]} must stay under the gate ceiling`,
    )
  }
})

test('the default runner hands the configured timeout to the child process', () => {
  const spawned = []
  spawnGit(['status', '--porcelain'], {
    cwd: '/repo',
    exec: (bin, args, options) => {
      spawned.push({ args, bin, options })
      return ''
    },
  })
  assert.equal(spawned.length, 1)
  assert.equal(spawned[0].bin, 'git')
  assert.equal(spawned[0].options.timeout, COMMAND_TIMEOUT_MS)
  assert.ok(spawned[0].options.timeout < GATE_TIMEOUT_CEILING_MS)
})

test('an exhausted whole-run budget spawns nothing and declines loudly', () => {
  const { calls, command } = scriptedCommand(CLEAN_ON_BASE)
  const budget = createBudget({ now: () => 0, totalMs: 0 })
  assert.equal(budget.nextTimeoutMs(), 0)
  assert.throws(() => refresh({ budget, command, root: '/repo' }), /budget/)
  assert.equal(calls.length, 0)
})

test('a timeout kill surfaces as a throw, not as an ordinary non-zero result', () => {
  assert.throws(
    () =>
      spawnGit(['fetch', '--prune', 'origin'], {
        cwd: '/repo',
        exec: () => {
          const err = new Error('spawn timed out')
          err.killed = true
          err.signal = 'SIGTERM'
          throw err
        },
      }),
    /exceeded\s+its\s+.*budget/,
  )
})

// ─── The always-non-zero exit contract (R1, R2, R3) ──────────────────────────────────────

test('the success path exits 1 and writes NOTHING to stderr', () => {
  const { exitCodes, stderrLines } = runGate({
    run: () => ({ base: 'main', outcome: 'advanced', via: 'merge-ff-only' }),
  })
  assert.deepEqual(exitCodes, [GATE_EXIT_CODE])
  assert.deepEqual(stderrLines, [])
})

test('the update-failure path exits 1 with exactly one prefixed stderr line', () => {
  const { exitCodes, stderrLines } = runGate({
    run: () => ({ outcome: 'declined', reason: 'main is live with local modifications' }),
  })
  assert.deepEqual(exitCodes, [GATE_EXIT_CODE])
  assert.equal(stderrLines.length, 1)
  assert.ok(stderrLines[0].startsWith(STDERR_PREFIX))
  assert.ok(stderrLines[0].endsWith('\n'), 'the line must be newline-terminated')
  assert.ok(!stderrLines[0].slice(0, -1).includes('\n'), 'exactly one line')
})

test('a thrown ladder error is collapsed to one prefixed stderr line', () => {
  const { exitCodes, stderrLines } = runGate({
    run: () => {
      throw new Error('git is unavailable: spawn git ENOENT\nsecond line that must not escape')
    },
  })
  assert.deepEqual(exitCodes, [GATE_EXIT_CODE])
  assert.equal(stderrLines.length, 1)
  assert.ok(stderrLines[0].startsWith(STDERR_PREFIX))
  assert.ok(!stderrLines[0].includes('second line'))
  assert.ok(stderrLines[0].endsWith('\n'))
  assert.ok(!stderrLines[0].slice(0, -1).includes('\n'))
})

test('the preflight-refusal path exits 1 with exactly one prefixed stderr line', () => {
  const { exitCodes, stderrLines } = runGate({
    validate: () => {
      throw new Error('refusing a foreign root: expected package bossanova, got other')
    },
  })
  assert.deepEqual(exitCodes, [GATE_EXIT_CODE])
  assert.equal(stderrLines.length, 1)
  assert.ok(stderrLines[0].startsWith(STDERR_PREFIX))
})

test('no path ever exits 0, 126, or 127 — the codes gatecmd reads as wake or red', () => {
  const paths = [
    { run: () => ({ outcome: 'advanced' }) },
    { run: () => ({ outcome: 'declined', reason: 'declined' }) },
    {
      validate: () => {
        throw new Error('refused')
      },
    },
    {
      run: () => {
        throw new Error('exploded')
      },
    },
  ]
  for (const scenario of paths) {
    const { exitCodes } = runGate(scenario)
    assert.deepEqual(exitCodes, [1])
    for (const code of exitCodes) {
      assert.notEqual(code, 0)
      assert.notEqual(code, 126)
      assert.notEqual(code, 127)
    }
  }
})

// ─── Source-level guarantees ─────────────────────────────────────────────────────────────

test('no path in the gate can emit a destructive or publishing git verb', () => {
  assert.doesNotMatch(
    gateSource(),
    /stash|reset|rebase|'clean'|'restore'|'revert'|'cherry-pick'|'update-ref'|'reflog'|'switch'|'branch'|'worktree'|'gc'|'checkout'|'push'|'commit'|--force/,
  )
})

test('the gate only auto-runs when it is the main module', () => {
  assert.match(gateSource(), /if \(isMainModule\(import\.meta\.url\)\) runUpdateGate\(\)/)
})

// ─── Skill body and its generated mirror ─────────────────────────────────────────────────

test('the skill frontmatter matches every sibling sweep', () => {
  const body = skillBody()
  assert.ok(body.includes('\nname: bs-sweep-update\n'))
  assert.ok(body.includes('\ndisable-model-invocation: true\n'))
  assert.ok(body.includes('\nallowed-tools: Bash, Read\n'))
})

test('the skill body states the inversion, the exit-code rule, and the dirty decline', () => {
  const body = skillBody()
  for (const literal of [
    'always exits non-zero',
    'selection',
    '126',
    '127',
    'dirty',
    'refs are already fresh',
    'node .claude/skills/bs-sweep-update/gate/gate.mjs',
    'GateCommand',
  ]) {
    assert.ok(body.includes(literal), `SKILL.md must carry ${JSON.stringify(literal)}`)
  }
})

test('the codex mirror is exactly what regenerating it from the .claude source produces', () => {
  assertMirrorRegenerated({
    mirrorPath: MIRROR_PATH,
    regenerate: rewriteClaudeSkillMarkdown,
    sourcePath: SKILL_PATH,
  })
})
