#!/usr/bin/env node

import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { compareBaseline, readBaseline } from './guards/baseline.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const baselinePath = 'scripts/web-filenames-baseline.json'
const frameworkExceptions = new Set(['src/main.tsx', 'src/vite-env.d.ts'])

function runBiome(biome, cwd) {
  const result = spawnSync(
    biome,
    [
      'lint',
      '--only=style/useFilenamingConvention',
      '--reporter=json',
      '--max-diagnostics=none',
      '.',
    ],
    { cwd, encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
  )
  if (result.error)
    return {
      failures: [`Biome could not run: ${result.error.message} — run pnpm install`],
      flagged: [],
    }
  let report
  try {
    report = JSON.parse(result.stdout)
  } catch {
    return { failures: [`Biome produced no readable report: ${result.stderr.trim()}`], flagged: [] }
  }
  const diagnostics = report.diagnostics ?? []
  const flagged = diagnostics
    .filter((entry) => entry.category === 'lint/style/useFilenamingConvention')
    .map((entry) =>
      path.relative(cwd, path.resolve(cwd, entry.location.path)).split(path.sep).join('/'),
    )
  const failures = diagnostics
    .filter((entry) => ['error', 'fatal'].includes(entry.severity))
    .map((entry) => `${entry.location?.path ?? 'Biome'}: ${entry.category}: ${entry.message}`)
  if (result.status !== 0 && failures.length === 0) {
    failures.push(`Biome failed (${result.status ?? result.signal}): ${result.stderr.trim()}`)
  }
  return {
    failures,
    flagged,
    otherFailures: failures.filter(
      (failure) => !failure.includes(': lint/style/useFilenamingConvention:'),
    ),
  }
}

export function checkWebFilenames({
  root = repoRoot,
  biome = path.join(root, 'services/web/node_modules/.bin/biome'),
} = {}) {
  const { entries, errors } = readBaseline(root, baselinePath)
  const failures = [...errors]
  if (errors.length) return { failures }
  const web = path.join(root, 'services/web')
  let config
  let tracked
  try {
    config = JSON.parse(fs.readFileSync(path.join(web, 'biome.json'), 'utf8'))
    tracked = new Set(
      execFileSync('git', ['-C', root, 'ls-files', '-z', '--', 'services/web'], {
        encoding: 'utf8',
      })
        .split('\0')
        .filter(Boolean)
        .map((name) => name.slice('services/web/'.length)),
    )
  } catch (error) {
    return { failures: [`Cannot read web configuration or tracked files: ${error.message}`] }
  }
  const exceptions = (config.overrides ?? []).filter((override) => {
    const cases = override.linter?.rules?.style?.useFilenamingConvention?.options?.filenameCases
    return ['camelCase', 'kebab-case', 'PascalCase'].every((name) => cases?.includes(name))
  })
  if (
    !exceptions.some((override) => entries.every((entry) => override.includes?.includes(entry)))
  ) {
    failures.push('services/web/biome.json: no exceptions override contains every baseline entry')
  }
  for (const override of exceptions) {
    for (const name of override.includes ?? []) {
      if (!name.includes('*') && !frameworkExceptions.has(name) && !entries.includes(name)) {
        failures.push(`services/web/biome.json: exception ${name} is missing from the baseline`)
      }
    }
  }
  const real = runBiome(biome, web)
  failures.push(...real.failures)
  if (!fs.existsSync(biome)) return { failures }

  const fixture = fs.mkdtempSync(path.join(os.tmpdir(), 'web-filenames-baseline-'))
  try {
    const bareConfig = structuredClone(config)
    bareConfig.overrides = bareConfig.overrides
      .map((override) => ({
        ...override,
        includes: override.includes?.filter((name) => !entries.includes(name)),
      }))
      .filter((override) => override.includes?.length)
    fs.writeFileSync(path.join(fixture, 'biome.json'), JSON.stringify(bareConfig))
    // Keep the command non-vacuous even after the last grandfathered file is fixed.
    fs.writeFileSync(path.join(fixture, 'filenameProbe.ts'), '')
    for (const entry of entries) {
      if (!tracked.has(entry) || !fs.existsSync(path.join(web, entry))) continue
      fs.mkdirSync(path.dirname(path.join(fixture, entry)), { recursive: true })
      fs.writeFileSync(path.join(fixture, entry), '')
    }
    const proof = runBiome(biome, fixture)
    failures.push(...(proof.otherFailures ?? proof.failures))
    failures.push(
      ...compareBaseline({
        baselinePath,
        entries,
        offenders: proof.flagged,
        remedy: 'fix filename casing',
      }).failures,
    )
  } finally {
    fs.rmSync(fixture, { recursive: true, force: true, maxRetries: 5 })
  }
  return { failures }
}

if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, strict: true, allowPositionals: false })
  const { failures } = checkWebFilenames()
  if (failures.length) {
    for (const failure of failures) console.error(failure)
    process.exitCode = 1
  } else {
    console.log('OK: web filenames follow Biome conventions and the baseline is current')
  }
}
