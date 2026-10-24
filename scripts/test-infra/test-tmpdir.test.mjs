import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'
import { tempDir } from './test-tmpdir.mjs'

for (const fail of [false, true]) {
  test(`test-context cleanup runs on ${fail ? 'failure' : 'pass'}`, (t) => {
    const fixture = tempDir('tempdir-test-', t)
    const record = join(fixture, 'record')
    const source = `
      import test from 'node:test';
      import { writeFileSync } from 'node:fs';
      import { tempDir } from ${JSON.stringify(new URL('./test-tmpdir.mjs', import.meta.url).href)};
      test('fixture', t => {
        writeFileSync(${JSON.stringify(record)}, tempDir('child-fixture-', t));
        ${fail ? "throw new Error('deliberate test failure');" : ''}
      });
    `
    const child = spawnSync(process.execPath, ['--input-type=module', '-e', source], {
      encoding: 'utf8',
    })
    assert.equal(child.status, fail ? 1 : 0, child.stdout + child.stderr)
    assert.equal(existsSync(readFileSync(record, 'utf8')), false)
  })
}

test('context-free directories are removed at process exit', (t) => {
  const fixture = tempDir('tempdir-test-', t)
  const record = join(fixture, 'record')
  const child = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      `
    import { writeFileSync } from 'node:fs';
    import { tempDir } from ${JSON.stringify(new URL('./test-tmpdir.mjs', import.meta.url).href)};
    writeFileSync(${JSON.stringify(record)}, tempDir('child-fixture-'));
    process.exitCode = 5;
  `,
    ],
    { encoding: 'utf8' },
  )
  assert.equal(child.status, 5, child.stderr)
  assert.equal(existsSync(readFileSync(record, 'utf8')), false)
})
