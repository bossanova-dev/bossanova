#!/usr/bin/env node

// Generates the rules tables from the guard registry (scripts/guards/registry/):
//   - a rule-bullet region inside the agent instructions file, between BEGIN/END marker lines;
//   - docs/philosophy/rules.md, generated in full with every record field.
//
//   node scripts/guards/rules-table.mjs --write   regenerate both files
//   node scripts/guards/rules-table.mjs --check   fail with a diff when either file is stale
//
// --region-file and --doc-file override the two output paths (tests point them at fixtures).
//
// Rows are sorted by id and cells are not padded, inside a prettier-ignore range: prettier pads
// every table cell to its column's widest cell, so one longer rule would rewrite every row and
// parallel registrations would conflict. Unpadded, each row is its own line; a conflict that
// remains is fixed by taking either side and rerunning `make rules-table-update`.

// guard-id: rules-table-fresh

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { spliceRegion, renderDrift } from './generated-region.mjs'
import { loadRegistry } from './registry.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')

export const UPDATE_COMMAND = 'make rules-table-update'
export const BEGIN_MARKER = `<!-- BEGIN GENERATED guard-registry: run \`${UPDATE_COMMAND}\`, do not edit -->`
export const END_MARKER = '<!-- END GENERATED guard-registry -->'
export const DEFAULT_REGION_FILE = 'AGENTS.md'
export const DEFAULT_DOC_FILE = 'docs/philosophy/rules.md'

const IGNORE_START = '<!-- prettier-ignore-start -->'
const IGNORE_END = '<!-- prettier-ignore-end -->'

export const cell = (text) => String(text).replaceAll('|', '\\|')
export const code = (text) => `\`${cell(text)}\``
const row = (cells) => `| ${cells.join(' | ')} |`

export function table(header, rows) {
  return [
    IGNORE_START,
    row(header),
    row(header.map(() => '---')),
    ...rows.map(row),
    IGNORE_END,
  ].join('\n')
}

/**
 * The body between the BEGIN and END markers: a two-column table, one row per record.
 *
 * @param {object[]} records Registry records, already sorted by id.
 * @returns {string}
 */
export function renderAgentsRegion(records) {
  return [IGNORE_START, ...records.map((record) => `- ${record.rule}`), IGNORE_END, ''].join('\n')
}

/**
 * The whole generated rules doc.
 *
 * @param {object[]} records Registry records, already sorted by id.
 * @returns {string}
 */
export function renderRulesDoc(records) {
  const rows = records.map((record) => [
    code(record.id),
    cell(record.rule),
    code(record.command),
    record.wiring.make === null ? '—' : code(`make ${record.wiring.make}`),
    `${code(record.wiring.ci.workflow)} ${code(record.wiring.ci.job)}`,
    record.baseline === null ? '—' : code(record.baseline),
    `${code(record.proof.file)}: ${cell(record.proof.test)}`,
  ])
  return [
    '---',
    'summary: Repository guard rules and their enforcement commands.',
    '---',
    '',
    '# Guard rules',
    '',
    `<!-- Generated from scripts/guards/registry/ by \`${UPDATE_COMMAND}\`; do not edit. On a rebase conflict, take either side and rerun it. -->`,
    '',
    table(['id', 'rule', 'command', 'make target', 'CI job', 'baseline', 'proof'], rows),
    '',
  ].join('\n')
}

function readOrEmpty(file) {
  try {
    return fs.readFileSync(file, 'utf8')
  } catch {
    return ''
  }
}

/**
 * Regenerate (`write`) or verify (`check`) both outputs.
 *
 * @param {{repoRoot?: string, regionFile?: string, docFile?: string, mode: 'write' | 'check'}} options
 *   Output paths are resolved against repoRoot.
 * @returns {Promise<{ok: boolean, messages: string[]}>}
 */
export async function rulesTable({
  repoRoot = REPO_ROOT,
  regionFile = DEFAULT_REGION_FILE,
  docFile = DEFAULT_DOC_FILE,
  mode,
}) {
  const { records, errors } = await loadRegistry(repoRoot)
  if (errors.length > 0) {
    return { ok: false, messages: ['guard registry is malformed:', ...errors.map((e) => `  ${e}`)] }
  }
  const regionPath = path.resolve(repoRoot, regionFile)
  const docPath = path.resolve(repoRoot, docFile)
  let regionText
  try {
    regionText = fs.readFileSync(regionPath, 'utf8')
  } catch (error) {
    return { ok: false, messages: [`cannot read ${regionFile}: ${error.message}`] }
  }
  let expectedRegion
  try {
    expectedRegion = spliceRegion(
      regionText,
      renderAgentsRegion(records),
      'guard-registry',
      UPDATE_COMMAND,
    )
  } catch (error) {
    return { ok: false, messages: [`${regionFile}: ${error.message}`] }
  }
  const outputs = [
    { label: regionFile, file: regionPath, actual: regionText, expected: expectedRegion },
    {
      label: docFile,
      file: docPath,
      actual: readOrEmpty(docPath),
      expected: renderRulesDoc(records),
    },
  ]
  if (mode === 'write') {
    for (const output of outputs) {
      if (output.actual === output.expected) continue
      fs.mkdirSync(path.dirname(output.file), { recursive: true })
      fs.writeFileSync(output.file, output.expected)
    }
    return { ok: true, messages: [] }
  }
  const messages = outputs
    .filter((output) => output.actual !== output.expected)
    .map((output) => renderDrift(output, UPDATE_COMMAND))
  return { ok: messages.length === 0, messages }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      write: { type: 'boolean', default: false },
      check: { type: 'boolean', default: false },
      'region-file': { type: 'string', default: DEFAULT_REGION_FILE },
      'doc-file': { type: 'string', default: DEFAULT_DOC_FILE },
    },
  })
  if (values.write === values.check) {
    process.stderr.write(
      'usage: node scripts/guards/rules-table.mjs (--write | --check) [--region-file <path>] [--doc-file <path>]\n',
    )
    process.exit(2)
  }
  const { ok, messages } = await rulesTable({
    regionFile: values['region-file'],
    docFile: values['doc-file'],
    mode: values.write ? 'write' : 'check',
  })
  for (const message of messages)
    process.stderr.write(message.endsWith('\n') ? message : `${message}\n`)
  if (ok && values.check) {
    console.log(`Rules tables OK (${values['region-file']}, ${values['doc-file']})`)
  }
  process.exit(ok ? 0 : 1)
}
