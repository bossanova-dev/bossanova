#!/usr/bin/env node

// guard-id: glossary-fresh

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import * as prettier from 'prettier'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { loadEntities } from './entities.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const ENTITIES = path.join(ROOT, 'docs/structure/entities.yaml')
const OUT = path.join(ROOT, 'docs/structure/glossary.md')
const code = (value) => (value === null ? '—' : `\`${value}\``)

/** @param {ReturnType<typeof loadEntities>} data */
export function renderGlossary(data) {
  const lines = [
    '---',
    'summary: Canonical entity names across code, storage, wire messages and the UI.',
    '---',
    '',
    '<!-- Generated from docs/structure/entities.yaml; run `make glossary-update`. -->',
    '',
    '# Entity glossary',
    '',
  ]
  for (const entity of data.entities) {
    lines.push(
      `## ${entity.code}`,
      '',
      `- Noun: ${entity.noun}`,
      `- Code: ${code(entity.code)}`,
      `- Table: ${code(entity.table)}`,
      `- Proto: ${code(entity.proto)}`,
      `- UI: ${entity.ui ?? '—'}`,
      '',
      entity.definition,
      '',
    )
    if (entity.notes) lines.push(entity.notes, '')
    for (const ban of entity.bans) {
      for (const layer of ban.layers) {
        const conditions = []
        if (ban.kind === 'text') conditions.push('literal casing')
        if (ban.position && ban.position !== 'any') conditions.push(`position: ${ban.position}`)
        if (ban.except_after?.length)
          conditions.push(`except after ${ban.except_after.map(code).join(', ')}`)
        if (ban.paths?.length) conditions.push(`paths: ${ban.paths.map(code).join(', ')}`)
        if (ban.exclude_paths?.length)
          conditions.push(`excluding ${ban.exclude_paths.map(code).join(', ')}`)
        const replacement = ban.use ?? (layer === 'table' ? entity.table : entity.code)
        lines.push(
          `- Banned in ${layer}: ${code(ban.match)} → ${code(replacement)}${conditions.length ? ` (${conditions.join('; ')})` : ''}`,
        )
      }
    }
    if (entity.bans.length) lines.push('')
  }
  return lines.join('\n')
}

/** @param {{entities?: string, out?: string, mode: 'write'|'check'}} options */
export async function glossary({ entities = ENTITIES, out = OUT, mode }) {
  const data = loadEntities(entities)
  if (data.errors.length) return { ok: false, messages: [...data.warnings, ...data.errors] }
  const expected = await prettier.format(renderGlossary(data), {
    ...(await prettier.resolveConfig(OUT)),
    parser: 'markdown',
  })
  let actual = ''
  try {
    actual = fs.readFileSync(out, 'utf8')
  } catch {}
  if (mode === 'write') {
    if (actual !== expected) {
      fs.mkdirSync(path.dirname(out), { recursive: true })
      fs.writeFileSync(out, expected)
    }
    return { ok: true, messages: data.warnings }
  }
  if (actual === expected) return { ok: true, messages: data.warnings }
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'glossary-diff-'))
  try {
    fs.writeFileSync(path.join(dir, 'actual'), actual)
    fs.writeFileSync(path.join(dir, 'expected'), expected)
    const diff = spawnSync(
      'diff',
      [
        '-u',
        '--label',
        out,
        path.join(dir, 'actual'),
        '--label',
        `${out} (expected)`,
        path.join(dir, 'expected'),
      ],
      { encoding: 'utf8' },
    )
    return {
      ok: false,
      messages: [
        ...data.warnings,
        `${out} is stale:\n${diff.stdout}${diff.stderr}Run: make glossary-update`,
      ],
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    strict: true,
    options: {
      entities: { type: 'string', default: ENTITIES },
      out: { type: 'string', default: OUT },
      write: { type: 'boolean', default: false },
      check: { type: 'boolean', default: false },
    },
  })
  if (values.write === values.check) {
    console.error(
      'usage: node scripts/guards/glossary.mjs (--write | --check) [--entities <path>] [--out <path>]',
    )
    process.exit(2)
  }
  const { ok, messages } = await glossary({
    entities: values.entities,
    out: values.out,
    mode: values.write ? 'write' : 'check',
  })
  for (const message of messages) console.error(message)
  process.exit(ok ? 0 : 1)
}
