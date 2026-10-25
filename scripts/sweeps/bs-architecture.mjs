#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'
import { randomUUID } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/** @typedef {Record<string, any>} Row */
/** @param {NodeJS.ProcessEnv} [env] */
export function stateDir(env = process.env) {
  return (
    env.BS_ARCHITECTURE_STATE_DIR ||
    path.join(
      env.XDG_STATE_HOME || path.join(env.HOME || os.homedir(), '.local/state'),
      'bossanova/bs-architecture',
    )
  )
}
/** @param {string} file @returns {Row} */
const read = (file) => JSON.parse(fs.readFileSync(file, 'utf8'))
/** @param {string} value */
function timestamp(value) {
  const time = Date.parse(value)
  if (!Number.isFinite(time)) throw new Error(`invalid timestamp: ${value}`)
  return time
}
/** @param {Row} metrics */
function validateMetrics(metrics) {
  if (
    metrics.schemaVersion !== 1 ||
    !metrics.measurements ||
    !/^[a-f0-9]{7,40}$/.test(metrics.commit)
  )
    throw new Error('expected v1 metrics with commit and measurements')
  timestamp(metrics.generatedAt)
}
/** @param {string} before @param {string} [dir] */
export function previous(before, dir = stateDir()) {
  const cutoff = timestamp(before)
  if (!fs.existsSync(dir)) return null
  return (
    fs
      .readdirSync(dir)
      .filter((name) => /^metrics-\d{4}-\d{2}-\d{2}-[a-f0-9]{7}\.json$/.test(name))
      .map((name) => {
        const file = path.join(dir, name)
        const metrics = read(file)
        validateMetrics(metrics)
        return { file, time: timestamp(metrics.generatedAt) }
      })
      .filter((row) => row.time < cutoff)
      .sort((a, b) => b.time - a.time || a.file.localeCompare(b.file))[0]?.file ?? null
  )
}
/** Exclusive hard-link publication is atomic and never replaces an existing run.
 * @param {Row} metrics @param {string} [dir]
 */
export function save(metrics, dir = stateDir()) {
  validateMetrics(metrics)
  let parent = path.resolve(dir)
  const suffix = []
  while (!fs.existsSync(parent)) {
    suffix.unshift(path.basename(parent))
    parent = path.dirname(parent)
  }
  const target = path.join(fs.realpathSync(parent), ...suffix)
  const checkout = spawnSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' })
  if (checkout.status === 0) {
    const root = fs.realpathSync(checkout.stdout.trim())
    if (target === root || target.startsWith(root + path.sep))
      throw new Error('state dir must be outside the checkout')
  }
  fs.mkdirSync(dir, { recursive: true })
  const file = path.join(
    dir,
    `metrics-${new Date(metrics.generatedAt).toISOString().slice(0, 10)}-${metrics.commit.slice(0, 7)}.json`,
  )
  const body = JSON.stringify(metrics, null, 2) + '\n'
  if (fs.existsSync(file)) {
    if (fs.readFileSync(file, 'utf8') === body) return file
    throw new Error(`different run already saved: ${file}`)
  }
  const temporary = path.join(dir, `.${randomUUID()}.tmp`)
  try {
    fs.writeFileSync(temporary, body, { flag: 'wx' })
    fs.linkSync(temporary, file)
  } finally {
    fs.rmSync(temporary, { force: true })
  }
  return file
}

// These packages cannot produce useful, bounded mutation evidence.
const MUTATION_SKIP = [
  /(?:^|\/)cmd(?:\/|$)/,
  /^services\/boss\/internal\/daemon(?:\/|$)/,
  /(?:^|\/)(?:gen|generated)(?:\/|$)/,
]
/** Flatten only numeric scalar measurements; null groups stay unavailable.
 * @param {unknown} value @param {string} prefix @param {Row} out
 */
function scalars(value, prefix, out) {
  if (value === null || typeof value === 'number') out[prefix] = value
  else if (value && typeof value === 'object' && !Array.isArray(value))
    for (const [key, child] of Object.entries(value))
      scalars(child, prefix ? `${prefix}.${key}` : key, out)
}
/** @param {Row|null} before @param {Row} current @param {number} [top] */
export function diffMetrics(before, current, top = 10) {
  const a = before?.measurements ?? {}
  const b = current.measurements
  const movers = []
  const add = (kind, old, now, field) => {
    if (old == null || now == null) return
    for (const [subject, row] of Object.entries(now)) {
      const after = row[field]
      const prior = Object.hasOwn(old, subject) ? old[subject][field] : 0
      if (typeof prior === 'number' && typeof after === 'number' && after > prior)
        movers.push({ kind, subject, before: prior, after, delta: after - prior })
    }
  }
  for (const field of ['lines', 'exported']) add(`package.${field}`, a.packages, b.packages, field)
  for (const field of ['fanIn', 'fanOut']) add(`package.${field}`, a.fanInOut, b.fanInOut, field)
  const index = (rows, key) =>
    rows == null ? null : Object.fromEntries(rows.map((row) => [key(row), row]))
  add(
    'interface.methods',
    index(a.interfaces, (r) => `${r.pkg}.${r.name}`),
    index(b.interfaces, (r) => `${r.pkg}.${r.name}`),
    'methods',
  )
  add(
    'file.lines',
    index(a.largestFiles, (r) => r.file),
    index(b.largestFiles, (r) => r.file),
    'lines',
  )
  add('baseline.total', a.baselines, b.baselines, 'total')
  movers.sort(
    (x, y) =>
      y.delta - x.delta || x.kind.localeCompare(y.kind) || x.subject.localeCompare(y.subject),
  )
  const budgetCuts = []
  for (const [guard, budget] of Object.entries(b.budgets ?? {})) {
    const rows = [
      ...Object.entries(budget.subjects),
      ...budget.defaultSubjects.map(({ subject, ...row }) => [subject, row]),
    ]
    for (const [subject, row] of rows)
      if (
        typeof row.measured === 'number' &&
        typeof row.limit === 'number' &&
        row.measured <= row.limit * 0.9
      )
        budgetCuts.push({ guard, subject, before: row.limit, after: row.measured })
  }
  budgetCuts.sort((x, y) => x.guard.localeCompare(y.guard) || x.subject.localeCompare(y.subject))
  const mutationTargets =
    b.packages == null || b.churn == null
      ? null
      : Object.entries(b.packages)
          .filter(
            ([subject, row]) =>
              row.lines > 0 &&
              row.testLines > 0 &&
              b.churn[subject] > 0 &&
              !MUTATION_SKIP.some((pattern) => pattern.test(subject)),
          )
          .map(([subject]) => ({ subject, churn: b.churn[subject] }))
          .sort((x, y) => y.churn - x.churn || x.subject.localeCompare(y.subject))
          .slice(0, 3)
  const old = {},
    now = {},
    deltas = {}
  for (const name of ['testRatio', 'flakyQuarantine', 'baselines', 'e2e']) {
    scalars(a[name] ?? null, name, old)
    scalars(b[name] ?? null, name, now)
  }
  for (const name of ['packages', 'interfaces', 'fanInOut', 'largestFiles', 'budgets', 'churn'])
    if (a[name] == null || b[name] == null) deltas[name] = { unavailable: true }
  for (const key of [...new Set([...Object.keys(old), ...Object.keys(now)])].sort()) {
    deltas[key] =
      typeof old[key] === 'number' && typeof now[key] === 'number'
        ? { before: old[key], after: now[key], delta: now[key] - old[key] }
        : { unavailable: true }
  }
  return {
    comparison: {
      previous: before ? { generatedAt: before.generatedAt, commit: before.commit } : null,
      current: { generatedAt: current.generatedAt, commit: current.commit },
    },
    movers: movers.slice(0, top),
    budgetCuts,
    mutationTargets,
    deltas,
  }
}

/** @param {unknown} value */
const text = (value) => typeof value === 'string' && value.trim().length > 0
/** @param {Row} diff @param {Row} review */
export function renderReport(diff, review) {
  if (
    !Array.isArray(diff.movers) ||
    !Array.isArray(diff.budgetCuts) ||
    !diff.deltas ||
    !diff.comparison ||
    !Array.isArray(review.movers) ||
    !review.movers.every(
      (row) =>
        row &&
        text(row.subject) &&
        ['ok', 'watch', 'fix'].includes(row.verdict) &&
        text(row.rule) &&
        text(row.note),
    ) ||
    !['survivors', 'deadTests', 'testValue'].every((key) => text(review[key])) ||
    !text(review.suggestedEpic?.title) ||
    !Array.isArray(review.suggestedEpic?.issues) ||
    !review.suggestedEpic.issues.every((row) => row && text(row.title) && text(row.description))
  )
    throw new Error(
      'invalid report/review shape: narrative strings and a suggestedEpic with title/issues are required',
    )
  for (const row of diff.movers)
    if (!review.movers.some((item) => item.subject === row.subject))
      throw new Error(`mover lacks review: ${row.subject}`)
  const json = (value) => JSON.stringify(value)
  const body = [
    '# Monthly architecture report',
    '',
    '## Comparison',
    '',
    diff.comparison.previous ? json(diff.comparison) : 'No comparison available.',
    '',
    '## Reviewed top movers',
    '',
    ...review.movers.map((row) => `- ${row.subject}: ${row.verdict} — ${row.rule}. ${row.note}`),
    '',
    'Measured growth:',
    json(diff.movers),
    '',
    '## Measurement deltas',
    '',
    json(diff.deltas),
    '',
    '## Mutation survivors',
    '',
    review.survivors,
    '',
    '## Tests that catch no mutants',
    '',
    review.deadTests,
    '',
    '## Test value',
    '',
    review.testValue,
    '',
    '## Budget-cut proposals',
    '',
    json(diff.budgetCuts),
    '',
    'Race budgets are not measured: scripts/guards/race-budgets.json.',
    '',
    '## Suggested epic',
    '',
    review.suggestedEpic.title,
    '',
    ...review.suggestedEpic.issues.flatMap((row) => [`### ${row.title}`, '', row.description, '']),
  ].join('\n')
  if (Buffer.byteLength(body) > 60000)
    throw new Error('report exceeds 60,000-byte cap; shorten the review')
  return body
}
/** @param {string[]} args @param {string} root */
function git(args, root) {
  const result = spawnSync('git', args, {
    cwd: root,
    encoding: 'utf8',
    maxBuffer: 32 * 1024 * 1024,
  })
  if (result.error || result.status !== 0) throw new Error(result.error?.message || result.stderr)
  return result.stdout
}
/** @param {string} [root] */
export function snapshot(root = process.cwd()) {
  const run = (...args) => git(args, root)
  return {
    status: run('status', '--porcelain=v1', '-uall'),
    branches: run('for-each-ref', '--format=%(refname)', 'refs/heads').trim().split('\n').sort(),
    stash: run('stash', 'list'),
    // HEAD may follow a fetched remote fast-forward. Branch names and unpushed
    // commits below still expose local work while worktree membership stays exact.
    worktrees: run('worktree', 'list', '--porcelain').replace(/^HEAD .*\n/gm, ''),
    unpushed: run('rev-list', '--branches', 'HEAD', '--not', '--remotes').trim().split('\n').sort(),
  }
}

/** @param {string[]} [argv] */
export function main(argv = process.argv.slice(2)) {
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      allowPositionals: true,
      strict: true,
      options: Object.fromEntries(
        [
          'before',
          'metrics',
          'previous',
          'current',
          'top',
          'diff',
          'review',
          'out',
          'snapshot',
        ].map((name) => [name, { type: 'string' }]),
      ),
    })
    const [verb] = positionals
    if (positionals.length !== 1) throw new Error('expected one verb')
    const required = (key) => {
      if (!values[key]) throw new Error(`--${key} is required`)
      return values[key]
    }
    if (verb === 'state-dir') console.log(stateDir())
    else if (verb === 'previous') {
      const file = previous(required('before'))
      if (!file) return 3
      console.log(file)
    } else if (verb === 'save') console.log(save(read(required('metrics'))))
    else if (verb === 'diff') {
      const top = Number(values.top ?? 10)
      if (!Number.isSafeInteger(top) || top < 1) throw new Error('--top must be a positive integer')
      console.log(
        JSON.stringify(
          diffMetrics(
            values.previous ? read(values.previous) : null,
            read(required('current')),
            top,
          ),
          null,
          2,
        ),
      )
    } else if (verb === 'report')
      fs.writeFileSync(
        required('out'),
        renderReport(read(required('diff')), read(required('review'))),
      )
    else if (verb === 'snapshot')
      fs.writeFileSync(required('out'), JSON.stringify(snapshot(), null, 2))
    else if (verb === 'verify-readonly') {
      const prior = read(required('snapshot')),
        current = snapshot()
      const changed = Object.keys(current).filter(
        (key) => JSON.stringify(prior[key]) !== JSON.stringify(current[key]),
      )
      if (changed.length) {
        console.error(`read-only violation: ${changed.join(', ')}`)
        return 1
      }
      console.log('verify-readonly: passed')
    } else throw new Error(`unknown verb: ${verb}`)
    return 0
  } catch (error) {
    console.error(error.message)
    return 2
  }
}
if (isMainModule(import.meta.url)) process.exitCode = main()
