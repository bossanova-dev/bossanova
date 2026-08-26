#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const scriptPath = path.join(repoRoot, '.github/scripts/tidy-drift-all.sh')
const tempRoots = []

function git(cwd, ...args) {
  return execFileSync('git', args, { cwd, encoding: 'utf8' })
}

function writeFile(root, rel, content, mode) {
  const full = path.join(root, rel)
  fs.mkdirSync(path.dirname(full), { recursive: true })
  fs.writeFileSync(full, content)
  if (mode !== undefined) fs.chmodSync(full, mode)
}

function initRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tidy-drift-'))
  tempRoots.push(dir)
  git(dir, 'init', '-q', '-b', 'main')
  git(dir, 'config', 'user.email', 'test@example.com')
  git(dir, 'config', 'user.name', 'Test')
  git(dir, 'config', 'commit.gpgsign', 'false')

  for (const module of ['lib/alpha', 'services/beta']) {
    writeFile(dir, `${module}/go.mod`, `module example.com/${module}\n\ngo 1.25\n`)
    writeFile(dir, `${module}/go.sum`, 'stale\n')
  }
  writeFile(dir, 'go.work.sum', 'workspace-hash\n')
  writeFile(
    dir,
    'bin/go',
    `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" != "mod" ] || [ "$2" != "tidy" ]; then
  echo "unexpected go invocation: $*" >&2
  exit 99
fi
case "\${TIDY_STUB_MODE:-clean}" in
  clean)
    ;;
  deterministic)
    printf 'tidy deterministic\\n' > go.sum
    ;;
  varying)
    count_file=".tidy-count"
    count=0
    if [ -f "$count_file" ]; then count="$(cat "$count_file")"; fi
    count=$((count + 1))
    printf '%s\\n' "$count" > "$count_file"
    printf 'tidy varying %s\\n' "$count" > go.sum
    ;;
  failing)
    printf 'partial tidy output\\n' > go.sum
    exit 42
    ;;
  *)
    echo "unknown TIDY_STUB_MODE=$TIDY_STUB_MODE" >&2
    exit 98
    ;;
esac
`,
    0o755,
  )
  git(dir, 'add', '.')
  git(dir, 'commit', '-q', '-m', 'base')
  return dir
}

function runTidy(dir, args = [], mode = 'clean') {
  const result = spawnSync('bash', [scriptPath, ...args], {
    cwd: dir,
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${path.join(dir, 'bin')}${path.delimiter}${process.env.PATH}`,
      TIDY_STUB_MODE: mode,
      TIDY_DRIFT_RETRY_SLEEP_SECONDS: '0',
    },
  })
  return {
    code: result.status ?? 1,
    stdout: result.stdout,
    stderr: result.stderr,
    output: `${result.stdout}${result.stderr}`,
  }
}

after(() => {
  for (const dir of tempRoots) {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('passes when go mod tidy changes no module files', () => {
  const dir = initRepo()

  const result = runTidy(dir)

  assert.equal(result.code, 0, result.output)
  assert.match(result.stdout, /All 2 modules tidy/)
})

test('fails deterministic drift without the transient proxy warning and names make tidy', () => {
  const dir = initRepo()

  const result = runTidy(dir, [], 'deterministic')

  assert.equal(result.code, 1)
  assert.match(result.output, /deterministic go\.mod\/go\.sum drift/)
  assert.match(result.output, /run make tidy/)
  assert.doesNotMatch(result.output, /likely transient proxy\.golang\.org flake/)
})

test('preserves an uncommitted working-tree tidy fix', () => {
  const dir = initRepo()
  writeFile(dir, 'lib/alpha/go.sum', 'tidy deterministic\n')
  writeFile(dir, 'services/beta/go.sum', 'tidy deterministic\n')

  const result = runTidy(dir, [], 'deterministic')

  assert.equal(result.code, 0, result.output)
  assert.equal(fs.readFileSync(path.join(dir, 'lib/alpha/go.sum'), 'utf8'), 'tidy deterministic\n')
  assert.equal(
    fs.readFileSync(path.join(dir, 'services/beta/go.sum'), 'utf8'),
    'tidy deterministic\n',
  )
})

test('warns and retries when consecutive attempts produce different bytes', () => {
  const dir = initRepo()

  const result = runTidy(dir, [], 'varying')

  assert.equal(result.code, 1)
  assert.match(result.output, /likely transient proxy\.golang\.org flake/)
  assert.match(result.output, /after 3 attempts/)
  assert.equal(fs.readFileSync(path.join(dir, 'lib/alpha/.tidy-count'), 'utf8'), '3\n')
})

test('check mode restores the snapshot when go mod tidy exits non-zero after writing files', () => {
  const dir = initRepo()

  const result = runTidy(dir, [], 'failing')

  assert.equal(result.code, 1)
  assert.match(result.output, /go mod tidy failed in lib\/alpha/)
  assert.equal(fs.readFileSync(path.join(dir, 'lib/alpha/go.sum'), 'utf8'), 'stale\n')
  assert.equal(fs.readFileSync(path.join(dir, 'services/beta/go.sum'), 'utf8'), 'stale\n')
})

test('--fix leaves tidy bytes in the working tree and exits zero', () => {
  const dir = initRepo()

  const result = runTidy(dir, ['--fix'], 'deterministic')

  assert.equal(result.code, 0, result.output)
  assert.match(result.stdout, /Tidied 2 module\(s\): lib\/alpha services\/beta/)
  assert.equal(fs.readFileSync(path.join(dir, 'lib/alpha/go.sum'), 'utf8'), 'tidy deterministic\n')
  assert.equal(
    fs.readFileSync(path.join(dir, 'services/beta/go.sum'), 'utf8'),
    'tidy deterministic\n',
  )
})

test('--fix does not touch go.work.sum', () => {
  const dir = initRepo()

  const result = runTidy(dir, ['--fix'], 'deterministic')

  assert.equal(result.code, 0, result.output)
  assert.equal(fs.readFileSync(path.join(dir, 'go.work.sum'), 'utf8'), 'workspace-hash\n')
  assert.equal(git(dir, 'status', '--porcelain', 'go.work.sum'), '')
})
