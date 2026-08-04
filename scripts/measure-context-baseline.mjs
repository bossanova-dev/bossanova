// Session-launch context baseline harness (BOS-671).
//
// Measures what a session pays BEFORE its first tool call — the system prompt,
// tool schemas, skills catalogue, agent catalogue and CLAUDE.md a turn reads
// before it does anything. One real model call per named variant, then a table
// of variant -> tokens -> delta against the manifest's chosen baseline.
//
// The ticket names `usage.cache_creation_input_tokens` and this runner prints
// that column verbatim, but it is NOT on its own the launch prefix: which of the
// three usage fields the prefix lands in depends on cache state, not on size.
// See `parseUsage` for the measured evidence and the three-term definition the
// deltas are actually taken on.
//
// NOT a test. This file makes real API calls and is deliberately named
// `measure-context-baseline.mjs`, outside the `scripts/*.test.mjs` glob that
// `scripts/Makefile`'s `test` target discovers. Its pure parts are exercised by
// the companion `measure-context-baseline.test.mjs`, which injects both the
// spawn and the binary preflight.
//
// Two things this harness deliberately refuses to measure silently:
//   * a run where a declared (or ambiently merged) MCP server did not attach —
//     see `checkMcpAttachment`. That is why the runner reads
//     `--output-format stream-json` rather than `json`: the plain `json` result
//     object carries no `mcp_servers` key at all, so the init event is the only
//     place Claude Code says what actually loaded.
//   * the fact that bossd's `--append-system-prompt` session context is NOT in
//     any row unless a variant asks for it — see `scopeNotice`. Every row is
//     labelled, so an absolute quoted from this table cannot be mistaken for the
//     full prefix a real tmux-hosted session pays.
//
// Usage:
//   node scripts/measure-context-baseline.mjs [--manifest PATH] [--json]
//
// Each run writes one MCP config per variant into a fresh `context-baseline-*`
// scratch dir (mode 0700) under the system temp dir and DELIBERATELY leaves it
// there: when a number looks wrong, the exact config each variant was measured
// under is the first thing you want. Configs are written VERBATIM from the
// manifest, so for the committed manifest they hold no secrets — its entries
// carry `${ENV_VAR}` references, and a test enforces that. A hand-written
// `--manifest` that inlines a literal credential will have that credential
// copied into the retained dir; that is the manifest author's call, not a
// property this runner can promise.
//
// Node built-ins only (agent/cron worktrees are dependency-free).
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO_ROOT = path.resolve(HERE, '..')

/**
 * Default variant manifest. Variants live in data so a later change adds one
 * without touching this runner: the dedicated keys cover the MCP and agent
 * surfaces, and `extraArgs` carries any other `claude` flag (the reserved ones
 * excepted) verbatim, so a new contributor to measure does not need a new key.
 */
export const MANIFEST_PATH = path.join(HERE, 'context-baseline-variants.json')

const MANIFEST_KEYS = new Set(['prompt', 'baseline', 'variants'])
const VARIANT_KEYS = new Set([
  'name',
  'description',
  'strictMcpConfig',
  'mcpServers',
  'agents',
  'extraArgs',
])

/**
 * Flags this runner owns. A variant may NOT smuggle them in through `extraArgs`:
 * a second `--strict-mcp-config` would bypass the "strict with no config strips
 * the boss server too" guard, and a second `--mcp-config` or `-p` would silently
 * measure something other than what the variant declares.
 */
const RESERVED_ARGS = new Set([
  '-p',
  '--print',
  '--output-format',
  '--verbose',
  '--mcp-config',
  '--strict-mcp-config',
  '--agents',
])

/**
 * The flags that supply bossd's session context. A variant naming one of them is
 * measuring the full launch shape; every other variant is measuring a subtotal,
 * and `scopeNotice` says so out loud rather than leaving it to the report's prose.
 */
const SESSION_CONTEXT_FLAGS = new Set(['--append-system-prompt', '--append-system-prompt-file'])

/** Read and JSON-parse a variant manifest. Validation is a separate step. */
export function loadManifest(file = MANIFEST_PATH) {
  return JSON.parse(fs.readFileSync(file, 'utf8'))
}

/**
 * Every entry of one variant's `mcpServers` document must name a transport that
 * could actually launch. `{"boss": {}}`, or a `url` with no `type`, would
 * otherwise pass: `checkVariantBinaries` keys off `command` and skips them,
 * `claude` tolerates what it cannot start and carries on with a smaller tool
 * surface, and the run exits 0 having measured something nobody asked for. That
 * is the same silent under-measurement `checkVariantBinaries` exists to prevent,
 * reached through the doors it does not watch — and `claude`'s own startup
 * warning is suppressed for a caller that captures stderr, which `spawnSync`
 * always does. Repo-root `.mcp.json` shows the legitimate shapes: stdio entries
 * omit `type`, remote ones carry `"type": "http"`.
 */
function validateMcpServers(variant) {
  if (variant.mcpServers === undefined) return
  if (
    !variant.mcpServers ||
    typeof variant.mcpServers !== 'object' ||
    Array.isArray(variant.mcpServers)
  ) {
    throw new Error(`variant ${JSON.stringify(variant.name)} "mcpServers" must be an object`)
  }
  for (const [server, spec] of Object.entries(variant.mcpServers)) {
    const where = `variant ${JSON.stringify(variant.name)} MCP server ${JSON.stringify(server)}`
    if (!spec || typeof spec !== 'object' || Array.isArray(spec)) {
      throw new Error(`${where} must be an object`)
    }
    const hasCommand = typeof spec.command === 'string' && spec.command.trim() !== ''
    const hasUrl = typeof spec.url === 'string' && spec.url.trim() !== ''
    if (hasCommand === hasUrl) {
      throw new Error(
        hasCommand
          ? `${where} declares both a "command" and a "url"; exactly one transport must be named, or claude resolves the ambiguity itself and the variant measures a surface it did not declare`
          : `${where} declares neither a non-empty "command" (stdio) nor a non-empty "url" (http); claude would start without it while this variant silently measured a smaller surface`,
      )
    }
    if (spec.type !== undefined && typeof spec.type !== 'string') {
      throw new Error(`${where} "type" must be a string`)
    }
    if (hasUrl && spec.type !== 'http' && spec.type !== 'sse') {
      throw new Error(
        `${where} declares a "url" but its "type" is ${JSON.stringify(spec.type)}; a remote server needs "http" or "sse", and claude SKIPS an entry it cannot classify while still exiting 0`,
      )
    }
    if (hasCommand && spec.type !== undefined && spec.type !== 'stdio') {
      throw new Error(
        `${where} declares a "command" but its "type" is ${JSON.stringify(spec.type)}; a stdio server must omit "type" or set it to "stdio"`,
      )
    }
  }
}

/**
 * `extraArgs` is the escape hatch that keeps "add a variant without touching the
 * runner" true for flags with no dedicated key — `--append-system-prompt`,
 * `--setting-sources`, `--model`, the ones the report's recommended next
 * measurements name. It reaches `spawnSync` verbatim, so it is validated
 * tightly: a non-string entry arrives as garbage, a RESERVED flag contradicts a
 * key the runner already owns, and a bare LEADING word is absorbed by the
 * variadic `--mcp-config` it is appended after rather than read as a new flag.
 * The reserved check matches the flag NAME because `claude --help` documents the
 * `=`-joined form, and a smuggled second `--mcp-config=…` ADDS servers rather
 * than erroring.
 */
function validateExtraArgs(variant) {
  if (variant.extraArgs === undefined) return
  if (!Array.isArray(variant.extraArgs)) {
    throw new Error(`variant ${JSON.stringify(variant.name)} "extraArgs" must be an array`)
  }
  for (const arg of variant.extraArgs) {
    if (typeof arg !== 'string' || arg === '') {
      throw new Error(
        `variant ${JSON.stringify(variant.name)} "extraArgs" must hold non-empty strings, got ${JSON.stringify(arg)}`,
      )
    }
    if (RESERVED_ARGS.has(arg.split('=', 1)[0])) {
      throw new Error(
        `variant ${JSON.stringify(variant.name)} "extraArgs" may not pass ${JSON.stringify(arg)}; this runner owns that flag and a second copy would measure a shape the variant does not declare`,
      )
    }
  }
  if (variant.extraArgs.length > 0 && !variant.extraArgs[0].startsWith('-')) {
    throw new Error(
      `variant ${JSON.stringify(variant.name)} "extraArgs" must start with a flag, got ${JSON.stringify(variant.extraArgs[0])}; a bare leading word would be absorbed by the variadic --mcp-config it follows`,
    )
  }
}

/**
 * Validate a variant manifest, returning it unchanged when it is well formed.
 * Rejects unknown keys (a typo'd variant key would otherwise be silently
 * ignored and measure the wrong thing) and duplicate variant names; the
 * transport and passthrough rules live in the two helpers above.
 */
export function validateManifest(doc) {
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) {
    throw new Error('manifest must be a JSON object')
  }
  for (const key of Object.keys(doc)) {
    if (!MANIFEST_KEYS.has(key)) {
      throw new Error(`manifest has unknown key ${JSON.stringify(key)}`)
    }
  }
  if (typeof doc.prompt !== 'string' || doc.prompt.trim() === '') {
    throw new Error('manifest "prompt" must be a non-empty string')
  }
  if (!Array.isArray(doc.variants) || doc.variants.length === 0) {
    throw new Error('manifest "variants" must be a non-empty array')
  }

  const seen = new Set()
  for (const variant of doc.variants) {
    if (!variant || typeof variant !== 'object' || Array.isArray(variant)) {
      throw new Error('each manifest variant must be a JSON object')
    }
    for (const key of Object.keys(variant)) {
      if (!VARIANT_KEYS.has(key)) {
        throw new Error(
          `variant ${JSON.stringify(variant.name ?? '?')} has unknown key ${JSON.stringify(key)}`,
        )
      }
    }
    if (typeof variant.name !== 'string' || variant.name.trim() === '') {
      throw new Error('each manifest variant needs a non-empty "name"')
    }
    if (seen.has(variant.name)) {
      throw new Error(`duplicate variant name ${JSON.stringify(variant.name)}`)
    }
    seen.add(variant.name)
    validateMcpServers(variant)
    // Every strictness decision downstream is `=== true`, so a non-boolean here
    // (`"strictMcpConfig": "true"`) would be silently read as NON-strict: the
    // variant would merge repo-root and user-scoped MCP config and measure a
    // shape nobody asked for, after billing for it. Same reason unknown keys are
    // rejected — a manifest must not be able to quietly measure the wrong thing.
    if (variant.strictMcpConfig !== undefined && typeof variant.strictMcpConfig !== 'boolean') {
      throw new Error(
        `variant ${JSON.stringify(variant.name)} "strictMcpConfig" must be a boolean, got ${JSON.stringify(variant.strictMcpConfig)}; a non-boolean would be read as non-strict and silently measure a merged MCP surface`,
      )
    }
    // --strict-mcp-config with no --mcp-config strips the boss server too (the
    // guard in plugins/bossd-plugin-claude/server.go exists for exactly this),
    // so a strict variant must declare an mcpServers document — an EMPTY one
    // ({}) is how "no MCP at all" is expressed.
    if (variant.strictMcpConfig === true && variant.mcpServers === undefined) {
      throw new Error(
        `variant ${JSON.stringify(variant.name)} sets strictMcpConfig without "mcpServers"; strict mode with no config file strips the boss server too — declare an empty {} instead`,
      )
    }
    if (variant.agents !== undefined && typeof variant.agents !== 'string') {
      throw new Error(`variant ${JSON.stringify(variant.name)} "agents" must be a JSON string`)
    }
    validateExtraArgs(variant)
  }

  if (typeof doc.baseline !== 'string' || !seen.has(doc.baseline)) {
    throw new Error(`manifest "baseline" ${JSON.stringify(doc.baseline)} names no variant`)
  }
  return doc
}

/** Keep an error message readable: a stream-json init event runs to tens of KB. */
function excerpt(text, max = 2000) {
  return text.length <= max ? text : `${text.slice(0, max)}… (${text.length} bytes total)`
}

/**
 * Split one `--output-format stream-json` run into the two events this harness
 * needs: the `system`/`init` event, the ONLY place Claude Code reports which MCP
 * servers actually attached, and the terminal `result` event, which carries the
 * same `usage` object the plain `json` format returns.
 *
 * Measured against claude 2.1.220, not assumed: a `--output-format json` result
 * object has no `mcp_servers` key at all, and `claude mcp list` ignores
 * `--mcp-config`/`--strict-mcp-config` entirely (it health-checks the ambient
 * config instead). The stream is therefore the only per-variant source of
 * attachment truth, which is what it is paid for here.
 *
 * Returns `{ mcpServers, resultLine }`. A line that is not JSON is skipped — the
 * stream interleaves hook events and assistant messages, and a stray banner must
 * not fail an already-billed measurement — but a MISSING init or result event
 * throws: both are unconditional in a successful run, so their absence means the
 * stream shape changed underneath us and every number downstream is suspect.
 */
export function parseStreamResult(stdout) {
  const text = String(stdout ?? '').trim()
  if (text === '') {
    throw new Error('claude -p produced no stdout; expected a stream-json event stream')
  }
  let init
  let resultLine
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (trimmed === '') continue
    let doc
    try {
      doc = JSON.parse(trimmed)
    } catch {
      continue
    }
    if (!doc || typeof doc !== 'object' || Array.isArray(doc)) continue
    // Not every `system` event is the init one: the stream also carries a
    // `system` event per hook invocation, and those have no mcp_servers.
    if (doc.type === 'system' && doc.subtype === 'init') init = doc
    // Keep the LAST result event; the stream terminates with it.
    else if (doc.type === 'result') resultLine = trimmed
  }
  if (!init) {
    throw new Error(
      `claude -p stream carried no system/init event, so what attached is unknowable: ${excerpt(text)}`,
    )
  }
  if (!resultLine) {
    throw new Error(`claude -p stream carried no result event: ${excerpt(text)}`)
  }
  if (!Array.isArray(init.mcp_servers)) {
    throw new Error(
      `claude -p init event carries no "mcp_servers" array; the harness cannot verify attachment: ${excerpt(text)}`,
    )
  }
  return { mcpServers: init.mcp_servers, resultLine }
}

/**
 * Refuse to record a run whose MCP surface is not the one that was asked for.
 *
 * Claude Code exits 0 with a perfectly valid `usage` when a remote server fails
 * to authenticate or connect — the published 2026-08-03 run is the proof: Linear
 * and Sentry contributed 320 tokens between them, far too little for Linear's
 * ~47-tool surface, so neither had attached, and the run recorded itself as a
 * clean data point anyway. Any later comparison spanning remote variants would
 * then attribute an absent tool surface to the change under test. This is the
 * same class of silent under-measurement `checkVariantBinaries` refuses for
 * stdio, reached through the door it cannot watch.
 *
 * TWO checks, because they fail differently and one does not imply the other:
 *
 *  1. Every server the variant DECLARES must appear in the reported list. A spec
 *     Claude Code rejects during config validation is dropped from the list
 *     entirely rather than listed as failed, so "no server reports an error" is
 *     not the same contract as "every declared server is there".
 *  2. Every server the session REPORTS must be `connected` — not only the
 *     declared ones. A non-strict variant merges repo-root `.mcp.json` and any
 *     user-scoped config, and it was precisely those merged-in remotes that
 *     silently sat out the published run. A prefix measured with one of them
 *     half-attached is not an absolute anybody should quote.
 */
export function checkMcpAttachment(variant, mcpServers) {
  const where = `variant ${JSON.stringify(variant.name)}`
  const reported = new Map()
  for (const entry of mcpServers) {
    if (!entry || typeof entry !== 'object' || typeof entry.name !== 'string') {
      throw new Error(
        `${where}: claude reported an unreadable MCP server entry ${JSON.stringify(entry)}`,
      )
    }
    reported.set(entry.name, entry.status)
  }
  for (const name of Object.keys(variant.mcpServers ?? {})) {
    if (!reported.has(name)) {
      const names = [...reported.keys()].map((n) => JSON.stringify(n)).join(', ') || 'none'
      throw new Error(
        `${where} declares MCP server ${JSON.stringify(name)} but the session reported no server by that name (reported: ${names}); a spec claude rejects is dropped from the list rather than listed as failed, so this variant measured a smaller surface than it declares`,
      )
    }
  }
  for (const [name, status] of reported) {
    if (status !== 'connected') {
      throw new Error(
        `${where}: MCP server ${JSON.stringify(name)} reported status ${JSON.stringify(status)}, not "connected", so its tool surface is absent from this run and the measured prefix is not the shape this variant names — authenticate it, drop it, or run strict, then re-measure`,
      )
    }
  }
}

/**
 * Extract the launch prefix from one `claude -p` result event.
 *
 * Returns `{ input, cacheCreation, cacheRead, tokens }`. `cacheCreation` is
 * `usage.cache_creation_input_tokens` verbatim — the column the ticket names —
 * but it is NOT on its own a stable measure of what launch costs: which of the
 * three usage fields the prefix lands in depends on cache state, not on size, so
 * `tokens` sums all three (written to cache, read from cache, and whatever sat
 * past the last cache breakpoint and was billed as plain input).
 *
 * The measured evidence for that — the ~5-minute ephemeral cache window, the
 * back-to-back run that reported 0, and the isolated re-run that did not — lives
 * in `docs/solutions/performance/2026-08-03-session-launch-context-baseline.md`
 * under "What launch prefix means". Deliberately NOT restated here: those are
 * measured constants that a re-run invalidates, and the report is the one place
 * that owns them.
 *
 * Throws — with the RAW output embedded — on anything malformed, including a
 * total of 0. A silent zero would read as "this variant is free", the worst
 * possible failure mode for a measurement harness.
 */
export function parseUsage(stdout) {
  const text = String(stdout ?? '').trim()
  if (text === '') {
    throw new Error('claude -p produced no stdout; expected a JSON result object')
  }
  let doc
  try {
    doc = JSON.parse(text)
  } catch {
    throw new Error(`claude -p stdout was not JSON: ${text}`)
  }
  // An errored turn still exits 0 and still reports a `usage`, but that usage
  // describes a truncated turn rather than a launch. Recording it would be a
  // silent under-measurement of exactly the kind this parser exists to refuse.
  if (doc && typeof doc === 'object' && doc.is_error === true) {
    throw new Error(
      `claude -p reported an errored result; its usage is not a launch prefix: ${text}`,
    )
  }
  const usage = doc && typeof doc === 'object' ? doc.usage : undefined
  if (!usage || typeof usage !== 'object') {
    throw new Error(`claude -p result carries no "usage" object: ${text}`)
  }
  // PRESENCE and VALUE are separate contracts, and conflating them is what makes
  // a strict parser reject a legitimate run.
  //
  // VALUE, identical for all three terms: an explicit `null` is a well-formed
  // ZERO — real `usage` objects emit null for a cache field that did not apply —
  // while PRESENT-BUT-NOT-A-NUMBER is malformed and must throw. Coercing the
  // latter to 0 is the silent under-measurement this parser exists to refuse:
  // `cache_read_input_tokens: "110160"` would otherwise be dropped entirely and
  // publish as a six-figure saving.
  //
  // PRESENCE, only for `cache_creation_input_tokens`: the KEY must exist. It is
  // the field the ticket names, so its disappearance means the `claude -p` result
  // shape changed underneath us and the whole parse is suspect. A key that is
  // present-and-null does NOT trip this — a cached run legitimately has nothing
  // to write to cache, and every row of the published table reports a zero
  // cache_creation. Throwing there would fail a real, already-billed measurement.
  //
  // Both halves are pinned by test so neither can drift silently.
  const numField = (key, { requireKey = false } = {}) => {
    const raw = usage[key]
    if (raw === undefined) {
      if (requireKey) {
        throw new Error(
          `claude -p usage carries no ${key} field at all; the result shape changed: ${text}`,
        )
      }
      return 0
    }
    if (raw === null) return 0
    if (typeof raw !== 'number' || !Number.isFinite(raw)) {
      throw new Error(`claude -p usage has a non-numeric ${key}: ${text}`)
    }
    return raw
  }
  const cacheCreation = numField('cache_creation_input_tokens', { requireKey: true })
  const cacheRead = numField('cache_read_input_tokens')
  const input = numField('input_tokens')
  const tokens = input + cacheCreation + cacheRead
  if (tokens === 0) {
    throw new Error(
      `claude -p reported a launch prefix of 0 tokens (input_tokens + cache_creation_input_tokens + cache_read_input_tokens); no session launches for free: ${text}`,
    )
  }
  return { input, cacheCreation, cacheRead, tokens }
}

/**
 * Turn measured results into table rows carrying a SIGNED delta against the
 * named baseline. A variant larger than the baseline yields a positive delta; it
 * is never clamped or reported as a saving. Deltas are taken on the launch
 * prefix (`tokens`), not on `cacheCreation`, so a warm run does not read as a
 * saving of the entire prefix.
 */
export function computeRows(results, baselineName) {
  const baseline = results.find((r) => r.name === baselineName)
  if (!baseline) {
    throw new Error(`baseline variant ${JSON.stringify(baselineName)} has no measured result`)
  }
  return results.map((r) => ({
    name: r.name,
    tokens: r.tokens,
    input: r.input,
    cacheCreation: r.cacheCreation,
    cacheRead: r.cacheRead,
    delta: r.tokens - baseline.tokens,
    isBaseline: r.name === baselineName,
    // Carried per row, not per run: the moment a later child adds the variant
    // the report asks for, some rows will carry the session context and some
    // will not, and only a per-row label can keep a delta between them honest.
    sessionContext: r.sessionContext === true,
  }))
}

/**
 * True when a variant asks `claude` for bossd's session context.
 *
 * `session.BuildAppendSystemPrompt` (services/bossd/internal/session/tmux_chat.go)
 * builds that text for every tmux-hosted spawn, and the claude plugin passes it
 * as `--append-system-prompt` (plugins/bossd-plugin-claude/server.go:191). It is
 * the one fully repo-owned contributor to the launch prefix, and this runner has
 * no dedicated key for it — a variant supplies it through `extraArgs`, which is
 * why the check reads argv rather than a manifest field. Matched on the flag
 * NAME so the `=`-joined form counts too.
 */
export function variantCarriesSessionContext(variant) {
  return (variant?.extraArgs ?? []).some((arg) =>
    SESSION_CONTEXT_FLAGS.has(String(arg).split('=', 1)[0]),
  )
}

/**
 * Name the harness's own scope, so an absolute lifted from its table cannot be
 * read as the full launch prefix.
 *
 * A row measured without `--append-system-prompt` is an MCP-and-installation
 * SUBTOTAL: a real tmux-hosted session pays it PLUS bossd's session context, and
 * cron pays more of that text still (`BuildAppendSystemPrompt` appends the
 * autonomy and subagent directives when `IsUnattended`). Prose in the report
 * cannot travel with a `--json` payload; this does. Returns null once every row
 * carries the flag, at which point there is nothing to disclaim.
 */
export function scopeNotice(rows) {
  const missing = rows.filter((r) => !r.sessionContext).map((r) => r.name)
  if (missing.length === 0) return null
  return `scope: ${missing.length} of ${rows.length} row(s) (${missing.join(', ')}) were measured WITHOUT bossd's --append-system-prompt session context (session.BuildAppendSystemPrompt, emitted for every tmux-hosted spawn at plugins/bossd-plugin-claude/server.go). Those rows are an MCP-and-installation subtotal, not the whole launch prefix — a real session pays them PLUS that text. Size it with "extraArgs": ["--append-system-prompt", "<text>"], and read a delta between a labelled and an unlabelled row as that text's cost, never as the change under test.`
}

function signed(n) {
  return n > 0 ? `+${n}` : String(n)
}

/** Render rows as a fixed-width table: variant, cache split, prefix, delta. */
export function formatTable(rows) {
  const header = [
    'variant',
    'cache_creation_input_tokens',
    'cache_read_input_tokens',
    'input_tokens',
    'launch prefix tokens',
    'delta vs baseline',
  ]
  const body = rows.map((r) => [
    r.name,
    String(r.cacheCreation),
    String(r.cacheRead),
    String(r.input),
    String(r.tokens),
    r.isBaseline ? '0 (baseline)' : signed(r.delta),
  ])
  const widths = header.map((h, i) => Math.max(h.length, ...body.map((cells) => cells[i].length)))
  const line = (cells) =>
    cells
      .map((c, i) => c.padEnd(widths[i]))
      .join('  ')
      .trimEnd()
  return [line(header), line(widths.map((w) => '-'.repeat(w))), ...body.map(line)].join('\n')
}

/**
 * Build the argv for one variant. `mcpConfigPath` is the file the caller wrote
 * this variant's `mcpServers` document to (undefined when the variant declares
 * none). Refuses to emit a bare `--strict-mcp-config`, which would strip the
 * boss server rather than measure it.
 */
export function buildClaudeArgs(variant, { prompt, mcpConfigPath } = {}) {
  // stream-json, not json: the plain `json` result object carries no
  // `mcp_servers` key, so the init event is the only place a run says which MCP
  // servers actually attached (see parseStreamResult). `--verbose` is what makes
  // `--print` emit the full event stream rather than the result alone.
  const args = ['-p', prompt, '--output-format', 'stream-json', '--verbose']
  if (mcpConfigPath) {
    args.push('--mcp-config', mcpConfigPath)
  }
  if (variant.strictMcpConfig === true) {
    if (!mcpConfigPath) {
      throw new Error(
        `variant ${JSON.stringify(variant.name)} is strict but has no --mcp-config file; strict mode with no config strips the boss server too`,
      )
    }
    args.push('--strict-mcp-config')
  }
  if (variant.agents !== undefined) {
    args.push('--agents', variant.agents)
  }
  // Last, so a variant's own flags are appended to — never able to reorder the
  // ones above. `validateManifest` has already refused any reserved flag (in
  // `=`-joined form too), so this cannot override them either. The real hazard of
  // the trailing position is ABSORPTION, not displacement: `--mcp-config` is
  // variadic, so a bare leading word would join its list — which is why the
  // validator requires `extraArgs[0]` to start with `-`.
  if (variant.extraArgs !== undefined) {
    args.push(...variant.extraArgs)
  }
  return args
}

/** A `claude` that blocks on a login prompt would otherwise hang the harness forever. */
const SPAWN_TIMEOUT_MS = 120_000

/** The real, impure step: one cold `claude -p` per variant, from the repo root. */
function spawnClaude({ args }) {
  const res = spawnSync('claude', args, {
    cwd: REPO_ROOT,
    encoding: 'utf8',
    timeout: SPAWN_TIMEOUT_MS,
  })
  if (res.error) {
    // On a timeout kill, spawnSync still carries whatever the child managed to
    // write — which is exactly the login prompt the timeout exists to diagnose.
    // Keep it rather than reporting a bare ETIMEDOUT.
    const timedOut = res.error.code === 'ETIMEDOUT'
    const detail = timedOut ? `timed out after ${SPAWN_TIMEOUT_MS}ms: ` : ''
    return {
      status: -1,
      stdout: res.stdout ?? '',
      stderr: `${detail}${res.error.message}\n${res.stderr ?? ''}`.trim(),
    }
  }
  return { status: res.status, stdout: res.stdout ?? '', stderr: res.stderr ?? '' }
}

/**
 * Refuse to measure a variant whose stdio MCP server cannot start.
 *
 * Claude Code TOLERATES an MCP server that fails to launch: it logs and carries
 * on with a smaller tool surface. For a measurement harness that is the same
 * class of failure `parseUsage` guards against one level up — the run still
 * exits 0, but every delta collapses toward zero and the report reads "the boss
 * MCP is free". The manifest points three variants at `./bin/mcp`, which is a
 * gitignored build artifact (`make build`), so a fresh clone hits this by
 * default rather than by accident.
 *
 * Only PATH-shaped commands are skipped: a bare `npx` is resolved by the OS and
 * cannot be cheaply checked, whereas anything bearing a separator is a path this
 * harness can and must verify. Resolved against `repoRoot`, because that is the
 * cwd the runner spawns `claude` in. `exists` must report EXECUTABILITY, not mere
 * presence — a directory or a non-+x file would fail to launch just as surely as
 * a missing one.
 *
 * KNOWN GAP: only `command` is checked. A server declared as
 * `{command: "node", args: ["./server.mjs"]}` or launched through `npx` slips
 * past, because neither the interpreter's presence nor a PATH lookup proves the
 * server will start. Every variant in the committed manifest is either a direct
 * path or a remote http server, so the gap is currently unreachable — a later
 * child adding an interpreter-launched variant must extend this.
 */
export function checkVariantBinaries(variant, { repoRoot = REPO_ROOT, exists } = {}) {
  const isPresent = exists ?? isExecutable
  for (const [server, spec] of Object.entries(variant.mcpServers ?? {})) {
    const command = spec && typeof spec === 'object' ? spec.command : undefined
    // Not `path.sep`: on win32 that is `\`, so a manifest written with the
    // portable `./bin/mcp` would have "no separator" and the guard would
    // silently degrade to a no-op — the very failure it exists to prevent.
    if (typeof command !== 'string' || !/[\\/]/.test(command)) continue
    const resolved = path.resolve(repoRoot, command)
    if (!isPresent(resolved)) {
      throw new Error(
        `variant ${JSON.stringify(variant.name)} declares MCP server ${JSON.stringify(server)} at ${command} (${resolved}), which is not an executable file; claude would start WITHOUT it and this variant would silently measure a smaller surface — build it first (e.g. \`make build\`)`,
      )
    }
  }
}

/** True only for something that could actually be exec'd — not a dir, not a non-+x file. */
function isExecutable(p) {
  try {
    if (!fs.statSync(p).isFile()) return false
    fs.accessSync(p, fs.constants.X_OK)
    return true
  } catch {
    return false
  }
}

/**
 * Measure every variant in `manifest` and return delta rows. `spawn` and
 * `checkBinaries` are injected so tests can exercise this without an API call or
 * a built `./bin/mcp`; the defaults really do call `claude` and really do stat
 * the declared binaries. Fails loudly on the first variant that cannot be
 * measured — including one whose MCP server would not have started.
 */
export async function runMeasurement({
  manifest,
  spawn = spawnClaude,
  checkBinaries = checkVariantBinaries,
  tmpDir,
} = {}) {
  // Preflight EVERY variant before spawning ANY of them, and preflight BOTH
  // halves of the guard. `validateManifest` is the shape half (a transport that
  // could never launch, a reserved flag smuggled through `extraArgs`);
  // `checkBinaries` is the executability half. Hoisting only the second one and
  // leaving the first to `main` would mean any caller reaching this exported
  // function directly — the next child adding a variant — got the executability
  // check and none of the shape checks, which is the exact silent
  // under-measurement both halves exist to refuse. Re-validating a manifest
  // `main` already validated is idempotent and free.
  validateManifest(manifest)
  for (const variant of manifest.variants) {
    checkBinaries(variant)
  }
  const dir = tmpDir ?? fs.mkdtempSync(path.join(os.tmpdir(), 'context-baseline-'))
  if (!tmpDir) {
    // The dir is retained deliberately; an unprinted random path is unfindable.
    process.stderr.write(`variant MCP configs: ${dir}\n`)
  }
  const results = []
  for (const variant of manifest.variants) {
    let mcpConfigPath
    if (variant.mcpServers !== undefined) {
      // Sanitize the variant name into one path-safe filename component, so a
      // manifest entry containing separators cannot make join() escape the
      // scratch dir (mirrors services/bossd/internal/session safeConfigBase).
      const base = variant.name.replace(/[^A-Za-z0-9._-]/g, '_')
      mcpConfigPath = path.join(dir, `${base}.mcp.json`)
      fs.writeFileSync(mcpConfigPath, JSON.stringify({ mcpServers: variant.mcpServers }, null, 2))
    }
    const args = buildClaudeArgs(variant, { prompt: manifest.prompt, mcpConfigPath })
    const res = await spawn({ args, variant, mcpConfigPath })
    if (res.status !== 0) {
      throw new Error(
        // BOTH streams, not `stderr || stdout`: on the timeout path stderr always
        // carries the "timed out after" prefix, which would short-circuit away the
        // partial stdout that path deliberately preserves — and `claude` prints its
        // most useful diagnostic ("Invalid API key · Please run /login") to stdout.
        `variant ${JSON.stringify(variant.name)}: claude exited ${res.status}: ${`${res.stderr ?? ''}\n${res.stdout ?? ''}`.trim()}`,
      )
    }
    // The two parsers below are wrapped so their messages name the variant; the
    // attachment check names it itself, so wrapping that one too would stutter.
    const named = (err) => new Error(`variant ${JSON.stringify(variant.name)}: ${err.message}`)
    let stream
    try {
      stream = parseStreamResult(res.stdout)
    } catch (err) {
      throw named(err)
    }
    // BEFORE the usage is parsed, not after: a usage recorded from a run whose
    // declared surface never loaded is worse than no usage at all, because it
    // looks exactly like a good one.
    checkMcpAttachment(variant, stream.mcpServers)
    let usage
    try {
      usage = parseUsage(stream.resultLine)
    } catch (err) {
      throw named(err)
    }
    results.push({
      name: variant.name,
      sessionContext: variantCarriesSessionContext(variant),
      ...usage,
    })
  }
  return computeRows(results, manifest.baseline)
}

/**
 * Parse argv. A `--manifest` with no value must THROW: falling through to the
 * default manifest would spend one real API call per variant measuring a config
 * the caller believes they overrode, and report it as success.
 */
export function parseArgv(argv) {
  const opts = { manifest: MANIFEST_PATH, json: false }
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === '--json') {
      opts.json = true
    } else if (argv[i] === '--manifest') {
      const value = argv[i + 1]
      if (value === undefined || value.startsWith('--')) {
        throw new Error('--manifest needs a path argument')
      }
      opts.manifest = value
      i += 1
    } else {
      throw new Error(`unknown argument ${JSON.stringify(argv[i])}`)
    }
  }
  return opts
}

/**
 * Render the measured rows for stdout. Split out of `main` so the `--json`
 * envelope's key names are testable without an API call: a typo in `baseline` or
 * `rows` would otherwise ship silently, since `main`'s only other exercise is a
 * real billed run.
 */
export function renderOutput(rows, { baseline, json = false } = {}) {
  // `scope` rides inside the envelope rather than only on stderr: a `--json`
  // consumer pipes stdout, and the one caveat that decides whether an absolute
  // may be quoted must not be the part that gets dropped.
  return json
    ? `${JSON.stringify({ baseline, scope: scopeNotice(rows), rows }, null, 2)}\n`
    : `${formatTable(rows)}\n`
}

/**
 * A warning for a run where EVERY row reported `cache_creation_input_tokens: 0`
 * — the AC-named column, which a reader scanning "variant → cache_creation →
 * delta" would otherwise mistake for a broken measurement. The deltas are still
 * valid either way: each variant's whole prefix is counted whoever paid for it.
 *
 * A zero cache_creation column has THREE causes and the notice must not assert
 * the wrong one — "the whole prefix was read from cache" is false in two of them.
 * Uncached (`cache_read` also 0): nothing was cached and the prefix was billed as
 * plain `input_tokens` — reachable via a variant that moves the last cache
 * breakpoint, or an API-key run with caching off. Partly cached (`cache_read > 0`
 * AND `input > 0`): served from cache except for whatever sat past the last
 * breakpoint; the report records a measured run carrying ~23.5k there, so this is
 * the common shape, not a corner. Fully warm: `input` is 0 too. Returns null when
 * there is nothing to say.
 */
export function warmRunNotice(rows) {
  if (rows.length === 0 || !rows.every((r) => r.cacheCreation === 0)) return null
  if (!rows.every((r) => r.cacheRead > 0)) {
    return 'note: cache_creation_input_tokens is 0 on every row and at least one row cached nothing, so that prefix was billed as plain input_tokens rather than written to cache. Deltas are valid; do not read the 0 column as a saving.'
  }
  const plainInput = rows.reduce((total, r) => total + (r.input ?? 0), 0)
  if (plainInput > 0) {
    return `note: cached run — cache_creation_input_tokens is 0 on every row because the prefix was served from an existing cache, except ${plainInput} token(s) across all rows billed as plain input_tokens past the last cache breakpoint. Deltas are valid; the absolutes reflect a cached prefix.`
  }
  return 'note: fully warm run — cache_creation_input_tokens is 0 on every row because the whole prefix was read from an existing cache. Deltas are valid; the absolutes reflect a cached prefix.'
}

export async function main(argv = process.argv.slice(2)) {
  const opts = parseArgv(argv)
  const manifest = validateManifest(loadManifest(opts.manifest))
  const rows = await runMeasurement({ manifest })
  const notice = warmRunNotice(rows)
  if (notice) process.stderr.write(`${notice}\n`)
  // Also on stderr for the table path, which has no envelope to carry it.
  const scope = scopeNotice(rows)
  if (scope) process.stderr.write(`${scope}\n`)
  process.stdout.write(renderOutput(rows, { baseline: manifest.baseline, json: opts.json }))
  return 0
}

if (isMainModule(import.meta.url)) {
  main().then(
    (code) => process.exit(code),
    (err) => {
      process.stderr.write(`${err.message}\n`)
      process.exit(1)
    },
  )
}
