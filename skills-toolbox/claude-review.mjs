#!/usr/bin/env node

import { spawn, spawnSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  assemblePrompt,
  boundedStderrTail,
  classifyProbe,
  interpretResult,
  REVIEW_PREAMBLE,
  resolveAgentBin,
  sanitizeOutput,
} from './cross-review-lib.mjs'

// Cross-model "outside voice" review helper for bs-implement — the
// claude-direction sibling of codex-review.mjs. Shells out to the
// Claude Code CLI (`claude -p`) over a git diff and returns sanitized review
// text. The agent-agnostic machinery lives in the sibling cross-review-lib.mjs;
// this file is the thin claude adapter + the CLI entrypoint.
//
// Pinned surface: claude-cli 2.x (`claude -p "<prompt>"`).
// `claude --version` exits 0 when the CLI is runnable — a cheap readiness
// signal. Claude has no separate `login status` subcommand, so auth failures
// surface later as a run skip (the non-fatal contract), which is fine.
//
// Unlike codex, claude has NO `-C <dir>` flag: the working directory is set via
// the spawn `cwd` option, not an argv element. So buildExecArgv (which hard-codes
// codex's `-C <repo>` shape) is NOT a fit here — this file builds claude's argv
// directly. `--permission-mode plan` keeps the reviewer read-only (it cannot
// create/edit/delete files), the analog of codex's `-s read-only` sandbox; and
// because the diff is EMBEDDED in the prompt the reviewer needs no file-system
// tools at all (no recursion into bs-review, no tree exploration).

// Re-export the pure primitives the test-suite (and downstream callers) consume
// from this module's public surface.
export { classifyProbe, sanitizeOutput }

// Default review timeout, env-overridable via BOSS_CROSS_REVIEW_TIMEOUT_MS.
// Shared env var + default with codex-review.mjs so a single knob tunes both
// cross-model reviewers.
const DEFAULT_TIMEOUT_MS = 300_000

// Cap on an embedded diff. Above this we fall back to instruct-mode (ask the
// reviewer to read the range itself) rather than ballooning the prompt.
const EMBED_DIFF_LIMIT_BYTES = 200 * 1024

// Cap on the synchronous diff-collection step. bestEffortDiff runs *before* the
// agent watchdog is armed, so without its own small bound a hung `git diff`
// could burn the entire review timeout before the agent even starts — making
// run()'s wall time up to 2× timeoutMs. We give diff collection a small slice
// of the budget and subtract whatever it actually spends from the agent timeout.
const DIFF_COLLECTION_BUDGET_MS = 30_000

// resolveTimeoutMs(env) → positive integer ms.
// Parses BOSS_CROSS_REVIEW_TIMEOUT_MS; falls back to the default when unset or
// not a positive integer. Uses strict Number() parsing (not the lenient
// parseInt) so trailing garbage like "100abc" is rejected to the default
// rather than silently truncated to 100. Identical contract to codex-review.mjs.
export function resolveTimeoutMs(env = process.env) {
  const n = Number(env?.BOSS_CROSS_REVIEW_TIMEOUT_MS)
  return Number.isInteger(n) && n > 0 ? n : DEFAULT_TIMEOUT_MS
}

// ---------------------------------------------------------------------------
// resolveClaudeBin(env) → string | null
//
// Thin claude wrapper over the generic resolver. See resolveAgentBin for the
// absolute-override / PATH-fallback contract.
// ---------------------------------------------------------------------------
export function resolveClaudeBin(env) {
  return resolveAgentBin(env, { overrideVar: 'BOSS_CLAUDE_BIN', binName: 'claude' })
}

// ---------------------------------------------------------------------------
// bareModeArgs(env) → ['--bare'] | []
//
// `--bare` runs claude in minimal mode: it skips auto-discovery of hooks, LSP,
// plugin/MCP sync, auto-memory, and CLAUDE.md. Without it, running the second
// voice in a developer/CI env that has user/project Claude Code customizations
// would auto-load those hooks/plugins — which can mutate the tree or hang even
// though this helper is advertised as a read-only, diff-scoped reviewer
// (`--permission-mode plan` blocks the *agent* from writing files, but not a
// hook's side effects). So enable `--bare` to make the reviewer hermetic.
//
// Caveat: `--bare` forces Anthropic auth to strictly ANTHROPIC_API_KEY (OAuth
// and keychain are never read). Enabling it unconditionally would break the
// reviewer — silently producing no review — on any box that authenticates via
// OAuth/keychain. So only add it when ANTHROPIC_API_KEY is present, which is
// exactly the CI/cron context where untrusted auto-discovered config is the
// real risk; OAuth/keychain sessions keep the existing (non-bare) invocation.
// ---------------------------------------------------------------------------
function bareModeArgs(env) {
  const key = env?.ANTHROPIC_API_KEY
  return typeof key === 'string' && key.trim() !== '' ? ['--bare'] : []
}

// ---------------------------------------------------------------------------
// claudeArgv(prompt, env) → string[]
//
// Pure argv shape for claude's headless print mode:
//   claude [--bare] -p --permission-mode plan "<prompt>"
//
//   • --bare                → hermetic mode when viable (see bareModeArgs).
//   • -p / --print          → non-interactive: print the response and exit.
//   • --permission-mode plan → read-only session; claude cannot create/edit/
//                              delete files (the codex `-s read-only` analog).
//
// No -C/working-dir arg: claude has none — the caller sets cwd on the spawn.
// The base/head SHAs are carried inside `prompt` (range string / embedded diff),
// never as argv, so this builder only needs the prompt (+ env for bare mode).
// ---------------------------------------------------------------------------
function claudeArgv(prompt, env = process.env) {
  return [...bareModeArgs(env), '-p', '--permission-mode', 'plan', prompt]
}

// ---------------------------------------------------------------------------
// buildClaudeArgs({ base, head }) → string[]
//
// Pure argv builder for `claude -p` — instruct-mode prompt (review the range
// with a no-explore scope guard), no diff embedding. `run()` builds an
// embed-mode variant when it can fetch the diff; this pure helper keeps the
// structural test-surface stable.
// ---------------------------------------------------------------------------
export function buildClaudeArgs({ base, head, env = process.env }) {
  const prompt = assemblePrompt({ preamble: REVIEW_PREAMBLE, range: `${base}...${head}` })
  return claudeArgv(prompt, env)
}

// buildClaudeArgsWithDiff — adaptive argv builder used by run().
// When a diff is available and under the embed cap, embed it directly into the
// prompt (so the reviewer never has to explore the tree); otherwise fall back
// to the pure instruct-mode argv. `env` gates bare mode (see bareModeArgs).
function buildClaudeArgsWithDiff({ base, head, diffText, env = process.env }) {
  if (typeof diffText === 'string' && diffText !== '') {
    const prompt = assemblePrompt({
      preamble: REVIEW_PREAMBLE,
      range: `${base}...${head}`,
      diffText,
    })
    return claudeArgv(prompt, env)
  }
  return buildClaudeArgs({ base, head, env })
}

// bestEffortDiff(repo, base, head, timeoutMs) → string
// Best-effort `git diff base...head` in `repo`. NEVER throws and never lets a
// non-git `repo` (or any git failure) break the caller — returns '' on any
// failure or empty/oversized diff, which routes run() to instruct-mode.
// This runs synchronously *before* the agent's timeout watchdog is armed, so it
// carries its own bound: --no-ext-diff/--no-textconv neutralize user-configured
// external diff drivers and textconv filters (the usual way `git diff` hangs),
// and a hard `timeout` (bounded by the caller's deadline) force-kills anything
// still running so the "bounded" run can never hang before it even starts.
function bestEffortDiff(repo, base, head, timeoutMs) {
  try {
    const result = spawnSync(
      'git',
      ['diff', '--no-ext-diff', '--no-textconv', `${base}...${head}`],
      {
        cwd: repo,
        encoding: 'utf8',
        // Generous buffer: we still gate on EMBED_DIFF_LIMIT_BYTES below. An
        // over-limit diff that trips maxBuffer surfaces as status!==0 → ''.
        maxBuffer: 4 * 1024 * 1024,
        // Hard backstop. A timeout leaves status=null (!== 0) → '' → instruct-mode.
        timeout: typeof timeoutMs === 'number' && timeoutMs > 0 ? timeoutMs : undefined,
        killSignal: 'SIGKILL',
      },
    )
    if (result.status !== 0 || typeof result.stdout !== 'string') return ''
    if (Buffer.byteLength(result.stdout, 'utf8') >= EMBED_DIFF_LIMIT_BYTES) return ''
    return result.stdout
  } catch {
    // Not a git repo, git missing, buffer exceeded, etc. — instruct-mode.
    return ''
  }
}

// ---------------------------------------------------------------------------
// probe({ env, timeoutMs }) → Promise<string>
//
// Resolves the claude binary, then runs `claude --version` with a short timeout
// and discarded stdio. Returns a classification string via classifyProbe.
// Never throws; ambiguous results → 'error'. (claude has no `login status`
// equivalent, so version-exit-0 ⇒ ready is the right cheap readiness signal.)
// ---------------------------------------------------------------------------
export async function probe({ env = process.env, timeoutMs = 5000 } = {}) {
  const bin = resolveClaudeBin(env)
  if (bin === null) {
    return classifyProbe({ spawnError: { code: 'ENOENT' }, status: null, signal: null })
  }

  return new Promise((resolve) => {
    let settled = false
    let timer = null

    const settle = (result) => {
      if (settled) return
      settled = true
      if (timer) {
        clearTimeout(timer)
        timer = null
      }
      resolve(result)
    }

    let child
    try {
      // We only need the exit code. Discard stdout/stderr entirely — piping and
      // not draining stderr would deadlock a chatty claude past the ~64KB pipe
      // buffer until the timeout fires.
      child = spawn(bin, ['--version'], {
        stdio: ['ignore', 'ignore', 'ignore'],
        detached: true,
      })
    } catch (err) {
      settle(classifyProbe({ spawnError: err, status: null, signal: null }))
      return
    }

    child.on('error', (err) => {
      settle(classifyProbe({ spawnError: err, status: null, signal: null }))
    })

    child.on('close', (code, sig) => {
      // `claude --version` has no auth semantics: a non-zero exit is a broken
      // CLI, not "not authenticated" (codex's meaning), so classify it 'error'.
      settle(classifyProbe({ spawnError: null, status: code, signal: sig, nonZeroStatus: 'error' }))
    })

    timer = setTimeout(() => {
      // Timed out — kill the whole process group. ESRCH (group already gone)
      // and any other kill error are intentionally swallowed: we are tearing
      // down and will resolve 'error' regardless.
      try {
        process.kill(-child.pid, 'SIGKILL')
      } catch {
        // intentionally ignored during teardown
      }
      settle('error')
    }, timeoutMs)
  })
}

// ---------------------------------------------------------------------------
// run({ env, base, head, repo, timeoutMs, maxBytes, maxStderrBytes }) →
//   Promise<{ ok: boolean, output: string, stderr: string, timedOut: boolean }>
//
// Resolves the claude binary, best-effort embeds the diff, spawns `claude -p`
// with cwd=repo, stdin=/dev/null and a process-group timeout kill. Captures
// stdout, sanitizes it, and returns the result. Never throws; non-zero exit is
// captured, not thrown. When `timeoutMs` is omitted the env-overridable default
// (BOSS_CROSS_REVIEW_TIMEOUT_MS) applies.
//
// stderr IS captured but only as a bounded, sanitized *tail* (last
// `maxStderrBytes`): the pipe is continuously drained so a chatty claude can
// never deadlock past the ~64KB buffer, yet on `ok=false` the caller still gets
// the actionable diagnostic instead of a silent empty result.
// ---------------------------------------------------------------------------
export async function run({
  env = process.env,
  base,
  head,
  repo = process.cwd(),
  timeoutMs,
  maxBytes = 65536,
  maxStderrBytes = 4096,
} = {}) {
  const bin = resolveClaudeBin(env)
  if (bin === null) {
    return { ok: false, output: '', stderr: '', timedOut: false }
  }

  const effectiveTimeoutMs = typeof timeoutMs === 'number' ? timeoutMs : resolveTimeoutMs(env)

  // Feed the diff, don't make the agent fetch it. Best-effort and failure-safe:
  // a non-git `repo` (as in the unit tests) just yields '' → instruct-mode.
  // Diff collection is synchronous and runs before the agent watchdog, so bound
  // it to a small slice of the budget and subtract the elapsed time — keeping
  // run()'s total wall time under effectiveTimeoutMs rather than diff-time + it.
  const diffBudgetMs = Math.min(effectiveTimeoutMs, DIFF_COLLECTION_BUDGET_MS)
  const diffStart = Date.now()
  const diffText = bestEffortDiff(repo, base, head, diffBudgetMs)
  const agentTimeoutMs = Math.max(0, effectiveTimeoutMs - (Date.now() - diffStart))
  const args = buildClaudeArgsWithDiff({ base, head, diffText, env })

  return new Promise((resolve) => {
    let settled = false
    let timer = null
    let timedOut = false
    // Bounded HEAD retention for stdout. We only ever return the head of stdout
    // (sanitizeOutput head-truncates to maxBytes), so once we hold comfortably
    // more than maxBytes of raw bytes we stop retaining further chunks — the pipe
    // keeps draining (the 'data' event already consumed each chunk) so a chatty or
    // buggy claude can't grow memory without bound before `close`. Headroom is
    // generous because sanitizeOutput strips escape/control bytes, so ANSI-heavy
    // output needs more raw bytes to still fill maxBytes of clean text.
    const maxStdoutRawBytes = maxBytes * 8
    const chunks = []
    let outBytes = 0
    let outCapped = false
    // Rolling bounded tail of stderr (memory stays bounded even under a 200KB
    // flood while preserving the most recent, most-diagnostic bytes).
    const stderrTail = boundedStderrTail(maxStderrBytes)

    const settle = (result) => {
      if (settled) return
      settled = true
      if (timer) {
        clearTimeout(timer)
        timer = null
      }
      // NOTE: we deliberately do NOT cancel the pending SIGKILL escalation here.
      // It is only ever armed after a timeout SIGTERM, and the group LEADER
      // exiting first (its `close` resolves run()) does not mean the rest of the
      // group is gone — a child that ignores SIGTERM would leak past the review
      // timeout. We let the escalation reap the whole group; see the timeout
      // handler below.
      resolve(result)
    }

    let child
    try {
      // Capture stdout, and DRAIN stderr into a bounded tail. We must keep the
      // stderr pipe drained (not ['ignore']-but-undrained) so a chatty claude
      // can't deadlock past the ~64KB buffer; the rolling compaction keeps the
      // retained bytes bounded. cwd=repo replaces codex's `-C <repo>` arg.
      child = spawn(bin, args, {
        cwd: repo,
        stdio: ['ignore', 'pipe', 'pipe'],
        detached: true,
      })
    } catch {
      settle({ ok: false, output: '', stderr: '', timedOut: false })
      return
    }

    child.stdout.on('data', (chunk) => {
      // Already hold enough head bytes — keep draining the pipe but drop the rest
      // so retained memory stays bounded at maxStdoutRawBytes.
      if (outCapped) return
      chunks.push(chunk)
      outBytes += chunk.length
      if (outBytes >= maxStdoutRawBytes) outCapped = true
    })

    child.stderr.on('data', (chunk) => {
      stderrTail.push(chunk)
    })

    child.on('error', () => {
      const raw = Buffer.concat(chunks).toString('utf8')
      settle({
        ok: false,
        output: sanitizeOutput(raw, { maxBytes }),
        stderr: stderrTail.tail(),
        timedOut,
      })
    })

    child.on('close', (code, signal) => {
      const raw = Buffer.concat(chunks).toString('utf8')
      const output = sanitizeOutput(raw, { maxBytes })
      // interpretResult is the single source of truth for ok/skip detection.
      const verdict = interpretResult({ code, signal, stdout: output, timedOut })
      settle({ ok: verdict.ok, output, stderr: stderrTail.tail(), timedOut })
    })

    timer = setTimeout(() => {
      timedOut = true
      // SIGTERM grace, then SIGKILL the whole group. ESRCH (group already gone)
      // and any other kill error are intentionally swallowed during teardown.
      try {
        process.kill(-child.pid, 'SIGTERM')
      } catch {
        // intentionally ignored during teardown
      }
      // Escalate to SIGKILL on the whole group after a grace period. This stays
      // armed even once run() has resolved (e.g. the leader exited on SIGTERM):
      // surviving children that ignore SIGTERM must still be reaped, otherwise
      // they outlive the review timeout. The trade-off is keeping the event loop
      // alive for ~200ms on the (already slow) timeout path — acceptable to
      // guarantee the process group is gone.
      setTimeout(() => {
        try {
          process.kill(-child.pid, 'SIGKILL')
        } catch {
          // intentionally ignored during teardown
        }
      }, 200)
    }, agentTimeoutMs)
  })
}

// ---------------------------------------------------------------------------
// CLI dispatch
// ---------------------------------------------------------------------------

function parseFlags(rest) {
  const flags = {}
  for (let i = 0; i < rest.length; i += 1) {
    const key = rest[i]
    if (typeof key !== 'string' || !key.startsWith('--')) continue
    const next = rest[i + 1]
    if (typeof next !== 'string' || next.startsWith('--')) {
      flags[key.slice(2)] = true
    } else {
      flags[key.slice(2)] = next
      i += 1
    }
  }
  return flags
}

async function main(argv) {
  const [cmd, ...rest] = argv

  if (cmd === 'probe') {
    const result = await probe({ env: process.env })
    process.stdout.write(`${result}\n`)
    return
  }

  if (cmd === 'run') {
    const flags = parseFlags(rest)
    if (!flags.base) throw new Error('--base <SHA> is required')
    if (!flags.head) throw new Error('--head <SHA> is required')
    const result = await run({
      env: process.env,
      base: flags.base,
      head: flags.head,
      repo: flags.repo ?? process.cwd(),
    })
    if (result.output) process.stdout.write(`${result.output}\n`)
    if (!result.ok) {
      // Surface the diagnostic tail (CLI-surface errors, auth failures, etc.) so
      // a failed run is debuggable instead of silently empty. Goes to stderr to
      // keep stdout review-text-only.
      const reason = result.timedOut ? 'timed out' : 'claude -p failed'
      const tail = result.stderr ? `\n${result.stderr}` : ''
      process.stderr.write(`claude-review: ${reason}${tail}\n`)
      process.exitCode = 1
    }
    return
  }

  throw new Error(`unknown command: ${cmd ?? '(none)'} (expected "probe" or "run")`)
}

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)

if (invokedDirectly) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  })
}
