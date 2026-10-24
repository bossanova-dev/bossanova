#!/usr/bin/env node

// guard-id: boundaries-doc-fresh

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { readBaseline } from './baseline.mjs'
import { renderDrift } from './generated-region.mjs'
import { loadRegistry } from './registry.mjs'
import { cell, code, table } from './rules-table.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const OUT = 'docs/structure/boundaries.md'

/** @param {string} root @param {{baseline: string|null, baselineFormat?: string}} record */
function baselineCount(root, record) {
  if (record.baseline === null) return '—'
  if (record.baselineFormat === 'json') {
    const value = JSON.parse(fs.readFileSync(path.resolve(root, record.baseline), 'utf8'))
    if (value === null || typeof value !== 'object' || Array.isArray(value))
      throw new Error(`${record.baseline} must be a JSON object`)
    return String(Object.keys(value).length)
  }
  const { entries, errors } = readBaseline(root, record.baseline)
  if (errors.length) throw new Error(errors.join('\n'))
  return String(entries.length)
}

/** @param {{root?: string, out?: string, mode: 'write'|'check'}} options */
export async function boundariesDoc({ root = ROOT, out = OUT, mode }) {
  const { records, errors } = await loadRegistry(root)
  if (errors.length) return { ok: false, messages: errors }
  let rows
  try {
    rows = records
      .filter((record) => record.boundary === true)
      .map((record) => [
        cell(record.rule),
        code(record.id),
        code(record.command),
        record.baseline === null ? '—' : code(record.baseline),
        baselineCount(root, record),
      ])
  } catch (error) {
    return { ok: false, messages: [error.message] }
  }
  const expected = [
    '---',
    'summary: Code boundary guards and their current baseline entry counts.',
    '---',
    '',
    '<!-- Generated from scripts/guards/registry/; run `make boundaries-doc-update`. Do not edit. -->',
    '',
    '# Boundaries',
    '',
    table(['boundary', 'guard id', 'command', 'baseline path', 'baseline entries'], rows),
    '',
  ].join('\n')
  const file = path.resolve(root, out)
  let actual = ''
  try {
    actual = fs.readFileSync(file, 'utf8')
  } catch {}
  if (mode === 'write') {
    if (actual !== expected) {
      fs.mkdirSync(path.dirname(file), { recursive: true })
      fs.writeFileSync(file, expected)
    }
    return { ok: true, messages: [] }
  }
  return actual === expected
    ? { ok: true, messages: [] }
    : {
        ok: false,
        messages: [renderDrift({ actual, expected, label: out }, 'make boundaries-doc-update')],
      }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    strict: true,
    options: {
      root: { type: 'string', default: ROOT },
      out: { type: 'string', default: OUT },
      write: { type: 'boolean', default: false },
      check: { type: 'boolean', default: false },
    },
  })
  if (values.write === values.check) {
    console.error(
      'usage: node scripts/guards/boundaries-doc.mjs (--write | --check) [--root <path>] [--out <path>]',
    )
    process.exit(2)
  }
  const { ok, messages } = await boundariesDoc({
    root: values.root,
    out: values.out,
    mode: values.write ? 'write' : 'check',
  })
  for (const message of messages) console.error(message)
  if (ok && values.check) console.log('Boundaries doc OK')
  process.exit(ok ? 0 : 1)
}
