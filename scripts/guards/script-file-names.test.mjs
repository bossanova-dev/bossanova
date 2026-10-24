import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync, execFileSync } from 'node:child_process'
import test from 'node:test'

function run(t, files, baseline = null) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'script-names-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const contents = {
    'scripts/guards/registry/example.mjs': `export default ${JSON.stringify({ id: 'example', rule: 'Example', command: 'node example.mjs', wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } }, scans: ['scripts/**'], baseline, proof: { file: 'example.test.mjs', test: 'example' } })}`,
    ...Object.fromEntries(files.map((file) => [file, ''])),
  }
  for (const [file, text] of Object.entries(contents)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '--', ...Object.keys(contents)], { cwd: root })
  const result = spawnSync(
    process.execPath,
    ['scripts/guards/script-file-names.mjs', '--root', root],
    { encoding: 'utf8' },
  )
  return { status: result.status, output: result.stdout + result.stderr }
}
for (const [name, file, diagnostic] of [
  ['non-kebab directory', 'skills-toolbox/Foo_Bar/ok.mjs', /Foo_Bar.*kebab-case/],
  ['orphan baseline', 'scripts/orphan-baseline.json', /orphan-baseline.json.*registered guard/],
]) {
  test(`script-file-names rejects a ${name} with the registered command`, (t) => {
    const result = run(t, [file])
    assert.equal(result.status, 1, result.output)
    assert.match(result.output, diagnostic)
  })
}
test('script-file-names accepts dotted kebab names, registered baselines and fixture exclusions', (t) => {
  const result = run(
    t,
    [
      'scripts/Makefile',
      'scripts/a.test.mjs',
      'scripts/a.generated.mjs',
      'scripts/com.bossanova.clean-go-build-cache.plist.example',
      'scripts/example-baseline.json',
      'scripts/fixtures/Foo_Bar/camelCase.mjs',
      'skills-toolbox/testdata/Foo_Bar.mjs',
    ],
    'scripts/example-baseline.json',
  )
  assert.equal(result.status, 0, result.output)
})

test('script-file-names rejects a camel-case file with the registered command', (t) => {
  const result = run(t, ['scripts/camelCase.mjs'])
  assert.equal(result.status, 1, result.output)
  assert.match(result.output, /camelCase.mjs.*kebab-case/)
})
