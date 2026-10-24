#!/usr/bin/env node
// Sweep-only baseline selection and exception expiry. No guard runs expiry enforcement.
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { trackedFiles } from '../dev/tree-scan-lib.mjs'
import { readBaseline, splitBaselineEntry } from '../guards/baseline.mjs'
import { expiredExceptions, parseExceptions } from '../guards/exceptions.mjs'
import { loadRegistry, scannedFiles } from '../guards/registry.mjs'
import { areaOf } from './bs-sweep-debt-survey.mjs'

const MAX_BUFFER = 64 << 20

function git(root, args) {
  return execFileSync('git', args, { cwd: root, encoding: 'utf8', maxBuffer: MAX_BUFFER })
}

function baseline(root, record) {
  const value = JSON.parse(fs.readFileSync(path.join(root, record.baseline), 'utf8'))
  if (Array.isArray(value) && value.every((entry) => typeof entry === 'string')) {
    const parsed = readBaseline(root, record.baseline)
    if (parsed.errors.length) throw new Error(parsed.errors.join('\n'))
    return { shape: 'list', entries: parsed.entries.map(splitBaselineEntry) }
  }
  if (
    value !== null &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    Object.values(value).every((count) => Number.isInteger(count) && count >= 0)
  ) {
    return {
      shape: 'counts',
      entries: Object.entries(value).map(([key, count]) => ({ key, reason: '', count })),
    }
  }
  return null
}

function openPRs(root, file) {
  const raw = file
    ? fs.readFileSync(file, 'utf8')
    : process.env.BS_CRON_OPEN_PRS_JSON ||
      execFileSync(
        'gh',
        ['pr', 'list', '--state', 'open', '--limit', '200', '--json', 'number,headRefName,body'],
        { cwd: root, encoding: 'utf8', maxBuffer: MAX_BUFFER },
      )
  const prs = JSON.parse(raw)
  if (!Array.isArray(prs)) throw new Error('open PR JSON must be an array')
  return prs
}

function claim(prs, marker, value) {
  return prs.find((pr) =>
    String(pr.body || '')
      .split(/\r?\n/)
      .some((line) => line.trim() === `${marker}: ${value}`),
  )
}

function pathsFor(key, files, scans) {
  const tokens = key
    .split(/[\s:|,]+/)
    .filter((token) => token.includes('/') || /\.[a-zA-Z0-9]+$/.test(token))
  const scanned = new Set(scans)
  return files.filter((file) =>
    tokens.some((token) => {
      const clean = token.replace(/\/$/, '')
      return (
        file === clean ||
        file.startsWith(`${clean}/`) ||
        (scanned.has(file) && file.endsWith(`/${clean}`))
      )
    }),
  )
}

function churn(root, ref) {
  const counts = new Map()
  const output = git(root, ['log', '--since=90.days', '--name-only', '--pretty=format:', ref])
  for (const file of output.split('\n').filter(Boolean))
    counts.set(file, (counts.get(file) || 0) + 1)
  return counts
}

function defaultRef(root) {
  try {
    git(root, ['rev-parse', '--verify', 'origin/main^{commit}'])
    return 'origin/main'
  } catch {
    return 'HEAD'
  }
}

async function recordsFor(root) {
  const loaded = await loadRegistry(root)
  if (loaded.errors.length) throw new Error(loaded.errors.join('\n'))
  return loaded.records
}

async function pick(root, values, today, prs) {
  const records = (await recordsFor(root)).filter((record) => record.baseline !== null)
  const candidates = []
  const skipped = []
  for (const record of records) {
    const data = baseline(root, record)
    if (!data) {
      skipped.push({ id: record.id, reason: 'unsupported-shape' })
      continue
    }
    if (!data.entries.length) {
      skipped.push({ id: record.id, reason: 'empty' })
      continue
    }
    candidates.push({ record, data })
  }
  if (!candidates.length) return { verdict: 'nothing-to-do', reason: 'all-empty', skipped }
  const start =
    ((Math.floor(Date.parse(today) / 86400000) % candidates.length) + candidates.length) %
    candidates.length
  let selected
  for (let offset = 0; offset < candidates.length; offset++) {
    const candidate = candidates[(start + offset) % candidates.length]
    const pr = claim(prs, 'Burn-Down-Guard', candidate.record.id)
    if (pr) {
      skipped.push({ id: candidate.record.id, reason: 'claimed', pr: pr.number })
      continue
    }
    if ((values['exclude-guard'] || []).includes(candidate.record.id)) {
      skipped.push({ id: candidate.record.id, reason: 'excluded' })
      continue
    }
    selected ??= candidate
  }
  if (!selected) return { verdict: 'nothing-to-do', reason: 'all-claimed', skipped }
  const { record, data } = selected
  const files = trackedFiles(root)
  const scans = scannedFiles(root, record.scans, { tracked: files })
  const counts = churn(root, values.ref || defaultRef(root))
  const entries = data.entries
    .map((entry) => {
      const paths = pathsFor(entry.key, files, scans)
      return {
        ...entry,
        paths,
        churn: paths.reduce((sum, file) => sum + (counts.get(file) || 0), 0),
      }
    })
    .sort((a, b) => b.churn - a.churn || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0))
  return {
    verdict: 'pick',
    guard: {
      id: record.id,
      rule: record.rule,
      command: record.command,
      baseline: record.baseline,
      shape: data.shape,
      size: data.entries.length,
    },
    entries: entries
      .slice(0, Number(values.limit || 5))
      .map((entry) => ({ ...entry, areas: [...new Set(entry.paths.map(areaOf))] })),
    skipped,
  }
}

async function expired(root, today, prs) {
  const pr = claim(prs, 'Exception-Expiry', 'yes')
  if (pr) return { verdict: 'claimed', exceptions: [], ignored: [], malformed: [], pr: pr.number }
  const files = trackedFiles(root)
  const records = await recordsFor(root)
  const scans = new Map(
    records.map((record) => [
      record.id,
      new Set(scannedFiles(root, record.scans, { tracked: files })),
    ]),
  )
  let markers
  try {
    markers = git(root, ['grep', '-l', '-z', '-F', 'guard-exception(', '--'])
      .split('\0')
      .filter(Boolean)
  } catch (error) {
    if (error.status === 1) markers = []
    else throw error
  }
  const exceptions = [],
    ignored = [],
    malformed = []
  for (const file of markers) {
    const parsed = parseExceptions(fs.readFileSync(path.join(root, file), 'utf8'))
    malformed.push(...parsed.errors.map((error) => ({ file, error })))
    for (const exception of parsed.exceptions) {
      const registered = scans.get(exception.guardId)
      if (!registered || !registered.has(file)) {
        ignored.push({ file, ...exception, reason: registered ? 'outside-scan' : 'unregistered' })
      } else if (expiredExceptions({ exceptions: [exception] }, today).length) {
        exceptions.push({ file, ...exception })
      }
    }
  }
  return { verdict: exceptions.length ? 'expired' : 'none', exceptions, ignored, malformed }
}

async function shrunk(root, values) {
  if (!values.guard || !values.base) throw new Error('shrunk requires --guard and --base')
  const record = (await recordsFor(root)).find((item) => item.id === values.guard && item.baseline)
  if (!record) throw new Error(`no baseline for guard ${values.guard}`)
  const before = JSON.parse(git(root, ['show', `${values.base}:${record.baseline}`]))
  const after = JSON.parse(fs.readFileSync(path.join(root, record.baseline), 'utf8'))
  if (
    Array.isArray(before) &&
    Array.isArray(after) &&
    [...before, ...after].every((entry) => typeof entry === 'string')
  ) {
    const oldKeys = new Set(before.map((entry) => splitBaselineEntry(entry).key))
    const newKeys = new Set(after.map((entry) => splitBaselineEntry(entry).key))
    const removed = [...oldKeys].filter((key) => !newKeys.has(key))
    const grew = [...newKeys].some((key) => !oldKeys.has(key))
    return {
      verdict: grew ? 'grew' : removed.length ? 'shrank' : 'unchanged',
      before: before.length,
      after: after.length,
      removed,
    }
  }
  const current = baseline(root, record)
  if (
    current?.shape !== 'counts' ||
    before === null ||
    typeof before !== 'object' ||
    Array.isArray(before) ||
    !Object.values(before).every((count) => Number.isInteger(count) && count >= 0)
  )
    throw new Error('unsupported baseline comparison')
  const total = (value) => Object.values(value).reduce((sum, count) => sum + count, 0)
  const grew = Object.entries(after).some(
    ([key, count]) => !Object.hasOwn(before, key) || count > before[key],
  )
  const removed = Object.keys(before).filter(
    (key) => !Object.hasOwn(after, key) || after[key] < before[key],
  )
  return {
    verdict: grew ? 'grew' : removed.length ? 'shrank' : 'unchanged',
    before: { keys: Object.keys(before).length, total: total(before) },
    after: { keys: Object.keys(after).length, total: total(after) },
    removed,
  }
}

export async function main(args = process.argv.slice(2)) {
  const { values, positionals } = parseArgs({
    args,
    strict: true,
    allowPositionals: true,
    options: {
      help: { type: 'boolean' },
      'repo-root': { type: 'string' },
      'open-prs-json': { type: 'string' },
      today: { type: 'string' },
      limit: { type: 'string' },
      'exclude-guard': { type: 'string', multiple: true },
      ref: { type: 'string' },
      guard: { type: 'string' },
      base: { type: 'string' },
    },
  })
  if (values.help) {
    process.stdout.write(
      'Usage: bs-sweep-debt-burndown.mjs <pick|expired|shrunk> [--repo-root path] [--open-prs-json file] [--today YYYY-MM-DD] [--limit N] [--exclude-guard id] [--ref ref] [--guard id --base ref]\n',
    )
    return
  }
  if (positionals.length !== 1 || !['pick', 'expired', 'shrunk'].includes(positionals[0]))
    throw new Error('expected pick, expired or shrunk')
  const root = path.resolve(
    values['repo-root'] || git(process.cwd(), ['rev-parse', '--show-toplevel']).trim(),
  )
  const today = values.today || new Date().toISOString().slice(0, 10)
  if (
    !/^\d{4}-\d{2}-\d{2}$/.test(today) ||
    new Date(`${today}T00:00:00Z`).toISOString().slice(0, 10) !== today
  )
    throw new Error('invalid --today')
  if (values.limit && (!/^\d+$/.test(values.limit) || Number(values.limit) < 1))
    throw new Error('--limit must be a positive integer')
  const command = positionals[0]
  let result
  if (command === 'shrunk') result = await shrunk(root, values)
  if (command === 'pick')
    result = await pick(root, values, today, openPRs(root, values['open-prs-json']))
  if (command === 'expired')
    result = await expired(root, today, openPRs(root, values['open-prs-json']))
  process.stdout.write(JSON.stringify(result) + '\n')
}

if (isMainModule(import.meta.url))
  main().catch((error) => {
    process.stderr.write(`${error.message}\n`)
    process.exitCode = 2
  })
