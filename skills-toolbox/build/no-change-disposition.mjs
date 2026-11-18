#!/usr/bin/env node
// A NO_CHANGE run may close only scope independently certified on the clean base tree.
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { loadSkillConfig, stateName } from '../skill-config.mjs'
import { worktreeState } from '../worktree-state.mjs'
import { isFullGateCheck } from './acceptance-evidence.mjs'

/**
 * @typedef {{id?: string, check?: string, derivedCheck?: string|null, result?: string,
 * satisfiedBy?: string|null, retried?: boolean}} Criterion
 * @typedef {{head?: string, tree?: string, reused?: boolean, criteria?: Criterion[]}} Certification
 */

/**
 * @param {{claimVerdict: string, currentState: string, head: string|null, tree: string|null,
 * baseTree: string|null, worktreeClean: boolean, acceptance?: Certification|null}} inputs
 * @param {object} config
 */
export function decideNoChangeDisposition(inputs, config) {
  const { claimVerdict, currentState, head, tree, baseTree, worktreeClean, acceptance } = inputs
  const criteria = Array.isArray(acceptance?.criteria) ? acceptance.criteria : []
  const decision = (disposition, reason, targetState = null) => ({
    disposition,
    reason,
    targetState,
    criteria,
  })
  if (claimVerdict === 'LOST') return decision('leave', 'peer-owns')
  if (currentState !== stateName(config, 'inProgress')) return decision('leave', 'state-not-ours')
  if (claimVerdict !== 'WON') return decision('restore', 'claim-not-won')
  if (!acceptance || typeof acceptance !== 'object') return decision('restore', 'no-certification')
  if (
    !worktreeClean ||
    !head ||
    !tree ||
    acceptance.reused ||
    acceptance.head !== head ||
    acceptance.tree !== tree
  )
    return decision('restore', 'certification-stale')
  if (!baseTree || baseTree !== tree) return decision('restore', 'base-tree-moved')
  const command = (criterion) => criterion?.check || criterion?.derivedCheck
  if (
    !criteria.length ||
    criteria.some(
      (criterion) =>
        criterion?.result !== 'met' ||
        criterion.satisfiedBy !== null ||
        criterion.retried !== false ||
        typeof command(criterion) !== 'string' ||
        !command(criterion).trim(),
    )
  )
    return decision('restore', 'criterion-not-met')
  if (criteria.every((criterion) => isFullGateCheck(config, command(criterion))))
    return decision('restore', 'only-full-gate-criteria')
  return decision('close', 'already-satisfied', stateName(config, 'done'))
}

const USAGE =
  'usage: no-change-disposition.mjs decide --base <ref> --claim-verdict <WON|LOST|NO_WINNER|unknown> --current-state <name> [--acceptance <file>] [--repo <dir>]'

/** @param {string[]} argv */
export function runCli(argv) {
  let values
  try {
    const parsed = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        base: { type: 'string' },
        'claim-verdict': { type: 'string' },
        'current-state': { type: 'string' },
        acceptance: { type: 'string' },
        repo: { type: 'string' },
      },
    })
    values = parsed.values
    const [cmd] = parsed.positionals
    if (
      parsed.positionals.length !== 1 ||
      cmd !== 'decide' ||
      !values.base ||
      !values['current-state'] ||
      !['WON', 'LOST', 'NO_WINNER', 'unknown'].includes(values['claim-verdict'])
    )
      throw new Error(USAGE)
  } catch (err) {
    process.stderr.write(`${err.message}\n${USAGE}\n`)
    return 2
  }
  const repo = resolve(values.repo ?? process.cwd())
  const git = (...args) => {
    try {
      return execFileSync('git', ['-C', repo, ...args], {
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'pipe'],
        timeout: 10000,
      }).trim()
    } catch {
      return null
    }
  }
  let acceptance = null
  try {
    if (values.acceptance) acceptance = JSON.parse(readFileSync(values.acceptance, 'utf8'))
  } catch {}
  const config = loadSkillConfig({ cwd: repo })
  const result = decideNoChangeDisposition(
    {
      claimVerdict: values['claim-verdict'],
      currentState: values['current-state'],
      acceptance,
      head: git('rev-parse', '--verify', 'HEAD^{commit}'),
      tree: git('rev-parse', '--verify', 'HEAD^{tree}'),
      baseTree: git('rev-parse', '--verify', '--end-of-options', `${values.base}^{tree}`),
      worktreeClean:
        worktreeState({
          repo,
          excludes: ['.claude/scheduled_tasks.lock', '.claude/settings.local.json'],
        }).verdict === 'clean',
    },
    config,
  )
  process.stdout.write(`${JSON.stringify(result)}\n`)
  return 0
}

if (isMainModule(import.meta.url)) process.exit(runCli(process.argv.slice(2)))
