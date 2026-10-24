#!/usr/bin/env node
// guard-id: sweep-schedule-fresh

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import * as prettier from 'prettier'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { expectedFires } from '../sweeps/cron-schedule.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const CONFIG = path.join(ROOT, '.boss-project.json')
const OUT = path.join(ROOT, 'docs/sweeps/schedule.md')
// Four complete weeks, including the first day of a month; independent of today's date.
const FROM = '2026-02-01T00:00:00Z'
const TO = '2026-03-01T00:00:00Z'
/** @typedef {{name: string, schedule?: string, factoryStage?: string, timezone?: string, agent?: string, prompt?: string, gateCommand?: string, zeroOutput?: boolean, enabled?: boolean}} Cron */

/** @param {string | undefined} schedule */
function cadence(schedule) {
  const count = expectedFires(schedule, FROM, TO)
  if (count === 1) return 'monthly'
  if (count === 4) return 'weekly'
  if (count === 28) return 'daily'
  if (count === 672) return 'hourly'
  if (count && count > 672 && 40320 % count === 0) return `every ${40320 / count} minutes`
  return 'custom'
}
/** @param {string | undefined} value */
const cell = (value) =>
  String(value ?? '—')
    .replaceAll('|', '&#124;')
    .replaceAll('\n', ' ')
/** @param {Cron[]} crons */
function render(crons) {
  const lines = [
    '---',
    'summary: Cron schedules generated from project configuration.',
    '---',
    '',
    '<!-- Generated from .boss-project.json; run `make sweep-schedule-update`. -->',
    '',
    '# Sweep schedule',
    '',
    'Factory stage defaults come from [factory_stages.go](../../lib/bossalib/projectconfig/factory_stages.go); deployment assignments are omitted.',
    '',
    '| Name | Cadence | Schedule | Time zone | Agent | Prompt | Gated | Zero-output | Enabled |',
    '| --- | --- | --- | --- | --- | --- | --- | --- | --- |',
  ]
  for (const cron of [...crons].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))) {
    const values = [
      cron.name,
      cron.factoryStage ? 'factory stage' : cadence(cron.schedule),
      cron.factoryStage ? `factory stage ${cron.factoryStage}` : cron.schedule,
      cron.timezone ?? 'daemon-local',
      cron.agent,
      cron.prompt,
      cron.gateCommand ? 'yes' : 'no',
      cron.zeroOutput ? 'yes' : 'no',
      cron.enabled === undefined ? '—' : cron.enabled ? 'yes' : 'no',
    ]
    lines.push(`| ${values.map(cell).join(' | ')} |`)
  }
  return lines.join('\n')
}
/** @param {{config?: string, out?: string, mode: 'write'|'check'}} options */
export async function sweepSchedule({ config = CONFIG, out = OUT, mode }) {
  const data = JSON.parse(fs.readFileSync(config, 'utf8'))
  const expected = await prettier.format(render(data.crons ?? []), {
    ...(await prettier.resolveConfig(OUT)),
    parser: 'markdown',
  })
  const actual = fs.existsSync(out) ? fs.readFileSync(out, 'utf8') : ''
  if (mode === 'write') {
    fs.mkdirSync(path.dirname(out), { recursive: true })
    if (actual !== expected) fs.writeFileSync(out, expected)
    return { ok: true, messages: [] }
  }
  if (actual === expected) return { ok: true, messages: [] }
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sweep-schedule-diff-'))
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
      messages: [`${out} is stale:\n${diff.stdout}${diff.stderr}Run: make sweep-schedule-update`],
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5 })
  }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    strict: true,
    options: {
      config: { type: 'string', default: CONFIG },
      out: { type: 'string', default: OUT },
      write: { type: 'boolean', default: false },
      check: { type: 'boolean', default: false },
    },
  })
  if (values.write === values.check) {
    console.error(
      'usage: node scripts/guards/sweep-schedule.mjs (--write | --check) [--config <path>] [--out <path>]',
    )
    process.exit(2)
  }
  const { ok, messages } = await sweepSchedule({
    config: values.config,
    out: values.out,
    mode: values.write ? 'write' : 'check',
  })
  for (const message of messages) console.error(message)
  process.exit(ok ? 0 : 1)
}
