#!/usr/bin/env node
// Git owns release points; extensions own release operations. The runner is the only I/O seam.
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { discoverExtensions, validateResult } from './skill-extensions.mjs'
import { classifyChecks } from './pr-check-state.mjs'
import { isMainModule } from './main-module.mjs'

export function defaultRun(command, args, { cwd }) {
  return execFileSync(command, args, {
    cwd,
    encoding: 'utf8',
    timeout: 30000,
    maxBuffer: 16 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim()
}

function releaseRepository({ root = process.cwd(), run = defaultRun } = {}) {
  root = path.resolve(root)
  const git = (...args) => run('git', args, { cwd: root }).trim()
  const gh = (...args) => JSON.parse(run('gh', args, { cwd: root }))
  const found = discoverExtensions({ core: 'boss-release', role: 'release', root })
  const descriptor = found.extensions[0] ?? null
  const environments = descriptor?.environments ?? ['production']
  const state = descriptor?.state ?? 'tag'
  const tagPrefix = descriptor?.tagPrefix ?? 'boss-release/'
  const warnings = found.skipped.filter((x) => !x.deliberate).map((x) => `${x.name}: ${x.reason}`)
  if (found.extensions.length > 1)
    warnings.push(
      `using ${descriptor.name}; ignoring ${found.extensions
        .slice(1)
        .map((x) => x.name)
        .join(', ')}`,
    )
  // An explicit refspec refreshes all source/environment branches even in a single-branch clone.
  git('fetch', '--quiet', '--tags', 'origin', '+refs/heads/*:refs/remotes/origin/*')
  let sourceRef
  try {
    sourceRef = git('symbolic-ref', 'refs/remotes/origin/HEAD')
  } catch {
    sourceRef = 'origin/main'
  }
  const sourceSha = git('rev-parse', '--verify', `${sourceRef}^{commit}`)
  const ancestor = (from, to) => {
    try {
      git('merge-base', '--is-ancestor', from, to)
      return true
    } catch (error) {
      if (error.status === 1) return false
      throw error
    }
  }
  const range = (from, to) =>
    to
      ? git('rev-list', '--first-parent', from ? `${from}..${to}` : to)
          .split('\n')
          .filter(Boolean)
      : []
  const points = environments.map((environment) => {
    if (state === 'branch') {
      const branch = `refs/remotes/origin/${environment}`
      if (!git('for-each-ref', '--format=%(refname)', branch).split('\n').includes(branch))
        return { environment, fromRef: '', anchor: 'never released' }
      return { environment, fromRef: git('merge-base', branch, sourceSha), anchor: branch }
    }
    const prefix = `${tagPrefix}${environment}/`
    // Compare actual tag names rather than passing an extension's prefix as a git glob.
    const tags = git('for-each-ref', '--format=%(refname:strip=2)', 'refs/tags')
      .split('\n')
      .filter((tag) => tag.startsWith(prefix) && /^\d{8}T\d{6}Z$/.test(tag.slice(prefix.length)))
      .sort()
    let tag = tags.at(-1)
    if (!tag) {
      // A release tag for another environment cannot initialise this environment's state.
      try {
        tag = git('describe', '--tags', '--abbrev=0', '--exclude', `${tagPrefix}*`, sourceSha)
      } catch (error) {
        if (error.status !== 128) throw error
      }
    }
    return {
      environment,
      fromRef: tag ? git('rev-parse', '--verify', `${tag}^{commit}`) : '',
      anchor: tag || 'never released',
    }
  })
  return {
    root,
    git,
    gh,
    descriptor,
    environments,
    state,
    tagPrefix,
    warnings,
    sourceRef,
    sourceSha,
    ancestor,
    range,
    points,
  }
}

function apiPages(gh, endpoint, key) {
  const pages = gh('api', endpoint, '--paginate', '--slurp')
  if (!Array.isArray(pages) || pages.some((page) => !page || !Array.isArray(page[key])))
    throw new Error(`unreadable ${key} payload`)
  const entries = pages.flatMap((page) => page[key])
  if (pages.some((page) => Number(page.total_count) > entries.length))
    throw new Error(`incomplete ${key} payload`)
  return { pages, entries }
}

function judgedCommit(repo, sha) {
  const base = `repos/{owner}/{repo}/commits/${sha}`
  const checkRuns = apiPages(
    repo.gh,
    `${base}/check-runs?per_page=100`,
    'check_runs',
  ).entries.filter((node) => !node.name?.startsWith('boss/'))
  const statuses = apiPages(repo.gh, `${base}/status?per_page=100`, 'statuses').entries.filter(
    (node) => !node.context?.startsWith('boss/'),
  )
  const workflows = apiPages(
    repo.gh,
    `repos/{owner}/{repo}/actions/runs?head_sha=${sha}&per_page=100`,
    'workflow_runs',
  )
  // Workflow failures can happen before any job check attaches. The shared classifier uses
  // workflow runs for liveness only, so carry non-successful runs as check evidence as well.
  const workflowChecks = workflows.entries
    .filter((node) => !node.head_sha || node.head_sha === sha)
    .filter((node) => !['success', 'skipped', 'neutral'].includes(node.conclusion))
    .map((node, index) => ({
      name: `workflow:${node.id ?? index}:${node.name ?? ''}`,
      status: node.status,
      conclusion: node.conclusion,
    }))
  return classifyChecks({
    headSHA: sha,
    observedSHA: sha,
    checkRuns,
    rollup: [
      ...statuses.map((node) => ({ name: node.context, state: node.state })),
      ...workflowChecks,
    ],
    workflowRuns: workflows.pages,
  })
}

export function pendingReleases(options = {}) {
  const repo = releaseRepository(options)
  const environments = repo.points.map(({ environment, fromRef, anchor }, index) => {
    const upperBound = index === 0 ? repo.sourceSha : repo.points[index - 1].fromRef
    const entry = {
      environment,
      verdict: 'up-to-date',
      fromRef,
      toRef: '',
      upperBound,
      count: 0,
      commits: [],
      reason: 'no unreleased changes',
      anchor,
    }
    if (fromRef && (!upperBound || !repo.ancestor(fromRef, upperBound)))
      return {
        ...entry,
        verdict: 'diverged',
        reason: `${fromRef} is not an ancestor of ${upperBound || 'unreleased previous environment'}`,
      }
    if (!upperBound) return entry
    entry.commits = repo.range(fromRef, upperBound)
    entry.count = entry.commits.length
    if (!entry.count) return entry
    if (!repo.descriptor)
      return { ...entry, verdict: 'ci-pending', reason: 'no release extension installed' }
    if (repo.state === 'branch') {
      const prs = repo.gh(
        'pr',
        'list',
        '--base',
        environment,
        '--state',
        'open',
        '--json',
        'number',
      )
      if (!Array.isArray(prs)) throw new Error('unreadable release PR list')
      if (prs.length)
        return {
          ...entry,
          verdict: 'in-flight',
          reason: `open release PR ${prs.map((pr) => `#${pr.number}`).join(', ')}`,
        }
    }
    for (const sha of entry.commits.slice(0, 20)) {
      const ci = judgedCommit(repo, sha)
      if (['no-checks', 'no-gate-ran'].includes(ci.reason)) continue
      if (ci.state !== 'green')
        return {
          ...entry,
          verdict:
            ci.state === 'failing'
              ? 'ci-red'
              : ci.state === 'pending'
                ? 'ci-pending'
                : 'ci-unknown',
          reason: `${sha}: ${ci.reason}`,
        }
      entry.toRef = sha
      // Envelope commits describe only the range the extension is asked to release.
      entry.commits = repo.range(fromRef, sha)
      const hold = `refs/boss-release/hold/${environment}`
      const held = repo.git('for-each-ref', '--format=%(objectname)', hold)
      return {
        ...entry,
        verdict: held === sha ? 'held' : 'ready',
        reason: held === sha ? `release held at ${sha}` : `CI green at ${sha}`,
      }
    }
    return {
      ...entry,
      verdict: 'ci-pending',
      reason: 'no CI-judged commit within the newest 20 unreleased commits',
    }
  })
  const report = repo.descriptor
    ? environments
        .map(
          (env) =>
            `${env.environment}: ${env.count} merged changes unreleased since ${env.anchor}; ${env.verdict} (${env.reason})`,
        )
        .join('\n')
    : `${environments[0].count} merged changes unreleased since ${environments[0].anchor}; no release extension installed`
  return {
    extension: repo.descriptor?.name ?? null,
    descriptor: repo.descriptor,
    sourceRef: repo.sourceRef,
    environments,
    report,
    warnings: repo.warnings,
  }
}

export function nextReleases(options = {}) {
  const result = pendingReleases(options)
  if (
    options.environment &&
    !result.environments.some((entry) => entry.environment === options.environment)
  )
    throw new Error(`unknown environment ${options.environment}`)
  const candidates = result.environments
    .filter(
      (entry) =>
        result.extension &&
        entry.verdict === 'ready' &&
        (!options.environment || options.environment === entry.environment),
    )
    .map(({ environment, fromRef, toRef, commits }) => ({ environment, fromRef, toRef, commits }))
  return { ...result, candidates }
}

export function recordRelease({
  environment,
  toRef,
  result,
  dryRun = false,
  now = new Date(),
  ...options
}) {
  const repo = releaseRepository(options)
  const rejected = (reason) => ({ verdict: 'rejected', reason, ref: '' })
  const validation = validateResult(result, 'release')
  if (!validation.ok) return rejected(validation.errors.join('; '))
  if (!repo.descriptor) return rejected('no release extension installed')
  const index = repo.environments.indexOf(environment)
  if (index < 0) return rejected(`unknown environment ${environment}`)
  if (!/^[a-f0-9]{40}$/i.test(toRef ?? '')) return rejected('to-ref must be a 40-hex SHA')
  const upperBound = index === 0 ? repo.sourceSha : repo.points[index - 1].fromRef
  if (!upperBound || !repo.ancestor(toRef, upperBound))
    return rejected(`${toRef} is outside promotion bound ${upperBound || '(none)'}`)
  const hold = `refs/boss-release/hold/${environment}`
  if (result.action !== 'released') {
    if (!dryRun) repo.git('update-ref', hold, toRef)
    return { verdict: result.action, reason: result.reason, ref: toRef, dryRun }
  }
  if (!repo.ancestor(toRef, result.ref) || !repo.ancestor(result.ref, upperBound))
    return rejected(
      `${result.ref} must include ${toRef} and stay within promotion bound ${upperBound}`,
    )
  const releasedPoint = repo.points[index].fromRef
  if (releasedPoint && !repo.ancestor(releasedPoint, result.ref))
    return rejected(`${result.ref} is behind the current release point ${releasedPoint}`)
  let tag
  if (repo.state === 'tag') {
    const stamp = now
      .toISOString()
      .replace(/[-:]/g, '')
      .replace(/\.\d{3}Z$/, 'Z')
    tag = `${repo.tagPrefix}${environment}/${stamp}`
    // Validate extension-provided prefixes before any write.
    repo.git('check-ref-format', `refs/tags/${tag}`)
    if (!dryRun) {
      repo.git('tag', '-a', tag, result.ref, '-m', `Release ${environment}: ${result.reason}`)
      try {
        repo.git('push', 'origin', `refs/tags/${tag}`)
      } catch (error) {
        repo.git('tag', '-d', tag)
        return rejected(`release tag push failed: ${error.message}`)
      }
    }
  }
  if (!dryRun && repo.git('for-each-ref', '--format=%(refname)', hold))
    repo.git('update-ref', '-d', hold)
  return {
    verdict: repo.state === 'branch' ? 'recorded-by-branch' : 'released',
    reason: result.reason,
    ref: result.ref,
    ...(tag ? { tag } : {}),
    dryRun,
  }
}

const USAGE =
  'usage: release-gate.mjs pending|next [--root <dir>] [--environment <env>] [--json]\n       release-gate.mjs record --environment <env> --to-ref <sha> --result <file> [--root <dir>] [--dry-run] [--json]\n'
export function main(
  argv,
  {
    run = defaultRun,
    stdout = (line) => process.stdout.write(line),
    stderr = (line) => process.stderr.write(line),
  } = {},
) {
  const [subcommand, ...rest] = argv
  const args = {}
  const flags = new Set(['json', 'dry-run'])
  const values = new Set(['root', 'environment', 'to-ref', 'result'])
  for (let i = 0; i < rest.length; i++) {
    const key = rest[i].startsWith('--') ? rest[i].slice(2) : ''
    if (flags.has(key)) args[key] = true
    else if (values.has(key) && rest[i + 1] && !rest[i + 1].startsWith('--')) args[key] = rest[++i]
    else {
      stderr(USAGE)
      return 64
    }
  }
  if (
    !['pending', 'next', 'record'].includes(subcommand) ||
    (subcommand === 'record' && (!args.environment || !args['to-ref'] || !args.result))
  ) {
    stderr(USAGE)
    return 64
  }
  try {
    const options = { root: args.root, run, environment: args.environment }
    const result =
      subcommand === 'record'
        ? recordRelease({
            ...options,
            toRef: args['to-ref'],
            result: JSON.parse(readFileSync(args.result, 'utf8')),
            dryRun: !!args['dry-run'],
          })
        : subcommand === 'next'
          ? nextReleases(options)
          : pendingReleases(options)
    stdout(`${args.json || subcommand === 'record' ? JSON.stringify(result) : result.report}\n`)
    return 0
  } catch (error) {
    stderr(`release-gate: ${error.message}\n`)
    return 2
  }
}
if (isMainModule(import.meta.url)) process.exit(main(process.argv.slice(2)))
