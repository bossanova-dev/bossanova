#!/usr/bin/env node
// Mechanical orchestration for a portable retrospective.
import fs from 'node:fs'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { isMainModule } from './main-module.mjs'
import { loadSkillConfig, retroConfig, selectionBlockFor } from './skill-config.mjs'
import { retroFilingSelection, parseSelectionFlags } from './selection.mjs'
import { renderChildDescription, parseNote } from './retro-notes.mjs'

const usage = (message) => {
  throw Object.assign(new Error(message), { exitCode: 64 })
}
const integer = (value, key) => {
  const n = Number(value)
  if (
    !/^\d+$/.test(String(value)) ||
    !Number.isSafeInteger(n) ||
    n < (['maxIssues', 'staleDays'].includes(key) ? 0 : 1)
  )
    usage(`${key} requires a valid integer`)
  return n
}
export function settings(
  argv = [],
  { config = {}, env = {}, now = Date.now, runDir, fs: io = fs } = {},
) {
  const parsed = parseSelectionFlags(argv)
  if (parsed.error) usage(parsed.error)
  const flags = {},
    cfg = retroConfig(config)
  let dryRun = false
  const keys = { '--max-issues': 'maxIssues', '--stale-days': 'staleDays', '--min-runs': 'minRuns' }
  for (let i = 0; i < parsed.positionals.length; i++) {
    const token = parsed.positionals[i]
    if (token === '--dry-run') {
      dryRun = true
      continue
    }
    if (token === '--no-dry-run') continue
    const [name, ...value] = token.split('=')
    if (!keys[name]) usage(`unknown argument ${token}`)
    flags[keys[name]] = integer(
      value.length ? value.join('=') : parsed.positionals[++i],
      keys[name],
    )
  }
  let selection, ignored
  try {
    ;({ selection, ignored } = retroFilingSelection(selectionBlockFor(config), parsed.flags))
  } catch (error) {
    usage(error.message)
  }
  const numeric = {},
    names = {
      maxIssues: 'BOSS_RETRO_MAX_ISSUES',
      staleDays: 'BOSS_RETRO_STALE_DAYS',
      minRuns: 'BOSS_RETRO_MIN_RUNS',
    }
  for (const [key, name] of Object.entries(names)) {
    const value = flags[key] ?? env[name] ?? cfg[key]
    if (value !== null && value !== undefined) numeric[key] = integer(value, key)
  }
  const selectArgs = ['maxIssues', 'staleDays', 'minRuns'].map((key) =>
    numeric[key] === undefined ? '' : String(numeric[key]),
  )
  while (selectArgs.length && selectArgs.at(-1) === '') selectArgs.pop()
  const runId = String(env.BOSS_AGENT_SESSION_ID || `retro-${now()}`).replace(
    /[^A-Za-z0-9._:-]/g,
    '_',
  )
  let guidanceAuditConfig = null
  if (cfg.guidanceAudit !== null && runDir) {
    guidanceAuditConfig = path.join(runDir, 'guidance-audit.json')
    io.writeFileSync(guidanceAuditConfig, JSON.stringify(cfg.guidanceAudit))
  }
  // Forward only supported slots; inherited unsupported filters must never reach resolution.
  const selectionArgs = [
    ...selection.labels.include.flatMap((label) => ['--label', label]),
    ...selection.projects.include.flatMap((project) => ['--project', project]),
  ]
  return {
    mode: dryRun ? 'dry-run' : 'write',
    runId,
    ...numeric,
    selectArgs,
    selection,
    selectionArgs,
    ignored,
    needsResolution: selectionArgs.length > 0,
    pathAliases: JSON.stringify(cfg.pathAliases),
    guidanceAuditConfig,
  }
}
export function filing(input) {
  const selection = input?.selection
  if (
    !selection ||
    !Array.isArray(selection.labels?.include) ||
    !Array.isArray(selection.projects?.include)
  )
    throw new Error('malformed resolved selection')
  if (selection.projects.include.length > 1) throw new Error('resolved project is ambiguous')
  const labels = selection.labels.include
  const project = selection.projects.include[0] ?? null
  if (
    [...labels, ...(project ? [project] : [])].some(
      (value) => typeof value !== 'string' || !value.trim(),
    )
  )
    throw new Error('malformed resolved value')
  return { labels: [...labels], project, warnings: input.warnings ?? [] }
}
export function augment(notes, { audit = [], signals = {}, runId, now = Date.now } = {}) {
  if (
    !Array.isArray(notes) ||
    !Array.isArray(audit) ||
    (signals.planned !== undefined && !Array.isArray(signals.planned))
  )
    throw new Error('malformed records')
  const synthetic = (record, source) => {
    if (!record || typeof record.body !== 'string' || !record.body.trim())
      throw new Error('record needs a body')
    const kind = record.kind === 'rule-of-the-run' ? 'rule-of-run' : record.kind || 'signal'
    const token =
      record.runId ||
      parseNote(record).run_id ||
      record.body.match(/^Run:\s*(?:run:)?([^\s]+)\s*$/m)?.[1] ||
      runId
    return {
      id: `synthetic:${source}:${kind}:${createHash('sha256').update(record.body).digest('hex').slice(0, 16)}`,
      body: record.body,
      tags: ['improvement'],
      created_at: new Date(now()).toISOString(),
      chat_id: token,
      session_id: token,
      repeatExempt: record.repeatExempt === true,
    }
  }
  return [
    ...notes,
    ...audit.map((record) => synthetic(record, 'guidance-audit')),
    ...(signals.planned ?? []).map((record) => synthetic(record, 'signals')),
  ]
}
export function finalize(selection, snapshotIds, augmented) {
  const out = structuredClone(selection)
  const buckets = ['selected', 'deferred', 'dropped', 'expired']
  if (
    !out ||
    Object.keys(out).length !== 4 ||
    buckets.some((bucket) => !Array.isArray(out[bucket]))
  )
    throw new Error('expected exactly four buckets')
  const ids = new Set(snapshotIds),
    synthetic = new Set(
      augmented.filter((note) => note.id?.startsWith('synthetic:')).map((note) => note.id),
    ),
    keys = new Set()
  const reasons = {
    selected: ['selected'],
    deferred: ['over-cap', 'below-threshold', 'stays-rule'],
    dropped: ['already-tracked'],
    expired: ['expired'],
  }
  for (const bucket of buckets)
    for (const entry of out[bucket]) {
      const cluster = entry?.cluster
      if (
        !cluster ||
        typeof cluster.key !== 'string' ||
        !cluster.key.trim() ||
        keys.has(cluster.key)
      )
        throw new Error('empty or duplicate cluster key')
      keys.add(cluster.key)
      if (!reasons[bucket].includes(entry.reason)) throw new Error('unknown bucket reason')
      if (
        !Array.isArray(cluster.notes) ||
        !cluster.notes.length ||
        cluster.notes.some((note) => !ids.has(note.id) && !synthetic.has(note.id))
      )
        throw new Error('note outside snapshot and augmented set')
    }
  const limit = out.selected.length
  const staysRule = ({ cluster: { notes, ladder } }) =>
    notes.every((note) => note.id.startsWith('synthetic:guidance-audit:rule-of-run:')) &&
    ['rule', 'context'].includes(ladder?.rung) &&
    !ladder.existingCheck &&
    !ladder.supersedes?.length
  const retained = [],
    stays = []
  for (const entry of out.selected) {
    if (staysRule(entry)) stays.push({ ...entry, reason: 'stays-rule' })
    else retained.push(entry)
  }
  out.deferred = out.deferred.flatMap((entry) => {
    if (entry.reason !== 'over-cap') return [entry]
    if (staysRule(entry)) return [{ ...entry, reason: 'stays-rule' }]
    if (retained.length < limit) {
      retained.push({ ...entry, reason: 'selected' })
      return []
    }
    return [entry]
  })
  out.selected = retained
  out.deferred.push(...stays)
  return out
}
export function prunePlan(plan) {
  if (
    !plan ||
    !Array.isArray(plan.delete) ||
    !Array.isArray(plan.retag) ||
    [...plan.delete, ...plan.retag].some((id) => typeof id !== 'string' || !id)
  )
    throw new Error('malformed retirement plan')
  const deletion = plan.delete.filter((id) => !id.startsWith('synthetic:')),
    retag = plan.retag.filter((id) => !id.startsWith('synthetic:'))
  return {
    delete: deletion,
    retag,
    counts: { delete: deletion.length, retag: retag.length, drain: deletion.length + retag.length },
  }
}
export function preview(selection, options = {}) {
  const children = selection.selected.map(({ cluster }) => ({
    title: cluster.title || cluster.statement,
    notes: cluster.notes.length,
    runs: new Set(cluster.notes.map((note) => note.run_id).filter(Boolean)).size,
    rung: cluster.ladder?.rung,
    description: renderChildDescription(cluster, { pathExists: fs.existsSync, ...options }),
  }))
  return {
    parentTitle: 'Retrospective improvements',
    children,
    counts: Object.fromEntries(
      Object.entries(selection).map(([key, value]) => [key, value.length]),
    ),
  }
}
export function recordRun(runId, { commonDir, now = Date.now, fs: io = fs } = {}) {
  if (!runId || /[^A-Za-z0-9._:-]/.test(runId)) throw new Error('invalid run id')
  const dir = path.join(commonDir, 'boss-retro'),
    file = path.join(dir, 'state.json'),
    tmp = path.join(dir, `state.${process.pid}.${runId}.tmp`)
  const state = { version: 1, lastRunAt: new Date(now()).toISOString(), runId }
  io.mkdirSync(dir, { recursive: true })
  try {
    io.writeFileSync(tmp, JSON.stringify(state) + '\n', { flag: 'wx' })
    io.renameSync(tmp, file)
  } finally {
    if (io.existsSync(tmp)) io.unlinkSync(tmp)
  }
  return state
}
export function runCli(argv, deps = {}) {
  const [command, ...args] = argv,
    io = deps.fs ?? fs
  const read = (file) => JSON.parse(io.readFileSync(file, 'utf8'))
  const options = (tokens, allowed) => {
    const out = {}
    for (let i = 0; i < tokens.length; i += 2) {
      if (!allowed.includes(tokens[i]) || !tokens[i + 1])
        usage(`unknown or incomplete option ${tokens[i]}`)
      out[tokens[i].slice(2)] = tokens[i + 1]
    }
    return out
  }
  if (command === 'settings')
    return settings(args[0] === '--' ? args.slice(1) : args, {
      ...deps,
      config: deps.config ?? loadSkillConfig(),
      env: deps.env ?? process.env,
      runDir: deps.runDir ?? process.env.BOSS_RETRO_RUN_DIR,
    })
  if (command === 'filing' && args.length === 1) return filing(read(args[0]))
  if (command === 'augment') {
    const opts = options(args.slice(1), ['--audit', '--signals', '--run-id'])
    if (!opts['run-id']) usage('augment requires --run-id')
    return augment(read(args[0]), {
      ...deps,
      audit: opts.audit ? read(opts.audit) : [],
      signals: opts.signals ? read(opts.signals) : {},
      runId: opts['run-id'],
    })
  }
  if (command === 'finalize' && args.length === 3)
    return finalize(
      read(args[0]),
      io.readFileSync(args[1], 'utf8').trim().split(/\r?\n/).filter(Boolean),
      read(args[2]),
    )
  if (command === 'preview') {
    const opts = options(args.slice(1), ['--path-aliases'])
    return preview(read(args[0]), { pathAliases: JSON.parse(opts['path-aliases'] ?? '{}') })
  }
  if (command === 'prune-plan' && args.length === 1) return prunePlan(read(args[0]))
  if (command === 'record-run') {
    const opts = options(args, ['--run-id'])
    return recordRun(opts['run-id'], {
      ...deps,
      commonDir:
        deps.commonDir ??
        execFileSync('git', ['rev-parse', '--git-common-dir'], { encoding: 'utf8' }).trim(),
    })
  }
  usage(`unknown or malformed command ${command}`)
}
if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(JSON.stringify(runCli(process.argv.slice(2))) + '\n')
  } catch (error) {
    process.stderr.write(`retro-run: ${error.message}\n`)
    process.exitCode = error.exitCode ?? 1
  }
}
