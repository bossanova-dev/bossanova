#!/usr/bin/env node

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { compareBaseline, readBaseline } from './baseline.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const baselinePath = 'scripts/guards/web-e2e-harness-imports-baseline.json'
const category = 'lint/style/noRestrictedImports'

function runBiome(biome, cwd) {
  const result = spawnSync(
    biome,
    ['lint', '--only=style/noRestrictedImports', '--reporter=json', '--max-diagnostics=none', '.'],
    { cwd, encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
  )
  if (result.error)
    return {
      failures: [`Biome could not run: ${result.error.message} — run pnpm install`],
      flagged: [],
    }
  try {
    const report = JSON.parse(result.stdout)
    if (!Array.isArray(report.diagnostics)) throw new Error('missing diagnostics array')
    const flagged = []
    const failures = []
    for (const entry of report.diagnostics) {
      if (entry.category === category) {
        if (typeof entry.location?.path !== 'string') throw new Error('missing diagnostic path')
        flagged.push(
          path.relative(cwd, path.resolve(cwd, entry.location.path)).split(path.sep).join('/'),
        )
      } else if (['error', 'fatal'].includes(entry.severity)) {
        failures.push(`${entry.location?.path ?? 'Biome'}: ${entry.category}: ${entry.message}`)
      }
    }
    if (result.status !== 0 && flagged.length === 0 && failures.length === 0) {
      failures.push(`Biome failed (${result.status ?? result.signal}): ${result.stderr.trim()}`)
    }
    return { failures, flagged }
  } catch (error) {
    return {
      failures: [`Biome produced no readable report: ${error.message}; ${result.stderr.trim()}`],
      flagged: [],
    }
  }
}

export function checkWebE2eHarnessImports({
  root = repoRoot,
  biome = path.join(root, 'services/web/node_modules/.bin/biome'),
} = {}) {
  const { entries, errors } = readBaseline(root, baselinePath)
  if (errors.length) return { failures: errors }
  const web = path.join(root, 'services/web')
  let config
  try {
    config = JSON.parse(fs.readFileSync(path.join(web, 'biome.json'), 'utf8'))
  } catch (error) {
    return { failures: [`Cannot read web configuration: ${error.message}`] }
  }
  const failures = []
  const restrictions = (config.overrides ?? []).filter(
    (o) => o.linter?.rules?.style?.noRestrictedImports,
  )
  const global = restrictions.filter((override) => override.includes?.includes('**'))
  const override = global[0]
  if (
    global.length !== 1 ||
    restrictions.some((entry) => {
      const rule = entry.linter.rules.style.noRestrictedImports
      return rule?.level !== 'error' || !rule?.options?.paths?.['@playwright/test']
    })
  ) {
    return {
      failures: [
        'services/web/biome.json: require one global error restriction for @playwright/test and retain it in every restricted override',
      ],
    }
  }
  const includes = [
    '**',
    '!e2e/**',
    '!playwright.config.ts',
    ...entries.map((entry) => `!${entry}`),
  ]
  if (JSON.stringify([...(override.includes ?? [])].sort()) !== JSON.stringify(includes.sort())) {
    failures.push(
      'services/web/biome.json: harness override includes must equal fixed exemptions and baseline negations',
    )
  }
  const real = runBiome(biome, web)
  failures.push(...real.failures)
  failures.push(
    ...compareBaseline({
      baselinePath,
      entries: [],
      offenders: real.flagged,
      remedy: 'move Playwright imports under e2e/',
    }).failures,
  )
  if (real.failures.length || entries.length === 0) return { failures }

  const fixture = fs.mkdtempSync(path.join(os.tmpdir(), 'web-e2e-harness-baseline-'))
  try {
    const bareConfig = structuredClone(config)
    const bareOverride = bareConfig.overrides.find(
      (o) => o.includes?.includes('**') && o.linter?.rules?.style?.noRestrictedImports,
    )
    bareOverride.includes = ['**', '!e2e/**', '!playwright.config.ts']
    fs.writeFileSync(path.join(fixture, 'biome.json'), JSON.stringify(bareConfig))
    fs.writeFileSync(path.join(fixture, 'baselineProbe.ts'), '')
    for (const entry of entries) {
      const source = path.join(web, entry)
      if (!fs.existsSync(source)) continue
      const target = path.join(fixture, entry)
      fs.mkdirSync(path.dirname(target), { recursive: true })
      fs.copyFileSync(source, target)
    }
    const proof = runBiome(biome, fixture)
    failures.push(...proof.failures)
    failures.push(
      ...compareBaseline({
        baselinePath,
        entries,
        offenders: proof.flagged,
        remedy: 'move Playwright imports under e2e/',
      }).failures,
    )
  } finally {
    fs.rmSync(fixture, { recursive: true, force: true, maxRetries: 5 })
  }
  return { failures }
}

if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, strict: true, allowPositionals: false })
  const { failures } = checkWebE2eHarnessImports()
  if (failures.length) {
    for (const failure of failures) console.error(failure)
    process.exitCode = 1
  } else {
    console.log('OK: web Playwright imports stay under e2e/ and the baseline is current')
  }
}
