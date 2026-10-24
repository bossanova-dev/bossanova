#!/usr/bin/env node
// Codex's final message is the review; tool events stay in an on-disk diagnostic log.
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'

import {
  buildExecArgv,
  classifyProbe,
  interpretResult,
} from '../../skills-toolbox/review/cross-review-lib.mjs'
import { resolveCodexBin } from '../../skills-toolbox/review/codex-review.mjs'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { parseFindingsMarkdown, RUN_LAYOUT } from './bs-security-findings.mjs'

class UsageError extends Error {}

export const POLICY_MARKERS = Object.freeze([
  'This content was flagged for possible cybersecurity risk',
])

export function buildPrompt(template, knownList) {
  const placeholder = '{{KNOWN_FINDINGS}}'
  if (template.split(placeholder).length !== 2) {
    throw new UsageError('template must contain exactly one {{KNOWN_FINDINGS}} placeholder')
  }
  // A function replacement keeps dollar signs in the supplied list literal.
  return template.replace(placeholder, () => knownList.trim() || '(none on file)')
}

export function classifyOutcome({
  probeResult,
  timedOut = false,
  verdict,
  findings = [],
  logText = '',
} = {}) {
  if (probeResult === 'not_installed' || probeResult === 'not_authed') {
    return { outcome: 'unavailable', reason: probeResult }
  }
  if (timedOut) return { outcome: 'timeout', reason: 'timed out' }
  if (findings.length > 0) {
    return { outcome: 'ok', reason: verdict?.skipReason ?? 'findings parsed' }
  }
  const marker = POLICY_MARKERS.find((value) => logText.toLowerCase().includes(value.toLowerCase()))
  if (marker) return { outcome: 'blocked', reason: marker }
  return { outcome: 'empty', reason: verdict?.skipReason ?? 'no finding blocks' }
}

function killGroup(pid, signal) {
  try {
    process.kill(-pid, signal)
  } catch {
    // The group may already have exited by the time teardown runs.
  }
}

function execute(bin, args, { env, stdio, timeoutMs }) {
  return new Promise((resolve) => {
    let timedOut = false
    let escalated = false
    let result
    let timer
    const finish = () => {
      if (!result || (timedOut && !escalated)) return
      clearTimeout(timer)
      resolve({ ...result, timedOut })
    }
    const child = spawn(bin, args, { env, stdio, detached: true })
    child.once('error', (spawnError) => {
      result = { code: null, signal: null, spawnError }
      finish()
    })
    child.once('close', (code, signal) => {
      result ??= { code, signal, spawnError: null }
      finish()
    })
    timer = setTimeout(() => {
      timedOut = true
      killGroup(child.pid, 'SIGTERM')
      // Keep this armed even if the leader exits: descendants may ignore SIGTERM.
      setTimeout(() => {
        killGroup(child.pid, 'SIGKILL')
        escalated = true
        finish()
      }, 200)
    }, timeoutMs)
  })
}

async function policyMarkerInLog(logPath) {
  // Match across chunk boundaries without retaining the multi-megabyte tool log.
  const overlap = Math.max(...POLICY_MARKERS.map((marker) => marker.length)) - 1
  let tail = ''
  for await (const chunk of fs.createReadStream(logPath, { encoding: 'utf8' })) {
    const text = tail + chunk
    const marker = POLICY_MARKERS.find((value) => text.toLowerCase().includes(value.toLowerCase()))
    if (marker) return marker
    tail = text.slice(-overlap)
  }
  return ''
}

export async function runCodexReview({
  repo,
  runDir,
  template,
  knownList,
  timeoutMs = 90 * 60 * 1000,
  env = process.env,
}) {
  const prompt = buildPrompt(fs.readFileSync(template, 'utf8'), fs.readFileSync(knownList, 'utf8'))
  repo = path.resolve(repo)
  runDir = path.resolve(runDir)
  const findingsPath = path.join(runDir, RUN_LAYOUT.codexFindings)
  const codexDir = path.dirname(findingsPath)
  const lastMessagePath = path.join(codexDir, 'last-message.md')
  const logPath = path.join(codexDir, 'raw.log')
  fs.mkdirSync(codexDir, { recursive: true })
  // A repeated invocation cannot inherit an old successful final message.
  fs.rmSync(lastMessagePath, { force: true })
  const logFd = fs.openSync(logPath, 'w')
  let probeResult = 'not_installed'
  let result = { code: null, signal: null, timedOut: false }
  try {
    const bin = resolveCodexBin(env)
    if (bin !== null) {
      const login = await execute(bin, ['login', 'status'], {
        env,
        stdio: ['ignore', 'ignore', 'ignore'],
        timeoutMs: 5000,
      })
      probeResult = classifyProbe({
        spawnError: login.spawnError,
        status: login.code,
        signal: login.signal,
      })
      if (login.timedOut) probeResult = 'error'
      if (probeResult !== 'not_installed' && probeResult !== 'not_authed') {
        const args = buildExecArgv(
          { subcommand: 'exec', flags: ['-s', 'read-only', '-o', lastMessagePath] },
          { repo, prompt },
        )
        result = await execute(bin, args, { env, stdio: ['ignore', logFd, logFd], timeoutMs })
        if (result.spawnError) fs.writeSync(logFd, `${result.spawnError.message}\n`)
      }
    }
  } finally {
    fs.closeSync(logFd)
  }
  let finalMessage = ''
  try {
    finalMessage = fs.readFileSync(lastMessagePath, 'utf8')
  } catch (err) {
    if (err.code !== 'ENOENT') throw err
  }
  const findings = parseFindingsMarkdown(finalMessage, {
    source: 'codex',
    lens: 'codex',
    lensFamily: 'cross-model',
    root: repo,
  }).records
  const verdict = interpretResult({ ...result, stdout: finalMessage })
  const classified = classifyOutcome({
    probeResult,
    timedOut: result.timedOut,
    verdict,
    findings,
    logText: findings.length > 0 ? '' : await policyMarkerInLog(logPath),
  })
  const outcome = { ...classified, findings, findingsPath, logPath }
  fs.writeFileSync(findingsPath, `${JSON.stringify(findings, null, 2)}\n`)
  fs.writeFileSync(path.join(codexDir, 'outcome.json'), `${JSON.stringify(outcome, null, 2)}\n`)
  return outcome
}

async function main(argv) {
  let parsed
  try {
    parsed = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        repo: { type: 'string' },
        'run-dir': { type: 'string' },
        template: { type: 'string' },
        'known-list': { type: 'string' },
        'timeout-ms': { type: 'string' },
      },
    })
  } catch (err) {
    throw new UsageError(err.message)
  }
  const { values, positionals } = parsed
  if (
    positionals.length !== 1 ||
    positionals[0] !== 'run' ||
    ['repo', 'run-dir', 'template', 'known-list'].some((key) => !values[key])
  ) {
    throw new UsageError(
      'usage: run --repo <dir> --run-dir <dir> --template <file> --known-list <file> [--timeout-ms <n>]',
    )
  }
  const timeoutMs = values['timeout-ms'] === undefined ? undefined : Number(values['timeout-ms'])
  if (
    timeoutMs !== undefined &&
    (!Number.isSafeInteger(timeoutMs) || timeoutMs <= 0 || timeoutMs > 2147483647)
  ) {
    throw new UsageError('--timeout-ms must be a positive integer no greater than 2147483647')
  }
  const result = await runCodexReview({
    repo: values.repo,
    runDir: values['run-dir'],
    template: values.template,
    knownList: values['known-list'],
    timeoutMs,
  })
  process.stdout.write(`${JSON.stringify(result)}\n`)
}

if (isMainModule(import.meta.url)) {
  main(process.argv.slice(2)).catch((err) => {
    process.stderr.write(`bs-security-codex: ${err.message}\n`)
    process.exitCode = err instanceof UsageError ? 2 : 1
  })
}
