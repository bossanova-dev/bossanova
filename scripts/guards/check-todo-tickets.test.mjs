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

const SCRIPT = 'scripts/guards/check-todo-tickets.mjs'
const BASELINE = 'scripts/guards/todo-tickets-baseline.json'

test('seeded unticketed TODO fails and passes once baselined', (t) => {
  const root = fixture(t, { 'services/x/a.go': '// function docs\n//\n// TODO: seeded\npackage x' })
  assert.equal(cli(root).status, 1)
  fs.writeFileSync(path.join(root, BASELINE), JSON.stringify(['services/x/a.go: TODO: seeded']))
  assert.equal(cli(root).status, 0)
})
test('TODO comment forms and ids on later lines are classified', (t) => {
  const root = fixture(t, {
    'services/x/a.go':
      '// TODO(BOS-12): yes\npackage x\nvar x = "TODO"\nvar y = context.TODO()\n// the TODO list\n/* TODO: yes\n * BOS-34\n */',
    'services/x/b.tsx': 'const x = <div>{/* TODO: x */}</div>',
    'services/x/c.ts': '/* TODO(hero) */',
  })
  const result = cli(root)
  assert.equal(result.status, 1)
  assert.match(result.stderr, /b.tsx/)
  assert.match(result.stderr, /c.ts/)
  assert.doesNotMatch(result.stderr, /a.go:/)
})
test('stale TODO baseline fails and inline exception covers its line', (t) => {
  const root = fixture(t, {
    'services/x/a.go':
      '// guard-exception(todo-tickets): queued elsewhere; expires 2030-01-01\n// TODO: x',
  })
  assert.equal(cli(root).status, 0)
  fs.writeFileSync(path.join(root, BASELINE), JSON.stringify(['services/x/a.go: TODO: old']))
  assert.equal(cli(root).status, 1)
})
