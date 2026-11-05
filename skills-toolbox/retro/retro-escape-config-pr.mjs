#!/usr/bin/env node
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { CONFIG_FILENAME, verifyPolicyFromConfigText } from '../skill-config.mjs'
import { isMainModule } from '../main-module.mjs'
import { checked, readReason } from './retro-signals.mjs'
import {
  MAX_PRS,
  proposeAlwaysReview,
  applyProposal,
  prTitle,
  prBody,
} from './retro-escape-config.mjs'

/** @typedef {{status?: number | null, stdout?: string | Buffer, stderr?: string | Buffer, error?: Error}} CommandResult
 * @typedef {(command: string, args: string[], options?: object) => CommandResult} Run
 * @typedef {{outcomes: string, repo?: string, max?: number, dryRun?: boolean, out?: string}} Options
 * @typedef {{run?: Run, fs?: typeof fs, stdout?: (text: string) => unknown, stderr?: (text: string) => unknown}} Dependencies
 */
/** @param {unknown} error */
const reason = (error) => (error instanceof Error ? error.message : String(error))
/** @param {string | null} before @param {string} after */
function diffLines(before, after) {
  const old = (before ?? '').split('\n'),
    next = after.split('\n')
  let start = 0,
    endOld = old.length,
    endNext = next.length
  while (start < endOld && start < endNext && old[start] === next[start]) start++
  while (endOld > start && endNext > start && old[endOld - 1] === next[endNext - 1]) {
    endOld--
    endNext--
  }
  return [
    ...old.slice(start, endOld).map((line) => `-${line}`),
    ...next.slice(start, endNext).map((line) => `+${line}`),
  ]
}

/** @param {Options} options @param {Dependencies} [deps] */
export function collect(options, deps = {}) {
  const io = deps.fs ?? fs
  const run = deps.run ?? ((cmd, args, opts) => spawnSync(cmd, args, { encoding: 'utf8', ...opts }))
  const outcomes = JSON.parse(io.readFileSync(options.outcomes, 'utf8'))
  const escapes = outcomes.sections?.escapeRows ?? outcomes.sections?.escapes
  if (escapes?.status !== 'ok')
    return { status: 'skipped', reason: 'no-escapes', detail: escapes?.reason ?? 'missing-section' }
  /** @type {{v: number, mode: string, repo?: string, base?: string, status?: string, reason?: string, proposals: (import('./retro-escape-config.mjs').Proposal & {diff?: string[]})[], skipped: {original: object, fix: object, reason: string}[], opened: {branch: string, number: number, url: string}[], existing: {branch: string, number: number, state: string, url: string}[], deferred: import('./retro-escape-config.mjs').Proposal[], failures: {branch: string, reason: string}[]}} */
  const result = {
    v: 1,
    mode: options.dryRun ? 'dry-run' : 'write',
    proposals: [],
    skipped: [],
    opened: [],
    existing: [],
    deferred: [],
    failures: [],
  }
  /** @param {string} endpoint */
  const read = (endpoint) => JSON.parse(checked(run, 'gh', ['api', endpoint]))
  /** @param {string} repo @param {string} ref */
  const readConfig = (repo, ref) => {
    const response = run(
      'gh',
      ['api', `repos/${repo}/contents/${CONFIG_FILENAME}?ref=${encodeURIComponent(ref)}`],
      { timeout: 30000 },
    )
    if (response.error || response.status !== 0) {
      if (/\b404\b/.test(String(response.stderr ?? '')))
        return {
          text: null,
          sha: undefined,
          policy: verifyPolicyFromConfigText('{}', 'absent config'),
        }
      throw new Error(readReason(response, 'gh'))
    }
    const file = JSON.parse(String(response.stdout ?? ''))
    if (
      file.type !== 'file' ||
      file.encoding !== 'base64' ||
      typeof file.content !== 'string' ||
      typeof file.sha !== 'string'
    )
      throw new Error('not-a-file')
    const text = Buffer.from(file.content, 'base64').toString('utf8')
    return { text, sha: file.sha, policy: verifyPolicyFromConfigText(text, CONFIG_FILENAME) }
  }
  /** @param {string} endpoint @param {string} method @param {object} payload */
  const write = (endpoint, method, payload) => {
    const directory = io.mkdtempSync(path.join(os.tmpdir(), 'retro-escape-config-'))
    try {
      const input = path.join(directory, 'payload.json')
      io.writeFileSync(input, JSON.stringify(payload), { mode: 0o600 })
      const response = run('gh', ['api', endpoint, '--method', method, '--input', input], {
        timeout: 30000,
      })
      if (response.error || response.status !== 0)
        throw new Error(String(response.stderr ?? '') || readReason(response, 'gh'))
      return JSON.parse(String(response.stdout ?? '{}'))
    } finally {
      io.rmSync(directory, { recursive: true, force: true, maxRetries: 3 })
    }
  }
  try {
    const repo =
      options.repo ??
      JSON.parse(checked(run, 'gh', ['repo', 'view', '--json', 'nameWithOwner'])).nameWithOwner
    if (typeof repo !== 'string' || !/^[\w.-]+\/[\w.-]+$/.test(repo)) throw new Error('no-repo')
    result.repo = repo
    const base = read(`repos/${repo}`).default_branch
    if (typeof base !== 'string' || !base) throw new Error('no-base-ref')
    result.base = base
    const tip = read(`repos/${repo}/git/ref/heads/${base}`).object?.sha
    if (typeof tip !== 'string' || !/^[a-f0-9]{40}$/i.test(tip)) throw new Error('no-base-sha')
    const config = readConfig(repo, tip)
    const proposed = proposeAlwaysReview(escapes.rows, config.policy.review)
    result.proposals = proposed.proposals
    result.skipped = proposed.skipped
    const owner = repo.split('/')[0]
    /** @param {string} branch */
    const existing = (branch) => {
      const prs = read(
        `repos/${repo}/pulls?state=all&head=${encodeURIComponent(`${owner}:${branch}`)}`,
      )
      if (!Array.isArray(prs)) throw new Error('invalid-pr-list')
      return prs[0]
    }
    /** @param {string} branch @param {{number: number, state: string, html_url: string}} pr */
    const recordExisting = (branch, pr) =>
      result.existing.push({ branch, number: pr.number, state: pr.state, url: pr.html_url })
    for (const proposal of result.proposals) {
      try {
        if (!options.dryRun) {
          const pr = existing(proposal.branch)
          if (pr) {
            recordExisting(proposal.branch, pr)
            continue
          }
          if (result.opened.length >= Math.min(options.max ?? MAX_PRS, MAX_PRS)) {
            result.deferred.push(proposal)
            continue
          }
        }
        const rendered = applyProposal(config.text, proposal)
        if (typeof rendered !== 'string') {
          if ('error' in rendered) throw new Error(rendered.error)
          result.skipped.push({ original: proposal.original, fix: proposal.fix, reason: 'covered' })
          continue
        }
        if (options.dryRun) {
          proposal.diff = diffLines(config.text, rendered)
          continue
        }
        try {
          write(`repos/${repo}/git/refs`, 'POST', {
            ref: `refs/heads/${proposal.branch}`,
            sha: tip,
          })
        } catch (error) {
          if (!/422/.test(reason(error)) || !/already exists/i.test(reason(error))) throw error
        }
        const branchConfig = readConfig(repo, proposal.branch)
        const content = applyProposal(branchConfig.text, proposal)
        if (typeof content === 'string')
          write(`repos/${repo}/contents/${CONFIG_FILENAME}`, 'PUT', {
            message: prTitle(proposal),
            content: Buffer.from(content).toString('base64'),
            branch: proposal.branch,
            ...(branchConfig.sha ? { sha: branchConfig.sha } : {}),
          })
        else if ('error' in content) throw new Error(content.error)
        try {
          const pr = write(`repos/${repo}/pulls`, 'POST', {
            title: prTitle(proposal),
            head: proposal.branch,
            base,
            body: prBody(proposal),
            draft: false,
          })
          result.opened.push({ branch: proposal.branch, number: pr.number, url: pr.html_url })
        } catch (error) {
          if (!/422/.test(reason(error)) || !/pull request already exists/i.test(reason(error)))
            throw error
          const pr = existing(proposal.branch)
          if (!pr) throw error
          recordExisting(proposal.branch, pr)
        }
      } catch (error) {
        result.failures.push({ branch: proposal.branch, reason: reason(error) })
      }
    }
    return result
  } catch (error) {
    return { ...result, status: 'failed', reason: reason(error) }
  }
}

/** @param {string[]} [argv] @param {Dependencies} [deps] */
export function main(argv = process.argv.slice(2), deps = {}) {
  const stderr = deps.stderr ?? ((text) => process.stderr.write(text))
  let options
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        outcomes: { type: 'string' },
        repo: { type: 'string' },
        max: { type: 'string' },
        'dry-run': { type: 'boolean' },
        out: { type: 'string' },
      },
    })
    if (
      positionals.length !== 1 ||
      positionals[0] !== 'open' ||
      !values.outcomes ||
      (values.max !== undefined &&
        (!/^\d+$/.test(values.max) || !Number.isSafeInteger(Number(values.max))))
    )
      throw new Error(
        'usage: open --outcomes <file> [--repo <owner/name>] [--max <n>] [--dry-run] [--out <file>]',
      )
    options = {
      outcomes: values.outcomes,
      repo: values.repo,
      max: values.max === undefined ? MAX_PRS : Number(values.max),
      dryRun: values['dry-run'],
      out: values.out,
    }
  } catch (error) {
    stderr(`${reason(error)}\n`)
    return 64
  }
  let output
  try {
    output = JSON.stringify(collect(options, deps), null, 2) + '\n'
  } catch (error) {
    stderr(`${reason(error)}\n`)
    return 1
  }
  if (options.out) {
    try {
      ;(deps.fs ?? fs).writeFileSync(options.out, output)
    } catch (error) {
      stderr(`${reason(error)}\n`)
    }
  }
  ;(deps.stdout ?? ((text) => process.stdout.write(text)))(output)
  return 0
}

if (isMainModule(import.meta.url)) process.exitCode = main()
