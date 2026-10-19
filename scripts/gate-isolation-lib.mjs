import path from 'node:path'
import { readGateLog } from './gate-log-lib.mjs'

const stripAnsi = (text) => text.replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '')
const targetKey = (target) => JSON.stringify([target.kind, target.label, target.pkg, target.file])

export function extractFailedTargets(logText, { cwd = process.cwd() } = {}) {
  const text = stripAnsi(String(logText ?? ''))
  const lines = text.split('\n')
  const targets = new Map()
  const unattributed = new Set()
  const namesByLabel = new Map()
  let outputLabel = null
  let tapFailure = false
  const add = (target) => targets.set(targetKey(target), target)
  const nodeFile = (value, knownLocation = false) => {
    const file = value.trim().replace(/^file:\/\//, '')
    if (knownLocation || /\.(?:[cm]?js|[jt]sx?|[cm]ts)$/.test(file)) {
      add({ kind: 'node-test', file: path.resolve(cwd, file) })
    }
  }

  for (const line of lines) {
    const output = /^=+ Test output for (\/\/\S+?):\s*$/.exec(line)
    if (output) {
      outputLabel = output[1]
      if (!namesByLabel.has(outputLabel)) namesByLabel.set(outputLabel, new Set())
    } else if (/^={5,}\s*$/.test(line)) {
      outputLabel = null
    }
    const name = /^\s*--- FAIL:\s+(\S+)/.exec(line)
    if (outputLabel && name) namesByLabel.get(outputLabel).add(name[1].split('/')[0])

    const vitest = /^\s*([^\s:]+):test:\s+FAIL\s+(.*?)\s+>/.exec(line)
    if (vitest) add({ kind: 'vitest', pkg: vitest[1], file: vitest[2].trim() })

    const notOk = /^\s*not ok\s+\d+\s*-\s*(.*)/.exec(line)
    if (notOk) {
      tapFailure = true
      nodeFile(notOk[1])
    } else if (/^\s*(?:ok\s+\d+|# (?:tests|pass|fail)\b)/.test(line)) {
      tapFailure = false
    }
    const location = /^\s*location:\s*['"]?(.*?):\d+:\d+['"]?\s*$/.exec(line)
    if (tapFailure && location) nodeFile(location[1], true)
    const spec = /^\s*test at (.*?):\d+:\d+\s*$/.exec(line)
    if (spec) nodeFile(spec[1], true)

    if (
      /^\s*\/\/\S+\s+(?:FAILED TO BUILD|NO STATUS)\b/.test(line) ||
      /^\s*ERROR:/.test(line) ||
      /\[(?:build|setup) failed\]/.test(line) ||
      (!outputLabel && /^FAIL[\t ]+\S/.test(line)) ||
      /^\s*[^\s:]+:(?!test:)[^\s:]+:\s*ERROR:.*(?:command|exit|failed)/.test(line) ||
      /^\s*Failed:\s+\S+#(?!test\b)\S+/.test(line)
    )
      unattributed.add(line.trim())
  }
  for (const line of lines) {
    const failed = /^\s*(\/\/\S+)\s+(?:FAILED in|TIMEOUT in)\b/.exec(line)
    if (failed)
      add({ kind: 'bazel', label: failed[1], names: [...(namesByLabel.get(failed[1]) ?? [])] })
  }
  for (const failure of readGateLog(text).postSummaryFailures) {
    unattributed.add(`post-summary failure: ${failure}`)
  }
  return { targets: [...targets.values()], unattributed: [...unattributed] }
}

export function isolationCommandFor(target, env = process.env) {
  switch (target.kind) {
    case 'bazel': {
      const names = target.names ?? []
      const escaped = names.map((name) => name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
      return [
        env.BAZEL || 'bazel',
        'test',
        '--test_output=errors',
        '--nocache_test_results',
        ...(env.RACE === '1' ? ['--config=race'] : []),
        ...(escaped.length ? [`--test_filter=^(${escaped.join('|')})$`] : []),
        target.label,
      ]
    }
    case 'vitest':
      return ['pnpm', '--filter', target.pkg, 'exec', 'vitest', 'run', target.file]
    case 'node-test':
      return [process.execPath, '--test', target.file]
    default:
      throw new Error(`Unsupported isolation target: ${target.kind}`)
  }
}

export function decideIsolationVerdict({
  targets = [],
  unattributed = [],
  results = [],
  budgetExhausted = false,
  maxTargets = 6,
}) {
  if (targets.length > maxTargets) return 'skipped-cap'
  if (budgetExhausted) return 'skipped-budget'
  if (unattributed.length) return 'unattributed'
  if (!targets.length) return 'none-found'
  if (results.some((result) => result.exit !== 0 || result.timedOut)) return 'reproduced'
  const completed = new Set(results.map((result) => targetKey(result.target)))
  if (
    results.length !== targets.length ||
    completed.size !== targets.length ||
    targets.some((target) => !completed.has(targetKey(target)))
  )
    return 'partial'
  return 'flaky-under-load'
}

function describeTarget(target) {
  if (target.kind === 'bazel')
    return `${target.label}${target.names?.length ? ` (${target.names.join(', ')})` : ''}`
  if (target.kind === 'vitest') return `${target.pkg} ${target.file}`
  return target.file
}

export function renderIsolationLines(record) {
  if (!record)
    return 'isolation: not attempted - isolation evidence unavailable (0 targets, 0 passed)'
  const { targets = [], results = [], unattributed = [], decision, maxTargets = 6 } = record
  const passed = results.filter((result) => result.exit === 0).length
  const counts = `${passed} of ${targets.length} failed targets passed alone`
  const details = results
    .map((result) => `${describeTarget(result.target)} ${(result.durationMs / 1000).toFixed(1)}s`)
    .join(', ')
  const reasons = {
    'flaky-under-load': '',
    reproduced: 'reproduced alone',
    partial: 'not all failed targets were re-run',
    unattributed: `${unattributed.length} unattributed failures`,
    'none-found': 'no re-runnable failure target in the log',
    'skipped-cap': `not attempted - ${targets.length} targets exceed the cap of ${maxTargets}`,
    'skipped-budget': 'not completed - isolation budget exhausted',
    disabled: 'not attempted - disabled by BOSS_GATE_ISOLATE=0',
  }
  return `isolation: ${counts}${reasons[decision] ? ` - ${reasons[decision]}` : ''}${details ? ` - ${details}` : ''}`
}

export function renderLoadLine(load) {
  if (!load || !Number.isFinite(load.cpus) || load.cpus <= 0 || !validPhase(load.gate))
    return 'load: unavailable'
  const ratio = (peak) =>
    `${(peak / load.cpus).toFixed(2)}x ${peak / load.cpus >= 1 ? 'heavy' : 'normal'}`
  const gate = load.gate
  let line = `load: gate peak ${gate.peak.toFixed(1)} / ${load.cpus} CPUs (${ratio(gate.peak)}), mean ${gate.mean.toFixed(1)} over ${gate.samples} samples`
  if (validPhase(load.isolation))
    line += ` - isolation peak ${load.isolation.peak.toFixed(1)} (${ratio(load.isolation.peak)})`
  return line
}

function validPhase(phase) {
  return (
    phase &&
    Number.isFinite(phase.peak) &&
    Number.isFinite(phase.mean) &&
    Number.isInteger(phase.samples) &&
    phase.samples > 0
  )
}
