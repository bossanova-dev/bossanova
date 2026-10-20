#!/usr/bin/env node

// Type-checks the non-test .mjs files in scripts/ and skills-toolbox/ under `tsc --checkJs`
// (scripts/tsconfig.json, non-strict) through the TypeScript compiler API, then ratchets the
// failing files against a committed baseline that only shrinks (baseline.mjs):
//   - a file with a diagnostic that is not in the baseline fails, with its diagnostics printed;
//   - a baselined file that now type-checks fails until it is removed from the baseline;
//   - a diagnostic with no file (a config or global error, e.g. a missing @types/node) fails;
//   - a checked file opting out with a leading `// @ts-nocheck` fails, baselined or not.
// Diagnostics in files outside the checked set (vendored toolbox copies pulled in by an import,
// lib .d.ts files) are ignored and counted: only the canonical trees are checked.
//
// The checked set comes from the js-types registry record's `scans` through scannedFiles, so it
// is tracked files only; tsconfig.json's include/exclude serve ad-hoc `tsc -p` runs, not this.
// `--update` rewrites the baseline to the current offenders.
//
// guard-id: js-types

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import ts from 'typescript'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { compareBaseline, formatBaseline, readBaseline } from './baseline.mjs'
import record from './registry/js-types.mjs'
import { scannedFiles } from './registry.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const DEFAULT_PROJECT = 'scripts/tsconfig.json'
const DEFAULT_BASELINE = record.baseline

const TYPE_REMEDY =
  'fails tsc --checkJs and is not in the baseline; fix the types (diagnostics below)'
const NOCHECK_REMEDY = 'remove @ts-nocheck; a file leaves the check only through the baseline'
// TypeScript also honours `// @ts-nocheck: reason`, so a colon ends the directive too.
const NOCHECK_PRAGMA = /^\/\/\/?\s*@ts-nocheck(?:[\s:]|$)/i

const toPosix = (file) => file.split(path.sep).join('/')

/** True when the file opts out of checking the way TypeScript honours it: a leading // comment. */
function hasNoCheckPragma(text) {
  return (ts.getLeadingCommentRanges(text, 0) ?? []).some((range) =>
    NOCHECK_PRAGMA.test(text.slice(range.pos, range.end)),
  )
}

/** Compiler options from a tsconfig, ignoring its include/exclude (the checked set is ours). */
function readCompilerOptions(projectPath) {
  const read = ts.readConfigFile(projectPath, ts.sys.readFile)
  if (read.error) return { options: {}, errors: [read.error] }
  const parsed = ts.parseJsonConfigFileContent(
    read.config,
    ts.sys,
    path.dirname(projectPath),
    undefined,
    projectPath,
  )
  // TS18003 "No inputs were found" concerns include/exclude, which the gate does not use.
  return { options: parsed.options, errors: parsed.errors.filter((error) => error.code !== 18003) }
}

/**
 * Type-check the checked set and compare its failing files against the baseline.
 *
 * @param {{repoRoot?: string, project?: string, baseline?: string, update?: boolean}} [options]
 *   `project` and `baseline` are repo-relative. `update` rewrites the baseline to the current
 *   offenders (skipped when a config-level diagnostic makes the result untrustworthy).
 * @returns {{failures: string[], newViolations: string[], staleEntries: string[],
 *   offenders: string[], checked: number, baselined: number, ignored: number, details: string,
 *   updated: boolean}}
 */
export function checkJsTypes({
  repoRoot = REPO_ROOT,
  project = DEFAULT_PROJECT,
  baseline = DEFAULT_BASELINE,
  update = false,
} = {}) {
  // TypeScript reports imported files by their real path; resolve the root the same way so a
  // checked file reached through an import still matches (os.tmpdir() is a symlink on macOS).
  const root = fs.realpathSync(repoRoot)
  // A tracked file deleted in the working tree has nothing left to check.
  const checked = scannedFiles(root, record.scans).filter(
    (file) =>
      file.endsWith('.mjs') && !file.endsWith('.test.mjs') && fs.existsSync(path.join(root, file)),
  )
  const checkedSet = new Set(checked)
  const { options, errors: configErrors } = readCompilerOptions(path.join(root, project))
  const program = ts.createProgram(
    checked.map((file) => path.join(root, file)),
    { ...options, noEmit: true },
  )

  const formatHost = {
    getCanonicalFileName: (file) => file,
    getCurrentDirectory: () => root,
    getNewLine: () => '\n',
  }
  const globalDiagnostics = [...configErrors]
  /** @type {Map<string, ts.Diagnostic[]>} */
  const byFile = new Map()
  let ignored = 0
  for (const diagnostic of ts.getPreEmitDiagnostics(program)) {
    if (!diagnostic.file) {
      globalDiagnostics.push(diagnostic)
      continue
    }
    const file = toPosix(path.relative(root, diagnostic.file.fileName))
    if (!checkedSet.has(file)) {
      ignored += 1
      continue
    }
    if (!byFile.has(file)) byFile.set(file, [])
    byFile.get(file).push(diagnostic)
  }

  const nocheck = checked.filter((file) =>
    hasNoCheckPragma(fs.readFileSync(path.join(root, file), 'utf8')),
  )
  const offenders = [...new Set([...byFile.keys(), ...nocheck])].sort()
  const failures = globalDiagnostics.map(
    (diagnostic) =>
      `config: ${ts.flattenDiagnosticMessageText(diagnostic.messageText, ' ').replace(/\s+/g, ' ')} (TS${diagnostic.code})`,
  )

  let entries = []
  let updated = false
  if (update && globalDiagnostics.length === 0) {
    fs.writeFileSync(path.join(root, baseline), formatBaseline(offenders))
    entries = offenders
    updated = true
  } else {
    const read = readBaseline(root, baseline)
    failures.push(...read.errors)
    entries = read.entries
  }

  // A @ts-nocheck file gets its own remedy rather than the type-error one; a baselined one stays
  // an offender, so it does not also read as a stale entry.
  const baselineSet = new Set(entries)
  const compared = compareBaseline({
    baselinePath: baseline,
    entries,
    offenders: offenders.filter((file) => !nocheck.includes(file) || baselineSet.has(file)),
    remedy: TYPE_REMEDY,
  })
  failures.push(...compared.failures, ...nocheck.map((file) => `${file}: ${NOCHECK_REMEDY}`))
  const newViolations = [
    ...new Set([...compared.newViolations, ...nocheck.filter((file) => !baselineSet.has(file))]),
  ].sort()

  const details = [
    ts.formatDiagnostics(globalDiagnostics, formatHost),
    ...newViolations.map((file) => ts.formatDiagnostics(byFile.get(file) ?? [], formatHost)),
  ].join('')

  return {
    failures,
    newViolations,
    staleEntries: compared.staleEntries,
    offenders,
    checked: checked.length,
    baselined: entries.length,
    ignored,
    details,
    updated,
  }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      update: { type: 'boolean', default: false },
      root: { type: 'string', default: REPO_ROOT },
      project: { type: 'string', default: DEFAULT_PROJECT },
      baseline: { type: 'string', default: DEFAULT_BASELINE },
    },
    allowPositionals: false,
    strict: true,
  })
  const result = checkJsTypes({
    repoRoot: values.root,
    project: values.project,
    baseline: values.baseline,
    update: values.update,
  })
  if (result.updated) {
    console.log(`Wrote ${values.baseline} (${result.offenders.length} entries)`)
  }
  const summary = `${result.checked} files checked, ${result.baselined} baselined, ${result.ignored} ignored out-of-scope diagnostics`
  if (result.failures.length > 0) {
    console.error('JS type check failed:')
    for (const failure of result.failures) console.error(`  ${failure}`)
    if (result.details) process.stderr.write(`\n${result.details}\n`)
    console.error(summary)
    process.exit(1)
  }
  console.log(`JS type check OK (${summary})`)
}
