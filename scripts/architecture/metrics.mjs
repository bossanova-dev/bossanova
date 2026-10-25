#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { spawnSync } from 'node:child_process'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { listWorkspacePackages } from '../guards/bossalib-consumers.mjs'
import { countBaselineEntries } from '../guards/baseline.mjs'
import { loadRegistry, REGISTRY_DIR } from '../guards/registry.mjs'
import { readBudgets } from '../guards/budgets.mjs'
import { measureBudgetedGuards } from '../guards/file-size.mjs'
import { QUARANTINE_FILE } from '../guards/check-flaky-quarantine.mjs'
import { loadFeatureMap } from '../../.claude/skills/verify/lib/features.mjs'
import { sourceFiles, sourceMeasurements, e2eMeasurements, isTest } from './source.mjs'

const TOOL = fileURLToPath(new URL('../../tools/gometrics', import.meta.url))
const ROOT = fileURLToPath(new URL('../../', import.meta.url))

/** @param {string} root @param {string} command @param {string[]} args @param {object} [options] */
function run(root, command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: root,
    encoding: 'utf8',
    maxBuffer: 128 * 1024 * 1024,
    ...options,
  })
  if (result.error || result.status !== 0)
    throw new Error(result.error?.message ?? result.stderr ?? `${command} failed`)
  return result.stdout
}

/** @param {object[]} listing @param {object[]} rows @param {string} root */
function packages(listing, rows, root) {
  const byDir = Object.fromEntries(
    listing.map((pkg) => [
      path.relative(root, pkg.Dir).split(path.sep).join('/'),
      { lines: 0, testLines: 0, files: 0, exported: 0 },
    ]),
  )
  const interfaces = []
  for (const row of rows) {
    const entry = byDir[row.dir]
    if (!entry || row.generated) continue
    if (row.test) {
      entry.testLines += row.lines
      continue
    }
    entry.lines += row.lines
    entry.files++
    entry.exported += row.exported
    for (const face of row.interfaces) interfaces.push({ pkg: row.dir, ...face })
  }
  const names = new Set(listing.map((pkg) => pkg.ImportPath))
  const fanInOut = Object.fromEntries(
    [...names].sort().map((name) => [name, { fanIn: 0, fanOut: 0 }]),
  )
  for (const pkg of listing)
    for (const imported of new Set(pkg.Imports ?? []))
      if (names.has(imported)) {
        fanInOut[pkg.ImportPath].fanOut++
        fanInOut[imported].fanIn++
      }
  return {
    packages: Object.fromEntries(Object.entries(byDir).sort(([a], [b]) => a.localeCompare(b))),
    interfaces: interfaces.sort(
      (a, b) => b.methods - a.methods || a.pkg.localeCompare(b.pkg) || a.name.localeCompare(b.name),
    ),
    fanInOut,
  }
}

/** @param {string} root @param {number} top */
function budgets(root, top) {
  const { budgets, errors } = readBudgets(root)
  if (errors.length) throw new Error(errors.join('; '))
  const measured = measureBudgetedGuards(root)
  return Object.fromEntries(
    Object.entries(budgets).map(([id, budget]) => {
      const counts = measured.get(id)
      const row = (subject, limit) => ({
        measured: counts?.get(subject) ?? null,
        limit,
        used: counts?.has(subject) ? counts.get(subject) / limit : null,
      })
      return [
        id,
        {
          default: budget.default ?? null,
          subjects: Object.fromEntries(
            Object.entries(budget.subjects ?? {}).map(([subject, limit]) => [
              subject,
              row(subject, limit),
            ]),
          ),
          defaultSubjects:
            budget.default === undefined
              ? []
              : [...(counts ?? [])]
                  .filter(([subject]) => !Object.hasOwn(budget.subjects ?? {}, subject))
                  .map(([subject]) => ({ subject, ...row(subject, budget.default) }))
                  .sort((a, b) => b.used - a.used || a.subject.localeCompare(b.subject))
                  .slice(0, top),
        },
      ]
    }),
  )
}

/** Count both sides of historical changes, including paths absent from the current tree.
 * @param {string} root @param {string} since
 */
function historicalChurn(root, since) {
  const output = run(root, 'git', [
    'log',
    `--since=${since}`,
    '--raw',
    '--numstat',
    '--no-abbrev',
    '--no-renames',
    '--format=',
    '-z',
    '--',
    '*.go',
  ])
  const tokens = output.split('\0')
  const pending = new Map()
  const changes = []
  const blobs = new Set()
  for (let i = 0; i < tokens.length; i++) {
    const token = tokens[i].replace(/^\n+/, '')
    if (token.startsWith(':')) {
      const [, , before, after] = token.split(' ')
      pending.set(tokens[++i], { before, after })
      continue
    }
    const stat = /^(\d+)\t(\d+)\t([\s\S]+)$/.exec(token)
    if (!stat) continue
    const [, added, deleted, file] = stat
    const versions = pending.get(file)
    if (!versions || isTest(file) || !sourceFiles([file]).length) continue
    /** @type {[string, number][]} */
    const sides = [
      [versions.after, Number(added)],
      [versions.before, Number(deleted)],
    ]
    for (const [blob, lines] of sides)
      if (lines > 0 && !/^0+$/.test(blob)) {
        changes.push({ file, blob, lines })
        blobs.add(blob)
      }
  }
  if (!blobs.size) return {}
  const snapshot = fs.mkdtempSync(path.join(os.tmpdir(), 'architecture-churn-'))
  try {
    // The batch protocol carries byte lengths so source newlines cannot split records.
    const data = Buffer.from(
      run(root, 'git', ['cat-file', '--batch'], {
        input: [...blobs].join('\n') + '\n',
        encoding: null,
      }),
    )
    let offset = 0
    while (offset < data.length) {
      const end = data.indexOf(10, offset)
      if (end < 0) throw new Error('incomplete git blob header')
      const [blob, type, size] = data.subarray(offset, end).toString('utf8').split(' ')
      const length = Number(size)
      if (
        type !== 'blob' ||
        !Number.isSafeInteger(length) ||
        length < 0 ||
        end + 1 + length >= data.length
      )
        throw new Error('invalid git blob response')
      fs.writeFileSync(path.join(snapshot, `${blob}.go`), data.subarray(end + 1, end + 1 + length))
      offset = end + length + 2
    }
    const rows = JSON.parse(
      run(TOOL, 'go', ['run', '.', '--root', snapshot], {
        input: [...blobs].map((blob) => `${blob}.go`).join('\n') + '\n',
        env: { ...process.env, GOWORK: 'off' },
      }),
    )
    const generated = new Set(
      rows.filter((row) => row.generated).map((row) => path.basename(row.file, '.go')),
    )
    const counts = {}
    for (const { file, blob, lines } of changes)
      if (!generated.has(blob)) {
        const dir = path.posix.dirname(file)
        counts[dir] = (counts[dir] ?? 0) + lines
      }
    return counts
  } finally {
    fs.rmSync(snapshot, { recursive: true, force: true, maxRetries: 3 })
  }
}

/** @param {{root?:string,since?:string,top?:number}} [options] */
export async function generateMetrics({ root = ROOT, since, top = 25 } = {}) {
  root = fs.realpathSync(root)
  /** @type {{measurement:string,reason:string}[]} */
  const unavailable = []
  /** @type {Record<string, any>} */
  const measurements = {}
  const capture = async (name, action) => {
    try {
      return await action()
    } catch (error) {
      unavailable.push({ measurement: name, reason: error.message })
      return null
    }
  }
  const commit = await capture('commit', () => run(root, 'git', ['rev-parse', 'HEAD']).trim())
  const tracked = await capture('source', () =>
    run(root, 'git', ['ls-files', '-z']).split('\0').filter(Boolean),
  )
  let goRows = await capture('goSource', () =>
    JSON.parse(
      run(TOOL, 'go', ['run', '.', '--root', root], {
        input:
          sourceFiles(tracked ?? [])
            .filter((file) => file.endsWith('.go'))
            .join('\n') + '\n',
        env: { ...process.env, GOWORK: 'off' },
      }),
    ),
  )
  const generated = new Set((goRows ?? []).filter((row) => row.generated).map((row) => row.file))
  const files = tracked ? sourceFiles(tracked, generated) : null
  if (files) {
    const source = await capture('source', () => sourceMeasurements(root, files))
    measurements.largestFiles = source?.rows.slice(0, top) ?? null
    measurements.testRatio = source?.testRatio ?? null
  } else {
    measurements.largestFiles = null
    measurements.testRatio = null
  }
  const workspace = await capture('packages', () =>
    listWorkspacePackages(root, 'ImportPath,Imports,Dir,Error,DepsErrors'),
  )
  if (workspace && goRows && files)
    Object.assign(
      measurements,
      packages(
        workspace.listing,
        goRows.filter((row) => files.includes(row.file)),
        root,
      ),
    )
  else
    for (const name of ['packages', 'interfaces', 'fanInOut']) {
      measurements[name] = null
      if (!unavailable.some((item) => item.measurement === name))
        unavailable.push({
          measurement: name,
          reason: 'Go source or workspace listing unavailable',
        })
    }
  measurements.baselines = await capture('baselines', async () => {
    if (!fs.existsSync(path.join(root, REGISTRY_DIR))) throw new Error('guard registry absent')
    const { records, errors } = await loadRegistry(root)
    if (errors.length) throw new Error(errors.join('; '))
    return Object.fromEntries(
      records
        .filter((record) => record.baseline)
        .map((record) => [
          record.id,
          {
            baseline: record.baseline,
            ...countBaselineEntries(
              JSON.parse(fs.readFileSync(path.join(root, record.baseline), 'utf8')),
            ),
            boundary: record.boundary === true,
          },
        ]),
    )
  })
  measurements.budgets = await capture('budgets', () => budgets(root, top))
  measurements.e2e = await capture('e2e', () => {
    if (!goRows || !files) throw new Error('source unavailable')
    const map = loadFeatureMap(path.join(root, 'docs/features'))
    if (map.warnings.length)
      throw new Error(map.warnings.map((warning) => warning.detail).join('; '))
    return e2eMeasurements(root, files, goRows, map.features)
  })
  measurements.flakyQuarantine = await capture('flakyQuarantine', () => {
    const entries = JSON.parse(fs.readFileSync(path.join(root, QUARANTINE_FILE), 'utf8'))
    return {
      entries: entries.length,
      tests: entries.reduce((total, entry) => total + (entry.tests?.length ?? 0), 0),
    }
  })
  if (since) measurements.churn = await capture('churn', () => historicalChurn(root, since))

  return {
    schemaVersion: 1,
    generatedAt: new Date().toISOString(),
    commit,
    root,
    measurements,
    unavailable,
  }
}

/** Short tables retain the complete data in JSON. @param {Awaited<ReturnType<typeof generateMetrics>>} report @param {number} [top] */
export function markdownSummary(report, top = 25) {
  const lines = [
    '# Architecture metrics',
    '',
    `Commit: ${report.commit ?? 'unavailable'}`,
    `Generated: ${report.generatedAt}`,
    '',
  ]
  for (const [name, value] of Object.entries(report.measurements)) {
    lines.push(`## ${name}`, '')
    if (value === null) {
      lines.push('unavailable', '')
      continue
    }
    lines.push('| Subject | Measurement |', '| --- | --- |')
    let rows = Array.isArray(value)
      ? value.map((row) => [row.file ?? `${row.pkg}.${row.name}`, row])
      : Object.entries(value)
    if (name === 'budgets')
      rows = Object.entries(value)
        .flatMap(([guard, budget]) => [
          ...Object.entries(budget.subjects).map(([subject, row]) => [`${guard}: ${subject}`, row]),
          ...budget.defaultSubjects.map(({ subject, ...row }) => [`${guard}: ${subject}`, row]),
        ])
        .sort((a, b) => (b[1].used ?? -1) - (a[1].used ?? -1))
    if (name === 'e2e')
      rows = Object.entries(value.features)
        .map(([feature, row]) => [
          feature,
          {
            ...row.bySurface,
            criteriaWithoutE2e: row.criteriaWithoutE2e.length,
            testsWithoutCriterion: row.testsWithoutCriterion,
          },
        ])
        .concat([['unmappedFiles', value.unmappedFiles.length]])
    if (name === 'testRatio') rows = [...Object.entries(value.parts), ['total', value.total]]
    for (const [subject, row] of rows.slice(0, top))
      lines.push(`| ${subject} | ${JSON.stringify(row).replaceAll('|', '\\|')} |`)
    lines.push('')
  }
  if (report.unavailable.length)
    lines.push(
      '## Unavailable',
      '',
      ...report.unavailable.map((row) => `- ${row.measurement}: ${row.reason}`),
      '',
    )
  return lines.join('\n')
}

if (isMainModule(import.meta.url)) {
  try {
    const { values } = parseArgs({
      strict: true,
      options: {
        root: { type: 'string', default: ROOT },
        since: { type: 'string' },
        json: { type: 'string' },
        markdown: { type: 'string' },
        top: { type: 'string', default: '25' },
      },
    })
    const top = Number(values.top)
    if (!Number.isSafeInteger(top) || top < 1) throw new Error('--top must be a positive integer')
    if (values.since && !Number.isFinite(Date.parse(values.since)))
      throw new Error('--since must be an ISO date')
    const report = await generateMetrics({ root: values.root, since: values.since, top })
    if (values.json) fs.writeFileSync(values.json, JSON.stringify(report, null, 2) + '\n')
    const summary = markdownSummary(report, top)
    if (values.markdown) fs.writeFileSync(values.markdown, summary)
    if (!values.json && !values.markdown) process.stdout.write(summary)
  } catch (error) {
    console.error(error.message)
    process.exitCode = 2
  }
}
