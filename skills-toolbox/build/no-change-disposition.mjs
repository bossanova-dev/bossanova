#!/usr/bin/env node
// A NO_CHANGE run may close only scope independently certified on the clean base tree;
// repeated or superseded nothing-built runs park with the configured needsHuman label instead of
// restoring to the build queue in a loop.
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { parseArgs } from 'node:util'
import { normalizeClaimComments } from '../linear-claim.mjs'
import { isMainModule } from '../main-module.mjs'
import { adapterFor, loadSkillConfig, optionalLabelName, stateName } from '../skill-config.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { worktreeState } from '../worktree-state.mjs'
import { isFullGateCheck } from './acceptance-evidence.mjs'

export const NO_CHANGE_BREADCRUMB_PREFIX = 'boss-build-no-change:'

const MARKDOWN_ESCAPE_RE = /\\([!"#$%&'()*+,\-./:;<=>?@[\\\]^_`{|}~])/g
const BREADCRUMB_LINE_RE = /^boss-build-no-change: (restore|park|close|leave) \(([A-Za-z0-9_-]+)\)$/
const SUPERSEDED_BY_RE = /^[A-Za-z0-9#][A-Za-z0-9._#/-]{0,63}$/

/**
 * @typedef {{id?: string, check?: string, derivedCheck?: string|null, result?: string,
 * satisfiedBy?: string|null, retried?: boolean}} Criterion
 * @typedef {{head?: string, tree?: string, reused?: boolean, criteria?: Criterion[]}} Certification
 * @typedef {{disposition: string, reason: string}} PriorBreadcrumb
 * @typedef {{applyIssueWrites?: (writes: {issueId: string, addLabels: string[]}) => Promise<{outcome?: string, reason?: string}>}} TrackerAdapterLike
 * @typedef {{config?: object, adapter?: TrackerAdapterLike|null, env?: Record<string, string|undefined>, cwd?: string}} CliDeps
 * @typedef {{code: number, out?: object, err?: string}} CliResult
 */

/**
 * Extract helper-rendered no-change breadcrumbs from tracker comments. Accepts a bare array, a
 * `{comments:[…]}` envelope, or nullish; unescapes markdown backslash escapes per line and matches
 * only whole lines.
 * @param {unknown} comments
 * @returns {PriorBreadcrumb[]}
 */
export function parsePriorBreadcrumbs(comments) {
  const breadcrumbs = []
  for (const entry of normalizeClaimComments(comments)) {
    const body = typeof entry?.body === 'string' ? entry.body : ''
    if (!body) continue
    for (const rawLine of body.split(/\r?\n/)) {
      const line = rawLine.replace(MARKDOWN_ESCAPE_RE, '$1')
      const match = BREADCRUMB_LINE_RE.exec(line)
      if (match) breadcrumbs.push({ disposition: match[1], reason: match[2] })
    }
  }
  return breadcrumbs
}

/**
 * @param {{claimVerdict: string, currentState: string, head: string|null, tree: string|null,
 * baseTree: string|null, worktreeClean: boolean, acceptance?: Certification|null,
 * supersededBy?: string|null, priorBreadcrumbs?: PriorBreadcrumb[]|null}} inputs
 * @param {object} config
 */
export function decideNoChangeDisposition(inputs, config) {
  const {
    claimVerdict,
    currentState,
    head,
    tree,
    baseTree,
    worktreeClean,
    acceptance,
    supersededBy = null,
    priorBreadcrumbs = null,
  } = inputs
  const criteria = Array.isArray(acceptance?.criteria) ? acceptance.criteria : []
  const decision = (disposition, reason, targetState = null, extra = {}) => ({
    disposition,
    reason,
    targetState,
    criteria,
    breadcrumb: `${NO_CHANGE_BREADCRUMB_PREFIX} ${disposition} (${reason})`,
    ...extra,
  })
  const restoreOrPark = (reason) => {
    const superseded =
      typeof supersededBy === 'string' && supersededBy.trim() !== '' ? supersededBy.trim() : null
    const prior = Array.isArray(priorBreadcrumbs)
      ? [...priorBreadcrumbs]
          .reverse()
          .find(
            (entry) =>
              (entry?.disposition === 'restore' || entry?.disposition === 'park') &&
              typeof entry?.reason === 'string' &&
              entry.reason !== '' &&
              entry.reason !== 'claim-not-won',
          )
      : undefined
    if (!superseded && !prior) return decision('restore', reason)
    const needsHuman = optionalLabelName(config, 'needsHuman')
    if (!needsHuman) {
      return decision('restore', reason, null, { parkSkipped: 'no-needs-human-label' })
    }
    if (superseded) {
      return decision('park', 'superseded', null, {
        label: needsHuman,
        supersededBy: superseded,
      })
    }
    return decision('park', 'repeat-no-change', null, {
      label: needsHuman,
      priorReason: prior.reason,
    })
  }

  if (claimVerdict === 'LOST') return decision('leave', 'peer-owns')
  if (currentState !== stateName(config, 'inProgress')) return decision('leave', 'state-not-ours')
  if (claimVerdict !== 'WON') return decision('restore', 'claim-not-won')
  if (!acceptance || typeof acceptance !== 'object') return restoreOrPark('no-certification')
  if (
    !worktreeClean ||
    !head ||
    !tree ||
    acceptance.reused ||
    acceptance.head !== head ||
    acceptance.tree !== tree
  )
    return restoreOrPark('certification-stale')
  if (!baseTree || baseTree !== tree) return restoreOrPark('base-tree-moved')
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
    return restoreOrPark('criterion-not-met')
  if (criteria.every((criterion) => isFullGateCheck(config, command(criterion))))
    return restoreOrPark('only-full-gate-criteria')
  return decision('close', 'already-satisfied', stateName(config, 'done'))
}

const USAGE = [
  'usage: no-change-disposition.mjs decide --base <ref> --claim-verdict <WON|LOST|NO_WINNER|unknown> --current-state <name> [--acceptance <file>] [--comments <file>] [--superseded-by <id>] [--repo <dir>]',
  '       no-change-disposition.mjs park --issue-id <id> [--repo <dir>]',
].join('\n')

/**
 * @param {string} message
 * @returns {CliResult}
 */
function usageError(message) {
  return {
    code: 2,
    err: message === USAGE ? `${USAGE}\n` : `${message}\n${USAGE}\n`,
  }
}

/**
 * @param {string[]} args
 * @param {CliDeps} deps
 * @returns {CliResult}
 */
function runDecide(args, deps) {
  let values
  try {
    const parsed = parseArgs({
      args,
      strict: true,
      allowPositionals: false,
      options: {
        base: { type: 'string' },
        'claim-verdict': { type: 'string' },
        'current-state': { type: 'string' },
        acceptance: { type: 'string' },
        comments: { type: 'string' },
        'superseded-by': { type: 'string' },
        repo: { type: 'string' },
      },
    })
    values = parsed.values
    if (
      !values.base ||
      !values['current-state'] ||
      !['WON', 'LOST', 'NO_WINNER', 'unknown'].includes(values['claim-verdict']) ||
      (values['superseded-by'] !== undefined && !SUPERSEDED_BY_RE.test(values['superseded-by']))
    )
      throw new Error(USAGE)
  } catch (err) {
    return usageError(err instanceof Error ? err.message : String(err))
  }
  const repo = resolve(values.repo ?? deps.cwd ?? process.cwd())
  const git = (...gitArgs) => {
    try {
      return execFileSync('git', ['-C', repo, ...gitArgs], {
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
  let priorBreadcrumbs = []
  try {
    if (values.comments) {
      priorBreadcrumbs = parsePriorBreadcrumbs(JSON.parse(readFileSync(values.comments, 'utf8')))
    }
  } catch {}
  const config = deps.config ?? loadSkillConfig({ cwd: repo })
  const result = decideNoChangeDisposition(
    {
      claimVerdict: values['claim-verdict'],
      currentState: values['current-state'],
      acceptance,
      priorBreadcrumbs,
      supersededBy: values['superseded-by'] ?? null,
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
  return { code: 0, out: result }
}

/**
 * @param {string[]} args
 * @param {CliDeps} deps
 * @returns {Promise<CliResult>}
 */
async function runPark(args, deps) {
  let values
  try {
    const parsed = parseArgs({
      args,
      strict: true,
      allowPositionals: false,
      options: {
        'issue-id': { type: 'string' },
        repo: { type: 'string' },
      },
    })
    values = parsed.values
    if (typeof values['issue-id'] !== 'string' || values['issue-id'].trim() === '') {
      throw new Error(USAGE)
    }
  } catch (err) {
    return usageError(err instanceof Error ? err.message : String(err))
  }
  const issueId = values['issue-id'].trim()
  const repo = resolve(values.repo ?? deps.cwd ?? process.cwd())
  const env = deps.env ?? process.env
  let config
  try {
    config = deps.config ?? loadSkillConfig({ cwd: repo })
  } catch (err) {
    return {
      code: 69,
      out: { issueId, reason: err instanceof Error ? err.message : String(err) },
    }
  }
  const label = optionalLabelName(config, 'needsHuman')
  if (!label) {
    return {
      code: 69,
      out: { issueId, reason: 'no needsHuman label is configured for the tracker' },
    }
  }
  let adapter = deps.adapter
  if (adapter === undefined) {
    try {
      const tracker = env.TRACKER ?? adapterFor(config, 'tracker')
      adapter = resolveTrackerAdapter({ env: tracker ? { ...env, TRACKER: tracker } : env })
    } catch {
      adapter = null
    }
  }
  if (typeof adapter?.applyIssueWrites !== 'function') {
    return {
      code: 69,
      out: {
        issueId,
        label,
        reason: 'the tracker adapter has no applyIssueWrites capability',
      },
    }
  }
  let result
  try {
    result = await adapter.applyIssueWrites({ issueId, addLabels: [label] })
  } catch (err) {
    return {
      code: 69,
      out: {
        issueId,
        label,
        reason: err instanceof Error ? err.message : String(err),
      },
    }
  }
  const outcome = result?.outcome ?? 'failed'
  if (outcome === 'ok') return { code: 0, out: { issueId, label, outcome } }
  return {
    code: 69,
    out: {
      issueId,
      label,
      outcome,
      reason: result?.reason ?? `label writes ${outcome}`,
    },
  }
}

/**
 * @param {string[]} argv
 * @param {CliDeps} [deps]
 * @returns {Promise<CliResult>}
 */
export async function runCli(argv, deps = {}) {
  const [cmd, ...rest] = argv
  if (cmd === 'decide') return runDecide(rest, deps)
  if (cmd === 'park') return runPark(rest, deps)
  return usageError(cmd ? `unknown command: ${cmd}` : USAGE)
}

if (isMainModule(import.meta.url)) {
  const { code, out, err } = await runCli(process.argv.slice(2))
  if (err) process.stderr.write(err)
  if (out !== undefined) process.stdout.write(`${JSON.stringify(out)}\n`)
  process.exit(code)
}
