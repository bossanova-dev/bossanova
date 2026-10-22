import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import test from 'node:test'
import { moveGoPackage } from './move-go-package.mjs'

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'move-go-package-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const files = {
    'services/example/go.mod': 'module example.com/service\n\ngo 1.26.0\n',
    'services/example/internal/old/old.go': 'package old\n',
    'services/example/internal/old/BUILD.bazel': `go_test(\n    name = "old_test",\n    data = glob(\n        ["*.go"],\n    ),  # keep\n    rundir = ".", # keep\n    shard_count = 2, # keep\n    size = "small", # keep\n)\n`,
    'services/example/use.go':
      'package example\nimport alias "example.com/service/internal/old"\nvar _ = alias.Value\n',
    'other/use.go': 'package other\nimport "example.com/service/internal/old"\n',
    'docs/example.md':
      'See services/example/internal/old/old.go and `services/example/internal/old`.\n',
    '.claude/skills/verify/features/example.md':
      'owns:\n  - services/example/internal/old/**\n  - services/example/internal/old*/**\n',
    '.codex/skills/example/SKILL.md': 'services/example/internal/old/old.go\n',
    'data.bin': Buffer.from([0, 255]),
  }
  for (const [name, content] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, name)), { recursive: true })
    fs.writeFileSync(path.join(root, name), content)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '.'], { cwd: root })
  return root
}
const from = 'services/example/internal/old'
const to = 'services/example/internal/domain/old'

test('moves a package, preserves aliases and rewrites tracked citations without narrowing wildcards', (t) => {
  const root = fixture(t)
  const calls = []
  moveGoPackage({ root, from, to, run: (cmd, args) => calls.push([cmd, args]) })
  assert.equal(fs.existsSync(path.join(root, from)), false)
  assert.equal(fs.readFileSync(path.join(root, to, 'old.go'), 'utf8'), 'package old\n')
  assert.match(
    fs.readFileSync(path.join(root, 'services/example/use.go'), 'utf8'),
    /alias "example.com\/service\/internal\/domain\/old"/,
  )
  assert.match(
    fs.readFileSync(path.join(root, 'other/use.go'), 'utf8'),
    /"example.com\/service\/internal\/domain\/old"/,
  )
  assert.match(fs.readFileSync(path.join(root, 'docs/example.md'), 'utf8'), /domain\/old\/old.go/)
  const feature = fs.readFileSync(
    path.join(root, '.claude/skills/verify/features/example.md'),
    'utf8',
  )
  assert.match(feature, /domain\/old\/\*\*/)
  assert.match(feature, /internal\/old\*\/\*\*/)
  assert.equal(
    fs.readFileSync(path.join(root, '.codex/skills/example/SKILL.md'), 'utf8'),
    'services/example/internal/old/old.go\n',
  )
  assert.deepEqual(
    calls.map(([cmd]) => cmd),
    ['gofmt', 'goimports', 'bazel'],
  )
  assert.deepEqual(calls[2][1], ['run', '//:gazelle'])
  assert.match(
    fs.readFileSync(path.join(root, to, 'BUILD.bazel'), 'utf8'),
    /shard_count = 2, # keep/,
  )
})

test('fails when gazelle drops or changes a kept BUILD attribute', (t) => {
  for (const replacement of ['', '    rundir = "elsewhere", # keep\n']) {
    const root = fixture(t)
    assert.throws(
      () =>
        moveGoPackage({
          root,
          from,
          to,
          run(cmd) {
            if (cmd === 'bazel') {
              const build = path.join(root, to, 'BUILD.bazel')
              fs.writeFileSync(
                build,
                fs.readFileSync(build, 'utf8').replace('    rundir = ".", # keep\n', replacement),
              )
            }
          },
        }),
      /kept BUILD attribute changed/,
    )
  }
})
