import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { fencedLineMap } from '../markdown-prose.mjs'
import { planScratchBasename } from './plan-scratch-paths.mjs'

export const DRAFT_TIERS = {
  light: {
    recon: 'Read the named files and their direct tests.',
    testRuns: 0,
    planBytes: 8192,
  },
  standard: {
    recon:
      'Read the named files, direct tests, touched module conventions and one consumer per changed contract.',
    testRuns: 1,
    planBytes: 24576,
  },
  full: {
    recon: 'Full recon; use the epic path for an epic.',
    testRuns: null,
    planBytes: null,
  },
}

/** @param {number} estimate @returns {keyof typeof DRAFT_TIERS} */
export function tierForEstimate(estimate) {
  if (!Number.isInteger(estimate) || estimate < 0) throw new Error('invalid estimate')
  return estimate <= 1 ? 'light' : estimate <= 3 ? 'standard' : 'full'
}

/** @param {string} text */
export function planBodyBytes(text) {
  const lines = text.split('\n')
  const literal = fencedLineMap(lines.map((line) => line.replace(/\r$/, '')))
  let offset = 0
  for (const [index, line] of lines.entries()) {
    if (!literal[index] && /^##[ \t]+Original notes[ \t]*\r?$/i.test(line)) {
      return Buffer.byteLength(text.slice(0, offset))
    }
    offset += line.length + 1
  }
  return Buffer.byteLength(text)
}

/** @typedef {{estimate: number, tier: keyof typeof DRAFT_TIERS, at: string}} TierSelection */
/** @typedef {{estimate: number, tier: keyof typeof DRAFT_TIERS, history: TierSelection[]}} TierRecord */

/** @param {string} file @returns {TierRecord | null} */
function readRecord(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch (error) {
    if (/** @type {NodeJS.ErrnoException} */ (error).code === 'ENOENT') return null
    throw error
  }
}

/** @param {string[]} args */
export function runCli(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    strict: true,
    options: {
      'run-scratch': { type: 'string' },
      issue: { type: 'string' },
      estimate: { type: 'string' },
      plan: { type: 'string' },
    },
  })
  const [cmd] = positionals
  if (
    positionals.length !== 1 ||
    !values['run-scratch'] ||
    !values.issue ||
    values.estimate === undefined
  ) {
    throw new Error(
      'usage: select|check --run-scratch <dir> --issue <ID> --estimate <n> [--plan <path>]',
    )
  }
  const estimate = Number(values.estimate)
  const tier = tierForEstimate(estimate)
  const scratch = resolve(values['run-scratch'])
  const file = join(scratch, planScratchBasename('draft-tier', { issueId: values.issue }))
  if (cmd === 'select') {
    const previous = readRecord(file)
    const history = [...(previous?.history ?? []), { estimate, tier, at: new Date().toISOString() }]
    const temporary = `${file}.${process.pid}.tmp`
    mkdirSync(scratch, { recursive: true })
    try {
      writeFileSync(temporary, JSON.stringify({ estimate, tier, history }) + '\n', { flag: 'wx' })
      renameSync(temporary, file)
    } finally {
      rmSync(temporary, { force: true })
    }
    process.stdout.write(
      JSON.stringify({
        tier,
        ...DRAFT_TIERS[tier],
        testExecution: 'Wrap any targeted test run in beat.',
      }) + '\n',
    )
  } else if (cmd === 'check') {
    if (!values.plan) throw new Error('check requires --plan')
    const record = readRecord(file)
    if (!record) throw new Error('draft-tier-missing')
    if (record.tier !== tier) throw new Error('draft-tier-stale')
    const bytes = planBodyBytes(readFileSync(values.plan, 'utf8'))
    const ceiling = DRAFT_TIERS[tier].planBytes
    if (ceiling !== null && bytes > ceiling) {
      throw new Error(`plan-over-tier-budget: ${bytes} bytes exceeds ${ceiling}`)
    }
  } else {
    throw new Error('unknown draft-tier command')
  }
}

if (isMainModule(import.meta.url)) {
  try {
    runCli(process.argv.slice(2))
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 1
  }
}
