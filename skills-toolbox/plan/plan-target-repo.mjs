import { execFileSync } from 'node:child_process'
import { readdirSync } from 'node:fs'
import { normalizeRepositoryIdentity } from '../callback/epic-target.mjs'
import { planDescriptionSections, scanFences } from '../skill-config.mjs'

/** @typedef {{ declared: false } | { declared: true, repo: string } | { declared: true, error: string }} TargetDeclaration */
/** @typedef {{ kind: 'local', root: string, couldNotEvaluate?: string } | { kind: 'foreign', repo: string, root: string } | { kind: 'foreign', repo: string, root: null, reason: string }} TargetContext */

/**
 * @param {Parameters<typeof planDescriptionSections>[0]} config
 * @param {string} description
 * @returns {TargetDeclaration}
 */
export function parseTargetRepoDeclaration(config, description) {
  const sections = planDescriptionSections(config, description).filter(
    (s) => s.heading === '## Planning',
  )
  const repos = new Set()
  for (const section of sections) {
    for (const { line } of scanFences(section.bodyLines.join('\n')).lines) {
      const match = /^\s*[-*] Target repo:\s*(.*?)\s*$/.exec(line)
      if (!match) continue
      const value = match[1].replace(/^`([^`]+)`$/, '$1')
      const repo = normalizeRepositoryIdentity(value)
      if (!repo)
        return { declared: true, error: 'Target repo declaration is not a repository identity' }
      repos.add(repo)
    }
  }
  if (repos.size > 1)
    return { declared: true, error: 'Target repo declarations name different repositories' }
  const repo = repos.values().next().value
  return repo ? { declared: true, repo } : { declared: false }
}

/**
 * @param {string} root
 * @param {{ exec?: typeof execFileSync }} [options]
 * @returns {string | null}
 */
export function readOriginIdentity(root, { exec = execFileSync } = {}) {
  try {
    return normalizeRepositoryIdentity(
      exec('git', ['remote', 'get-url', 'origin'], {
        cwd: root,
        encoding: 'utf8',
        timeout: 3000,
        stdio: ['ignore', 'pipe', 'pipe'],
      }),
    )
  } catch {
    return null
  }
}

/**
 * @param {{ declaration: TargetDeclaration, checkoutRoot: string, targetRoot?: string | null,
 *   readOrigin?: (root: string) => string | null }} input
 * @returns {TargetContext}
 */
export function resolveTargetContext({
  declaration,
  checkoutRoot,
  targetRoot,
  readOrigin = readOriginIdentity,
}) {
  if (!declaration.declared) return { kind: 'local', root: checkoutRoot }
  if ('error' in declaration)
    return { kind: 'local', root: checkoutRoot, couldNotEvaluate: declaration.error }
  const origin = normalizeRepositoryIdentity(readOrigin(checkoutRoot))
  if (!origin)
    return { kind: 'local', root: checkoutRoot, couldNotEvaluate: 'checkout origin is unreadable' }
  const repo = declaration.repo
  if (repo === origin) return { kind: 'local', root: checkoutRoot }
  if (!targetRoot)
    return { kind: 'foreign', repo, root: null, reason: 'no --target-root was supplied' }
  const targetOrigin = normalizeRepositoryIdentity(readOrigin(targetRoot))
  if (targetOrigin === repo) return { kind: 'foreign', repo, root: targetRoot }
  return {
    kind: 'foreign',
    repo,
    root: null,
    reason: targetOrigin
      ? `target root origin ${targetOrigin} does not match ${repo}`
      : 'target root origin is unreadable',
  }
}

/** @param {Extract<TargetContext, { root: null }>} context */
export function targetRepoNotResolvedAdvisory(context) {
  return {
    code: 'advisory: target-repo-not-resolved',
    message: `plan-contract-guard: target repository ${context.repo}: ${context.reason}; root-dependent checks were skipped`,
  }
}

/** @param {string} root @returns {string[]} */
export function targetModuleRoots(root) {
  return readdirSync(root).filter((entry) => entry !== '.git')
}
