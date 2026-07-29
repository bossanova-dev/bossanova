#!/usr/bin/env node
import { spawnSync } from 'node:child_process'

const HELP = `Usage: account-rotation-smoke [--test] [--dry-run] [--verbose] [--help]

Runs local account-rotation registry checks through boss account commands.

Options:
  --test      Also run boss account test <account-id> --json for each account.
  --dry-run   Print the commands that would run, without invoking boss.
  --verbose   In --test mode, include provider detail strings returned by boss.
  --help      Show this help.

This script prints no secrets. It reads only non-secret fields from boss account
JSON output and never prints raw JSON, email, label, or credentials.

It runs local registry/list/test checks only and does not trigger rotation; no
runtime fake-rotation harness exists.
`

const NON_SECRET_ACCOUNT_FIELDS = [
  'id',
  'provider',
  'status',
  'priority',
  'health',
  'cooldown_until',
  'last_test_error',
]

function parseArgs(argv) {
  const opts = { test: false, dryRun: false, verbose: false, help: false }
  for (const arg of argv) {
    if (arg === '--test') opts.test = true
    else if (arg === '--dry-run') opts.dryRun = true
    else if (arg === '--verbose') opts.verbose = true
    else if (arg === '--help' || arg === '-h') opts.help = true
    else throw new Error(`unknown argument: ${arg}`)
  }
  return opts
}

function bossBin(env = process.env) {
  return env.BOSS_BIN || 'boss'
}

function runBoss(args, { env = process.env } = {}) {
  return spawnSync(bossBin(env), args, { encoding: 'utf8', env })
}

function pickAccount(raw) {
  const account = {}
  for (const field of NON_SECRET_ACCOUNT_FIELDS) account[field] = raw?.[field] ?? ''
  account.priority = Number.isFinite(Number(account.priority)) ? Number(account.priority) : 0
  return account
}

function parseAccountList(stdout) {
  const parsed = JSON.parse(stdout)
  if (!Array.isArray(parsed)) throw new Error('boss account ls --json did not return an array')
  return parsed.map(pickAccount)
}

function providerCounts(accounts) {
  const counts = new Map()
  for (const account of accounts) {
    const provider = account.provider || 'unknown'
    counts.set(provider, (counts.get(provider) || 0) + 1)
  }
  return [...counts.entries()].sort(([a], [b]) => a.localeCompare(b))
}

function printAccountSummary(accounts) {
  console.log('Account registry reachable.')
  console.log('Provider counts:')
  for (const [provider, count] of providerCounts(accounts)) console.log(`- ${provider}: ${count}`)
  console.log('Accounts:')
  for (const account of accounts) {
    const cooldown = account.cooldown_until || '-'
    console.log(
      `- ${account.id}: provider=${account.provider || '-'} status=${account.status || '-'} priority=${account.priority} health=${account.health || '-'} cooldown_until=${cooldown}`,
    )
  }
}

function parseTestResult(stdout) {
  const parsed = JSON.parse(stdout)
  return {
    account: pickAccount(parsed?.account || {}),
    liveSmokeRan: Boolean(parsed?.live_smoke_ran),
    detail: typeof parsed?.detail === 'string' ? parsed.detail : '',
  }
}

// isRotationEligible mirrors the rotation selector's predicate
// (services/bossd/internal/rotation/engine.go isSelectable + selectCandidate's
// cooldown check): an account can only rotate in when it is active, healthy, and
// not still cooling down. A passing credential test alone does not make a
// disabled or cooling account rotatable, so the smoke must apply the same gate
// before certifying `ok`.
function isRotationEligible(account, now) {
  if (account.status !== 'active') return false
  if (account.health !== 'ok') return false
  if (account.cooldown_until) {
    const until = Date.parse(account.cooldown_until)
    if (Number.isFinite(until) && until > now) return false
  }
  return true
}

function runAccountTests(accounts, opts, { env = process.env } = {}) {
  let failed = false
  console.log('Credential tests:')
  const now = Date.now()
  for (const account of accounts) {
    const result = runBoss(['account', 'test', account.id, '--json'], { env })
    if (result.error) {
      failed = true
      console.log(`- ${account.id}: error live_smoke_ran=false`)
      continue
    }
    let parsed
    try {
      parsed = parseTestResult(result.stdout)
    } catch {
      failed = true
      console.log(`- ${account.id}: error live_smoke_ran=false`)
      continue
    }
    // `ok` requires both a passing credential test and rotation eligibility:
    // account health is "ok" | "failed" (or empty when never tested), and a
    // stale/disabled/cooling account can still pass a fresh credential test yet
    // remain non-rotatable, so it must not be reported as ok.
    const status =
      result.status === 0 &&
      !parsed.account.last_test_error &&
      isRotationEligible(parsed.account, now)
        ? 'ok'
        : 'error'
    if (status !== 'ok') failed = true
    console.log(`- ${account.id}: ${status} live_smoke_ran=${parsed.liveSmokeRan}`)
    if (opts.verbose && parsed.detail) console.log(`  detail: ${parsed.detail}`)
  }
  return failed ? 1 : 0
}

function main(argv = process.argv.slice(2), { env = process.env } = {}) {
  let opts
  try {
    opts = parseArgs(argv)
  } catch (err) {
    console.error(err.message)
    console.error('Run with --help for usage.')
    return 2
  }

  if (opts.help) {
    console.log(HELP.trimEnd())
    return 0
  }

  const listArgs = ['account', 'ls', '--json']
  if (opts.dryRun) {
    console.log(`would run: ${bossBin(env)} ${listArgs.join(' ')}`)
    if (opts.test) console.log(`would run: ${bossBin(env)} account test <account-id> --json`)
    return 0
  }

  const list = spawnSync(bossBin(env), listArgs, { encoding: 'utf8', env })
  if (list.error || list.status !== 0) {
    console.error('registry not reachable (is the daemon running?)')
    return list.status || 1
  }

  let accounts
  try {
    accounts = parseAccountList(list.stdout)
  } catch (err) {
    console.error(`could not parse account registry: ${err.message}`)
    return 1
  }

  printAccountSummary(accounts)
  if (!opts.test) return 0
  return runAccountTests(accounts, opts, { env })
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invoked = isMainModule(import.meta.url)
if (invoked) process.exit(main())

export { main, parseAccountList, pickAccount }
