#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  checkFormat,
  chunk,
  filterExistingFiles,
  goFormatKey,
  isGeneratedGoPath,
  listUnformatted,
  onPath,
  resolveGoimports,
} from './check-go-format.mjs'

const keyFiles = () => [
  { path: 'a/x.go', content: Buffer.from('package a\n') },
  { path: 'b/y.go', content: Buffer.from('package b\n') },
]

test('goFormatKey is deterministic and order-independent', () => {
  const tv = 'go version go1.25 | goimports -l'
  const k1 = goFormatKey({ files: keyFiles(), toolVersions: tv })
  const k2 = goFormatKey({ files: [...keyFiles()].reverse(), toolVersions: tv })
  assert.equal(k1, k2)
  assert.match(k1, /^[a-f0-9]{64}$/)
})

test('goFormatKey changes on file content, file set, and tool version', () => {
  const tv = 'go version go1.25 | goimports -l'
  const base = goFormatKey({ files: keyFiles(), toolVersions: tv })
  const edited = keyFiles()
  edited[0].content = Buffer.from('package a // touched\n')
  assert.notEqual(base, goFormatKey({ files: edited, toolVersions: tv }))
  const added = [...keyFiles(), { path: 'c/z.go', content: Buffer.from('package c\n') }]
  assert.notEqual(base, goFormatKey({ files: added, toolVersions: tv }))
  assert.notEqual(
    base,
    goFormatKey({ files: keyFiles(), toolVersions: 'go version go1.26 | goimports -l' }),
  )
})

// A fixed goimports descriptor so checkFormat tests never depend on whether the
// standalone `goimports` binary happens to be on PATH in the test environment.
const GOIMPORTS = { label: 'goimports -l', cmd: 'goimports', baseArgs: ['-l'] }

test('isGeneratedGoPath matches gen/ segments, not gen substrings', () => {
  assert.equal(isGeneratedGoPath('lib/bossalib/gen/bossanova/v1/x.pb.go'), true)
  assert.equal(isGeneratedGoPath('gen/x.go'), true)
  assert.equal(isGeneratedGoPath('services/bossd/internal/gen/y.go'), true)
  assert.equal(isGeneratedGoPath('services/bossd/generator/z.go'), false)
  assert.equal(isGeneratedGoPath('lib/gendata/w.go'), false)
})

test('filterExistingFiles excludes tracked files deleted from the worktree', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'go-format-files-'))
  try {
    fs.mkdirSync(path.join(dir, 'pkg'))
    fs.writeFileSync(path.join(dir, 'pkg', 'present.go'), 'package pkg\n')
    assert.deepEqual(filterExistingFiles(dir, ['pkg/present.go', 'pkg/deleted.go']), [
      'pkg/present.go',
    ])
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('chunk splits into fixed-size batches', () => {
  assert.deepEqual(chunk([1, 2, 3, 4, 5], 2), [[1, 2], [3, 4], [5]])
  assert.deepEqual(chunk([], 3), [])
})

test('listUnformatted collects flagged paths and batches large inputs', () => {
  const calls = []
  const run = (cmd, args) => {
    const files = args.slice(1) // drop the -l flag
    calls.push(files.length)
    // Flag any file whose basename starts with "bad".
    return files.filter((f) => f.split('/').pop().startsWith('bad')).join('\n')
  }
  const files = Array.from({ length: 850 }, (_, i) => (i < 2 ? `bad${i}.go` : `ok${i}.go`))
  const flagged = listUnformatted({
    label: 'gofmt -l',
    cmd: 'gofmt',
    baseArgs: ['-l'],
    files,
    run,
    repoRoot: '/repo',
  })
  assert.deepEqual([...flagged].sort(), ['bad0.go', 'bad1.go'])
  // 850 files at batch size 400 → 3 batches (400 + 400 + 50).
  assert.deepEqual(calls, [400, 400, 50])
})

test('resolveGoimports prefers the PATH binary, falls back to read-only `go tool`', () => {
  const orig = process.env.PATH
  try {
    process.env.PATH = '' // no goimports binary → the go-tool fallback
    const r = resolveGoimports()
    assert.deepEqual(r.baseArgs, ['-C', 'lib/bossalib', 'tool', 'goimports', '-l'])
    assert.equal(r.cmd, 'go')
    assert.equal(r.absolutePaths, true)
    assert.match(r.env.GOFLAGS, /(?:^|\s)-mod=readonly(?:\s|$)/)
    assert.equal(r.env.GOWORK, 'off')
  } finally {
    process.env.PATH = orig
  }
  assert.equal(onPath('definitely-not-a-real-binary-xyz'), false)
})

test('resolveGoimports runs a PATH goimports binary in read-only module mode', () => {
  const originalPath = process.env.PATH
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'goimports-path-'))
  const binary = path.join(dir, 'goimports')
  fs.writeFileSync(binary, '#!/bin/sh\nexit 0\n')
  fs.chmodSync(binary, 0o755)
  try {
    process.env.PATH = dir
    const resolved = resolveGoimports()
    assert.equal(resolved.cmd, 'goimports')
    assert.match(resolved.env.GOFLAGS, /(?:^|\s)-mod=readonly(?:\s|$)/)
    assert.equal(resolved.env.GOWORK, 'off')
  } finally {
    process.env.PATH = originalPath
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('checkFormat preserves the goimports read-only environment', () => {
  const calls = []
  checkFormat({
    repoRoot: '/repo',
    files: ['a.go'],
    run: (cmd, _args, env) => {
      calls.push({ cmd, env })
      return ''
    },
    goimports: {
      label: 'read-only goimports',
      cmd: 'go',
      baseArgs: ['tool', 'goimports', '-l'],
      env: { GOFLAGS: '-mod=readonly', GOWORK: 'off' },
    },
  })

  assert.deepEqual(calls, [
    { cmd: 'gofmt', env: undefined },
    { cmd: 'go', env: { GOFLAGS: '-mod=readonly', GOWORK: 'off' } },
  ])
})

test('checkFormat unions gofmt and goimports offenders, deduped and sorted', () => {
  const run = (cmd) => {
    if (cmd === 'gofmt') return 'b.go\n'
    return 'a.go\nb.go\n' // goimports
  }
  const offenders = checkFormat({
    repoRoot: '/repo',
    files: ['a.go', 'b.go', 'c.go'],
    run,
    goimports: GOIMPORTS,
  })
  assert.deepEqual(offenders, ['a.go', 'b.go'])
})

test('checkFormat returns nothing for an empty file set (no formatter invoked)', () => {
  let called = false
  const run = () => {
    called = true
    return ''
  }
  assert.deepEqual(checkFormat({ repoRoot: '/repo', files: [], run, goimports: GOIMPORTS }), [])
  assert.equal(called, false)
})

test('checkFormat surfaces a formatter that cannot run as a hard error', () => {
  const run = () => {
    throw new Error('gofmt: not found')
  }
  assert.throws(
    () => checkFormat({ repoRoot: '/repo', files: ['a.go'], run, goimports: GOIMPORTS }),
    /gofmt -l failed to run/,
  )
})
