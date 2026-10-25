#!/usr/bin/env node
import { runGolangciSync } from '../test-infra/golangci-admission.mjs'
// guard-id: go-cognitive-complexity
// guard-id: go-interface-width
// guard-id: go-returned-interfaces
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { stripCommentsAndStrings } from './check-timing-bounds.mjs'
import { readBudgets, checkBudget } from './budgets.mjs'
import { readBaseline, compareBaseline, splitBaselineEntry } from './baseline.mjs'

export const BASELINE = 'scripts/guards/go-returned-interfaces-baseline.json'
export const GUARD_IDS = ['go-cognitive-complexity', 'go-interface-width', 'go-returned-interfaces']
const LINTERS = { gocognit: GUARD_IDS[0], interfacebloat: GUARD_IDS[1], ireturn: GUARD_IDS[2] }

/** @param {Record<string, {default?: number}>} budgets */
export function renderConfig(budgets) {
  const cognitive = budgets[GUARD_IDS[0]]?.default
  const width = budgets[GUARD_IDS[1]]?.default
  if (
    !Number.isSafeInteger(cognitive) ||
    cognitive < 1 ||
    !Number.isSafeInteger(width) ||
    width < 1
  )
    throw new Error('Go complexity budgets require positive integer defaults')
  return `version: "2"
run:
  timeout: 5m
  relative-path-mode: wd
  tests: false
issues:
  max-issues-per-linter: 0
  max-same-issues: 0
  uniq-by-line: false
linters:
  default: none
  enable: [gocognit, interfacebloat, ireturn]
  settings:
    gocognit:
      min-complexity: ${cognitive}
    interfacebloat:
      max: ${width}
    ireturn:
      allow: [anon, error, empty, stdlib]
  exclusions:
    generated: strict
    paths: ['(^|/)gen/', '_test\\.go$']
`
}

/** Resolve the declaration at golangci's position; never infer names from its prose. */
export function subjectKey(file, source, line, linter) {
  return declarationKey(file, stripCommentsAndStrings(source).split('\n'), line, linter)
}

function declarationKey(file, lines, line, linter) {
  if (!Number.isSafeInteger(line) || line < 1 || line > lines.length)
    throw new Error(`invalid declaration position ${file}:${line}`)
  const tail = lines.slice(line - 1).join('\n')
  let symbol
  if (linter === 'interfacebloat') {
    symbol = /^\s*(?:type\s+)?([\p{L}_][\p{L}\p{N}_]*)\s*(?:\[[^\]]*\]\s*)?interface\b/u.exec(
      tail,
    )?.[1]
  } else {
    const match = /^\s*func\s+(?:\(([^)]*)\)\s*)?([\p{L}_][\p{L}\p{N}_]*)\s*(?:\[|\()/u.exec(tail)
    if (match) {
      const receiver = match[1]
        ?.replace(/\[[\s\S]*$/, '')
        .trim()
        .split(/\s+/)
        .at(-1)
        ?.replace(/^\*/, '')
      symbol = receiver ? `${receiver}.${match[2]}` : match[2]
    }
  }
  if (!symbol) throw new Error(`cannot resolve ${linter} declaration at ${file}:${line}`)
  return `${path.posix.dirname(file)}:${symbol}`
}

/** @typedef {{guardId: string, subject: string, count: number}} Finding */
/** @returns {Finding[]} */
export function parseReport(text, repoRoot, moduleDir) {
  const report = JSON.parse(text)
  if (!Object.hasOwn(report, 'Issues') || !(report.Issues === null || Array.isArray(report.Issues)))
    throw new Error('golangci JSON report requires Issues array or null')
  const findings = []
  const sources = new Map()
  for (const issue of report.Issues ?? []) {
    const guardId = LINTERS[issue.FromLinter]
    if (!guardId || typeof issue.Text !== 'string' || typeof issue.Pos?.Filename !== 'string')
      throw new Error('unexpected golangci complexity finding')
    const absolute = path.resolve(repoRoot, moduleDir, issue.Pos.Filename)
    const file = path.relative(repoRoot, absolute).split(path.sep).join('/')
    if (file.startsWith('../') || !inModule(`${path.posix.dirname(file)}:`, moduleDir))
      throw new Error(`golangci finding outside module: ${file}`)
    if (!sources.has(absolute))
      sources.set(absolute, stripCommentsAndStrings(fs.readFileSync(absolute, 'utf8')).split('\n'))
    const subject = declarationKey(file, sources.get(absolute), issue.Pos.Line, issue.FromLinter)
    const numeric =
      issue.FromLinter === 'gocognit'
        ? /cognitive complexity (\d+)/.exec(issue.Text)?.[1]
        : /interface has more than \d+ methods: (\d+)/.exec(issue.Text)?.[1]
    const count = issue.FromLinter === 'ireturn' ? 1 : Number(numeric)
    if (!Number.isSafeInteger(count) || count < 1)
      throw new Error(`unrecognised ${issue.FromLinter} measurement: ${issue.Text}`)
    findings.push({ guardId, subject, count })
  }
  return findings
}

function inModule(subject, moduleDir) {
  if (moduleDir === '.') return true
  const dir = subject.split(':')[0]
  return dir === moduleDir || dir.startsWith(`${moduleDir}/`)
}

/** @param {{moduleDir: string, budgets: object, entries: string[], findings: Finding[], inactiveSubjects?: Set<string>}} input */
export function checkFindings({
  moduleDir,
  budgets,
  entries,
  findings,
  inactiveSubjects = new Set(),
}) {
  const failures = []
  for (const guardId of GUARD_IDS.slice(0, 2)) {
    const entry = budgets[guardId]
    const subjects = Object.fromEntries(
      Object.entries(entry?.subjects ?? {}).filter(
        ([s]) =>
          inModule(s, moduleDir) &&
          (!inactiveSubjects.has(s) ||
            findings.some((f) => f.subject === s && f.guardId === guardId)),
      ),
    )
    const measured = new Map(
      findings.filter((f) => f.guardId === guardId).map((f) => [f.subject, f.count]),
    )
    failures.push(
      ...checkBudget({ budgets: { [guardId]: { ...entry, subjects } }, guardId, measured })
        .failures,
    )
    for (const [subject, count] of measured) {
      if (
        Object.hasOwn(subjects, subject) &&
        count <= entry.default &&
        !inactiveSubjects.has(subject)
      )
        failures.push(`${guardId}: ${subject} no longer needs its own budget — remove it`)
    }
  }
  failures.push(
    ...compareBaseline({
      baselinePath: BASELINE,
      entries: entries
        .map((e) => splitBaselineEntry(e).key)
        .filter(
          (e) =>
            inModule(e, moduleDir) &&
            (!inactiveSubjects.has(e) ||
              findings.some((f) => f.subject === e && f.guardId === GUARD_IDS[2])),
        ),
      offenders: findings.filter((f) => f.guardId === GUARD_IDS[2]).map((f) => f.subject),
      remedy: 'go-returned-interfaces: return a concrete type or review an explicit baseline entry',
    }).failures,
  )
  return failures
}

/** Missing measurements can belong to excluded platform variants, including same-name variants.
 * Measured active declarations still obey their budgets; deleted and active-only subjects stay stale.
 */
export function inactiveSubjectsFor({ repoRoot, moduleDir, subjects, env = process.env }) {
  repoRoot = fs.realpathSync(repoRoot)
  const dirs = [
    ...new Set(subjects.filter((s) => inModule(s, moduleDir)).map((s) => s.split(':')[0])),
  ]
  if (dirs.length === 0) return new Set()
  // NUL-delimited paths preserve file names without parsing human-facing go list output.
  const template = '{{range .IgnoredGoFiles}}{{$.Dir}}/{{.}}{{"\\x00"}}{{end}}'
  const result = spawnSync(
    'go',
    [
      'list',
      '-e',
      '-f',
      template,
      ...dirs.map(
        (dir) =>
          `./${path.relative(path.join(repoRoot, moduleDir), path.join(repoRoot, dir)) || '.'}`,
      ),
    ],
    {
      cwd: path.join(repoRoot, moduleDir),
      env,
      encoding: 'utf8',
      maxBuffer: 64 * 1024 * 1024,
    },
  )
  if (result.error) throw result.error
  if (result.status !== 0) throw new Error(`Go build file selection failed: ${result.stderr}`)
  const ignored = new Set()
  for (const record of result.stdout.split('\0')) {
    const absolute = record.replace(/^\n+/, '')
    if (!absolute) continue
    if (absolute.endsWith('_test.go')) continue
    const file = path.relative(repoRoot, absolute).split(path.sep).join('/')
    const lines = stripCommentsAndStrings(fs.readFileSync(absolute, 'utf8')).split('\n')
    for (let line = 1; line <= lines.length; line++) {
      if (!/^\s*(?:func\b|.*\binterface\b)/u.test(lines[line - 1])) continue
      for (const linter of ['gocognit', 'interfacebloat']) {
        try {
          ignored.add(declarationKey(file, lines, line, linter))
        } catch {
          /* Not a declaration. */
        }
      }
    }
  }
  return ignored
}

/** @param {{repoRoot: string, moduleDir: string, budgets: object, env?: NodeJS.ProcessEnv}} options */
export function measureModule({ repoRoot, moduleDir, budgets, env = process.env }) {
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'go-complexity-'))
  try {
    const config = path.join(scratch, 'config.yml')
    const report = path.join(scratch, 'report.json')
    fs.writeFileSync(config, renderConfig(budgets))
    const result = runGolangciSync(
      [
        'run',
        '--config',
        config,
        '--output.json.path',
        report,
        '--output.text.path',
        '/dev/null',
        './...',
      ],
      {
        privateCache: true,
        cwd: path.join(repoRoot, moduleDir),
        // A private cache: golangci 2.11's warm cache returns no ireturn issues for a
        // package re-analysed after a dependency changed, and every missing finding
        // would read as a stale baseline entry.
        env: { ...env, GOLANGCI_LINT_CACHE: path.join(scratch, 'golangci-cache') },
        encoding: 'utf8',
        maxBuffer: 64 * 1024 * 1024,
      },
    )
    if (result.error) throw result.error
    if (result.status !== 0 && result.status !== 1)
      throw new Error(`golangci complexity failed (${result.status}): ${result.stderr}`)
    const findings = parseReport(fs.readFileSync(report, 'utf8'), repoRoot, moduleDir)
    if (result.status === 1 && findings.length === 0)
      throw new Error(`golangci complexity failed without findings: ${result.stderr}`)
    return findings
  } finally {
    fs.rmSync(scratch, { recursive: true, force: true, maxRetries: 5 })
  }
}

export function checkModule({ repoRoot, moduleDir }) {
  try {
    const { budgets, errors } = readBudgets(repoRoot)
    const baseline = readBaseline(repoRoot, BASELINE)
    if (errors.length || baseline.errors.length)
      return { ok: false, lines: [...errors, ...baseline.errors] }
    const findings = measureModule({ repoRoot, moduleDir, budgets })
    const subjects = [
      ...GUARD_IDS.slice(0, 2).flatMap((id) => Object.keys(budgets[id]?.subjects ?? {})),
      ...baseline.entries.map((entry) => splitBaselineEntry(entry).key),
    ]
    const inactiveSubjects = inactiveSubjectsFor({ repoRoot, moduleDir, subjects })
    const lines = checkFindings({
      moduleDir,
      budgets,
      entries: baseline.entries,
      findings,
      inactiveSubjects,
    })
    return { ok: lines.length === 0, lines }
  } catch (error) {
    return { ok: false, lines: [`Go complexity: ${error.message}`] }
  }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({ options: { module: { type: 'string' } }, strict: true })
  if (!values.module) throw new Error('--module is required')
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
  const result = checkModule({ repoRoot, moduleDir: values.module })
  for (const line of result.lines) console.error(line)
  if (!result.ok) process.exitCode = 1
}
