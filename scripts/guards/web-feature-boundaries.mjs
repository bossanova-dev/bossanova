#!/usr/bin/env node

import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { compareBaseline, formatBaseline, readBaseline } from './baseline.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const baselinePath = 'scripts/guards/web-layout-baseline.json'
const sourcePrefix = 'services/web/src/'
const declarations = new Set(['vite-env.d.ts', 'vitest.d.ts'])

function directories(root) {
  return fs.existsSync(root) ? fs.readdirSync(root, { withFileTypes: true }) : []
}

function owned(override) {
  return override.includes?.some((pattern) =>
    /^(src\/features\/|src\/shared\/|src\/app\/)/.test(pattern),
  )
}

function generatedOverrides(root, existing, baseRule) {
  const src = path.join(root, sourcePrefix)
  const features = directories(path.join(src, 'features'))
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort()
  const legacy = directories(src)
    .filter(
      (entry) =>
        !['app', 'features', 'shared', 'gen'].includes(entry.name) &&
        !declarations.has(entry.name) &&
        !/\.(test|spec)\./.test(entry.name) &&
        entry.name !== 'BUILD.bazel',
    )
    .flatMap((entry) =>
      entry.isDirectory()
        ? [`~/${entry.name}`, `~/${entry.name}/**`]
        : [`~/${entry.name.replace(/\.[^.]+$/, '')}`, `~/${entry.name}`],
    )
    .sort()
  // Overrides replace this rule's options: carry the existing production restrictions into
  // every generated rule, including the Playwright restriction added by the e2e guard.
  const prior = [
    baseRule,
    ...existing
      .filter((entry) => entry.includes?.includes('**'))
      .map((entry) => entry.linter?.rules?.style?.noRestrictedImports),
  ].filter((rule) => rule && typeof rule === 'object')
  const paths = Object.assign({}, ...prior.map((rule) => rule.options?.paths ?? {}))
  const patterns = prior.flatMap((rule) => rule.options?.patterns ?? [])
  const restriction = (includes, groups) => ({
    includes: [includes],
    linter: {
      rules: {
        style: {
          noRestrictedImports: {
            level: 'error',
            options: {
              paths,
              patterns: [
                ...patterns,
                ...groups.map((group) => ({
                  group,
                  message:
                    'Use feature public indexes and the ~/ alias; shared code cannot depend on features or app.',
                })),
              ],
            },
          },
        },
      },
    },
  })
  return [
    ...features.map((feature) =>
      restriction(`src/features/${feature}/**`, [
        ['~/features/*/**', `!~/features/${feature}/**`],
        ['~/app', '~/app/**'],
        ['../**'],
      ]),
    ),
    restriction('src/shared/**', [
      ['~/features', '~/features/**'],
      ['~/app', '~/app/**'],
      ['../**'],
      ...(legacy.length ? [legacy] : []),
    ]),
    restriction('src/app/**', [['~/features/*/**'], ['../**']]),
    {
      includes: ['src/features/*/index.ts'],
      linter: { rules: { performance: { noBarrelFile: 'off', noReExportAll: 'off' } } },
    },
  ]
}

// guard-id: web-feature-overrides-fresh
export function overrides({
  root = repoRoot,
  write = false,
  biome = path.join(root, 'services/web/node_modules/.bin/biome'),
} = {}) {
  const configPath = path.join(root, 'services/web/biome.json')
  const original = fs.readFileSync(configPath, 'utf8')
  const config = JSON.parse(original)
  const retained = (config.overrides ?? []).filter((entry) => !owned(entry))
  config.overrides = [...retained, ...generatedOverrides(root, retained)]
  const formatted = spawnSync(biome, ['format', '--stdin-file-path=biome.json'], {
    cwd: path.join(root, 'services/web'),
    input: JSON.stringify(config),
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024,
  })
  if (formatted.error || formatted.status !== 0)
    return [`Biome formatter failed: ${formatted.error?.message ?? formatted.stderr}`]
  try {
    if (JSON.stringify(JSON.parse(formatted.stdout)) !== JSON.stringify(config))
      throw new Error('formatter changed configuration values')
  } catch (error) {
    return [`Biome formatter returned invalid configuration: ${error.message}`]
  }
  if (write) {
    fs.writeFileSync(configPath, formatted.stdout)
    return []
  }
  return original === formatted.stdout
    ? []
    : [
        'Boundary overrides are stale: run node scripts/guards/web-feature-boundaries.mjs overrides --write',
      ]
}

// guard-id: web-feature-boundaries
export function boundaries({
  root = repoRoot,
  biome = path.join(root, 'services/web/node_modules/.bin/biome'),
} = {}) {
  const failures = overrides({ root, biome })
  if (failures.length) return failures
  const result = spawnSync(
    biome,
    [
      'lint',
      '--only=style/noRestrictedImports',
      '--reporter=json',
      '--max-diagnostics=none',
      'src',
    ],
    { cwd: path.join(root, 'services/web'), encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
  )
  if (result.error) return [`Biome could not run: ${result.error.message}`]
  let report
  try {
    report = JSON.parse(result.stdout)
    if (!Array.isArray(report.diagnostics)) throw new Error('missing diagnostics array')
  } catch {
    return [`Biome produced no readable report: ${result.stderr}`]
  }
  for (const diagnostic of report.diagnostics ?? []) {
    failures.push(
      `${diagnostic.location?.path ?? 'Biome'}: ${diagnostic.category}: ${diagnostic.message}`,
    )
  }
  if (result.status !== 0 && !failures.length)
    failures.push(`Biome failed (${result.status ?? result.signal}): ${result.stderr}`)
  return failures
}

export function legacyFiles(root = repoRoot) {
  return execFileSync('git', ['-C', root, 'ls-files', '-z', '--', sourcePrefix], {
    encoding: 'utf8',
  })
    .split('\0')
    .filter(Boolean)
    .filter((name) => {
      const relative = name.slice(sourcePrefix.length)
      return (
        !/^(app\/|shared\/|gen\/|features\/[^/]+\/)/.test(relative) &&
        !declarations.has(relative) &&
        !/\.(test|spec)\.[^/]+$/.test(relative) &&
        path.basename(relative) !== 'BUILD.bazel'
      )
    })
    .sort()
}

// guard-id: web-layout
export function layout({ root = repoRoot, update = false } = {}) {
  const { entries, errors } = readBaseline(root, baselinePath)
  if (errors.length) return errors
  const offenders = legacyFiles(root)
  const result = compareBaseline({
    baselinePath,
    entries,
    offenders,
    remedy: 'put production web files in app/, features/<feature>/, shared/ or gen/',
  })
  if (update && !result.newViolations.length) {
    fs.writeFileSync(path.join(root, baselinePath), formatBaseline(offenders))
    return []
  }
  return result.failures
}

if (isMainModule(import.meta.url)) {
  try {
    const { values, positionals } = parseArgs({
      options: {
        check: { type: 'boolean' },
        write: { type: 'boolean' },
        update: { type: 'boolean' },
        root: { type: 'string' },
        biome: { type: 'string' },
      },
      strict: true,
      allowPositionals: true,
    })
    const [verb] = positionals
    if (
      positionals.length !== 1 ||
      !['overrides', 'boundaries', 'layout'].includes(verb) ||
      Number(Boolean(values.check)) +
        Number(Boolean(values.write)) +
        Number(Boolean(values.update)) !==
        1 ||
      (values.write && verb !== 'overrides') ||
      (values.update && verb !== 'layout')
    )
      throw new Error(
        'Usage: web-feature-boundaries.mjs overrides --check|--write | boundaries --check | layout --check|--update',
      )
    const failures =
      verb === 'overrides'
        ? overrides(values)
        : verb === 'boundaries'
          ? boundaries(values)
          : layout(values)
    if (failures.length) {
      console.error(failures.join('\n'))
      process.exitCode = 1
    } else console.log(`OK: web ${verb}`)
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}
