#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import { run } from './lint-docs-affected.mjs'

// Create real files so the isCheckableFile lstat guard passes, then check them
// with an injected exec/log so no real prettier runs.
function withTempFiles(names, fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-docs-'))
  try {
    const paths = names.map((name) => {
      const p = path.join(dir, name)
      fs.writeFileSync(p, '# x\n')
      return p
    })
    return fn(paths)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

test('run passes changed prettier-owned files to prettier --check', () => {
  withTempFiles(['a.md', 'b.json'], (paths) => {
    const logs = []
    let execArgs = null
    const code = run(paths, {
      prettierCmd: 'prettier',
      exec: (_cmd, args) => {
        execArgs = args
      },
      log: (line) => logs.push(line),
    })
    assert.equal(code, 0)
    assert.equal(execArgs[0], '--check')
    assert.deepEqual(execArgs.slice(1).sort(), [...paths].sort())
    assert.ok(logs.some((l) => l.includes('prettier --check')))
  })
})

test('run returns 0 when no prettier-owned files changed', () => {
  withTempFiles(['main.go'], (paths) => {
    let called = false
    const code = run(paths, {
      prettierCmd: 'prettier',
      exec: () => {
        called = true
      },
      log: () => {},
    })
    assert.equal(code, 0)
    assert.equal(called, false)
  })
})

test('run returns non-zero when prettier --check reports a mis-formatted file', () => {
  withTempFiles(['bad.md'], (paths) => {
    const code = run(paths, {
      prettierCmd: 'prettier',
      exec: () => {
        throw new Error('Code style issues found')
      },
      log: () => {},
    })
    assert.equal(code, 1)
  })
})

test('run skips (exit 0) when prettier is unavailable', () => {
  withTempFiles(['a.md'], (paths) => {
    let called = false
    const code = run(paths, {
      prettierCmd: null,
      exec: () => {
        called = true
      },
      log: () => {},
    })
    assert.equal(code, 0)
    assert.equal(called, false)
  })
})
