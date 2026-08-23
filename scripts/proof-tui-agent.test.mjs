/**
 * proof-tui-agent.test.mjs — Unit tests for the TUI agent proof orchestrator.
 *
 * Strategy (mirrors proof-agent.test.mjs): drive the REAL runTuiAgentProof with
 * a fully stubbed dependency seam — a scripted NDJSON bridge, a scripted
 * Anthropic-shaped model, an injectable still renderer, and an injectable
 * cast→video converter. NOTHING here spawns the Go bridge, agg, ffmpeg, or hits
 * the Anthropic API / network. The Go `proof-tui-agent` bridge binary does not
 * exist on main yet (BOS-69 is in review) and there is no ANTHROPIC_API_KEY in
 * CI, so every test runs on stubs only.
 *
 * Required cases (from the Task 2 brief):
 *   1. brief → loop → frames → manifest (happy path)
 *   2. secret-scan abort (planted AWS-shaped key in done summary)
 *   3. budget stop (maxSteps / wall-clock / tokens variants)
 *   4. zero-frame fallback (no stills captured → exactly one fallback still)
 *   5. cast→video stubbed (mp4 present) AND null (stills-only still succeeds)
 *   6. T1 evidence gate (present → passed; absent → failed)
 */

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { silenceConsole } from './quiet-test-console.mjs'

const REPO_ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')

// Silence the code-under-test's console output (the "[proof-tui-agent] DEGRADED
// …" warnings and finalize's run-file manifest JSON dumps). The few tests that
// assert on warnings install their own capture inside the test body; that
// composes cleanly with the per-test restore. See quiet-test-console.mjs.
silenceConsole()

// Hermeticity guard (BOS-141): finalizeAgentProof's default `judge` dep
// (judgeProof) makes a REAL Anthropic API call whenever PROOF_ANTHROPIC_API_KEY
// is set — and it IS set in every provisioned worktree (repo .env is copied
// in). runTuiAgentProof forwards deps.finalizeDeps verbatim into finalize and
// no test here stubs it, so drop the key for this file's test process:
// judgeProof then short-circuits to { unjudged: true, reason: 'missing-key' }
// BEFORE constructing the SDK (unit-proven in proof-judge.test.mjs), keeping
// this file's documented no-network contract true. The createSdkModel tests
// that need the key set and restore it around themselves, unaffected.
// node --test runs each file in its own process, so this cannot leak.
delete process.env.PROOF_ANTHROPIC_API_KEY

// ── Test doubles ──────────────────────────────────────────────────────────────

/** Build an Anthropic-shaped tool_use response. */
function toolUse(name, input, { text = '', id, usage } = {}) {
  return {
    stop_reason: 'tool_use',
    usage: usage ?? { input_tokens: 10, output_tokens: 5 },
    content: [
      ...(text ? [{ type: 'text', text }] : []),
      { type: 'tool_use', id: id ?? `tu-${name}`, name, input },
    ],
  }
}

/**
 * Scripted model: returns the next queued response per createMessage call.
 * When `repeatLast` is set, keeps returning the final response forever (used to
 * drive the budget-stop tests where the model never calls done).
 */
function scriptedModel(responses, { repeatLast = false } = {}) {
  let i = 0
  const model = {
    calls: 0,
    async createMessage() {
      model.calls += 1
      const idx = repeatLast ? Math.min(i, responses.length - 1) : i
      const resp = responses[Math.min(idx, responses.length - 1)]
      if (i < responses.length - 1) i += 1
      return resp
    },
  }
  return model
}

/**
 * Scripted NDJSON bridge stub. `screens` is consumed one per bridge op; the last
 * screen repeats once exhausted. Tracks whether quit() was sent.
 */
function scriptedBridge({ screens = ['HOME SCREEN'] } = {}) {
  let i = 0
  const next = () => {
    const s = screens[Math.min(i, screens.length - 1)]
    i += 1
    return s
  }
  const bridge = {
    quitCalled: false,
    observeCount: 0,
    async observe() {
      bridge.observeCount += 1
      return { screen: next() }
    },
    async sendKeys() {
      return { screen: next() }
    },
    async typeText() {
      return { screen: next() }
    },
    async quit() {
      bridge.quitCalled = true
    },
  }
  return bridge
}

/** Still renderer stub: writes a fake PNG at the requested output path. */
function fakeRenderStill() {
  const calls = []
  const fn = async ({ input, output, title }) => {
    calls.push({ input, output, title })
    fs.writeFileSync(output, 'fake-png-bytes')
  }
  fn.calls = calls
  return fn
}

async function withTempBrief(brief, fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-brief-'))
  const briefPath = path.join(dir, 'brief.json')
  fs.writeFileSync(briefPath, JSON.stringify(brief, null, 2))
  try {
    return await fn(briefPath)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

function withEnv(overrides, fn) {
  const keys = [
    'BOSS_PROOF_BRIEF',
    'BOSS_PROOF_UPLOAD',
    'BOSS_PROOF_RUN_ID',
    'BOSS_PROOF_RUN_TOKEN',
    'BOSS_PROOF_R2_BUCKET',
    'BOSS_PROOF_PUBLIC_BASE_URL',
    'BOSS_PROOF_MODEL',
    'BOSS_PROOF_TUI_MODEL',
    'BOSS_PROOF_AGENT_TIMEOUT_MS',
    'BOSS_PROOF_TUI_ALLOW_STILLS_ONLY',
  ]
  const saved = {}
  for (const k of keys) saved[k] = process.env[k]
  for (const [k, v] of Object.entries(overrides)) {
    if (v === undefined) delete process.env[k]
    else process.env[k] = v
  }
  return Promise.resolve()
    .then(fn)
    .finally(() => {
      for (const k of keys) {
        if (saved[k] === undefined) delete process.env[k]
        else process.env[k] = saved[k]
      }
    })
}

function cleanupPr(prNumber) {
  fs.rmSync(path.join(REPO_ROOT, '.proof', `pr-${prNumber}`), { recursive: true, force: true })
}

const BASE_ENV = (over = {}) => ({
  BOSS_PROOF_UPLOAD: '0',
  BOSS_PROOF_RUN_TOKEN: 'tok-tui-test',
  BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
  ...over,
})

// ── Anthropic SDK loading ────────────────────────────────────────────────────

test('SYSTEM_PROMPT: instructs the agent to finish all steps before done()', async () => {
  const mod = await import('./proof-tui-agent.mjs')
  assert.match(mod.SYSTEM_PROMPT, /complete every/i)
  assert.match(mod.SYSTEM_PROMPT, /before calling done/i)
  assert.match(mod.SYSTEM_PROMPT, /done\(passed=true\)/i)
  assert.match(mod.SYSTEM_PROMPT, /ALL.*expected-evidence strings/i)
  assert.match(mod.SYSTEM_PROMPT, /INJECTION GUARD/i)
  assert.match(mod.SYSTEM_PROMPT, /done\(passed=false\).*broken.*cannot complete/i)
})

test('SYSTEM_PROMPT: requires a narration sentence alongside each action (caption source)', async () => {
  const mod = await import('./proof-tui-agent.mjs')
  // The TUI caption is sourced from the model's per-turn text block, so the
  // prompt must require narration or tool-only turns leave frames uncaptioned.
  assert.match(mod.SYSTEM_PROMPT, /CAPTION/)
  assert.match(mod.SYSTEM_PROMPT, /same message as each observe\/send_keys\/type_text/i)
  assert.match(mod.SYSTEM_PROMPT, /never call those tools without a leading narration/i)
})

test('SYSTEM_PROMPT instructs a final captioned observe() before done() (BOS-393)', async () => {
  const { SYSTEM_PROMPT } = await import('./proof-tui-agent.mjs')
  // Loose but anchored on the three load-bearing ideas: a final observe, quoting
  // the on-screen evidence, before done() — so the closing narration is a strong
  // final caption for the confirmation outro.
  assert.match(SYSTEM_PROMPT, /final observe\(\)[\s\S]*quot(e|es|ing)[\s\S]*before[\s\S]*done\(\)/i)
})

test('doc-drift: send_keys documents the full KeyBytes vocabulary via golden (BOS-282)', async () => {
  const mod = await import('./proof-tui-agent.mjs')
  // The accepted key vocabulary is owned by the Go parser (tuidriver.namedKeys)
  // and pinned into key-vocab.json by TestKeyVocabGolden. Read it back rather
  // than hardcoding a subset copy (the drift bug BOS-213 left behind): a new
  // named key added to namedKeys is auto-derived into the golden, and this guard
  // then fails until send_keys documents it. Families are parametric key kinds
  // KeyBytes recognizes structurally (not map keys), so generateKeyVocab lists
  // them by hand — a new one only reaches the golden when a maintainer edits that
  // list, at which point the else-branch below fails loud until this guard learns
  // to check it.
  const golden = JSON.parse(
    fs.readFileSync(
      path.join(REPO_ROOT, 'services/boss/internal/tuidriver/testdata/key-vocab.json'),
      'utf8',
    ),
  )
  const sendKeys = mod.TOOL_DEFS.find((t) => t.name === 'send_keys')
  assert.ok(sendKeys, 'send_keys tool must exist')

  // Every parser-accepted named key must appear verbatim in the tool
  // description so the model knows it is a legal keystroke. Match the QUOTED
  // token (`"esc"`), not a bare substring: short names like "esc", "tab", "up",
  // "f1" are substrings of longer tokens ("escape", "shifttab", "pgup", "f10")
  // and a bare `includes(name)` would silently pass even if the key's own
  // standalone mention were dropped from the prose — defeating the drift guard.
  // The description quotes every alias, so the quoted form is exact.
  for (const name of golden.named) {
    assert.ok(
      sendKeys.description.includes(`"${name}"`),
      `send_keys description should mention key "${name}" as a quoted token`,
    )
  }

  // Reverse drift (BOS-282): the forward loop only proves every CURRENT golden
  // key is documented — it cannot catch a key that was removed or renamed in
  // namedKeys (and dropped from the regenerated golden) but left behind in the
  // prose, which would keep advertising a keystroke the Go parser now rejects.
  // So walk every quoted token in the description and require each to be either a
  // current golden named key OR a documented family example (a single printable
  // char like "s", a "ctrl+<a-z>" chord like "ctrl+c", or the "ctrl+<a-z>"
  // family label itself). Anything else is a stale documented key.
  const namedSet = new Set(golden.named)
  const isFamilyExample = (tok) =>
    tok.length === 1 || // single-printable-char family (e.g. "s", "/")
    /^ctrl\+[a-z]$/.test(tok) || // ctrl+<a-z> chord example (e.g. "ctrl+c")
    tok === 'ctrl+<a-z>' // the ctrl+<a-z> family label itself
  for (const [, tok] of sendKeys.description.matchAll(/"([^"]+)"/g)) {
    assert.ok(
      namedSet.has(tok) || isFamilyExample(tok),
      `send_keys description quotes "${tok}", which is neither a current ` +
        'parser-accepted named key (golden.named) nor a documented family example — ' +
        'remove the stale key, or regenerate key-vocab.json if the parser still accepts it',
    )
  }

  // Families are parametric key kinds (not fixed names). Map each known family
  // to the substring/shape that documents it. An UNKNOWN family means KeyBytes
  // grew a new kind the guard doesn't yet check — fail loud so the maintainer
  // teaches this guard instead of letting the doc silently drift.
  for (const family of golden.families) {
    if (family === 'ctrl+<a-z>') {
      assert.ok(
        sendKeys.description.includes('ctrl+'),
        'send_keys description must document the ctrl+<a-z> family',
      )
    } else if (family === 'single-printable-char') {
      assert.ok(
        /single(\s|-)?(printable\s)?char/i.test(sendKeys.description),
        'send_keys description must document the single-printable-character family',
      )
    } else {
      assert.fail(
        `unknown key-vocab family "${family}"; teach this guard how to check it ` +
          '(update the family switch in scripts/proof-tui-agent.test.mjs)',
      )
    }
  }

  // TUI_CONTEXT_BLOCK is app-semantic help, not the full parser alias list, so
  // it deliberately advertises only the everyday navigation keys (OQ2). For
  // those, assert BOTH that the golden accepts the key (the app help can never
  // advertise a keystroke the Go parser would reject) AND that the context
  // block still names it (BOS-213's guarantee, retained).
  const appNavKeys = [
    'up',
    'down',
    'left',
    'right',
    'tab',
    'shift+tab',
    'pgup',
    'pgdn',
    'home',
    'end',
    'backspace',
    'delete',
  ]
  for (const key of appNavKeys) {
    assert.ok(golden.named.includes(key), `golden vocabulary must accept nav key "${key}"`)
    assert.ok(
      mod.TUI_CONTEXT_BLOCK.includes(key),
      `TUI_CONTEXT_BLOCK should mention nav key "${key}"`,
    )
  }
})

test('loadAnthropic: missing SDK rejects with install hint', async () => {
  const { loadAnthropic } = await import('./proof-tui-agent.mjs')

  await assert.rejects(
    loadAnthropic(async () => {
      throw { code: 'ERR_MODULE_NOT_FOUND' }
    }),
    /pnpm install.*@anthropic-ai\/sdk/,
  )
})

test('loadAnthropic: returns default export function', async () => {
  const { loadAnthropic } = await import('./proof-tui-agent.mjs')

  function AnthropicStub() {}

  assert.equal(await loadAnthropic(async () => ({ default: AnthropicStub })), AnthropicStub)
})

test('runTuiAgentProof: generated brief missing SDK rejects with install hint', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')

  try {
    await withEnv(
      BASE_ENV({
        BOSS_PROOF_BRIEF: undefined,
        BOSS_PROOF_RUN_ID: 'tui-generated-brief-sdk-missing',
      }),
      async () => {
        await assert.rejects(
          runTuiAgentProof({
            prNumber: 'tuimissingbriefsdk',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              generateBriefFromDiff: async () => {
                throw { code: 'ERR_MODULE_NOT_FOUND' }
              },
            },
          }),
          /pnpm install.*@anthropic-ai\/sdk/,
        )
      },
    )
  } finally {
    cleanupPr('tuimissingbriefsdk')
  }
})

// ── 1. brief → loop → frames → manifest (happy path) ─────────────────────────

test('runTuiAgentProof: brief → loop → frames → manifest (happy path)', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Open settings',
    description: 'Demonstrates the settings screen opens',
    expectedEvidence: ['Settings'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-happy' }),
        async () => {
          const renderStill = fakeRenderStill()
          const bridge = scriptedBridge({ screens: ['Settings panel open', 'Settings saved'] })
          const model = scriptedModel([
            toolUse('observe', {}),
            toolUse('send_keys', { keys: ['s'] }),
            toolUse('done', { summary: 'Opened settings successfully', passed: true }),
          ])

          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuihappy',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: { bridge, model, renderStill, castToVideo: async () => null },
          })

          assert.equal(manifest.verdict, 'passed')
          assert.equal(manifest.agentRunnerStubbed, true)
          const cap = manifest.captures[0]
          assert.equal(cap.surface, 'tui')
          assert.equal(cap.status, 'passed')
          assert.ok(Array.isArray(cap.stills) && cap.stills.length >= 1, 'frames must be present')
          assert.ok(bridge.quitCalled, 'bridge.quit() must always be sent')
          // Hermeticity tripwire: this media-carrying run reaches finalize's
          // judge dep, which must have short-circuited on the deleted
          // PROOF_ANTHROPIC_API_KEY — any other value means a real (or
          // attempted) Anthropic call leaked into a unit test.
          assert.deepEqual(manifest.judge, { unjudged: true, reason: 'missing-key' })
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuihappy')
  }
})

// ── Regression: agent run must not abort on its own ISO-runId public URL ──────

test('runTuiAgentProof: ISO runId does not trip the manifest secret scan', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const brief = {
    title: 'Open settings',
    description: 'settings opens',
    expectedEvidence: ['Settings'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: '2026-06-26T02-16-36-825Z' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'isorun',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Settings panel open'] }),
              model: scriptedModel([
                toolUse('send_keys', { keys: ['s'] }),
                toolUse('done', { summary: 'Opened settings', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })

          assert.equal(manifest.verdict, 'passed')
        },
      ),
    )
  } finally {
    cleanupPr('isorun')
  }
})

// ── 2. budget stops ───────────────────────────────────────────────────────────

test('runTuiAgentProof: model never calls done → halts at maxSteps, not passed', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Runaway',
    description: 'Agent loops forever',
    budgets: { maxSteps: 3 },
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-maxsteps' }),
        async () => {
          const model = scriptedModel([toolUse('observe', {})], { repeatLast: true })
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuimaxsteps',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Home'] }),
              model,
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'failed')
          assert.equal(manifest.captures[0].status, 'failed')
          assert.ok(
            model.calls <= 3,
            `model must not be called more than maxSteps (was ${model.calls})`,
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuimaxsteps')
  }
})

test('runTuiAgentProof: token budget halts the loop with a non-passed result', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Token burner',
    description: 'Agent burns the token budget',
    budgets: { maxSteps: 50, maxTokens: 10 },
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-tokens' }),
        async () => {
          // Each step reports usage that exceeds the 10-token budget, so the loop
          // must stop after the first step.
          const model = scriptedModel(
            [toolUse('observe', {}, { usage: { input_tokens: 80, output_tokens: 40 } })],
            { repeatLast: true },
          )
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuitokens',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Home'] }),
              model,
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'failed')
          assert.ok(
            model.calls <= 2,
            `token budget must stop the loop early (calls=${model.calls})`,
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuitokens')
  }
})

test('runTuiWithReplayFallback: a wall-clock-truncated agent leg (no scenario) softens to a deferred tui-truncated (exit 0), not agent-incomplete (BOS-354)', async () => {
  const { runTuiAgentProof, runTuiWithReplayFallback } = await import('./proof-tui-agent.mjs')
  const { classifySurfaceOutcomes, aggregateExitCode } =
    await import('./proof-finalize-outcome.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Slowpoke',
    description: 'Agent exceeds the wall-clock budget',
    budgets: { maxSteps: 50, maxWallClockMs: -1 },
  }

  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-truncated-'))
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-wallclock' }),
        async () => {
          const model = scriptedModel([toolUse('observe', {})], { repeatLast: true })
          // Drive the real agent leg through the dispatcher in collect mode with a
          // negative wall clock: the loop breaks before any verdict → truncated.
          const run = await runTuiWithReplayFallback(
            {
              prNumber: 'tuiwallclock',
              commit: 'abc1234',
              changedFiles: [], // no committed scenario → replay leg not attempted
              dryRun: true,
              runContext: {
                collect: true,
                runId: 'tui-wallclock',
                token: 'tok-tui-test',
                localDir,
                maxWallClockMs: -1,
              },
              deps: {
                bridge: scriptedBridge({ screens: ['Home'] }),
                model,
                renderStill: fakeRenderStill(),
                castToVideo: async () => null,
              },
            },
            { runTuiAgentProof, agentUsable: true },
          )
          // The dispatcher softens the truncated leg into a neutral deferral.
          assert.equal(
            run.reasonCode,
            'tui-truncated',
            'truncated agent leg softens to tui-truncated',
          )
          assert.equal(run.hasFailure, false, 'a truncation is not a failure')
          assert.equal(
            model.calls,
            0,
            'a negative wall-clock budget must stop before any model call',
          )
          // Downstream: deferred + exit 0 (never exit-1 on time alone).
          const perSurface = classifySurfaceOutcomes([run])
          assert.deepEqual(perSurface, [
            { surface: 'tui', outcome: 'deferred', reasonCode: 'tui-truncated', error: null },
          ])
          assert.equal(aggregateExitCode(perSurface), 0)
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(localDir, { recursive: true, force: true })
    cleanupPr('tuiwallclock')
  }
})

test('runTuiWithReplayFallback: truncated agent leg + no scenario → SurfaceRun carries reasonCode tui-truncated, hasFailure false (BOS-354)', async () => {
  const { runTuiWithReplayFallback } = await import('./proof-tui-agent.mjs')
  // A minimal fake agent leg that reports a collect-mode SurfaceRun flagged
  // `truncated: true` (as the real leg does on a mid-flight wall-clock cutoff).
  const fakeAgentLeg = async () => ({
    surface: 'tui',
    captureShapes: [{ surface: 'tui', status: 'failed', error: 'cut off mid-flight' }],
    brief: { title: 'Rich 4-scene brief' },
    agentResult: {
      passed: false,
      summary: 'ran out of clock before finishing',
      evidence: [],
      steps: 3,
    },
    hasFailure: true,
    noSurface: false,
    scanTexts: ['partial evidence'],
    elapsedMs: 1234,
    reasonCode: null,
    truncated: true,
  })
  const run = await runTuiWithReplayFallback(
    {
      prNumber: '1',
      commit: 'abc1234',
      changedFiles: [], // no scenario → replay leg not attempted
      dryRun: true,
      runContext: { collect: true, runId: 'r', token: 't', maxWallClockMs: 60_000 },
    },
    { runTuiAgentProof: fakeAgentLeg, agentUsable: true },
  )
  assert.equal(run.reasonCode, 'tui-truncated')
  assert.equal(run.hasFailure, false)
  assert.equal(run.proofSource, null)
  assert.equal(run.surface, 'tui')
})

test("runTuiWithReplayFallback: threads the replayed scenario's terminal into the leg deps (BOS-571)", async () => {
  const { runTuiWithReplayFallback } = await import('./proof-tui-agent.mjs')
  const seen = []
  const fakeLeg = async (ctx) => {
    seen.push(ctx.deps)
    return {
      surface: 'tui',
      captureShapes: [],
      brief: {},
      agentResult: { passed: false, summary: '', evidence: [], steps: 0 },
      hasFailure: true,
      noSurface: false,
      elapsedMs: 0,
      reasonCode: null,
    }
  }
  const baseDeps = {
    runTuiAgentProof: fakeLeg,
    agentUsable: false, // keyless ⇒ replay-only leg
    runReplayLoop: async () => ({ agentResult: { passed: true }, finalScreen: '' }),
    synthesizeBrief: () => ({ title: 't', description: 'd' }),
    makeScenarioEvaluator: () => () => ({ passed: true }),
  }
  const ctx = {
    prNumber: '1',
    commit: 'abc1234',
    changedFiles: ['proof/scenarios/narrow.scenario.json'],
    dryRun: true,
    runContext: { collect: true, runId: 'r', token: 't', maxWallClockMs: 60_000 },
    deps: { renderStill: 'sentinel' },
  }

  // A scenario declaring a terminal forwards it alongside the caller's own deps.
  await runTuiWithReplayFallback(ctx, {
    ...baseDeps,
    loadScenario: () => ({ scenario: { title: 'narrow', terminal: { cols: 72 } } }),
  })
  assert.deepEqual(seen.at(-1), { renderStill: 'sentinel', terminal: { cols: 72 } })

  // A scenario WITHOUT a terminal leaves the deps object byte-identical.
  await runTuiWithReplayFallback(ctx, {
    ...baseDeps,
    loadScenario: () => ({ scenario: { title: 'wide' } }),
  })
  assert.deepEqual(seen.at(-1), { renderStill: 'sentinel' })
  assert.equal('terminal' in seen.at(-1), false, 'no terminal key is invented')
})

test("runTuiWithReplayFallback: threads the replayed scenario's fixture preset and env into the leg deps (BOS-976)", async () => {
  const { runTuiWithReplayFallback } = await import('./proof-tui-agent.mjs')
  const seen = []
  const fakeLeg = async (ctx) => {
    seen.push(ctx.deps)
    return {
      surface: 'tui',
      captureShapes: [],
      brief: {},
      agentResult: { passed: false, summary: '', evidence: [], steps: 0 },
      hasFailure: true,
      noSurface: false,
      elapsedMs: 0,
      reasonCode: null,
    }
  }
  const baseDeps = {
    runTuiAgentProof: fakeLeg,
    agentUsable: false, // keyless ⇒ replay-only leg
    runReplayLoop: async () => ({ agentResult: { passed: true }, finalScreen: '' }),
    synthesizeBrief: () => ({ title: 't', description: 'd' }),
    makeScenarioEvaluator: () => () => ({ passed: true }),
  }
  const ctx = {
    prNumber: '1',
    commit: 'abc1234',
    changedFiles: ['proof/scenarios/preset.scenario.json'],
    dryRun: true,
    runContext: { collect: true, runId: 'r', token: 't', maxWallClockMs: 60_000 },
    deps: { renderStill: 'sentinel' },
  }

  // The whole point: a scenario built on a purpose-made preset must replay against
  // THAT preset. Before this the leg saw no fixture at all and the bridge defaulted
  // to `demo`, so the scenario proved something about the wrong fixture.
  await runTuiWithReplayFallback(ctx, {
    ...baseDeps,
    loadScenario: () => ({
      scenario: {
        title: 'slow probe',
        fixture: { preset: 'slow-agent-probe', env: { BOSS_AUTH_E2E_EMAIL: 'a@b.c' } },
        terminal: { cols: 72 },
      },
    }),
  })
  assert.deepEqual(seen.at(-1), {
    renderStill: 'sentinel',
    fixture: 'slow-agent-probe',
    seedEnv: { BOSS_AUTH_E2E_EMAIL: 'a@b.c' },
    terminal: { cols: 72 },
  })

  // A scenario object carrying no fixture at all contributes nothing — the deps bag
  // is left byte-identical. This is the programmatic-caller shape; the PRODUCTION
  // shape is the case below, and the two must be kept distinct or the assertion
  // proves the wrong one.
  await runTuiWithReplayFallback(ctx, {
    ...baseDeps,
    loadScenario: () => ({ scenario: { title: 'plain' } }),
  })
  assert.deepEqual(seen.at(-1), { renderStill: 'sentinel' })

  // What loadScenario ACTUALLY hands back for a scenario declaring no fixture:
  // scripts/proof-scenario.mjs defaults it to `{preset: 'demo'}`. So on the real
  // path a pre-existing scenario contributes `fixture: 'demo'` and the spawn argv
  // gains `--fixture demo` where it previously had none. That is the Go flag's own
  // default (services/boss/cmd/proof-tui-agent/main.go), so the EFFECTIVE preset is
  // unchanged — but the argv is not, and asserting only the hand-built shape above
  // would leave that difference unmeasured.
  await runTuiWithReplayFallback(ctx, {
    ...baseDeps,
    loadScenario: () => ({ scenario: { title: 'plain', fixture: { preset: 'demo' } } }),
  })
  assert.deepEqual(seen.at(-1), { renderStill: 'sentinel', fixture: 'demo' })

  // An explicitly injected dep still wins over the scenario's declaration.
  await runTuiWithReplayFallback(
    { ...ctx, deps: { renderStill: 'sentinel', fixture: 'demo' } },
    {
      ...baseDeps,
      loadScenario: () => ({ scenario: { title: 'x', fixture: { preset: 'slow-agent-probe' } } }),
    },
  )
  assert.equal(seen.at(-1).fixture, 'demo')
})

// ── 4. zero-frame fallback ────────────────────────────────────────────────────

test('runTuiAgentProof: no stills captured → exactly one fallback still', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'No screenshots',
    description: 'Agent calls done immediately',
    expectedEvidence: ['Home'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-zeroframe' }),
        async () => {
          const renderStill = fakeRenderStill()
          const bridge = scriptedBridge({ screens: ['Home dashboard'] })
          // Model calls done immediately — no observe/send_keys/type_text → 0 screens.
          const model = scriptedModel([
            toolUse('done', { summary: 'Nothing to capture', passed: true }),
          ])
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuizeroframe',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: { bridge, model, renderStill, castToVideo: async () => null },
          })
          const cap = manifest.captures[0]
          assert.ok(Array.isArray(cap.stills), 'capture must have a stills array')
          assert.equal(cap.stills.length, 1, 'exactly one fallback still expected')
          assert.ok(bridge.observeCount >= 1, 'fallback must observe once for the still')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuizeroframe')
  }
})

// ── 4b. planRequiredProof soft steering ───────────────────────────────────────

test('runTuiAgentProof: planRequiredProof steers the goal but never fails the hard evidence gate', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  // Long plan prose that never appears verbatim on a TUI screen — the failure
  // mode this fix prevents (feeding such prose into the hard substring gate).
  const planProse = 'Test output — updated scripts/ suite green; this never renders on a TUI screen'
  const brief = {
    title: 'Plan-steered proof',
    description: 'Demonstrate the change',
    expectedEvidence: ['Ready'], // short on-screen token (model-derived)
    planRequiredProof: [planProse], // soft steering only
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-plansteer' }),
        async () => {
          let capturedGoal = null
          const responses = [
            toolUse('observe', {}),
            toolUse('done', { summary: 'done', passed: true }),
          ]
          let i = 0
          // Capturing model: records the goal (first user message) it receives.
          const model = {
            async createMessage({ messages }) {
              if (capturedGoal === null) capturedGoal = messages[0].content
              const resp = responses[Math.min(i, responses.length - 1)]
              if (i < responses.length - 1) i += 1
              return resp
            },
          }
          // The settled screen shows the expectedEvidence token but NOT the plan prose.
          const bridge = scriptedBridge({ screens: ['Ready to go'] })
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuiplansteer',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: { bridge, model, renderStill: fakeRenderStill(), castToVideo: async () => null },
          })

          // 1. Plan proof reaches the agent goal as SOFT guidance.
          assert.ok(
            capturedGoal.includes('steer your run to demonstrate it'),
            'goal must carry the plan-proof steering line',
          )
          assert.ok(capturedGoal.includes(planProse), 'goal must include the plan prose verbatim')

          // 2. The HARD evidence gate reads expectedEvidence only: the run passes
          //    even though the plan prose never appears on the final screen.
          const cap = manifest.captures[0]
          assert.equal(cap.status, 'passed', 'plan-prose absence must not fail the gate')
          assert.ok(
            !(cap.error && cap.error.includes('evidence gate failed')),
            'no evidence-gate failure from the unmatched plan prose',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuiplansteer')
  }
})

// ── 4c. BOS-221: generated path threads TUI framing + scenario anchors ─────────

test('runTuiAgentProof: generated path passes surface:tui + excludeLowSignal + TUI context + scenario anchors to the generator', async () => {
  const { runTuiAgentProof, TUI_CONTEXT_BLOCK } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const planProse = 'Plan requires the settings screen'
  const anchors = ['Open settings scenario', 'Settings', 'READY']

  try {
    // No BOSS_PROOF_BRIEF → resolveBrief takes the GENERATED path and calls the
    // injected generator spy + the injected loadScenarioAnchors fake.
    await withEnv(
      BASE_ENV({ BOSS_PROOF_BRIEF: undefined, BOSS_PROOF_RUN_ID: 'tui-gen-anchors' }),
      async () => {
        let seen = null
        const generateBriefFromDiff = async (args) => {
          seen = args
          return {
            title: 'Generated',
            description: 'A generated TUI brief',
            targetRoutes: [],
            stepsHints: [],
            expectedEvidence: ['Settings'],
          }
        }
        let anchorsChangedFiles = null
        const loadScenarioAnchors = async (changedFiles) => {
          anchorsChangedFiles = changedFiles
          return anchors
        }
        const bridge = scriptedBridge({ screens: ['Settings panel open'] })
        const model = scriptedModel([
          toolUse('observe', {}),
          toolUse('done', { summary: 'ok', passed: true }),
        ])

        await runTuiAgentProof({
          prNumber: 'tuigenanchors',
          commit: 'abc1234',
          changedFiles: ['services/boss/foo.go', 'docs/plans/BOS-999.md'],
          dryRun: true,
          planRequiredProof: [planProse],
          deps: {
            bridge,
            model,
            renderStill: fakeRenderStill(),
            castToVideo: async () => null,
            generateBriefFromDiff,
            loadScenarioAnchors,
          },
        })

        assert.ok(seen, 'the generator must be invoked on the generated path')
        assert.equal(seen.surface, 'tui', 'surface must be tui')
        assert.equal(seen.excludeLowSignal, true, 'excludeLowSignal must be true on the TUI path')
        assert.equal(seen.routes, TUI_CONTEXT_BLOCK, 'routes must be the TUI context block')
        assert.equal(seen.fixtures, TUI_CONTEXT_BLOCK, 'fixtures must be the TUI context block')
        assert.deepEqual(seen.planRequiredProof, [planProse], 'planRequiredProof must be forwarded')
        assert.deepEqual(seen.scenarioAnchors, anchors, 'scenario anchors must be forwarded')
        // The anchor seam is scoped by the PR's changed files.
        assert.deepEqual(anchorsChangedFiles, ['services/boss/foo.go', 'docs/plans/BOS-999.md'])
      },
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuigenanchors')
  }
})

// ── defaultLoadScenarioAnchors: change-scoped anchor derivation (BOS-221) ──────
//
// The default seam anchors the brief on the scenario(s) THIS PR ships, keyed off
// the normalized changed-files list (not a top-level directory scan). These
// tests write real scenario files under REPO_ROOT/proof/scenarios/ (the loader
// resolves relative to the module's repoRoot) in a unique temp subdir and clean
// up in finally.

function writeScenarioFile(relDir, name, scenario) {
  const absDir = path.join(REPO_ROOT, relDir)
  fs.mkdirSync(absDir, { recursive: true })
  const abs = path.join(absDir, name)
  fs.writeFileSync(abs, JSON.stringify(scenario, null, 2))
  return `${relDir}/${name}`
}

test('defaultLoadScenarioAnchors: anchors a NESTED committed scenario (title + expectedEvidence)', async () => {
  const { defaultLoadScenarioAnchors } = await import('./proof-tui-agent.mjs')
  const uniq = 'proof/scenarios/__anchor_test_nested__'
  try {
    const rel = writeScenarioFile(uniq, 'home.scenario.json', {
      version: 1,
      title: 'Home view renders',
      scenes: [
        {
          title: 'Open home',
          steps: [{ key: 'enter' }, { expect: 'READY' }, { expect: 'Sessions' }],
        },
      ],
    })
    // The changed path is nested (proof/scenarios/<subdir>/…). A non-recursive
    // readdir of proof/scenarios would have returned [] for this; the
    // change-scoped loader must still derive anchors.
    const anchors = defaultLoadScenarioAnchors([rel])
    assert.deepEqual(anchors, ['Home view renders', 'READY', 'Sessions'])
  } finally {
    fs.rmSync(path.join(REPO_ROOT, uniq), { recursive: true, force: true })
  }
})

test('defaultLoadScenarioAnchors: derives ONLY the changed scenario, never an unrelated sibling', async () => {
  const { defaultLoadScenarioAnchors } = await import('./proof-tui-agent.mjs')
  const uniq = 'proof/scenarios/__anchor_test_scoped__'
  try {
    const changedRel = writeScenarioFile(uniq, 'changed.scenario.json', {
      version: 1,
      title: 'Changed scenario',
      scenes: [{ title: 'Scene', steps: [{ expect: 'ChangedEvidence' }] }],
    })
    // A sibling scenario that this PR did NOT touch — its title/evidence must
    // never leak into the brief anchors.
    writeScenarioFile(uniq, 'unrelated.scenario.json', {
      version: 1,
      title: 'Unrelated scenario',
      scenes: [{ title: 'Scene', steps: [{ expect: 'UnrelatedEvidence' }] }],
    })
    const anchors = defaultLoadScenarioAnchors([changedRel, 'services/boss/foo.go'])
    assert.deepEqual(anchors, ['Changed scenario', 'ChangedEvidence'])
    assert.ok(!anchors.includes('Unrelated scenario'), 'unrelated title must not leak')
    assert.ok(!anchors.includes('UnrelatedEvidence'), 'unrelated evidence must not leak')
  } finally {
    fs.rmSync(path.join(REPO_ROOT, uniq), { recursive: true, force: true })
  }
})

test('defaultLoadScenarioAnchors: no committed scenario in the diff → [] (byte-identical brief)', async () => {
  const { defaultLoadScenarioAnchors } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(defaultLoadScenarioAnchors([]), [])
  assert.deepEqual(defaultLoadScenarioAnchors(['services/boss/foo.go', 'docs/plans/x.md']), [])
  // A scenario-shaped path OUTSIDE proof/scenarios/ does not count.
  assert.deepEqual(defaultLoadScenarioAnchors(['scripts/home.scenario.json']), [])
})

test('defaultLoadScenarioAnchors: an invalid changed scenario is skipped, not fatal', async () => {
  const { defaultLoadScenarioAnchors } = await import('./proof-tui-agent.mjs')
  const uniq = 'proof/scenarios/__anchor_test_invalid__'
  try {
    const absDir = path.join(REPO_ROOT, uniq)
    fs.mkdirSync(absDir, { recursive: true })
    fs.writeFileSync(path.join(absDir, 'broken.scenario.json'), '{ not valid json')
    const anchors = defaultLoadScenarioAnchors([`${uniq}/broken.scenario.json`])
    assert.deepEqual(anchors, [])
  } finally {
    fs.rmSync(path.join(REPO_ROOT, uniq), { recursive: true, force: true })
  }
})

// ── 5. cast→video stubbed ─────────────────────────────────────────────────────

test('runTuiAgentProof: castToVideo returns mp4 → capture mediaType mp4', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'With video',
    description: 'Produces a video',
    expectedEvidence: ['Ready'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-video' }),
        async () => {
          const ghDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-gh-'))
          const ghPath = path.join(ghDir, 'gh')
          fs.writeFileSync(
            ghPath,
            [
              '#!/usr/bin/env node',
              'const args = process.argv.slice(2);',
              "if (args.join(' ') === 'repo view --json name -q .name') {",
              "  console.log('bossanova');",
              '  process.exit(0);',
              '}',
              "if (args[0] === 'pr' && args[1] === 'view' && args[2] === '123') {",
              '  console.log(JSON.stringify({ number: 123, title: "Targeted PR title" }));',
              '  process.exit(0);',
              '}',
              'process.exit(2);',
            ].join('\n'),
          )
          fs.chmodSync(ghPath, 0o755)
          const originalPath = process.env.PATH
          process.env.PATH = `${ghDir}${path.delimiter}${originalPath ?? ''}`
          let castArgs = null
          const castToVideo = async (args) => {
            castArgs = args
            const { captureDir, captureId } = args
            const mp4Path = path.join(captureDir, `${captureId}.mp4`)
            const posterPath = path.join(captureDir, `${captureId}.png`)
            fs.writeFileSync(mp4Path, 'fake-mp4')
            fs.writeFileSync(posterPath, 'fake-poster')
            // A real render returns an output-clock timeline; the primary
            // ffmpeg-extraction path requires it (a null timeline is the
            // plain-mp4 fallback, covered by its own test below).
            return {
              mp4Path,
              posterPath,
              timeline: {
                trimMs: 0,
                segments: [{ startMs: 0, endMs: 5000, speed: 1 }],
                introMs: 1000,
              },
            }
          }
          // BOS-216: with an mp4 present, stills come from the video via the
          // extractStill seam (no Chromium). renderStill must never be called.
          const renderStill = fakeRenderStill()
          const extractStill = async ({ output }) => {
            fs.writeFileSync(output, 'fake-extracted-png')
            return true
          }
          try {
            const { manifest } = await runTuiAgentProof({
              prNumber: '123',
              commit: 'abc1234',
              changedFiles: [],
              dryRun: true,
              deps: {
                bridge: scriptedBridge({ screens: ['Ready to go'] }),
                model: scriptedModel([
                  toolUse('observe', {}),
                  toolUse('done', { summary: 'done', passed: true }),
                ]),
                renderStill,
                extractStill,
                castToVideo,
              },
            })
            const cap = manifest.captures[0]
            assert.equal(cap.mediaType, 'mp4')
            assert.equal(cap.status, 'passed')
            assert.ok(cap.videoUrl, 'video capture must expose a videoUrl')
            assert.ok(cap.stills.length >= 1, 'stills extracted from the mp4')
            assert.equal(
              renderStill.calls.length,
              0,
              'Chromium text-scrape renderer must be OFF the happy (mp4-present) path',
            )
            // Poster base is null now — the video (webm last frame) owns the
            // poster pixels, so no pre-rendered still is threaded into castToVideo.
            assert.equal(castArgs.posterBasePng, null)
            assert.equal(castArgs.keepWebm, true)
            assert.equal(castArgs.label, 'bossanova#123')
            assert.equal(castArgs.cardTitle, 'Targeted PR title')
          } finally {
            if (originalPath === undefined) delete process.env.PATH
            else process.env.PATH = originalPath
            fs.rmSync(ghDir, { recursive: true, force: true })
          }
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('123')
  }
})

// ── BOS-393: outroStartMs threading + outroDegraded surfacing ─────────────────

/**
 * Run runTuiAgentProof in collect mode with a loopRunner that returns a fixed
 * `outro`, recording the options handed to castToVideo. castToVideo writes a
 * fake mp4 (+ timeline) so the stills-only `degraded` channel stays null and we
 * can prove the outro path never rides it.
 */
async function runOutroThreadCollect(outro) {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  let recorded = null
  try {
    return await withEnv(
      BASE_ENV({ BOSS_PROOF_BRIEF: undefined, BOSS_PROOF_RUN_ID: 'tui-outrothread' }),
      async () => {
        const brief = {
          title: 'Outro thread',
          description: 'thread outro',
          scenes: [{ id: 'scene-01', title: 'S', expectedEvidence: ['READY'] }],
          budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
        }
        const loopRunner = async ({ rawDir }) => {
          fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'READY screen')
          return {
            agentResult: { passed: true, summary: 'ok', evidence: [], steps: 1 },
            finalScreen: 'READY screen',
            captionTimings: [{ seq: 1, caption: 'looking', startMs: 100 }],
            sceneTimings: [{ id: 'scene-01', title: 'S', startMs: 0 }],
            sceneForScreen: { 1: 'scene-01' },
            screens: [{ seq: 1, text: 'READY screen', castMs: 100 }],
            nullCastRead: false,
            outro,
          }
        }
        const castToVideo = async (args) => {
          recorded = args
          const { captureDir, captureId } = args
          const mp4Path = path.join(captureDir, `${captureId}.mp4`)
          const posterPath = path.join(captureDir, `${captureId}.png`)
          fs.writeFileSync(mp4Path, 'fake-mp4')
          fs.writeFileSync(posterPath, 'fake-poster')
          return {
            mp4Path,
            posterPath,
            timeline: { trimMs: 0, segments: [{ startMs: 0, endMs: 5000, speed: 1 }], introMs: 0 },
          }
        }
        const extractStill = async ({ output }) => {
          fs.writeFileSync(output, 'fake-extracted-png')
          return true
        }
        const surfaceRun = await runTuiAgentProof({
          prNumber: 'tuioutrothread',
          commit: 'abc1234',
          changedFiles: [],
          dryRun: true,
          brief,
          loopRunner,
          runContext: { collect: true, runId: 'tui-outrothread', token: 'tok-tui-test' },
          deps: {
            bridge: scriptedBridge({ screens: ['READY screen'] }),
            model: scriptedModel([toolUse('done', { passed: true, summary: 'ok' })]),
            renderStill: fakeRenderStill(),
            extractStill,
            castToVideo,
          },
        })
        return { surfaceRun, recorded }
      },
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuioutrothread')
  }
}

test('runTuiAgentProof threads outro.startMs to castToVideo as outroStartMs', async () => {
  const { recorded } = await runOutroThreadCollect({ startMs: 4200 })
  assert.equal(recorded.outroStartMs, 4200)
})

test('runTuiAgentProof passes null outroStartMs and surfaces outroDegraded on a degraded outro', async () => {
  const { surfaceRun, recorded } = await runOutroThreadCollect({
    degraded: { reason: 'outro-degraded', detail: 'x' },
  })
  assert.equal(recorded.outroStartMs, null)
  assert.deepEqual(surfaceRun.captureShapes[0].outroDegraded, {
    reason: 'outro-degraded',
    detail: 'x',
  })
  // The stills-only degraded channel must be untouched by an outro failure.
  assert.equal(surfaceRun.captureShapes[0].degraded ?? null, null)
})

test('runTuiAgentProof: castToVideo returns null → stills-only run still succeeds', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Stills only',
    description: 'No agg available',
    expectedEvidence: ['Ready'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-stills' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuistills',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null, // agg missing
            },
          })
          const cap = manifest.captures[0]
          assert.notEqual(cap.status, 'failed', 'missing video alone must not fail the run')
          assert.equal(manifest.verdict, 'passed')
          assert.notEqual(cap.mediaType, 'mp4')
          assert.ok(cap.stills.length >= 1, 'stills-only capture must still carry stills')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuistills')
  }
})

test('defaultCastToVideo: returns a structured degraded marker (no mp4) when the cast file is absent (BOS-216)', async () => {
  const { defaultCastToVideo } = await import('./proof-tui-agent.mjs')
  const warnings = []
  const originalWarn = console.warn
  console.warn = (message) => warnings.push(String(message))
  let out
  try {
    out = defaultCastToVideo({
      castPath: '/nonexistent/session.cast',
      captureDir: '/tmp',
      captureId: 'tui-agent',
    })
  } finally {
    console.warn = originalWarn
  }
  // BOS-216: null-media semantics for the caller (no mp4Path) but a
  // machine-readable reason so the degrade is diagnosable, not silent.
  assert.equal(out.mp4Path, undefined, 'no mp4 on the degraded path')
  assert.equal(out.degraded.reason, 'cast-missing')
  assert.ok(typeof out.degraded.detail === 'string' && out.degraded.detail.length > 0)
  assert.ok(
    warnings.some(
      (message) =>
        /DEGRADED/.test(message) && /cast/i.test(message) && /stills-only/i.test(message),
    ),
    'missing cast should warn loudly that the proof is falling back to stills-only',
  )
})

// ── BOS-216: ffmpeg-extracted stills, degraded marker, stills-only ack env ────

test('buildStillExtractArgs: -ss carries the output-clock ms, clamped to [0, videoDurationMs]', async () => {
  const { buildStillExtractArgs } = await import('./proof-tui-agent.mjs')
  // In-range value: exact-frame selection (`-frames:v 1`), -ss in seconds@ms.
  assert.deepEqual(
    buildStillExtractArgs({
      mp4Path: '/v.mp4',
      output: '/o.png',
      outputMs: 2000,
      videoDurationMs: 5000,
    }),
    [
      '-y',
      '-loglevel',
      'error',
      '-ss',
      '2.000',
      '-i',
      '/v.mp4',
      '-frames:v',
      '1',
      '-update',
      '1',
      '/o.png',
    ],
  )
  const ss = (args) => args[args.indexOf('-ss') + 1]
  // Past the trimmed end → clamps to the duration (never an empty PNG).
  assert.equal(
    ss(
      buildStillExtractArgs({
        mp4Path: '/v',
        output: '/o',
        outputMs: 99999,
        videoDurationMs: 5000,
      }),
    ),
    '5.000',
  )
  // Negative floors to 0.
  assert.equal(
    ss(
      buildStillExtractArgs({ mp4Path: '/v', output: '/o', outputMs: -10, videoDurationMs: 5000 }),
    ),
    '0.000',
  )
  // Non-finite (null cast read → mapSourceToOutputMs returned null) falls back to
  // the ceiling (last frame) when a duration is known, else 0.
  assert.equal(
    ss(
      buildStillExtractArgs({ mp4Path: '/v', output: '/o', outputMs: null, videoDurationMs: 5000 }),
    ),
    '5.000',
  )
  assert.equal(
    ss(
      buildStillExtractArgs({ mp4Path: '/v', output: '/o', outputMs: null, videoDurationMs: null }),
    ),
    '0.000',
  )
})

test('buildStillExtractArgs -ss equals mapSourceToOutputMs(timeline, castMs)/1000 (acceptance)', async () => {
  const { buildStillExtractArgs, timelineDurationMs } = await import('./proof-tui-agent.mjs')
  const { mapSourceToOutputMs } = await import('./proof-video.mjs')
  const timeline = { trimMs: 500, segments: [{ startMs: 0, endMs: 5000, speed: 1 }], introMs: 2000 }
  const castMs = 1500 // max(0,1500-500)=1000 through identity segment + introMs 2000 = 3000
  const outputMs = mapSourceToOutputMs(timeline, castMs)
  assert.equal(outputMs, 3000)
  const videoDurationMs = timelineDurationMs(timeline) // 2000 intro + 5000 retimed = 7000
  assert.equal(videoDurationMs, 7000)
  const args = buildStillExtractArgs({
    mp4Path: '/v.mp4',
    output: '/o.png',
    outputMs,
    videoDurationMs,
  })
  assert.equal(args[args.indexOf('-ss') + 1], (outputMs / 1000).toFixed(3))
})

test('extractStillsFromVideo: mirrors the renderFrames gallery shape and maps each screen through the timeline', async () => {
  const { extractStillsFromVideo } = await import('./proof-tui-agent.mjs')
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-extract-'))
  try {
    const seen = []
    const extractStill = async ({ output, outputMs }) => {
      seen.push({ output: path.basename(output), outputMs })
      fs.writeFileSync(output, 'png')
      return true
    }
    const timeline = {
      trimMs: 0,
      segments: [{ startMs: 0, endMs: 10000, speed: 1 }],
      introMs: 1000,
    }
    const stills = await extractStillsFromVideo({
      screens: [
        { seq: 1, castMs: 0 },
        { seq: 2, castMs: 4000 },
      ],
      sceneForScreen: { 1: 'scene-01', 2: 'scene-02' },
      timeline,
      mp4Path: '/v.mp4',
      captureDir,
      // Defines the last screen's mid-dwell window (BOS-355).
      endCutMs: 8000,
      extractStill,
      // Bright probe → nothing is dropped; the mid-dwell mapping is what's asserted.
      probeStillLuma: () => 128,
    })
    assert.deepEqual(stills, [
      {
        fileName: 'tui-agent/scene-01-frame-01.png',
        label: 'scene 01 frame 01',
        sceneId: 'scene-01',
      },
      {
        fileName: 'tui-agent/scene-02-frame-02.png',
        label: 'scene 02 frame 02',
        sceneId: 'scene-02',
      },
    ])
    // Mid-dwell sampling (BOS-355): screen 1 dwell [0,4000] → castMs+min(2000,1500)
    // =1500 → +introMs 1000 = 2500; screen 2 (last) dwell [4000,endCutMs 8000] →
    // 4000+min(2000,1500)=5500 → +introMs 1000 = 6500.
    assert.deepEqual(seen, [
      { output: 'scene-01-frame-01.png', outputMs: 2500 },
      { output: 'scene-02-frame-02.png', outputMs: 6500 },
    ])
  } finally {
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

// ── BOS-355: mid-dwell still sampling + near-black luma guard ─────────────────

test('stillSampleSourceMs: samples a settled mid-dwell frame, capped and clamped', async () => {
  const { stillSampleSourceMs, SETTLE_SAMPLE_CAP_MS, RETRY_SAMPLE_CAP_MS, FALLBACK_DWELL_MS } =
    await import('./proof-tui-agent.mjs')
  const screens = [{ castMs: 0 }, { castMs: 4000 }]
  // Interior screen → halfway to the next screen's castMs (dwell 4000 → +2000,
  // capped to SETTLE_SAMPLE_CAP_MS = 1500).
  assert.equal(stillSampleSourceMs({ screens, index: 0, endCutMs: 8000 }), 1500)
  // Last screen → dwell measured off endCutMs (dwell 4000 → +1500 cap).
  assert.equal(stillSampleSourceMs({ screens, index: 1, endCutMs: 8000 }), 5500)
  // Tiny dwell → half the dwell, strictly below the next screen.
  const tiny = [{ castMs: 100 }, { castMs: 110 }]
  const tinySample = stillSampleSourceMs({ screens: tiny, index: 0 })
  assert.equal(tinySample, 105)
  assert.ok(tinySample < 110, 'tiny-dwell sample must stay strictly below next')
  // Long dwell → capped exactly at the cap offset.
  const long = [{ castMs: 0 }, { castMs: 100_000 }]
  assert.equal(stillSampleSourceMs({ screens: long, index: 0 }), SETTLE_SAMPLE_CAP_MS)
  // The near-black retry pass (deeper fraction 0.75 + the larger RETRY_SAMPLE_CAP_MS)
  // samples a STRICTLY LATER timestamp than the primary (0.5, SETTLE_SAMPLE_CAP_MS)
  // even when the primary saturated its cap, so the retry re-samples a different
  // point in the dwell (BOS-355 follow-up).
  const primary = stillSampleSourceMs({ screens: long, index: 0, fraction: 0.5 })
  const retry = stillSampleSourceMs({
    screens: long,
    index: 0,
    fraction: 0.75,
    capMs: RETRY_SAMPLE_CAP_MS,
  })
  assert.equal(primary, SETTLE_SAMPLE_CAP_MS)
  assert.equal(retry, RETRY_SAMPLE_CAP_MS)
  assert.ok(retry > primary, 'the retry pass must sample later than the primary on a capped dwell')
  // Non-finite castMs → returned unchanged (preserves the null-cast degrade).
  assert.equal(stillSampleSourceMs({ screens: [{ castMs: null }], index: 0 }), null)
  assert.equal(stillSampleSourceMs({ screens: [{}], index: 0 }), undefined)
  // Degenerate next<=castMs (last screen, endCutMs === castMs) → FALLBACK_DWELL_MS.
  assert.equal(
    stillSampleSourceMs({ screens: [{ castMs: 5000 }], index: 0, endCutMs: 5000 }),
    5000 + Math.min(FALLBACK_DWELL_MS * 0.5, SETTLE_SAMPLE_CAP_MS),
  )
})

test('extractStillsFromVideo: injected probeStillLuma drops a near-black still, bright neighbours survive (BOS-355)', async () => {
  const { extractStillsFromVideo } = await import('./proof-tui-agent.mjs')
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-luma-'))
  try {
    const extractStill = async ({ output }) => {
      fs.writeFileSync(output, 'png')
      return true
    }
    // Screen 2's frame is near-black on both passes → dropped; 1 and 3 survive.
    const probeStillLuma = (p) => (path.basename(p) === 'scene-01-frame-02.png' ? 4 : 120)
    const stills = await extractStillsFromVideo({
      screens: [
        { seq: 1, text: 'A', castMs: 0 },
        { seq: 2, text: 'B', castMs: 2000 },
        { seq: 3, text: 'C', castMs: 4000 },
      ],
      sceneForScreen: { 1: 'scene-01', 2: 'scene-01', 3: 'scene-01' },
      timeline: { trimMs: 0, segments: [{ startMs: 0, endMs: 8000, speed: 1 }], introMs: 0 },
      mp4Path: '/tmp/fake.mp4',
      captureDir,
      endCutMs: 8000,
      extractStill,
      probeStillLuma,
    })
    assert.deepEqual(
      stills.map((s) => s.fileName),
      ['tui-agent/scene-01-frame-01.png', 'tui-agent/scene-01-frame-03.png'],
    )
    // Kept stills preserve the gallery shape.
    assert.deepEqual(stills[0], {
      fileName: 'tui-agent/scene-01-frame-01.png',
      label: 'scene 01 frame 01',
      sceneId: 'scene-01',
    })
  } finally {
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('extractStillsFromVideo: an all-near-black set keeps exactly the brightest still (floor, BOS-355)', async () => {
  const { extractStillsFromVideo } = await import('./proof-tui-agent.mjs')
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-floor-'))
  try {
    const extractStill = async ({ output }) => {
      fs.writeFileSync(output, 'png')
      return true
    }
    // Every still is near-black (<10); frame-02 is the brightest → the floor keeps it.
    const luma = {
      'scene-01-frame-01.png': 3,
      'scene-01-frame-02.png': 8,
      'scene-01-frame-03.png': 1,
    }
    const probeStillLuma = (p) => luma[path.basename(p)]
    const stills = await extractStillsFromVideo({
      screens: [
        { seq: 1, text: 'A', castMs: 0 },
        { seq: 2, text: 'B', castMs: 2000 },
        { seq: 3, text: 'C', castMs: 4000 },
      ],
      sceneForScreen: { 1: 'scene-01', 2: 'scene-01', 3: 'scene-01' },
      timeline: { trimMs: 0, segments: [{ startMs: 0, endMs: 8000, speed: 1 }], introMs: 0 },
      mp4Path: '/tmp/fake.mp4',
      captureDir,
      endCutMs: 8000,
      extractStill,
      probeStillLuma,
    })
    assert.equal(stills.length, 1, 'floor keeps exactly one still so the gallery never empties')
    assert.equal(
      stills[0].fileName,
      'tui-agent/scene-01-frame-02.png',
      'the brightest still is kept',
    )
  } finally {
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('extractStillsFromVideo: near-black at 0.5 but bright at 0.75 → the retry rescues the still (BOS-355)', async () => {
  const { extractStillsFromVideo } = await import('./proof-tui-agent.mjs')
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-rescue-'))
  try {
    const extractStill = async ({ output }) => {
      fs.writeFileSync(output, 'png')
      return true
    }
    // Single screen; probe returns near-black (4) on the first (0.5) pass, then a
    // bright value (120) on the re-probe after the 0.75 retry re-extract → the
    // still is rescued and kept, not dropped. Guards the retry-then-KEEP branch.
    let calls = 0
    const probeStillLuma = () => (++calls === 1 ? 4 : 120)
    const stills = await extractStillsFromVideo({
      screens: [{ seq: 1, text: 'A', castMs: 0 }],
      sceneForScreen: { 1: 'scene-01' },
      timeline: { trimMs: 0, segments: [{ startMs: 0, endMs: 8000, speed: 1 }], introMs: 0 },
      mp4Path: '/tmp/fake.mp4',
      captureDir,
      endCutMs: 8000,
      extractStill,
      probeStillLuma,
    })
    assert.equal(calls, 2, 'the near-black still is re-probed after the retry re-extract')
    assert.deepEqual(
      stills.map((s) => s.fileName),
      ['tui-agent/scene-01-frame-01.png'],
      'a still that goes bright on the retry is kept, not dropped',
    )
  } finally {
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('runTuiAgentProof: mp4 absent → Chromium renderFrames fallback runs, extractStill never does (seam) (BOS-216)', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const brief = {
    title: 'Fallback',
    description: 'No video available',
    expectedEvidence: ['Ready'],
  }
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-fallback-seam' }),
        async () => {
          const renderStill = fakeRenderStill()
          let extractCalls = 0
          const extractStill = async ({ output }) => {
            extractCalls += 1
            fs.writeFileSync(output, 'png')
            return true
          }
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuifallbackseam',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill,
              extractStill,
              castToVideo: async () => null, // mp4 absent → fallback path
            },
          })
          const cap = manifest.captures[0]
          assert.equal(extractCalls, 0, 'no mp4 → still extraction must NOT run')
          assert.ok(
            renderStill.calls.length >= 1,
            'fallback must use the Chromium text-scrape renderer',
          )
          assert.equal(
            cap.status,
            'passed',
            'stills-only fallback still passes (exit-code invariant)',
          )
          assert.ok(cap.stills.length >= 1)
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuifallbackseam')
  }
})

test('runTuiAgentProof: mp4 present but null timeline (plain-mp4 fallback) → Chromium renderFrames, extractStill never runs (BOS-216)', async () => {
  // Guards against extracting every still at the first frame: without a timeline
  // each screen's castMs maps to a null output ms, so the ffmpeg-extraction path
  // must be skipped in favour of the per-screen Chromium renderer (as `main`
  // does). Exit-code invariant: status stays `passed`.
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const brief = {
    title: 'Plain mp4',
    description: 'Video without a timeline',
    expectedEvidence: ['Ready'],
  }
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-plain-mp4' }),
        async () => {
          const renderStill = fakeRenderStill()
          let extractCalls = 0
          const extractStill = async ({ output }) => {
            extractCalls += 1
            fs.writeFileSync(output, 'png')
            return true
          }
          const castToVideo = async ({ captureDir, captureId }) => {
            const mp4Path = path.join(captureDir, `${captureId}.mp4`)
            fs.writeFileSync(mp4Path, 'fake-mp4')
            // Plain-mp4 fallback: an mp4 exists but post-processing produced no
            // timeline (finishVideo `post.ok === false`).
            return { mp4Path, timeline: null }
          }
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuiplainmp4',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill,
              extractStill,
              castToVideo,
            },
          })
          const cap = manifest.captures[0]
          assert.equal(extractCalls, 0, 'null timeline → ffmpeg still extraction must NOT run')
          assert.ok(
            renderStill.calls.length >= 1,
            'null-timeline plain mp4 must fall back to the per-screen Chromium renderer',
          )
          assert.equal(cap.mediaType, 'mp4', 'the plain mp4 is still surfaced as media')
          assert.equal(
            cap.status,
            'passed',
            'plain-mp4 fallback still passes (exit-code invariant)',
          )
          assert.ok(cap.stills.length >= 1)
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuiplainmp4')
  }
})

/**
 * Drive a degraded run and return the resulting capture + captured warnings.
 * `castToVideo` supplies the degraded/mp4 behavior under test. Pins the Epic-1
 * invariant: status/hasFailure must never flip on a degraded path.
 */
async function runDegraded({ runId, prNumber, castToVideo, env = {} }) {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const brief = { title: 'Degraded', description: 'Degraded path', expectedEvidence: ['Ready'] }
  const warnings = []
  const originalWarn = console.warn
  console.warn = (message) => warnings.push(String(message))
  let manifest
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: runId, ...env }),
        async () => {
          ;({ manifest } = await runTuiAgentProof({
            prNumber,
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo,
            },
          }))
        },
      ),
    )
  } finally {
    console.warn = originalWarn
  }
  return { cap: manifest.captures[0], verdict: manifest.verdict, warnings }
}

test('runTuiAgentProof: missing-agg degrade → loud marker, status/verdict unchanged from main (BOS-216)', async () => {
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  try {
    const { cap, verdict, warnings } = await runDegraded({
      runId: 'tui-degraded-agg',
      prNumber: 'tuidegradedagg',
      castToVideo: async () => ({ degraded: { reason: 'agg-missing', detail: 'agg not on PATH' } }),
    })
    // Warn-only through Epic 3: a missing agg must NOT fail the run.
    assert.equal(cap.status, 'passed')
    assert.equal(verdict, 'passed')
    assert.equal(cap.degraded.reason, 'agg-missing')
    assert.match(cap.degraded.detail, /agg not on PATH/)
    assert.ok(
      warnings.some((w) => /DEGRADED \(agg-missing\)/.test(w)),
      'a loud doctor-style degraded warning must name the missing prereq',
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuidegradedagg')
  }
})

test('runTuiAgentProof: missing-cast degrade → cast-missing marker, status unchanged (BOS-216)', async () => {
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  try {
    const { cap, verdict } = await runDegraded({
      runId: 'tui-degraded-cast',
      prNumber: 'tuidegradedcast',
      castToVideo: async () => ({
        degraded: { reason: 'cast-missing', detail: 'cast file missing' },
      }),
    })
    assert.equal(cap.status, 'passed')
    assert.equal(verdict, 'passed')
    assert.equal(cap.degraded.reason, 'cast-missing')
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuidegradedcast')
  }
})

test('runTuiAgentProof: null cast read → cast-unreadable marker + unlinked later chapter, scene-1 baseline unaffected, status unchanged (BOS-216)', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const brief = {
    title: 'Two scenes',
    description: 'prove it',
    scenes: [
      { id: 'scene-01', title: 'Home', expectedEvidence: ['Home'] },
      { id: 'scene-02', title: 'Settings', expectedEvidence: ['Settings'] },
    ],
  }
  const warnings = []
  const originalWarn = console.warn
  console.warn = (message) => warnings.push(String(message))
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-nullcast' }),
        async () => {
          // mp4 + timeline present, but no session.cast on disk → every readCastMs
          // returns null (nullCastRead). Scene-1's baseline startMs 0 is hardcoded,
          // not a cast read, so it stays linked; scene-2's null marker → unlinked.
          const castToVideo = async ({ captureDir, captureId }) => {
            const mp4Path = path.join(captureDir, `${captureId}.mp4`)
            fs.writeFileSync(mp4Path, 'fake-mp4')
            return {
              mp4Path,
              timeline: {
                trimMs: 0,
                segments: [{ startMs: 0, endMs: 5000, speed: 1 }],
                introMs: 2000,
              },
            }
          }
          const secondTurn = {
            stop_reason: 'tool_use',
            usage: { input_tokens: 10, output_tokens: 5 },
            content: [
              { type: 'tool_use', id: 'bs', name: 'begin_scene', input: { id: 'scene-02' } },
              { type: 'tool_use', id: 'sk', name: 'send_keys', input: { keys: ['s'] } },
            ],
          }
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuinullcast',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Home screen', 'Settings screen'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                secondTurn,
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              extractStill: async ({ output }) => {
                fs.writeFileSync(output, 'png')
                return true
              },
              castToVideo,
            },
          })
          const cap = manifest.captures[0]
          assert.equal(cap.status, 'passed', 'a null cast read must NOT fail the run')
          assert.equal(cap.degraded.reason, 'cast-unreadable')
          // Scene-1 baseline (startMs 0, hardcoded) stays linked at introMs; the
          // scene-2 marker read null → chapter renders explicitly unlinked.
          assert.equal(cap.scenes[0].outputMs, 2000)
          assert.equal(cap.scenes[1].outputMs, null)
          assert.ok(warnings.some((w) => /DEGRADED \(cast-unreadable\)/.test(w)))
        },
      ),
    )
  } finally {
    console.warn = originalWarn
    process.exitCode = originalExitCode
    cleanupPr('tuinullcast')
  }
})

test('runTuiAgentProof: BOSS_PROOF_TUI_ALLOW_STILLS_ONLY changes only the note, never the exit code (BOS-216)', async () => {
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const degrade = async () => ({ degraded: { reason: 'agg-missing', detail: 'agg not on PATH' } })
  try {
    const unset = await runDegraded({
      runId: 'tui-ack-unset',
      prNumber: 'tuiackunset',
      castToVideo: degrade,
    })
    const set = await runDegraded({
      runId: 'tui-ack-set',
      prNumber: 'tuiackset',
      castToVideo: degrade,
      env: { BOSS_PROOF_TUI_ALLOW_STILLS_ONLY: '1' },
    })
    // Exit code (status/verdict) identical with and without the ack.
    assert.equal(unset.cap.status, set.cap.status)
    assert.equal(unset.verdict, set.verdict)
    assert.equal(set.cap.status, 'passed')
    // The ack softens only the log wording.
    assert.ok(
      unset.warnings.some((w) => /set BOSS_PROOF_TUI_ALLOW_STILLS_ONLY to acknowledge/.test(w)),
    )
    assert.ok(set.warnings.some((w) => /acknowledged via BOSS_PROOF_TUI_ALLOW_STILLS_ONLY/.test(w)))
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuiackunset')
    cleanupPr('tuiackset')
  }
})

// ── 6. T1 evidence gate ───────────────────────────────────────────────────────

test('runTuiAgentProof: T1 gate — evidence present + done(passed) → verdict passed', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Gate pass',
    description: 'Evidence present',
    expectedEvidence: ['Session created'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-gate-pass' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuigatepass',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Session created OK'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'created', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'passed')
          assert.equal(manifest.captures[0].status, 'passed')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuigatepass')
  }
})

// ── 6b. evaluateSceneEvidence + journey-wide gate (P3c, BOS-140) ─────────────

test('evaluateSceneEvidence: evidence on an EARLY screen of its scene passes', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const scenes = [{ id: 'scene-01', title: 'Home', expectedEvidence: ['Ready'] }]
  const screens = [
    { seq: 1, text: 'Ready to go' },
    { seq: 2, text: 'Something else entirely' },
  ]
  const sceneForScreen = { 1: 'scene-01', 2: 'scene-01' }
  const result = evaluateSceneEvidence({ scenes, screens, sceneForScreen })
  assert.deepEqual(result, [
    { id: 'scene-01', title: 'Home', passed: true, missing: [], missingContext: [] },
  ])
})

test("evaluateSceneEvidence: evidence present only on a DIFFERENT scene's screen is missing (window isolation)", async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const scenes = [
    { id: 'scene-01', title: 'Home', expectedEvidence: ['Home ready'] },
    // scene-02's expected substring literally appears on screen 1 (scene-01's
    // window), never on a screen attributed to scene-02 itself.
    { id: 'scene-02', title: 'Settings', expectedEvidence: ['Settings saved'] },
  ]
  const screens = [
    { seq: 1, text: 'Home ready — Settings saved (leaked from a future screen)' },
    { seq: 2, text: 'Settings open, not yet confirmed' },
  ]
  const sceneForScreen = { 1: 'scene-01', 2: 'scene-02' }
  const result = evaluateSceneEvidence({ scenes, screens, sceneForScreen })
  // scene-01's own evidence appears in its own window → passed.
  assert.deepEqual(result[0], {
    id: 'scene-01',
    title: 'Home',
    passed: true,
    missing: [],
    missingContext: [],
  })
  // scene-02's evidence never appears in any screen attributed to scene-02 —
  // its presence on scene-01's screen does not count (window isolation).
  assert.equal(result[1].passed, false)
  assert.deepEqual(result[1].missing, ['Settings saved'])
  // scene-02's window is screen 2 ("Settings open, not yet confirmed"), which is
  // its final settled screen → the missing-context excerpt points at it.
  assert.deepEqual(result[1].missingContext, [
    { expectation: 'Settings saved', screen: 'Settings open, not yet confirmed' },
  ])
})

test('evaluateSceneEvidence: one failed + one passed scene are evaluated independently', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const scenes = [
    { id: 'scene-01', title: 'Home', expectedEvidence: ['Home ready'] },
    { id: 'scene-02', title: 'Settings', expectedEvidence: ['Settings saved'] },
  ]
  const screens = [
    { seq: 1, text: 'Home ready' },
    { seq: 2, text: 'Settings open' },
  ]
  const sceneForScreen = { 1: 'scene-01', 2: 'scene-02' }
  const result = evaluateSceneEvidence({ scenes, screens, sceneForScreen })
  assert.equal(result[0].passed, true)
  assert.equal(result[1].passed, false)
  assert.deepEqual(result[1].missing, ['Settings saved'])
})

test('evaluateSceneEvidence: a scene with zero captured screens fails with all evidence missing', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const scenes = [{ id: 'scene-02', title: 'Settings', expectedEvidence: ['Saved', 'Settings'] }]
  const screens = [{ seq: 1, text: 'Home ready' }]
  const sceneForScreen = { 1: 'scene-01' }
  const result = evaluateSceneEvidence({ scenes, screens, sceneForScreen })
  // Zero captured screens for this scene → all evidence missing, and the
  // screen-context is null (no settled screen to point at).
  assert.deepEqual(result, [
    {
      id: 'scene-02',
      title: 'Settings',
      passed: false,
      missing: ['Saved', 'Settings'],
      missingContext: [
        { expectation: 'Saved', screen: null },
        { expectation: 'Settings', screen: null },
      ],
    },
  ])
})

test('evaluateSceneEvidence: empty expectedEvidence passes trivially', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const scenes = [{ id: 'scene-01', title: 'Home', expectedEvidence: [] }]
  const result = evaluateSceneEvidence({ scenes, screens: [], sceneForScreen: {} })
  assert.deepEqual(result, [
    { id: 'scene-01', title: 'Home', passed: true, missing: [], missingContext: [] },
  ])
})

// ── 6b-bis. Shared-matcher adoption (BOS-222) ────────────────────────────────
// The gate now delegates per-string matching to proof-evidence-matcher.mjs.
// These pin the ADOPTION contract — defaults, opt-in modes, and the
// failure-context plumbing — not the matcher's own semantics (owned by BOS-218).

const evidenceScene = (expectedEvidence) => [{ id: 'scene-01', title: 'S', expectedEvidence }]
const oneScreen = (text) => ({ screens: [{ seq: 1, text }], sceneForScreen: { 1: 'scene-01' } })

test('evaluateSceneEvidence: plain-string default is normalized — collapses whitespace (pass)', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const result = evaluateSceneEvidence({
    scenes: evidenceScene(['Settings']),
    ...oneScreen('  Settings  '),
  })
  assert.equal(result[0].passed, true)
  assert.deepEqual(result[0].missing, [])
})

test('evaluateSceneEvidence: plain-string default is normalized — case-SENSITIVE (miss)', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const result = evaluateSceneEvidence({
    scenes: evidenceScene(['Settings']),
    ...oneScreen('settings'),
  })
  assert.equal(result[0].passed, false)
  assert.deepEqual(result[0].missing, ['Settings'])
})

test('evaluateSceneEvidence: normalized-ci opt-in matches case-insensitively (both directions)', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const lower = evaluateSceneEvidence({
    scenes: evidenceScene([{ text: 'settings', match: 'normalized-ci' }]),
    ...oneScreen('Settings'),
  })
  assert.equal(lower[0].passed, true)
  const upper = evaluateSceneEvidence({
    scenes: evidenceScene([{ text: 'SETTINGS', match: 'normalized-ci' }]),
    ...oneScreen('settings'),
  })
  assert.equal(upper[0].passed, true)
})

test('evaluateSceneEvidence: literal opt-in requires an exact byte match (whitespace/case mismatch fails)', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const exact = evaluateSceneEvidence({
    scenes: evidenceScene([{ text: 'Save now', match: 'literal' }]),
    ...oneScreen('press: Save now, please'),
  })
  assert.equal(exact[0].passed, true)
  const collapsed = evaluateSceneEvidence({
    scenes: evidenceScene([{ text: 'Save now', match: 'literal' }]),
    ...oneScreen('Save    now'),
  })
  assert.equal(collapsed[0].passed, false)
  assert.deepEqual(collapsed[0].missing, ['Save now'])
})

test('evaluateSceneEvidence: regex expression passes and fails correctly', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const hit = evaluateSceneEvidence({
    scenes: evidenceScene([{ text: 'v[0-9]+\\.[0-9]+', match: 'regex' }]),
    ...oneScreen('release v12.4 shipped'),
  })
  assert.equal(hit[0].passed, true)
  const miss = evaluateSceneEvidence({
    scenes: evidenceScene([{ text: 'v[0-9]+\\.[0-9]+', match: 'regex' }]),
    ...oneScreen('release vNext shipped'),
  })
  assert.equal(miss[0].passed, false)
})

test('evaluateSceneEvidence: anyOf passes when any variant label is present and fails when none are', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const exp = [{ anyOf: [{ text: 'Saved' }, { text: 'Updated' }], label: 'save confirmation' }]
  const hit = evaluateSceneEvidence({ scenes: evidenceScene(exp), ...oneScreen('Changes Updated') })
  assert.equal(hit[0].passed, true)
  const miss = evaluateSceneEvidence({ scenes: evidenceScene(exp), ...oneScreen('Nothing here') })
  assert.equal(miss[0].passed, false)
  // The display text of an anyOf falls back to the label.
  assert.deepEqual(miss[0].missing, ['save confirmation'])
})

test('evaluateSceneEvidence: literal-fails-but-normalized-passes regression (the false-negative shape)', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  // Wrapped/padded whitespace: the OLD t.includes(sub) literal loop failed this
  // even though the agent reached the screen. The normalized default passes it.
  const scenes = evidenceScene(['Session created'])
  const screens = oneScreen('Session\n   created  ✓')
  const normalized = evaluateSceneEvidence({ scenes, ...screens })
  assert.equal(normalized[0].passed, true)
  // Same expectation under an explicit literal opt-in still fails.
  const literal = evaluateSceneEvidence({
    scenes: evidenceScene([{ text: 'Session created', match: 'literal' }]),
    ...screens,
  })
  assert.equal(literal[0].passed, false)
})

test('evaluateSceneEvidence: a failed scene carries missingContext with the settled-screen excerpt', async () => {
  const { evaluateSceneEvidence } = await import('./proof-tui-agent.mjs')
  const result = evaluateSceneEvidence({
    scenes: evidenceScene(['Settings saved']),
    screens: [
      { seq: 1, text: 'Settings open' },
      { seq: 2, text: 'Settings open, not yet confirmed' },
    ],
    sceneForScreen: { 1: 'scene-01', 2: 'scene-01' },
  })
  assert.equal(result[0].passed, false)
  // The screen-context is the scene's FINAL settled screen (screen 2), not the
  // first — deliberately the last-seen state for debuggability.
  assert.deepEqual(result[0].missingContext, [
    { expectation: 'Settings saved', screen: 'Settings open, not yet confirmed' },
  ])
})

// This is the regression pin for the epic's limitation #5: under the OLD
// final-screen-only T1 gate (`finalScreen.includes(sub)`), evidence that
// appeared mid-run and then scrolled off screen before the run ended would
// fail the gate even though the run genuinely demonstrated it. The new
// journey-wide gate checks evidence against ANY settled screen captured while
// the (single, default) scene was active, so this run must now pass.
test('runTuiAgentProof: early evidence regression — evidence on an early screen leaves the final screen but still passes (any settled screen, not final-screen-only)', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Early evidence',
    description: 'Evidence appears then disappears before the run ends',
    expectedEvidence: ['Session created'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-early-evidence' }),
        async () => {
          // Screen 1 shows the evidence; screen 2 (the FINAL settled screen) does
          // not. Under the old gate this would fail; under P3c it must pass.
          const bridge = scriptedBridge({
            screens: ['Session created OK', 'Home screen, nothing about sessions here'],
          })
          const model = scriptedModel([
            toolUse('observe', {}),
            toolUse('observe', {}),
            toolUse('done', { summary: 'created then navigated home', passed: true }),
          ])

          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuiearlyevidence',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: { bridge, model, renderStill: fakeRenderStill(), castToVideo: async () => null },
          })

          assert.equal(
            manifest.verdict,
            'passed',
            'evidence on an earlier settled screen must pass',
          )
          const cap = manifest.captures[0]
          assert.equal(cap.status, 'passed')
          assert.equal(cap.scenes[0].passed, true)
          assert.deepEqual(cap.scenes[0].missing, [])
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuiearlyevidence')
  }
})

test('runTuiAgentProof: scene-02 evidence never appears → captureShape.scenes[1].passed is false and status is failed with the per-scene error string', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Two-scene journey',
    description: 'Home then Settings',
    scenes: [
      { id: 'scene-01', title: 'Home', expectedEvidence: ['Home ready'] },
      { id: 'scene-02', title: 'Settings', expectedEvidence: ['Settings saved'] },
    ],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-scene2-missing' }),
        async () => {
          // scene-01's screen shows its evidence; the begin_scene marker advances
          // to scene-02, whose screen never shows "Settings saved".
          const bridge = scriptedBridge({ screens: ['Home ready', 'Settings open, unsaved'] })
          const model = scriptedModel([
            toolUse('observe', {}),
            toolUse('begin_scene', { id: 'scene-02' }),
            toolUse('observe', {}),
            toolUse('done', { summary: 'toured both scenes', passed: true }),
          ])

          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuiscene2missing',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: { bridge, model, renderStill: fakeRenderStill(), castToVideo: async () => null },
          })

          assert.equal(manifest.verdict, 'failed')
          const cap = manifest.captures[0]
          assert.equal(cap.status, 'failed')
          assert.equal(cap.scenes[0].passed, true, 'scene-01 evidence was shown in its own window')
          assert.equal(cap.scenes[1].passed, false, 'scene-02 evidence never appeared')
          assert.deepEqual(cap.scenes[1].missing, ['Settings saved'])
          assert.match(cap.error, /evidence gate failed/)
          assert.match(cap.error, /scene-02 missing Settings saved/)
          // BOS-222: the settled-screen excerpt rides into the error string and
          // the serialized manifest (captureShape.scenes[].missingContext).
          assert.match(cap.error, /screen showed: Settings open, unsaved/)
          assert.deepEqual(cap.scenes[1].missingContext, [
            { expectation: 'Settings saved', screen: 'Settings open, unsaved' },
          ])
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuiscene2missing')
  }
})

// ── 6c. Chapter-timestamp mapping (P3b, BOS-140) ─────────────────────────────

test('runTuiAgentProof: castToVideo timeline → captureShape.scenes[].outputMs is populated', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Chapter mapping',
    description: 'castToVideo returns a timeline to map the scene marker through',
    expectedEvidence: ['Ready'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-chapter-map' }),
        async () => {
          const castToVideo = async ({ captureDir, captureId }) => {
            const mp4Path = path.join(captureDir, `${captureId}.mp4`)
            const posterPath = path.join(captureDir, `${captureId}.png`)
            fs.writeFileSync(mp4Path, 'fake-mp4')
            fs.writeFileSync(posterPath, 'fake-poster')
            return {
              mp4Path,
              posterPath,
              // trimMs 500 + a single identity segment + a 2s intro card. Scene 1
              // always starts at cast-clock 0 in this harness (no real .cast file
              // is written), so the mapped value is deterministic:
              // max(0, 0-500)=0 through the identity segment → introMs + 0 = 2000.
              timeline: {
                trimMs: 500,
                segments: [{ startMs: 0, endMs: 5000, speed: 1 }],
                introMs: 2000,
              },
            }
          }

          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuichaptermap',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo,
            },
          })

          const cap = manifest.captures[0]
          assert.equal(cap.scenes[0].outputMs, 2000)
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuichaptermap')
  }
})

test('runTuiAgentProof: castToVideo returns null (stills-only) → captureShape.scenes[].outputMs is null', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'No video, no chapter link',
    description: 'A stills-only proof must not throw mapping a null timeline',
    expectedEvidence: ['Ready'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-chapter-null' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuichapternull',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null, // agg missing — stills-only
            },
          })

          const cap = manifest.captures[0]
          assert.equal(cap.scenes[0].outputMs, null)
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuichapternull')
  }
})

// ── 7. makeStdioBridge unit tests (real pipes, fake binary) ──────────────────

test('bridgeSpawnArgs: includes only cast path when bossBin is absent', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast' }), [
    '--cast',
    '/tmp/session.cast',
  ])
})

test('bridgeSpawnArgs: forwards boss binary when bossBin is set', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast', bossBin: '/tmp/boss-e2e' }), [
    '--cast',
    '/tmp/session.cast',
    '--boss-bin',
    '/tmp/boss-e2e',
  ])
})

test('bridgeSpawnArgs: appends fixture and seed only when provided', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  // fixture + seed forwarded after the existing cast/boss-bin args.
  assert.deepEqual(
    bridgeSpawnArgs({
      castPath: '/tmp/session.cast',
      bossBin: '/tmp/boss-e2e',
      fixture: 'busy',
      seedPath: '/tmp/seed.json',
    }),
    [
      '--cast',
      '/tmp/session.cast',
      '--boss-bin',
      '/tmp/boss-e2e',
      '--fixture',
      'busy',
      '--seed',
      '/tmp/seed.json',
    ],
  )
  // fixture without seed: only --fixture appended.
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast', fixture: 'empty' }), [
    '--cast',
    '/tmp/session.cast',
    '--fixture',
    'empty',
  ])
  // seed without fixture: only --seed appended.
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast', seedPath: '/tmp/s.json' }), [
    '--cast',
    '/tmp/session.cast',
    '--seed',
    '/tmp/s.json',
  ])
})

// ── BOS-571: scenario-selected terminal size → --width/--height ───────────────

test('bridgeSpawnArgs: default argv is byte-identical when no terminal is supplied (BOS-571)', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  // The regression guard the ticket pins: adding the terminal knob must not
  // change what every existing scenario spawns today.
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast' }), [
    '--cast',
    '/tmp/session.cast',
  ])
})

test('bridgeSpawnArgs: appends --width/--height after the existing args when a terminal is supplied (BOS-571)', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(
    bridgeSpawnArgs({
      castPath: '/tmp/session.cast',
      bossBin: '/tmp/boss-e2e',
      fixture: 'demo',
      seedPath: '/tmp/seed.json',
      terminal: { cols: 72, rows: 30 },
    }),
    [
      '--cast',
      '/tmp/session.cast',
      '--boss-bin',
      '/tmp/boss-e2e',
      '--fixture',
      'demo',
      '--seed',
      '/tmp/seed.json',
      '--width',
      '72',
      '--height',
      '30',
    ],
  )
})

test('bridgeSpawnArgs: each terminal member is independently optional (BOS-571)', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  // cols only → --width, no --height (the Go -height default 36 applies).
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast', terminal: { cols: 72 } }), [
    '--cast',
    '/tmp/session.cast',
    '--width',
    '72',
  ])
  // rows only → --height, no --width (the Go -width default 140 applies).
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast', terminal: { rows: 30 } }), [
    '--cast',
    '/tmp/session.cast',
    '--height',
    '30',
  ])
})

test('bridgeSpawnArgs: a malformed or empty terminal leaves the argv unchanged (BOS-571)', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  const base = ['--cast', '/tmp/session.cast']
  for (const terminal of [
    {},
    null,
    undefined,
    'wide',
    72,
    [],
    { cols: null, rows: null },
    { cols: '72', rows: '30' },
    { cols: 72.5 },
    { cols: Number.NaN, rows: Infinity },
    { widthPx: 900 },
  ]) {
    assert.deepEqual(
      bridgeSpawnArgs({ castPath: '/tmp/session.cast', terminal }),
      base,
      `terminal ${JSON.stringify(terminal) ?? String(terminal)} must not change the argv`,
    )
  }
})

test('settleExtra: forwards valid integer settle overrides', async () => {
  const { settleExtra } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(settleExtra({ settleMs: 120, hardCapMs: 900 }), {
    settleMs: 120,
    hardCapMs: 900,
  })
})

test('settleExtra: absent overrides ⇒ empty object (default behavior)', async () => {
  const { settleExtra } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(settleExtra(), {})
  assert.deepEqual(settleExtra({}), {})
})

test('settleExtra: drops fractional overrides (json.Unmarshal into int64 would fail)', async () => {
  const { settleExtra } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(settleExtra({ settleMs: 10.5, hardCapMs: 900.25 }), {})
  // A valid sibling still passes while the fractional one is dropped.
  assert.deepEqual(settleExtra({ settleMs: 10.5, hardCapMs: 900 }), { hardCapMs: 900 })
})

test('settleExtra: drops oversized integers that overflow int64', async () => {
  const { settleExtra } = await import('./proof-tui-agent.mjs')
  // Number.isInteger(1e30) is true, but 1e30 overflows int64 and would fail
  // json.Unmarshal on the Go bridge before resolveSettle could clamp it.
  assert.deepEqual(settleExtra({ settleMs: 1e30, hardCapMs: 1e30 }), {})
  assert.deepEqual(settleExtra({ settleMs: Number.MAX_VALUE }), {})
  // Just past the safe-integer ceiling is also dropped; the valid sibling stays.
  assert.deepEqual(settleExtra({ settleMs: Number.MAX_SAFE_INTEGER + 1, hardCapMs: 900 }), {
    hardCapMs: 900,
  })
})

test('settleExtra: drops non-finite overrides', async () => {
  const { settleExtra } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(settleExtra({ settleMs: Infinity, hardCapMs: Number.NaN }), {})
})

// Write a tiny fake-bridge executable once; all bridge tests share it.
// It reads NDJSON lines from stdin, responds with { id, ok, screen }, and exits
// on op=quit. Setting FAKE_BRIDGE_EXIT_AFTER=N causes it to exit (code 0) after
// N successful responses, without responding to the N+1-th request.
const _fakeBridgeDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-bridge-'))
const FAKE_BRIDGE_BIN = path.join(_fakeBridgeDir, 'fake-bridge.js')
fs.writeFileSync(
  FAKE_BRIDGE_BIN,
  [
    '#!/usr/bin/env node',
    "'use strict';",
    "const readline = require('node:readline');",
    'const rl = readline.createInterface({ input: process.stdin, terminal: false });',
    'const exitAfter = process.env.FAKE_BRIDGE_EXIT_AFTER !== undefined',
    '  ? Number(process.env.FAKE_BRIDGE_EXIT_AFTER) : Infinity;',
    "const baseScreen = process.env.FAKE_BRIDGE_SCREEN || 'FAKE SCREEN';",
    // BOS-571: when FAKE_BRIDGE_ARGV_OUT is set, record the flags the spawner
    // actually passed so a test can assert the real spawn argv. Unset ⇒ no-op.
    'if (process.env.FAKE_BRIDGE_ARGV_OUT) {',
    "  require('node:fs').writeFileSync(process.env.FAKE_BRIDGE_ARGV_OUT, JSON.stringify(process.argv.slice(2)));",
    '}',
    'let count = 0;',
    "rl.on('line', (line) => {",
    '  const t = line.trim();',
    '  if (!t) return;',
    '  let msg;',
    '  try { msg = JSON.parse(t); } catch { return; }',
    '  if (count >= exitAfter) { process.exit(0); }',
    '  count++;',
    "  process.stdout.write(JSON.stringify({ id: msg.id, ok: true, screen: baseScreen + ' ' + count }) + '\\n');",
    "  if (msg.op === 'quit') { process.exit(0); }",
    '});',
  ].join('\n'),
)
fs.chmodSync(FAKE_BRIDGE_BIN, 0o755)
process.on('exit', () => {
  try {
    fs.rmSync(_fakeBridgeDir, { recursive: true, force: true })
  } catch {
    // ignore cleanup errors
  }
})

/** Save/restore env keys around a bridge test. */
function withBridgeEnv(overrides, fn) {
  const KEYS = [
    'BOSS_PROOF_TUI_BRIDGE_BIN',
    'FAKE_BRIDGE_EXIT_AFTER',
    'FAKE_BRIDGE_SCREEN',
    'FAKE_BRIDGE_ARGV_OUT',
  ]
  const saved = {}
  for (const k of KEYS) saved[k] = process.env[k]
  for (const [k, v] of Object.entries(overrides)) {
    if (v === undefined) delete process.env[k]
    else process.env[k] = String(v)
  }
  return Promise.resolve()
    .then(fn)
    .finally(() => {
      for (const k of KEYS) {
        if (saved[k] === undefined) delete process.env[k]
        else process.env[k] = saved[k]
      }
    })
}

/** Create throw-away localDir + rawDir under os.tmpdir(). Returns { localDir, rawDir, cleanup }. */
function makeBridgeDirs(label) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), `bridge-${label}-`))
  const localDir = path.join(root, 'local')
  const rawDir = path.join(root, 'raw')
  fs.mkdirSync(localDir, { recursive: true })
  fs.mkdirSync(rawDir, { recursive: true })
  return { localDir, rawDir, cleanup: () => fs.rmSync(root, { recursive: true, force: true }) }
}

test('makeStdioBridge: forwards a scenario terminal size to the spawned bridge argv (BOS-571)', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('terminal')
  const argvOut = path.join(localDir, 'argv.json')
  try {
    await withBridgeEnv(
      { BOSS_PROOF_TUI_BRIDGE_BIN: FAKE_BRIDGE_BIN, FAKE_BRIDGE_ARGV_OUT: argvOut },
      async () => {
        const bridge = makeStdioBridge({ localDir, rawDir, terminal: { cols: 72, rows: 30 } })
        try {
          await bridge.observe()
          const argv = JSON.parse(fs.readFileSync(argvOut, 'utf8'))
          assert.deepEqual(argv.slice(-4), ['--width', '72', '--height', '30'])
          assert.deepEqual(argv.slice(0, 2), ['--cast', path.join(rawDir, 'session.cast')])
        } finally {
          try {
            await bridge.quit()
          } catch {
            // ignore
          }
        }
      },
    )
  } finally {
    cleanup()
  }
})

test('makeStdioBridge: omits --width/--height when no terminal is supplied (BOS-571)', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('noterminal')
  const argvOut = path.join(localDir, 'argv.json')
  try {
    await withBridgeEnv(
      { BOSS_PROOF_TUI_BRIDGE_BIN: FAKE_BRIDGE_BIN, FAKE_BRIDGE_ARGV_OUT: argvOut },
      async () => {
        const bridge = makeStdioBridge({ localDir, rawDir })
        try {
          await bridge.observe()
          const argv = JSON.parse(fs.readFileSync(argvOut, 'utf8'))
          assert.equal(argv.includes('--width'), false, 'no --width without a terminal')
          assert.equal(argv.includes('--height'), false, 'no --height without a terminal')
        } finally {
          try {
            await bridge.quit()
          } catch {
            // ignore
          }
        }
      },
    )
  } finally {
    cleanup()
  }
})

test('makeStdioBridge: observe/sendKeys/typeText round-trip returns expected screen', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('roundtrip')
  try {
    await withBridgeEnv({ BOSS_PROOF_TUI_BRIDGE_BIN: FAKE_BRIDGE_BIN }, async () => {
      const bridge = makeStdioBridge({ localDir, rawDir })
      try {
        const r1 = await bridge.observe()
        const r2 = await bridge.sendKeys(['s'])
        const r3 = await bridge.typeText('hello')

        assert.ok(
          typeof r1.screen === 'string' && r1.screen.includes('FAKE SCREEN'),
          'observe returns screen',
        )
        assert.ok(
          typeof r2.screen === 'string' && r2.screen.includes('FAKE SCREEN'),
          'sendKeys returns screen',
        )
        assert.ok(
          typeof r3.screen === 'string' && r3.screen.includes('FAKE SCREEN'),
          'typeText returns screen',
        )
        // Responses arrive in order: count increments 1, 2, 3
        assert.ok(r1.screen !== r2.screen, 'screens must differ (monotonic counter)')
        assert.ok(r2.screen !== r3.screen, 'screens must differ (monotonic counter)')
      } finally {
        try {
          await bridge.quit()
        } catch {
          // best-effort
        }
      }
    })
  } finally {
    cleanup()
  }
})

// Echo fake-bridge (BOS-219): reflects each received NDJSON request back in the
// `screen` field so a serialization test can assert the exact op + params the JS
// bridge emits for enter/esc/key/type/daemon/capabilities.
const ECHO_BRIDGE_BIN = path.join(_fakeBridgeDir, 'echo-bridge.js')
fs.writeFileSync(
  ECHO_BRIDGE_BIN,
  [
    '#!/usr/bin/env node',
    "'use strict';",
    "const readline = require('node:readline');",
    'const rl = readline.createInterface({ input: process.stdin, terminal: false });',
    "rl.on('line', (line) => {",
    '  const t = line.trim();',
    '  if (!t) return;',
    '  let msg;',
    '  try { msg = JSON.parse(t); } catch { return; }',
    "  process.stdout.write(JSON.stringify({ id: msg.id, ok: true, screen: t }) + '\\n');",
    "  if (msg.op === 'quit') { process.exit(0); }",
    '});',
  ].join('\n'),
)
fs.chmodSync(ECHO_BRIDGE_BIN, 0o755)

test('makeStdioBridge: enter/esc/key/type/daemon/capabilities emit the correct NDJSON ops', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('serialize')
  try {
    await withBridgeEnv({ BOSS_PROOF_TUI_BRIDGE_BIN: ECHO_BRIDGE_BIN }, async () => {
      const bridge = makeStdioBridge({ localDir, rawDir })
      // The returned object must expose the replay-loop op set.
      for (const m of ['enter', 'esc', 'key', 'type', 'daemon', 'capabilities']) {
        assert.equal(typeof bridge[m], 'function', `bridge.${m} must be a function`)
      }
      try {
        const sent = async (p) => JSON.parse((await p).screen)
        assert.equal((await sent(bridge.enter())).op, 'enter')
        assert.equal((await sent(bridge.esc())).op, 'esc')
        // key(name) → op 'key' with the single name wrapped as keys:[name].
        const keyReq = await sent(bridge.key('down'))
        assert.equal(keyReq.op, 'key')
        assert.deepEqual(keyReq.keys, ['down'])
        const typeReq = await sent(bridge.type('hi'))
        assert.equal(typeReq.op, 'type')
        assert.equal(typeReq.text, 'hi')
        // daemon(params) → op 'daemon' with the scenario params forwarded verbatim.
        const daemonReq = await sent(bridge.daemon({ action: 'add_session', sessionId: 's1' }))
        assert.equal(daemonReq.op, 'daemon')
        assert.equal(daemonReq.action, 'add_session')
        assert.equal(daemonReq.sessionId, 's1')
        assert.equal((await sent(bridge.capabilities())).op, 'capabilities')
      } finally {
        try {
          await bridge.quit()
        } catch {
          // best-effort
        }
      }
    })
  } finally {
    cleanup()
  }
})

test('makeStdioBridge: multiple requests complete in order with correct id dispatch', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('ordering')
  try {
    await withBridgeEnv({ BOSS_PROOF_TUI_BRIDGE_BIN: FAKE_BRIDGE_BIN }, async () => {
      const bridge = makeStdioBridge({ localDir, rawDir })
      try {
        // Fire 4 requests sequentially (bridge serialises anyway); each must
        // resolve with a distinct screen, proving id-based dispatch is correct.
        const screens = []
        for (let i = 0; i < 4; i++) {
          // eslint-disable-next-line no-await-in-loop
          const r = await bridge.observe()
          screens.push(r.screen)
        }
        assert.equal(screens.length, 4)
        // All screens contain the fixture marker
        assert.ok(
          screens.every((s) => s.includes('FAKE SCREEN')),
          'all responses must carry a screen',
        )
        // Each response is unique (counter increments per request)
        const unique = new Set(screens)
        assert.equal(unique.size, 4, 'each response must be distinct (monotonic ids)')
      } finally {
        try {
          await bridge.quit()
        } catch {
          // best-effort
        }
      }
    })
  } finally {
    cleanup()
  }
})

test('makeStdioBridge: bridge exit/closed-pipe fails pending waiters promptly (not after 30s)', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('crash')
  try {
    await withBridgeEnv(
      { BOSS_PROOF_TUI_BRIDGE_BIN: FAKE_BRIDGE_BIN, FAKE_BRIDGE_EXIT_AFTER: '0' },
      async () => {
        const bridge = makeStdioBridge({ localDir, rawDir })

        // First request: bridge exits before responding → must reject promptly
        await assert.rejects(bridge.observe(), (err) => {
          assert.match(err.message, /bridge/i, 'error must identify the bridge failure')
          return true
        })

        // Second request: crashed is latched → also rejects immediately
        await assert.rejects(bridge.observe(), /bridge/i)
      },
    )
  } finally {
    cleanup()
  }
})

test('makeStdioBridge: missing binary throws bridge-not-found error synchronously', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('nobin')
  try {
    await withBridgeEnv({ BOSS_PROOF_TUI_BRIDGE_BIN: undefined }, () => {
      assert.throws(
        () => makeStdioBridge({ localDir, rawDir }),
        /bridge binary not found/i,
        'constructor must throw immediately when binary is absent',
      )
    })
  } finally {
    cleanup()
  }
})

test('runTuiAgentProof: T1 gate — evidence ABSENT + done(passed) → verdict failed', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Gate fail',
    description: 'Evidence missing',
    expectedEvidence: ['Session created'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-gate-fail' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuigatefail',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              // Screen never shows the expected evidence substring.
              bridge: scriptedBridge({ screens: ['Some other screen'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'claims success', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(
            manifest.verdict,
            'failed',
            'model done(passed=true) must be overridden when evidence is absent',
          )
          assert.equal(manifest.captures[0].status, 'failed')
          assert.match(manifest.captures[0].error, /evidence/i)
          // BOS-223: the legacy recipe-floor hook is retired — a degraded agent run
          // carries ONLY the agent's own captureShape, never a synthetic floor.
          assert.equal(manifest.captures.length, 1, 'no recipe-floor captureShape is appended')
          assert.equal(
            manifest.captures.every((c) => c.recipeId === 'tui-agent'),
            true,
            'the sole capture is the agent’s own, not a recipe floor',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuigatefail')
  }
})

test('runTuiAgentProof: no recipe floor — degraded agent defers with only agent captures (Q1)', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'No floor',
    description: 'Evidence missing, no recipe fallback',
    expectedEvidence: ['Session created'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-no-floor' }),
        async () => {
          // BOS-223: the recipe-floor hook is retired, so the TUI surface can no
          // longer fall back to recipe media. A degraded agent run yields only the
          // agent's own capture(s) and defers to a neutral comment.
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuinofloor',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Some other screen'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'claims success', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'failed', 'absent evidence overrides done(passed=true)')
          assert.equal(
            manifest.deferred,
            true,
            'a degraded TUI run defers to a neutral comment (no gallery)',
          )
          assert.equal(
            manifest.captures.some((c) => c.recipeId === 'tui-home'),
            false,
            'TUI surface must NOT include any recipe-floor capture',
          )
          assert.equal(
            manifest.captures.every((c) => c.surface === 'tui' && c.recipeId === 'tui-agent'),
            true,
            'manifest carries only the agent’s own captures, no recipe floor',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuinofloor')
  }
})

// ── Task 3: TUI SDK abort guard ───────────────────────────────────────────────

test('createSdkModel: aborted SDK call yields degraded result (not a throw)', async () => {
  const { createSdkModel } = await import('./proof-tui-agent.mjs')
  const origTimeout = process.env.BOSS_PROOF_AGENT_TIMEOUT_MS
  // Abort after 50ms so the test is fast.
  process.env.BOSS_PROOF_AGENT_TIMEOUT_MS = '50'

  try {
    // Mock Anthropic client whose create() hangs forever but respects the abort signal.
    class HangingAnthropic {
      constructor() {
        this.messages = {
          // `signal` is passed as the second (request-options) arg, mirroring the
          // real SDK contract — not as a body param.
          create: (_body, { signal } = {}) =>
            new Promise((_, reject) => {
              if (signal?.aborted) {
                reject(new Error('aborted'))
                return
              }
              signal?.addEventListener('abort', () => reject(new Error('aborted')))
            }),
        }
      }
    }

    const model = createSdkModel({ importer: async () => ({ default: HangingAnthropic }) })
    const result = await model.createMessage({
      model: 'claude-sonnet-4-6',
      system: 'test',
      tools: [],
      messages: [{ role: 'user', content: 'hi' }],
      maxTokens: 100,
    })

    assert.equal(result.stop_reason, 'end_turn', 'aborted call must return degraded stop_reason')
    assert.deepEqual(result.content, [], 'aborted call must return empty content')
  } finally {
    if (origTimeout === undefined) delete process.env.BOSS_PROOF_AGENT_TIMEOUT_MS
    else process.env.BOSS_PROOF_AGENT_TIMEOUT_MS = origTimeout
  }
})

test('createSdkModel: forwards PROOF_ANTHROPIC_API_KEY and passes signal as a request option', async () => {
  const { createSdkModel } = await import('./proof-tui-agent.mjs')
  const origKey = process.env.PROOF_ANTHROPIC_API_KEY
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-proof-test-key'

  try {
    let ctorOpts = null
    let createArgs = null
    class RecordingAnthropic {
      constructor(opts) {
        ctorOpts = opts
        this.messages = {
          create: (body, options) => {
            createArgs = { body, options }
            return Promise.resolve({
              stop_reason: 'end_turn',
              content: [],
              usage: { input_tokens: 1, output_tokens: 1 },
            })
          },
        }
      }
    }

    const model = createSdkModel({ importer: async () => ({ default: RecordingAnthropic }) })
    await model.createMessage({
      model: 'claude-sonnet-4-6',
      system: 'test',
      tools: [],
      messages: [{ role: 'user', content: 'hi' }],
      maxTokens: 100,
    })

    // Bug 1: the proof key must reach the SDK constructor, not the implicit lookup.
    assert.equal(ctorOpts?.apiKey, 'sk-proof-test-key', 'apiKey must be forwarded to the client')
    // Bug 2: signal belongs in the request-options arg, never in the body.
    assert.ok(createArgs, 'create() must have been called')
    assert.equal(createArgs.body.signal, undefined, 'signal must not appear in the request body')
    assert.ok(createArgs.options?.signal instanceof AbortSignal, 'signal must be a request option')
  } finally {
    if (origKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = origKey
  }
})

test('buildRenderArgs forwards the caption to proof-render-terminal', async () => {
  const { buildRenderArgs } = await import('./proof-tui-agent.mjs')
  const args = buildRenderArgs({
    outPath: '/tmp/f.png',
    title: 'boss',
    caption: 'Opening cron list',
  })
  assert.ok(args.includes('--caption'))
  assert.ok(args.includes('Opening cron list'))
})

test('buildRenderArgs omits --caption when narration is empty', async () => {
  const { buildRenderArgs } = await import('./proof-tui-agent.mjs')
  const args = buildRenderArgs({ outPath: '/tmp/f.png', title: 'boss', caption: '' })
  assert.ok(!args.includes('--caption'))
})

test('renderFrames reads the sidecar caption-NN.txt and forwards it to renderStill', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'a TUI screen')
    fs.writeFileSync(path.join(rawDir, 'caption-01.txt'), 'Opening cron list')
    const seen = []
    const renderStill = async ({ output, caption }) => {
      seen.push({ caption })
      fs.writeFileSync(output, 'fake-png')
    }
    await renderFrames({ rawDir, captureDir, title: 'boss', renderStill })
    assert.equal(seen.length, 1)
    assert.equal(seen[0].caption, 'Opening cron list')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('renderFrames batches all frames through renderStills in one call (single browser)', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'screen one')
    fs.writeFileSync(path.join(rawDir, 'caption-01.txt'), 'Home')
    fs.writeFileSync(path.join(rawDir, 'screen-02.txt'), 'screen two')
    let batchCalls = 0
    let jobsSeen = null
    // One batch call for ALL frames — not one call per frame.
    const renderStills = async (jobs) => {
      batchCalls += 1
      jobsSeen = jobs
      for (const j of jobs) fs.writeFileSync(j.output, 'fake-png')
    }
    const stills = await renderFrames({ rawDir, captureDir, title: 'boss', renderStills })
    assert.equal(batchCalls, 1, 'all frames render in a single batch invocation')
    assert.equal(jobsSeen.length, 2)
    assert.equal(jobsSeen[0].caption, 'Home')
    assert.equal(jobsSeen[1].caption, '') // no sidecar → empty
    // P3b (BOS-140): the still-filename prefix is now scene-scoped; no
    // sceneForScreen map defaults every screen to scene 01.
    assert.deepEqual(
      stills.map((s) => s.label),
      ['scene 01 frame 01', 'scene 01 frame 02'],
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('renderFrames batch: only frames whose PNG was written are returned', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'one')
    fs.writeFileSync(path.join(rawDir, 'screen-02.txt'), 'two')
    // Simulate a partial batch: only frame 02 gets written.
    const renderStills = async (jobs) => {
      const j = jobs.find((x) => x.output.endsWith('frame-02.png'))
      fs.writeFileSync(j.output, 'fake-png')
    }
    const stills = await renderFrames({ rawDir, captureDir, title: 'boss', renderStills })
    assert.deepEqual(
      stills.map((s) => s.label),
      ['scene 01 frame 02'],
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('renderFrames batch failure falls back to per-frame renderStill', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'one')
    fs.writeFileSync(path.join(rawDir, 'screen-02.txt'), 'two')
    // Batch dies before writing any PNG (manifest/browser failure). Without a
    // fallback this would drop ALL stills and force the zero-frame path.
    const renderStills = async () => {
      throw new Error('manifest render exploded')
    }
    // Per-frame renderer still works — the fallback must recover every frame.
    const perFrame = []
    const renderStill = async ({ output, caption }) => {
      perFrame.push({ output, caption })
      fs.writeFileSync(output, 'fake-png')
    }
    const stills = await renderFrames({
      rawDir,
      captureDir,
      title: 'boss',
      renderStill,
      renderStills,
    })
    assert.equal(perFrame.length, 2, 'both missing frames retried individually')
    assert.deepEqual(
      stills.map((s) => s.label),
      ['scene 01 frame 01', 'scene 01 frame 02'],
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('renderFrames defaults caption to empty string when no sidecar exists', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'a TUI screen')
    const seen = []
    const renderStill = async ({ output, caption }) => {
      seen.push({ caption })
      fs.writeFileSync(output, 'fake-png')
    }
    await renderFrames({ rawDir, captureDir, title: 'boss', renderStill })
    assert.equal(seen.length, 1)
    assert.equal(seen[0].caption, '')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('runAgentLoop binds each frame caption to the narration preceding its tool call', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    // A SINGLE response carries two actions, each preceded by its own narration.
    // The turn-wide text join would caption both frames identically; the fix
    // must write each tool call's preceding text to its own sidecar.
    const firstTurn = {
      stop_reason: 'tool_use',
      usage: { input_tokens: 10, output_tokens: 5 },
      content: [
        { type: 'text', text: 'Opening the cron list' },
        { type: 'tool_use', id: 'a', name: 'observe', input: {} },
        { type: 'text', text: 'Selecting the first row' },
        { type: 'tool_use', id: 'b', name: 'send_keys', input: { keys: ['down'] } },
      ],
    }
    const model = scriptedModel([
      firstTurn,
      toolUse('done', { passed: true, summary: 'done', evidence: [] }, { text: 'wrapping up' }),
    ])
    const bridge = scriptedBridge({ screens: ['SCREEN ONE', 'SCREEN TWO'] })
    await runAgentLoop({
      brief: {
        description: 'prove it',
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.equal(
      fs.readFileSync(path.join(rawDir, 'caption-01.txt'), 'utf8'),
      'Opening the cron list',
    )
    assert.equal(
      fs.readFileSync(path.join(rawDir, 'caption-02.txt'), 'utf8'),
      'Selecting the first row',
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

// ── Confirmation outro (BOS-393) ─────────────────────────────────────────────

/**
 * Drive runAgentLoop with a scripted model + bridge inside a scratch rawDir,
 * returning the loop result. `bridge` defaults to a scriptedBridge; `readCastMs`
 * defaults to an injected finite clock so the outro frame gets a real timestamp.
 */
async function runOutroLoop({
  responses,
  bridge = scriptedBridge({ screens: ['EVIDENCE SCREEN'] }),
  scenes,
  stamps = [1000, 2000, 3000, 4000],
  readCastMs,
  budgets = { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
}) {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-outro-'))
  let i = 0
  const clock = readCastMs ?? (() => stamps[Math.min(i++, stamps.length - 1)])
  try {
    return await runAgentLoop({
      brief: { description: 'prove it', budgets, ...(scenes ? { scenes } : {}) },
      ...(scenes ? { scenes } : {}),
      model: 'test-model',
      modelDep: scriptedModel(responses),
      bridge,
      rawDir,
      readCastMs: clock,
    })
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
}

test('runAgentLoop captures a ✔ outro frame after an accepted done(passed=true)', async () => {
  const loop = await runOutroLoop({
    responses: [
      toolUse('observe', {}, { text: 'observing evidence' }),
      toolUse(
        'done',
        { passed: true, summary: 'evidence visible', evidence: [] },
        { text: 'closing narration text' },
      ),
    ],
  })
  assert.equal(loop.agentResult.passed, true)
  assert.ok(Number.isFinite(loop.outro.startMs))
  const last = loop.captionTimings.at(-1)
  assert.match(last.caption, /^✔ /)
  assert.match(last.caption, /closing narration text/) // narration wins over summary
  assert.equal(loop.screens.at(-1).text, 'EVIDENCE SCREEN') // outro pushed to screens
  assert.equal(loop.outro.startMs, loop.screens.at(-1).castMs)
})

test('runAgentLoop outro uses done.summary when no narration precedes done()', async () => {
  const loop = await runOutroLoop({
    responses: [
      toolUse('observe', {}, { text: 'observing evidence' }),
      toolUse('done', { passed: true, summary: 'evidence visible', evidence: [] }),
    ],
  })
  assert.match(loop.captionTimings.at(-1).caption, /^✔ evidence visible/)
})

test('runAgentLoop outro falls back to done.summary when narration is whitespace-only (BOS-393)', async () => {
  // A closing text block of only whitespace is truthy but empty once trimmed; it
  // must NOT win over done.summary and yield a bare `✔`.
  const loop = await runOutroLoop({
    responses: [
      toolUse('observe', {}, { text: 'observing evidence' }),
      toolUse('done', { passed: true, summary: 'evidence visible', evidence: [] }, { text: '   ' }),
    ],
  })
  assert.match(loop.captionTimings.at(-1).caption, /^✔ evidence visible/)
})

test('runAgentLoop captures a ✖ outro for an ACCEPTED done(passed=false)', async () => {
  // Single (synthesized) scene → evaluateDoneCall accepts the failing verdict.
  const loop = await runOutroLoop({
    responses: [
      toolUse('observe', {}, { text: 'looking' }),
      toolUse(
        'done',
        { passed: false, summary: 'could not load', evidence: [] },
        {
          text: 'blocker found',
        },
      ),
    ],
  })
  assert.equal(loop.agentResult.passed, false)
  assert.match(loop.captionTimings.at(-1).caption, /^✖ /)
  assert.match(loop.captionTimings.at(-1).caption, /blocker found/)
})

test('runAgentLoop emits NO outro for a rejected premature done', async () => {
  // done(passed=true) with a later scene unbegun → evaluateDoneCall rejects; with
  // maxSteps=2 the loop exhausts before the rejection budget is spent, so
  // done.passed stays null and no outro is staged.
  const loop = await runOutroLoop({
    scenes: [
      { id: 'scene-01', title: 'A' },
      { id: 'scene-02', title: 'B' },
    ],
    responses: [
      toolUse('done', { passed: true, summary: 'x', evidence: [] }),
      toolUse('done', { passed: true, summary: 'x', evidence: [] }),
    ],
    budgets: { maxSteps: 2, maxWallClockMs: 60_000, maxTokens: 100_000 },
  })
  assert.equal(loop.outro, null)
  assert.ok(!loop.captionTimings.at(-1)?.caption?.startsWith('✔'))
})

test('runAgentLoop outro is all-or-nothing: observe error degrades, verdict preserved', async () => {
  // bridge.observe succeeds in the loop, throws ONLY on the post-done outro call.
  let observeCount = 0
  const bridge = {
    async observe() {
      observeCount += 1
      if (observeCount > 1) throw new Error('outro observe boom')
      return { screen: 'EVIDENCE SCREEN' }
    },
    async sendKeys() {
      return { screen: 'EVIDENCE SCREEN' }
    },
    async typeText() {
      return { screen: 'EVIDENCE SCREEN' }
    },
    async quit() {},
  }
  const loop = await runOutroLoop({
    bridge,
    responses: [
      toolUse('observe', {}, { text: 'looking' }),
      toolUse('done', { passed: true, summary: 'ok', evidence: [] }, { text: 'closing' }),
    ],
  })
  assert.equal(loop.agentResult.passed, true) // verdict untouched (2b-A)
  assert.equal(loop.outro.degraded.reason, 'outro-degraded')
  assert.ok(!loop.captionTimings.at(-1)?.caption?.startsWith('✔')) // nothing half-written
})

test('runAgentLoop outro skips on a blank screen (all-or-nothing, nullCastRead unchanged)', async () => {
  let observeCount = 0
  const bridge = {
    async observe() {
      observeCount += 1
      return { screen: observeCount > 1 ? '   \n' : 'EVIDENCE SCREEN' }
    },
    async sendKeys() {
      return { screen: 'EVIDENCE SCREEN' }
    },
    async typeText() {
      return { screen: 'EVIDENCE SCREEN' }
    },
    async quit() {},
  }
  const loop = await runOutroLoop({
    bridge,
    responses: [
      toolUse('observe', {}, { text: 'looking' }),
      toolUse('done', { passed: true, summary: 'ok', evidence: [] }, { text: 'closing' }),
    ],
  })
  assert.equal(loop.outro.degraded.reason, 'outro-degraded')
  assert.equal(loop.screens.length, 1) // no outro frame pushed
  assert.equal(loop.nullCastRead, false) // outro never sets the BOS-216 flag
})

test('runAgentLoop outro skips on a null cast read and rolls the partial capture back', async () => {
  // outro observe returns a real screen, but readCastMs() → null on the outro
  // read: the partial capture must roll back and nullCastRead must stay false.
  const stamps = [1000, null]
  let i = 0
  const loop = await runOutroLoop({
    responses: [
      toolUse('observe', {}, { text: 'looking' }),
      toolUse('done', { passed: true, summary: 'ok', evidence: [] }, { text: 'closing' }),
    ],
    readCastMs: () => stamps[Math.min(i++, stamps.length - 1)],
  })
  assert.equal(loop.outro.degraded.reason, 'outro-degraded')
  assert.equal(loop.screens.length, 1) // rolled back to loop-only frame
  assert.equal(loop.captionTimings.length, 1)
  assert.equal(loop.nullCastRead, false) // outro never sets the BOS-216 flag
})

test('runAgentLoop outro restores finalScreen on the null-cast rollback (BOS-393)', async () => {
  // The outro observe returns a DIFFERENT screen than the last tool frame; the
  // cast read goes null on the outro, so the whole capture — finalScreen too —
  // must roll back to the pre-outro frame. finalScreen feeds the evidence scan,
  // so leaving it on an unrecorded outro screen would scan a dropped frame.
  let observeCount = 0
  const bridge = {
    async observe() {
      observeCount += 1
      return { screen: observeCount > 1 ? 'OUTRO SCREEN' : 'LOOP SCREEN' }
    },
    async sendKeys() {
      return { screen: 'LOOP SCREEN' }
    },
    async typeText() {
      return { screen: 'LOOP SCREEN' }
    },
    async quit() {},
  }
  const stamps = [1000, null]
  let i = 0
  const loop = await runOutroLoop({
    bridge,
    responses: [
      toolUse('observe', {}, { text: 'looking' }),
      toolUse('done', { passed: true, summary: 'ok', evidence: [] }, { text: 'closing' }),
    ],
    readCastMs: () => stamps[Math.min(i++, stamps.length - 1)],
  })
  assert.equal(loop.outro.degraded.reason, 'outro-degraded')
  assert.equal(loop.finalScreen, 'LOOP SCREEN') // restored, not the outro screen
  assert.equal(loop.screens.at(-1).text, 'LOOP SCREEN') // no outro frame stranded
})

test('runAgentLoop outro rolls back wholesale when captureSettledScreen throws (BOS-393)', async () => {
  // readCastMs throws DURING the outro capture (after the sidecars are written).
  // The outro must degrade AND leave no partial state: verdict, frame count,
  // finalScreen, and the on-disk sidecars all restored to the pre-outro loop.
  let observeCount = 0
  const bridge = {
    async observe() {
      observeCount += 1
      return { screen: observeCount > 1 ? 'OUTRO SCREEN' : 'LOOP SCREEN' }
    },
    async sendKeys() {
      return { screen: 'LOOP SCREEN' }
    },
    async typeText() {
      return { screen: 'LOOP SCREEN' }
    },
    async quit() {},
  }
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-outro-throw-'))
  let reads = 0
  const readCastMs = () => {
    reads += 1
    if (reads > 1) throw new Error('cast read boom') // throws on the outro read
    return 1000
  }
  try {
    const loop = await runAgentLoop({
      brief: {
        description: 'prove it',
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      model: 'test-model',
      modelDep: scriptedModel([
        toolUse('observe', {}, { text: 'looking' }),
        toolUse('done', { passed: true, summary: 'ok', evidence: [] }, { text: 'closing' }),
      ]),
      bridge,
      rawDir,
      readCastMs,
    })
    assert.equal(loop.agentResult.passed, true) // verdict preserved (2b-A)
    assert.equal(loop.outro.degraded.reason, 'outro-degraded')
    assert.match(loop.outro.degraded.detail, /outro capture failed/)
    assert.equal(loop.screens.length, 1) // no stranded outro frame
    assert.equal(loop.captionTimings.length, 1)
    assert.equal(loop.finalScreen, 'LOOP SCREEN') // rolled back
    assert.equal(loop.nullCastRead, false)
    // The outro sidecars written before the throw must be cleaned up.
    assert.equal(fs.existsSync(path.join(rawDir, 'screen-02.txt')), false)
    assert.equal(fs.existsSync(path.join(rawDir, 'caption-02.txt')), false)
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop outro is FINAL even when tool blocks trail the done() in the same message', async () => {
  // One turn: [narration, done(passed=true), observe] — the trailing observe
  // still executes and captures a screen; the outro must come AFTER it (6A).
  const loop = await runOutroLoop({
    responses: [
      {
        stop_reason: 'tool_use',
        usage: { input_tokens: 10, output_tokens: 5 },
        content: [
          { type: 'text', text: 'closing narration' },
          { type: 'tool_use', id: 'd', name: 'done', input: { passed: true, summary: 'x' } },
          { type: 'tool_use', id: 'o', name: 'observe', input: {} },
        ],
      },
    ],
  })
  assert.match(loop.captionTimings.at(-1).caption, /^✔ /)
  assert.match(loop.captionTimings.at(-1).caption, /closing narration/)
})

// ── parseCastTailMs (caption clock, BOS-121) ─────────────────────────────────

test('parseCastTailMs: returns the last event time in ms (header skipped)', async () => {
  const { parseCastTailMs } = await import('./proof-tui-agent.mjs')
  const cast = [
    '{"version":2,"width":80,"height":24}',
    '[0.5, "o", "boot"]',
    '[2.25, "o", "settings"]',
    '[3.0, "o", "done"]',
  ].join('\n')
  assert.equal(parseCastTailMs(cast), 3000)
})

test('parseCastTailMs: ignores trailing blank lines and partial last lines', async () => {
  const { parseCastTailMs } = await import('./proof-tui-agent.mjs')
  const cast = '{"version":2}\n[1.0, "o", "a"]\n[2.5, "o", "b"]\n\n  \n'
  assert.equal(parseCastTailMs(cast), 2500)
})

test('parseCastTailMs: empty / header-only / non-string → null (no-cast contract, BOS-216)', async () => {
  const { parseCastTailMs } = await import('./proof-tui-agent.mjs')
  // BOS-216: null (not 0) distinguishes "no cast" from a genuine t=0 event so an
  // unreadable cast surfaces a loud degraded marker instead of a silent startMs:0.
  assert.equal(parseCastTailMs(''), null)
  assert.equal(parseCastTailMs('{"version":2,"width":80}'), null)
  assert.equal(parseCastTailMs(undefined), null)
  assert.equal(parseCastTailMs(null), null)
  // A genuine t=0 event still returns 0 (round-trippable, not null).
  assert.equal(parseCastTailMs('{"v":2}\n[0, "o", "boot"]'), 0)
})

test('parseCastTailMs: rounds to the nearest ms and never goes negative', async () => {
  const { parseCastTailMs } = await import('./proof-tui-agent.mjs')
  assert.equal(parseCastTailMs('{"v":2}\n[1.2345, "o", "x"]'), 1235)
  assert.equal(parseCastTailMs('{"v":2}\n[-0.4, "o", "x"]'), 0)
})

test('runAgentLoop: records per-step caption timings from the injected cast clock', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const firstTurn = {
      stop_reason: 'tool_use',
      usage: { input_tokens: 10, output_tokens: 5 },
      content: [
        { type: 'text', text: 'Opening the cron list' },
        { type: 'tool_use', id: 'a', name: 'observe', input: {} },
        { type: 'text', text: 'Selecting the first row' },
        { type: 'tool_use', id: 'b', name: 'send_keys', input: { keys: ['down'] } },
      ],
    }
    const model = scriptedModel([
      firstTurn,
      toolUse('done', { passed: true, summary: 'done', evidence: [] }),
    ])
    const bridge = scriptedBridge({ screens: ['SCREEN ONE', 'SCREEN TWO'] })
    // Injected cast clock advances per settled screen — no real bridge/cast.
    const stamps = [1000, 3500]
    let i = 0
    const { captionTimings } = await runAgentLoop({
      brief: {
        description: 'prove it',
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
      readCastMs: () => stamps[Math.min(i++, stamps.length - 1)],
    })
    assert.deepEqual(captionTimings, [
      { seq: 1, caption: 'Opening the cron list', startMs: 1000 },
      { seq: 2, caption: 'Selecting the first row', startMs: 3500 },
      // BOS-393 confirmation outro: the accepted done() burns a final ✔ frame
      // (done.summary — no narration preceded this done() call). The injected
      // clock repeats its last stamp for the outro read.
      { seq: 3, caption: '✔ done', startMs: 3500 },
    ])
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: default cast clock degrades to startMs null when no .cast exists (BOS-216)', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const model = scriptedModel([
      toolUse('observe', {}, { text: 'Looking at the home screen' }),
      toolUse('done', { passed: true, summary: 'done', evidence: [] }),
    ])
    const bridge = scriptedBridge({ screens: ['HOME'] })
    // No readCastMs injected and no session.cast on disk → reader returns null.
    const result = await runAgentLoop({
      brief: {
        description: 'prove it',
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    // BOS-216: no session.cast on disk → reader returns null (not 0). planCaptionWindows
    // drops non-finite startMs so caption burning stays safe; the null is surfaced as a
    // loud degraded marker at the run level.
    assert.deepEqual(result.captionTimings, [
      { seq: 1, caption: 'Looking at the home screen', startMs: null },
    ])
    assert.equal(result.nullCastRead, true, 'a null cast read is reported for the degraded marker')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: wallClockTruncated is true ONLY when the clock breaks before a verdict (BOS-354)', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')

  // (1) Wall-clock cutoff before any verdict → truncated.
  const truncated = await runAgentLoop({
    brief: {
      description: 'never finishes in time',
      budgets: { maxSteps: 50, maxWallClockMs: -1, maxTokens: 100_000 },
    },
    model: 'test-model',
    modelDep: scriptedModel([toolUse('observe', {})], { repeatLast: true }),
    bridge: scriptedBridge({ screens: ['HOME'] }),
    rawDir: fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-trunc-')),
  })
  assert.equal(truncated.wallClockTruncated, true, 'clock break with no verdict → truncated')
  assert.equal(truncated.agentResult.passed, false, 'a truncated run never passes')

  // (2) The agent reached an explicit done({passed:false}) verdict → NOT truncated.
  const rejected = await runAgentLoop({
    brief: {
      description: 'agent judges its own evidence bad',
      budgets: { maxSteps: 50, maxWallClockMs: 60_000, maxTokens: 100_000 },
    },
    model: 'test-model',
    modelDep: scriptedModel([toolUse('done', { passed: false, summary: 'blocked', evidence: [] })]),
    bridge: scriptedBridge({ screens: ['HOME'] }),
    rawDir: fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-trunc-')),
  })
  assert.equal(
    rejected.wallClockTruncated,
    false,
    'a genuine done(passed:false) is not a truncation',
  )

  // (3) Step exhaustion (never calls done) → NOT truncated.
  const exhausted = await runAgentLoop({
    brief: {
      description: 'runs out of steps',
      budgets: { maxSteps: 2, maxWallClockMs: 60_000, maxTokens: 100_000 },
    },
    model: 'test-model',
    modelDep: scriptedModel([toolUse('observe', {})], { repeatLast: true }),
    bridge: scriptedBridge({ screens: ['HOME'] }),
    rawDir: fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-trunc-')),
  })
  assert.equal(
    exhausted.wallClockTruncated,
    false,
    'step exhaustion is genuine incompleteness, not a clock cutoff',
  )
})

test('runAgentLoop: a procedurally-rejected done(passed:false) then a wall-clock cutoff stays fatal, never tui-truncated (BOS-354 / BOS-226)', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')

  // A model that pauses ~80ms per call so the wall clock (25ms budget) trips on
  // the SECOND top-of-loop check — AFTER the first response is processed. setTimeout
  // only ever fires late, so iter-0 elapsed (~0 < 25) passes and iter-1 elapsed
  // (>=80 > 25) breaks: deterministic, no real-time flakiness either direction.
  const delayedModel = (responses) => {
    let i = 0
    return {
      calls: 0,
      async createMessage() {
        this.calls += 1
        await new Promise((r) => setTimeout(r, 80))
        const resp = responses[Math.min(i, responses.length - 1)]
        if (i < responses.length - 1) i += 1
        return resp
      },
    }
  }
  // Two scenes so a done() at scene 1 leaves scenes remaining ⇒ evaluateDoneCall
  // PROCEDURALLY REJECTS it (rejection budget unspent), leaving done.passed === null.
  const twoScene = {
    description: 'multi-scene: agent hits a blocker in scene 1',
    scenes: [
      { id: 'scene-01', title: 'Home', expectedEvidence: ['Home'] },
      { id: 'scene-02', title: 'Settings', expectedEvidence: ['Settings'] },
    ],
    budgets: { maxSteps: 50, maxWallClockMs: 25, maxTokens: 100_000 },
  }

  // A genuine blocker signalled via done(passed:false), rejected (scenes remain),
  // then the clock expires: MUST stay fatal (the BOS-226 invariant this ticket
  // swears to keep) — never soften to the neutral tui-truncated deferral.
  const failModel = delayedModel([
    toolUse('done', { passed: false, summary: 'blocked in scene 1', evidence: [] }),
  ])
  const rejectedFail = await runAgentLoop({
    brief: twoScene,
    model: 'test-model',
    modelDep: failModel,
    bridge: scriptedBridge({ screens: ['HOME'] }),
    rawDir: fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-trunc-')),
  })
  assert.equal(
    failModel.calls,
    1,
    'the fail verdict is processed before the clock breaks on the next iteration',
  )
  assert.equal(
    rejectedFail.wallClockTruncated,
    false,
    'a rejected done(passed:false) blocker + wall-clock cutoff stays fatal agent-incomplete, never tui-truncated',
  )
  assert.equal(rejectedFail.agentResult.passed, false)

  // Positive control: the SAME delayed clock with an observe-only run (never a
  // failing verdict) DOES truncate — proving the clock genuinely broke above and
  // the `false` is the failing-verdict guard, not a clock that never fired.
  const observeTimeout = await runAgentLoop({
    brief: twoScene,
    model: 'test-model',
    modelDep: delayedModel([toolUse('observe', {})]),
    bridge: scriptedBridge({ screens: ['HOME'] }),
    rawDir: fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-trunc-')),
  })
  assert.equal(
    observeTimeout.wallClockTruncated,
    true,
    'same delayed clock, no failing verdict → truncated (confirms the clock genuinely broke)',
  )
})

test('runTuiAgentProof: persists raw/caption-timings.json and forwards timings to castToVideo', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const brief = {
    title: 'Captioned video',
    description: 'Produces a captioned video',
    expectedEvidence: ['Ready'],
  }
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-captions' }),
        async () => {
          let castArgs = null
          const castToVideo = async (args) => {
            castArgs = args
            return null // stills-only; we only assert the timings were threaded
          }
          const { manifest } = await runTuiAgentProof({
            prNumber: '0',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}, { text: 'Observing the ready screen' }),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo,
            },
          })
          assert.ok(manifest, 'run completes')
          assert.ok(
            Array.isArray(castArgs.captionTimings),
            'captionTimings forwarded to castToVideo',
          )
          assert.equal(castArgs.captionTimings[0].caption, 'Observing the ready screen')
          // raw/caption-timings.json persisted next to the run's screens. The
          // path is deterministic from the env (commit/run-id/run-token in BASE_ENV).
          const rawTimingsPath = path.join(
            REPO_ROOT,
            '.proof',
            'pr-0',
            'abc1234',
            'tui-captions',
            'tok-tui-test',
            'raw',
            'caption-timings.json',
          )
          const persisted = JSON.parse(fs.readFileSync(rawTimingsPath, 'utf8'))
          assert.equal(persisted[0].caption, 'Observing the ready screen')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('0')
  }
})

// ── BOS-139: collect mode (orchestrator owns identity + budget) ──────────────

test('runTuiAgentProof: collect mode returns a SurfaceRun (no finalize), per-surface brief, clamped budget', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const brief = {
    title: 'Open settings',
    description: 'Demonstrates the settings screen opens',
    expectedEvidence: ['Settings'],
  }
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-collect-'))
  let finalizeReached = false
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(BASE_ENV({ BOSS_PROOF_BRIEF: briefPath }), async () => {
        const surfaceRun = await runTuiAgentProof({
          prNumber: 'tuicollect',
          commit: 'abc1234',
          changedFiles: [],
          dryRun: true,
          runContext: {
            runId: 'RUN',
            token: 'tok',
            paths: { publicPrefix: 'proof/bossanova/pr-tuicollect/abc1234/RUN/tok' },
            localDir,
            briefFileName: 'brief-tui.json',
            maxWallClockMs: 60_000,
            collect: true,
          },
          deps: {
            bridge: scriptedBridge({ screens: ['Settings panel open', 'Settings saved'] }),
            model: scriptedModel([
              toolUse('observe', {}),
              toolUse('send_keys', { keys: ['s'] }),
              toolUse('done', { summary: 'Opened settings', passed: true }),
            ]),
            renderStill: fakeRenderStill(),
            castToVideo: async () => null,
            // finalizeDeps is forwarded to finalizeAgentProof; a post here would
            // prove finalize ran. Collect mode must never reach it.
            finalizeDeps: {
              postComment: () => {
                finalizeReached = true
              },
              uploadBundle: () => {
                finalizeReached = true
              },
            },
          },
        })
        assert.equal(surfaceRun.surface, 'tui')
        assert.ok(Array.isArray(surfaceRun.captureShapes) && surfaceRun.captureShapes.length >= 1)
        assert.equal(surfaceRun.brief.budgets.maxWallClockMs, 60_000, 'budget clamped to grant')
        assert.equal(surfaceRun.noSurface, false)
        assert.equal(surfaceRun.reasonCode, null)
        assert.equal(typeof surfaceRun.elapsedMs, 'number')
        assert.ok(fs.existsSync(path.join(localDir, 'brief-tui.json')), 'per-surface brief written')
        assert.equal(finalizeReached, false, 'collect mode must not finalize')
      }),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(localDir, { recursive: true, force: true })
  }
})

// ── Task 3 (P3b, BOS-140): begin_scene markers, scene-attributed screens ─────

test('runAgentLoop: begin_scene(scene-02) records sceneTimings + sceneForScreen and burns a scene caption', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      scenes: [
        { id: 'scene-01', title: 'Home', expectedEvidence: ['Home'] },
        { id: 'scene-02', title: 'Settings', expectedEvidence: ['Settings'] },
      ],
      budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    // Turn 2 marks scene 2 THEN acts within it, in the same message — mirrors
    // the multi-tool-call-per-turn shape the caption-binding tests already use.
    const secondTurn = {
      stop_reason: 'tool_use',
      usage: { input_tokens: 10, output_tokens: 5 },
      content: [
        { type: 'tool_use', id: 'bs', name: 'begin_scene', input: { id: 'scene-02' } },
        { type: 'tool_use', id: 'sk', name: 'send_keys', input: { keys: ['s'] } },
      ],
    }
    const model = scriptedModel([
      toolUse('observe', {}),
      secondTurn,
      toolUse('done', { passed: true, summary: 'done', evidence: [] }),
    ])
    const bridge = scriptedBridge({ screens: ['Home screen', 'Settings screen'] })
    // 3 readCastMs() calls occur: screen-1 capture, the begin_scene marker,
    // then the screen-2 capture — the marker's stamp becomes scene-02's startMs.
    const stamps = [500, 1200, 1400]
    let i = 0
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
      readCastMs: () => stamps[Math.min(i++, stamps.length - 1)],
    })
    assert.deepEqual(result.sceneTimings, [
      { id: 'scene-01', title: 'Home', startMs: 0 },
      { id: 'scene-02', title: 'Settings', startMs: 1200 },
    ])
    // BOS-393: the accepted done() adds a 3rd (outro) frame within the active
    // scene (scene-02), so sceneForScreen/screens now carry that trailing entry.
    assert.deepEqual(result.sceneForScreen, { 1: 'scene-01', 2: 'scene-02', 3: 'scene-02' })
    assert.equal(result.captionTimings[0].caption, '')
    assert.equal(
      result.captionTimings[1].caption,
      'Scene 2 — Settings',
      'the marker burns a default scene-change caption into the video timeline',
    )
    assert.equal(
      result.captionTimings[1].startMs,
      1200,
      'scene-change caption starts at the begin_scene marker timestamp, not the later frame timestamp',
    )
    assert.deepEqual(
      result.screens.map((s) => s.seq),
      [1, 2, 3],
    )
    assert.deepEqual(
      result.screens.map((s) => s.text),
      // The 3rd is the confirmation-outro frame (the last screen repeats).
      ['Home screen', 'Settings screen', 'Settings screen'],
    )
    assert.match(result.captionTimings.at(-1).caption, /^✔ done/)
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: out-of-order begin_scene marker errors and leaves the active scene unchanged', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      scenes: [
        { id: 'scene-01', title: 'One' },
        { id: 'scene-02', title: 'Two' },
        { id: 'scene-03', title: 'Three' },
      ],
      budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const badMarkerTurn = {
      stop_reason: 'tool_use',
      usage: { input_tokens: 10, output_tokens: 5 },
      content: [{ type: 'tool_use', id: 'bs', name: 'begin_scene', input: { id: 'scene-03' } }],
    }
    let step = 0
    let markerToolResult = null
    const model = {
      async createMessage({ messages }) {
        step += 1
        if (step === 1) return badMarkerTurn
        // The prior assistant turn's tool_result is the last user message —
        // inspect it to assert the error shape the loop fed back to the model.
        // Capture only the FIRST tool_result: the premature done(passed=true)
        // below is itself rejected once (BOS-251), so later turns carry the
        // done-rejection error instead of the marker error.
        const lastUser = messages[messages.length - 1]
        if (markerToolResult === null) markerToolResult = JSON.parse(lastUser.content[0].content)
        return toolUse('done', { passed: true, summary: 'done', evidence: [] })
      },
    }
    const bridge = scriptedBridge({ screens: ['Screen'] })
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.ok(markerToolResult.error, 'out-of-order marker must return an error tool_result')
    assert.match(markerToolResult.error, /expected scene-02/)
    assert.deepEqual(
      result.sceneTimings,
      [{ id: 'scene-01', title: 'One', startMs: 0 }],
      'no state change on an out-of-order marker',
    )
    assert.deepEqual(result.sceneForScreen, {}, 'no screens were captured in this run')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: duplicate begin_scene for the already-active scene is a no-op', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      scenes: [{ id: 'scene-01', title: 'One' }],
      budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const dupeTurn = {
      stop_reason: 'tool_use',
      usage: { input_tokens: 10, output_tokens: 5 },
      content: [{ type: 'tool_use', id: 'bs', name: 'begin_scene', input: { id: 'scene-01' } }],
    }
    let step = 0
    let markerToolResult = null
    const model = {
      async createMessage({ messages }) {
        step += 1
        if (step === 1) return dupeTurn
        const lastUser = messages[messages.length - 1]
        markerToolResult = JSON.parse(lastUser.content[0].content)
        return toolUse('done', { passed: true, summary: 'done', evidence: [] })
      },
    }
    const bridge = scriptedBridge({ screens: ['Screen'] })
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.deepEqual(markerToolResult, { ok: true }, 'duplicate active-scene marker is a no-op')
    assert.deepEqual(result.sceneTimings, [{ id: 'scene-01', title: 'One', startMs: 0 }])
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: marker-less multi-scene run attributes every screen to scene 1 (back-compat)', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      scenes: [
        { id: 'scene-01', title: 'One' },
        { id: 'scene-02', title: 'Two' },
      ],
      budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const model = scriptedModel([
      toolUse('observe', {}),
      toolUse('send_keys', { keys: ['s'] }),
      toolUse('done', { passed: true, summary: 'done', evidence: [] }),
    ])
    const bridge = scriptedBridge({ screens: ['Screen one', 'Screen two'] })
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.deepEqual(
      result.sceneTimings,
      [{ id: 'scene-01', title: 'One', startMs: 0 }],
      'no marker was called, so scene 1 is the only sceneTimings entry',
    )
    assert.deepEqual(result.sceneForScreen, { 1: 'scene-01', 2: 'scene-01' })
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: scenes default from normalizeScenes(brief) when omitted (single-scene back-compat)', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      title: 'Legacy scene-less brief',
      description: 'prove it',
      expectedEvidence: ['Home'],
      budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const model = scriptedModel([
      toolUse('observe', {}),
      toolUse('done', { passed: true, summary: 'done', evidence: [] }),
    ])
    const bridge = scriptedBridge({ screens: ['Home screen'] })
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.deepEqual(result.sceneTimings, [
      { id: 'scene-01', title: 'Legacy scene-less brief', startMs: 0 },
    ])
    assert.deepEqual(result.sceneForScreen, { 1: 'scene-01' })
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: multi-scene goal renders a numbered scene block instead of flat hints/evidence', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      scenes: [
        { id: 'scene-01', title: 'Home', stepsHints: ['press s'], expectedEvidence: ['Home'] },
        { id: 'scene-02', title: 'Settings', expectedEvidence: ['Settings'] },
      ],
      budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    let capturedGoal = null
    const model = {
      async createMessage({ messages }) {
        if (capturedGoal === null) capturedGoal = messages[0].content
        return toolUse('done', { passed: true, summary: 'done', evidence: [] })
      },
    }
    const bridge = scriptedBridge({ screens: ['Home screen'] })
    await runAgentLoop({ brief, model: 'test-model', modelDep: model, bridge, rawDir })
    assert.match(capturedGoal, /Scene 1 \(scene-01\) — Home/)
    assert.match(capturedGoal, /Scene 2 \(scene-02\) — Settings/)
    assert.match(capturedGoal, /must show: Settings/)
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: a one-explicit-scene brief steers the agent from the scene evidence the gate uses (BOS-222)', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    // A brief with exactly ONE explicit scene normalizes to a single-element
    // scenes array → the single-scene goal branch. Its steering must come from
    // scenes[0].expectedEvidence (what evaluateSceneEvidence gates on), NOT the
    // inert top-level brief.expectedEvidence — otherwise the agent is steered
    // toward the wrong tokens and legitimate runs fail the gate spuriously.
    const brief = {
      description: 'prove it',
      expectedEvidence: ['TOP LEVEL INERT'], // required by schema, ignored by the gate
      scenes: [{ id: 'scene-01', title: 'Home', expectedEvidence: ['Scene token'] }],
      budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    let capturedGoal = null
    const model = {
      async createMessage({ messages }) {
        if (capturedGoal === null) capturedGoal = messages[0].content
        return toolUse('done', { passed: true, summary: 'done', evidence: [] })
      },
    }
    const bridge = scriptedBridge({ screens: ['Home screen'] })
    await runAgentLoop({ brief, model: 'test-model', modelDep: model, bridge, rawDir })
    assert.match(capturedGoal, /must see ALL of these on screen[^\n]*Scene token/)
    assert.doesNotMatch(
      capturedGoal,
      /TOP LEVEL INERT/,
      'goal must not steer on inert top-level evidence',
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: matcher-object evidence renders readably in the agent goal, never [object Object] (BOS-222)', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    // The gate now accepts regex/anyOf/normalized-ci matcher objects, and the
    // brief prompt actively tells the model to emit them. The live agent must be
    // steered with their DISPLAY text (label / alternatives / raw token), not a
    // useless `[object Object]` — otherwise it is blinded to the very tokens it
    // must surface, inflating false negatives on the new matcher forms.
    const objectEvidence = [
      'Home',
      { text: 'v[0-9]+', match: 'regex' },
      { anyOf: [{ text: 'Saved' }, { text: 'Updated' }], label: 'save confirmation' },
    ]
    let capturedGoal = null
    const model = {
      async createMessage({ messages }) {
        if (capturedGoal === null) capturedGoal = messages[0].content
        return toolUse('done', { passed: true, summary: 'done', evidence: [] })
      },
    }
    const bridge = scriptedBridge({ screens: ['Home screen'] })

    // Multi-scene branch.
    await runAgentLoop({
      brief: {
        description: 'prove it',
        scenes: [{ id: 'scene-01', title: 'Home', expectedEvidence: objectEvidence }],
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      scenes: [
        { id: 'scene-01', title: 'Home', stepsHints: [], expectedEvidence: objectEvidence },
        { id: 'scene-02', title: 'Two', stepsHints: [], expectedEvidence: ['x'] },
      ],
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.doesNotMatch(capturedGoal, /\[object Object\]/, 'multi-scene goal must not leak objects')
    assert.match(capturedGoal, /must show: Home \| v\[0-9\]\+ \| save confirmation/)

    // Single-scene branch (top-level expectedEvidence).
    capturedGoal = null
    await runAgentLoop({
      brief: {
        description: 'prove it',
        expectedEvidence: objectEvidence,
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.doesNotMatch(
      capturedGoal,
      /\[object Object\]/,
      'single-scene goal must not leak objects',
    )
    assert.match(capturedGoal, /Home \| v\[0-9\]\+ \| save confirmation/)
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('renderFrames: names outputs scene-SS-frame-NN.png and stamps sceneId from sceneForScreen', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'one')
    fs.writeFileSync(path.join(rawDir, 'screen-02.txt'), 'two')
    const renderStill = async ({ output }) => fs.writeFileSync(output, 'fake-png')
    const stills = await renderFrames({
      rawDir,
      captureDir,
      title: 'boss',
      renderStill,
      sceneForScreen: { 1: 'scene-01', 2: 'scene-02' },
    })
    assert.ok(
      stills.some((s) => s.fileName.endsWith('scene-01-frame-01.png')),
      'scene 1 frame keeps the scene-01 prefix',
    )
    assert.ok(
      stills.some((s) => s.fileName.endsWith('scene-02-frame-02.png')),
      'scene 2 frame gets the scene-02 prefix',
    )
    assert.deepEqual(
      stills.map((s) => s.sceneId),
      ['scene-01', 'scene-02'],
    )
    assert.ok(fs.existsSync(path.join(captureDir, 'scene-01-frame-01.png')))
    assert.ok(fs.existsSync(path.join(captureDir, 'scene-02-frame-02.png')))
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('renderFrames: no sceneForScreen entry defaults a screen to scene 01 (zero-frame-fallback shape)', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'one')
    const renderStill = async ({ output }) => fs.writeFileSync(output, 'fake-png')
    const stills = await renderFrames({ rawDir, captureDir, title: 'boss', renderStill })
    assert.equal(stills[0].fileName.endsWith('scene-01-frame-01.png'), true)
    assert.equal(stills[0].sceneId, 'scene-01')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('runTuiAgentProof: persists raw/scene-timings.json alongside caption-timings.json', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const brief = {
    title: 'Scene timings persisted',
    description: 'Demonstrates a single-scene run persists scene-timings.json',
    expectedEvidence: ['Ready'],
  }
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-scene-timings' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuiscenetimings',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.ok(manifest, 'run completes')
          const rawTimingsPath = path.join(
            REPO_ROOT,
            '.proof',
            'pr-tuiscenetimings',
            'abc1234',
            'tui-scene-timings',
            'tok-tui-test',
            'raw',
            'scene-timings.json',
          )
          const persisted = JSON.parse(fs.readFileSync(rawTimingsPath, 'utf8'))
          assert.deepEqual(persisted, [
            { id: 'scene-01', title: 'Scene timings persisted', startMs: 0 },
          ])
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuiscenetimings')
  }
})

// ── BOS-219 seams: brief / loopRunner / evaluateEvidence ──────────────────────
// These pin the byte-identical DEFAULT behavior (no options → resolveBrief +
// runAgentLoop + evaluateSceneEvidence) and prove each seam swaps cleanly. The
// brief seam is the D4 no-SDK guarantee at the orchestrator level.

test('runTuiAgentProof: default seams call resolveBrief (generateBriefFromDiff) and runAgentLoop', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  try {
    await withEnv(
      BASE_ENV({ BOSS_PROOF_BRIEF: undefined, BOSS_PROOF_RUN_ID: 'tui-defaultseam' }),
      async () => {
        let genCalls = 0
        const generateBriefFromDiff = async () => {
          genCalls += 1
          return { title: 'Gen', description: 'generated brief', expectedEvidence: ['Home'] }
        }
        const bridge = scriptedBridge({ screens: ['Home screen'] })
        const model = scriptedModel([
          toolUse('observe', {}),
          toolUse('done', { summary: 'saw Home', passed: true }),
        ])
        const { manifest } = await runTuiAgentProof({
          prNumber: 'tuidefaultseam',
          commit: 'abc1234',
          changedFiles: [],
          dryRun: true,
          deps: {
            bridge,
            model,
            generateBriefFromDiff,
            renderStill: fakeRenderStill(),
            castToVideo: async () => null,
          },
        })
        assert.equal(genCalls, 1, 'default path must call resolveBrief → generateBriefFromDiff')
        assert.ok(model.calls >= 1, 'default path must drive runAgentLoop (model called)')
        assert.equal(manifest.captures[0].surface, 'tui')
      },
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuidefaultseam')
  }
})

test('runTuiAgentProof: an injected brief skips resolveBrief (generateBriefFromDiff never called)', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const { synthesizeBrief } = await import('./proof-tui-replay.mjs')
  const { loadScenario } = await import('./proof-scenario.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  try {
    await withEnv(
      BASE_ENV({ BOSS_PROOF_BRIEF: undefined, BOSS_PROOF_RUN_ID: 'tui-briefseam' }),
      async () => {
        const { scenario } = loadScenario(
          path.join(REPO_ROOT, 'scripts/testdata/scenario-fixtures/valid-full.json'),
        )
        const brief = synthesizeBrief(scenario, 'valid-full.scenario.json')
        let genCalls = 0
        const generateBriefFromDiff = async () => {
          genCalls += 1
          return { title: 'Gen', description: 'should never be used' }
        }
        const bridge = scriptedBridge({ screens: ['Session settings'] })
        const model = scriptedModel([
          toolUse('observe', {}),
          toolUse('done', { summary: 'done', passed: true }),
        ])
        await runTuiAgentProof({
          prNumber: 'tuibriefseam',
          commit: 'abc1234',
          changedFiles: [],
          dryRun: true,
          brief,
          deps: {
            bridge,
            model,
            generateBriefFromDiff,
            renderStill: fakeRenderStill(),
            castToVideo: async () => null,
          },
        })
        assert.equal(genCalls, 0, 'injected brief must skip resolveBrief entirely (no SDK path)')
      },
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuibriefseam')
  }
})

test('runTuiAgentProof: injected brief+loopRunner+evaluateEvidence flow replay artifacts with zero model calls', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const { synthesizeBrief, makeScenarioEvaluator } = await import('./proof-tui-replay.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const scenario = {
    version: 1,
    title: 'Replay',
    fixture: { preset: 'demo' },
    scenes: [{ id: 'scene-01', title: 'S', steps: [{ key: 'a' }, { expect: ['REPO', 'STATUS'] }] }],
  }
  try {
    await withEnv(
      BASE_ENV({ BOSS_PROOF_BRIEF: undefined, BOSS_PROOF_RUN_ID: 'tui-replaypair' }),
      async () => {
        const brief = synthesizeBrief(scenario, 'replay.scenario.json')
        let modelCalls = 0
        const model = {
          calls: 0,
          async createMessage() {
            modelCalls += 1
            this.calls += 1
            return { stop_reason: 'end_turn', content: [], usage: {} }
          },
        }
        let genCalls = 0
        const generateBriefFromDiff = async () => {
          genCalls += 1
          return { title: 'x', description: 'y' }
        }
        const loopRunner = async ({ rawDir }) => {
          fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'REPO STATUS ready')
          return {
            agentResult: {
              passed: true,
              summary: 'deterministic scenario replay of replay.scenario.json',
              evidence: [],
              steps: 1,
            },
            finalScreen: 'REPO STATUS ready',
            captionTimings: [],
            sceneTimings: [{ id: 'scene-01', title: 'S', startMs: 0 }],
            sceneForScreen: { 1: 'scene-01' },
            screens: [{ seq: 1, text: 'REPO STATUS ready', castMs: null }],
          }
        }
        const evaluateEvidence = makeScenarioEvaluator(scenario)
        const surfaceRun = await runTuiAgentProof({
          prNumber: 'tuireplaypair',
          commit: 'abc1234',
          changedFiles: [],
          dryRun: true,
          brief,
          loopRunner,
          evaluateEvidence,
          runContext: { collect: true, runId: 'tui-replaypair', token: 'tok-tui-test' },
          deps: {
            bridge: scriptedBridge({ screens: ['ignored'] }),
            model,
            generateBriefFromDiff,
            renderStill: fakeRenderStill(),
            castToVideo: async () => null,
          },
        })
        assert.equal(genCalls, 0, 'brief injected → no brief generation')
        assert.equal(modelCalls, 0, 'loopRunner replaces runAgentLoop → no model/SDK call')
        const cap = surfaceRun.captureShapes[0]
        assert.equal(cap.surface, 'tui')
        assert.equal(cap.scenes[0].passed, true, 'evaluator gate result flows into captureShape')
        assert.deepEqual(cap.scenes[0].missing, [])
      },
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuireplaypair')
  }
})

// ── BOS-351: TUI model default + resolution seam + unchanged budgets ────────────

test('resolveTuiModel: TUI default is claude-sonnet-4-6 with no env set', async () => {
  const { resolveTuiModel, DEFAULT_MODEL } = await import('./proof-tui-agent.mjs')
  assert.equal(DEFAULT_MODEL, 'claude-sonnet-4-6', 'exported TUI default model')
  assert.equal(resolveTuiModel({}), 'claude-sonnet-4-6', 'no env override → Sonnet TUI default')
})

test('resolveTuiModel: BOSS_PROOF_TUI_MODEL takes precedence over BOSS_PROOF_MODEL', async () => {
  const { resolveTuiModel } = await import('./proof-tui-agent.mjs')
  assert.equal(
    resolveTuiModel({
      BOSS_PROOF_TUI_MODEL: 'claude-tui-override',
      BOSS_PROOF_MODEL: 'claude-shared-override',
    }),
    'claude-tui-override',
    'TUI-scoped knob wins over the shared knob',
  )
})

test('resolveTuiModel: BOSS_PROOF_MODEL alone is still honored (back-compat)', async () => {
  const { resolveTuiModel } = await import('./proof-tui-agent.mjs')
  assert.equal(
    resolveTuiModel({ BOSS_PROOF_MODEL: 'claude-shared-override' }),
    'claude-shared-override',
    'shared knob honored when TUI knob is unset',
  )
})

test('proof-agent (web) default is unchanged at claude-haiku-4-5 and ignores TUI knob', async () => {
  const { resolveWebModel, DEFAULT_MODEL } = await import('./proof-agent.mjs')
  assert.equal(DEFAULT_MODEL, 'claude-haiku-4-5', 'web default model unchanged')
  assert.equal(resolveWebModel({}), 'claude-haiku-4-5', 'no env override → Haiku web default')
  assert.equal(
    resolveWebModel({ BOSS_PROOF_TUI_MODEL: 'claude-sonnet-4-6' }),
    'claude-haiku-4-5',
    'web leg must NOT read the TUI-scoped knob',
  )
  assert.equal(
    resolveWebModel({ BOSS_PROOF_MODEL: 'claude-shared-override' }),
    'claude-shared-override',
    'web leg still honors the shared BOSS_PROOF_MODEL',
  )
})

test('TUI_BUDGETS: maxSteps 40 (BOS-351); wall clock raised 4→6 min (BOS-354); tokens unchanged', async () => {
  const { TUI_BUDGETS } = await import('./proof-tui-agent.mjs')
  assert.equal(TUI_BUDGETS.maxSteps, 40, 'BOS-351 step-budget bump')
  assert.equal(TUI_BUDGETS.maxWallClockMs, 6 * 60 * 1000, 'BOS-354 wall-clock raise (6 min)')
  assert.equal(TUI_BUDGETS.maxTokens, 200_000, 'token budget unchanged (200k)')
})

// ── BOS-251: loop robustness + no-ui-surface honesty valve ────────────────────

test('SYSTEM_PROMPT: forbids quitting the TUI and steers un-showable scenes back on screen', async () => {
  const mod = await import('./proof-tui-agent.mjs')
  assert.match(mod.SYSTEM_PROMPT, /NEVER EXIT THE TUI/)
  assert.match(mod.SYSTEM_PROMPT, /no shell/i)
  assert.match(mod.SYSTEM_PROMPT, /never press q/i)
  assert.match(mod.SYSTEM_PROMPT, /nearest TUI-visible behavior/i)
})

test('evaluateDoneCall: rejects a premature done while scenes remain, accepts once the bound is hit', async () => {
  const { evaluateDoneCall, MAX_DONE_REJECTIONS } = await import('./proof-tui-agent.mjs')
  const scenes = [
    { id: 'scene-01', title: 'One' },
    { id: 'scene-02', title: 'Two' },
  ]
  // Premature pass with a scene still owed → bounded corrective rejection.
  const early = evaluateDoneCall({
    input: { passed: true, summary: 'too early' },
    activeSceneIndex: 0,
    scenes,
    doneRejections: 0,
  })
  assert.equal(early.accept, false)
  assert.equal(early.rejected, true)
  assert.equal(early.done, undefined)
  assert.match(early.toolResult.error, /Only scene 1 of 2/)
  assert.match(early.toolResult.error, /begin_scene\({id:"scene-02"}\)/)

  // passed=false as a per-scene checkpoint is bounced too, with the blocker wording.
  const checkpoint = evaluateDoneCall({
    input: { passed: false, summary: 'scene 1 confirmed' },
    activeSceneIndex: 0,
    scenes,
    doneRejections: 0,
  })
  assert.equal(checkpoint.rejected, true)
  assert.match(checkpoint.toolResult.error, /genuine blocker/)

  // Past the rejection bound, an insistence is accepted so a stuck agent cannot loop forever.
  const insisted = evaluateDoneCall({
    input: { passed: false, summary: 'blocked', evidence: ['nope'] },
    activeSceneIndex: 0,
    scenes,
    doneRejections: MAX_DONE_REJECTIONS,
  })
  assert.equal(insisted.accept, true)
  assert.equal(insisted.rejected, false)
  assert.deepEqual(insisted.done, { passed: false, summary: 'blocked', evidence: ['nope'] })

  // Last scene active → nothing owed → accept immediately.
  const final = evaluateDoneCall({
    input: { passed: true, summary: 'done' },
    activeSceneIndex: 1,
    scenes,
    doneRejections: 0,
  })
  assert.equal(final.accept, true)
  assert.equal(final.rejected, false)
  assert.equal(final.done.passed, true)
})

test('runAgentLoop: premature done(passed=true) is rejected once, then the run continues', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      scenes: [
        { id: 'scene-01', title: 'One' },
        { id: 'scene-02', title: 'Two' },
      ],
      budgets: { maxSteps: 8, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const rejections = []
    let step = 0
    const model = {
      async createMessage({ messages }) {
        step += 1
        if (step === 1) return toolUse('done', { passed: true, summary: 'too early' })
        if (step === 2) {
          const lastUser = messages[messages.length - 1]
          rejections.push(JSON.parse(lastUser.content[0].content))
          return toolUse('begin_scene', { id: 'scene-02' })
        }
        return toolUse('done', { passed: true, summary: 'finished both scenes' })
      },
    }
    const bridge = scriptedBridge({ screens: ['Screen'] })
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
      readCastMs: () => 1000,
    })
    assert.equal(rejections.length, 1, 'first premature done must be rejected')
    assert.match(rejections[0].error, /Only scene 1 of 2/)
    assert.match(rejections[0].error, /begin_scene\({id:"scene-02"}\)/)
    assert.equal(result.agentResult.passed, true, 'run completes after the nudge')
    assert.equal(result.sceneTimings.length, 2, 'scene 2 was begun after the rejection')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: a repeated premature done(passed=true) is accepted (no infinite loop)', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      scenes: [
        { id: 'scene-01', title: 'One' },
        { id: 'scene-02', title: 'Two' },
      ],
      budgets: { maxSteps: 8, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const model = scriptedModel([toolUse('done', { passed: true, summary: 'insisting' })], {
      repeatLast: true,
    })
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge: scriptedBridge(),
      rawDir,
    })
    const { MAX_DONE_REJECTIONS } = await import('./proof-tui-agent.mjs')
    assert.equal(result.agentResult.passed, true)
    assert.equal(
      model.calls,
      MAX_DONE_REJECTIONS + 1,
      'bounded rejections, then the insistence is accepted',
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: a text-only turn is nudged back onto tools instead of ending the run', async () => {
  const { runAgentLoop, MAX_TEXT_ONLY_NUDGES } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      budgets: { maxSteps: 8, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const textTurn = {
      stop_reason: 'end_turn',
      usage: { input_tokens: 5, output_tokens: 5 },
      content: [{ type: 'text', text: 'Let me think about this…' }],
    }
    let sawNudge = null
    let step = 0
    const model = {
      async createMessage({ messages }) {
        step += 1
        if (step === 1) return textTurn
        if (step === 2) {
          sawNudge = messages[messages.length - 1].content
          return toolUse('done', { passed: true, summary: 'ok' })
        }
        return toolUse('done', { passed: true, summary: 'ok' })
      },
    }
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge: scriptedBridge(),
      rawDir,
    })
    assert.equal(result.agentResult.passed, true, 'run recovers from a narration-only turn')
    assert.match(String(sawNudge), /tool calls only/i)
    assert.ok(MAX_TEXT_ONLY_NUDGES >= 1)
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: persistent text-only turns still end the run after the nudge budget', async () => {
  const { runAgentLoop, MAX_TEXT_ONLY_NUDGES } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      budgets: { maxSteps: 10, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const textTurn = {
      stop_reason: 'end_turn',
      usage: { input_tokens: 5, output_tokens: 5 },
      content: [{ type: 'text', text: 'still talking' }],
    }
    const model = scriptedModel([textTurn], { repeatLast: true })
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge: scriptedBridge(),
      rawDir,
    })
    assert.equal(result.agentResult.passed, false)
    assert.equal(model.calls, MAX_TEXT_ONLY_NUDGES + 1, 'bounded: nudges + the final give-up turn')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('extractStillsFromVideo: blank and duplicate screens are skipped', async () => {
  const { extractStillsFromVideo } = await import('./proof-tui-agent.mjs')
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-stills-'))
  try {
    const extracted = []
    const extractStill = async ({ output }) => {
      extracted.push(path.basename(output))
      fs.writeFileSync(output, 'png')
      return true
    }
    const stills = await extractStillsFromVideo({
      screens: [
        { seq: 1, text: 'HOME', castMs: 0 },
        { seq: 2, text: '', castMs: 500 }, // blank — skipped
        { seq: 3, text: 'HOME', castMs: 900 }, // duplicate of 1 — skipped
        { seq: 4, text: 'SETTINGS', castMs: 1400 },
        { seq: 5, text: '   \n  ', castMs: 1800 }, // whitespace-only — skipped
      ],
      sceneForScreen: { 1: 'scene-01', 2: 'scene-01', 3: 'scene-01', 4: 'scene-02', 5: 'scene-02' },
      timeline: { trimMs: 0, segments: [{ startMs: 0, endMs: 2000, speed: 1 }], introMs: 0 },
      mp4Path: '/tmp/fake.mp4',
      captureDir,
      extractStill,
    })
    assert.deepEqual(extracted, ['scene-01-frame-01.png', 'scene-02-frame-04.png'])
    assert.equal(stills.length, 2)
  } finally {
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('extractStillsFromVideo: all-blank screens fall back to the raw list (never zero stills)', async () => {
  const { extractStillsFromVideo } = await import('./proof-tui-agent.mjs')
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-stills-'))
  try {
    const extractStill = async ({ output }) => {
      fs.writeFileSync(output, 'png')
      return true
    }
    const stills = await extractStillsFromVideo({
      screens: [
        { seq: 1, text: '', castMs: 0 },
        { seq: 2, text: '', castMs: 500 },
      ],
      sceneForScreen: { 1: 'scene-01', 2: 'scene-01' },
      timeline: { trimMs: 0, segments: [{ startMs: 0, endMs: 1000, speed: 1 }], introMs: 0 },
      mp4Path: '/tmp/fake.mp4',
      captureDir,
      extractStill,
    })
    assert.equal(stills.length, 2, 'blank-only capture keeps its frames rather than none')
  } finally {
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('extractStillsFromVideo: an exact repeat across a scene boundary still keeps the later scene a still', async () => {
  const { extractStillsFromVideo } = await import('./proof-tui-agent.mjs')
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-stills-'))
  try {
    const extracted = []
    const extractStill = async ({ output }) => {
      extracted.push(path.basename(output))
      fs.writeFileSync(output, 'png')
      return true
    }
    const stills = await extractStillsFromVideo({
      screens: [
        { seq: 1, text: 'HOME', castMs: 0 },
        { seq: 2, text: 'SESSIONS', castMs: 500 },
        // scene-02's first screen is textually identical to scene-01's last:
        // the repeat filter must reset at the boundary so scene-02 is not starved.
        { seq: 3, text: 'SESSIONS', castMs: 1000 },
      ],
      sceneForScreen: { 1: 'scene-01', 2: 'scene-01', 3: 'scene-02' },
      timeline: { trimMs: 0, segments: [{ startMs: 0, endMs: 1500, speed: 1 }], introMs: 0 },
      mp4Path: '/tmp/fake.mp4',
      captureDir,
      extractStill,
    })
    assert.deepEqual(extracted, [
      'scene-01-frame-01.png',
      'scene-01-frame-02.png',
      'scene-02-frame-03.png',
    ])
    assert.equal(
      stills.filter((s) => s.sceneId === 'scene-02').length,
      1,
      'scene-02 must keep its still even though it repeats scene-01 last screen',
    )
  } finally {
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('runTuiAgentProof: generated noUiSurface brief defers as no-ui-surface without driving the TUI', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  try {
    const bridge = scriptedBridge()
    const surfaceRun = await runTuiAgentProof({
      prNumber: 'tuinosurface',
      commit: 'abc1234',
      changedFiles: ['services/bossd/internal/server/foo.go'],
      dryRun: true,
      runContext: { collect: true, runId: 'tui-nosurface', token: 'tok-tui-test' },
      deps: {
        bridge,
        model: {
          async createMessage() {
            throw new Error('agent loop must not run')
          },
        },
        generateBriefFromDiff: async () => ({
          title: 'backend-only change',
          description: 'no TUI screen is affected — daemon-internal retry logic only',
          targetRoutes: [],
          stepsHints: [],
          expectedEvidence: [],
          noUiSurface: true,
        }),
        renderStill: fakeRenderStill(),
        castToVideo: async () => null,
      },
    })
    assert.equal(surfaceRun.noSurface, true)
    assert.equal(surfaceRun.reasonCode, 'no-ui-surface')
    assert.deepEqual(surfaceRun.captureShapes, [])
    assert.equal(surfaceRun.hasFailure, false, 'neutral deferral — never a failure')
    assert.equal(bridge.observeCount, 0, 'bridge never driven')
    assert.equal(bridge.quitCalled, false, 'bridge never spawned/quit')
  } finally {
    cleanupPr('tuinosurface')
  }
})

test('runTuiAgentProof: noUiSurface is IGNORED when the diff touches view code', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  try {
    // Views changed → the valve must not skip capture; the injected loopRunner
    // records that the capture path actually ran.
    let loopRan = false
    const surfaceRun = await runTuiAgentProof({
      prNumber: 'tuiviewsforce',
      commit: 'abc1234',
      changedFiles: ['services/boss/internal/views/home.go'],
      dryRun: true,
      loopRunner: async ({ rawDir }) => {
        loopRan = true
        fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'HOME')
        return {
          agentResult: { passed: true, summary: 'ok', evidence: [], steps: 1 },
          finalScreen: 'HOME',
          captionTimings: [],
          sceneTimings: [{ id: 'scene-01', title: 'S', startMs: 0 }],
          sceneForScreen: { 1: 'scene-01' },
          screens: [{ seq: 1, text: 'HOME', castMs: null }],
        }
      },
      deps: {
        bridge: scriptedBridge(),
        model: {
          async createMessage() {
            throw new Error('loopRunner injected')
          },
        },
        generateBriefFromDiff: async () => ({
          title: 'view change',
          description: 'claims no surface (wrongly)',
          targetRoutes: [],
          stepsHints: [],
          expectedEvidence: ['HOME'],
          noUiSurface: true,
        }),
        renderStill: fakeRenderStill(),
        castToVideo: async () => null,
      },
      runContext: { collect: true, runId: 'tui-viewsforce', token: 'tok-tui-test' },
    })
    assert.equal(loopRan, true, 'capture ran despite the noUiSurface claim')
    assert.equal(surfaceRun.noSurface, false)
  } finally {
    cleanupPr('tuiviewsforce')
  }
})

// ── BOS-251: TUI_CONTEXT_BLOCK ↔ fixture/view drift guard ────────────────────
// The brief generator plans evidence tokens FROM the context block, and the
// evidence gate matches them against what the fixture TUI actually renders. A
// context-block fact that drifts from the Go sources sends every future proof
// chasing strings that never appear (the PR-1240 failure shape). Pin the
// critical facts on both sides.

test('TUI_CONTEXT_BLOCK: fixture facts exist in fixtures.go and view keymaps in the views', async () => {
  const { TUI_CONTEXT_BLOCK } = await import('./proof-tui-agent.mjs')
  const fixturesGo = fs.readFileSync(
    path.join(REPO_ROOT, 'services/boss/internal/fixtures/fixtures.go'),
    'utf8',
  )
  const settingsGo = fs.readFileSync(
    path.join(REPO_ROOT, 'services/boss/internal/views/settings.go'),
    'utf8',
  )
  const statusGo = fs.readFileSync(
    path.join(REPO_ROOT, 'services/boss/internal/views/status.go'),
    'utf8',
  )

  // Every fixture fact the context block advertises must exist in fixtures.go…
  const fixtureFacts = [
    'Add dark mode',
    'Fix login bug',
    'Add rate limiting to public API',
    'Refresh landing page hero',
    'Upgrade to React Navigation 7',
    '/Users/demo/worktrees/my-app/add-dark-mode',
    'work-claude',
    'personal-codex',
    'Daily dependency update',
    'Paused release gate',
    'Paused visual regression',
    'Initial implementation',
    'Final polish pass',
  ]
  // The block hard-wraps long lines, so match with whitespace collapsed.
  const ctx = TUI_CONTEXT_BLOCK.replace(/\s+/g, ' ')
  for (const fact of fixtureFacts) {
    assert.ok(ctx.includes(fact), `context block must mention ${fact}`)
    assert.ok(fixturesGo.includes(fact), `fixtures.go must still provide ${fact}`)
  }

  // …and the promised keymap/action-bar tokens must exist in the views.
  for (const key of ['[a]ccounts', '[c]ron', '[r]epos', '[t]rash']) {
    assert.ok(TUI_CONTEXT_BLOCK.includes(`"${key}"`), `context block must quote ${key}`)
    assert.ok(settingsGo.includes(`"${key}"`), `views/settings.go must still render ${key}`)
  }
  assert.ok(TUI_CONTEXT_BLOCK.includes('"archiving"'), 'context block quotes the archiving label')
  assert.ok(statusGo.includes('"archiving"'), 'views/status.go must still render "archiving"')
})

test('runAgentLoop: done(passed=false) used as a scene checkpoint is rejected, genuine repeat accepted', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const brief = {
      description: 'prove it',
      scenes: [
        { id: 'scene-01', title: 'One' },
        { id: 'scene-02', title: 'Two' },
      ],
      budgets: { maxSteps: 8, maxWallClockMs: 60_000, maxTokens: 100_000 },
    }
    const rejections = []
    let step = 0
    const model = {
      async createMessage({ messages }) {
        step += 1
        if (step === 1) return toolUse('done', { passed: false, summary: 'Scene 1 confirmed!' })
        if (step === 2) {
          rejections.push(JSON.parse(messages[messages.length - 1].content[0].content))
          return toolUse('begin_scene', { id: 'scene-02' })
        }
        return toolUse('done', { passed: true, summary: 'both scenes shown' })
      },
    }
    const result = await runAgentLoop({
      brief,
      model: 'test-model',
      modelDep: model,
      bridge: scriptedBridge(),
      rawDir,
      readCastMs: () => 500,
    })
    assert.equal(rejections.length, 1)
    assert.match(rejections[0].error, /ends the WHOLE recording/)
    assert.match(rejections[0].error, /If you are NOT blocked/)
    assert.equal(result.agentResult.passed, true, 'run recovered and completed both scenes')
    assert.equal(result.sceneTimings.length, 2)
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})
