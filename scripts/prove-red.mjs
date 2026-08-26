#!/usr/bin/env node

import { spawn, spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import process from 'node:process'

const VERDICTS = Object.freeze({
  red: 'PROOF RED',
  vacuous: 'PROOF VACUOUS — gate stayed green under an applied mutation',
  byteIdentical:
    'PROOF VACUOUS — output byte-identical under an applied mutation (the gate may not have re-executed; see the deferred BOS-1005 caching half)',
  baselineRed: 'PROOF INCONCLUSIVE — baseline already red',
  restoreMismatch: 'PROOF UNSAFE — restore mismatch',
  restoreGateRed: 'PROOF UNSAFE — gate not green after restore',
  wrongReason: 'PROOF INCONCLUSIVE — red for the wrong reason',
  mutationDidNotApply: 'PROOF INCONCLUSIVE — mutation did not apply',
  lockHeld: 'PROOF BLOCKED — worktree lock held by peer',
})

const EXIT = Object.freeze({
  red: 0,
  vacuous: 1,
  usage: 2,
  byteIdentical: 92,
  baselineRed: 93,
  unsafe: 94,
  wrongReason: 95,
  mutationDidNotApply: 96,
  lockHeld: 97,
})

const EMERGENCY_KILL_GRACE_MS = 2_000

const state = {
  cleanupScratch: true,
  currentChild: null,
  currentChildDone: null,
  locked: false,
  lockScript: null,
  original: null,
  releaseLockOnExit: true,
  restored: false,
  root: null,
  runId: process.env.BLI_RUNID || `prove-red-${process.pid}-${Date.now()}`,
  scratch: null,
  target: null,
}

function usage() {
  process.stderr.write(
    [
      'usage:',
      '  node scripts/prove-red.mjs --target <path> --from <literal> --to <literal> --expect-red-matching <regexp> [--occurrences <n>] -- <gate cmd...>',
      '',
    ].join('\n'),
  )
  process.exit(EXIT.usage)
}

function parseArgs(args) {
  const parsed = { occurrences: 1 }
  const sep = args.indexOf('--')
  if (sep === -1 || sep === args.length - 1) usage()
  const options = args.slice(0, sep)
  const command = args.slice(sep + 1)

  for (let i = 0; i < options.length; i += 1) {
    const arg = options[i]
    const value = options[i + 1]
    if (!value) usage()
    if (arg === '--target') parsed.target = value
    else if (arg === '--from') parsed.from = value
    else if (arg === '--to') parsed.to = value
    else if (arg === '--expect-red-matching') parsed.expectRedMatching = value
    else if (arg === '--occurrences') {
      parsed.occurrences = Number.parseInt(value, 10)
      if (!Number.isInteger(parsed.occurrences) || parsed.occurrences <= 0) usage()
    } else usage()
    i += 1
  }

  if (
    !parsed.target ||
    parsed.from === undefined ||
    parsed.from === '' ||
    parsed.to === undefined ||
    !parsed.expectRedMatching
  ) {
    usage()
  }

  try {
    parsed.expectRed = new RegExp(parsed.expectRedMatching)
  } catch (err) {
    process.stderr.write(`invalid --expect-red-matching: ${err.message}\n`)
    process.exit(EXIT.usage)
  }

  return { ...parsed, command }
}

function runGit(args, options = {}) {
  return spawnSync('git', args, {
    cwd: options.cwd ?? process.cwd(),
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  })
}

function requireGitRoot() {
  const result = runGit(['rev-parse', '--show-toplevel'])
  if (result.status !== 0) {
    process.stderr.write(result.stderr || 'not in a git worktree\n')
    process.exit(EXIT.usage)
  }
  return fs.realpathSync(result.stdout.trim())
}

function requireTarget(root, targetArg) {
  const resolved = path.resolve(root, targetArg)
  const realParent = fs.realpathSync(path.dirname(resolved))
  if (realParent !== root && !realParent.startsWith(`${root}${path.sep}`)) {
    process.stderr.write(`target must stay inside repo: ${targetArg}\n`)
    process.exit(EXIT.usage)
  }
  if (!fs.existsSync(resolved)) {
    process.stderr.write(`target does not exist: ${targetArg}\n`)
    process.exit(EXIT.usage)
  }
  const stat = fs.lstatSync(resolved)
  if (stat.isSymbolicLink() || !stat.isFile()) {
    process.stderr.write(`target must be a tracked regular file: ${targetArg}\n`)
    process.exit(EXIT.usage)
  }
  const rel = path.relative(root, resolved)
  const tracked = runGit(['ls-files', '--error-unmatch', '--', rel], { cwd: root })
  if (tracked.status !== 0) {
    process.stderr.write(`target must be tracked: ${rel}\n`)
    process.exit(EXIT.usage)
  }
  return { path: resolved, rel }
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex')
}

function statusForTarget(root, rel) {
  const result = runGit(['status', '--porcelain', '--', rel], { cwd: root })
  return result.status === 0 ? result.stdout : null
}

function snapshotTarget(root, target) {
  const bytes = fs.readFileSync(target.path)
  const stat = fs.statSync(target.path)
  state.scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'prove-red-'))
  fs.writeFileSync(path.join(state.scratch, 'target.before'), bytes, { mode: stat.mode })
  return {
    bytes,
    mode: stat.mode,
    mtime: stat.mtime,
    sha: sha256(bytes),
    status: statusForTarget(root, target.rel),
  }
}

function countOccurrences(text, needle) {
  let count = 0
  let index = 0
  while (index <= text.length) {
    const found = text.indexOf(needle, index)
    if (found === -1) break
    count += 1
    index = found + needle.length
  }
  return count
}

function applyMutation(target, originalText, from, to, occurrences) {
  const count = countOccurrences(originalText, from)
  if (count !== occurrences) return { applied: false, count }
  const mutated = originalText.split(from).join(to)
  fs.writeFileSync(target.path, mutated)
  return { applied: true, count, sha: sha256(fs.readFileSync(target.path)) }
}

function acquireLock(root) {
  const lockScript = path.join(root, 'skills-toolbox', 'worktree-lock.sh')
  state.lockScript = lockScript
  state.releaseLockOnExit = !process.env.BLI_RUNID
  if (!fs.existsSync(lockScript)) {
    process.stderr.write(`missing worktree lock: ${path.relative(root, lockScript)}\n`)
    process.exit(EXIT.usage)
  }
  const result = spawnSync('bash', [lockScript, 'acquire', state.runId, 'prove-red'], {
    cwd: root,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  if (result.status === 3) {
    process.stdout.write(`${VERDICTS.lockHeld}\n`)
    process.exit(EXIT.lockHeld)
  }
  if (result.status !== 0) {
    process.stderr.write(result.stderr || result.stdout || 'worktree lock acquire failed\n')
    process.exit(EXIT.usage)
  }
  state.locked = true
}

function releaseLock() {
  if (!state.locked || !state.lockScript || !state.root || !state.releaseLockOnExit) return
  spawnSync('bash', [state.lockScript, 'release', state.runId], {
    cwd: state.root,
    stdio: 'ignore',
  })
  state.locked = false
}

function restoreExact() {
  if (!state.target || !state.original || state.restored) return { ok: true }
  try {
    if (fs.existsSync(state.target.path)) {
      const stat = fs.lstatSync(state.target.path)
      if (stat.isSymbolicLink() || !stat.isFile()) {
        return { ok: false, reason: 'target is no longer a regular file' }
      }
    }
    fs.writeFileSync(state.target.path, state.original.bytes, { mode: state.original.mode })
    fs.chmodSync(state.target.path, state.original.mode)
    fs.utimesSync(state.target.path, state.original.mtime, state.original.mtime)
    const afterSha = sha256(fs.readFileSync(state.target.path))
    const afterStatus = statusForTarget(state.root, state.target.rel)
    if (afterSha !== state.original.sha) return { ok: false, reason: 'sha mismatch after restore' }
    if (afterStatus !== state.original.status) {
      return { ok: false, reason: 'git status changed after restore' }
    }
    state.restored = true
    return { ok: true }
  } catch (err) {
    return { ok: false, reason: err.message }
  }
}

function cleanupScratch() {
  if (state.scratch && state.cleanupScratch) {
    fs.rmSync(state.scratch, { recursive: true, force: true })
    state.scratch = null
  }
}

function cleanup() {
  restoreExact()
  releaseLock()
  cleanupScratch()
}

function unsafe(verdict, reason) {
  state.cleanupScratch = false
  process.stdout.write(`${verdict}\n`)
  if (state.scratch) process.stdout.write(`retained scratch: ${state.scratch}\n`)
  if (reason) process.stdout.write(`${reason}\n`)
  releaseLock()
  process.exit(EXIT.unsafe)
}

async function waitForCurrentChildExit(signal) {
  const child = state.currentChild
  const done = state.currentChildDone
  if (!child?.pid || !done) return

  try {
    child.kill(signal)
  } catch {}

  const outcome = await Promise.race([
    done.then(() => 'exited'),
    new Promise((resolve) => setTimeout(() => resolve('timeout'), EMERGENCY_KILL_GRACE_MS)),
  ])

  if (outcome === 'timeout' && state.currentChild === child) {
    try {
      child.kill('SIGKILL')
    } catch {}
    await done
  }
}

async function emergencyExit(signal) {
  await waitForCurrentChildExit(signal)
  restoreExact()
  releaseLock()
  state.cleanupScratch = false
  process.exit(128 + (os.constants.signals[signal] ?? 1))
}

for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.once(signal, () => emergencyExit(signal))
}

process.on('exit', cleanup)
process.on('uncaughtException', (err) => {
  restoreExact()
  releaseLock()
  state.cleanupScratch = false
  process.stderr.write(`${err.stack || err.message}\n`)
  process.exit(EXIT.unsafe)
})

function statusCode(code, signal) {
  if (Number.isInteger(code)) return code
  if (!signal) return 1
  return 128 + (os.constants.signals[signal] ?? 1)
}

function runGate(command, root) {
  return new Promise((resolve) => {
    const child = spawn(command[0], command.slice(1), {
      cwd: root,
      env: process.env,
      shell: false,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    let settle
    const done = new Promise((resolveDone) => {
      settle = resolveDone
    })
    state.currentChild = child
    state.currentChildDone = done
    const stdout = []
    const stderr = []
    child.stdout.on('data', (chunk) => stdout.push(chunk))
    child.stderr.on('data', (chunk) => stderr.push(chunk))
    child.on('error', (err) => {
      state.currentChild = null
      state.currentChildDone = null
      settle()
      resolve({
        status: 127,
        stdout: Buffer.alloc(0),
        stderr: Buffer.from(`${err.message}\n`),
      })
    })
    child.on('exit', (code, signal) => {
      state.currentChild = null
      state.currentChildDone = null
      settle()
      resolve({
        status: statusCode(code, signal),
        stdout: Buffer.concat(stdout),
        stderr: Buffer.concat(stderr),
      })
    })
  })
}

function combinedOutput(result) {
  return Buffer.concat([result.stdout, result.stderr])
}

function printOutputHead(result) {
  const text = combinedOutput(result).toString('utf8').slice(0, 4000)
  if (text) process.stdout.write(text.endsWith('\n') ? text : `${text}\n`)
}

async function main() {
  const args = parseArgs(process.argv.slice(2))
  state.root = requireGitRoot()
  state.target = requireTarget(state.root, args.target)
  acquireLock(state.root)
  state.original = snapshotTarget(state.root, state.target)

  const baseline = await runGate(args.command, state.root)
  if (baseline.status !== 0) {
    process.stdout.write(`${VERDICTS.baselineRed}\n`)
    printOutputHead(baseline)
    process.exit(EXIT.baselineRed)
  }

  const originalText = state.original.bytes.toString('utf8')
  const mutation = applyMutation(state.target, originalText, args.from, args.to, args.occurrences)
  if (!mutation.applied || mutation.sha === state.original.sha) {
    process.stdout.write(`${VERDICTS.mutationDidNotApply}\n`)
    process.stdout.write(`expected occurrences: ${args.occurrences}; found: ${mutation.count}\n`)
    process.exit(EXIT.mutationDidNotApply)
  }

  const mutated = await runGate(args.command, state.root)
  const restore = restoreExact()
  if (!restore.ok) unsafe(VERDICTS.restoreMismatch, restore.reason)

  if (mutated.status === 0) {
    const restored = await runGate(args.command, state.root)
    if (restored.status !== 0)
      unsafe(VERDICTS.restoreGateRed, combinedOutput(restored).toString('utf8'))
    if (combinedOutput(mutated).equals(combinedOutput(baseline))) {
      process.stdout.write(`${VERDICTS.byteIdentical}\n`)
      process.exit(EXIT.byteIdentical)
    }
    process.stdout.write(`${VERDICTS.vacuous}\n`)
    process.exit(EXIT.vacuous)
  }

  if (!args.expectRed.test(combinedOutput(mutated).toString('utf8'))) {
    process.stdout.write(`${VERDICTS.wrongReason}\n`)
    printOutputHead(mutated)
    process.exit(EXIT.wrongReason)
  }

  const restored = await runGate(args.command, state.root)
  if (restored.status !== 0)
    unsafe(VERDICTS.restoreGateRed, combinedOutput(restored).toString('utf8'))
  process.stdout.write(`${VERDICTS.red}\n`)
}

await main()
