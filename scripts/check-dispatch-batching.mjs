#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { auditBatching, collectDispatches } from '../skills-toolbox/bs-dispatch-batch-audit.mjs'
import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { AUDIT_PATH, auditRows } from './check-dispatch-await-citations.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const NONE = '—'

function normalizePattern(value) {
  return String(value ?? '')
    .trim()
    .replace(/^`|`$/g, '')
}

function declaredStem(pattern) {
  return normalizePattern(pattern).split('<')[0]
}

function fixtureEntry(requestId, outPath) {
  return JSON.stringify({
    type: 'assistant',
    requestId,
    message: {
      content: [
        {
          type: 'tool_use',
          name: 'Agent',
          input: { prompt: `write ${outPath}`, description: outPath },
        },
      ],
    },
  })
}

function fixtureVerdicts() {
  const phaseRules = [{ id: 'ROUND', pattern: 'findings-round-' }]
  const cases = [
    {
      name: 'batched',
      want: 'batched',
      text: [
        fixtureEntry('req-1', 'findings-round-a.json'),
        fixtureEntry('req-1', 'findings-round-b.json'),
      ].join('\n'),
    },
    {
      name: 'serial',
      want: 'serial',
      text: [
        fixtureEntry('req-1', 'findings-round-a.json'),
        fixtureEntry('req-2', 'findings-round-b.json'),
      ].join('\n'),
    },
    {
      name: 'single',
      want: 'single',
      text: fixtureEntry('req-1', 'findings-round-a.json'),
    },
    {
      name: 'indeterminate',
      want: 'indeterminate',
      text: JSON.stringify({
        type: 'assistant',
        message: {
          content: [
            {
              type: 'tool_use',
              name: 'Agent',
              input: { prompt: 'write findings-round-a.json' },
            },
          ],
        },
      }),
    },
  ]

  const failures = []
  for (const fixture of cases) {
    const verdict = auditBatching(collectDispatches(fixture.text), { phaseRules }).phases[0].verdict
    if (verdict !== fixture.want) {
      failures.push(`fixture ${fixture.name}: expected ${fixture.want}, got ${verdict}`)
    }
  }
  return failures
}

export function checkDispatchBatching({ root = REPO_ROOT } = {}) {
  const markdown = fs.readFileSync(path.join(root, AUDIT_PATH), 'utf8')
  const rows = auditRows(markdown)
  const failures = []
  for (const row of rows) {
    const pattern = normalizePattern(row.parallelOutPath)
    if (row.verdict === 'independent') {
      if (!pattern || pattern === NONE) {
        failures.push(`${row.id}: independent row has blank parallel out-path`)
        continue
      }
      const stem = declaredStem(pattern)
      const skillPath = path.join(root, row.file)
      let prose
      try {
        prose = fs.readFileSync(skillPath, 'utf8')
      } catch {
        failures.push(`${row.id}: missing file ${row.file}`)
        continue
      }
      if (!prose.includes(stem)) {
        failures.push(`${row.id}: declared parallel out-path stem absent from ${row.file}: ${stem}`)
      }
      continue
    }
    if (pattern && pattern !== NONE) {
      failures.push(`${row.id}: non-independent row declares parallel out-path ${pattern}`)
    }
  }
  failures.push(...fixtureVerdicts())
  if (rows.length === 0) failures.push(`${AUDIT_PATH}: no audit rows parsed`)
  return { ok: failures.length === 0, failures, rows }
}

if (isMainModule(import.meta.url)) {
  const result = checkDispatchBatching()
  if (!result.ok) {
    process.stderr.write(result.failures.join('\n') + '\n')
    process.exit(1)
  }
  process.stdout.write(`Verified ${result.rows.length} dispatch batching audit rows\n`)
}
