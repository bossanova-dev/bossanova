import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { test } from 'node:test'
import { syncGeneratedTree } from './sync-generated-tree.mjs'

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'sync-generated-test-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  const from = path.join(root, 'from')
  const to = path.join(root, 'to')
  fs.mkdirSync(from)
  fs.mkdirSync(to)
  const write = (dir, name, bytes, mode = 0o644) => {
    const file = path.join(dir, name)
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, bytes, { mode })
    return file
  }
  return { root, from, to, write }
}

test('unchanged bytes and mode preserve inode and mtime', (t) => {
  const f = fixture(t)
  f.write(f.from, 'same.go', 'same')
  const file = f.write(f.to, 'same.go', 'same')
  fs.utimesSync(file, 100, 100)
  const before = fs.statSync(file)
  const result = syncGeneratedTree(f)
  const after = fs.statSync(file)
  assert.equal(after.ino, before.ino)
  assert.equal(after.mtimeMs, before.mtimeMs)
  assert.deepEqual(result, {
    written: [],
    removed: [],
    unchanged: [path.relative(process.cwd(), file)],
  })
})

test('changed files are atomically replaced and temporary siblings are cleaned', (t) => {
  const f = fixture(t)
  f.write(f.from, 'changed.go', 'new')
  const file = f.write(f.to, 'changed.go', 'old')
  const before = fs.statSync(file)
  const result = syncGeneratedTree(f)
  assert.equal(fs.readFileSync(file, 'utf8'), 'new')
  assert.notEqual(fs.statSync(file).ino, before.ino)
  assert.deepEqual(result.written, [path.relative(process.cwd(), file)])
  assert.deepEqual(fs.readdirSync(f.to), ['changed.go'])
})

test('new nested files and changed modes receive the source mode', (t) => {
  const f = fixture(t)
  f.write(f.from, 'nested/new.go', 'new', 0o755)
  f.write(f.from, 'mode.go', 'same', 0o755)
  f.write(f.to, 'mode.go', 'same', 0o644)
  syncGeneratedTree(f)
  for (const name of ['nested/new.go', 'mode.go']) {
    assert.equal(fs.statSync(path.join(f.to, name)).mode & 0o777, 0o755)
  }
})

test('pruning removes only stale matches after all atomic replacements', (t) => {
  const f = fixture(t)
  f.write(f.from, 'present.go', 'new')
  f.write(f.from, 'nested/new.go', 'new')
  f.write(f.to, 'present.go', 'old')
  f.write(f.to, 'stale.go', 'stale')
  f.write(f.to, 'nested/stale.go', 'stale')
  f.write(f.to, 'BUILD.bazel', 'keep')
  const operations = []
  const recordingFs = new Proxy(fs, {
    get(target, key) {
      if (['writeFileSync', 'renameSync', 'unlinkSync', 'rmSync'].includes(key)) {
        return (...args) => {
          operations.push([key, ...args])
          return target[key](...args)
        }
      }
      return target[key]
    },
  })
  const result = syncGeneratedTree({ ...f, prune: ['**/*.go'], fs: recordingFs })
  assert.equal(result.removed.length, 2)
  assert.equal(fs.readFileSync(path.join(f.to, 'BUILD.bazel'), 'utf8'), 'keep')
  const lastRename = operations.findLastIndex(([name]) => name === 'renameSync')
  assert.ok(lastRename >= 0)
  for (const [index, [name, file, destination]] of operations.entries()) {
    if (name === 'writeFileSync') assert.match(file, /\.[^/]+\.gen-tmp$/)
    if (name === 'renameSync') assert.ok(destination.endsWith('.go'))
    if (name === 'unlinkSync' || name === 'rmSync') {
      assert.ok(index > lastRename)
      assert.ok(!fs.existsSync(path.join(f.from, path.relative(f.to, file))))
    }
  }
})

for (const missing of [false, true]) {
  test(`${missing ? 'missing' : 'empty'} source fails before touching destination`, (t) => {
    const f = fixture(t)
    const file = f.write(f.to, 'keep.go', 'keep')
    const before = fs.statSync(file)
    if (missing) fs.rmdirSync(f.from)
    assert.throws(() => syncGeneratedTree({ ...f, prune: ['**/*.go'] }))
    assert.deepEqual(fs.statSync(file), before)
    assert.equal(fs.readFileSync(file, 'utf8'), 'keep')
    assert.deepEqual(fs.readdirSync(f.to), ['keep.go'])
  })
}

test('failed rename cleans its temporary sibling and preserves old bytes', (t) => {
  const f = fixture(t)
  f.write(f.from, 'present.go', 'new')
  const file = f.write(f.to, 'present.go', 'old')
  assert.throws(
    () =>
      syncGeneratedTree({
        ...f,
        fs: {
          ...fs,
          renameSync() {
            throw new Error('rename failed')
          },
        },
      }),
    /rename failed/,
  )
  assert.equal(fs.readFileSync(file, 'utf8'), 'old')
  assert.deepEqual(fs.readdirSync(f.to), ['present.go'])
})

test('CLI rejects unknown flags and missing required source', () => {
  for (const args of [['--unknown'], ['--to', 'unused']]) {
    const result = spawnSync(
      process.execPath,
      ['scripts/skills/sync-generated-tree.mjs', ...args],
      {
        encoding: 'utf8',
      },
    )
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, /unknown|--from/i)
  }
})
