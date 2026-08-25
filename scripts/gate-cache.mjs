#!/usr/bin/env node

import { spawnSync, execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import {
  branchAddsOrRenames,
  computeTreeHash,
  eligibleGate,
  expansionVersions,
  gateKey,
  makeStampDir,
  readStamp,
  recordStamp,
  resolveBaseCommit,
} from './gate-stamp-lib.mjs'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const HIT = 0
const MISS = 1
const FORCED_MISS = 2
const NOT_ELIGIBLE = 3
const DISABLED = 4

function parseArgs(argv) {
  const args = { mode: argv[2], site: '', command: '', baseRef: '', exec: [] }
  let i = 3
  for (; i < argv.length; i++) {
    const arg = argv[i]
    if (arg === '--') {
      args.exec = argv.slice(i + 1)
      break
    }
    if (arg === '--site') args.site = argv[++i]
    else if (arg === '--command') args.command = argv[++i]
    else if (arg === '--base-ref') args.baseRef = argv[++i]
    else throw new Error(`unknown argument: ${arg}`)
  }
  return args
}

export function loadConfig(repoRoot) {
  try {
    return JSON.parse(fs.readFileSync(path.join(repoRoot, '.boss-skills.json'), 'utf8'))
  } catch {
    return {}
  }
}

function repoRoot() {
  return execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim()
}

function defaultBaseRef(root) {
  return process.env.BASE_REF || `origin/${defaultBranch(root)}`
}

function defaultBranch(root) {
  try {
    return execFileSync(
      'gh',
      ['repo', 'view', '--json', 'defaultBranchRef', '-q', '.defaultBranchRef.name'],
      {
        cwd: root,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'ignore'],
      },
    ).trim()
  } catch {
    return 'main'
  }
}

function stampDir() {
  return (
    process.env.BOSS_GATE_STAMP_DIR || path.join(os.homedir(), '.cache', 'bossanova-gate-stamps')
  )
}

function configuredUncachedCommand(config) {
  const command = String(config?.commands?.testUncached || '').trim()
  return command || ''
}

function evaluate(args) {
  const root = repoRoot()
  const config = loadConfig(root)
  const eligibility = eligibleGate(config, args.site || args.command)
  if (!eligibility.eligible) {
    return { status: 'not-eligible', code: NOT_ELIGIBLE, eligibility }
  }

  const baseRef = args.baseRef || defaultBaseRef(root)
  let treeHash
  let baseCommit
  let forcedUncached
  try {
    treeHash = computeTreeHash(root)
    baseCommit = resolveBaseCommit(root, baseRef)
    forcedUncached = branchAddsOrRenames(root, baseRef)
  } catch (err) {
    return {
      status: 'disabled',
      code: DISABLED,
      reason: `git input unavailable: ${err.message}`,
      eligibility,
    }
  }

  const key = gateKey({
    treeHash,
    command: args.command,
    baseCommit,
    versions: expansionVersions(process.env),
  })
  const stamps = makeStampDir(stampDir())
  if (!stamps.ok) {
    return {
      status: 'disabled',
      code: DISABLED,
      reason: 'stamp directory unavailable',
      treeHash,
      key,
      eligibility,
    }
  }

  const existing = readStamp(stamps.dir, key)
  if (existing.hit) {
    return {
      status: 'hit',
      code: HIT,
      treeHash,
      treeShort: treeHash.slice(0, 12),
      key,
      stamp: existing.stamp,
      eligibility,
    }
  }

  const uncachedCommand = configuredUncachedCommand(config)
  if (forcedUncached && !uncachedCommand) {
    return {
      status: 'disabled',
      code: DISABLED,
      reason: 'commands.testUncached unavailable',
      treeHash,
      treeShort: treeHash.slice(0, 12),
      key,
      eligibility,
    }
  }

  return {
    status: forcedUncached ? 'forced-miss' : 'miss',
    code: forcedUncached ? FORCED_MISS : MISS,
    treeHash,
    treeShort: treeHash.slice(0, 12),
    key,
    forcedUncached,
    uncachedCommand,
    stampDir: stamps.dir,
    eligibility,
    corrupt: existing.corrupt,
  }
}

function printVerdict(verdict) {
  if (verdict.status === 'hit') {
    console.log(`gate ${verdict.eligibility.site}: cached at tree ${verdict.treeShort}`)
  } else if (verdict.status === 'forced-miss') {
    console.log(
      `gate ${verdict.eligibility.site}: fresh at tree ${verdict.treeShort} (forced uncached: added or renamed input)`,
    )
  } else if (verdict.status === 'miss') {
    console.log(`gate ${verdict.eligibility.site}: fresh at tree ${verdict.treeShort}`)
  } else if (verdict.status === 'not-eligible') {
    console.log(
      `gate ${verdict.eligibility.site}: fresh (not cacheable: ${verdict.eligibility.reason})`,
    )
  } else {
    console.log(
      `gate ${verdict.eligibility?.site || 'unknown'}: fresh (cache disabled: ${verdict.reason})`,
    )
  }
}

function run(args) {
  const verdict = evaluate(args)
  printVerdict(verdict)
  if (verdict.status === 'hit') return 0

  const env = { ...process.env }
  if (verdict.status === 'forced-miss') {
    env.BOSS_GATE_FORCE_UNCACHED = '1'
    env.BOSS_GATE_UNCACHED_COMMAND_ACTIVE = '1'
    if (!env.GO_TEST_COUNT) env.GO_TEST_COUNT = '1'
  }
  const result = spawnSync(args.exec[0], args.exec.slice(1), {
    cwd: repoRoot(),
    env,
    stdio: 'inherit',
  })
  const status = result.status ?? 1
  if ((verdict.status === 'miss' || verdict.status === 'forced-miss') && status === 0) {
    recordStamp(verdict.stampDir, verdict.key, {
      exitStatus: 0,
      treeHash: verdict.treeHash,
      site: verdict.eligibility.site,
      command: args.command,
      forcedUncached: verdict.status === 'forced-miss',
    })
  }
  return status
}

function main() {
  const args = parseArgs(process.argv)
  if (args.mode === 'check') {
    const verdict = evaluate(args)
    printVerdict(verdict)
    process.exitCode = verdict.code
    return
  }
  if (args.mode === 'record') {
    const root = repoRoot()
    const config = loadConfig(root)
    const eligibility = eligibleGate(config, args.site || args.command)
    if (!eligibility.eligible) return
    if (process.env.GATE_EXIT_STATUS !== '0') return
    const baseRef = args.baseRef || defaultBaseRef(root)
    const treeHash = computeTreeHash(root)
    const key = gateKey({
      treeHash,
      command: args.command,
      baseCommit: resolveBaseCommit(root, baseRef),
      versions: expansionVersions(process.env),
    })
    const stamps = makeStampDir(stampDir())
    if (stamps.ok) {
      recordStamp(stamps.dir, key, {
        exitStatus: 0,
        treeHash,
        site: eligibility.site,
        command: args.command,
      })
    }
    return
  }
  if (args.mode === 'run') {
    if (args.exec.length === 0) throw new Error('run mode requires a command after --')
    process.exitCode = run(args)
    return
  }
  throw new Error(`unknown mode: ${args.mode}`)
}

if (isMainModule(import.meta.url)) {
  main()
}
