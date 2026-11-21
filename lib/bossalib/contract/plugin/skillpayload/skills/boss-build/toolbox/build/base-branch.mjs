#!/usr/bin/env node

import { execFile } from 'node:child_process'
import { parseArgs, promisify } from 'node:util'

import { resolveBossBinary } from '../boss-binary.mjs'
import { isMainModule } from '../main-module.mjs'

/** @typedef {{remote: string, branch: string, source: string}} BaseBranch */

/**
 * Resolve the session's intended base before considering its tracking branch.
 * @param {{sessionBranch: string, sessionBase?: unknown, prBase?: unknown, upstream?: string, defaultBranch?: unknown}} input
 * @returns {BaseBranch|null}
 */
export function resolveBaseBranch({
  sessionBranch,
  sessionBase,
  prBase,
  upstream = '',
  defaultBranch,
}) {
  const separator = upstream.indexOf('/')
  const upstreamRemote = separator > 0 ? upstream.slice(0, separator) : ''
  const upstreamBranch = separator > 0 ? upstream.slice(separator + 1) : ''
  const prRemote =
    typeof prBase === 'string' &&
    prBase.trim() === upstreamBranch &&
    upstreamBranch !== sessionBranch
      ? upstreamRemote
      : 'origin'
  /** @type {[string, unknown, string][]} */
  const candidates = [
    ['origin', sessionBase, 'session'],
    [prRemote, prBase, 'pr'],
    [upstreamRemote, upstreamBranch, 'upstream'],
    ['origin', defaultBranch, 'default'],
  ]
  for (const [remote, candidate, source] of candidates) {
    if (typeof candidate !== 'string') continue
    const branch = candidate.trim()
    if (remote && branch && branch !== sessionBranch) return { remote, branch, source }
  }
  return null
}

const execAsync = promisify(execFile)

/** @param {string} command @param {string[]} args */
async function execute(command, args) {
  const { stdout } = await execAsync(command, args, { encoding: 'utf8', timeout: 15_000 })
  return stdout
}

/**
 * @param {string[]} argv
 * @param {{exec?: (command: string, args: string[]) => Promise<string>}} [deps]
 * @returns {Promise<{exitCode: number, stdout: string, stderr: string}>}
 */
export async function main(argv, deps = {}) {
  let values
  try {
    const parsed = parseArgs({
      args: argv,
      options: {
        'session-branch': { type: 'string' },
        upstream: { type: 'string' },
        'session-id': { type: 'string' },
        boss: { type: 'string' },
        gh: { type: 'string' },
      },
      strict: true,
      allowPositionals: true,
    })
    values = parsed.values
    const [command] = parsed.positionals
    if (
      parsed.positionals.length !== 1 ||
      command !== 'resolve' ||
      !values['session-branch']?.trim()
    ) {
      throw new Error(
        'usage: base-branch.mjs resolve --session-branch <branch> [--session-id <id>] [--upstream <remote/branch>] [--boss <path>] [--gh <path>]',
      )
    }
  } catch (error) {
    return {
      exitCode: 64,
      stdout: '',
      stderr: `${error instanceof Error ? error.message : String(error)}\n`,
    }
  }

  const exec = deps.exec ?? execute
  const sessionBranch = values['session-branch'] ?? ''
  const gh = values.gh || 'gh'
  const tried = []
  /**
   * Failed commands and malformed source payloads are misses.
   * @param {string} command
   * @param {string[]} args
   */
  async function read(command, args) {
    try {
      return JSON.parse(await exec(command, args))
    } catch {
      return null
    }
  }
  /** @param {BaseBranch} base */
  const found = (base) => ({
    exitCode: 0,
    stdout: `${base.remote}\t${base.branch}\t${base.source}\n`,
    stderr: '',
  })

  if (values['session-id']) {
    tried.push('session')
    const boss = values.boss || resolveBossBinary().path
    const session = boss ? await read(boss, ['show', values['session-id'], '--json']) : null
    const base = resolveBaseBranch({ sessionBranch, sessionBase: session?.session?.base_branch })
    if (base) {
      // Stored session bases can outlive a merged parent branch.
      const ref = `refs/heads/${base.branch}`
      try {
        const refs = await exec('git', ['ls-remote', '--heads', base.remote, ref])
        if (refs.split('\n').some((line) => line.split(/\s+/)[1] === ref)) return found(base)
      } catch {
        // An unreadable remote makes this source a miss, like the other source reads.
      }
    }
  }
  tried.push('pr')
  const pr = await read(gh, ['pr', 'view', sessionBranch, '--json', 'baseRefName,state'])
  const prBase = resolveBaseBranch({
    sessionBranch,
    prBase: pr?.state === 'OPEN' ? pr.baseRefName : undefined,
    upstream: values.upstream,
  })
  if (prBase) return found(prBase)

  tried.push('upstream')
  const upstreamBase = resolveBaseBranch({ sessionBranch, upstream: values.upstream })
  if (upstreamBase) return found(upstreamBase)

  tried.push('default')
  const repo = await read(gh, ['repo', 'view', '--json', 'defaultBranchRef'])
  const defaultBase = resolveBaseBranch({
    sessionBranch,
    defaultBranch: repo?.defaultBranchRef?.name,
  })
  if (defaultBase) return found(defaultBase)
  return {
    exitCode: 1,
    stdout: '',
    stderr: `unable to resolve the base branch; tried ${tried.join(', ')}\n`,
  }
}

if (isMainModule(import.meta.url)) {
  const result = await main(process.argv.slice(2))
  process.stdout.write(result.stdout)
  process.stderr.write(result.stderr)
  process.exitCode = result.exitCode
}
