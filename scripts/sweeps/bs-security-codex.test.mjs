import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { buildPrompt, classifyOutcome, runCodexReview } from './bs-security-codex.mjs'

const SCRIPT = fileURLToPath(new URL('./bs-security-codex.mjs', import.meta.url))
const REPORT = `### Widget deletion skips ownership
- Severity: high
- Location: widget.go:12
- Defect: The handler trusts the requested id.
- Impact: Another user's widget can be deleted.
- Fix: Check the owner.
- Confidence: likely
`

function fixture(t, mode = 'ok') {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-security-codex-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const bin = path.join(dir, 'codex')
  fs.writeFileSync(
    bin,
    `#!${process.execPath}\n` +
      String.raw`
const fs = require('node:fs')
const { spawn } = require('node:child_process')
const path = require('node:path')
const args = process.argv.slice(2)
const mode = process.env.FAKE_CODEX_MODE
if (args[0] === 'login') {
  if (mode === 'probe-error') process.kill(process.pid, 'SIGTERM')
  else process.exit(mode === 'not-authed' ? 1 : 0)
}
fs.writeFileSync(process.env.FAKE_EXEC_ARGS, JSON.stringify(args))
const out = args[args.indexOf('-o') + 1]
if (mode === 'timeout') {
  const grandchild = spawn(process.execPath, ['-e', 'process.on("SIGTERM", () => {}); setInterval(() => {}, 1000)'], { stdio: 'ignore' })
  grandchild.on('spawn', () => fs.writeFileSync(process.env.FAKE_GRANDCHILD, String(grandchild.pid)))
  setInterval(() => {}, 1000)
} else if (mode === 'blocked') {
  process.stderr.write('This content was flagged for possible cybersecurity risk\n')
  process.exitCode = 1
} else if (mode === 'stdin') {
  process.stdin.resume()
  process.stdin.on('end', () => fs.writeFileSync(out, process.env.FAKE_REPORT))
} else {
  fs.writeFileSync(out, mode === 'empty' ? 'No defects found.' : process.env.FAKE_REPORT)
  process.stdout.write('event\n'.repeat(mode === 'large-log' ? 200000 : 1))
  process.stderr.write('diagnostic\n')
  if (mode === 'non-zero') process.exitCode = 7
}
`,
    { mode: 0o755 },
  )
  const template = path.join(dir, 'template.md')
  const knownList = path.join(dir, 'known.md')
  fs.writeFileSync(template, 'Maintainers code review. Known findings:\n{{KNOWN_FINDINGS}}')
  fs.writeFileSync(knownList, '')
  return {
    repo: dir,
    runDir: path.join(dir, 'run'),
    template,
    knownList,
    timeoutMs: 5000,
    env: {
      ...process.env,
      BOSS_CODEX_BIN: bin,
      FAKE_CODEX_MODE: mode,
      FAKE_EXEC_ARGS: path.join(dir, 'args.json'),
      FAKE_GRANDCHILD: path.join(dir, 'grandchild.pid'),
      FAKE_REPORT: REPORT,
    },
  }
}

function assertArtifacts(result) {
  assert.deepEqual(JSON.parse(fs.readFileSync(result.findingsPath, 'utf8')), result.findings)
  assert.deepEqual(
    JSON.parse(fs.readFileSync(path.join(path.dirname(result.logPath), 'outcome.json'), 'utf8')),
    result,
  )
  assert.ok(fs.existsSync(result.logPath))
}

test('ok: findings in the final message give outcome ok', async (t) => {
  const result = await runCodexReview(fixture(t))
  assert.equal(result.outcome, 'ok')
  assert.equal(result.findings.length, 1)
  assert.equal(result.findings[0].source, 'codex')
  assert.equal(result.findings[0].lens, 'codex')
  assert.equal(result.findings[0].lensFamily, 'cross-model')
  assertArtifacts(result)
})

test('blocked: a policy refusal with no findings gives outcome blocked', async (t) => {
  const opts = fixture(t, 'blocked')
  const child = spawnSync(
    process.execPath,
    [
      SCRIPT,
      'run',
      '--repo',
      opts.repo,
      '--run-dir',
      opts.runDir,
      '--template',
      opts.template,
      '--known-list',
      opts.knownList,
    ],
    { env: opts.env, encoding: 'utf8' },
  )
  assert.equal(child.status, 0, child.stderr)
  assert.equal(child.stdout.trim().split('\n').length, 1)
  const result = JSON.parse(child.stdout)
  assert.equal(result.outcome, 'blocked')
  assertArtifacts(result)
})

test('timeout: a fake that sleeps past the timeout gives outcome timeout and its process group is gone', async (t) => {
  const opts = fixture(t, 'timeout')
  const result = await runCodexReview({ ...opts, timeoutMs: 1000 })
  assert.equal(result.outcome, 'timeout')
  const pid = Number(fs.readFileSync(opts.env.FAKE_GRANDCHILD, 'utf8'))
  // Give init time to reap the orphan after the runner has killed the whole group.
  let alive = true
  for (let attempt = 0; attempt < 100 && alive; attempt++) {
    try {
      process.kill(pid, 0)
    } catch (err) {
      assert.equal(err.code, 'ESRCH')
      alive = false
    }
    if (alive) await new Promise((resolve) => setTimeout(resolve, 20))
  }
  assert.equal(alive, false, 'grandchild must not outlive the timeout')
  assertArtifacts(result)
})

test('unavailable: an override pointing at a missing binary gives outcome unavailable', async (t) => {
  const opts = fixture(t)
  opts.env.BOSS_CODEX_BIN = path.join(opts.repo, 'missing')
  const child = spawnSync(
    process.execPath,
    [
      SCRIPT,
      'run',
      '--repo',
      opts.repo,
      '--run-dir',
      opts.runDir,
      '--template',
      opts.template,
      '--known-list',
      opts.knownList,
    ],
    { env: opts.env, encoding: 'utf8' },
  )
  assert.equal(child.status, 0, child.stderr)
  const result = JSON.parse(child.stdout)
  assert.equal(result.outcome, 'unavailable')
  assert.equal(fs.existsSync(opts.env.FAKE_EXEC_ARGS), false)
  assertArtifacts(result)
})

test('unavailable: a failing login status gives outcome unavailable without spawning exec', async (t) => {
  const opts = fixture(t, 'not-authed')
  const result = await runCodexReview(opts)
  assert.equal(result.outcome, 'unavailable')
  assert.equal(fs.existsSync(opts.env.FAKE_EXEC_ARGS), false)
  assertArtifacts(result)
})

test('empty: exit 0 with prose and no finding blocks gives outcome empty', async (t) => {
  const result = await runCodexReview(fixture(t, 'empty'))
  assert.equal(result.outcome, 'empty')
  assert.equal(result.reason, 'no finding blocks')
  assert.deepEqual(result.findings, [])
})

test('stdin is closed: a fake that reads stdin to EOF still finishes well inside the timeout', async (t) => {
  const started = Date.now()
  const opts = fixture(t, 'stdin')
  const result = await runCodexReview(opts)
  assert.equal(result.outcome, 'ok')
  assert.ok(Date.now() - started < opts.timeoutMs)
})

test('argv carries -s read-only and -o and no model flag', async (t) => {
  const opts = fixture(t)
  await runCodexReview(opts)
  const args = JSON.parse(fs.readFileSync(opts.env.FAKE_EXEC_ARGS, 'utf8'))
  assert.deepEqual(args.slice(0, 5), ['exec', '-C', opts.repo, '-s', 'read-only'])
  assert.equal(args[5], '-o')
  assert.equal(args[6], path.join(opts.runDir, 'codex/last-message.md'))
  assert.equal(args[7], 'Maintainers code review. Known findings:\n(none on file)')
  assert.equal(args.length, 8)
})

test('buildPrompt throws when the template has no known-findings placeholder', () => {
  assert.throws(() => buildPrompt('Review this.', ''), /exactly one/)
  assert.throws(() => buildPrompt('{{KNOWN_FINDINGS}} {{KNOWN_FINDINGS}}', ''), /exactly one/)
  assert.equal(buildPrompt('{{KNOWN_FINDINGS}}', 'already filed'), 'already filed')
})

test('the raw log is written to the run dir, not buffered into the result', async (t) => {
  const result = await runCodexReview(fixture(t, 'large-log'))
  assert.equal(result.outcome, 'ok')
  assert.ok(fs.statSync(result.logPath).size > 1000000)
  assert.deepEqual(
    Object.keys(result).sort(),
    ['outcome', 'reason', 'findings', 'findingsPath', 'logPath'].sort(),
  )
  assertArtifacts(result)
})

test('findings win over non-zero exit and policy markers, but timeout takes precedence', async (t) => {
  const result = await runCodexReview(fixture(t, 'non-zero'))
  assert.equal(result.outcome, 'ok')
  assert.match(result.reason, /non-zero exit \(7\)/)
  assert.equal(
    classifyOutcome({
      probeResult: 'ready',
      findings: result.findings,
      verdict: { skipReason: 'non-zero exit (7)' },
      logText: 'This content was flagged for possible cybersecurity risk',
    }).outcome,
    'ok',
  )
  assert.equal(
    classifyOutcome({ probeResult: 'ready', timedOut: true, findings: result.findings }).outcome,
    'timeout',
  )
  assert.equal(
    classifyOutcome({ probeResult: 'error', verdict: { skipReason: 'non-zero exit (2)' } }).outcome,
    'empty',
  )
})

test('CLI rejects usage and malformed templates with exit 2', (t) => {
  const opts = fixture(t)
  for (const args of [
    ['run', '--unknown'],
    ['run', '--timeout-ms', '0'],
  ]) {
    assert.equal(spawnSync(process.execPath, [SCRIPT, ...args], { encoding: 'utf8' }).status, 2)
  }
  fs.writeFileSync(opts.template, 'Review without known list')
  assert.equal(
    spawnSync(
      process.execPath,
      [
        SCRIPT,
        'run',
        '--repo',
        opts.repo,
        '--run-dir',
        opts.runDir,
        '--template',
        opts.template,
        '--known-list',
        opts.knownList,
      ],
      { env: opts.env, encoding: 'utf8' },
    ).status,
    2,
  )
})

test('probe errors proceed to exec and reused run dirs cannot reuse stale findings', async (t) => {
  const opts = fixture(t, 'probe-error')
  assert.equal((await runCodexReview(opts)).outcome, 'ok')
  opts.env.FAKE_CODEX_MODE = 'blocked'
  const result = await runCodexReview(opts)
  assert.equal(result.outcome, 'blocked')
  assert.deepEqual(result.findings, [])
  assertArtifacts(result)
})
