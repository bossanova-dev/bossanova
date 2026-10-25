#!/usr/bin/env node
// guard-id: e2e-criterion-ids
// guard-id: feature-coverage-fresh
// guard-id: criterion-proofs-kept
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import ts from 'typescript'
import * as prettier from 'prettier'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { loadFeatureMap } from '../../.claude/skills/verify/lib/features.mjs'
import { compareBaseline, formatBaseline, readBaseline } from './baseline.mjs'
import { scannedFiles } from './registry.mjs'
import lintRecord from './registry/e2e-criterion-ids.mjs'
import pageRecord from './registry/feature-coverage-fresh.mjs'
import proofsRecord from './registry/criterion-proofs-kept.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const PAGE = 'docs/features/coverage.md'
/** @typedef {{file: string, name: string, ids: string[], surface: string, level: string, layout: boolean, computed?: boolean, unknownTags?: string[]}} Proof */

function featureNames(features) {
  const names = new Map()
  for (const feature of features) {
    const key = feature.id.replaceAll('-', '').toLowerCase()
    if (names.has(key))
      throw new Error(`colliding Go feature ids: ${names.get(key)} and ${feature.id}`)
    names.set(key, feature.id)
  }
  return names
}
function goTests(file, source, features, e2e) {
  const names = featureNames(features)
  const surface = file.endsWith('_cli_test.go')
    ? 'cli'
    : file.endsWith('_tui_test.go')
      ? 'tui'
      : 'unknown'
  return [...source.matchAll(/^func\s+(Test\w+)\s*\(\s*\w+\s+\*testing\.T\s*\)/gm)].map((match) => {
    const name = match[1]
    const ids = []
    for (const segment of name.matchAll(/_([A-Za-z][A-Za-z0-9]*)(_(?:AC\d+_?)+)/g)) {
      const feature = names.get(segment[1].toLowerCase()) ?? segment[1]
      for (const criterion of segment[2].matchAll(/AC\d+/g)) ids.push(`${feature}.${criterion[0]}`)
    }
    return {
      file,
      name,
      ids: [...new Set(ids)],
      surface,
      level: e2e ? `e2e-${surface}` : 'go',
      layout: name.startsWith('TestLayout_'),
    }
  })
}
/** Top-level Go e2e tests, classified by file suffix. @returns {Proof[]} */
export function goE2eTests(file, source, features) {
  return goTests(file, source, features, true)
}
const literal = (node) => (node && ts.isStringLiteralLike(node) ? node.text : null)
function callKind(expression) {
  const parts = []
  while (ts.isPropertyAccessExpression(expression)) {
    parts.unshift(expression.name.text)
    expression = expression.expression
  }
  if (!ts.isIdentifier(expression) || !['test', 'it'].includes(expression.text)) return null
  if (parts.includes('describe')) return 'describe'
  return parts.every((part) => ['only', 'skip', 'fixme', 'fail'].includes(part)) ? 'test' : null
}
function optionsTags(node) {
  if (!node || !ts.isObjectLiteralExpression(node)) return { tags: [], computed: false }
  const tags = []
  let computed = false
  for (const prop of node.properties) {
    if (prop.name?.getText().replaceAll(/["']/g, '') !== 'tag') continue
    if (!ts.isPropertyAssignment(prop)) {
      computed = true
      continue
    }
    const values = ts.isArrayLiteralExpression(prop.initializer)
      ? prop.initializer.elements
      : [prop.initializer]
    for (const value of values) {
      const text = literal(value)
      if (text === null) computed = true
      else tags.push(text)
    }
  }
  return { tags, computed }
}
/** Parse literal Playwright tags and inherited describe tags using the TypeScript AST. @returns {Proof[]} */
export function playwrightTests(file, source) {
  const tree = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS)
  const tests = []
  function visit(node, titles = [], inherited = [], inheritedComputed = false) {
    if (ts.isCallExpression(node)) {
      const kind = callKind(node.expression)
      const titleNode = node.arguments[0]
      const title =
        literal(titleNode) ??
        (titleNode &&
        !ts.isFunctionLike(titleNode) &&
        node.arguments.some(
          (argument) => ts.isArrowFunction(argument) || ts.isFunctionExpression(argument),
        )
          ? titleNode.getText(tree)
          : null)
      if (kind && title !== null) {
        const own = optionsTags(node.arguments[1])
        const tags = [...inherited, ...own.tags]
        const computed = inheritedComputed || own.computed
        if (kind === 'describe') {
          for (const argument of node.arguments) {
            if (ts.isArrowFunction(argument) || ts.isFunctionExpression(argument))
              visit(argument.body, [...titles, title], tags, computed)
          }
          return
        }
        tests.push({
          file,
          name: [...titles, title].join(' > '),
          ids: [
            ...new Set(
              tags.filter((tag) => /^@[^\s]+\.AC\d+$/.test(tag)).map((tag) => tag.slice(1)),
            ),
          ],
          surface: 'web',
          level: file.startsWith('services/web/e2e/real/') ? 'e2e-web-real' : 'e2e-web',
          layout: tags.includes('@layout'),
          computed,
          unknownTags: tags.filter(
            (tag) =>
              !/^@[^\s]+\.AC\d+$/.test(tag) && !['@layout', '@mobile', '@quarantine'].includes(tag),
          ),
        })
        return
      }
    }
    ts.forEachChild(node, (child) => visit(child, titles, inherited, inheritedComputed))
  }
  visit(tree)
  return tests
}
/** Non-e2e Go name segments and vitest title tokens. @returns {Proof[]} */
export function lowerLevelProofs(file, source, features) {
  if (file.endsWith('_test.go'))
    return goTests(file, source, features, false).filter((proof) => proof.ids.length)
  const tree = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const tests = []
  function visit(node) {
    if (ts.isCallExpression(node) && callKind(node.expression) === 'test') {
      const title = literal(node.arguments[0])
      if (title !== null) {
        const ids = [...title.matchAll(/(?:^|\s)@([a-z][a-z0-9-]*\.AC\d+)\b/g)].map(
          (match) => match[1],
        )
        if (ids.length)
          tests.push({
            file,
            name: title,
            ids: [...new Set(ids)],
            surface: 'web',
            level: 'vitest',
            layout: false,
          })
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(tree)
  return tests
}
/** Stable shrinking-baseline keys for e2e violations. */
export function lintOffenders(features, tests) {
  featureNames(features)
  const criteria = new Set(
    features.flatMap((feature) => feature.criteria.map((criterion) => criterion.id)),
  )
  const offenders = new Set()
  const counts = new Map()
  for (const test of tests) {
    const key = `${test.file}::${test.name}`
    if (test.surface === 'unknown') offenders.add(`unknown-surface ${test.file}`)
    if (!test.ids.length && !test.layout) offenders.add(`missing-id ${key}`)
    if (test.computed) offenders.add(`computed-tag ${key}`)
    for (const id of [...test.ids, ...(test.unknownTags ?? [])]) {
      if (!criteria.has(id)) offenders.add(`unknown-id ${key}::${id}`)
    }
    for (const id of test.ids) {
      const duplicate = `${id} ${test.surface}`
      counts.set(duplicate, (counts.get(duplicate) ?? 0) + 1)
    }
  }
  for (const [key, count] of counts) if (count > 1) offenders.add(`duplicate ${key}`)
  return [...offenders].sort()
}
/** Criteria with no proof at any automated level. */
export function unprovenCriteria(features, proofs) {
  const proven = new Set(proofs.flatMap((proof) => proof.ids))
  return features
    .flatMap((feature) => feature.criteria.map((criterion) => criterion.id))
    .filter((id) => !proven.has(id))
    .sort()
}
/** Deterministic markdown; callers format with the repository's Prettier configuration. */
export function renderCoveragePage(features, proofs) {
  const lines = [
    '---',
    'summary: Automated acceptance criterion proofs by feature and test level.',
    '---',
    '',
    '# Feature coverage',
    '',
    'Generated by `make feature-coverage-update`. Tests generated in loops count once per call site.',
    '',
    '| Feature | Criteria | Proven | Unproven | CLI | TUI | Web | Web real | Layout |',
    '| --- | --- | --- | --- | --- | --- | --- | --- | --- |',
  ]
  const unproven = new Set(unprovenCriteria(features, proofs))
  for (const feature of features) {
    const tests = proofs.filter((proof) =>
      proof.ids.some((id) => feature.criteria.some((criterion) => criterion.id === id)),
    )
    const missing = feature.criteria.filter((criterion) => unproven.has(criterion.id)).length
    const count = (level) => tests.filter((proof) => proof.level === level).length
    lines.push(
      `| ${feature.id} | ${feature.criteria.length} | ${feature.criteria.length - missing} | ${missing} | ${count('e2e-cli')} | ${count('e2e-tui')} | ${count('e2e-web')} | ${count('e2e-web-real')} | ${tests.filter((proof) => proof.layout).length} |`,
    )
  }
  lines.push(
    '',
    `Layout tests across all features: ${proofs.filter((proof) => proof.layout).length}.`,
    '',
  )
  for (const feature of features) {
    lines.push(
      `## ${feature.title}`,
      '',
      '| Criterion | Out of stack | e2e-cli | e2e-tui | e2e-web | e2e-web-real | go | vitest |',
      '| --- | --- | --- | --- | --- | --- | --- | --- |',
    )
    for (const criterion of feature.criteria) {
      const cells = ['e2e-cli', 'e2e-tui', 'e2e-web', 'e2e-web-real', 'go', 'vitest'].map(
        (level) => {
          const matches = proofs.filter(
            (proof) => proof.level === level && proof.ids.includes(criterion.id),
          )
          return (
            [
              ...new Set(
                matches.map((proof) =>
                  level === 'go' ? path.posix.dirname(proof.file) : proof.file,
                ),
              ),
            ]
              .sort()
              .map((value) => `\`${value}\``)
              .join('<br>') || '—'
          )
        },
      )
      lines.push(
        `| ${criterion.id} | ${criterion.outOfStack ? 'yes' : '—'} | ${cells.join(' | ')} |`,
      )
    }
    lines.push('')
  }
  return lines.join('\n')
}

function collect(root, record, features) {
  const proofs = []
  for (const file of scannedFiles(root, record.scans)) {
    if (!fs.existsSync(path.join(root, file))) continue
    const source = fs.readFileSync(path.join(root, file), 'utf8')
    if (file.startsWith('services/boss/e2e/') && file.endsWith('_test.go'))
      proofs.push(...goE2eTests(file, source, features))
    else if (file.startsWith('services/web/e2e/') && /\.spec\.tsx?$/.test(file))
      proofs.push(...playwrightTests(file, source))
    else if (
      /^(services|lib|plugins)\/.*_test\.go$/.test(file) ||
      /^services\/web\/src\/.*\.test\.tsx?$/.test(file)
    )
      proofs.push(...lowerLevelProofs(file, source, features))
  }
  return proofs
}
async function main() {
  const { positionals, values } = parseArgs({
    allowPositionals: true,
    strict: true,
    options: {
      check: { type: 'boolean' },
      update: { type: 'boolean' },
      write: { type: 'boolean' },
      root: { type: 'string', default: ROOT },
    },
  })
  const [verb] = positionals
  if (
    positionals.length !== 1 ||
    !['lint', 'page', 'proofs'].includes(verb) ||
    (verb === 'page'
      ? !values.check === !values.write || values.update
      : !values.check === !values.update || values.write)
  )
    throw new Error(
      'usage: feature-coverage.mjs lint|proofs --check|--update; page --check|--write',
    )
  const root = path.resolve(values.root)
  const map = loadFeatureMap(path.join(root, 'docs/features'))
  const dropped = map.warnings.filter((warning) =>
    ['missing-id', 'bad-id', 'missing-owns', 'duplicate-id'].includes(warning.code),
  )
  if (dropped.length)
    throw new Error(dropped.map((warning) => `${warning.file}: ${warning.code}`).join('\n'))
  const record = verb === 'lint' ? lintRecord : verb === 'page' ? pageRecord : proofsRecord
  const proofs = collect(root, record, map.features)
  let failures = []
  if (verb === 'page') {
    const config = await prettier.resolveConfig(path.join(ROOT, PAGE))
    const content = await prettier.format(renderCoveragePage(map.features, proofs), {
      ...config,
      parser: 'markdown',
    })
    if (values.write) fs.writeFileSync(path.join(root, PAGE), content)
    else if (
      !fs.existsSync(path.join(root, PAGE)) ||
      fs.readFileSync(path.join(root, PAGE), 'utf8') !== content
    )
      failures.push(`${PAGE} is stale: run make feature-coverage-update`)
  } else {
    const offenders =
      verb === 'lint' ? lintOffenders(map.features, proofs) : unprovenCriteria(map.features, proofs)
    if (values.update) fs.writeFileSync(path.join(root, record.baseline), formatBaseline(offenders))
    else {
      const baseline = readBaseline(root, record.baseline)
      failures = [
        ...baseline.errors,
        ...compareBaseline({
          baselinePath: record.baseline,
          entries: baseline.entries,
          offenders,
          remedy:
            verb === 'lint'
              ? 'name a real criterion and keep at most one e2e proof per surface'
              : 'restore an automated proof or explicitly baseline the unproven criterion',
        }).failures,
      ]
    }
  }
  for (const failure of failures) console.error(failure)
  if (failures.length) process.exitCode = 1
  else console.log(`feature-coverage ${verb}: PASS`)
}
if (isMainModule(import.meta.url))
  main().catch((error) => {
    console.error(error.message)
    process.exitCode = 1
  })
