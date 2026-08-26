#!/usr/bin/env node

// Transcript batch analyzer for awaited dispatch phases.
//
// Claude Code stores one assistant JSONL entry per content block, so the entry uuid is never a
// dispatch batch key. Sibling tool-use blocks from one assistant message share requestId/message.id.
//
// Node built-ins only — cron worktrees are dependency-free.

import fs from 'node:fs'
import path from 'node:path'
import { isMainModule } from './main-module.mjs'

export const AGENT_TOOL_NAMES = ['Agent', 'Task']

export const DEFAULT_PHASE_RULES = [
  { id: 'BR-P1-LENS', pattern: 'findings-lens-' },
  { id: 'BR-PR-ROUND', pattern: 'findings-round-' },
  { id: 'BR-PD-DEFAULT', pattern: 'findings-round-default-' },
]

const OUT_PATH_RE =
  /findings-(?:round-default-[a-z0-9-]+|round-[a-z0-9-]+|lens-\d+-[a-z0-9-]+)\.json/g

export function batchKeyFor(entry) {
  return entry?.requestId ?? entry?.message?.id ?? null
}

function promptFor(block) {
  return block?.input?.prompt ?? block?.input?.description ?? ''
}

function descriptionFor(block) {
  return block?.input?.description ?? block?.input?.prompt ?? ''
}

function firstOutPath(prompt) {
  OUT_PATH_RE.lastIndex = 0
  return OUT_PATH_RE.exec(prompt)?.[0] ?? null
}

export function collectDispatches(text, { runTmp } = {}) {
  const dispatches = []
  for (const line of text.split(/\r?\n/)) {
    if (!line.trim()) continue
    let entry
    try {
      entry = JSON.parse(line)
    } catch {
      continue
    }
    if (entry?.type !== 'assistant') continue
    const content = entry?.message?.content
    if (!Array.isArray(content)) continue
    for (const block of content) {
      if (block?.type !== 'tool_use' || !AGENT_TOOL_NAMES.includes(block?.name)) continue
      const prompt = promptFor(block)
      if (runTmp && !prompt.includes(runTmp)) continue
      dispatches.push({
        batchKey: batchKeyFor(entry),
        outPath: firstOutPath(prompt),
        description: descriptionFor(block),
        timestamp: entry.timestamp ?? entry.createdAt ?? null,
      })
    }
  }
  return dispatches
}

export function groupBatches(dispatches) {
  const groups = new Map()
  for (const dispatch of dispatches) {
    if (dispatch.batchKey === null) continue
    const existing = groups.get(dispatch.batchKey) ?? []
    existing.push(dispatch)
    groups.set(dispatch.batchKey, existing)
  }
  return groups
}

function normalizedPattern(pattern) {
  return String(pattern ?? '')
    .replace(/^`|`$/g, '')
    .replace(/<i>/g, '')
    .replace(/<name>/g, '')
    .replace(/<capability>/g, '')
    .replace(/\.json$/g, '')
}

export function attributePhase(outPath, phaseRules = DEFAULT_PHASE_RULES) {
  if (!outPath) return null
  for (const rule of phaseRules) {
    const pattern = normalizedPattern(rule.pattern ?? rule.parallelOutPath)
    if (pattern && outPath.includes(pattern)) return rule.id
  }
  return null
}

export function auditBatching(dispatches, { phaseRules = DEFAULT_PHASE_RULES } = {}) {
  const phases = phaseRules.map((rule) => ({ phaseId: rule.id, dispatches: [] }))
  const byPhase = new Map(phases.map((phase) => [phase.phaseId, phase]))
  for (const dispatch of dispatches) {
    const phaseId = attributePhase(dispatch.outPath, phaseRules)
    if (!phaseId) continue
    byPhase.get(phaseId)?.dispatches.push(dispatch)
  }

  const results = phases.map(({ phaseId, dispatches: phaseDispatches }) => {
    const dispatchCount = phaseDispatches.length
    const batches = new Map()
    let hasUngrouped = false
    for (const dispatch of phaseDispatches) {
      if (dispatch.batchKey === null) {
        hasUngrouped = true
        continue
      }
      const existing = batches.get(dispatch.batchKey) ?? 0
      batches.set(dispatch.batchKey, existing + 1)
    }
    const maxBatchSize = Math.max(0, ...batches.values())
    let verdict = 'single'
    if (hasUngrouped) verdict = 'indeterminate'
    else if (dispatchCount < 2) verdict = 'single'
    else if (maxBatchSize >= 2) verdict = 'batched'
    else verdict = 'serial'
    return {
      phaseId,
      dispatchCount,
      maxBatchSize,
      batches: batches.size + (hasUngrouped ? 1 : 0),
      verdict,
      ok: verdict !== 'serial' && verdict !== 'indeterminate',
    }
  })

  return {
    ok: results.every((phase) => phase.verdict !== 'serial'),
    phases: results,
  }
}

export function measureParallelism(sessionDir) {
  const subagentDir = path.join(sessionDir, 'subagents')
  if (!fs.existsSync(subagentDir)) return { subagentSeconds: 0, wallClockSeconds: 0, ratio: 0 }
  const spans = []
  for (const name of fs.readdirSync(subagentDir)) {
    if (!name.endsWith('.jsonl')) continue
    const file = path.join(subagentDir, name)
    const timestamps = []
    for (const line of fs.readFileSync(file, 'utf8').split(/\r?\n/)) {
      if (!line.trim()) continue
      try {
        const timestamp = JSON.parse(line).timestamp
        const ms = Date.parse(timestamp)
        if (Number.isFinite(ms)) timestamps.push(ms)
      } catch {
        // Ignore truncated or non-JSON transcript lines.
      }
    }
    if (timestamps.length >= 2) spans.push([Math.min(...timestamps), Math.max(...timestamps)])
  }
  if (spans.length === 0) return { subagentSeconds: 0, wallClockSeconds: 0, ratio: 0 }
  const subagentMs = spans.reduce((sum, [start, end]) => sum + Math.max(0, end - start), 0)
  const wallStart = Math.min(...spans.map(([start]) => start))
  const wallEnd = Math.max(...spans.map(([, end]) => end))
  const wallMs = Math.max(0, wallEnd - wallStart)
  return {
    subagentSeconds: subagentMs / 1000,
    wallClockSeconds: wallMs / 1000,
    ratio: wallMs === 0 ? 0 : subagentMs / wallMs,
  }
}

function* walkJsonlFiles(root) {
  if (!fs.existsSync(root)) return
  const stat = fs.statSync(root)
  if (stat.isFile()) {
    if (root.endsWith('.jsonl')) yield root
    return
  }
  for (const entry of fs.readdirSync(root, { withFileTypes: true })) {
    const child = path.join(root, entry.name)
    if (entry.isDirectory()) yield* walkJsonlFiles(child)
    else if (entry.isFile() && child.endsWith('.jsonl')) yield child
  }
}

function collectFromFiles(files, opts) {
  const dispatches = []
  const sinceMs = opts.since ? Date.parse(opts.since) : null
  for (const file of files) {
    if (sinceMs && fs.statSync(file).mtimeMs < sinceMs) continue
    const text = fs.readFileSync(file, 'utf8')
    if (!text.includes('"tool_use"')) continue
    if (opts.runTmp && !text.includes(opts.runTmp)) continue
    dispatches.push(...collectDispatches(text, { runTmp: opts.runTmp }))
  }
  return dispatches
}

function parseArgs(argv) {
  const opts = {
    projectsRoot: path.join(process.env.HOME ?? '.', '.claude/projects'),
    format: 'text',
    strict: false,
  }
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i]
    if (arg === '--run-tmp') opts.runTmp = argv[++i]
    else if (arg === '--projects-root') opts.projectsRoot = argv[++i]
    else if (arg === '--transcript') opts.transcript = argv[++i]
    else if (arg === '--since') opts.since = argv[++i]
    else if (arg === '--format') opts.format = argv[++i]
    else if (arg === '--strict') opts.strict = true
    else throw new Error(`unknown argument: ${arg}`)
  }
  if (!['json', 'text'].includes(opts.format)) throw new Error('--format must be json or text')
  return opts
}

function renderText(result) {
  return result.phases
    .map(
      (phase) =>
        `${phase.phaseId}: ${phase.verdict} dispatches=${phase.dispatchCount} maxBatchSize=${phase.maxBatchSize} batches=${phase.batches}`,
    )
    .join('\n')
}

function main(argv = process.argv.slice(2)) {
  const [cmd, ...rest] = argv
  if (cmd !== 'audit')
    throw new Error(
      'usage: bs-dispatch-batch-audit.mjs audit [--transcript file | --projects-root dir] [--run-tmp dir] [--since iso] [--format json|text] [--strict]',
    )
  const opts = parseArgs(rest)
  const files = opts.transcript ? [opts.transcript] : [...walkJsonlFiles(opts.projectsRoot)]
  const dispatches = collectFromFiles(files, opts)
  const result = auditBatching(dispatches, { phaseRules: DEFAULT_PHASE_RULES })
  if (opts.format === 'json') process.stdout.write(`${JSON.stringify(result, null, 2)}\n`)
  else process.stdout.write(`${renderText(result)}\n`)
  if (result.phases.some((phase) => phase.verdict === 'serial')) return 1
  if (opts.strict && result.phases.some((phase) => phase.verdict === 'indeterminate')) return 1
  return 0
}

if (isMainModule(import.meta.url)) {
  try {
    process.exitCode = main()
  } catch (error) {
    process.stderr.write(`${error.message}\n`)
    process.exitCode = 2
  }
}
