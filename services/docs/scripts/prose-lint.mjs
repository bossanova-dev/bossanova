#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../../../skills-toolbox/main-module.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const CONFIG = path.join(ROOT, '.vale.ini')
const BASELINE = path.join(ROOT, 'vale-baseline.json')

function findInToolCache(root, minimum) {
  const versionDirectory = path.join(root, 'vale', minimum)
  if (!fs.existsSync(versionDirectory)) return null
  for (const platform of fs.readdirSync(versionDirectory)) {
    const candidate = path.join(
      versionDirectory,
      platform,
      process.platform === 'win32' ? 'vale.exe' : 'vale',
    )
    if (fs.existsSync(candidate)) return candidate
  }
  return null
}

function minimumVersion() {
  const match = fs
    .readFileSync(CONFIG, 'utf8')
    .match(/^# Vale (\d+\.\d+\.\d+) or newer is required/m)
  if (!match) throw new Error('.vale.ini must declare `# Vale X.Y.Z or newer is required`')
  return match[1]
}

function versionParts(version) {
  return version.split('.').map(Number)
}

export function versionAtLeast(actual, minimum) {
  const left = versionParts(actual)
  const right = versionParts(minimum)
  for (let index = 0; index < right.length; index += 1) {
    if (left[index] > right[index]) return true
    if (left[index] < right[index]) return false
  }
  return true
}

export function valeBinary(minimum = minimumVersion()) {
  if (process.env.VALE_BIN) return process.env.VALE_BIN
  if (process.env.RUNNER_TOOL_CACHE) {
    const cached = findInToolCache(process.env.RUNNER_TOOL_CACHE, minimum)
    if (cached) return cached
  }
  return 'vale'
}

export function runVale(binary, args) {
  try {
    return execFileSync(binary, args, { cwd: ROOT, encoding: 'utf8' })
  } catch (error) {
    if (error.status === 1 && error.stdout) return error.stdout
    throw error
  }
}

export function alertCounts(report) {
  return Object.fromEntries(
    Object.entries(report)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([file, alerts]) => {
        const counts = {}
        for (const alert of alerts) counts[alert.Check] = (counts[alert.Check] ?? 0) + 1
        return [file.replaceAll('\\', '/'), Object.fromEntries(Object.entries(counts).sort())]
      }),
  )
}

export function compareAlertCounts(actual, baseline) {
  const failures = []
  for (const [file, rules] of Object.entries(actual)) {
    for (const [rule, count] of Object.entries(rules)) {
      const allowed = baseline[file]?.[rule] ?? 0
      if (count > allowed) failures.push({ file, rule, actual: count, baseline: allowed })
    }
  }
  return failures
}

function main() {
  const minimum = minimumVersion()
  const binary = valeBinary(minimum)
  const versionOutput = execFileSync(binary, ['--version'], { cwd: ROOT, encoding: 'utf8' })
  const actualVersion = versionOutput.match(/(\d+\.\d+\.\d+)/)?.[1]
  if (!actualVersion || !versionAtLeast(actualVersion, minimum)) {
    throw new Error(`Vale ${minimum}+ is required; found ${versionOutput.trim()}`)
  }

  const report = JSON.parse(runVale(binary, ['--config=.vale.ini', '--output=JSON', 'docs']))
  const actual = alertCounts(report)
  if (process.argv.includes('--write-baseline')) {
    fs.writeFileSync(BASELINE, `${JSON.stringify(actual, null, 2)}\n`)
    console.log(`Wrote vale-baseline.json with Vale ${actualVersion}`)
    return
  }

  let baseline
  try {
    baseline = JSON.parse(fs.readFileSync(BASELINE, 'utf8'))
  } catch (error) {
    if (error.code === 'ENOENT') throw new Error('vale-baseline.json is missing', { cause: error })
    throw error
  }
  const failures = compareAlertCounts(actual, baseline)
  if (failures.length > 0) {
    console.error('Vale prose ratchet failed:')
    for (const failure of failures) {
      console.error(
        `- ${failure.file}: ${failure.rule} ${failure.actual} exceeds ${failure.baseline}`,
      )
    }
    process.exitCode = 1
    return
  }
  console.log(
    `Vale prose OK (${Object.keys(actual).length} files with baseline alerts, Vale ${actualVersion})`,
  )
}

if (isMainModule(import.meta.url)) main()
