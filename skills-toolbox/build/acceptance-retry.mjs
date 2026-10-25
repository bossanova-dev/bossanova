#!/usr/bin/env node

// Retry an acceptance command on a temporary rebase without changing the caller's branch.
import { spawn } from 'node:child_process'
import { closeSync, mkdtempSync, openSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, join, resolve } from 'node:path'
import { parseArgs } from 'node:util'

import { countBehind, runGit } from '../base-drift.mjs'
import { isMainModule } from '../main-module.mjs'
import { parseSkillGateOutput } from '../skill-drift-verdict.mjs'

const labels = (line) => [...line.matchAll(/([^\s;,]+) \(([^,()]+), ([^()]+)\)/g)]

function skillsCheckoutBehind(output) {
  if (
    /reinstall withheld \[(?!downgrade\])|unable to check|⚠ drift — this running boss binary's embedded skills are behind checkout source/.test(
      output,
    )
  )
    return null
  const gate = parseSkillGateOutput(output)
  if (gate.rows.length) {
    return gate.remedy.refusal === 'downgrade' &&
      gate.rows.every((row) => row.direction === 'ahead')
      ? gate.rows.map((row) => `${row.path} (${row.kind}, ${row.direction})`)
      : null
  }
  const lines = output
    .split('\n')
    .filter((line) => line.includes('⚠ drift — installed skills differ from the selected payload;'))
  if (!lines.length) return null
  const evidence = []
  for (const line of lines) {
    const entries = labels(line)
    if (
      !line.includes('reinstall withheld [downgrade]') ||
      /run `/.test(line) ||
      !entries.length ||
      entries.some((entry) => entry[3] !== 'ahead')
    )
      return null
    evidence.push(...entries.map((entry) => entry[0]))
  }
  return evidence
}

const signals = Object.freeze({ 'skills-checkout-behind': skillsCheckoutBehind })

export function classify({ status, output, base, repo = process.cwd(), count = countBehind }) {
  const report = {
    verdict: 'no-retry',
    signal: null,
    reason: 'no-host-state-signal',
    behind: null,
    evidence: [],
  }
  if (status === 0) return { ...report, reason: 'not-failing' }
  if (!Number.isInteger(status) || status < 0 || typeof output !== 'string') return report
  for (const [signal, recognise] of Object.entries(signals)) {
    const evidence = recognise(output)
    if (!evidence) continue
    let behind
    try {
      behind = count({ repo, base })
    } catch {
      behind = 'unevaluated'
    }
    if (typeof behind !== 'number' || !Number.isInteger(behind) || behind <= 0)
      return { ...report, behind, reason: behind === 0 ? 'not-behind-base' : 'behind-unevaluated' }
    return { verdict: 'rebase-retry', signal, reason: 'checkout-behind-base', behind, evidence }
  }
  return report
}

// File descriptors keep arbitrary command output outside the process capture buffer.
export function runCommand({ repo, command, outputFile, timeoutMs }) {
  return new Promise((done) => {
    const fd = openSync(outputFile, 'w', 0o600)
    let child
    try {
      child = spawn('sh', ['-c', command], {
        cwd: repo,
        stdio: ['ignore', fd, fd],
        detached: process.platform !== 'win32',
      })
    } finally {
      closeSync(fd)
    }
    let timedOut = false
    const timer = setTimeout(() => {
      timedOut = true
      try {
        if (process.platform === 'win32') child.kill('SIGKILL')
        else process.kill(-child.pid, 'SIGKILL')
      } catch {
        /* The command may have exited while its deadline fired. */
      }
    }, timeoutMs)
    child.once('error', () => {
      clearTimeout(timer)
      done({ status: null, timedOut, error: 'command-start-failed' })
    })
    child.once('close', (status) => {
      clearTimeout(timer)
      done({ status, timedOut, error: null })
    })
  })
}

export async function retry({
  command,
  base,
  repo = process.cwd(),
  timeoutMs = 1200000,
  outputFile,
  tmpRoot = tmpdir(),
  run = runGit,
  execute = runCommand,
}) {
  const report = {
    result: 'unevaluated',
    met: false,
    head: null,
    base,
    baseSha: null,
    rebasedHead: null,
    status: null,
    outputFile: outputFile ? resolve(outputFile) : null,
    reason: '',
    evidence: '',
  }
  const finish = (result, reason) =>
    Object.assign(report, { result, met: result === 'pass', reason })
  let scratch
  let worktree
  try {
    const head = run(repo, ['rev-parse', '--verify', 'HEAD'])
    if (head.status !== 0) return finish('unevaluated', 'head-unevaluated')
    report.head = head.stdout.trim()
    // Split once: branch names may themselves contain slashes.
    const slash = base.indexOf('/')
    if (slash <= 0 || slash === base.length - 1 || base.startsWith('-'))
      return finish('unevaluated', 'base must name remote/branch')
    const remote = base.slice(0, slash)
    const branch = base.slice(slash + 1)
    if (run(repo, ['fetch', '--quiet', remote, branch]).status !== 0)
      return finish('unevaluated', `fetch-failed for ${base}`)
    // FETCH_HEAD is the fetched branch even with a nonstandard remote fetch refspec.
    const baseResult = run(repo, ['rev-parse', '--verify', 'FETCH_HEAD^{commit}'])
    if (baseResult.status !== 0) return finish('unevaluated', `base-unevaluated for ${base}`)
    report.baseSha = baseResult.stdout.trim()
    const target = `${base}@${report.baseSha.slice(0, 7)}`
    scratch = mkdtempSync(join(tmpRoot, 'acceptance-retry-'))
    worktree = join(scratch, 'worktree')
    report.outputFile ||= join(tmpRoot, `${basename(scratch)}.log`)
    if (
      run(repo, [
        '-c',
        'core.hooksPath=/dev/null',
        'worktree',
        'add',
        '--detach',
        worktree,
        report.head,
      ]).status !== 0
    )
      return finish('unevaluated', `worktree-add-failed for ${target}`)
    const rebased = run(worktree, [
      '-c',
      'core.hooksPath=/dev/null',
      '-c',
      'commit.gpgsign=false',
      'rebase',
      report.baseSha,
    ])
    if (rebased.status !== 0) {
      const conflicted = run(worktree, ['ls-files', '--unmerged'])
      run(worktree, ['-c', 'core.hooksPath=/dev/null', 'rebase', '--abort'])
      return conflicted.status === 0 && conflicted.stdout.trim()
        ? finish('rebase-conflict', `rebase-conflict onto ${target}`)
        : finish('unevaluated', `rebase-failed onto ${target}`)
    }
    const tip = run(worktree, ['rev-parse', '--verify', 'HEAD'])
    if (tip.status !== 0) return finish('unevaluated', `rebased-head-unevaluated onto ${target}`)
    report.rebasedHead = tip.stdout.trim()
    const result = await execute({
      repo: worktree,
      command,
      outputFile: report.outputFile,
      timeoutMs,
    })
    report.status = result.status
    if (result.timedOut || result.error)
      return finish(
        'unevaluated',
        `${result.timedOut ? 'timeout' : result.error} after rebase onto ${target}`,
      )
    const displayCommand = command.replace(/`/g, '\\`').replace(/[\r\n]+/g, ' ')
    if (result.status === 0) {
      report.evidence = `rebased-retry: \`${displayCommand}\` → pass on ${report.rebasedHead.slice(0, 7)} rebased onto ${target} in a scratch tree (not pushed)`
      return finish('pass', `passes after rebase onto ${target}`)
    }
    const output = readFileSync(report.outputFile, 'utf8')
    const persisted = classify({
      status: result.status,
      output,
      base: report.baseSha,
      repo: worktree,
      count: (opts) => countBehind({ ...opts, run }),
    })
    const signal = skillsCheckoutBehind(output) ? 'yes' : 'no'
    return finish(
      'fail',
      `still fails after rebase onto ${target} (exit ${result.status}; signal persisted: ${persisted.signal || signal})`,
    )
  } catch (error) {
    return finish('unevaluated', `retry-unevaluated: ${String(error.message).split('\n')[0]}`)
  } finally {
    if (scratch) {
      if (worktree) run(repo, ['worktree', 'remove', '--force', worktree])
      rmSync(scratch, { recursive: true, force: true, maxRetries: 5 })
      run(repo, ['worktree', 'prune'])
    }
  }
}

export async function main(args = process.argv.slice(2)) {
  try {
    const { values, positionals } = parseArgs({
      args,
      strict: true,
      allowPositionals: true,
      options: {
        status: { type: 'string' },
        'output-file': { type: 'string' },
        base: { type: 'string' },
        repo: { type: 'string' },
        command: { type: 'string' },
        'timeout-ms': { type: 'string' },
      },
    })
    const [subcommand] = positionals
    const timeoutMs = values['timeout-ms'] === undefined ? 1200000 : Number(values['timeout-ms'])
    if (
      positionals.length !== 1 ||
      !values.base ||
      !['classify', 'retry'].includes(subcommand) ||
      (subcommand === 'classify' &&
        (!/^\d+$/.test(values.status || '') || !values['output-file'])) ||
      (subcommand === 'retry' &&
        (!values.command || !Number.isSafeInteger(timeoutMs) || timeoutMs <= 0))
    ) {
      process.stderr.write(
        'usage: acceptance-retry.mjs classify --status <n> --output-file <path> --base <ref> [--repo <dir>] | retry --command <cmd> --base <remote/branch> [--repo <dir>] [--timeout-ms <n>] [--output-file <path>]\n',
      )
      return 2
    }
    let result
    if (subcommand === 'classify') {
      let output
      try {
        output = readFileSync(values['output-file'], 'utf8')
      } catch {
        output = null
      }
      result = classify({
        status: Number(values.status),
        output,
        base: values.base,
        repo: values.repo,
      })
    } else
      result = await retry({
        command: values.command,
        base: values.base,
        repo: values.repo,
        timeoutMs,
        outputFile: values['output-file'],
      })
    process.stdout.write(`${JSON.stringify(result)}\n`)
    return result.verdict === 'rebase-retry' || result.result === 'pass' ? 0 : 3
  } catch (error) {
    process.stderr.write(`${error.message}\n`)
    return error.code?.startsWith('ERR_PARSE_ARGS') ? 2 : 1
  }
}

if (isMainModule(import.meta.url)) process.exitCode = await main()
