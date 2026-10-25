import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'

const helper = new URL('./discard-capture-outside-boundary.mjs', import.meta.url)

test('capture discard restores tracked files and removes additions outside the solution boundary', () => {
  const repo = mkdtempSync(join(tmpdir(), 'capture-boundary-'))
  const git = (...args) => execFileSync('git', ['-C', repo, ...args], { encoding: 'utf8' })
  try {
    git('init', '-q')
    git('config', 'commit.gpgsign', 'false')
    git('config', 'user.name', 'Fixture')
    git('config', 'user.email', 'fixture@example.test')
    for (const name of ['edited.md', 'deleted.md', 'renamed.md'])
      writeFileSync(join(repo, name), name)
    mkdirSync(join(repo, 'docs/solutions'), { recursive: true })
    writeFileSync(join(repo, 'docs/solutions/kept.md'), 'original')
    git('add', '.')
    git('commit', '-qm', 'fixture')
    writeFileSync(join(repo, 'edited.md'), 'capture')
    git('add', 'edited.md')
    // A restored worktree must not hide capture bytes still in the index.
    writeFileSync(join(repo, 'edited.md'), 'edited.md')
    rmSync(join(repo, 'deleted.md'))
    git('mv', 'renamed.md', 'new name.md')
    writeFileSync(join(repo, 'staged.md'), 'new')
    git('add', 'staged.md')
    writeFileSync(join(repo, 'untracked name.md'), 'new')
    mkdirSync(join(repo, 'docs/solutions-sibling'), { recursive: true })
    writeFileSync(join(repo, 'docs/solutions-sibling/new.md'), 'outside')
    writeFileSync(join(repo, 'docs/solutions/kept.md'), 'capture solution')
    writeFileSync(join(repo, 'docs/solutions/new.md'), 'new solution')
    const run = spawnSync(process.execPath, [helper.pathname], { cwd: repo, encoding: 'utf8' })
    assert.equal(run.status, 0, run.stderr)
    const discarded = JSON.parse(run.stdout).discarded
    assert.deepEqual(
      discarded.sort(),
      [
        'deleted.md',
        'docs/solutions-sibling/new.md',
        'edited.md',
        'new name.md',
        'renamed.md',
        'staged.md',
        'untracked name.md',
      ].sort(),
    )
    for (const name of ['edited.md', 'deleted.md', 'renamed.md'])
      assert.equal(readFileSync(join(repo, name), 'utf8'), name)
    for (const name of ['staged.md', 'new name.md', 'untracked name.md'])
      assert.equal(existsSync(join(repo, name)), false)
    assert.equal(readFileSync(join(repo, 'docs/solutions/kept.md'), 'utf8'), 'capture solution')
    assert.equal(readFileSync(join(repo, 'docs/solutions/new.md'), 'utf8'), 'new solution')
    assert.equal(git('diff', '--cached', '--name-only'), '')
    const again = spawnSync(process.execPath, [helper.pathname], { cwd: repo, encoding: 'utf8' })
    assert.equal(again.status, 0, again.stderr)
    assert.deepEqual(JSON.parse(again.stdout).discarded, [])
  } finally {
    rmSync(repo, { recursive: true, force: true, maxRetries: 3 })
  }
})
