import fs from 'node:fs'
import path from 'node:path'
import { isFixturePath } from '../layout/file-sets.mjs'
import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)

/** Tracked human-authored source, with Go generated headers supplied by gometrics.
 * @param {string[]} files @param {Set<string>} [generated]
 */
export function sourceFiles(files, generated = new Set()) {
  return files.filter(
    (file) =>
      /\.(?:go|mjs|cjs|js|ts|tsx|py)$/.test(file) &&
      !isFixturePath(file) &&
      !/(?:^|\/)gen\//.test(file) &&
      !/\.generated\.|\.pb\.go$/.test(file) &&
      !generated.has(file) &&
      !file.startsWith('.codex/') &&
      !/^\.claude\/skills\/[^/]+\/toolbox\//.test(file) &&
      !/^lib\/bossalib\/contract\/plugin\/skillpayload\/skills\/.*\/toolbox\//.test(file),
  )
}

/** @param {string} file */
export function isTest(file) {
  return (
    file.endsWith('_test.go') ||
    (/\.(?:mjs|cjs|js|ts|tsx)$/.test(file) &&
      (/\.(?:test|spec)\./.test(file) || /(?:^|\/)(?:e2e|tests)\//.test(file))) ||
    (file.endsWith('.py') &&
      (/(?:^|\/)test_[^/]+\.py$|_test\.py$/.test(file) || /(?:^|\/)tests\//.test(file)))
  )
}

/** @param {string} file */
function part(file) {
  const match = /^(services|plugins|lib)\/[^/]+/.exec(file)
  if (match) return match[0]
  if (file.startsWith('.claude/skills/')) return '.claude/skills'
  return ['scripts', 'skills-toolbox'].find((name) => file.startsWith(`${name}/`)) ?? 'other'
}

/** @param {string} root @param {string[]} files */
export function sourceMeasurements(root, files) {
  const rows = files.map((file) => ({
    file,
    lines: (fs.readFileSync(path.join(root, file), 'utf8').match(/\n/g) ?? []).length,
    test: isTest(file),
  }))
  /** @type {Record<string, {prodLines:number,testLines:number,ratio:number|null}>} */
  const parts = {}
  const total = { prodLines: 0, testLines: 0, ratio: null }
  for (const row of rows) {
    const name = part(row.file)
    const counts = (parts[name] ??= { prodLines: 0, testLines: 0, ratio: null })
    const key = row.test ? 'testLines' : 'prodLines'
    counts[key] += row.lines
    total[key] += row.lines
  }
  for (const counts of [...Object.values(parts), total])
    counts.ratio = counts.prodLines ? counts.testLines / counts.prodLines : null
  return {
    rows: rows.sort((a, b) => b.lines - a.lines || a.file.localeCompare(b.file)),
    testRatio: { parts, total },
  }
}

/** Literal Playwright declarations only; suite/step titles do not prove a criterion.
 * @param {string} file @param {string} text
 */
function playwrightTitles(file, text) {
  /** @type {typeof import('typescript')} */
  const ts = require('typescript')
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const titles = []
  function visit(node) {
    if (
      ts.isCallExpression(node) &&
      /^test(?:\.(?:only|skip|fixme|fail))?$/.test(node.expression.getText(source))
    ) {
      const title = node.arguments[0]
      if (title && (ts.isStringLiteral(title) || ts.isNoSubstitutionTemplateLiteral(title)))
        titles.push(title.text)
    }
    ts.forEachChild(node, visit)
  }
  visit(source)
  return titles
}

/** @param {string} root @param {string[]} files @param {object[]} goRows @param {object[]} features */
export function e2eMeasurements(root, files, goRows, features) {
  const byFeature = Object.fromEntries(
    features.map((feature) => [
      feature.id,
      {
        bySurface: { cli: 0, tui: 0, web: 0 },
        criteria: Object.fromEntries(feature.criteria.map((criterion) => [criterion.id, 0])),
        criteriaWithoutE2e: [],
        testsWithoutCriterion: 0,
      },
    ]),
  )
  const unmappedFiles = []
  for (const file of files.filter(
    (file) =>
      /(?:^|\/)e2e\//.test(file) &&
      (file.endsWith('_test.go') || /\.spec\.[cm]?[jt]sx?$/.test(file)),
  )) {
    const name = path
      .basename(file)
      .replace(/_(?:cli|tui)_test\.go$|_test\.go$|\.spec\.[^.]+$/g, '')
      .replace(/^real-/, '')
      .replaceAll('_', '-')
    const feature = features
      .filter((f) => name === f.id || name.startsWith(`${f.id}-`))
      .sort((a, b) => b.id.length - a.id.length)[0]
    if (!feature) {
      unmappedFiles.push(file)
      continue
    }
    const entry = byFeature[feature.id]
    const surface = file.endsWith('.go') ? (file.endsWith('_tui_test.go') ? 'tui' : 'cli') : 'web'
    const titles = file.endsWith('.go')
      ? (goRows.find((row) => row.file === file)?.tests ?? []).map((test) =>
          test.subtests.join(' '),
        )
      : playwrightTitles(file, fs.readFileSync(path.join(root, file), 'utf8'))
    for (const title of titles) {
      entry.bySurface[surface]++
      const criteria = new Set(title.match(/[a-z][a-z0-9-]*\.AC[1-9][0-9]*/g) ?? [])
      const known = [...criteria].filter((id) => Object.hasOwn(entry.criteria, id))
      if (!known.length) entry.testsWithoutCriterion++
      for (const id of known) entry.criteria[id]++
    }
  }
  for (const entry of Object.values(byFeature))
    entry.criteriaWithoutE2e = Object.keys(entry.criteria).filter((id) => !entry.criteria[id])
  return { features: byFeature, unmappedFiles: unmappedFiles.sort() }
}
