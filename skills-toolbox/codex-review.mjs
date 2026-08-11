#!/usr/bin/env node

import { spawn, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  assemblePrompt,
  boundedStderrTail,
  buildExecArgv,
  classifyProbe,
  interpretResult,
  removeTierAProbeRoot,
  reviewPreamble,
  resolveAgentBin,
  sanitizeOutput,
} from './cross-review-lib.mjs'

// Cross-model "outside voice" review helper for bs-implement.
// Shells out to the Codex CLI (`codex exec`) read-only over a git diff and
// returns sanitized review text.  The agent-agnostic machinery lives in
// the sibling cross-review-lib.mjs; this file is the thin codex adapter + the
// CLI entrypoint.  Vendored into the boss-review skill toolbox.
//
// Pinned surface: codex-cli 0.142.0
// `codex login status` exits 0 when authenticated, non-zero when not.

// Re-export the pure primitives the test-suite (and downstream callers) consume
// from this module's public surface.
export { classifyProbe, sanitizeOutput }

// ---------------------------------------------------------------------------
// Codex adapter spec — feeds buildExecArgv. `codex exec` invocation shape
// (codex-cli 0.142.0):
//   codex exec -C <repo> -s read-only -c model_reasoning_effort="high" <prompt>
//
// NOTE: `codex exec` is already non-interactive and never prompts for approval,
// so it has NO `-a`/`--ask-for-approval` flag (that lives only on the top-level
// `codex` command). Passing `-a never` makes codex exit with
// "unexpected argument '-a' found" and produce no review — read-only sandbox
// (`-s read-only`) is what actually prevents any writes here.
// ---------------------------------------------------------------------------
const CODEX_ADAPTER = {
  subcommand: 'exec',
  flags: ['-s', 'read-only', '-c', 'model_reasoning_effort="high"'],
}

function isDescendant(pathname, ancestor) {
  return (
    pathname === ancestor ||
    ancestor === path.parse(ancestor).root ||
    pathname.startsWith(`${ancestor}${path.sep}`)
  )
}

// Codex's workspace-write configuration can confine writes, but its supported
// settings do not provide a readable-path allowlist. Tier A executes copied,
// untrusted code, so write-only confinement is insufficient: it could read
// host credentials and return them in the review output. Keep Tier A disabled
// until Codex exposes read confinement alongside writable_roots.
function canConfineCodexTierAReads() {
  return false
}

// Resolve the checkout once before Tier A passes it to prompts or environment.
// A relative --repo would otherwise be interpreted from Tier A's scratch cwd.
export function canonicalizeCheckoutRoot(repo) {
  if (typeof repo !== 'string' || repo === '') return ''
  try {
    return fs.realpathSync(repo)
  } catch {
    return ''
  }
}

// createCodexTierAProbeRoot(repo) → string
//
// Codex's workspace-write sandbox normally allows the system temporary roots
// in addition to its working directory. A workspace below /tmp would therefore
// make a /tmp checkout writable too. Keep the per-run workspace in a canonical
// user-cache directory instead, and reject it if the host topology cannot prove
// that it is outside both the checkout and the system temporary root.
export function createCodexTierAProbeRoot(
  repo,
  { homeDir = os.homedir(), tmpDir = os.tmpdir() } = {},
) {
  let checkoutRoot
  let homeRoot
  let tmpRoot
  let cacheRoot
  try {
    checkoutRoot = fs.realpathSync(repo)
    homeRoot = fs.realpathSync(homeDir)
    tmpRoot = fs.realpathSync(tmpDir)
    // Start from the canonical, already-existing home root. A fresh account
    // need not have ~/.cache yet, so resolving that missing child before mkdir
    // would incorrectly disable Tier A. Validate the ancestor first: only then
    // may we create ~/.cache, and the post-create canonicalization catches a
    // pre-existing symlink or other unexpected topology.
    if (isDescendant(homeRoot, tmpRoot) || isDescendant(homeRoot, checkoutRoot)) {
      return ''
    }
    const cacheParentPath = path.join(homeRoot, '.cache')
    fs.mkdirSync(cacheParentPath, { recursive: true, mode: 0o700 })
    const cacheParent = fs.realpathSync(cacheParentPath)
    if (
      !isDescendant(cacheParent, homeRoot) ||
      isDescendant(cacheParent, tmpRoot) ||
      isDescendant(cacheParent, checkoutRoot)
    ) {
      return ''
    }
    cacheRoot = path.join(cacheParent, 'boss-review')
    fs.mkdirSync(cacheRoot, { recursive: true, mode: 0o700 })
    cacheRoot = fs.realpathSync(cacheRoot)
  } catch {
    return ''
  }

  if (
    !isDescendant(cacheRoot, homeRoot) ||
    isDescendant(cacheRoot, tmpRoot) ||
    isDescendant(cacheRoot, checkoutRoot)
  ) {
    return ''
  }

  let probeRoot = ''
  try {
    probeRoot = fs.realpathSync(fs.mkdtempSync(path.join(cacheRoot, 'tier-a-')))
    if (
      !isDescendant(probeRoot, cacheRoot) ||
      isDescendant(probeRoot, tmpRoot) ||
      isDescendant(probeRoot, checkoutRoot)
    ) {
      removeTierAProbeRoot(probeRoot)
      return ''
    }
    return probeRoot
  } catch {
    removeTierAProbeRoot(probeRoot)
    return ''
  }
}

function codexAdapter(tierAProbeRoot) {
  if (typeof tierAProbeRoot !== 'string' || !path.isAbsolute(tierAProbeRoot)) {
    return CODEX_ADAPTER
  }
  // The scratch root, not the reviewed checkout, is Codex's workspace. This
  // grants Tier A exactly one writable tree while the checkout remains
  // readable for inspecting the real gate and production feed. Codex otherwise
  // includes $TMPDIR and /tmp in workspace-write by default, so explicitly
  // exclude both and allow only this per-run root.
  return {
    subcommand: 'exec',
    flags: [
      '--skip-git-repo-check',
      '-s',
      'workspace-write',
      '-c',
      'model_reasoning_effort="high"',
      '-c',
      `sandbox_workspace_write.writable_roots=${JSON.stringify([tierAProbeRoot])}`,
      '-c',
      'sandbox_workspace_write.exclude_tmpdir_env_var=true',
      '-c',
      'sandbox_workspace_write.exclude_slash_tmp=true',
    ],
  }
}

// Default review timeout, env-overridable via BOSS_CROSS_REVIEW_TIMEOUT_MS.
// Raised from the original 120_000 to 300_000 (cross-model review over a large
// diff with high reasoning effort routinely needs more than two minutes).
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
// Parses BOSS_CROSS_REVIEW_TIMEOUT_MS; falls back to the default unless the
// value is a run of PLAIN DECIMAL DIGITS denoting a positive number. Trailing
// garbage like "100abc" and fractional "1.5" are rejected to the default rather
// than silently truncated to 100 / 1, as they always were.
//
// The digits-only shape gate is load bearing, not belt-and-braces on top of
// Number(). Number() alone also accepts `1.8e6`, `0x2710`, `+600000` and
// whitespace-padded ` 1800000` — forms no POSIX shell glob can express, and the
// budget gates that PRICE this timeout are shell. boss-build's Step 6b gate
// (its review-stack reference) normalizes with a digits-only `case` glob plus an
// explicit base-10 `$(( 10# ))` radix, so every form Number() accepted but the
// glob cannot is one where the gate reserves a different number of minutes than
// the helper then spends: `1.8e6` reserves 5 minutes for a leg granted 30, and
// the run eats its own post-review reserve. Narrowing the accepted set to what
// BOTH readers parse identically closes that divergence at the source, instead
// of teaching the shell one more literal form per round. `0600000` stays valid
// (600000 ms): it is plain digits, and the gate's `10#` radix reads it the same.
export function resolveTimeoutMs(env = process.env) {
  const raw = env?.BOSS_CROSS_REVIEW_TIMEOUT_MS
  if (raw == null || !/^[0-9]+$/.test(String(raw))) return DEFAULT_TIMEOUT_MS
  const n = Number(raw)
  return Number.isInteger(n) && n > 0 ? n : DEFAULT_TIMEOUT_MS
}

// ---------------------------------------------------------------------------
// resolveCodexBin(env) → string | null
//
// Thin codex wrapper over the generic resolver. See resolveAgentBin for the
// absolute-override / PATH-fallback contract.
// ---------------------------------------------------------------------------
export function resolveCodexBin(env) {
  return resolveAgentBin(env, { overrideVar: 'BOSS_CODEX_BIN', binName: 'codex' })
}

// ---------------------------------------------------------------------------
// probe({ env, timeoutMs }) → Promise<string>
//
// Resolves the codex binary, then runs `codex login status` with a short
// timeout and captured stdio.  Returns a classification string.  Never throws;
// ambiguous results → 'error'.
// ---------------------------------------------------------------------------
export async function probe({ env = process.env, timeoutMs = 5000 } = {}) {
  const bin = resolveCodexBin(env)
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
      // not draining stderr would deadlock a chatty codex past the ~64KB pipe
      // buffer until the timeout fires.
      child = spawn(bin, ['login', 'status'], {
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
      settle(classifyProbe({ spawnError: null, status: code, signal: sig }))
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
// buildCodexArgs({ base, head, repo }) → string[]
//
// Pure argv builder for `codex exec` — no diff embedding (it cannot shell out).
// Uses instruct-mode prompt (review the range with a no-explore scope guard).
// `run()` builds an embed-mode variant when it can fetch the diff; this pure
// helper keeps the structural test-surface stable.
// ---------------------------------------------------------------------------
export function buildCodexArgs({ base, head, repo, falsificationReference, tierAProbeRoot = '' }) {
  const tierA =
    canConfineCodexTierAReads() &&
    typeof tierAProbeRoot === 'string' &&
    path.isAbsolute(tierAProbeRoot)
  const prompt = assemblePrompt({
    preamble: reviewPreamble(falsificationReference, { checkoutRoot: repo, tierAProbeRoot }),
    range: `${base}...${head}`,
    tierA,
  })
  return buildExecArgv(codexAdapter(tierA ? tierAProbeRoot : ''), {
    base,
    head,
    repo: tierA ? tierAProbeRoot : repo,
    prompt,
  })
}

// buildCodexArgsWithDiff — adaptive argv builder used by run().
// When a diff is available and under the embed cap, embed it directly into the
// prompt (so the reviewer never has to explore the tree); otherwise fall back
// to the pure instruct-mode argv.
function buildCodexArgsWithDiff({
  base,
  head,
  repo,
  diffText,
  falsificationReference,
  tierAProbeRoot = '',
}) {
  const tierA =
    canConfineCodexTierAReads() &&
    typeof tierAProbeRoot === 'string' &&
    path.isAbsolute(tierAProbeRoot)
  if (typeof diffText === 'string' && diffText !== '') {
    const prompt = assemblePrompt({
      preamble: reviewPreamble(falsificationReference, { checkoutRoot: repo, tierAProbeRoot }),
      range: `${base}...${head}`,
      diffText,
      tierA,
    })
    return buildExecArgv(codexAdapter(tierA ? tierAProbeRoot : ''), {
      base,
      head,
      repo: tierA ? tierAProbeRoot : repo,
      prompt,
    })
  }
  return buildCodexArgs({ base, head, repo, falsificationReference, tierAProbeRoot })
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
// run({ env, base, head, repo, timeoutMs, maxBytes, maxStderrBytes }) →
//   Promise<{ ok: boolean, output: string, stderr: string, timedOut: boolean }>
//
// Resolves the codex binary, best-effort embeds the diff, spawns `codex exec`
// with stdin=/dev/null and a process-group timeout kill.  Captures stdout,
// sanitizes it, and returns the result.  Never throws; non-zero exit is
// captured, not thrown.  When `timeoutMs` is omitted the env-overridable
// default (BOSS_CROSS_REVIEW_TIMEOUT_MS) applies.
//
// stderr IS captured but only as a bounded, sanitized *tail* (last
// `maxStderrBytes`): the pipe is continuously drained so a chatty codex can
// never deadlock past the ~64KB buffer, yet on `ok=false` the caller still gets
// the actionable diagnostic (e.g. an "unexpected argument" CLI-surface error)
// instead of a silent empty result.
// ---------------------------------------------------------------------------
export async function run({
  env = process.env,
  base,
  head,
  repo = process.cwd(),
  timeoutMs,
  maxBytes = 65536,
  maxStderrBytes = 4096,
  falsificationReference = env.BOSS_REVIEW_FALSIFICATION_REFERENCE,
} = {}) {
  const bin = resolveCodexBin(env)
  if (bin === null) {
    return { ok: false, output: '', stderr: '', timedOut: false }
  }

  const effectiveTimeoutMs = typeof timeoutMs === 'number' ? timeoutMs : resolveTimeoutMs(env)
  const tierARequested =
    typeof falsificationReference === 'string' && path.isAbsolute(falsificationReference)
  const tierAEnabled = tierARequested && canConfineCodexTierAReads()
  const checkoutRoot = tierAEnabled ? canonicalizeCheckoutRoot(repo) : repo

  // Keep the read-only outside voice available when Tier A cannot be confined.
  // The prompt requires it to report the blocked probe rather than attempting
  // an unsafe fallback. Only an enabled Tier A needs a canonical checkout.
  if (tierAEnabled && checkoutRoot === '') {
    return { ok: false, output: '', stderr: '', timedOut: false }
  }

  // Feed the diff, don't make the agent fetch it. Best-effort and failure-safe:
  // a non-git `repo` (as in the unit tests) just yields '' → instruct-mode.
  // Diff collection is synchronous and runs before the agent watchdog, so bound
  // it to a small slice of the budget and subtract the elapsed time — keeping
  // run()'s total wall time under effectiveTimeoutMs rather than diff-time + it.
  const diffBudgetMs = Math.min(effectiveTimeoutMs, DIFF_COLLECTION_BUDGET_MS)
  const diffStart = Date.now()
  const diffText = bestEffortDiff(checkoutRoot, base, head, diffBudgetMs)
  const agentTimeoutMs = Math.max(0, effectiveTimeoutMs - (Date.now() - diffStart))
  const tierAProbeRoot = tierAEnabled ? createCodexTierAProbeRoot(checkoutRoot) : ''
  if (tierAEnabled && !tierAProbeRoot) {
    return { ok: false, output: '', stderr: '', timedOut: false }
  }
  const args = buildCodexArgsWithDiff({
    base,
    head,
    repo: checkoutRoot,
    diffText,
    falsificationReference,
    tierAProbeRoot,
  })

  return new Promise((resolve) => {
    let settled = false
    let timer = null
    let timedOut = false
    // Bounded HEAD retention for stdout. We only ever return the head of stdout
    // (sanitizeOutput head-truncates to maxBytes), so once we hold comfortably
    // more than maxBytes of raw bytes we stop retaining further chunks — the pipe
    // keeps draining (the 'data' event already consumed each chunk) so a chatty or
    // buggy codex can't grow memory without bound before `close`. Headroom is
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
      removeTierAProbeRoot(tierAProbeRoot)
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
      // stderr pipe drained (not ['ignore']-but-undrained) so a chatty codex
      // can't deadlock past the ~64KB buffer; the rolling compaction keeps the
      // retained bytes bounded.
      child = spawn(bin, args, {
        env:
          tierAProbeRoot === ''
            ? env
            : {
                ...env,
                BOSS_REVIEW_CHECKOUT_ROOT: repo,
                BOSS_REVIEW_TIER_A_ROOT: tierAProbeRoot,
              },
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
      falsificationReference: flags['falsification-reference'],
    })
    if (result.output) process.stdout.write(`${result.output}\n`)
    if (!result.ok) {
      // Surface the diagnostic tail (CLI-surface errors, auth failures, etc.) so
      // a failed run is debuggable instead of silently empty. Goes to stderr to
      // keep stdout review-text-only.
      const reason = result.timedOut ? 'timed out' : 'codex exec failed'
      const tail = result.stderr ? `\n${result.stderr}` : ''
      process.stderr.write(`codex-review: ${reason}${tail}\n`)
      process.exitCode = 1
    }
    return
  }

  throw new Error(`unknown command: ${cmd ?? '(none)'} (expected "probe" or "run")`)
}

import { isMainModule } from './main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  })
}
