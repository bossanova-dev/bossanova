#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { after, test } from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const proveRed = path.join(repoRoot, 'scripts', 'prove-red.mjs')
const tempRoots = []

function sha256(file) {
  return createHash('sha256').update(fs.readFileSync(file)).digest('hex')
}

function initRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'prove-red-fixture-'))
  tempRoots.push(dir)
  execFileSync('git', ['init', '-q', dir])
  execFileSync('git', ['-C', dir, 'config', 'user.email', 'test@example.com'])
  execFileSync('git', ['-C', dir, 'config', 'user.name', 'Test'])
  execFileSync('git', ['-C', dir, 'config', 'commit.gpgsign', 'false'])
  fs.mkdirSync(path.join(dir, 'skills-toolbox'), { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'skills-toolbox', 'worktree-lock.sh'),
    [
      '#!/usr/bin/env bash',
      'set -euo pipefail',
      'if [ "${PROVE_RED_LOCK_HELD:-}" = "1" ] && [ "${1:-}" = "acquire" ]; then',
      '  echo "HELD_BY_PEER runid=peer age=0s"',
      '  exit 3',
      'fi',
      'printf "%s %s %s\\n" "${1:-}" "${2:-}" "${BLI_RUNID:-}" >> lock-events',
      'case "${1:-}" in',
      '  acquire) echo "ACQUIRED runid=${2:-}" ;;',
      '  release) echo "RELEASED" ;;',
      '  *) exit 2 ;;',
      'esac',
      '',
    ].join('\n'),
    { mode: 0o755 },
  )
  fs.writeFileSync(path.join(dir, 'target.txt'), 'alpha ORIGINAL omega\n')
  execFileSync('git', ['-C', dir, 'add', 'target.txt', 'skills-toolbox/worktree-lock.sh'])
  execFileSync('git', ['-C', dir, 'commit', '-q', '-m', 'init'])
  return dir
}

function writeGate(repo, body) {
  fs.writeFileSync(
    path.join(repo, 'gate.mjs'),
    [
      "import fs from 'node:fs'",
      "import path from 'node:path'",
      "const target = fs.readFileSync(path.join(process.cwd(), 'target.txt'), 'utf8')",
      body,
      '',
    ].join('\n'),
  )
}

function run(repo, extraArgs = [], options = {}) {
  try {
    const stdout = execFileSync(
      process.execPath,
      [
        proveRed,
        '--target',
        'target.txt',
        '--from',
        'ORIGINAL',
        '--to',
        'MUTATED',
        '--expect-red-matching',
        'expected failure',
        ...extraArgs,
        '--',
        process.execPath,
        'gate.mjs',
      ],
      {
        cwd: repo,
        encoding: 'utf8',
        env: { ...process.env, ...options.env },
        stdio: ['ignore', 'pipe', 'pipe'],
      },
    )
    return { code: 0, stdout }
  } catch (err) {
    return {
      code: err.status ?? 1,
      stdout: err.stdout?.toString() ?? '',
      stderr: err.stderr?.toString() ?? '',
    }
  }
}

function porcelain(repo) {
  return execFileSync('git', ['-C', repo, 'status', '--porcelain', '--', 'target.txt'], {
    encoding: 'utf8',
  })
}

after(() => {
  for (const dir of tempRoots) fs.rmSync(dir, { recursive: true, force: true })
})

test('prove-red reports PROOF RED for a landed mutation that fails for the expected reason', () => {
  const repo = initRepo()
  writeGate(
    repo,
    [
      "if (target.includes('MUTATED')) {",
      "  console.error('expected failure: mutant reached the assertion')",
      '  process.exit(1)',
      '}',
      "console.log('green')",
    ].join('\n'),
  )

  const result = run(repo)

  assert.equal(result.code, 0, result.stderr)
  assert.match(result.stdout, /^PROOF RED$/m)
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
  assert.equal(porcelain(repo), '')
})

test('prove-red reports exit 96 and skips the mutated leg when the mutation matches nothing', () => {
  const repo = initRepo()
  fs.writeFileSync(path.join(repo, 'count'), '0')
  writeGate(
    repo,
    [
      "const countPath = path.join(process.cwd(), 'count')",
      "fs.writeFileSync(countPath, String(Number(fs.readFileSync(countPath, 'utf8')) + 1))",
      "if (target.includes('MUTATED')) process.exit(9)",
      "console.log('green')",
    ].join('\n'),
  )

  const result = run(repo, ['--from', 'NOT_PRESENT'])

  assert.equal(result.code, 96)
  assert.match(result.stdout, /^PROOF INCONCLUSIVE — mutation did not apply$/m)
  assert.doesNotMatch(result.stdout, /PROOF RED/)
  assert.equal(fs.readFileSync(path.join(repo, 'count'), 'utf8'), '1')
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
})

test('prove-red reports exit 95 when the gate is red for the wrong reason', () => {
  const repo = initRepo()
  writeGate(
    repo,
    [
      "if (target.includes('MUTATED')) {",
      "  console.error('unrelated module resolution failure')",
      '  process.exit(1)',
      '}',
      "console.log('green')",
    ].join('\n'),
  )

  const result = run(repo)

  assert.equal(result.code, 95)
  assert.match(result.stdout, /^PROOF INCONCLUSIVE — red for the wrong reason$/m)
  assert.doesNotMatch(result.stdout, /PROOF RED/)
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
})

test('prove-red reports exit 1 when a landed mutation leaves the gate green with changed output', () => {
  const repo = initRepo()
  writeGate(repo, "console.log(`green: ${target.includes('MUTATED') ? 'mutated' : 'baseline'}`)")

  const result = run(repo)

  assert.equal(result.code, 1)
  assert.match(result.stdout, /^PROOF VACUOUS — gate stayed green under an applied mutation$/m)
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
})

test('prove-red reports exit 92 when green output is byte-identical under mutation', () => {
  const repo = initRepo()
  writeGate(repo, "console.log('green')")

  const result = run(repo)

  assert.equal(result.code, 92)
  assert.match(result.stdout, /^PROOF VACUOUS — output byte-identical under an applied mutation/m)
  assert.match(result.stdout, /deferred BOS-1005 caching half/)
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
})

test('prove-red reports exit 93 and does not mutate when the baseline is already red', () => {
  const repo = initRepo()
  writeGate(repo, "console.error('baseline failure'); process.exit(1)")

  const result = run(repo)

  assert.equal(result.code, 93)
  assert.match(result.stdout, /^PROOF INCONCLUSIVE — baseline already red$/m)
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
})

test('prove-red reports exit 94 and retains scratch when exact restore is impossible', () => {
  const repo = initRepo()
  writeGate(
    repo,
    [
      "if (target.includes('MUTATED')) {",
      "  fs.rmSync(path.join(process.cwd(), 'target.txt'))",
      "  fs.mkdirSync(path.join(process.cwd(), 'target.txt'))",
      "  console.error('expected failure after destructive gate')",
      '  process.exit(1)',
      '}',
      "console.log('green')",
    ].join('\n'),
  )

  const result = run(repo)

  assert.equal(result.code, 94)
  assert.match(result.stdout, /^PROOF UNSAFE — restore mismatch$/m)
  const scratch = result.stdout.match(/retained scratch: (.+)/)?.[1]
  assert.ok(scratch, result.stdout)
  assert.equal(fs.existsSync(scratch), true)
})

test('prove-red reports exit 94 when the restored gate is not green', () => {
  const repo = initRepo()
  fs.writeFileSync(path.join(repo, 'count'), '0')
  writeGate(
    repo,
    [
      "const countPath = path.join(process.cwd(), 'count')",
      "const next = Number(fs.readFileSync(countPath, 'utf8')) + 1",
      'fs.writeFileSync(countPath, String(next))',
      "if (target.includes('MUTATED')) {",
      "  console.error('expected failure under mutation')",
      '  process.exit(1)',
      '}',
      'if (next === 3) {',
      "  console.error('restored leg failure')",
      '  process.exit(1)',
      '}',
      "console.log('green')",
    ].join('\n'),
  )

  const result = run(repo)

  assert.equal(result.code, 94)
  assert.match(result.stdout, /^PROOF UNSAFE — gate not green after restore$/m)
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
})

test('prove-red reports exit 97 and mutates nothing when the worktree lock is held by a peer', () => {
  const repo = initRepo()
  writeGate(repo, "throw new Error('gate must not run')")

  const result = run(repo, [], { env: { PROVE_RED_LOCK_HELD: '1' } })

  assert.equal(result.code, 97)
  assert.match(result.stdout, /^PROOF BLOCKED — worktree lock held by peer$/m)
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
  assert.equal(porcelain(repo), '')
})

test('prove-red re-enters an outer boss-build lock without releasing it', () => {
  const repo = initRepo()
  writeGate(
    repo,
    [
      "if (target.includes('MUTATED')) {",
      "  console.error('expected failure under caller lock')",
      '  process.exit(1)',
      '}',
      "console.log('green')",
    ].join('\n'),
  )

  const result = run(repo, [], { env: { BLI_RUNID: 'outer-run' } })

  assert.equal(result.code, 0, result.stderr)
  assert.match(result.stdout, /^PROOF RED$/m)
  assert.equal(
    fs.readFileSync(path.join(repo, 'lock-events'), 'utf8').trim(),
    'acquire outer-run outer-run',
  )
  assert.equal(fs.readFileSync(path.join(repo, 'target.txt'), 'utf8'), 'alpha ORIGINAL omega\n')
})

test('prove-red restores uncommitted target bytes after a full proof cycle', () => {
  const repo = initRepo()
  fs.writeFileSync(path.join(repo, 'target.txt'), 'alpha ORIGINAL uncommitted omega\n')
  const beforeSha = sha256(path.join(repo, 'target.txt'))
  const beforeStatus = porcelain(repo)
  writeGate(
    repo,
    [
      "if (target.includes('MUTATED')) {",
      "  console.error('expected failure on uncommitted mutant')",
      '  process.exit(1)',
      '}',
      "console.log('green')",
    ].join('\n'),
  )

  const result = run(repo)

  assert.equal(result.code, 0, result.stderr)
  assert.equal(sha256(path.join(repo, 'target.txt')), beforeSha)
  assert.equal(porcelain(repo), beforeStatus)
  assert.match(beforeStatus, /^ M target\.txt$/m)
})

test('prove-red restores the target after SIGTERM during the mutated gate leg', async () => {
  const repo = initRepo()
  const marker = path.join(repo, 'mutated-started')
  writeGate(
    repo,
    [
      "if (target.includes('MUTATED')) {",
      "  fs.writeFileSync(path.join(process.cwd(), 'mutated-started'), '1')",
      '  setTimeout(() => {}, 10_000)',
      '} else {',
      "  console.log('green')",
      '}',
    ].join('\n'),
  )
  const beforeSha = sha256(path.join(repo, 'target.txt'))
  const beforeStatus = porcelain(repo)
  const child = spawn(
    process.execPath,
    [
      proveRed,
      '--target',
      'target.txt',
      '--from',
      'ORIGINAL',
      '--to',
      'MUTATED',
      '--expect-red-matching',
      'expected failure',
      '--',
      process.execPath,
      'gate.mjs',
    ],
    { cwd: repo, stdio: ['ignore', 'pipe', 'pipe'] },
  )

  for (let i = 0; i < 100 && !fs.existsSync(marker); i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 20))
  }
  assert.equal(fs.existsSync(marker), true, 'mutated gate leg did not start')
  child.kill('SIGTERM')
  await new Promise((resolve) => child.on('exit', resolve))

  assert.equal(sha256(path.join(repo, 'target.txt')), beforeSha)
  assert.equal(porcelain(repo), beforeStatus)
})

test('prove-red waits for a SIGTERM-trapping gate before restoring the target', async () => {
  const repo = initRepo()
  const marker = path.join(repo, 'mutated-started')
  writeGate(
    repo,
    [
      "if (target.includes('MUTATED')) {",
      "  fs.writeFileSync(path.join(process.cwd(), 'mutated-started'), '1')",
      "  process.on('SIGTERM', () => {",
      '    setTimeout(() => {',
      "      fs.writeFileSync(path.join(process.cwd(), 'target.txt'), 'late child write\\n')",
      '      process.exit(0)',
      '    }, 100)',
      '  })',
      '  setInterval(() => {}, 1_000)',
      '} else {',
      "  console.log('green')",
      '}',
    ].join('\n'),
  )
  const beforeSha = sha256(path.join(repo, 'target.txt'))
  const beforeStatus = porcelain(repo)
  const child = spawn(
    process.execPath,
    [
      proveRed,
      '--target',
      'target.txt',
      '--from',
      'ORIGINAL',
      '--to',
      'MUTATED',
      '--expect-red-matching',
      'expected failure',
      '--',
      process.execPath,
      'gate.mjs',
    ],
    { cwd: repo, stdio: ['ignore', 'pipe', 'pipe'] },
  )

  for (let i = 0; i < 100 && !fs.existsSync(marker); i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 20))
  }
  assert.equal(fs.existsSync(marker), true, 'mutated gate leg did not start')
  child.kill('SIGTERM')
  await new Promise((resolve) => child.on('exit', resolve))
  await new Promise((resolve) => setTimeout(resolve, 200))

  assert.equal(sha256(path.join(repo, 'target.txt')), beforeSha)
  assert.equal(porcelain(repo), beforeStatus)
})
