import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import test from 'node:test'

function fixture(t, files, baseline = []) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'comment-guard-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  for (const [file, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }
  fs.mkdirSync(path.join(root, 'scripts/guards'), { recursive: true })
  fs.writeFileSync(path.join(root, BASELINE), JSON.stringify(baseline))
  for (const args of [
    ['init', '-q'],
    ['config', 'core.excludesFile', '/dev/null'],
    ['add', '.'],
    [
      '-c',
      'user.name=Test',
      '-c',
      'user.email=test@example.com',
      '-c',
      'commit.gpgsign=false',
      'commit',
      '-qm',
      'fixture',
    ],
  ]) {
    execFileSync('git', args, { cwd: root })
  }
  return root
}
function cli(root) {
  return spawnSync(process.execPath, [SCRIPT, '--root', root], { encoding: 'utf8' })
}

const SCRIPT = 'scripts/guards/check-comment-paths.mjs'
const BASELINE = 'scripts/guards/comment-paths-baseline.json'

test('seeded missing citation fails exact CLI and passes once baselined', (t) => {
  const root = fixture(t, { 'services/x/a.go': '// see `services/x/gone.go`\n// scripts/gone.mjs' })
  assert.equal(cli(root).status, 1)
  fs.writeFileSync(
    path.join(root, BASELINE),
    JSON.stringify(['services/x/a.go: scripts/gone.mjs', 'services/x/a.go: services/x/gone.go']),
  )
  assert.equal(cli(root).status, 0)
})
test('existing relative paths packages placeholders and non-citations pass', (t) => {
  const root = fixture(t, {
    'services/web/src/lib/a.ts': [
      '// lib/theme.ts services/web/src/shared/lib/theme.ts:10-12#anchor',
      '// services/web/src/lib services/web/src/lib.Symbol.Method',
      '// services/x/*.go services/<placeholder>/a.go scripts/check- tools/list',
      '// https://host/services/x/gone.go services/x/.../gone.go',
      '// services/x/$name.go services/x/…/gone.go',
      '// services/x/ignored.go',
      'const x = "services/x/gone.go"',
    ].join('\n'),
    'services/web/src/shared/lib/theme.ts': '',
    '.gitignore': 'services/x/ignored.go\n',
  })
  const result = cli(root)
  assert.equal(result.status, 0, result.stderr)
})
test('stale path baseline fails and exception covers missing citation', (t) => {
  const root = fixture(t, {
    'services/x/a.go':
      '// guard-exception(comment-paths): historical mention; expires 2030-01-01\n// services/x/gone.go',
  })
  assert.equal(cli(root).status, 0)
  fs.writeFileSync(
    path.join(root, BASELINE),
    JSON.stringify(['services/x/a.go: services/x/old.go']),
  )
  assert.equal(cli(root).status, 1)
})

test('clean checkout ignores daemon runtime citations without hiding missing paths', (t) => {
  const root = fixture(t, {
    '.gitignore': fs.readFileSync('.gitignore', 'utf8'),
    '.claude/README.md': '',
    'services/x/a.go': '// .claude/settings.local.json\n// .claude/scheduled_tasks.lock',
  })
  for (const file of ['.claude/settings.local.json', '.claude/scheduled_tasks.lock']) {
    assert.equal(fs.existsSync(path.join(root, file)), false)
    assert.equal(spawnSync('git', ['check-ignore', '--quiet', '--', file], { cwd: root }).status, 0)
  }
  assert.equal(cli(root).status, 0)
  fs.appendFileSync(path.join(root, 'services/x/a.go'), '\n// .claude/missing.json')
  const result = cli(root)
  assert.equal(result.status, 1)
  assert.match(result.stderr, /\.claude\/missing\.json/)
})
