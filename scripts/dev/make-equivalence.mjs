#!/usr/bin/env node

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { rootMakefileSources } from './root-makefile-lib.mjs'

const PROBE = '__boss_make_equivalence_no_such_goal__'
const NORMALIZATIONS = [
  'Discard database location comments and MAKEFILE_LIST',
  'Sort .PHONY prerequisites',
  'Replace detached worktree absolute paths with <ROOT>',
  'In test-affected only, replace the exact smoke-target grep operand Makefile with $(MAKEFILE_LIST) so the reader follows included sources',
]

function run(command, args, cwd, env) {
  const result = spawnSync(command, args, {
    cwd,
    env,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
  if (result.error) throw result.error
  if (result.signal) throw new Error(`${command} terminated by ${result.signal}`)
  return { status: result.status, stdout: result.stdout, stderr: result.stderr }
}

/** Use an allowlist so secrets and inherited make overrides cannot enter a report. */
function makeEnvironment(flipped) {
  const env = {
    PATH: process.env.PATH,
    HOME: process.env.HOME,
    TMPDIR: os.tmpdir(),
    LC_ALL: 'C',
    TZ: 'UTC',
    VERSION: 'equivalence',
    COMMIT: 'equivalence',
    DATE: '2000-01-01T00:00:00Z',
  }
  return flipped
    ? {
        ...env,
        BOSS_NO_BAZEL: '1',
        RACE: '1',
        BOSS_GATE_FORCE_UNCACHED: '1',
        BOSSO_REQUIRE_POSTGRES_TESTS: '1',
        BOSSO_TEST_DATABASE_URL: 'postgres://placeholder/bosso',
        WAREHOUSE_TEST_ADMIN_URL: 'postgres://placeholder/warehouse',
      }
    : env
}

function normalize(text, root) {
  return text.split(root).join('<ROOT>')
}

/** GNU make 3.81 calls these commands; 4.x calls them recipes. */
function parseDatabase(text) {
  const variables = {}
  const rules = {}
  let section = ''
  let rule = null
  let notTarget = false
  const lines = text.split('\n')
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    if (line === '# Variables') {
      section = 'variables'
      continue
    }
    if (line === '# Implicit Rules' || line === '# Files') {
      section = 'rules'
      rule = null
      continue
    }
    if (
      line.startsWith('# files hash-table stats:') ||
      line.startsWith('# Finished Make data base')
    )
      section = ''
    if (section === 'variables') {
      const define = line.match(/^define (.+)$/)
      if (define) {
        const value = []
        while (++i < lines.length && lines[i] !== 'endef') value.push(lines[i])
        variables[define[1]] = { flavor: '=', value: value.join('\n') }
        continue
      }
      const variable = line.match(/^([^#\s][^=]*?)\s*(:=|=)\s?(.*)$/)
      if (variable && variable[1].trim() !== 'MAKEFILE_LIST') {
        variables[variable[1].trim()] = { flavor: variable[2], value: variable[3] }
      }
    }
    if (section !== 'rules') continue
    if (line === '# Not a target:') {
      notTarget = true
      continue
    }
    if (line.startsWith('\t') && rule) {
      rule.recipe.push(line)
      continue
    }
    const location = line.match(
      /^#\s+(?:commands|recipe) to execute \(from [`'](.+?)['’], line \d+\):$/,
    )
    if (location && rule) {
      rule.file = location[1]
      continue
    }
    if (!line || line.startsWith('#')) continue
    const head = line.match(/^([^\s:]+):\s?(.*)$/)
    if (!head) continue
    rule = null
    if (!notTarget) {
      const [normal, orderOnly = ''] = head[2].split(/\s*\|\s*/)
      const prerequisites = normal.trim().split(/\s+/).filter(Boolean)
      if (head[1] === '.PHONY') prerequisites.sort()
      rule = {
        prerequisites,
        orderOnly: orderOnly.split(/\s+/).filter(Boolean),
        recipe: [],
        file: null,
      }
      rules[head[1]] = rule
    }
    notTarget = false
  }
  return { variables, rules }
}

function database(root, env) {
  const result = run('make', ['-pq', '-f', 'Makefile', PROBE], root, env)
  if (
    result.status !== 2 ||
    !result.stderr.includes(`No rule to make target`) ||
    !result.stderr.includes(PROBE)
  ) {
    throw new Error(`Cannot read make database: exit ${result.status}: ${result.stderr}`)
  }
  const parsed = parseDatabase(normalize(result.stdout, root))
  const sources = rootMakefileSources(root)
  for (const [target, rule] of Object.entries(parsed.rules)) {
    rule.file ??=
      sources.find(({ text }) =>
        text
          .split('\n')
          .some((line) =>
            line
              .replace(/\$\((\w+)\)/g, (match, name) => parsed.variables[name]?.value ?? match)
              .startsWith(`${target}:`),
          ),
      )?.path ?? null
  }
  return { ...parsed, warnings: normalize(result.stderr, root) }
}

function diffs(before, after, label) {
  return [...new Set([...Object.keys(before), ...Object.keys(after)])]
    .sort()
    .flatMap((key) =>
      JSON.stringify(before[key]) === JSON.stringify(after[key])
        ? []
        : [{ kind: label, name: key, base: before[key] ?? null, head: after[key] ?? null }],
    )
}

function comparableRules(rules) {
  return Object.fromEntries(
    Object.entries(rules).map(([target, { file, ...rule }]) => [
      target,
      {
        ...rule,
        recipe:
          target === 'test-affected'
            ? rule.recipe.map((line) =>
                line.replace(
                  "grep -Eq '^test-smoke:' Makefile",
                  "grep -Eq '^test-smoke:' $(MAKEFILE_LIST)",
                ),
              )
            : rule.recipe,
      },
    ]),
  )
}

function targetManifest(rules) {
  return Object.keys(rules)
    .filter((target) => !/^\.[A-Z_]+$/.test(target) && !target.includes('%'))
    .sort()
}

/** Check expanded macro bodies too: $(call ...) may hide a recursive make. */
function recursiveRecipe(text, variables, seen = new Set()) {
  if (/\$(?:\(MAKE\)|\{MAKE\})|^\t[@-]*\+/m.test(text)) return true
  const names = [...text.matchAll(/\$[({](?:call\s+)?([\w.-]+)/g)].map((match) => match[1])
  return names.some((name) => {
    if (seen.has(name) || !variables[name]) return false
    return recursiveRecipe(variables[name].value, variables, new Set([...seen, name]))
  })
}

function unsafeTarget(target, db, visited = new Set()) {
  if (visited.has(target)) return false
  visited.add(target)
  let rule = db.rules[target]
  if (!rule) {
    const pattern = Object.keys(db.rules).find(
      (key) =>
        key.includes('%') &&
        target.startsWith(key.split('%')[0]) &&
        target.endsWith(key.split('%')[1]),
    )
    rule = pattern ? db.rules[pattern] : null
  }
  if (!rule) return false
  return (
    recursiveRecipe(rule.recipe.join('\n'), db.variables) ||
    [...rule.prerequisites, ...rule.orderOnly].some((dependency) =>
      unsafeTarget(dependency, db, visited),
    )
  )
}

/** Compare committed trees, keeping all worktrees and outputs owned by this invocation. */
export function makeEquivalence({ base, head = 'HEAD', repoRoot = process.cwd() }) {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'make-equivalence-'))
  const roots = [path.join(temp, 'base'), path.join(temp, 'head')]
  const created = []
  const report = {
    equivalent: false,
    targets: [],
    targetSetDiff: [],
    configurations: [],
    normalizations: NORMALIZATIONS,
  }
  try {
    for (const [i, ref] of [base, head].entries()) {
      const result = run(
        'git',
        ['worktree', 'add', '--detach', roots[i], ref],
        repoRoot,
        process.env,
      )
      if (result.status !== 0) throw new Error(`Cannot create ${ref} worktree: ${result.stderr}`)
      created.push(roots[i])
    }
    for (const flipped of [false, true]) {
      const env = makeEnvironment(flipped)
      const [before, after] = roots.map((root) => database(root, env))
      const manifests = [before, after].map((db) => targetManifest(db.rules))
      report.targets = manifests[1]
      report.targetSetDiff = [...new Set([...manifests[0], ...manifests[1]])].filter(
        (target) => manifests[0].includes(target) !== manifests[1].includes(target),
      )
      const databaseDiffs = [
        ...diffs(before.variables, after.variables, 'variable'),
        ...diffs(comparableRules(before.rules), comparableRules(after.rules), 'rule'),
      ]
      const databaseOnlyTargets = []
      const dryRunDiffs = []
      for (const target of [...new Set([...manifests[0], ...manifests[1]])]
        .sort()
        .concat('<default>')) {
        const goals = target === '<default>' ? [] : [target]
        const unsafe = [before, after].some((db) =>
          unsafeTarget(target === '<default>' ? db.variables['.DEFAULT_GOAL']?.value : target, db),
        )
        if (unsafe) {
          databaseOnlyTargets.push(target)
          continue
        }
        const results = roots.map((root) => {
          const result = run('make', ['-n', '--no-print-directory', ...goals], root, env)
          return {
            ...result,
            stdout: normalize(result.stdout, root),
            stderr: normalize(result.stderr, root),
          }
        })
        if (JSON.stringify(results[0]) !== JSON.stringify(results[1]))
          dryRunDiffs.push({ target, base: results[0], head: results[1] })
      }
      report.configurations.push({
        name: flipped ? 'flipped' : 'default',
        databaseDiffs,
        dryRunDiffs,
        databaseOnlyTargets,
        definingFiles: Object.fromEntries(
          manifests[1].map((target) => [target, after.rules[target].file]),
        ),
        duplicateRecipeWarnings: /overriding|ignoring old/.test(after.warnings)
          ? after.warnings
          : null,
      })
    }
    report.equivalent =
      !report.targetSetDiff.length &&
      report.configurations.every(
        (config) =>
          !config.databaseDiffs.length &&
          !config.dryRunDiffs.length &&
          !config.duplicateRecipeWarnings,
      )
    return report
  } finally {
    for (const root of created.reverse()) {
      const result = run('git', ['worktree', 'remove', '--force', root], repoRoot, process.env)
      if (result.status !== 0)
        throw new Error(`Cannot remove owned worktree ${root}: ${result.stderr}`)
    }
    fs.rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
  }
}

if (isMainModule(import.meta.url)) {
  try {
    const { values } = parseArgs({
      options: {
        base: { type: 'string' },
        head: { type: 'string', default: 'HEAD' },
        json: { type: 'boolean', default: false },
      },
      strict: true,
    })
    if (!values.base)
      throw new Error('Usage: make-equivalence.mjs --base <ref> [--head <ref>] [--json]')
    const report = makeEquivalence({ base: values.base, head: values.head })
    console.log(
      values.json
        ? JSON.stringify(report, null, 2)
        : `${report.equivalent ? 'PASS' : 'FAIL'} make equivalence\n${JSON.stringify(report, null, 2)}`,
    )
    process.exitCode = report.equivalent ? 0 : 1
  } catch (error) {
    console.error(error.message)
    process.exitCode = 2
  }
}
